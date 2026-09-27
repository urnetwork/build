// Mock-only regression controls for failure evidence; no hosted requests.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/urnetwork/build/all/acceptance/authcases"
	"github.com/urnetwork/build/all/acceptance/testconfig"
	"github.com/urnetwork/sdk"
	"github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"
)

func reportingFixture(t *testing.T) options {
	t.Helper()
	root := t.TempDir()
	privateKey := solana.PrivateKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)))
	const mnemonic = "bottom drive obey lake curtain smoke basket hold race lonely fit walk"
	pair, err := subkey.DeriveKeyPair(sr25519.Scheme{}, mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	config := testconfig.Config{
		Version:           1,
		Android:           testconfig.Android{UnlockCode: "010181"},
		Lifecycle:         testconfig.Lifecycle{AllowAccountCreateDelete: true},
		EmailVerification: testconfig.EmailVerification{BypassDomains: []string{"acceptance.invalid"}, SuppressAccountMessages: true},
		DataPlaneAccount:  testconfig.DataPlaneAccount{Email: "private-data@example.com", Password: "private-data-password"},
		Signup: testconfig.Signup{NetworkNamePrefix: "acceptance", Password: "private-signup-password",
			SeedphraseRateLimitBypassIPs: []string{"192.0.2.10"},
			Email:                        testconfig.SignupEmail{Domain: "acceptance.invalid", LocalPartPrefix: "case"},
			Phone:                        testconfig.SignupPhone{Number: "+15555550123"}},
		Providers: testconfig.Providers{
			Google: testconfig.ProviderGoogle{Email: "google@example.com", Password: "private-google-password", TOTPSecret: "JBSWY3DPEHPK3PXP", RecoveryEmail: "recovery@example.com", BrowserProfile: "google.json"},
			Apple:  testconfig.ProviderApple{Email: "apple@example.com", Password: "private-apple-password", BrowserProfile: "apple.json"}},
		Wallets: testconfig.Wallets{
			Solana:    testconfig.SolanaWallet{Address: privateKey.PublicKey().String(), PrivateKeyBase58: privateKey.String()},
			Bittensor: testconfig.BittensorWallet{Address: pair.SS58Address(42), Mnemonic: mnemonic, SS58Prefix: 42}},
	}
	data, err := json.Marshal(&config)
	if err != nil {
		t.Fatal(err)
	}
	opts := options{
		Credentials: filepath.Join(root, "credentials"), Tests: filepath.Join(root, "tests.json"),
		Fixture: filepath.Join(root, "secret-key"), ActiveClient: filepath.Join(root, "active-client"),
		PeerProviderClient: filepath.Join(root, "provider-client"), StateDir: filepath.Join(root, "state"),
		SdkVersion: "test", AppVersion: "test", ServiceVersion: "test", Repeat: 2,
	}
	for name, value := range map[string][]byte{
		opts.Credentials:        []byte("private-data@example.com\nprivate-data-password\n"),
		opts.Tests:              data,
		opts.PeerProviderClient: []byte("00000000-0000-0000-0000-000000000123\n"),
	} {
		if err := os.WriteFile(name, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return opts
}

func failedAuthReportingRun(t *testing.T) (*acceptanceResult, error, string) {
	t.Helper()
	opts := reportingFixture(t)
	previousTransport := http.DefaultTransport
	previousStderr := os.Stderr
	previousSDKVersion := sdk.Version
	logPath := filepath.Join(t.TempDir(), "stderr")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		http.DefaultTransport = previousTransport
		os.Stderr = previousStderr
		sdk.Version = previousSDKVersion
		_ = logFile.Close()
	})
	var routes []string
	http.DefaultTransport = publicIpRoundTripper(func(request *http.Request) (*http.Response, error) {
		routes = append(routes, request.URL.Path)
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, errors.Join(context.DeadlineExceeded, errors.New("private-data@example.com private-data-password private-signup-password"))
	})
	os.Stderr = logFile
	result, runErr := run(opts)
	os.Stderr = previousStderr
	http.DefaultTransport = previousTransport
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	wantRoutes := []string{"/auth/network-create", "/auth/login-with-password", "/auth/wallet-challenge", "/auth/wallet-challenge"}
	if !reflect.DeepEqual(routes, wantRoutes) {
		t.Fatalf("attempted routes = %v, want %v", routes, wantRoutes)
	}
	t.Log("all four auth lifecycles reached the injected transport; no sockets opened")
	if runErr == nil {
		t.Fatal("transport failures must retain the overall failure")
	}
	if strings.Contains(string(logBytes)+runErr.Error(), "private-") {
		t.Fatal("reporting exposed a configured private fixture value")
	}
	return result, runErr, string(logBytes)
}

func TestAuthCaseFailurePreservesCompletedResults(t *testing.T) {
	result, _, _ := failedAuthReportingRun(t)
	if result == nil {
		t.Fatal("all four auth outcomes were discarded after the first failure")
	}
	if result.Ok {
		t.Fatal("partial failed run was mislabeled successful")
	}
	if result.Complete || result.CompletedRepetitions != 0 || result.Repetitions != 2 {
		t.Fatal("partial failure changed requested repetitions or invented completed work")
	}
	if len(result.AuthCases) != 4 {
		t.Fatalf("auth results = %d, want all four completed rows", len(result.AuthCases))
	}
	for index, name := range []string{"email", "phone", "solana", "bittensor"} {
		if got := result.AuthCases[index]; got.Case != name || got.Status != "FAIL" {
			t.Fatalf("row %d lost its outcome", index)
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-", "00000000-0000-0000-0000-000000000123"} {
		if strings.Contains(string(data), private) {
			t.Fatal("partial JSON exposed a private fixture value")
		}
	}
	if len(result.BeforeIps) != 0 || len(result.AfterIps) != 0 || len(result.PeerToPeer) != 0 {
		t.Fatal("auth failure fabricated unexecuted data-plane outcomes")
	}
}

func TestAuthFailureLogsEveryCompletedCase(t *testing.T) {
	_, _, logs := failedAuthReportingRun(t)
	for _, name := range []string{"email", "phone", "solana", "bittensor"} {
		if !strings.Contains(logs, "acceptance: auth case "+name+": FAIL") {
			t.Errorf("completed %s outcome was hidden", name)
		}
	}
}

func TestWriteAcceptanceResultPreservesFailedAuthJSON(t *testing.T) {
	result, runErr, _ := failedAuthReportingRun(t)
	var stdout, stderr bytes.Buffer
	if got := writeAcceptanceResult(&stdout, &stderr, result, runErr); got != 1 {
		t.Fatalf("failure exit = %d, want 1", got)
	}
	var decoded acceptanceResult
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("partial result was not valid JSON: %v", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatal("stdout must contain exactly one JSON record")
	}
	if decoded.Ok || decoded.Complete || decoded.CompletedRepetitions != 0 || len(decoded.AuthCases) != 4 {
		t.Fatal("failed auth evidence was lost or marked complete")
	}
	data, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-", "00000000-0000-0000-0000-000000000123"} {
		if strings.Contains(string(data)+stderr.String(), private) {
			t.Fatal("JSON or stderr exposed private fixture data")
		}
	}
}

func TestWriteAcceptanceResultCompletionContract(t *testing.T) {
	for _, test := range []struct {
		name         string
		result       *acceptanceResult
		err          error
		wantCode     int
		wantOK       bool
		wantComplete bool
	}{
		{name: "success", result: &acceptanceResult{Ok: true, Complete: true, Repetitions: 2, CompletedRepetitions: 2}, wantOK: true, wantComplete: true},
		{name: "partial repetition", result: &acceptanceResult{Repetitions: 2, CompletedRepetitions: 1}, err: errors.New("repetition failed"), wantCode: 1},
		{name: "error overrides success", result: &acceptanceResult{Ok: true, Complete: true}, err: errors.New("late failure"), wantCode: 1},
		{name: "incomplete cannot pass", result: &acceptanceResult{Ok: true}, wantCode: 1},
		{name: "setup failure", err: errors.New("setup failed"), wantCode: 1},
		{name: "missing result", wantCode: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := writeAcceptanceResult(&stdout, &stderr, test.result, test.err); got != test.wantCode {
				t.Fatalf("exit = %d, want %d", got, test.wantCode)
			}
			if test.result == nil {
				if stdout.Len() != 0 || stderr.Len() == 0 {
					t.Fatal("no result must not fabricate completed cases")
				}
				return
			}
			var decoded acceptanceResult
			if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Ok != test.wantOK || decoded.Complete != test.wantComplete {
				t.Fatal("unexpected completion classification")
			}
			if decoded.Repetitions != test.result.Repetitions || decoded.CompletedRepetitions != test.result.CompletedRepetitions {
				t.Fatal("reporting changed requested or completed repetitions")
			}
		})
	}
}

type failedResultWriter struct{}

func (failedResultWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWriteAcceptanceResultEncodingFailureFailsClosed(t *testing.T) {
	var stderr bytes.Buffer
	result := &acceptanceResult{Ok: true, Complete: true, Repetitions: 1, CompletedRepetitions: 1}
	if got := writeAcceptanceResult(failedResultWriter{}, &stderr, result, nil); got != 1 {
		t.Fatal("JSON write failure was accepted")
	}
	if !strings.Contains(stderr.String(), "encode result") {
		t.Fatal("JSON failure was not reported")
	}
}

func TestReportAuthCasesPreservesMixedOutcomes(t *testing.T) {
	results := []authcases.Result{
		{Case: "email", Status: "FAIL", Detail: "timeout"},
		{Case: "phone", Status: "PASS"},
		{Case: "solana", Status: "FAIL", Detail: "timeout"},
		{Case: "bittensor", Status: "PASS"},
	}
	var stderr bytes.Buffer
	err := reportAuthCases(&stderr, results)
	if err == nil || strings.Count(err.Error(), "auth case") != 2 {
		t.Fatal("mixed auth outcomes lost failures or invented another failure")
	}
	for _, result := range results {
		if !strings.Contains(stderr.String(), "auth case "+result.Case+": "+result.Status) {
			t.Fatalf("completed %s status was not preserved", result.Case)
		}
	}
	stderr.Reset()
	if err := reportAuthCases(&stderr, []authcases.Result{{Case: "email", Status: "PASS"}}); err != nil {
		t.Fatal("reporting changed a passing outcome")
	}
}

// Linux first rejects a nonzero agent exit, then searches for the compact
// "ok":true token. Windows first rejects nonzero exit, then reads ok/platform/
// repetitions from ConvertFrom-Json; neither rejects additional JSON fields.
// Keep the legacy success gate valid even when failure detail resembles JSON.
func TestAcceptanceResultPreservesLegacyPlatformGates(t *testing.T) {
	for _, complete := range []bool{false, true} {
		result := &acceptanceResult{Ok: true, Complete: complete, Platform: "windows", Repetitions: 1,
			AuthCases: []authcases.Result{{Case: "email", Status: "FAIL", Detail: `untrusted "ok":true`}}}
		var stdout, stderr bytes.Buffer
		code := writeAcceptanceResult(&stdout, &stderr, result, nil)
		var legacy struct {
			OK          bool   `json:"ok"`
			Platform    string `json:"platform"`
			Repetitions int    `json:"repetitions"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &legacy); err != nil {
			t.Fatal(err)
		}
		if legacy.OK != complete || legacy.Platform != "windows" || legacy.Repetitions != 1 {
			t.Fatal("additive completion fields changed the legacy JSON result contract")
		}
		if got := bytes.Contains(stdout.Bytes(), []byte(`"ok":true`)); got != complete {
			t.Fatal("incomplete JSON could spoof the Linux success marker")
		}
		if (code == 0) != complete {
			t.Fatal("incomplete evidence could pass the platform process-exit gate")
		}
	}
}
