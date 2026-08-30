package authcases

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/urnetwork/build/all/acceptance/testconfig"
	"github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"
)

const lifecycleMnemonic = "bottom drive obey lake curtain smoke basket hold race lonely fit walk"

func lifecycleConfig(t *testing.T) *testconfig.Config {
	t.Helper()
	privateKey, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPair, err := subkey.DeriveKeyPair(sr25519.Scheme{}, lifecycleMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	return &testconfig.Config{
		Version:   1,
		Lifecycle: testconfig.Lifecycle{AllowAccountCreateDelete: true},
		EmailVerification: testconfig.EmailVerification{
			BypassDomains: []string{"acceptance.invalid"}, SuppressAccountMessages: true,
		},
		DataPlaneAccount: testconfig.DataPlaneAccount{Email: "data@example.com", Password: "data-password"},
		Signup: testconfig.Signup{
			NetworkNamePrefix: "acceptance", Password: "signup-password",
			Email: testconfig.SignupEmail{Domain: "acceptance.invalid", LocalPartPrefix: "case"},
			Phone: testconfig.SignupPhone{Number: "+15555550123"},
		},
		Providers: testconfig.Providers{
			Google: testconfig.ProviderGoogle{Email: "google@example.com", Password: "x", TOTPSecret: "JBSWY3DPEHPK3PXP", RecoveryEmail: "r@example.com", BrowserProfile: "google.json"},
			Apple:  testconfig.ProviderApple{Email: "apple@example.com", Password: "x", BrowserProfile: "apple.json"},
		},
		Wallets: testconfig.Wallets{
			Solana:    testconfig.SolanaWallet{Address: privateKey.PublicKey().String(), PrivateKeyBase58: privateKey.String()},
			Bittensor: testconfig.BittensorWallet{Address: keyPair.SS58Address(42), Mnemonic: lifecycleMnemonic, SS58Prefix: 42},
		},
	}
}

type fakeAuthAPI struct {
	t             *testing.T
	mutex         sync.Mutex
	challenge     int
	used          map[string]bool
	walletCreated map[string]bool
	passwordMade  map[string]bool
	verified      map[string]bool
	deletes       int
}

func (f *fakeAuthAPI) serve(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	var body map[string]any
	if request.Body != nil {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			f.t.Errorf("decode %s: %v", request.URL.Path, err)
		}
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	switch request.URL.Path {
	case "/auth/wallet-challenge":
		address, _ := body["wallet_address"].(string)
		if address == "" {
			f.t.Error("challenge was not address-bound")
		}
		f.challenge++
		message := fmt.Sprintf("challenge-%d", f.challenge)
		f.used[message] = false
		_ = json.NewEncoder(response).Encode(map[string]any{"message_template": message})
	case "/auth/login":
		auth := body["wallet_auth"].(map[string]any)
		message := auth["wallet_message"].(string)
		address := auth["wallet_address"].(string)
		if f.used[message] {
			f.t.Errorf("challenge %q was replayed", message)
		}
		f.used[message] = true
		if f.walletCreated[address] {
			_ = json.NewEncoder(response).Encode(map[string]any{"network": map[string]any{"by_jwt": fakeJWT(address)}})
		} else {
			_ = json.NewEncoder(response).Encode(map[string]any{"wallet_auth": auth})
		}
	case "/auth/network-create":
		if auth, ok := body["wallet_auth"].(map[string]any); ok {
			message := auth["wallet_message"].(string)
			address := auth["wallet_address"].(string)
			if f.used[message] {
				f.t.Errorf("wallet create replayed challenge %q", message)
			}
			f.used[message] = true
			f.walletCreated[address] = true
			_ = json.NewEncoder(response).Encode(map[string]any{"network": map[string]any{"by_jwt": fakeJWT(address)}})
			return
		}
		user := body["user_auth"].(string)
		f.passwordMade[user] = true
		f.verified[user] = true
		_ = json.NewEncoder(response).Encode(map[string]any{"network": map[string]any{"by_jwt": fakeJWT(user)}})
	case "/auth/login-with-password":
		user := body["user_auth"].(string)
		if !f.passwordMade[user] {
			_ = json.NewEncoder(response).Encode(map[string]any{"error": map[string]any{"message": "Invalid user or password."}})
		} else if !f.verified[user] {
			_ = json.NewEncoder(response).Encode(map[string]any{"verification_required": map[string]any{"user_auth": user}})
		} else {
			_ = json.NewEncoder(response).Encode(map[string]any{"network": map[string]any{"by_jwt": fakeJWT(user)}})
		}
	case "/auth/network-delete":
		f.deletes++
		for user := range f.passwordMade {
			if strings.Contains(request.Header.Get("Authorization"), jwtNetworkID(user)) {
				f.passwordMade[user] = false
				f.verified[user] = false
			}
		}
		for address := range f.walletCreated {
			if strings.Contains(request.Header.Get("Authorization"), jwtNetworkID(address)) {
				f.walletCreated[address] = false
			}
		}
		_ = json.NewEncoder(response).Encode(map[string]any{})
	default:
		http.Error(response, "unknown", http.StatusNotFound)
	}
}

func fakeJWT(identity string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"network_id":"`+jwtNetworkID(identity)+`"}`)) + ".sig"
}

func jwtNetworkID(identity string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(identity))
}

func TestRunFullDeterministicAuthLifecycle(t *testing.T) {
	fake := &fakeAuthAPI{
		t: t, used: map[string]bool{}, walletCreated: map[string]bool{},
		passwordMade: map[string]bool{}, verified: map[string]bool{},
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	config := lifecycleConfig(t)
	results := (&Runner{APIURL: server.URL, Config: config, Client: server.Client()}).Run(
		t.Context(), []string{"email", "phone", "solana", "bittensor"},
	)
	for _, result := range results {
		if result.Status != "PASS" {
			t.Fatalf("%s = %s: %s", result.Case, result.Status, result.Detail)
		}
	}
	if fake.challenge != 6 {
		t.Fatalf("wallet challenges = %d, want 6 (discovery/create/login per chain)", fake.challenge)
	}
	if fake.deletes != 4 {
		t.Fatalf("account deletes = %d, want 4", fake.deletes)
	}
}

func TestPhoneLifecycleRecoversExistingFixture(t *testing.T) {
	config := lifecycleConfig(t)
	phone := config.Signup.Phone.Number
	fake := &fakeAuthAPI{
		t: t, used: map[string]bool{}, walletCreated: map[string]bool{},
		passwordMade: map[string]bool{phone: true}, verified: map[string]bool{phone: true},
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	results := (&Runner{APIURL: server.URL, Config: config, Client: server.Client()}).Run(
		t.Context(), []string{"phone"},
	)
	if len(results) != 1 || results[0].Status != "PASS" {
		t.Fatalf("phone recovery = %#v", results)
	}
	if fake.deletes != 2 {
		t.Fatalf("account deletes = %d, want stale cleanup plus lifecycle cleanup", fake.deletes)
	}
}

func TestPhoneLifecycleAcceptsLegacyMissingFixtureResponse(t *testing.T) {
	config := lifecycleConfig(t)
	phone := config.Signup.Phone.Number
	fake := &fakeAuthAPI{
		t: t, used: map[string]bool{}, walletCreated: map[string]bool{},
		passwordMade: map[string]bool{}, verified: map[string]bool{},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/auth/login-with-password" && !fake.passwordMade[phone] {
			http.Error(response, "User does not exist.", http.StatusInternalServerError)
			return
		}
		fake.serve(response, request)
	}))
	defer server.Close()

	results := (&Runner{APIURL: server.URL, Config: config, Client: server.Client()}).Run(
		t.Context(), []string{"phone"},
	)
	if len(results) != 1 || results[0].Status != "PASS" {
		t.Fatalf("legacy clean phone recovery = %#v", results)
	}
}

func TestPhoneLifecycleRejectsUnrelatedPlainTextServerError(t *testing.T) {
	config := lifecycleConfig(t)
	fake := &fakeAuthAPI{
		t: t, used: map[string]bool{}, walletCreated: map[string]bool{},
		passwordMade: map[string]bool{}, verified: map[string]bool{},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/auth/login-with-password" {
			http.Error(response, "database unavailable", http.StatusInternalServerError)
			return
		}
		fake.serve(response, request)
	}))
	defer server.Close()

	results := (&Runner{APIURL: server.URL, Config: config, Client: server.Client()}).Run(
		t.Context(), []string{"phone"},
	)
	if len(results) != 1 || results[0].Status != "FAIL" ||
		!strings.Contains(results[0].Detail, "database unavailable") {
		t.Fatalf("unrelated phone server error = %#v", results)
	}
	if len(fake.passwordMade) != 0 {
		t.Fatalf("phone lifecycle continued after an unrelated server error: %#v", fake.passwordMade)
	}
}

func TestNetworkNameSuffixUsesServerAcceptedAlphabet(t *testing.T) {
	// Raw URL-safe base64 encoded this input as underscores, which the server
	// correctly rejects in network names. Hex is lowercase and stays inside the
	// public network-name contract for every possible random byte.
	got := encodeNetworkNameSuffix([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	if got != "ffffffffffff" {
		t.Fatalf("network-name suffix = %q, want lowercase hexadecimal", got)
	}
}

func TestRedactCoversProviderSecrets(t *testing.T) {
	config := lifecycleConfig(t)
	message := strings.Join([]string{
		config.Providers.Google.RecoveryEmail,
		config.Providers.Google.TOTPSecret,
	}, "\n")
	redacted := (&Runner{Config: config}).redact(message)
	if strings.Contains(redacted, config.Providers.Google.RecoveryEmail) ||
		strings.Contains(redacted, config.Providers.Google.TOTPSecret) {
		t.Fatalf("redact() left provider secrets in %q", redacted)
	}
	if strings.ContainsAny(redacted, "\r\n\t") {
		t.Fatalf("redact() left control whitespace in %q", redacted)
	}
}
