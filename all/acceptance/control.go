// Shared service-control types keep the Linux socket and Windows pipe agents
// on one acceptance contract.
package main

import "context"

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
