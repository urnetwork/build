// Package paycases exercises the USDC payment paths against a live API with
// real money.
//
// What this proves that nothing else does. Every existing Solana test in the
// tree hand-builds a *SolanaTransaction and hands it straight to the Helius
// webhook. That proves the grant logic and nothing about the chain: it cannot
// catch a merchant address that drifted, a mint that changed, a reference the
// indexer never surfaces, a webhook that stopped being called, or a price the
// server quotes but will not honour. These cases pay the way a customer pays
// and then assert the entitlement arrived.
//
// What it costs. Each case moves real USDC on Solana mainnet into the
// production merchant address, and that money is not recoverable. So nothing
// here runs unless vault tests.yml sets payments.allow_real_usdc_spend, and
// two ceilings -- per payment and per campaign -- bound what a misconfigured
// run can spend. A disabled payments section reports SKIP, which is the normal
// state for a machine that is not deliberately spending.
//
// Once spending IS enabled, a product surface that cannot be paid is a
// failure, not a skip. x402 answering 404 because nobody configured its
// facilitator is a real gap in a shipped, documented feature, and the matrix
// should keep saying so.
//
// Each case creates its own throwaway network, pays into it, and deletes it,
// on the same create/defer-delete shape authcases uses. The entitlement is
// bought and then thrown away; what is being tested is that it arrived.
package paycases

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/build/all/acceptance/testconfig"
	"github.com/urnetwork/build/all/acceptance/usdcpay"
)

const maxResponseBytes = 1024 * 1024

// MerchantAddress is where a URnetwork USDC payment goes. It is the first
// entry of the server's solanaReceiverAddresses and the same address the site
// and the apps put in a payment url. A transfer to any other address is not a
// payment, so this is pinned rather than read from the config: if it drifts,
// the case should fail loudly rather than quietly pay somewhere else.
const MerchantAddress = "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM"

// Case names, matching the acceptance result matrix.
const (
	CaseSubscription = "usdc-subscription"
	CaseDataPack     = "usdc-data-pack"
	CaseX402         = "x402-purchase"
)

// AllCases is every case this package knows, in the order a campaign runs
// them: cheapest first, so a budget exhausted late costs as little as
// possible.
var AllCases = []string{CaseDataPack, CaseSubscription, CaseX402}

// grantTimeout bounds the wait between a finalized transfer and the
// entitlement appearing. The webhook is push-driven and usually lands within
// seconds; the reconciler is the backstop. Waiting longer than this and
// reporting a failure is the right trade -- a payment that takes ten minutes
// to apply is a defect a customer would report.
const (
	grantTimeout      = 10 * time.Minute
	grantPollInterval = 10 * time.Second
)

type Result struct {
	Case   string
	Status string
	Detail string
}

type apiError struct {
	Message string `json:"message"`
}

type networkRef struct {
	ByJWT string `json:"by_jwt"`
}

type networkCreateResult struct {
	Network              *networkRef `json:"network,omitempty"`
	VerificationRequired *struct {
		UserAuth string `json:"user_auth"`
	} `json:"verification_required,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type subscriptionIntentResult struct {
	AmountUsd        float64   `json:"amount_usd,omitempty"`
	Tier             string    `json:"tier,omitempty"`
	Plan             string    `json:"plan,omitempty"`
	RegularAmountUsd float64   `json:"regular_amount_usd,omitempty"`
	OfferApplied     bool      `json:"offer_applied,omitempty"`
	Currency         string    `json:"currency,omitempty"`
	Error            *apiError `json:"error,omitempty"`
}

type dataIntentResult struct {
	AmountUsd   float64    `json:"amount_usd,omitempty"`
	Reference   string     `json:"reference,omitempty"`
	Memo        string     `json:"memo,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	NetworkName string     `json:"network_name,omitempty"`
	Error       *apiError  `json:"error,omitempty"`
}

type dataStatusResult struct {
	Status      string    `json:"status"`
	ItemId      string    `json:"item_id,omitempty"`
	NetworkName string    `json:"network_name,omitempty"`
	AmountUsd   float64   `json:"amount_usd,omitempty"`
	Error       *apiError `json:"error,omitempty"`
}

type subscription struct {
	Store string `json:"store"`
	Plan  string `json:"plan"`
}

type balanceResult struct {
	BalanceByteCount      int64           `json:"balance_byte_count"`
	StartBalanceByteCount int64           `json:"start_balance_byte_count"`
	CurrentSubscription   *subscription   `json:"current_subscription,omitempty"`
	Subscriptions         []*subscription `json:"subscriptions,omitempty"`
	Error                 *apiError       `json:"error,omitempty"`
}

// Runner holds everything a payment case needs. Payer is nil when the vault
// has not enabled spending, which is what turns every case into a SKIP.
type Runner struct {
	APIURL string
	Config *testconfig.Config
	Client *http.Client

	payer *usdcpay.Payer
	// spent is what this runner has already moved, against
	// payments.max_campaign_spend_usd.
	spent float64
	// skipReason explains a disabled or unusable payment configuration.
	skipReason string
}

// NewRunner builds a runner. A vault with payments off yields a runner whose
// cases all SKIP; it does not yield an error, because not spending is the
// normal state.
func NewRunner(apiURL string, config *testconfig.Config, client *http.Client) (*Runner, error) {
	runner := &Runner{APIURL: apiURL, Config: config, Client: client}

	payments := config.Payments
	if !payments.Enabled() {
		runner.skipReason = "payments.allow_real_usdc_spend is not true in the tests vault"
		return runner, nil
	}
	payer, err := usdcpay.NewPayer(
		payments.SolanaRpcUrl,
		payments.Payer.PrivateKeyBase58,
		payments.Payer.Address,
		payments.MaxSpendUsd,
	)
	if err != nil {
		// An enabled-but-broken payer is a configuration failure, not a skip:
		// somebody asked for these cases to run.
		return nil, fmt.Errorf("payments payer: %w", err)
	}
	runner.payer = payer
	return runner, nil
}

// PayerAddress is the wallet the operator funds. Empty when payments are off.
func (r *Runner) PayerAddress() string {
	if r.payer == nil {
		return ""
	}
	return r.payer.Address()
}

// Preflight reports the payer's balances so an operator sees what is missing
// before a campaign burns an hour reaching the payment phase. It is advisory:
// a failure here is reported, not fatal.
func (r *Runner) Preflight(ctx context.Context) (usdc float64, sol float64, err error) {
	if r.payer == nil {
		return 0, 0, errors.New(r.skipReason)
	}
	return r.payer.Balances(ctx)
}

func (r *Runner) Run(ctx context.Context, cases []string) []Result {
	results := make([]Result, 0, len(cases))
	for _, name := range cases {
		results = append(results, r.runOne(ctx, name))
	}
	return results
}

func (r *Runner) runOne(ctx context.Context, name string) Result {
	if r.payer == nil {
		return Result{Case: name, Status: "SKIP", Detail: r.skipReason}
	}
	var err error
	var detail string
	switch name {
	case CaseSubscription:
		detail, err = r.runSubscription(ctx)
	case CaseDataPack:
		detail, err = r.runDataPack(ctx)
	case CaseX402:
		detail, err = r.runX402(ctx)
	default:
		err = fmt.Errorf("unknown payment case %q", name)
	}
	switch {
	case errors.Is(err, errSkip):
		return Result{Case: name, Status: "SKIP", Detail: r.redact(unwrapSkip(err))}
	case err != nil:
		return Result{Case: name, Status: "FAIL", Detail: r.redact(err.Error())}
	default:
		return Result{Case: name, Status: "PASS", Detail: r.redact(detail)}
	}
}

// errSkip marks an authorized exclusion discovered at runtime -- today, x402
// being switched off on the deployment under test.
var errSkip = errors.New("skip")

func skipf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSkip, fmt.Sprintf(format, args...))
}

func unwrapSkip(err error) string {
	return strings.TrimPrefix(err.Error(), "skip: ")
}

// ----- the cases -----

// runSubscription buys a Pro year the way the site and the Android app do:
// register an intent, pay the amount the SERVER quoted, wait for the
// entitlement.
func (r *Runner) runSubscription(ctx context.Context) (detail string, returnErr error) {
	jwt, cleanup, err := r.createNetwork(ctx, "usdc-sub")
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, cleanup()) }()

	reference, err := newReference()
	if err != nil {
		return "", err
	}

	var intent subscriptionIntentResult
	if err := r.post(ctx, "/solana/payment-intent", map[string]any{
		"reference": reference,
		// The plan must be sent: without it the server answers "Unknown
		// plan." and the wallet never opens. That was a real shipped bug.
		"plan": "yearly",
	}, jwt, &intent); err != nil {
		return "", fmt.Errorf("register payment intent: %w", err)
	}
	if intent.Error != nil {
		return "", fmt.Errorf("register payment intent: %s", intent.Error.Message)
	}
	// The client never names its own price: whatever the server quoted is what
	// gets paid, and a quote of zero or nonsense is a failure rather than
	// something to paper over with a default.
	if intent.AmountUsd <= 0 {
		return "", fmt.Errorf("payment intent quoted a non-positive amount: %v", intent.AmountUsd)
	}

	payment, err := r.pay(ctx, intent.AmountUsd, reference)
	if err != nil {
		return "", err
	}

	if err := r.awaitPro(ctx, jwt); err != nil {
		return "", fmt.Errorf(
			"paid %.2f USDC in %s but the Pro entitlement did not arrive: %w",
			payment.AmountUsd, payment.Signature, err,
		)
	}
	return fmt.Sprintf(
		"paid %.2f USDC on Solana (%s) and the Pro year was granted",
		payment.AmountUsd, payment.Signature,
	), nil
}

// runDataPack buys a data pack through the unauthenticated path the Buy data
// page uses: the intent is keyed by network name, and the memo is the
// reference, which is how a transfer sent by hand from an exchange is matched.
func (r *Runner) runDataPack(ctx context.Context) (detail string, returnErr error) {
	jwt, cleanup, err := r.createNetwork(ctx, "usdc-data")
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, cleanup()) }()

	networkName, err := jwtClaim(jwt, "network_name")
	if err != nil {
		// Older tokens do not carry the name; fall back to the balance call,
		// which needs only the JWT.
		return "", fmt.Errorf("read network name: %w", err)
	}

	reference, err := newReference()
	if err != nil {
		return "", err
	}

	var intent dataIntentResult
	if err := r.post(ctx, "/pay/data/solana-intent", map[string]any{
		"item_id":      "data_1tib",
		"network_name": networkName,
		"reference":    reference,
	}, "", &intent); err != nil {
		return "", fmt.Errorf("register data intent: %w", err)
	}
	if intent.Error != nil {
		return "", fmt.Errorf("register data intent: %s", intent.Error.Message)
	}
	if intent.AmountUsd <= 0 {
		return "", fmt.Errorf("data intent quoted a non-positive amount: %v", intent.AmountUsd)
	}
	// The memo the server tells the customer to include must be the reference
	// it will match on. If those ever diverge, every hand-sent transfer is
	// unmatchable -- exactly the class of bug this case exists to catch.
	if intent.Memo != "" && intent.Memo != reference {
		return "", fmt.Errorf(
			"data intent asked for memo %q but registered reference %q", intent.Memo, reference,
		)
	}

	payment, err := r.pay(ctx, intent.AmountUsd, reference)
	if err != nil {
		return "", err
	}

	if err := r.awaitDataPaid(ctx, reference); err != nil {
		return "", fmt.Errorf(
			"paid %.2f USDC in %s but the data pack was not applied: %w",
			payment.AmountUsd, payment.Signature, err,
		)
	}
	return fmt.Sprintf(
		"paid %.2f USDC on Solana (%s) and the data pack was applied to %s",
		payment.AmountUsd, payment.Signature, networkName,
	), nil
}

// runX402 exercises the agent payment path: ask for a resource, get 402 with
// terms, sign a payment, retry.
//
// x402 is chain-neutral by design -- the server quotes base, solana and tempo
// and the agent picks whichever it holds funds on. This runner holds USDC on
// Solana, so it selects the Solana terms. That is a property of the payer, not
// of the product: nothing here should be read as saying x402 is Solana-only.
func (r *Runner) runX402(ctx context.Context) (detail string, returnErr error) {
	skus, status, err := r.getRaw(ctx, "/x402/skus")
	if err != nil {
		return "", fmt.Errorf("read x402 catalog: %w", err)
	}
	if status == http.StatusNotFound {
		// vault x402.yml with enabled:false, or enabled with a blank
		// facilitator, both of which 404 every x402 route.
		//
		// This is a FAILURE, not a skip. x402 is a shipped, documented
		// product surface: it is named in the docs corpus, in agents.md and
		// in the agent skill, and an agent that follows those instructions
		// today gets a 404. Recording that as an authorized exclusion would
		// let the gap sit in the matrix indefinitely looking like a choice
		// somebody made. The campaign should keep saying it is broken until
		// the facilitator is configured.
		return "", errors.New(
			"x402 is not configured: GET /x402/skus returned 404. " +
				"vault/<env>/x402.yml needs a CDP facilitator url and credential, a Stripe " +
				"deposit address under pay_to for every quoted network, and enabled: true. " +
				"Its header lists what is still missing",
		)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET /x402/skus returned HTTP %d", status)
	}

	var catalog struct {
		Networks []string `json:"networks"`
		Asset    string   `json:"asset"`
		Skus     []struct {
			SkuId     string `json:"sku_id"`
			AmountUsd string `json:"amount_usd"`
		} `json:"skus"`
	}
	if err := json.Unmarshal(skus, &catalog); err != nil {
		return "", fmt.Errorf("decode x402 catalog: %w", err)
	}
	if !contains(catalog.Networks, "solana") {
		// The payer holds USDC on Solana only. x402 is chain-neutral by
		// design, so dropping Solana from the quoted set is a legitimate
		// product decision -- but it leaves this case unable to pay, and
		// silently skipping would hide that the acceptance payer needs
		// funding on another chain.
		return "", fmt.Errorf(
			"x402 quotes %s but the acceptance payer holds USDC on Solana only; "+
				"fund the payer on a quoted chain or restore solana to x402.yml networks",
			strings.Join(catalog.Networks, ", "),
		)
	}

	jwt, cleanup, err := r.createNetwork(ctx, "x402")
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, cleanup()) }()

	// Step one: no X-PAYMENT, so the server must answer 402 with terms.
	terms, err := r.x402Quote(ctx, jwt, "pro_1month")
	if err != nil {
		return "", err
	}

	amountUsd := atomicToUsd(terms.MaxAmountRequired)
	if amountUsd <= 0 {
		return "", fmt.Errorf("x402 quoted a non-positive amount: %q", terms.MaxAmountRequired)
	}
	if err := r.budget(amountUsd); err != nil {
		return "", err
	}

	// Step two: sign a transfer of exactly the quoted amount to the quoted
	// payTo, wrap it as an X-PAYMENT payload, and retry the same request.
	header, signature, err := r.x402Payment(ctx, terms)
	if err != nil {
		return "", err
	}
	r.spent += amountUsd

	var purchase struct {
		Complete    bool      `json:"complete"`
		SkuId       string    `json:"sku_id"`
		Pro         bool      `json:"pro"`
		Transaction string    `json:"transaction,omitempty"`
		Network     string    `json:"network,omitempty"`
		Error       *apiError `json:"error,omitempty"`
	}
	status, err = r.postWithHeaders(ctx, "/x402/purchase", map[string]any{
		"sku_id":  "pro_1month",
		"network": "solana",
	}, jwt, map[string]string{"X-PAYMENT": header}, &purchase)
	if err != nil {
		return "", fmt.Errorf("x402 purchase (transfer %s): %w", signature, err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("x402 purchase returned HTTP %d after payment %s", status, signature)
	}
	if purchase.Error != nil {
		return "", fmt.Errorf("x402 purchase: %s", purchase.Error.Message)
	}
	if !purchase.Complete {
		return "", fmt.Errorf("x402 purchase did not complete after payment %s", signature)
	}
	if err := r.awaitPro(ctx, jwt); err != nil {
		return "", fmt.Errorf("x402 settled %s but the entitlement did not arrive: %w", signature, err)
	}
	return fmt.Sprintf(
		"x402 quoted $%.2f on solana, settled it, and granted pro_1month (%s)",
		amountUsd, purchase.Transaction,
	), nil
}

// PayReference sends one payment against a reference somebody else registered.
//
// The browser acceptance cases need this: the page under test registers its own
// intent and builds its own payment url, which is the whole point -- the client
// doing it is what is being checked. The test then has to actually pay the
// reference the PAGE produced, not one this process invented.
//
// It enforces the same ceilings as every other payment here.
func (r *Runner) PayReference(ctx context.Context, reference string, amountUsd float64) (*usdcpay.Result, error) {
	if r.payer == nil {
		return nil, errors.New(r.skipReason)
	}
	return r.pay(ctx, amountUsd, reference)
}

// ----- payment plumbing -----

// pay enforces the campaign budget, sends, and records the spend.
func (r *Runner) pay(ctx context.Context, amountUsd float64, reference string) (*usdcpay.Result, error) {
	if err := r.budget(amountUsd); err != nil {
		return nil, err
	}
	payment, err := r.payer.Send(ctx, usdcpay.Payment{
		Recipient: MerchantAddress,
		AmountUsd: amountUsd,
		Reference: reference,
	})
	if payment != nil {
		// Count anything that was broadcast, whether or not it confirmed:
		// the money left either way.
		r.spent += payment.AmountUsd
	}
	if err != nil {
		return nil, fmt.Errorf("send %.2f USDC: %w", amountUsd, err)
	}
	return payment, nil
}

// budget refuses a payment that would take the campaign past its ceiling.
func (r *Runner) budget(amountUsd float64) error {
	limit := r.Config.Payments.MaxCampaignSpendUsd
	if limit <= 0 {
		return errors.New("payments.max_campaign_spend_usd is not configured")
	}
	if r.spent+amountUsd > limit+1e-9 {
		return fmt.Errorf(
			"this payment of $%.2f would take the campaign to $%.2f, past the $%.2f ceiling",
			amountUsd, r.spent+amountUsd, limit,
		)
	}
	return nil
}

// awaitPro polls the balance until a Solana- or x402-granted subscription
// appears. It names WHICH store granted it: a network that was already Pro for
// another reason must not make this case pass.
func (r *Runner) awaitPro(ctx context.Context, jwt string) error {
	deadline := time.Now().Add(grantTimeout)
	var last string
	for {
		var balance balanceResult
		err := r.get(ctx, "/subscription/balance", jwt, &balance)
		if err == nil && balance.Error == nil {
			for _, candidate := range allSubscriptions(&balance) {
				if candidate.Store == "solana" || candidate.Store == "x402" {
					return nil
				}
			}
			if len(allSubscriptions(&balance)) != 0 {
				last = "the network is subscribed through another store"
			} else {
				last = "no subscription yet"
			}
		} else if err != nil {
			last = err.Error()
		} else {
			last = balance.Error.Message
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no solana or x402 subscription within %s (%s)", grantTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(grantPollInterval):
		}
	}
}

// awaitDataPaid polls the public status endpoint until the intent reads paid.
func (r *Runner) awaitDataPaid(ctx context.Context, reference string) error {
	deadline := time.Now().Add(grantTimeout)
	var last string
	for {
		var status dataStatusResult
		err := r.post(ctx, "/pay/data/solana-status", map[string]any{"reference": reference}, "", &status)
		switch {
		case err != nil:
			last = err.Error()
		case status.Error != nil:
			last = status.Error.Message
		case status.Status == "paid":
			return nil
		case status.Status == "expired":
			return errors.New("the intent expired before the transfer was matched")
		default:
			last = "status " + status.Status
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not applied within %s (%s)", grantTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(grantPollInterval):
		}
	}
}

func allSubscriptions(balance *balanceResult) []*subscription {
	out := balance.Subscriptions
	if balance.CurrentSubscription != nil {
		out = append(out, balance.CurrentSubscription)
	}
	return out
}

// ----- network lifecycle -----

// createNetwork makes a throwaway account and returns a cleanup that deletes
// it. Every payment case buys an entitlement and then throws the account away;
// what is under test is that the entitlement arrived at all.
func (r *Runner) createNetwork(ctx context.Context, method string) (string, func() error, error) {
	if !r.Config.Lifecycle.AllowAccountCreateDelete {
		return "", nil, errors.New("lifecycle.allow_account_create_delete must be true")
	}
	userAuth := fmt.Sprintf(
		"%s-%s@%s",
		r.Config.Signup.Email.LocalPartPrefix, suffix(), r.Config.Signup.Email.Domain,
	)
	var created networkCreateResult
	err := r.post(ctx, "/auth/network-create", map[string]any{
		"user_auth":          userAuth,
		"password":           r.Config.Signup.Password,
		"network_name":       r.networkName(method),
		"terms":              true,
		"verify_use_numeric": false,
	}, "", &created)
	if err != nil {
		return "", nil, fmt.Errorf("create acceptance network: %w", err)
	}
	if created.Error != nil {
		return "", nil, fmt.Errorf("create acceptance network: %s", created.Error.Message)
	}
	if created.VerificationRequired != nil {
		return "", nil, errors.New("test-domain signup unexpectedly required verification")
	}
	if created.Network == nil || created.Network.ByJWT == "" {
		return "", nil, errors.New("signup returned no network JWT")
	}
	jwt := created.Network.ByJWT
	return jwt, func() error {
		var result struct {
			Error *apiError `json:"error,omitempty"`
		}
		if err := r.post(context.WithoutCancel(ctx), "/auth/network-delete", struct{}{}, jwt, &result); err != nil {
			return fmt.Errorf("delete acceptance account: %w", err)
		}
		if result.Error != nil {
			return fmt.Errorf("delete acceptance account: %s", result.Error.Message)
		}
		return nil
	}, nil
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
	return hex.EncodeToString(value)
}

// newReference mints a Solana Pay reference: a base58 32-byte pubkey, which is
// what the spec requires and what the indexer can surface. A hex uuid here is
// the bug that once made every web payment unmatchable.
func newReference() (string, error) {
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		return "", fmt.Errorf("mint payment reference: %w", err)
	}
	return key.PublicKey().String(), nil
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

// redact keeps every credential out of a result detail, which is written to a
// TSV an operator pastes around. The payer key and the RPC url (which carries
// an api key in the Helius form) are the two this package adds.
func (r *Runner) redact(message string) string {
	for _, value := range []string{
		r.Config.Signup.Password,
		r.Config.Payments.Payer.PrivateKeyBase58,
		r.Config.Payments.SolanaRpcUrl,
		r.Config.Wallets.Solana.PrivateKeyBase58,
		r.Config.Wallets.Bittensor.Mnemonic,
	} {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return message
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func atomicToUsd(atomic string) float64 {
	var out float64
	for _, c := range atomic {
		if c < '0' || c > '9' {
			return 0
		}
		out = out*10 + float64(c-'0')
	}
	return out / 1e6
}

// ----- http -----

func (r *Runner) httpClient() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *Runner) url(path string) string {
	return strings.TrimRight(r.APIURL, "/") + path
}

func (r *Runner) post(ctx context.Context, path string, body any, jwt string, output any) error {
	status, err := r.postWithHeaders(ctx, path, body, jwt, nil, output)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("%s returned HTTP %d", path, status)
	}
	return nil
}

// postWithHeaders returns the status rather than treating a non-2xx as fatal,
// because the x402 flow depends on reading a 402 as a normal answer.
func (r *Runner) postWithHeaders(
	ctx context.Context, path string, body any, jwt string,
	headers map[string]string, output any,
) (int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url(path), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Client-Version", "1.0.0-pay-acceptance")
	if jwt != "" {
		request.Header.Set("Authorization", "Bearer "+jwt)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := r.httpClient().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return response.StatusCode, err
	}
	if output != nil && len(bytes.TrimSpace(data)) != 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return response.StatusCode, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return response.StatusCode, nil
}

func (r *Runner) get(ctx context.Context, path, jwt string, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(path), nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Client-Version", "1.0.0-pay-acceptance")
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
		return fmt.Errorf("%s returned HTTP %d", path, response.StatusCode)
	}
	return json.Unmarshal(data, output)
}

func (r *Runner) getRaw(ctx context.Context, path string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(path), nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("X-Client-Version", "1.0.0-pay-acceptance")
	response, err := r.httpClient().Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	return data, response.StatusCode, err
}
