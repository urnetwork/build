// Unit tests cover private fixture handling and local parsing without touching
// a production account or network client.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

type recordingNetworkClientRemover struct {
	args   *sdk.RemoveNetworkClientArgs
	byJwt  string
	result *sdk.RemoveNetworkClientResult
	err    error
}

// Records the SDK cleanup call without opening a network connection.
func (self *recordingNetworkClientRemover) RemoveNetworkClientSyncWithContextAndJwt(
	ctx context.Context,
	args *sdk.RemoveNetworkClientArgs,
	byJwt string,
) (*sdk.RemoveNetworkClientResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("cleanup context has no deadline")
	}
	self.args = args
	self.byJwt = byJwt
	return self.result, self.err
}

type publicIpRoundTripper func(request *http.Request) (*http.Response, error)

// Implements the deterministic transport seam used by public-address tests.
func (self publicIpRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// Forces the observed Android failure: the first hostname lookup fails while
// an independent endpoint on the same network remains healthy.
func TestPublicIpFallsBackAfterEndpointDnsFailure(t *testing.T) {
	urls := []string{"https://first.invalid/", "https://second.invalid/"}
	openedUrls := []string{}
	client := &http.Client{Transport: publicIpRoundTripper(func(request *http.Request) (*http.Response, error) {
		openedUrls = append(openedUrls, request.URL.String())
		if request.URL.String() == urls[0] {
			return nil, errors.New("deterministic DNS timeout")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("203.0.113.9\n")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}

	address, err := publicIpWithClient(context.Background(), client, urls)
	if err != nil {
		t.Fatal(err)
	}
	if address != "203.0.113.9" {
		t.Fatalf("public address = %q, want 203.0.113.9", address)
	}
	if !reflect.DeepEqual(openedUrls, urls) {
		t.Fatalf("opened urls = %q, want %q", openedUrls, urls)
	}
}

// Requires every independent endpoint to fail before the probe fails.
func TestPublicIpReportsEveryEndpointFailure(t *testing.T) {
	urls := []string{"https://first.invalid/", "https://second.invalid/"}
	client := &http.Client{Transport: publicIpRoundTripper(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("deterministic DNS timeout")
	})}

	_, err := publicIpWithClient(context.Background(), client, urls)
	if err == nil {
		t.Fatal("all-endpoint DNS failure was accepted")
	}
	for _, url := range urls {
		if !strings.Contains(err.Error(), url) {
			t.Fatalf("failure omitted %q: %v", url, err)
		}
	}
}

// Prevents an HTTP success page from being mistaken for an address.
func TestPublicIpRejectsInvalidEndpointBodies(t *testing.T) {
	client := &http.Client{Transport: publicIpRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("not an address\n")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}

	if _, err := publicIpWithClient(context.Background(), client, []string{"https://invalid.test/"}); err == nil {
		t.Fatal("invalid public IP response was accepted")
	}
}

// Reproduces the Linux teardown failure at its ownership boundary: cleanup
// must use the still-live SDK API instead of raw net/http, whose resolver can
// retain the tunnel-only DNS server after /etc/resolv.conf is restored.
func TestRemoveClientUsesSdkNetworkTransport(t *testing.T) {
	clientId := "00000000-0000-0000-0000-000000000123"
	remover := &recordingNetworkClientRemover{result: &sdk.RemoveNetworkClientResult{}}
	const networkJwt = "network-owner-jwt"
	if err := removeClient(remover, networkJwt, clientId); err != nil {
		t.Fatal(err)
	}
	if remover.args == nil || remover.args.ClientId == nil || remover.args.ClientId.String() != clientId {
		t.Fatalf("remove args = %+v, want client %s", remover.args, clientId)
	}
	if remover.byJwt != networkJwt {
		t.Fatalf("cleanup jwt = %q, want explicit network-owner jwt", remover.byJwt)
	}
}

// Keeps already-removed cleanup idempotent while changing the transport.
func TestRemoveClientAcceptsAnAlreadyRemovedClient(t *testing.T) {
	remover := &recordingNetworkClientRemover{result: &sdk.RemoveNetworkClientResult{
		Error: &sdk.ApiError{Message: "Client does not exist."},
	}}
	if err := removeClient(remover, "network-owner-jwt", "00000000-0000-0000-0000-000000000123"); err != nil {
		t.Fatalf("already removed client = %v", err)
	}
}

// Keeps the acceptance runner aligned with the two independent desktop
// service protocols. Windows is intentionally newer than Linux; treating the
// Linux version as a universal constant rejects a correctly packaged MSI.
func TestValidateControlProtocolUsesPlatformContract(t *testing.T) {
	for _, test := range []struct {
		name     string
		platform string
		version  int
	}{
		{name: "linux", platform: "linux", version: 1},
		{name: "windows", platform: "windows", version: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateControlProtocol(test.platform, test.version); err != nil {
				t.Fatalf("valid %s protocol rejected: %v", test.platform, err)
			}
		})
	}

	if err := validateControlProtocol("windows", 1); err == nil {
		t.Fatal("Windows accepted the Linux control protocol")
	}
	if err := validateControlProtocol("darwin", 1); err == nil {
		t.Fatal("unsupported platform accepted a control protocol")
	}
}

// Requires every service start to carry one complete, named mTLS generation.
func TestPinnedStartTunnelPayloadCarriesCompleteDeviceRpcGeneration(t *testing.T) {
	config := tunnelConfig{
		ByJwt:             "client-jwt",
		NetworkSpaceJson:  "space-json",
		InstanceId:        "instance-id",
		AppVersion:        "app-version",
		RpcServerPem:      "server-pem",
		RpcClientCertPem:  "client-cert-pem",
		RpcListenHostPort: "127.0.0.1:12042",
		RpcSessionId:      "rpc-session-id",
	}
	want := map[string]any{
		"by_jwt":              config.ByJwt,
		"network_space_json":  config.NetworkSpaceJson,
		"instance_id":         config.InstanceId,
		"app_version":         config.AppVersion,
		"rpc_server_pem":      config.RpcServerPem,
		"rpc_client_cert_pem": config.RpcClientCertPem,
		"rpc_listen_hostport": config.RpcListenHostPort,
		"rpc_session_id":      config.RpcSessionId,
	}
	if got := pinnedStartTunnelPayload(config); !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned start payload = %#v, want %#v", got, want)
	}
}

// Prevents the Docker acceptance agent from inheriting the daemon's cgroup-BPF
// socket mark and bypassing the tunnel it is supposed to measure.
func TestLinuxRunnerIsolatesDaemonCgroupBeforeExec(t *testing.T) {
	scriptBytes, err := os.ReadFile("run-linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	move := strings.Index(script, `printf '%s\n' "$BASHPID" >"$daemon_cgroup/cgroup.procs"`)
	exec := strings.Index(script, `exec "$daemon" --foreground`)
	agent := strings.Index(script, `URNETWORK_CONTROL_SOCKET="$socket" "$agent"`)
	if move < 0 || exec < 0 || agent < 0 {
		t.Fatalf("Linux runner is missing the daemon cgroup move, exec, or agent boundary")
	}
	if !(move < exec && exec < agent) {
		t.Fatalf("Linux runner ordering is move=%d exec=%d agent=%d, want move < exec < agent", move, exec, agent)
	}
	if !strings.Contains(script, `daemon_cgroup_path="$(sed -n 's/^0::\///p' "/proc/$daemon_pid/cgroup")"`) {
		t.Fatal("Linux runner does not verify the daemon's live cgroup after launch")
	}
}

// A functional result cannot overrule evidence that the installed daemon left
// a Pion ICE worker behind. The first main run remained silent for 18 minutes
// after its assertions and was still recorded as PASS; keep the live detector,
// stack capture, and terminal gate together and ordered around the agent.
func TestLinuxRunnerFailsOnWebRtcTeardownStall(t *testing.T) {
	scriptBytes, err := os.ReadFile("run-linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	agentStart := strings.Index(script, `agent_pid=$!`)
	watchdogStart := strings.Index(script, "watch_teardown_stall &")
	agentWait := strings.Index(script, `wait "$agent_pid"`)
	terminalGate := strings.LastIndex(script, `teardown-stall.detected`)
	if agentStart < 0 || watchdogStart < 0 || agentWait < 0 || terminalGate < 0 {
		t.Fatal("Linux runner is missing the agent owner, teardown watcher, wait, or terminal gate")
	}
	if !(agentStart < watchdogStart && watchdogStart < agentWait && agentWait < terminalGate) {
		t.Fatalf(
			"Linux teardown gate ordering is agent=%d watcher=%d wait=%d gate=%d",
			agentStart,
			watchdogStart,
			agentWait,
			terminalGate,
		)
	}
	for _, required := range []string{
		`\[peerconn\]teardown stalled`,
		`kill -QUIT "$stalled_pid"`,
		`"$UR_ACCEPT_ARTIFACTS/teardown-stall.detected"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("Linux teardown gate is missing %q", required)
		}
	}
}

// Uses fixed entropy to prove the session name is opaque and deterministic at
// the boundary, while production supplies crypto/rand.Reader.
func TestMintRpcSessionId(t *testing.T) {
	got, err := mintRpcSessionId(bytes.NewReader(bytes.Repeat([]byte{0xff}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("ff", 16) {
		t.Fatalf("RPC session ID = %q, want 128 bits of hexadecimal entropy", got)
	}
	if _, err := mintRpcSessionId(bytes.NewReader(nil)); err == nil {
		t.Fatal("RPC session ID accepted insufficient entropy")
	}
}

// Records every SDK session transition made by the guest lifecycle verifier.
type recordingGuestSession struct {
	value       string
	transitions []string
}

// Sets the current test session and retains the transition for exact ordering
// assertions.
func (self *recordingGuestSession) SetByJwt(value string) {
	self.value = value
	self.transitions = append(self.transitions, value)
}

// Returns the current test session.
func (self *recordingGuestSession) GetByJwt() string {
	return self.value
}

// Covers whitespace normalization and strict word-count rejection.
func TestNormalizeSecret(t *testing.T) {
	words := make([]string, 24)
	for i := range words {
		words[i] = "Word"
	}
	input := "  " + strings.Join(words, "\n") + "  "
	want := strings.TrimSpace(strings.ToLower(strings.ReplaceAll(input, "\n", " ")))
	if got := normalizeSecret(input); got != want {
		t.Fatalf("normalizeSecret() = %q, want %q", got, want)
	}
	if got := normalizeSecret("only two"); got != "" {
		t.Fatalf("normalizeSecret accepted an invalid secret: %q", got)
	}
}

// Covers atomic owner-only fixture persistence and reload.
func TestSecretFixtureRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture")
	secret := strings.TrimSpace(strings.Repeat("word ", 24))
	if err := writeSecret(path, secret); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("fixture mode = %o, want 600", info.Mode().Perm())
	}
	got, err := readSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("readSecret() = %q, want %q", got, secret)
	}
}

// Covers the retained client marker used after a killed VM or container.
func TestActiveClientRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "active-client-id")
	if err := writeActiveClient(path, "client-1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "client-1\n" {
		t.Fatalf("active client = %q, want client-1", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("active client mode = %o, want 600", info.Mode().Perm())
	}
	if err := removeActiveClient(path); err != nil {
		t.Fatal(err)
	}
	if err := removeActiveClient(path); err != nil {
		t.Fatalf("removeActiveClient is not idempotent: %v", err)
	}
}

// Requires an account with no usable credential to be deleted immediately.
func TestRetainGuestFixtureDeletesUnrecoverableAccount(t *testing.T) {
	deletedJwt := ""
	_, err := retainGuestFixture(
		filepath.Join(t.TempDir(), "fixture"),
		"not a secret",
		"guest-jwt",
		func(networkJwt string) error {
			deletedJwt = networkJwt
			return nil
		},
	)
	if err == nil {
		t.Fatal("retainGuestFixture accepted an unrecoverable account")
	}
	if deletedJwt != "guest-jwt" {
		t.Fatalf("deleted jwt = %q, want guest-jwt", deletedJwt)
	}
}

// Requires a fixture persistence failure to delete the new account and report
// both persistence and cleanup errors.
func TestRetainGuestFixtureReportsPersistenceAndCleanupFailures(t *testing.T) {
	secret := strings.TrimSpace(strings.Repeat("word ", 24))
	fixtureDirectory := t.TempDir()
	_, err := retainGuestFixture(
		fixtureDirectory,
		secret,
		"guest-jwt",
		func(networkJwt string) error {
			if networkJwt != "guest-jwt" {
				t.Fatalf("deleted jwt = %q, want guest-jwt", networkJwt)
			}
			return os.ErrPermission
		},
	)
	if err == nil {
		t.Fatal("retainGuestFixture accepted a fixture persistence failure")
	}
	message := err.Error()
	if !strings.Contains(message, "persist instant-account secret") ||
		!strings.Contains(message, "delete instant account") {
		t.Fatalf("combined error = %q", message)
	}
}

// Requires the guest lifecycle to log out before recovery and to log out again
// after the recovered session has been installed and verified.
func TestVerifyGuestSessionRecoveryLogsOutBothSessions(t *testing.T) {
	firstJwt := testNetworkJwt("network-1")
	secondJwt := testNetworkJwt("network-1")
	session := &recordingGuestSession{}
	recoveryCalls := 0
	err := verifyGuestSessionRecovery(session, firstJwt, func() (string, error) {
		recoveryCalls++
		if session.GetByJwt() != "" {
			t.Fatalf("recovery started with active session %q", session.GetByJwt())
		}
		return secondJwt, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recoveryCalls != 1 {
		t.Fatalf("recovery calls = %d, want 1", recoveryCalls)
	}
	wantTransitions := []string{firstJwt, "", secondJwt, ""}
	if !reflect.DeepEqual(session.transitions, wantTransitions) {
		t.Fatalf("session transitions = %q, want %q", session.transitions, wantTransitions)
	}
	if session.GetByJwt() != "" {
		t.Fatalf("recovered session remained active: %q", session.GetByJwt())
	}
}

// Requires a failed network-identity assertion to log out of the recovered
// session before returning the failure.
func TestVerifyGuestSessionRecoveryLogsOutAfterNetworkMismatch(t *testing.T) {
	firstJwt := testNetworkJwt("network-1")
	secondJwt := testNetworkJwt("network-2")
	session := &recordingGuestSession{}
	err := verifyGuestSessionRecovery(session, firstJwt, func() (string, error) {
		return secondJwt, nil
	})
	if err == nil || !strings.Contains(err.Error(), "different network") {
		t.Fatalf("network mismatch error = %v", err)
	}
	wantTransitions := []string{firstJwt, "", secondJwt, ""}
	if !reflect.DeepEqual(session.transitions, wantTransitions) {
		t.Fatalf("session transitions = %q, want %q", session.transitions, wantTransitions)
	}
	if session.GetByJwt() != "" {
		t.Fatalf("mismatched recovered session remained active: %q", session.GetByJwt())
	}
}

// Prevents malformed mounted credentials from shifting password boundaries.
func TestReadCredentialsRejectsExtraLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("user\npass\nextra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCredentials(path); err == nil {
		t.Fatal("readCredentials accepted an extra line")
	}
}

// Covers claim extraction and required-claim rejection.
func TestJwtStringClaim(t *testing.T) {
	token := testNetworkJwt("network-1")
	got, err := jwtStringClaim(token, "network_id")
	if err != nil {
		t.Fatal(err)
	}
	if got != "network-1" {
		t.Fatalf("claim = %q, want network-1", got)
	}
	if _, err := jwtStringClaim(token, "client_id"); err == nil {
		t.Fatal("missing claim was accepted")
	}
}

// Creates a structurally valid test JWT with the requested network claim.
func testNetworkJwt(networkId string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"network_id":"` + networkId + `"}`))
	return "header." + payload + ".signature"
}
