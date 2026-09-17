package paycases

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/build/all/acceptance/testconfig"
)

// These tests never spend. Either payments are off, or the case stops at a
// stage before a transfer is built -- a unit test that could move money would
// be a liability, not a test.

func offConfig() *testconfig.Config {
	return &testconfig.Config{
		Version:   testconfig.Version,
		Lifecycle: testconfig.Lifecycle{AllowAccountCreateDelete: true},
	}
}

func armedConfig(t *testing.T) *testconfig.Config {
	t.Helper()
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	config := offConfig()
	config.Payments = testconfig.Payments{
		AllowRealUsdcSpend:  true,
		MaxSpendUsd:         25,
		MaxCampaignSpendUsd: 50,
		SolanaRpcUrl:        "https://rpc.invalid",
		Payer: testconfig.PayerWallet{
			Address:          key.PublicKey().String(),
			PrivateKeyBase58: key.String(),
		},
	}
	return config
}

// Payments being off is a deliberate local choice about spending money, so it
// stays a SKIP -- including for x402, which cannot be paid either way.
func TestEveryCaseSkipsWhenPaymentsAreOff(t *testing.T) {
	runner, err := NewRunner("https://api.invalid", offConfig(), nil)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	results := runner.Run(context.Background(), AllCases)
	if len(results) != len(AllCases) {
		t.Fatalf("got %d results, want %d", len(results), len(AllCases))
	}
	for _, result := range results {
		if result.Status != "SKIP" {
			t.Errorf("%s: status %s, want SKIP", result.Case, result.Status)
		}
		if !strings.Contains(result.Detail, "allow_real_usdc_spend") {
			t.Errorf("%s: detail should name the guard, got %q", result.Case, result.Detail)
		}
	}
	if runner.PayerAddress() != "" {
		t.Error("no payer should exist while payments are off")
	}
}

// An enabled section with a key that does not match its address must not
// degrade to a skip: somebody asked for these cases to run.
func TestAnEnabledButBrokenPayerIsAnError(t *testing.T) {
	config := armedConfig(t)
	other, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	config.Payments.Payer.PrivateKeyBase58 = other.String()
	if _, err := NewRunner("https://api.invalid", config, nil); err == nil {
		t.Fatal("expected a mismatched payer to be an error, not a skip")
	}
}

// x402 is a shipped, documented surface. A deployment that answers 404 is
// broken, not exempt, so the case fails and keeps failing until somebody
// configures the facilitator.
func TestX402FailsWhenTheDeploymentHasItOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x402/skus" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("x402 is not enabled."))
			return
		}
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer server.Close()

	runner, err := NewRunner(server.URL, armedConfig(t), server.Client())
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	result := runner.runOne(context.Background(), CaseX402)
	if result.Status != "FAIL" {
		t.Fatalf("status %s (%s), want FAIL", result.Status, result.Detail)
	}
	// The detail is what an operator reads out of the matrix, so it has to say
	// which file to fix.
	for _, want := range []string{"not configured", "x402.yml", "facilitator"} {
		if !strings.Contains(result.Detail, want) {
			t.Errorf("detail should mention %q, got %q", want, result.Detail)
		}
	}
}

// x402 is chain-neutral by design, so dropping Solana is a legitimate product
// decision -- but it leaves the acceptance payer unable to pay, and that needs
// somebody's attention rather than a silent skip.
func TestX402FailsWhenSolanaIsNotQuoted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x402/skus" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"networks": []string{"base", "tempo"},
				"asset":    "usdc",
			})
			return
		}
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer server.Close()

	runner, err := NewRunner(server.URL, armedConfig(t), server.Client())
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	result := runner.runOne(context.Background(), CaseX402)
	if result.Status != "FAIL" {
		t.Fatalf("status %s (%s), want FAIL", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "Solana") {
		t.Errorf("detail should say what the payer holds, got %q", result.Detail)
	}
}

func TestBudgetRefusesPastTheCampaignCeiling(t *testing.T) {
	runner, err := NewRunner("https://api.invalid", armedConfig(t), nil)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	// The ceiling is 50.
	if err := runner.budget(40); err != nil {
		t.Fatalf("40 should be within the ceiling: %v", err)
	}
	runner.spent = 40
	if err := runner.budget(5); err != nil {
		t.Fatalf("45 total should be within the ceiling: %v", err)
	}
	if err := runner.budget(11); err == nil {
		t.Fatal("51 total should be refused")
	}
}

func TestBudgetRefusesWithNoCeilingConfigured(t *testing.T) {
	config := armedConfig(t)
	config.Payments.MaxCampaignSpendUsd = 0
	runner, err := NewRunner("https://api.invalid", config, nil)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := runner.budget(1); err == nil {
		t.Fatal("an unset campaign ceiling must refuse every payment")
	}
}

func TestValidateAcceptRejectsUnpayableTerms(t *testing.T) {
	good := x402Accept{
		Scheme:            "exact",
		Network:           "solana",
		MaxAmountRequired: "5000000",
		PayTo:             MerchantAddress,
		Asset:             "usdc",
	}
	if err := validateAccept(&good); err != nil {
		t.Fatalf("well-formed terms should validate: %v", err)
	}

	for name, mutate := range map[string]func(*x402Accept){
		"wrong scheme":  func(a *x402Accept) { a.Scheme = "upto" },
		"wrong asset":   func(a *x402Accept) { a.Asset = "usdt" },
		"no payTo":      func(a *x402Accept) { a.PayTo = "" },
		"non-integer":   func(a *x402Accept) { a.MaxAmountRequired = "5.00" },
		"zero amount":   func(a *x402Accept) { a.MaxAmountRequired = "0" },
		"empty amount":  func(a *x402Accept) { a.MaxAmountRequired = "" },
		"negative-ish":  func(a *x402Accept) { a.MaxAmountRequired = "-1" },
		"dollar amount": func(a *x402Accept) { a.MaxAmountRequired = "$5" },
		"uppercase asset": func(a *x402Accept) {
			// Case should NOT be a failure -- this one must still pass.
			a.Asset = "USDC"
		},
	} {
		candidate := good
		mutate(&candidate)
		err := validateAccept(&candidate)
		if name == "uppercase asset" {
			if err != nil {
				t.Errorf("%s: asset case should not matter, got %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected a problem", name)
		}
	}
}

func TestAtomicToUsdReadsQuotedTerms(t *testing.T) {
	for input, want := range map[string]float64{
		"5000000":  5,
		"3000000":  3,
		"20000000": 20,
		"1":        0.000001,
		"":         0,
		"5.00":     0,
		"abc":      0,
	} {
		if got := atomicToUsd(input); got != want {
			t.Errorf("atomicToUsd(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestMerchantAddressIsTheServerReceiver(t *testing.T) {
	// Pinned against server/controller/subscription_controller.go
	// solanaReceiverAddresses[0]. A transfer to any other address is not a
	// payment, so a drift here must fail rather than quietly misdirect money.
	const serverReceiver = "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM"
	if MerchantAddress != serverReceiver {
		t.Fatalf("merchant address drifted: %s != %s", MerchantAddress, serverReceiver)
	}
	if _, err := solana.PublicKeyFromBase58(MerchantAddress); err != nil {
		t.Fatalf("merchant address is not a valid solana address: %v", err)
	}
}

func TestNewReferenceIsABase58Pubkey(t *testing.T) {
	// Solana Pay requires a base58 32-byte pubkey. A hex uuid here is the bug
	// that once made every web payment unmatchable, so this is pinned.
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		reference, err := newReference()
		if err != nil {
			t.Fatalf("mint reference: %v", err)
		}
		key, err := solana.PublicKeyFromBase58(reference)
		if err != nil {
			t.Fatalf("reference %q is not a base58 pubkey: %v", reference, err)
		}
		if len(key.Bytes()) != 32 {
			t.Fatalf("reference is %d bytes, want 32", len(key.Bytes()))
		}
		if seen[reference] {
			t.Fatal("references must not repeat")
		}
		seen[reference] = true
	}
}

func TestRedactKeepsCredentialsOutOfDetails(t *testing.T) {
	config := armedConfig(t)
	config.Signup.Password = "signup-secret-123"
	runner, err := NewRunner("https://api.invalid", config, nil)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	message := strings.Join([]string{
		config.Payments.Payer.PrivateKeyBase58,
		config.Payments.SolanaRpcUrl,
		config.Signup.Password,
	}, " and ")
	got := runner.redact(message)
	for _, secret := range []string{
		config.Payments.Payer.PrivateKeyBase58,
		config.Payments.SolanaRpcUrl,
		config.Signup.Password,
	} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted detail still contains a credential: %q", got)
		}
	}
}

func TestX402PaymentPayloadIsBase64Json(t *testing.T) {
	// The header shape is pinned even though the settle half is unproven: if
	// it changes, it should change deliberately.
	payload := x402PaymentPayload{
		X402Version: 1,
		Scheme:      "exact",
		Network:     "solana",
		Payload:     x402ExactPayload{Transaction: "AQID"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.StdEncoding.EncodeToString(encoded)
	decoded, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("header must be base64: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(decoded, &round); err != nil {
		t.Fatalf("header must decode to json: %v", err)
	}
	for _, key := range []string{"x402Version", "scheme", "network", "payload"} {
		if _, present := round[key]; !present {
			t.Errorf("payload is missing %q", key)
		}
	}
}

func TestAllCasesRunsCheapestFirst(t *testing.T) {
	// A campaign that exhausts its budget should have bought the cheap thing
	// first, so the expensive case is the one that goes unpaid.
	if AllCases[0] != CaseDataPack {
		t.Errorf("AllCases should start with the cheapest case, got %s", AllCases[0])
	}
	for _, name := range AllCases {
		switch name {
		case CaseDataPack, CaseSubscription, CaseX402:
		default:
			t.Errorf("unknown case %q in AllCases", name)
		}
	}
}

func TestUnknownCaseFails(t *testing.T) {
	runner, err := NewRunner("https://api.invalid", armedConfig(t), nil)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	result := runner.runOne(context.Background(), "not-a-case")
	if result.Status != "FAIL" {
		t.Errorf("an unknown case should fail, got %s", result.Status)
	}
}
