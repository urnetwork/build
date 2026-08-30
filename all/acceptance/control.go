// Shared service-control types keep the Linux socket and Windows pipe agents
// on one acceptance contract.
package main

import (
	"context"
	"fmt"
)

const (
	linuxControlProtocolVersion   = 1
	windowsControlProtocolVersion = 3
)

// Requires the exact protocol implemented by each platform service. The
// Linux socket and Windows named pipe evolve independently, so neither
// version is a universal desktop protocol number.
func validateControlProtocol(platform string, version int) error {
	var expected int
	switch platform {
	case "linux":
		expected = linuxControlProtocolVersion
	case "windows":
		expected = windowsControlProtocolVersion
	default:
		return fmt.Errorf("unsupported control platform %q", platform)
	}
	if version != expected {
		return fmt.Errorf("unexpected %s control protocol %d; want %d", platform, version, expected)
	}
	return nil
}

// Carries everything the installed service needs to start one local tunnel.
type tunnelConfig struct {
	ByJwt             string
	NetworkSpaceJson  string
	InstanceId        string
	DeviceDescription string
	DeviceSpec        string
	AppVersion        string
	RpcServerPem      string
	RpcClientCertPem  string
	RpcListenHostPort string
	RpcSessionId      string
}

// Builds the cross-platform fields that identify one complete, pinned device
// RPC generation. Both services reject a partial or unpinned start.
func pinnedStartTunnelPayload(config tunnelConfig) map[string]any {
	return map[string]any{
		"by_jwt":              config.ByJwt,
		"network_space_json":  config.NetworkSpaceJson,
		"instance_id":         config.InstanceId,
		"app_version":         config.AppVersion,
		"rpc_server_pem":      config.RpcServerPem,
		"rpc_client_cert_pem": config.RpcClientCertPem,
		"rpc_listen_hostport": config.RpcListenHostPort,
		"rpc_session_id":      config.RpcSessionId,
	}
}

// Normalizes the platform-specific service reply used by acceptance checks.
type tunnelStatus struct {
	State            string
	RpcPort          int
	RpcListenAddress string
	ServiceVersion   string
	ProtocolVersion  int
	SdkVersion       string
	Error            string
}

// Exposes the common lifecycle supported by each desktop service.
type platformControl interface {
	// Negotiates protocol and local build versions.
	Hello(context.Context, string) (tunnelStatus, error)
	// Reads the current service state.
	Status(context.Context) (tunnelStatus, error)
	// Starts a tunnel from one registered client session.
	Start(context.Context, tunnelConfig) (tunnelStatus, error)
	// Stops any active local tunnel.
	Stop(context.Context) error
	// Releases the control transport.
	Close() error
}
