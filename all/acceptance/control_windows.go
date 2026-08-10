//go:build windows

// Windows control speaks the installed service's named-pipe protocol with
// overlapped I/O so every read and write can be deadline-bound.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// Serializes commands because the named pipe is one ordered reply stream.
type windowsControl struct {
	pipe      *os.File
	reader    *bufio.Reader
	stateLock sync.Mutex
}

// Retries the local named pipe only until the caller's deadline.
func newPlatformControl(ctx context.Context) (platformControl, error) {
	const pipeName = `\\.\pipe\urnetwork.control`
	var lastError error
	for {
		// Overlapped I/O lets os.File enforce deadlines on Windows named
		// pipes. A synchronous handle can block the entire acceptance run if
		// the service stops replying.
		pipe, err := os.OpenFile(pipeName, os.O_RDWR|windows.O_FILE_FLAG_OVERLAPPED, 0)
		if err == nil {
			return &windowsControl{pipe: pipe, reader: bufio.NewReader(pipe)}, nil
		}
		lastError = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to Windows control pipe: %w (%v)", ctx.Err(), lastError)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Matches each service reply to the command that caused it.
func (self *windowsControl) call(ctx context.Context, messageType string, payload map[string]any) (map[string]any, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	payload["type"] = messageType
	frame, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := self.pipe.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set Windows control deadline: %w", err)
		}
	}
	if _, err := self.pipe.Write(append(frame, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := self.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var reply map[string]any
		if err := json.Unmarshal(line, &reply); err != nil {
			return nil, fmt.Errorf("invalid Windows control reply: %w", err)
		}
		if stringValue(reply["type"], "") != "reply" || stringValue(reply["in_reply_to"], "") != messageType {
			continue
		}
		if ok, _ := reply["ok"].(bool); !ok {
			return nil, fmt.Errorf("%s", stringValue(reply["error"], "control request rejected"))
		}
		return reply, nil
	}
}

// Converts the Windows wire fields into the shared status shape.
func windowsStatus(reply map[string]any) tunnelStatus {
	status, _ := reply["status"].(map[string]any)
	return tunnelStatus{
		State:            stringValue(status["state"], "stopped"),
		RpcListenAddress: stringValue(status["rpc_listen_hostport"], ""),
		ServiceVersion:   stringValue(status["service_version"], ""),
		ProtocolVersion:  int(numberValue(status["protocol_version"])),
		Error:            stringValue(status["error"], ""),
	}
}

// Reads the installed service version and control protocol.
func (self *windowsControl) Hello(ctx context.Context, _ string) (tunnelStatus, error) {
	reply, err := self.call(ctx, "hello", map[string]any{})
	if err != nil {
		return tunnelStatus{}, err
	}
	return windowsStatus(reply), nil
}

// Reads current service and tunnel state.
func (self *windowsControl) Status(ctx context.Context) (tunnelStatus, error) {
	reply, err := self.call(ctx, "get_state", map[string]any{})
	if err != nil {
		return tunnelStatus{}, err
	}
	return windowsStatus(reply), nil
}

// Starts the service with the SDK state and temporary device-rpc material.
func (self *windowsControl) Start(ctx context.Context, config tunnelConfig) (tunnelStatus, error) {
	reply, err := self.call(ctx, "start_tunnel", map[string]any{
		"by_jwt":              config.ByJwt,
		"network_space_json":  config.NetworkSpaceJson,
		"instance_id":         config.InstanceId,
		"device_description":  config.DeviceDescription,
		"device_spec":         config.DeviceSpec,
		"app_version":         config.AppVersion,
		"rpc_server_pem":      config.RpcServerPem,
		"rpc_client_cert_pem": config.RpcClientCertPem,
		"rpc_listen_hostport": config.RpcListenHostPort,
		"excluded_app_paths":  []string{},
		"allowlist_mode":      false,
	})
	if err != nil {
		return tunnelStatus{}, err
	}
	return windowsStatus(reply), nil
}

// Stops the local tunnel and waits for the service reply.
func (self *windowsControl) Stop(ctx context.Context) error {
	_, err := self.call(ctx, "stop_tunnel", map[string]any{})
	return err
}

// Releases the local named-pipe handle.
func (self *windowsControl) Close() error { return self.pipe.Close() }
