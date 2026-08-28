// Package authcases exercises destructive, fully recoverable signup/login/
// delete lifecycles against an explicitly selected API deployment.
package authcases

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/urnetwork/build/all/acceptance/testconfig"
	"github.com/urnetwork/build/all/acceptance/walletfixture"
)

const maxResponseBytes = 1024 * 1024

type Result struct {
	Case   string
	Status string
	Detail string
}

type Runner struct {
	APIURL string
	Config *testconfig.Config
	Client *http.Client
}

type apiError struct {
	Message string `json:"message"`
}

type network struct {
	ByJWT string `json:"by_jwt"`
}

type networkCreateResult struct {
	Network              *network `json:"network,omitempty"`
	VerificationRequired *struct {
		UserAuth string `json:"user_auth"`
	} `json:"verification_required,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type passwordLoginResult struct {
	Network              *network `json:"network,omitempty"`
	VerificationRequired *struct {
		UserAuth string `json:"user_auth"`
	} `json:"verification_required,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type walletAuth struct {
	WalletAddress   string `json:"wallet_address"`
	WalletSignature string `json:"wallet_signature"`
	WalletMessage   string `json:"wallet_message"`
	Blockchain      string `json:"blockchain"`
}

type authLoginResult struct {
	Network    *network    `json:"network,omitempty"`
	WalletAuth *walletAuth `json:"wallet_auth,omitempty"`
	Error      *apiError   `json:"error,omitempty"`
}

func (r *Runner) Run(ctx context.Context, cases []string) []Result {
	results := make([]Result, 0, len(cases))
	for _, name := range cases {
		var err error
		switch name {
		case "email":
			err = r.runEmail(ctx)
		case "phone":
			err = r.runPhone(ctx)
		case "solana":
			err = r.runWallet(ctx, r.solanaSigner)
		case "bittensor":
			err = r.runWallet(ctx, r.bittensorSigner)
		default:
			err = fmt.Errorf("unknown auth case %q", name)
		}
		if err != nil {
			results = append(results, Result{Case: name, Status: "FAIL", Detail: r.redact(err.Error())})
		} else {
			results = append(results, Result{Case: name, Status: "PASS", Detail: "signup, fresh login, and account deletion succeeded"})
		}
	}
	return results
}

func (r *Runner) httpClient() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *Runner) post(ctx context.Context, path string, body any, jwt string, output any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.APIURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Client-Version", "1.0.0-auth-acceptance")
	if jwt != "" {
		request.Header.Set("Authorization", "Bearer "+jwt)
	}
	response, err := r.httpClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var result struct {
			Error *apiError `json:"error"`
		}
		_ = json.Unmarshal(data, &result)
		if result.Error != nil && result.Error.Message != "" {
			return fmt.Errorf("%s returned HTTP %d: %s", path, response.StatusCode, result.Error.Message)
		}
		return fmt.Errorf("%s returned HTTP %d", path, response.StatusCode)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (r *Runner) createPassword(ctx context.Context, userAuth, networkName string, numeric bool) (*networkCreateResult, error) {
	var result networkCreateResult
	err := r.post(ctx, "/auth/network-create", map[string]any{
		"user_auth":          userAuth,
		"password":           r.Config.Signup.Password,
		"network_name":       networkName,
		"terms":              true,
		"verify_use_numeric": numeric,
	}, "", &result)
	if err == nil && result.Error != nil {
		err = errors.New(result.Error.Message)
	}
	return &result, err
}

func (r *Runner) loginPassword(ctx context.Context, userAuth string) (*passwordLoginResult, error) {
	var result passwordLoginResult
	err := r.post(ctx, "/auth/login-with-password", map[string]any{
		"user_auth":          userAuth,
		"password":           r.Config.Signup.Password,
		"verify_otp_numeric": true,
	}, "", &result)
	if err == nil && result.Error != nil {
		err = errors.New(result.Error.Message)
	}
	return &result, err
}

func (r *Runner) deleteNetwork(ctx context.Context, jwt string) error {
	if jwt == "" {
		return nil
	}
	var result struct {
		Error *apiError `json:"error,omitempty"`
	}
	if err := r.post(ctx, "/auth/network-delete", struct{}{}, jwt, &result); err != nil {
		return err
	}
	if result.Error != nil {
		return errors.New(result.Error.Message)
	}
	return nil
}

func (r *Runner) runEmail(ctx context.Context) (returnErr error) {
	userAuth := fmt.Sprintf("%s-%s@%s", r.Config.Signup.Email.LocalPartPrefix, suffix(), r.Config.Signup.Email.Domain)
	created, err := r.createPassword(ctx, userAuth, r.networkName("email"), false)
	if err != nil {
		return fmt.Errorf("email signup: %w", err)
	}
	if created.VerificationRequired != nil {
		return errors.New("test-domain email signup unexpectedly required verification")
	}
	if created.Network == nil || created.Network.ByJWT == "" {
		return errors.New("email signup returned no network JWT")
	}
	jwt := created.Network.ByJWT
	defer func() { returnErr = errors.Join(returnErr, cleanupError(r.deleteNetwork(ctx, jwt))) }()

	loggedIn, err := r.loginPassword(ctx, userAuth)
	if err != nil {
		return fmt.Errorf("email password login: %w", err)
	}
	if loggedIn.Network == nil || loggedIn.Network.ByJWT == "" {
		return errors.New("email password login returned no network JWT")
	}
	return sameNetwork(jwt, loggedIn.Network.ByJWT)
}

func (r *Runner) runPhone(ctx context.Context) (returnErr error) {
	userAuth := r.Config.Signup.Phone.Number
	// The exact phone fixture in tests.yml is verified by server policy. Recover
	// an account left by an interrupted campaign before creating it again.
	// "User does not exist" is the expected clean starting state.
	stale, err := r.loginPassword(ctx, userAuth)
	if err != nil {
		if !strings.Contains(err.Error(), "User does not exist") {
			return fmt.Errorf("inspect stale phone fixture: %w", err)
		}
	} else if stale.Network != nil && stale.Network.ByJWT != "" {
		if err := r.deleteNetwork(ctx, stale.Network.ByJWT); err != nil {
			return fmt.Errorf("remove stale phone fixture: %w", err)
		}
	} else if stale.VerificationRequired != nil {
		return errors.New("configured phone login unexpectedly required verification")
	} else {
		return errors.New("stale phone fixture lookup returned no usable state")
	}
	created, err := r.createPassword(ctx, userAuth, r.networkName("phone"), true)
	if err != nil {
		return fmt.Errorf("phone signup: %w", err)
	}
	if created.VerificationRequired != nil {
		return errors.New("configured phone signup unexpectedly required verification")
	}
	if created.Network == nil || created.Network.ByJWT == "" {
		return errors.New("phone signup returned no network JWT")
	}
	jwt := created.Network.ByJWT
	defer func() { returnErr = errors.Join(returnErr, cleanupError(r.deleteNetwork(ctx, jwt))) }()
	loggedIn, err := r.loginPassword(ctx, userAuth)
	if err != nil {
		return fmt.Errorf("phone password login: %w", err)
	}
	if loggedIn.Network == nil || loggedIn.Network.ByJWT == "" {
		return errors.New("phone password login returned no network JWT")
	}
	return sameNetwork(jwt, loggedIn.Network.ByJWT)
}

func (r *Runner) solanaSigner() (walletfixture.Signer, error) {
	return walletfixture.NewSolana(r.Config.Wallets.Solana.Address, r.Config.Wallets.Solana.PrivateKeyBase58)
}

func (r *Runner) bittensorSigner() (walletfixture.Signer, error) {
	return walletfixture.NewBittensor(r.Config.Wallets.Bittensor.Address, r.Config.Wallets.Bittensor.Mnemonic, r.Config.Wallets.Bittensor.SS58Prefix)
}

func (r *Runner) signedChallenge(ctx context.Context, signer walletfixture.Signer) (*walletAuth, error) {
	var challenge struct {
		MessageTemplate string    `json:"message_template"`
		Error           *apiError `json:"error,omitempty"`
	}
	if err := r.post(ctx, "/auth/wallet-challenge", map[string]any{
		"wallet_address": signer.Address(),
		"blockchain":     signer.Blockchain(),
	}, "", &challenge); err != nil {
		return nil, err
	}
	if challenge.Error != nil {
		return nil, errors.New(challenge.Error.Message)
	}
	if challenge.MessageTemplate == "" {
		return nil, errors.New("wallet challenge returned no message")
	}
	signature, err := signer.Sign(challenge.MessageTemplate)
	if err != nil {
		return nil, err
	}
	return &walletAuth{
		WalletAddress: signer.Address(), WalletSignature: signature,
		WalletMessage: challenge.MessageTemplate, Blockchain: signer.Blockchain(),
	}, nil
}

func (r *Runner) loginWallet(ctx context.Context, signer walletfixture.Signer) (*authLoginResult, error) {
	auth, err := r.signedChallenge(ctx, signer)
	if err != nil {
		return nil, err
	}
	var result authLoginResult
	if err := r.post(ctx, "/auth/login", map[string]any{"wallet_auth": auth}, "", &result); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, errors.New(result.Error.Message)
	}
	return &result, nil
}

func (r *Runner) runWallet(ctx context.Context, makeSigner func() (walletfixture.Signer, error)) (returnErr error) {
	signer, err := makeSigner()
	if err != nil {
		return err
	}
	discovery, err := r.loginWallet(ctx, signer)
	if err != nil {
		return fmt.Errorf("wallet discovery: %w", err)
	}
	// A dedicated fixture left behind by an interrupted campaign is recovered
	// and removed before testing a new signup. This is deliberately guarded by
	// lifecycle.allow_account_create_delete in the parsed fixture.
	if discovery.Network != nil && discovery.Network.ByJWT != "" {
		if err := r.deleteNetwork(ctx, discovery.Network.ByJWT); err != nil {
			return fmt.Errorf("remove stale wallet fixture: %w", err)
		}
		discovery, err = r.loginWallet(ctx, signer)
		if err != nil {
			return fmt.Errorf("wallet rediscovery: %w", err)
		}
	}
	if discovery.WalletAuth == nil || discovery.WalletAuth.WalletAddress != signer.Address() {
		return errors.New("unregistered wallet discovery did not return the wallet identity")
	}

	// Discovery consumed its challenge. Creation must sign a new address-bound
	// challenge, exactly like the product clients.
	createAuth, err := r.signedChallenge(ctx, signer)
	if err != nil {
		return fmt.Errorf("wallet create challenge: %w", err)
	}
	var created networkCreateResult
	if err := r.post(ctx, "/auth/network-create", map[string]any{
		"network_name": r.networkName(strings.ToLower(signer.Blockchain())),
		"terms":        true,
		"wallet_auth":  createAuth,
	}, "", &created); err != nil {
		return fmt.Errorf("wallet signup: %w", err)
	}
	if created.Error != nil {
		return errors.New(created.Error.Message)
	}
	if created.Network == nil || created.Network.ByJWT == "" {
		return errors.New("wallet signup returned no network JWT")
	}
	jwt := created.Network.ByJWT
	defer func() { returnErr = errors.Join(returnErr, cleanupError(r.deleteNetwork(ctx, jwt))) }()

	loggedIn, err := r.loginWallet(ctx, signer)
	if err != nil {
		return fmt.Errorf("wallet login: %w", err)
	}
	if loggedIn.Network == nil || loggedIn.Network.ByJWT == "" {
		return errors.New("registered wallet login returned no network JWT")
	}
	return sameNetwork(jwt, loggedIn.Network.ByJWT)
}

func (r *Runner) networkName(method string) string {
	prefix := strings.Trim(strings.ToLower(r.Config.Signup.NetworkNamePrefix), "-")
	name := prefix + "-" + method + "-" + suffix()
	if len(name) > 49 {
		name = name[:49]
	}
	return strings.TrimRight(name, "-")
}

func suffix() string {
	value := make([]byte, 6)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(value))
}

func sameNetwork(firstJWT, secondJWT string) error {
	first, err := jwtClaim(firstJWT, "network_id")
	if err != nil {
		return err
	}
	second, err := jwtClaim(secondJWT, "network_id")
	if err != nil {
		return err
	}
	if first != second {
		return errors.New("fresh login returned a different network")
	}
	return nil
}

func jwtClaim(token, name string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("API returned an invalid JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("API returned an invalid JWT payload")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("API returned an invalid JWT claims object")
	}
	value, ok := claims[name].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("API JWT has no %s claim", name)
	}
	return value, nil
}

func cleanupError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("delete acceptance account: %w", err)
}

func (r *Runner) redact(message string) string {
	values := []string{
		r.Config.DataPlaneAccount.Email, r.Config.DataPlaneAccount.Password,
		r.Config.Signup.Password, r.Config.Signup.Phone.Number,
		r.Config.Providers.Google.Email, r.Config.Providers.Google.Password, r.Config.Providers.Google.TOTPSecret,
		r.Config.Providers.Google.RecoveryEmail,
		r.Config.Providers.Apple.Email, r.Config.Providers.Apple.Password,
		r.Config.Wallets.Solana.Address, r.Config.Wallets.Solana.PrivateKeyBase58,
		r.Config.Wallets.Bittensor.Address, r.Config.Wallets.Bittensor.Mnemonic,
	}
	for _, value := range values {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return strings.Map(func(char rune) rune {
		if char == '\n' || char == '\r' || char == '\t' {
			return ' '
		}
		return char
	}, message)
}
