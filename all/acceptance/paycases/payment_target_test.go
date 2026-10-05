// Tests of where the payment cases pay: the recipient a quote names, only when
// that is the official merchant in USDC, and for x402 only the pinned
// settlement address. The API is an httptest server and the wallet a recorder,
// so nothing here signs or sends a transaction. Addresses are fixture keys
// (sha256 of a fixed phrase as a public key), not wallets; the official
// merchant is compared by its sha256 so its address stays out of test data.
package paycases

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/build/all/acceptance/usdcpay"
)

const (
	// the official merchant's address by its sha256 (hex), as the server and
	// ur.io tests pin it
	officialMerchantSha256 = "b6fed7b0a3462afeda2f9703ecc17076b664bb8b1129bb0b62c70304bd50ab2c"

	// what the fake API quotes
	testSubscriptionAmountUsd = 40.004317
	testDataPackAmountUsd     = 20.000123
	testX402AmountAtomic      = "5000000"
)

// A fixture key, not a wallet: sha256 of the phrase as a public key.
func testSolanaKey(phrase string) string {
	sum := sha256.Sum256([]byte(phrase))
	return solana.PublicKeyFromBytes(sum[:]).String()
}

var (
	// the merchant and x402 settlement address the test runners pin
	testMerchant  = testSolanaKey("urnetwork acceptance test merchant")
	testX402PayTo = testSolanaKey("urnetwork acceptance test x402 settlement")
	// where a misconfigured server might say to pay instead
	testOtherRecipient = testSolanaKey("urnetwork acceptance test other recipient")
	testOtherMint      = testSolanaKey("urnetwork acceptance test other mint")
)

// The acceptance wallet, faked: it records what it is asked to pay or sign and
// moves nothing.
type testUsdcPayer struct {
	sentPayments     []usdcpay.Payment
	signedRecipients []string
}

// A fixture key stands in for the wallet.
func (self *testUsdcPayer) Address() string {
	return testSolanaKey("urnetwork acceptance test payer")
}

// Enough of both to pay anything a case quotes.
func (self *testUsdcPayer) Balances(ctx context.Context) (float64, float64, error) {
	return 1000, 1, nil
}

// Records the payment and reports it finalized.
func (self *testUsdcPayer) Send(ctx context.Context, payment usdcpay.Payment) (*usdcpay.Result, error) {
	self.sentPayments = append(self.sentPayments, payment)
	return &usdcpay.Result{
		Signature: "test-payment-signature",
		AmountUsd: payment.AmountUsd,
		Reference: payment.Reference,
		Recipient: payment.Recipient,
	}, nil
}

// Records the recipient and returns a transfer that is not a transaction.
func (self *testUsdcPayer) SignTransfer(
	ctx context.Context, recipient string, amountAtomic uint64,
) (*usdcpay.SignedTransfer, error) {
	self.signedRecipients = append(self.signedRecipients, recipient)
	return &usdcpay.SignedTransfer{
		Signature: "test-transfer-signature",
		Base64:    "AQID",
		AmountUsd: float64(amountAtomic) / 1e6,
	}, nil
}

// The API a payment case talks to, faked. Every quote says to pay the
// recipient, mint and x402 payTo given here, and leaves out an empty one as a
// server that predates the field does; whatever is paid is granted at once.
type testPayApi struct {
	recipient    string
	splTokenMint string
	x402PayTo    string
}

// Serves the API until the test ends.
func (self *testPayApi) serve(t *testing.T) *httptest.Server {
	writeJson := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	whereToPay := func(quote map[string]any) map[string]any {
		if self.recipient != "" {
			quote["recipient"] = self.recipient
		}
		if self.splTokenMint != "" {
			quote["spl_token_mint"] = self.splTokenMint
		}
		return quote
	}
	// only the network_name claim of a network jwt is read
	networkJwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"network_name":"acceptance-test"}`)) + ".dGVzdA"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /auth/network-create":
			writeJson(w, http.StatusOK, map[string]any{"network": map[string]any{"by_jwt": networkJwt}})
		case "POST /auth/network-delete":
			writeJson(w, http.StatusOK, map[string]any{})
		case "POST /solana/payment-intent":
			writeJson(w, http.StatusOK, whereToPay(map[string]any{
				"amount_usd": testSubscriptionAmountUsd,
				"plan":       "yearly",
				"currency":   "USD",
			}))
		case "POST /pay/data/solana-intent":
			var intentArgs struct {
				Reference string `json:"reference"`
			}
			if err := json.NewDecoder(r.Body).Decode(&intentArgs); err != nil {
				t.Errorf("data intent body: %v", err)
			}
			writeJson(w, http.StatusOK, whereToPay(map[string]any{
				"amount_usd": testDataPackAmountUsd,
				"reference":  intentArgs.Reference,
				"memo":       intentArgs.Reference,
			}))
		case "POST /pay/data/solana-status":
			writeJson(w, http.StatusOK, map[string]any{"status": "paid"})
		case "GET /subscription/balance":
			writeJson(w, http.StatusOK, map[string]any{
				"current_subscription": map[string]any{"store": "solana", "plan": "yearly"},
			})
		case "GET /x402/skus":
			writeJson(w, http.StatusOK, map[string]any{"networks": []string{"solana"}, "asset": "usdc"})
		case "POST /x402/purchase":
			if r.Header.Get("X-PAYMENT") == "" {
				writeJson(w, http.StatusPaymentRequired, map[string]any{
					"x402Version": 1,
					"accepts": []map[string]any{{
						"scheme":            "exact",
						"network":           "solana",
						"maxAmountRequired": testX402AmountAtomic,
						"payTo":             self.x402PayTo,
						"asset":             "usdc",
						"maxTimeoutSeconds": 300,
					}},
				})
				return
			}
			writeJson(w, http.StatusOK, map[string]any{"complete": true, "sku_id": "pro_1month", "pro": true})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// An armed runner against the api url whose wallet is a recorder and whose
// merchant and x402 settlement address are the fixture keys.
func newTestRunner(t *testing.T, apiUrl string, client *http.Client) (*Runner, *testUsdcPayer) {
	t.Helper()
	runner, err := NewRunner(apiUrl, armedConfig(t), client)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	payer := &testUsdcPayer{}
	runner.payer = payer
	runner.merchantAddress = testMerchant
	runner.x402SolanaPayTo = testX402PayTo
	return runner, payer
}

// A quote naming the official merchant in USDC is paid: exactly the amount and
// reference it quoted, to the recipient it named.
func TestPaymentCasesPayTheQuotedRecipient(t *testing.T) {
	cases := []struct {
		name      string
		amountUsd float64
	}{
		{name: CaseSubscription, amountUsd: testSubscriptionAmountUsd},
		{name: CaseDataPack, amountUsd: testDataPackAmountUsd},
	}
	for _, c := range cases {
		api := &testPayApi{recipient: testMerchant, splTokenMint: usdcpay.UsdcMintMainnet}
		server := api.serve(t)
		runner, payer := newTestRunner(t, server.URL, server.Client())

		result := runner.runOne(context.Background(), c.name)
		if result.Status != "PASS" {
			t.Fatalf("%s: status %s (%s), want PASS", c.name, result.Status, result.Detail)
		}
		if len(payer.sentPayments) != 1 {
			t.Fatalf("%s: %d payments sent, want 1", c.name, len(payer.sentPayments))
		}
		payment := payer.sentPayments[0]
		if payment.Recipient != testMerchant {
			t.Errorf("%s: paid %s, want the quoted recipient %s", c.name, payment.Recipient, testMerchant)
		}
		if payment.AmountUsd != c.amountUsd {
			t.Errorf("%s: paid %v USDC, want the quoted %v", c.name, payment.AmountUsd, c.amountUsd)
		}
		if _, err := solana.PublicKeyFromBase58(payment.Reference); err != nil {
			t.Errorf("%s: reference %q is not a base58 public key: %v", c.name, payment.Reference, err)
		}
		if runner.spent != c.amountUsd {
			t.Errorf("%s: the campaign counted %v spent, want %v", c.name, runner.spent, c.amountUsd)
		}
	}
}

// A quote that says to pay anyone but the official merchant in USDC: another
// address, another mint, or nowhere (a server that predates quoting where to
// pay) fails its case with nothing paid.
func TestPaymentCasesRefuseAnotherPaymentTarget(t *testing.T) {
	quotes := []struct {
		name         string
		recipient    string
		splTokenMint string
		detail       string
	}{
		{name: "another recipient", recipient: testOtherRecipient, splTokenMint: usdcpay.UsdcMintMainnet, detail: testOtherRecipient},
		{name: "another mint", recipient: testMerchant, splTokenMint: testOtherMint, detail: testOtherMint},
		{name: "no recipient", recipient: "", splTokenMint: usdcpay.UsdcMintMainnet, detail: "where to pay"},
		{name: "no mint", recipient: testMerchant, splTokenMint: "", detail: "where to pay"},
		{name: "neither", recipient: "", splTokenMint: "", detail: "where to pay"},
	}
	for _, quote := range quotes {
		for _, caseName := range []string{CaseSubscription, CaseDataPack} {
			api := &testPayApi{recipient: quote.recipient, splTokenMint: quote.splTokenMint}
			server := api.serve(t)
			runner, payer := newTestRunner(t, server.URL, server.Client())

			result := runner.runOne(context.Background(), caseName)
			if result.Status != "FAIL" {
				t.Errorf("%s, %s: status %s (%s), want FAIL", quote.name, caseName, result.Status, result.Detail)
			}
			if len(payer.sentPayments) != 0 {
				t.Errorf("%s, %s: paid %+v, want nothing paid", quote.name, caseName, payer.sentPayments)
			}
			if runner.spent != 0 {
				t.Errorf("%s, %s: the campaign counted %v spent, want 0", quote.name, caseName, runner.spent)
			}
			for _, want := range []string{quote.detail, "nothing was paid"} {
				if !strings.Contains(result.Detail, want) {
					t.Errorf("%s, %s: detail %q should mention %q", quote.name, caseName, result.Detail, want)
				}
			}
		}
	}
}

// The browser cases' single payment goes to the recipient the page's payment
// names when that is the official merchant in USDC, and nowhere otherwise.
func TestPayReferencePaysOnlyTheOfficialMerchant(t *testing.T) {
	reference := testSolanaKey("urnetwork acceptance test reference")
	payments := []struct {
		name         string
		recipient    string
		splTokenMint string
		paid         bool
	}{
		{name: "the official merchant in USDC", recipient: testMerchant, splTokenMint: usdcpay.UsdcMintMainnet, paid: true},
		{name: "another recipient", recipient: testOtherRecipient, splTokenMint: usdcpay.UsdcMintMainnet},
		{name: "another mint", recipient: testMerchant, splTokenMint: testOtherMint},
		{name: "no recipient", recipient: "", splTokenMint: usdcpay.UsdcMintMainnet},
		{name: "no mint", recipient: testMerchant, splTokenMint: ""},
	}
	for _, payment := range payments {
		// no request reaches the api: the page registered the intent
		runner, payer := newTestRunner(t, "https://api.invalid", nil)

		result, err := runner.PayReference(context.Background(), reference, 20, payment.recipient, payment.splTokenMint)
		if !payment.paid {
			if err == nil || !strings.Contains(err.Error(), "nothing was paid") {
				t.Errorf("%s: error %v, want a refusal that says nothing was paid", payment.name, err)
			}
			if len(payer.sentPayments) != 0 {
				t.Errorf("%s: paid %+v, want nothing paid", payment.name, payer.sentPayments)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", payment.name, err)
		}
		want := []usdcpay.Payment{{Recipient: testMerchant, AmountUsd: 20, Reference: reference}}
		if !slices.Equal(payer.sentPayments, want) {
			t.Errorf("%s: paid %+v, want %+v", payment.name, payer.sentPayments, want)
		}
		if result.Recipient != testMerchant {
			t.Errorf("%s: result names recipient %s, want %s", payment.name, result.Recipient, testMerchant)
		}
	}
}

// x402 signs a transfer only to the settlement address pinned for Solana:
// terms naming another payTo, or any payTo while none is pinned, fail the case
// with nothing signed.
func TestX402SignsOnlyForThePinnedSettlementAddress(t *testing.T) {
	cases := []struct {
		name   string
		payTo  string
		pinned string
		signed bool
	}{
		{name: "the pinned address", payTo: testX402PayTo, pinned: testX402PayTo, signed: true},
		{name: "another address", payTo: testOtherRecipient, pinned: testX402PayTo},
		{name: "nothing pinned", payTo: testX402PayTo, pinned: ""},
	}
	for _, c := range cases {
		api := &testPayApi{x402PayTo: c.payTo}
		server := api.serve(t)
		runner, payer := newTestRunner(t, server.URL, server.Client())
		runner.x402SolanaPayTo = c.pinned

		result := runner.runOne(context.Background(), CaseX402)
		if !c.signed {
			if result.Status != "FAIL" || !strings.Contains(result.Detail, "nothing was signed") {
				t.Errorf("%s: status %s (%s), want FAIL with nothing signed", c.name, result.Status, result.Detail)
			}
			if len(payer.signedRecipients) != 0 || runner.spent != 0 {
				t.Errorf("%s: signed for %v and counted %v spent, want nothing", c.name, payer.signedRecipients, runner.spent)
			}
			continue
		}
		if result.Status != "PASS" {
			t.Fatalf("%s: status %s (%s), want PASS", c.name, result.Status, result.Detail)
		}
		if !slices.Equal(payer.signedRecipients, []string{c.payTo}) {
			t.Errorf("%s: signed for %v, want only %s", c.name, payer.signedRecipients, c.payTo)
		}
	}
}

// The runner pins the official merchant, compared by its sha256 so the
// address stays out of test data.
func TestRunnerPinsTheOfficialMerchant(t *testing.T) {
	runner, err := NewRunner("https://api.invalid", offConfig(), nil)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	sum := sha256.Sum256([]byte(runner.merchantAddress))
	if hex.EncodeToString(sum[:]) != officialMerchantSha256 {
		t.Fatal("the runner's merchant is not the official one (server solanaReceiverAddresses[0])")
	}
	if _, err := solana.PublicKeyFromBase58(runner.merchantAddress); err != nil {
		t.Fatalf("the merchant is not a valid solana address: %v", err)
	}
}

// The official merchant appears once in the acceptance sources: as the pin in
// paycases.go. Test data uses fixture keys, and nothing else carries a payment
// target of its own.
func TestOfficialMerchantIsPinnedOnlyInPaycases(t *testing.T) {
	base58Token := regexp.MustCompile(`[1-9A-HJ-NP-Za-km-z]{32,44}`)
	foundLines := []string{}
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for lineIndex, line := range strings.Split(string(data), "\n") {
			for _, token := range base58Token.FindAllString(line, -1) {
				sum := sha256.Sum256([]byte(token))
				if hex.EncodeToString(sum[:]) == officialMerchantSha256 {
					foundLines = append(foundLines, fmt.Sprintf("%s:%d", filepath.ToSlash(path), lineIndex+1))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the acceptance sources: %v", err)
	}
	if len(foundLines) != 1 || !strings.HasPrefix(foundLines[0], "../paycases/paycases.go:") {
		t.Fatalf("the official merchant should appear only as the pin in paycases.go, found at %v", foundLines)
	}
}
