package testconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/build/all/acceptance/walletfixture"
	"gopkg.in/yaml.v3"
)

const Version = 1

const maxBrowserProfileBytes = 16 * 1024 * 1024

var (
	domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	pathPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)*$`)
	prefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	phonePattern  = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	totpPattern   = regexp.MustCompile(`^[A-Za-z2-7 ]+$`)
	unlockPattern = regexp.MustCompile(`^[0-9]{4,16}$`)
)

type Config struct {
	Version           int               `yaml:"version" json:"version"`
	Android           Android           `yaml:"android" json:"android"`
	Lifecycle         Lifecycle         `yaml:"lifecycle" json:"lifecycle"`
	EmailVerification EmailVerification `yaml:"email_verification" json:"email_verification"`
	DataPlaneAccount  DataPlaneAccount  `yaml:"data_plane_account" json:"data_plane_account"`
	Signup            Signup            `yaml:"signup" json:"signup"`
	Providers         Providers         `yaml:"providers" json:"providers"`
	Wallets           Wallets           `yaml:"wallets" json:"wallets"`
	Payments          Payments          `yaml:"payments" json:"payments"`
}

type Android struct {
	// UnlockCode is host automation input. Runners must never print it or place
	// it in an adb command argument; Android accepts it over the remote shell's
	// standard input instead.
	UnlockCode string `yaml:"unlock_code" json:"unlock_code" secret:"true"`
}

type Lifecycle struct {
	AllowAccountCreateDelete bool `yaml:"allow_account_create_delete" json:"allow_account_create_delete"`
}

type EmailVerification struct {
	BypassDomains           []string `yaml:"bypass_domains" json:"bypass_domains"`
	SuppressAccountMessages bool     `yaml:"suppress_account_messages" json:"suppress_account_messages"`
}

type DataPlaneAccount struct {
	Email    string `yaml:"email" json:"email" secret:"true"`
	Password string `yaml:"password" json:"password" secret:"true"`
}

type Signup struct {
	NetworkNamePrefix            string      `yaml:"network_name_prefix" json:"network_name_prefix"`
	Password                     string      `yaml:"password" json:"password" secret:"true"`
	SeedphraseRateLimitBypassIPs []string    `yaml:"seedphrase_rate_limit_bypass_ips" json:"seedphrase_rate_limit_bypass_ips"`
	Email                        SignupEmail `yaml:"email" json:"email"`
	Phone                        SignupPhone `yaml:"phone" json:"phone"`
}

type SignupEmail struct {
	Domain          string `yaml:"domain" json:"domain"`
	LocalPartPrefix string `yaml:"local_part_prefix" json:"local_part_prefix"`
}

type SignupPhone struct {
	Number string `yaml:"number" json:"number" secret:"true"`
}

type Providers struct {
	Google ProviderGoogle `yaml:"google" json:"google"`
	Apple  ProviderApple  `yaml:"apple" json:"apple"`
}

type ProviderGoogle struct {
	Email          string `yaml:"email" json:"email" secret:"true"`
	Password       string `yaml:"password" json:"password" secret:"true"`
	TOTPSecret     string `yaml:"totp_secret" json:"totp_secret" secret:"true"`
	RecoveryEmail  string `yaml:"recovery_email" json:"recovery_email" secret:"true"`
	BrowserProfile string `yaml:"browser_profile" json:"browser_profile"`
}

type ProviderApple struct {
	Email          string `yaml:"email" json:"email" secret:"true"`
	Password       string `yaml:"password" json:"password" secret:"true"`
	BrowserProfile string `yaml:"browser_profile" json:"browser_profile"`
}

type Wallets struct {
	Solana    SolanaWallet    `yaml:"solana" json:"solana"`
	Bittensor BittensorWallet `yaml:"bittensor" json:"bittensor"`
}

type SolanaWallet struct {
	Address          string `yaml:"address" json:"address"`
	PrivateKeyBase58 string `yaml:"private_key_base58" json:"private_key_base58" secret:"true"`
}

type BittensorWallet struct {
	Address    string `yaml:"address" json:"address"`
	Mnemonic   string `yaml:"mnemonic" json:"mnemonic" secret:"true"`
	SS58Prefix int    `yaml:"ss58_prefix" json:"ss58_prefix"`
}

// Payments configures the real-money acceptance cases: a USDC transfer on
// Solana mainnet into the production merchant address, so the campaign proves
// the path a paying customer actually takes rather than a synthetic webhook.
//
// Every field here is optional. An unconfigured section is the normal state
// and means the payment cases report SKIP -- the same shape as
// lifecycle.allow_account_create_delete, which also guards an action the suite
// must never take by accident. Money is different from an account, though: an
// account the suite creates it can also delete, and a transfer it broadcasts
// is gone. So the guard is off by default and two ceilings bound the damage a
// misconfiguration can do.
type Payments struct {
	// AllowRealUsdcSpend must be true before any transfer is built. While it
	// is false the payer is never even constructed.
	AllowRealUsdcSpend bool `yaml:"allow_real_usdc_spend" json:"allow_real_usdc_spend"`
	// MaxSpendUsd caps a single payment.
	MaxSpendUsd float64 `yaml:"max_spend_usd" json:"max_spend_usd"`
	// MaxCampaignSpendUsd caps everything one campaign may spend. A runner
	// that has spent this much refuses the next payment rather than
	// continuing.
	MaxCampaignSpendUsd float64 `yaml:"max_campaign_spend_usd" json:"max_campaign_spend_usd"`
	// SolanaRpcUrl is the JSON-RPC endpoint the payer broadcasts through.
	// The public mainnet endpoint rate-limits hard; use the Helius endpoint
	// the server already reconciles against.
	SolanaRpcUrl string `yaml:"solana_rpc_url" json:"solana_rpc_url" optional:"true" secret:"true"`
	// Payer is the funded wallet. NEVER a production wallet: it should hold
	// only what a campaign is allowed to spend, so that a bug costs that and
	// nothing more.
	Payer PayerWallet `yaml:"payer" json:"payer"`
}

type PayerWallet struct {
	Address          string `yaml:"address" json:"address" optional:"true"`
	PrivateKeyBase58 string `yaml:"private_key_base58" json:"private_key_base58" optional:"true" secret:"true"`
}

// Enabled reports whether the payment cases may spend. A section that is
// enabled but incomplete is a configuration error, not a skip -- see Validate.
func (p Payments) Enabled() bool {
	return p.AllowRealUsdcSpend
}

func Load(path string) (*Config, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("read tests config: %w", err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open tests config: %w", err)
	}
	defer f.Close()

	var config Config
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parse tests config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("parse tests config: multiple YAML documents are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse tests config: %w", err)
	}
	if err := config.Validate(false); err != nil {
		return nil, err
	}
	return &config, nil
}

// LoadJSON reads the private, resolved fixture emitted by test-config
// write-json. Platform tests use this instead of reparsing the vault file.
func LoadJSON(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open tests JSON: %w", err)
	}
	defer f.Close()

	var config Config
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parse tests JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("parse tests JSON: multiple JSON values are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse tests JSON: %w", err)
	}
	if err := config.Validate(true); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *Config) Validate(ready bool) error {
	var problems []string
	if c.Version != Version {
		problems = append(problems, fmt.Sprintf("version must be %d", Version))
	}
	if isConfiguredString(c.Android.UnlockCode) && !unlockPattern.MatchString(c.Android.UnlockCode) {
		problems = append(problems, "android.unlock_code must be 4..16 decimal digits")
	}
	if ready && !c.Lifecycle.AllowAccountCreateDelete {
		problems = append(problems, "lifecycle.allow_account_create_delete must be true")
	}
	if len(c.EmailVerification.BypassDomains) == 0 {
		problems = append(problems, "email_verification.bypass_domains must not be empty")
	}
	if !c.EmailVerification.SuppressAccountMessages {
		problems = append(problems, "email_verification.suppress_account_messages must be true")
	}
	seenDomains := map[string]bool{}
	for _, domain := range c.EmailVerification.BypassDomains {
		if domain != strings.ToLower(domain) || !domainPattern.MatchString(domain) {
			problems = append(problems, "email_verification.bypass_domains contains an invalid exact domain")
		}
		if seenDomains[domain] {
			problems = append(problems, "email_verification.bypass_domains contains a duplicate")
		}
		seenDomains[domain] = true
	}
	if !seenDomains[c.Signup.Email.Domain] {
		problems = append(problems, "signup.email.domain must be listed in email_verification.bypass_domains")
	}
	if !validPrefix(c.Signup.NetworkNamePrefix, 24) {
		problems = append(problems, "signup.network_name_prefix must be 1..24 lowercase letters, digits, or hyphens")
	}
	if !validPrefix(c.Signup.Email.LocalPartPrefix, 32) {
		problems = append(problems, "signup.email.local_part_prefix must be 1..32 lowercase letters, digits, or hyphens")
	}
	if isConfiguredString(c.Signup.Password) && len(c.Signup.Password) < 12 {
		problems = append(problems, "signup.password must be at least 12 characters")
	}
	if isConfiguredString(c.Signup.Phone.Number) && !phonePattern.MatchString(c.Signup.Phone.Number) {
		problems = append(problems, "signup.phone.number must be an E.164 number")
	}
	seenSeedphraseBypassIPs := map[netip.Addr]bool{}
	if ready && len(c.Signup.SeedphraseRateLimitBypassIPs) == 0 {
		problems = append(problems, "signup.seedphrase_rate_limit_bypass_ips must not be empty")
	}
	for _, ip := range c.Signup.SeedphraseRateLimitBypassIPs {
		addr, err := netip.ParseAddr(ip)
		if err != nil || addr.Is4In6() || addr.String() != ip {
			problems = append(problems, "signup.seedphrase_rate_limit_bypass_ips contains an invalid canonical IP address")
			continue
		}
		if seenSeedphraseBypassIPs[addr] {
			problems = append(problems, "signup.seedphrase_rate_limit_bypass_ips contains a duplicate")
		}
		seenSeedphraseBypassIPs[addr] = true
	}
	if c.Wallets.Bittensor.SS58Prefix < 0 || c.Wallets.Bittensor.SS58Prefix > 16383 {
		problems = append(problems, "wallets.bittensor.ss58_prefix must be between 0 and 16383")
	}
	problems = append(problems, c.Payments.problems()...)
	if ready {
		for path, value := range c.StringValues() {
			if strings.TrimSpace(value) == "" || strings.HasPrefix(value, "REPLACE_ME") {
				problems = append(problems, path+" is not configured")
			}
		}
		if _, err := walletfixture.NewSolana(c.Wallets.Solana.Address, c.Wallets.Solana.PrivateKeyBase58); err != nil {
			problems = append(problems, "wallets.solana: "+err.Error())
		}
		if _, err := walletfixture.NewBittensor(c.Wallets.Bittensor.Address, c.Wallets.Bittensor.Mnemonic, c.Wallets.Bittensor.SS58Prefix); err != nil {
			problems = append(problems, "wallets.bittensor: "+err.Error())
		}
		for path, value := range map[string]string{
			"data_plane_account.email":        c.DataPlaneAccount.Email,
			"providers.google.email":          c.Providers.Google.Email,
			"providers.google.recovery_email": c.Providers.Google.RecoveryEmail,
			"providers.apple.email":           c.Providers.Apple.Email,
		} {
			if isConfiguredString(value) && !validEmail(value) {
				problems = append(problems, path+" must be an email address with an exact DNS domain")
			}
		}
		totp := strings.ReplaceAll(c.Providers.Google.TOTPSecret, " ", "")
		if isConfiguredString(c.Providers.Google.TOTPSecret) && (len(totp) < 16 || !totpPattern.MatchString(c.Providers.Google.TOTPSecret)) {
			problems = append(problems, "providers.google.totp_secret must be a base32 secret of at least 16 characters")
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// problems validates the payments section. An unconfigured section is silent:
// not spending is the default and needs no ceremony. A section that is turned
// ON, though, is checked hard and at every level, because each field is
// something that -- wrong -- either loses money or lets the suite spend more
// of it than anyone intended.
//
// This runs in both validation modes, not just ready. A machine that never
// runs the payment cases should still be told its guard is half-set, rather
// than discovering it mid-campaign.
func (p Payments) problems() []string {
	var problems []string

	// The ceilings are meaningful even while the guard is off: they are what
	// bounds the blast radius the moment someone turns it on.
	if p.MaxSpendUsd < 0 {
		problems = append(problems, "payments.max_spend_usd must not be negative")
	}
	if p.MaxCampaignSpendUsd < 0 {
		problems = append(problems, "payments.max_campaign_spend_usd must not be negative")
	}
	if 0 < p.MaxSpendUsd && 0 < p.MaxCampaignSpendUsd && p.MaxCampaignSpendUsd < p.MaxSpendUsd {
		problems = append(problems,
			"payments.max_campaign_spend_usd must not be less than payments.max_spend_usd")
	}

	if !p.AllowRealUsdcSpend {
		// Nothing else is required, but a half-configured section that looks
		// armed is worth naming: someone filled in a funded wallet and
		// expected it to be used.
		if isConfiguredString(p.Payer.PrivateKeyBase58) && !isConfiguredString(p.Payer.Address) {
			problems = append(problems,
				"payments.payer.address must be set alongside payments.payer.private_key_base58")
		}
		return problems
	}

	if !isConfiguredString(p.SolanaRpcUrl) {
		problems = append(problems,
			"payments.solana_rpc_url is required when payments.allow_real_usdc_spend is true")
	} else if !strings.HasPrefix(p.SolanaRpcUrl, "https://") {
		// Broadcasting a signed transfer over plaintext is not acceptable
		// even in a test.
		problems = append(problems, "payments.solana_rpc_url must be an https url")
	}
	if !isConfiguredString(p.Payer.Address) {
		problems = append(problems,
			"payments.payer.address is required when payments.allow_real_usdc_spend is true")
	}
	if !isConfiguredString(p.Payer.PrivateKeyBase58) {
		problems = append(problems,
			"payments.payer.private_key_base58 is required when payments.allow_real_usdc_spend is true")
	}
	if isConfiguredString(p.Payer.Address) && isConfiguredString(p.Payer.PrivateKeyBase58) {
		// Same check walletfixture makes for the signing wallet: prove the
		// key belongs to the address before the suite is allowed to spend
		// from it.
		if _, err := walletfixture.NewSolana(p.Payer.Address, p.Payer.PrivateKeyBase58); err != nil {
			problems = append(problems, "payments.payer: "+err.Error())
		}
	}
	// A ceiling of zero with spending enabled would refuse every payment,
	// which reads as a broken campaign rather than a deliberate one.
	if p.MaxSpendUsd <= 0 {
		problems = append(problems,
			"payments.max_spend_usd must be greater than zero when payments.allow_real_usdc_spend is true")
	}
	if p.MaxCampaignSpendUsd <= 0 {
		problems = append(problems,
			"payments.max_campaign_spend_usd must be greater than zero when payments.allow_real_usdc_spend is true")
	}
	return problems
}

func validPrefix(value string, maxLength int) bool {
	return 0 < len(value) && len(value) <= maxLength && prefixPattern.MatchString(value)
}

func isConfiguredString(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.HasPrefix(value, "REPLACE_ME")
}

func validEmail(value string) bool {
	if len(value) == 0 || 254 < len(value) || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	at := strings.LastIndexByte(value, '@')
	if at <= 0 || 64 < at || at == len(value)-1 {
		return false
	}
	domain := value[at+1:]
	return domain == strings.ToLower(domain) && domainPattern.MatchString(domain)
}

func (c *Config) StringValues() map[string]string {
	values := map[string]string{}
	collectStrings(reflect.ValueOf(c).Elem(), reflect.TypeOf(*c), "", values)
	return values
}

func collectStrings(value reflect.Value, typ reflect.Type, prefix string, values map[string]string) {
	for i := 0; i < value.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fieldValue := value.Field(i)
		switch fieldValue.Kind() {
		case reflect.String:
			// Validate(ready) requires every collected string to be set.
			// Fields tagged optional are exempt: the payments section is
			// unconfigured on every machine that is not running the
			// real-money cases, and that is not a provisioning failure.
			if field.Tag.Get("optional") == "true" {
				continue
			}
			values[path] = fieldValue.String()
		case reflect.Struct:
			collectStrings(fieldValue, field.Type, path, values)
		}
	}
}

func (c *Config) Get(path string) (string, error) {
	if !pathPattern.MatchString(path) {
		return "", errors.New("invalid config path")
	}
	value := reflect.ValueOf(c).Elem()
	typ := value.Type()
	for _, part := range strings.Split(path, ".") {
		index := -1
		for i := 0; i < typ.NumField(); i++ {
			if strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0] == part {
				index = i
				break
			}
		}
		if index < 0 {
			return "", fmt.Errorf("unknown config path %q", path)
		}
		value = value.Field(index)
		typ = value.Type()
	}
	switch value.Kind() {
	case reflect.String:
		return value.String(), nil
	case reflect.Int:
		return strconv.FormatInt(value.Int(), 10), nil
	case reflect.Bool:
		return strconv.FormatBool(value.Bool()), nil
	case reflect.Float64:
		// Shell callers read the spend ceilings; 'f' with -1 precision keeps
		// 25 as "25" and 12.5 as "12.5" rather than 1.25e+01.
		return strconv.FormatFloat(value.Float(), 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("config path %q is not a scalar", path)
	}
}

func ResolveProfile(configPath, profile string) string {
	if profile == "" || filepath.IsAbs(profile) {
		return profile
	}
	return filepath.Join(filepath.Dir(configPath), profile)
}

// ResolveProfiles returns a copy suitable for distribution to platform tests.
// Relative private browser-state paths are anchored to tests.yml before the
// JSON leaves the parser process.
func (c Config) ResolveProfiles(configPath string) Config {
	c.Providers.Google.BrowserProfile = ResolveProfile(configPath, c.Providers.Google.BrowserProfile)
	c.Providers.Apple.BrowserProfile = ResolveProfile(configPath, c.Providers.Apple.BrowserProfile)
	return c
}

// ValidateProvisioned verifies the private provider state that cannot be
// represented inline in tests.yml. Provider profiles are Playwright
// storage-state files, not browser user-data directories. Keeping this check
// in the central parser gives every platform the same fail-fast behavior and
// prevents an acceptance run from reaching main with stale or world-readable
// provider credentials.
func (c *Config) ValidateProvisioned(configPath string) error {
	providers := []struct {
		name          string
		profile       string
		cookieDomains []string
	}{
		{name: "google", profile: c.Providers.Google.BrowserProfile, cookieDomains: []string{"google.com", "accounts.google.com"}},
		{name: "apple", profile: c.Providers.Apple.BrowserProfile, cookieDomains: []string{"apple.com", "icloud.com"}},
	}

	var problems []string
	for _, provider := range providers {
		path := ResolveProfile(configPath, provider.profile)
		if err := validateBrowserProfile(path, provider.cookieDomains); err != nil {
			problems = append(problems, fmt.Sprintf("providers.%s.browser_profile: %v", provider.name, err))
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

type browserStorageState struct {
	Cookies []struct {
		Name    string  `json:"name"`
		Value   string  `json:"value"`
		Domain  string  `json:"domain"`
		Path    string  `json:"path"`
		Expires float64 `json:"expires"`
	} `json:"cookies"`
	Origins []json.RawMessage `json:"origins"`
}

func validateBrowserProfile(path string, cookieDomains []string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("profile file is missing")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("profile must be a regular file, not a symlink or directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("profile permissions must not grant group or other access")
	}
	if info.Size() <= 0 || info.Size() > maxBrowserProfileBytes {
		return errors.New("profile size is outside the allowed range")
	}

	file, err := os.Open(path)
	if err != nil {
		return errors.New("profile cannot be opened")
	}
	defer file.Close()
	var state browserStorageState
	decoder := json.NewDecoder(io.LimitReader(file, maxBrowserProfileBytes+1))
	if err := decoder.Decode(&state); err != nil {
		return errors.New("profile is not valid Playwright storage-state JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("profile contains multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("profile is not valid Playwright storage-state JSON")
	}

	now := float64(time.Now().Unix())
	for _, cookie := range state.Cookies {
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		matches := false
		for _, allowed := range cookieDomains {
			allowed = strings.ToLower(allowed)
			if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
				matches = true
				break
			}
		}
		if matches && cookie.Name != "" && cookie.Value != "" && (cookie.Expires <= 0 || cookie.Expires > now) {
			return nil
		}
	}
	return errors.New("profile has no live provider session cookie")
}
