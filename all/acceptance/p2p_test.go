// Controlled peer tests keep fixture parsing and traffic proof deterministic.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

type blockingPeerProviderDevice struct {
	events  *[]string
	entered chan struct{}
	release chan struct{}
}

func (self *blockingPeerProviderDevice) SetProvideControlMode(mode sdk.ProvideControlMode) {
	if mode != sdk.ProvideControlModeNever {
		panic("provider cleanup did not disable provide control")
	}
	*self.events = append(*self.events, "provide-never")
}

func (self *blockingPeerProviderDevice) SetTunnelStarted(tunnelStarted bool) {
	if tunnelStarted {
		panic("provider cleanup left the tunnel started")
	}
	*self.events = append(*self.events, "tunnel-stopped")
}

func (self *blockingPeerProviderDevice) CloseAndWait(ctx context.Context) error {
	*self.events = append(*self.events, "device-close-start")
	if self.entered != nil {
		close(self.entered)
	}
	select {
	case <-ctx.Done():
		*self.events = append(*self.events, "device-close-timeout")
		return ctx.Err()
	case <-self.release:
		*self.events = append(*self.events, "device-close-done")
		return nil
	}
}

func peerProviderLifecycleTestCallbacks(events *[]string) (func() error, func() error, func()) {
	return func() error {
			*events = append(*events, "remove-client")
			return nil
		}, func() error {
			*events = append(*events, "logout")
			return nil
		}, func() {
			*events = append(*events, "close-manager")
		}
}

// A provider handoff accepts exactly one canonical SDK id.
func TestReadRetainedClientRequiresOneCanonicalId(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-client")
	id := sdk.NewId().String()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readRetainedClient(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("readRetainedClient() = %q, want %q", got, id)
	}

	for name, value := range map[string]string{
		"empty":      "\n",
		"two-values": id + "\n" + sdk.NewId().String() + "\n",
		"malformed":  "client-1\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := readRetainedClient(path); err == nil {
			t.Errorf("%s: readRetainedClient accepted %q", name, value)
		}
	}
}

// Traffic proof rejects one-way movement and counter resets.
func TestPacketStatsDeltaRequiresMonotonicBidirectionalTraffic(t *testing.T) {
	before := packetStatsSnapshot{
		remoteEgressPackets:  10,
		remoteEgressBytes:    1000,
		remoteIngressPackets: 9,
		remoteIngressBytes:   900,
	}
	tests := []struct {
		name          string
		after         packetStatsSnapshot
		wantMonotonic bool
		wantTraffic   bool
	}{
		{
			name: "bidirectional",
			after: packetStatsSnapshot{
				remoteEgressPackets:  11,
				remoteEgressBytes:    1100,
				remoteIngressPackets: 10,
				remoteIngressBytes:   1000,
			},
			wantMonotonic: true,
			wantTraffic:   true,
		},
		{
			name: "request-only",
			after: packetStatsSnapshot{
				remoteEgressPackets:  11,
				remoteEgressBytes:    1100,
				remoteIngressPackets: 9,
				remoteIngressBytes:   900,
			},
			wantMonotonic: true,
		},
		{
			name: "response-only",
			after: packetStatsSnapshot{
				remoteEgressPackets:  10,
				remoteEgressBytes:    1000,
				remoteIngressPackets: 10,
				remoteIngressBytes:   1000,
			},
			wantMonotonic: true,
		},
		{
			name: "counter-reset",
			after: packetStatsSnapshot{
				remoteEgressPackets:  1,
				remoteEgressBytes:    100,
				remoteIngressPackets: 1,
				remoteIngressBytes:   100,
			},
		},
	}
	for _, test := range tests {
		delta, monotonic := packetStatsDelta(before, test.after)
		if monotonic != test.wantMonotonic {
			t.Errorf("%s: monotonic = %t, want %t", test.name, monotonic, test.wantMonotonic)
		}
		if delta.bidirectional() != test.wantTraffic {
			t.Errorf("%s: bidirectional = %t, want %t; delta=%+v", test.name, delta.bidirectional(), test.wantTraffic, delta)
		}
	}
}

// Peer discovery must begin while the existing provider keeps the installed
// tunnel and device RPC alive. This is the deterministic form of the Windows
// watchdog failure that appeared only after an explicit pre-discovery
// disconnect.
func TestRequirePeerDiscoveryConnection(t *testing.T) {
	if err := requirePeerDiscoveryConnection(sdk.Connected); err != nil {
		t.Fatalf("connected tunnel rejected: %v", err)
	}
	for _, status := range []sdk.ConnectionStatus{
		sdk.Disconnected,
		sdk.Connecting,
		sdk.DestinationSet,
		sdk.ConnectFailed,
	} {
		if err := requirePeerDiscoveryConnection(status); err == nil {
			t.Errorf("status %q accepted without an active connection", status)
		}
	}
}

// A stale Connected status from the public provider is not proof that the
// asynchronous switch reached the controlled peer.
func TestControlledNetworkPeerConnectedRequiresExactPeerLocation(t *testing.T) {
	providerId := sdk.NewId()
	otherId := sdk.NewId()
	exact := &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{ClientId: providerId},
		NetworkPeer:       true,
	}
	tests := []struct {
		name     string
		status   sdk.ConnectionStatus
		location *sdk.ConnectLocation
		want     bool
	}{
		{name: "exact peer", status: sdk.Connected, location: exact, want: true},
		{name: "stale public provider", status: sdk.Connected, location: &sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true}}},
		{name: "wrong peer", status: sdk.Connected, location: &sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{ClientId: otherId}, NetworkPeer: true}},
		{name: "device without network-peer trust", status: sdk.Connected, location: &sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{ClientId: providerId}}},
		{name: "location switched but still connecting", status: sdk.Connecting, location: exact},
		{name: "missing location", status: sdk.Connected},
	}
	for _, test := range tests {
		if got := controlledNetworkPeerConnected(test.status, test.location, providerId); got != test.want {
			t.Errorf("%s: connected = %t, want %t", test.name, got, test.want)
		}
	}
	if controlledNetworkPeerConnected(sdk.Connected, exact, nil) {
		t.Error("nil controlled provider ID was accepted")
	}
}

// The new peer location can become visible before the outgoing provider's
// Connected status is cleared. That hybrid snapshot must not start the traffic
// proof until the new connection generation has published a transition.
func TestControlledNetworkPeerConnectionRequiresObservedGenerationTransition(t *testing.T) {
	providerId := sdk.NewId()
	exact := &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{ClientId: providerId},
		NetworkPeer:       true,
	}
	observer := &controlledPeerConnectionObserver{}

	if observer.connected(sdk.Connected, exact, providerId) {
		t.Fatal("outgoing Connected status and new peer location were accepted together")
	}
	observer.observeStatus(sdk.Connected)
	if observer.connected(sdk.Connected, exact, providerId) {
		t.Fatal("another terminal status armed the new connection generation")
	}
	observer.observeStatus(sdk.DestinationSet)
	if observer.connected(sdk.Connecting, exact, providerId) {
		t.Fatal("in-progress peer generation was accepted")
	}
	if !observer.connected(sdk.Connected, exact, providerId) {
		t.Fatal("connected peer was rejected after its generation transition")
	}
}

// Provider results remain owner-readable only.
func TestWritePrivateJSONIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-result.json")
	if err := writePrivateJSON(path, peerProviderResult{Ok: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result mode = %o, want 600", info.Mode().Perm())
	}
}

// The provider result is a success marker, never a stale proof published
// before a later lifecycle cleanup failure.
func TestCompletePeerProviderPublishesOnlyAfterSuccessfulCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-result.json")
	proof := &peerProviderResult{
		Ok:                   true,
		RemoteEgressPackets:  1,
		RemoteEgressBytes:    2,
		RemoteIngressPackets: 3,
		RemoteIngressBytes:   4,
	}
	cleanupErr := errors.New("remove provider client")
	if err := completePeerProvider(path, proof, cleanupErr); !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error = %v, want %v", err, cleanupErr)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup failure published a success marker: %v", err)
	}
	if err := completePeerProvider(path, proof, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("successful cleanup did not publish the marker: %v", err)
	}
}

// DeviceLocal.Close is callback-safe but non-joining. The provider must not
// revoke its client JWT or cancel the shared strategy until CloseAndWait has
// released every admitted post-client-close contract control.
func TestPeerProviderLifecycleJoinsDeviceBeforeIdentityTeardown(t *testing.T) {
	events := []string{}
	entered := make(chan struct{})
	release := make(chan struct{})
	removeStarted := make(chan struct{})
	removeClient, logout, closeManager := peerProviderLifecycleTestCallbacks(&events)
	lifecycle := &peerProviderLifecycle{
		device: &blockingPeerProviderDevice{
			events:  &events,
			entered: entered,
			release: release,
		},
		removeClient: func() error {
			close(removeStarted)
			return removeClient()
		},
		logout:        logout,
		closeManager:  closeManager,
		deviceTimeout: time.Second,
	}
	done := make(chan error, 1)
	go func() {
		done <- lifecycle.close()
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("device join did not start")
	}
	select {
	case <-removeStarted:
		t.Fatal("provider identity teardown overtook the device join")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := []string{
		"provide-never",
		"tunnel-stopped",
		"device-close-start",
		"device-close-done",
		"remove-client",
		"logout",
		"close-manager",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("cleanup order = %v, want %v", events, want)
	}
}

// A stuck device join is bounded and fails the provider proof, while later
// resource cleanup still runs. That preserves one-shot cleanup without leaving
// a provider process or local credential behind.
func TestPeerProviderLifecycleFailsClosedWhenDeviceJoinBlocks(t *testing.T) {
	events := []string{}
	resultPath := filepath.Join(t.TempDir(), "provider-result.json")
	removeClient, logout, closeManager := peerProviderLifecycleTestCallbacks(&events)
	lifecycle := &peerProviderLifecycle{
		device: &blockingPeerProviderDevice{
			events:  &events,
			release: make(chan struct{}),
		},
		removeClient:  removeClient,
		logout:        logout,
		closeManager:  closeManager,
		deviceTimeout: 10 * time.Millisecond,
	}

	err := finishPeerProvider(resultPath, &peerProviderResult{Ok: true}, nil, lifecycle)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked close error = %v, want deadline exceeded", err)
	}
	if _, statErr := os.Stat(resultPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("blocked device join published provider success: %v", statErr)
	}
	want := []string{
		"provide-never",
		"tunnel-stopped",
		"device-close-start",
		"device-close-timeout",
		"remove-client",
		"logout",
		"close-manager",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("blocked cleanup order = %v, want %v", events, want)
	}
}
