package testconfig

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// fieldByYamlName finds a struct field by the name in its yaml tag.
func fieldByYamlName(value any, name string) (reflect.StructField, bool) {
	typ := reflect.TypeOf(value)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if strings.Split(field.Tag.Get("yaml"), ",")[0] == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// payerKey returns a throwaway keypair as (address, private key base58).
func payerKey(t *testing.T) (string, string) {
	t.Helper()
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key.PublicKey().String(), key.String()
}

// The default state of every machine that is not running the real-money cases.
// It must validate cleanly in both modes: an unconfigured payments section is
// not a provisioning failure, it is "this campaign does not spend".
func TestUnconfiguredPaymentsValidateInBothModes(t *testing.T) {
	path := writeConfig(t, configuredConfig(t), 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.Payments.Enabled() {
		t.Fatal("payments must be off unless the vault turns them on")
	}
	if err := config.Validate(false); err != nil {
		t.Fatalf("validate(false): %v", err)
	}
	if err := config.Validate(true); err != nil {
		t.Fatalf("validate(true): %v", err)
	}
}

// The blanket "every string must be set" rule in ready mode must not reach the
// payments fields, or every existing machine fails the moment the section
// exists.
func TestPaymentsFieldsAreExemptFromTheConfiguredStringSweep(t *testing.T) {
	path := writeConfig(t, configuredConfig(t), 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, exempt := range []string{
		"payments.solana_rpc_url",
		"payments.payer.address",
		"payments.payer.private_key_base58",
	} {
		if _, present := config.StringValues()[exempt]; present {
			t.Errorf("%s must not be swept as a required string", exempt)
		}
	}
}

func TestEnabledPaymentsRequireEveryField(t *testing.T) {
	address, privateKey := payerKey(t)
	base := Payments{
		AllowRealUsdcSpend:  true,
		MaxSpendUsd:         25,
		MaxCampaignSpendUsd: 100,
		SolanaRpcUrl:        "https://mainnet.helius-rpc.com/?api-key=x",
		Payer:               PayerWallet{Address: address, PrivateKeyBase58: privateKey},
	}
	if problems := base.problems(); len(problems) != 0 {
		t.Fatalf("a complete section should validate, got %v", problems)
	}

	for name, mutate := range map[string]func(*Payments){
		"no rpc url":          func(p *Payments) { p.SolanaRpcUrl = "" },
		"plaintext rpc url":   func(p *Payments) { p.SolanaRpcUrl = "http://mainnet.example" },
		"no payer address":    func(p *Payments) { p.Payer.Address = "" },
		"no payer key":        func(p *Payments) { p.Payer.PrivateKeyBase58 = "" },
		"zero single ceiling": func(p *Payments) { p.MaxSpendUsd = 0 },
		"zero campaign cap":   func(p *Payments) { p.MaxCampaignSpendUsd = 0 },
		"campaign below single": func(p *Payments) {
			p.MaxSpendUsd = 50
			p.MaxCampaignSpendUsd = 10
		},
	} {
		candidate := base
		mutate(&candidate)
		if problems := candidate.problems(); len(problems) == 0 {
			t.Errorf("%s: expected a problem", name)
		}
	}
}

// The same guarantee walletfixture gives the signing wallet: prove the key
// belongs to the address before the suite may spend from it.
func TestEnabledPaymentsRejectAKeyThatIsNotTheAddress(t *testing.T) {
	address, _ := payerKey(t)
	_, otherKey := payerKey(t)
	payments := Payments{
		AllowRealUsdcSpend:  true,
		MaxSpendUsd:         25,
		MaxCampaignSpendUsd: 100,
		SolanaRpcUrl:        "https://mainnet.helius-rpc.com/?api-key=x",
		Payer:               PayerWallet{Address: address, PrivateKeyBase58: otherKey},
	}
	problems := payments.problems()
	if len(problems) == 0 {
		t.Fatal("expected a mismatched payer key to be refused")
	}
	if !strings.Contains(strings.Join(problems, "; "), "payments.payer") {
		t.Errorf("problem should name payments.payer, got %v", problems)
	}
}

// Ceilings are checked even while the guard is off, because they are what
// bounds the damage the moment someone turns it on.
func TestCeilingsAreValidatedWhilePaymentsAreOff(t *testing.T) {
	for name, payments := range map[string]Payments{
		"negative single":       {MaxSpendUsd: -1},
		"negative campaign":     {MaxCampaignSpendUsd: -1},
		"campaign below single": {MaxSpendUsd: 50, MaxCampaignSpendUsd: 10},
	} {
		if problems := payments.problems(); len(problems) == 0 {
			t.Errorf("%s: expected a problem while payments are off", name)
		}
	}
}

// A funded key sitting in the vault with no address next to it is somebody
// half-arming the suite; say so rather than silently not spending.
func TestAHalfConfiguredPayerIsReported(t *testing.T) {
	_, privateKey := payerKey(t)
	payments := Payments{Payer: PayerWallet{PrivateKeyBase58: privateKey}}
	if problems := payments.problems(); len(problems) == 0 {
		t.Fatal("expected a half-configured payer to be reported")
	}
}

func TestPaymentsParseFromTheVaultFile(t *testing.T) {
	address, privateKey := payerKey(t)
	contents := configuredConfig(t) + `payments:
  allow_real_usdc_spend: true
  max_spend_usd: 12.5
  max_campaign_spend_usd: 50
  solana_rpc_url: https://mainnet.helius-rpc.com/?api-key=x
  payer:
    address: ` + address + `
    private_key_base58: ` + privateKey + `
`
	path := writeConfig(t, contents, 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !config.Payments.Enabled() {
		t.Fatal("payments should read as enabled")
	}
	if config.Payments.MaxSpendUsd != 12.5 {
		t.Errorf("max_spend_usd = %v", config.Payments.MaxSpendUsd)
	}
	// Shell callers read the ceilings through Get; a float must not come back
	// in scientific notation.
	got, err := config.Get("payments.max_spend_usd")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "12.5" {
		t.Errorf("payments.max_spend_usd = %q, want \"12.5\"", got)
	}
	if got, err := config.Get("payments.allow_real_usdc_spend"); err != nil || got != "true" {
		t.Errorf("payments.allow_real_usdc_spend = %q, %v", got, err)
	}
}

// The private key must carry the secret tag so it is redacted wherever the
// other credentials are.
func TestPayerPrivateKeyIsTaggedSecret(t *testing.T) {
	field, ok := fieldByYamlName(PayerWallet{}, "private_key_base58")
	if !ok {
		t.Fatal("private_key_base58 field not found")
	}
	if field.Tag.Get("secret") != "true" {
		t.Error("payments.payer.private_key_base58 must be tagged secret")
	}
	// The rpc url embeds an api key in the Helius form, so it is a secret too.
	rpc, ok := fieldByYamlName(Payments{}, "solana_rpc_url")
	if !ok {
		t.Fatal("solana_rpc_url field not found")
	}
	if rpc.Tag.Get("secret") != "true" {
		t.Error("payments.solana_rpc_url must be tagged secret")
	}
}
