// Controlled peer support proves native same-platform traffic in both directions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/sdk"
)

// peerToPeerResult is the client-side proof that traffic crossed the exact
// same-network provider selected by the acceptance runner.
type peerToPeerResult struct {
	ProviderSelected     bool  `json:"provider_selected"`
	RemoteEgressPackets  int64 `json:"remote_egress_packets"`
	RemoteEgressBytes    int64 `json:"remote_egress_bytes"`
	RemoteIngressPackets int64 `json:"remote_ingress_packets"`
	RemoteIngressBytes   int64 `json:"remote_ingress_bytes"`
}

// peerProviderResult is the independent provider-side proof that the second
// app received the client's request and returned its response.
type peerProviderResult struct {
	Ok                   bool  `json:"ok"`
	RemoteEgressPackets  int64 `json:"remote_egress_packets"`
	RemoteEgressBytes    int64 `json:"remote_egress_bytes"`
	RemoteIngressPackets int64 `json:"remote_ingress_packets"`
	RemoteIngressBytes   int64 `json:"remote_ingress_bytes"`
}

// packetStatsSnapshot keeps only the cumulative counters required to prove a
// bidirectional request/response exchange.
type packetStatsSnapshot struct {
	remoteEgressPackets  int64
	remoteEgressBytes    int64
	remoteIngressPackets int64
	remoteIngressBytes   int64
}

// readRetainedClient loads exactly one canonical SDK client ID from a private
// handoff file. The producer may terminate the record with one newline.
func readRetainedClient(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" || len(strings.Fields(string(data))) != 1 {
		return "", errors.New("client ID file must contain exactly one value")
	}
	id, err := sdk.ParseId(value)
	if err != nil {
		return "", fmt.Errorf("invalid client ID: %w", err)
	}
	return id.String(), nil
}

// snapshotPacketStats copies the counters without retaining the SDK object.
func snapshotPacketStats(stats *sdk.PacketStats) packetStatsSnapshot {
	if stats == nil {
		return packetStatsSnapshot{}
	}
	return packetStatsSnapshot{
		remoteEgressPackets:  stats.RemoteEgressPacketCount,
		remoteEgressBytes:    int64(stats.RemoteEgressByteCount),
		remoteIngressPackets: stats.RemoteIngressPacketCount,
		remoteIngressBytes:   int64(stats.RemoteIngressByteCount),
	}
}

// packetStatsDelta rejects counter resets instead of turning them into a false
// traffic success.
func packetStatsDelta(before, after packetStatsSnapshot) (packetStatsSnapshot, bool) {
	if after.remoteEgressPackets < before.remoteEgressPackets ||
		after.remoteEgressBytes < before.remoteEgressBytes ||
		after.remoteIngressPackets < before.remoteIngressPackets ||
		after.remoteIngressBytes < before.remoteIngressBytes {
		return packetStatsSnapshot{}, false
	}
	return packetStatsSnapshot{
		remoteEgressPackets:  after.remoteEgressPackets - before.remoteEgressPackets,
		remoteEgressBytes:    after.remoteEgressBytes - before.remoteEgressBytes,
		remoteIngressPackets: after.remoteIngressPackets - before.remoteIngressPackets,
		remoteIngressBytes:   after.remoteIngressBytes - before.remoteIngressBytes,
	}, true
}

func (self packetStatsSnapshot) bidirectional() bool {
	return self.remoteEgressPackets > 0 && self.remoteEgressBytes > 0 &&
		self.remoteIngressPackets > 0 && self.remoteIngressBytes > 0
}

// clientResult converts a verified delta into the client result contract.
func (self packetStatsSnapshot) clientResult() peerToPeerResult {
	return peerToPeerResult{
		ProviderSelected:     true,
		RemoteEgressPackets:  self.remoteEgressPackets,
		RemoteEgressBytes:    self.remoteEgressBytes,
		RemoteIngressPackets: self.remoteIngressPackets,
		RemoteIngressBytes:   self.remoteIngressBytes,
	}
}

// providerResult converts a verified delta into the provider result contract.
func (self packetStatsSnapshot) providerResult() peerProviderResult {
	return peerProviderResult{
		Ok:                   true,
		RemoteEgressPackets:  self.remoteEgressPackets,
		RemoteEgressBytes:    self.remoteEgressBytes,
		RemoteIngressPackets: self.remoteIngressPackets,
		RemoteIngressBytes:   self.remoteIngressBytes,
	}
}

// requirePeerDiscoveryConnection protects the OS tunnel while the controlled
// peer is discovered. On Windows, disconnecting the public provider first
// leaves the tunnel idle long enough for the service watchdog to close the
// device RPC, making an otherwise healthy peer intermittently undiscoverable.
func requirePeerDiscoveryConnection(status sdk.ConnectionStatus) error {
	if status != sdk.Connected {
		return fmt.Errorf("peer discovery requires an active tunnel connection: status %s", status)
	}
	return nil
}

// isControlledNetworkPeer rejects a stale Connected status left over from the
// public provider. A peer switch is complete only when the selected location
// names the exact runner-created same-network client.
func isControlledNetworkPeer(location *sdk.ConnectLocation, providerId *sdk.Id) bool {
	return location != nil && location.NetworkPeer && providerId != nil &&
		location.ConnectLocationId != nil && location.ConnectLocationId.ClientId != nil &&
		location.ConnectLocationId.ClientId.Cmp(providerId) == 0
}

func controlledNetworkPeerConnected(
	status sdk.ConnectionStatus,
	location *sdk.ConnectLocation,
	providerId *sdk.Id,
) bool {
	return status == sdk.Connected && isControlledNetworkPeer(location, providerId)
}

// runPeerToPeerClient selects only the runner-created same-network peer and
// proves request and response bytes on the installed app's tunnel.
func runPeerToPeerClient(
	device *sdk.DeviceRemote,
	controller *sdk.ConnectViewController,
	providerClientId string,
) (peerToPeerResult, error) {
	providerId, err := sdk.ParseId(providerClientId)
	if err != nil {
		return peerToPeerResult{}, fmt.Errorf("parse provider client ID: %w", err)
	}
	if err := requirePeerDiscoveryConnection(controller.GetConnectionStatus()); err != nil {
		return peerToPeerResult{}, err
	}
	peerController := device.OpenPeerViewController()
	peerController.Start()
	defer device.ClosePeerViewController(peerController)

	var selectedName string
	if err := waitUntil(180*time.Second, func() bool {
		peers := peerController.GetPeers()
		if peers == nil {
			return false
		}
		for i := 0; i < peers.Len(); i++ {
			peer := peers.Get(i)
			if peer != nil && peer.ProvideEnabled && peer.ClientId != nil && peer.ClientId.Cmp(providerId) == 0 {
				selectedName = peer.DeviceName
				return true
			}
		}
		return false
	}); err != nil {
		return peerToPeerResult{}, errors.New("controlled provider was not discoverable")
	}

	before := snapshotPacketStats(device.GetPacketStats())
	controller.Connect(&sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{ClientId: providerId},
		Name:              selectedName,
		NetworkPeer:       true,
	})
	defer controller.Disconnect()
	if err := waitUntil(120*time.Second, func() bool {
		return controlledNetworkPeerConnected(
			controller.GetConnectionStatus(),
			device.GetConnectLocation(),
			providerId,
		)
	}); err != nil {
		return peerToPeerResult{}, fmt.Errorf(
			"controlled provider did not connect: last status %s, exact peer selected %t",
			controller.GetConnectionStatus(),
			isControlledNetworkPeer(device.GetConnectLocation(), providerId),
		)
	}

	if _, err := publicIp(); err != nil {
		return peerToPeerResult{}, fmt.Errorf("peer egress request: %w", err)
	}
	var delta packetStatsSnapshot
	if err := waitUntil(30*time.Second, func() bool {
		var monotonic bool
		delta, monotonic = packetStatsDelta(before, snapshotPacketStats(device.GetPacketStats()))
		return monotonic && delta.bidirectional()
	}); err != nil {
		return peerToPeerResult{}, errors.New("client observed no bidirectional traffic through the controlled provider")
	}

	controller.Disconnect()
	if err := waitUntil(60*time.Second, func() bool {
		return controller.GetConnectionStatus() == sdk.Disconnected
	}); err != nil {
		return peerToPeerResult{}, errors.New("controlled provider did not disconnect")
	}
	return delta.clientResult(), nil
}

// runPeerProvider creates the second same-platform app identity, advertises
// Network-only provide mode, and independently verifies both traffic directions.
func runPeerProvider(opts options) (returnErr error) {
	if opts.Credentials == "" || opts.ActiveClient == "" || opts.StateDir == "" ||
		opts.SdkVersion == "" || opts.AppVersion == "" || opts.ProviderReady == "" ||
		opts.ProviderStop == "" || opts.ProviderResult == "" {
		return errors.New("credentials, active-client, state-dir, sdk-version, app-version, peer-provider-ready, peer-provider-stop, and peer-provider-result are required")
	}
	if opts.ProviderEgressInterface != "" && opts.ProviderEgressIndex != 0 {
		return errors.New("set only one peer provider egress interface selector")
	}
	if err := configureProviderEgress(opts); err != nil {
		return err
	}

	user, password, err := readCredentials(opts.Credentials)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return err
	}
	for _, path := range []string{opts.ActiveClient, opts.ProviderReady, opts.ProviderStop, opts.ProviderResult} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
	}
	_ = os.Remove(opts.ProviderStop)
	_ = os.Remove(opts.ProviderResult)
	_ = os.Remove(opts.ProviderReady)
	var completion *peerProviderResult
	// Registered before every lifecycle cleanup below, so it runs last. The
	// atomic result is the cross-shell success marker only after client removal,
	// logout, and device teardown have all left returnErr clear.
	defer func() {
		returnErr = completePeerProvider(opts.ProviderResult, completion, returnErr)
	}()

	sdk.Version = opts.SdkVersion
	manager := sdk.NewNetworkSpaceManager(opts.StateDir)
	defer manager.Close()
	networkSpace := manager.UpdateNetworkSpaceValues(
		sdk.NewNetworkSpaceKey("ur.network", "main"),
		&sdk.NetworkSpaceValues{
			Bundled:                  true,
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
			LinkHostName:             "ur.io",
			MigrationHostName:        "bringyour.com",
		},
	)
	api := networkSpace.GetApi()

	requestCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	login, err := api.AuthLoginWithPasswordSyncWithContext(requestCtx, &sdk.AuthLoginWithPasswordArgs{
		UserAuth: user,
		Password: password,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("password login: %w", err)
	}
	if login.Error != nil {
		return fmt.Errorf("password login: %s", login.Error.Message)
	}
	if login.Network == nil || login.Network.ByJwt == "" {
		return errors.New("password login returned no network JWT")
	}
	networkJWT := login.Network.ByJwt
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	if err := localState.SetByJwt(networkJWT); err != nil {
		return fmt.Errorf("persist network JWT: %w", err)
	}
	api.SetByJwt(networkJWT)
	defer func() {
		if err := localState.Logout(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("clear provider SDK session: %w", err))
		}
	}()

	requestCtx, cancel = context.WithTimeout(context.Background(), 45*time.Second)
	client, err := api.AuthNetworkClientSyncWithContext(requestCtx, &sdk.AuthNetworkClientArgs{
		DeviceDescription: "URnetwork P2P acceptance provider " + runtime.GOOS,
		DeviceSpec:        runtime.GOOS + "/" + runtime.GOARCH,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("register provider client: %w", err)
	}
	if client.Error != nil {
		return fmt.Errorf("register provider client: %s", client.Error.Message)
	}
	if client.ByClientJwt == "" {
		return errors.New("register provider client returned no client JWT")
	}
	clientId, err := jwtStringClaim(client.ByClientJwt, "client_id")
	if err != nil {
		return err
	}
	defer func() {
		if err := removeClient(networkJWT, clientId); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("release provider client: %w", err))
		} else if err := removeActiveClient(opts.ActiveClient); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("clear retained provider client: %w", err))
		}
		_ = os.Remove(opts.ProviderReady)
	}()
	if err := writeActiveClient(opts.ActiveClient, clientId); err != nil {
		return fmt.Errorf("retain provider client: %w", err)
	}
	if err := localState.SetByClientJwt(client.ByClientJwt); err != nil {
		return fmt.Errorf("persist provider client JWT: %w", err)
	}
	instanceId := localState.GetInstanceId()
	if instanceId == nil {
		return errors.New("provider client registration did not create an instance ID")
	}

	device, err := sdk.NewDeviceLocalWithDefaults(
		networkSpace,
		client.ByClientJwt,
		"URnetwork P2P acceptance provider "+runtime.GOOS,
		runtime.GOOS+"/"+runtime.GOARCH,
		opts.AppVersion,
		instanceId,
		false,
	)
	if err != nil {
		return fmt.Errorf("create provider device: %w", err)
	}
	defer device.Close()
	defer func() {
		device.SetProvideControlMode(sdk.ProvideControlModeNever)
		device.SetTunnelStarted(false)
	}()
	device.SetTunnelStarted(true)
	device.SetProvideNetworkMode(sdk.ProvideNetworkModeAll)
	device.SetProvideControlMode(sdk.ProvideControlModeNetwork)
	device.SetProvidePaused(false)
	if err := waitUntil(120*time.Second, func() bool {
		return device.GetProvideEnabled() && device.GetProvideMode() == sdk.ProvideModeNetwork
	}); err != nil {
		return errors.New("provider did not enter Network provide mode")
	}
	baseline := snapshotPacketStats(device.GetProviderPacketStats())
	if err := writeActiveClient(opts.ProviderReady, clientId); err != nil {
		return fmt.Errorf("publish provider readiness: %w", err)
	}
	fmt.Fprintln(os.Stderr, "acceptance peer provider: ready")

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	for {
		if _, err := os.Stat(opts.ProviderStop); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect provider stop marker: %w", err)
		}
		select {
		case <-signalCtx.Done():
			return signalCtx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}

	var delta packetStatsSnapshot
	if err := waitUntil(30*time.Second, func() bool {
		var monotonic bool
		delta, monotonic = packetStatsDelta(baseline, snapshotPacketStats(device.GetProviderPacketStats()))
		return monotonic && delta.bidirectional()
	}); err != nil {
		return errors.New("provider observed no bidirectional peer traffic")
	}
	result := delta.providerResult()
	completion = &result
	return nil
}

// Publishes the provider's atomic success marker only after its deferred
// lifecycle cleanup has completed successfully.
func completePeerProvider(path string, result *peerProviderResult, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if result == nil {
		return errors.New("provider completed without a traffic proof")
	}
	if err := writePrivateJSON(path, result); err != nil {
		return fmt.Errorf("write provider result: %w", err)
	}
	return nil
}

// configureProviderEgress keeps a same-host provider outside the client tunnel.
func configureProviderEgress(opts options) error {
	index := opts.ProviderEgressIndex
	if opts.ProviderEgressInterface != "" {
		iface, err := net.InterfaceByName(opts.ProviderEgressInterface)
		if err != nil {
			return fmt.Errorf("provider egress interface: %w", err)
		}
		if iface.Index <= 0 {
			return errors.New("provider egress interface has no usable index")
		}
		index = uint(iface.Index)
	}
	if index != 0 {
		sdk.SetEgressInterfaceIndex(int(index), int(index))
	}
	return nil
}

// writePrivateJSON atomically publishes an owner-only result file.
func writePrivateJSON(path string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0o600)
}
