//go:build linux

// Linux control speaks the newline-delimited local daemon protocol over its
// unix socket, with a deadline on every exchange.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// Serializes request ids because the daemon protocol is one ordered stream.
type linuxControl struct {
	conn      net.Conn
	reader    *bufio.Reader
	stateLock sync.Mutex
	nextId    int64
}

// Connects only to the local service socket selected by the runner.
func newPlatformControl(ctx context.Context) (platformControl, error) {
	path := os.Getenv("URNETWORK_CONTROL_SOCKET")
	if path == "" {
		path = "/run/urnetwork/control.sock"
	}
	dialer := net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect to Linux control socket: %w", err)
	}
	return &linuxControl{conn: conn, reader: bufio.NewReader(conn), nextId: 1}, nil
}

// Matches replies by request id and rejects daemon error replies.
func (self *linuxControl) call(ctx context.Context, verb string, payload map[string]any) (map[string]any, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	id := self.nextId
	self.nextId++
	payload["verb"] = verb
	payload["id"] = id
	frame, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	if err := self.conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := self.conn.Write(append(frame, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := self.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var reply map[string]any
		if err := json.Unmarshal(line, &reply); err != nil {
			return nil, fmt.Errorf("invalid Linux control reply: %w", err)
		}
		if replyID, _ := reply["id"].(float64); int64(replyID) != id {
			continue
		}
		if ok, _ := reply["ok"].(bool); !ok {
			return nil, fmt.Errorf("%s", stringValue(reply["error"], "control request rejected"))
		}
		return reply, nil
	}
}

// Converts the Linux wire fields into the shared status shape.
func linuxStatus(reply map[string]any) tunnelStatus {
	return tunnelStatus{
		State:           stringValue(reply["tunnel_state"], "stopped"),
		RpcPort:         int(numberValue(reply["rpc_port"])),
		ServiceVersion:  stringValue(reply["daemon_version"], ""),
		ProtocolVersion: int(numberValue(reply["protocol_version"])),
		SdkVersion:      stringValue(reply["sdk_version"], ""),
		Error:           stringValue(reply["error"], ""),
	}
}

// Verifies the exact control and SDK versions expected by the local build.
func (self *linuxControl) Hello(ctx context.Context, sdkVersion string) (tunnelStatus, error) {
	reply, err := self.call(ctx, "hello", map[string]any{
		"protocol_version": 1,
		"sdk_version":      sdkVersion,
	})
	if err != nil {
		return tunnelStatus{}, err
	}
	return linuxStatus(reply), nil
}

// Reads current daemon and tunnel state.
func (self *linuxControl) Status(ctx context.Context) (tunnelStatus, error) {
	reply, err := self.call(ctx, "status", map[string]any{})
	if err != nil {
		return tunnelStatus{}, err
	}
	return linuxStatus(reply), nil
}

// Starts the local daemon with the registered client session.
func (self *linuxControl) Start(ctx context.Context, config tunnelConfig) (tunnelStatus, error) {
	reply, err := self.call(ctx, "start_tunnel", pinnedStartTunnelPayload(config))
	if err != nil {
		return tunnelStatus{}, err
	}
	return linuxStatus(reply), nil
}

// Stops the local tunnel and waits for the daemon reply.
func (self *linuxControl) Stop(ctx context.Context) error {
	_, err := self.call(ctx, "stop_tunnel", map[string]any{})
	return err
}

// Releases the local control connection.
func (self *linuxControl) Close() error { return self.conn.Close() }
