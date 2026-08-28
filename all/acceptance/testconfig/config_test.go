package testconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"
)

const validConfig = `version: 1
lifecycle: {allow_account_create_delete: true}
email_verification:
  bypass_domains: [acceptance.invalid]
  suppress_account_messages: true
data_plane_account: {email: data@example.com, password: secret}
signup:
  network_name_prefix: acceptance
  password: signup-secret-123
  email: {domain: acceptance.invalid, local_part_prefix: urnetwork}
  phone: {number: "+15555550123"}
providers:
  google: {email: google@example.com, password: secret, totp_secret: JBSWY3DPEHPK3PXP, recovery_email: recovery@example.com, browser_profile: google.json}
  apple: {email: apple@example.com, password: secret, browser_profile: apple.json}
wallets:
  solana: {address: SOL_ADDRESS, private_key_base58: SOL_PRIVATE_KEY}
  bittensor: {address: TAO_ADDRESS, mnemonic: "bottom drive obey lake curtain smoke basket hold race lonely fit walk", ss58_prefix: 42}
`

func configuredConfig(t *testing.T) string {
	t.Helper()
	privateKey, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPair, err := subkey.DeriveKeyPair(sr25519.Scheme{}, "bottom drive obey lake curtain smoke basket hold race lonely fit walk")
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Replace(validConfig, "SOL_ADDRESS", privateKey.PublicKey().String(), 1)
	value = strings.Replace(value, "SOL_PRIVATE_KEY", privateKey.String(), 1)
	return strings.Replace(value, "TAO_ADDRESS", keyPair.SS58Address(42), 1)
}

func writeConfig(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tests.yml")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadGetAndResolve(t *testing.T) {
	path := writeConfig(t, configuredConfig(t), 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(true); err != nil {
		t.Fatal(err)
	}
	if got, err := config.Get("data_plane_account.email"); err != nil || got != "data@example.com" {
		t.Fatalf("Get() = %q, %v", got, err)
	}
	wantProfile := filepath.Join(filepath.Dir(path), "google.json")
	if got := ResolveProfile(path, "google.json"); got != wantProfile {
		t.Fatalf("ResolveProfile() = %q, want %q", got, wantProfile)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	if _, err := Load(writeConfig(t, configuredConfig(t)+"unknown: true\n", 0o600)); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestValidateReadyReportsPlaceholders(t *testing.T) {
	config, err := Load(writeConfig(t, strings.Replace(configuredConfig(t), "password: secret", "password: REPLACE_ME", 1), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(true); err == nil || !strings.Contains(err.Error(), "data_plane_account.password") {
		t.Fatalf("Validate(true) = %v", err)
	}
}

func TestEmailDomainMustBeExactBypassDomain(t *testing.T) {
	config, err := Load(writeConfig(t, strings.Replace(configuredConfig(t), "domain: acceptance.invalid", "domain: sub.acceptance.invalid", 1), 0o600))
	if err == nil || config != nil {
		t.Fatalf("Load() = %#v, %v", config, err)
	}
}

func TestValidateReadyRejectsWalletAddressMismatch(t *testing.T) {
	// Use a direct decode/edit to avoid relying on private values in the error.
	config, err := Load(writeConfig(t, configuredConfig(t), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	config.Wallets.Solana.Address = solana.SystemProgramID.String()
	if err := config.Validate(true); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Validate(true) = %v", err)
	}
}

func TestValidateReadyRejectsMalformedIdentityFixtures(t *testing.T) {
	config, err := Load(writeConfig(t, configuredConfig(t), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{name: "short signup password", edit: func(config *Config) { config.Signup.Password = "too-short" }, want: "at least 12"},
		{name: "phone", edit: func(config *Config) { config.Signup.Phone.Number = "555-0100" }, want: "E.164"},
		{name: "google totp", edit: func(config *Config) { config.Providers.Google.TOTPSecret = "not-a-secret" }, want: "base32"},
		{name: "provider email", edit: func(config *Config) { config.Providers.Apple.Email = "not-an-email" }, want: "exact DNS domain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := *config
			test.edit(&copy)
			if err := copy.Validate(true); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate(true) = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveProfilesCopiesConfig(t *testing.T) {
	path := writeConfig(t, configuredConfig(t), 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved := config.ResolveProfiles(path)
	if got := resolved.Providers.Google.BrowserProfile; got != filepath.Join(filepath.Dir(path), "google.json") {
		t.Fatalf("resolved profile = %q", got)
	}
	if config.Providers.Google.BrowserProfile != "google.json" {
		t.Fatal("ResolveProfiles mutated source config")
	}
}

func TestLoadJSONRequiresReadyStrictFixture(t *testing.T) {
	yamlPath := writeConfig(t, configuredConfig(t), 0o600)
	config, err := Load(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(t.TempDir(), "tests.json")
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSON(jsonPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, append(data[:len(data)-1], []byte(`,"unknown":true}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSON(jsonPath); err == nil {
		t.Fatal("unknown JSON field was accepted")
	}
}

func writeProviderProfile(t *testing.T, path, domain string, mode os.FileMode) {
	t.Helper()
	contents := fmt.Sprintf(`{"cookies":[{"name":"session","value":"opaque","domain":%q,"path":"/","expires":%d}],"origins":[]}`,
		domain, time.Now().Add(time.Hour).Unix())
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProvisionedAcceptsPrivateLiveProviderProfiles(t *testing.T) {
	path := writeConfig(t, configuredConfig(t), 0o600)
	writeProviderProfile(t, filepath.Join(filepath.Dir(path), "google.json"), ".accounts.google.com", 0o600)
	writeProviderProfile(t, filepath.Join(filepath.Dir(path), "apple.json"), "idmsa.apple.com", 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateProvisioned(path); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProvisionedRejectsMissingStaleAndExposedProfiles(t *testing.T) {
	path := writeConfig(t, configuredConfig(t), 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateProvisioned(path); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing profiles: %v", err)
	}

	writeProviderProfile(t, filepath.Join(filepath.Dir(path), "google.json"), ".google.com", 0o644)
	contents := fmt.Sprintf(`{"cookies":[{"name":"session","value":"opaque","domain":"apple.com","path":"/","expires":%d}],"origins":[]}`,
		time.Now().Add(-time.Hour).Unix())
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "apple.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	err = config.ValidateProvisioned(path)
	if err == nil || !strings.Contains(err.Error(), "permissions") || !strings.Contains(err.Error(), "no live") {
		t.Fatalf("unsafe profiles: %v", err)
	}
}
