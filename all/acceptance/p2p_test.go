// Controlled peer tests keep fixture parsing and traffic proof deterministic.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sdk"
)

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
