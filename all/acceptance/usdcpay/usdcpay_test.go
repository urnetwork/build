package usdcpay

import (
	"context"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// newTestPayer builds a payer from a throwaway keypair. It never talks to the
// network: every test here stops before a broadcast, which is the point -- a
// unit test must not be able to spend.
func newTestPayer(t *testing.T, maxUsd float64) (*Payer, solana.PrivateKey) {
	t.Helper()
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	payer, err := NewPayer(
		"https://example.invalid",
		key.String(),
		key.PublicKey().String(),
		maxUsd,
	)
	if err != nil {
		t.Fatalf("new payer: %v", err)
	}
	return payer, key
}

func TestUsdToAtomicRoundsRatherThanTruncates(t *testing.T) {
	// The float nearest 39.99 is slightly below it, so truncation yields
	// 39989999 -- a cent short of the quote, which the server reads as an
	// underpayment and refuses to grant. Rounding is what makes the transfer
	// match the price.
	for _, testCase := range []struct {
		usd  float64
		want uint64
	}{
		{0.01, 10_000},
		{0.10, 100_000},
		{1, 1_000_000},
		{3, 3_000_000},
		{5, 5_000_000},
		{20, 20_000_000},
		{39.99, 39_990_000},
		{40, 40_000_000},
		{9.99, 9_990_000},
	} {
		if got := usdToAtomic(testCase.usd); got != testCase.want {
			t.Errorf("usdToAtomic(%v) = %d, want %d", testCase.usd, got, testCase.want)
		}
	}
}

func TestAtomicToUsdRoundTrips(t *testing.T) {
	for _, usd := range []float64{0.01, 1, 3, 5, 20, 39.99, 40} {
		if got := atomicToUsd(usdToAtomic(usd)); got != usd {
			t.Errorf("round trip of %v produced %v", usd, got)
		}
	}
}

func TestNewPayerRefusesAKeyThatIsNotTheConfiguredAddress(t *testing.T) {
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	other, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	_, err = NewPayer("https://example.invalid", key.String(), other.PublicKey().String(), 25)
	if err == nil {
		t.Fatal("expected a mismatched key and address to be refused")
	}
	// The failure must not leak either key into a log.
	if strings.Contains(err.Error(), key.String()) {
		t.Error("error text contains the private key")
	}
}

func TestNewPayerRequiresItsInputs(t *testing.T) {
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	address := key.PublicKey().String()

	for name, build := range map[string]func() (*Payer, error){
		"no rpc url": func() (*Payer, error) {
			return NewPayer("", key.String(), address, 25)
		},
		"no private key": func() (*Payer, error) {
			return NewPayer("https://example.invalid", "", address, 25)
		},
		"no address": func() (*Payer, error) {
			return NewPayer("https://example.invalid", key.String(), "", 25)
		},
		"zero ceiling": func() (*Payer, error) {
			return NewPayer("https://example.invalid", key.String(), address, 0)
		},
		"negative ceiling": func() (*Payer, error) {
			return NewPayer("https://example.invalid", key.String(), address, -1)
		},
		"unparseable private key": func() (*Payer, error) {
			return NewPayer("https://example.invalid", "not-base58-!!", address, 25)
		},
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSendRefusesAboveTheCeiling(t *testing.T) {
	payer, _ := newTestPayer(t, 25)
	reference, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate reference: %v", err)
	}
	_, err = payer.Send(context.Background(), Payment{
		Recipient: "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
		AmountUsd: 25.01,
		Reference: reference.PublicKey().String(),
	})
	if err == nil {
		t.Fatal("expected a payment above the ceiling to be refused")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("error should name the ceiling, got %q", err)
	}
}

func TestSendRefusesANonPositiveAmount(t *testing.T) {
	payer, _ := newTestPayer(t, 25)
	reference, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate reference: %v", err)
	}
	for _, amount := range []float64{0, -1} {
		_, err := payer.Send(context.Background(), Payment{
			Recipient: "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
			AmountUsd: amount,
			Reference: reference.PublicKey().String(),
		})
		if err == nil {
			t.Errorf("expected an amount of %v to be refused", amount)
		}
	}
}

func TestSendRefusesAHexUuidReference(t *testing.T) {
	// This is the shape that once made every web payment unmatchable: Solana
	// Pay requires a base58 32-byte pubkey, and a hex uuid is neither. The
	// refusal must happen before anything is signed.
	payer, _ := newTestPayer(t, 25)
	_, err := payer.Send(context.Background(), Payment{
		Recipient: "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
		AmountUsd: 5,
		Reference: "0f9a6d2c8b7e4f1aa3c5d7e9f1b3c5d7",
	})
	if err == nil {
		t.Fatal("expected a hex uuid reference to be refused")
	}
	if !strings.Contains(err.Error(), "base58") {
		t.Errorf("error should explain the reference format, got %q", err)
	}
}

func TestSendRefusesAnUnparseableRecipient(t *testing.T) {
	payer, _ := newTestPayer(t, 25)
	reference, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate reference: %v", err)
	}
	_, err = payer.Send(context.Background(), Payment{
		Recipient: "not a solana address",
		AmountUsd: 5,
		Reference: reference.PublicKey().String(),
	})
	if err == nil {
		t.Fatal("expected an unparseable recipient to be refused")
	}
}

func TestTransferInstructionCarriesTheReferenceAsAnAccountKey(t *testing.T) {
	// The server matches a transfer to its intent by the transaction's
	// account keys or its memo. If the reference stops being attached as a
	// key, Solana Pay wallets still work but the match narrows to the memo
	// alone -- so this pins the account-key half.
	payer, key := newTestPayer(t, 25)
	reference, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate reference: %v", err)
	}
	source, err := payer.TokenAccount()
	if err != nil {
		t.Fatalf("payer token account: %v", err)
	}
	recipient := solana.MustPublicKeyFromBase58("4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM")
	destination, _, err := solana.FindAssociatedTokenAddress(recipient, payer.mint)
	if err != nil {
		t.Fatalf("recipient token account: %v", err)
	}

	instruction, err := payer.transferInstruction(source, destination, 5, reference.PublicKey())
	if err != nil {
		t.Fatalf("build transfer: %v", err)
	}

	var found *solana.AccountMeta
	for _, account := range instruction.Accounts() {
		if account.PublicKey.Equals(reference.PublicKey()) {
			found = account
		}
	}
	if found == nil {
		t.Fatal("reference is not among the instruction accounts")
	}
	if found.IsSigner {
		t.Error("the reference must not be a signer")
	}
	if found.IsWritable {
		t.Error("the reference must not be writable")
	}
	if !instruction.ProgramID().Equals(solana.TokenProgramID) {
		t.Errorf("transfer should target the token program, got %s", instruction.ProgramID())
	}
	// The owner signing the transfer is the payer, not anyone else.
	var sawOwner bool
	for _, account := range instruction.Accounts() {
		if account.PublicKey.Equals(key.PublicKey()) && account.IsSigner {
			sawOwner = true
		}
	}
	if !sawOwner {
		t.Error("the payer must sign the transfer")
	}
}

func TestParseUintStopsAtNonDigits(t *testing.T) {
	for input, want := range map[string]uint64{
		"":         0,
		"0":        0,
		"1000000":  1_000_000,
		"12a34":    12,
		"abc":      0,
		"39990000": 39_990_000,
	} {
		if got := parseUint(input); got != want {
			t.Errorf("parseUint(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestUsdcMintIsTheMainnetMint(t *testing.T) {
	// Pinned deliberately: this constant has to stay identical to the one the
	// server matches on and the one the apps build payment urls with. A
	// divergence here sends real money to an asset the server ignores.
	const serverMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	if UsdcMintMainnet != serverMint {
		t.Fatalf("usdc mint drifted: %s != %s", UsdcMintMainnet, serverMint)
	}
	if _, err := solana.PublicKeyFromBase58(UsdcMintMainnet); err != nil {
		t.Fatalf("usdc mint is not a valid address: %v", err)
	}
	if UsdcDecimals != 6 {
		t.Fatalf("usdc decimals drifted: %d", UsdcDecimals)
	}
}

func TestIsAccountNotFoundRecognizesBothShapes(t *testing.T) {
	if !isAccountNotFound(errShim("could not find account")) {
		t.Error("provider phrasing should read as not found")
	}
	if isAccountNotFound(errShim("rate limit exceeded")) {
		t.Error("an unrelated failure must not read as not found")
	}
	if isAccountNotFound(nil) {
		t.Error("nil is not a not-found error")
	}
}

type errShim string

func (e errShim) Error() string { return string(e) }
