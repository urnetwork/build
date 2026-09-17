package paycases

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// x402Accept mirrors controller.X402Accept: one set of payment terms, for one
// network. The server quotes one per configured network and the agent picks.
type x402Accept struct {
	Scheme            string `json:"scheme"`
	Network           string `json:"network"`
	MaxAmountRequired string `json:"maxAmountRequired"`
	Resource          string `json:"resource"`
	Description       string `json:"description"`
	MimeType          string `json:"mimeType"`
	PayTo             string `json:"payTo"`
	Asset             string `json:"asset"`
	MaxTimeoutSeconds int    `json:"maxTimeoutSeconds"`
}

type x402PaymentRequired struct {
	X402Version int          `json:"x402Version"`
	Error       string       `json:"error"`
	Accepts     []x402Accept `json:"accepts"`
}

// x402PaymentPayload is what goes in the X-PAYMENT header, base64 of this
// JSON. The `exact` scheme on Solana carries a signed transaction the
// facilitator submits.
type x402PaymentPayload struct {
	X402Version int              `json:"x402Version"`
	Scheme      string           `json:"scheme"`
	Network     string           `json:"network"`
	Payload     x402ExactPayload `json:"payload"`
}

type x402ExactPayload struct {
	Transaction string `json:"transaction"`
}

// x402Quote asks for a resource with no payment and reads back the terms.
//
// It checks the shape hard, because these terms are what a payment gets bound
// to: a wrong payTo sends an agent's money to the wrong account, and a wrong
// amount either overcharges or quotes something the settle step will refuse.
// The server rebuilds terms from its own config on the settle side, so the
// only place a bad quote can be caught is here.
func (r *Runner) x402Quote(ctx context.Context, jwt, skuId string) (*x402Accept, error) {
	var quoted x402PaymentRequired
	status, err := r.postWithHeaders(ctx, "/x402/purchase", map[string]any{
		"sku_id": skuId,
	}, jwt, nil, &quoted)
	if err != nil {
		return nil, fmt.Errorf("request x402 terms: %w", err)
	}
	if status != http.StatusPaymentRequired {
		return nil, fmt.Errorf(
			"an unpaid x402 purchase should answer HTTP 402, got HTTP %d", status,
		)
	}
	if quoted.X402Version != 1 {
		return nil, fmt.Errorf("unexpected x402 version %d", quoted.X402Version)
	}
	if len(quoted.Accepts) == 0 {
		return nil, errors.New("x402 answered 402 with no payment terms")
	}

	var chosen *x402Accept
	for i := range quoted.Accepts {
		accept := &quoted.Accepts[i]
		if err := validateAccept(accept); err != nil {
			return nil, err
		}
		// The acceptance payer holds USDC on Solana. Selecting Solana here is
		// a fact about this wallet, not about x402, which quotes every
		// configured chain on purpose.
		if accept.Network == "solana" {
			chosen = accept
		}
	}
	if chosen == nil {
		return nil, skipf("x402 quoted no solana terms for %s", skuId)
	}
	return chosen, nil
}

// validateAccept rejects terms that could not be paid correctly.
func validateAccept(accept *x402Accept) error {
	if accept.Scheme != "exact" {
		return fmt.Errorf("unsupported x402 scheme %q", accept.Scheme)
	}
	if !strings.EqualFold(accept.Asset, "usdc") {
		return fmt.Errorf("x402 quoted asset %q, expected usdc", accept.Asset)
	}
	if accept.PayTo == "" {
		return errors.New("x402 terms carry no payTo address")
	}
	amount, err := strconv.ParseUint(accept.MaxAmountRequired, 10, 64)
	if err != nil {
		return fmt.Errorf(
			"x402 maxAmountRequired %q is not an integer of atomic units", accept.MaxAmountRequired,
		)
	}
	if amount == 0 {
		// The server itself refuses to quote a free sku; a zero here means
		// that guard stopped working.
		return errors.New("x402 quoted an amount of zero")
	}
	return nil
}

// x402Payment signs a transfer of exactly the quoted amount to the quoted
// payTo and wraps it as an X-PAYMENT header value.
//
// NOTE ON VERIFICATION. The settle half of this flow has never run anywhere:
// vault x402.yml ships enabled:false with a blank facilitator url and api key,
// so every x402 route 404s and the case above skips before reaching here. The
// header is built to the x402 `exact` shape, but which party the facilitator
// expects to pay the transaction fee, and whether it wants a fully or
// partially signed transaction, cannot be confirmed against a facilitator that
// does not exist yet. Treat this as implemented-but-unproven until x402 is
// configured, and expect to adjust it against the first real 402.
func (r *Runner) x402Payment(ctx context.Context, terms *x402Accept) (header string, signature string, err error) {
	if r.payer == nil {
		return "", "", errors.New(r.skipReason)
	}
	amount, err := strconv.ParseUint(terms.MaxAmountRequired, 10, 64)
	if err != nil {
		return "", "", fmt.Errorf("parse quoted amount: %w", err)
	}
	signed, err := r.payer.SignTransfer(ctx, terms.PayTo, amount)
	if err != nil {
		return "", "", fmt.Errorf("sign x402 transfer: %w", err)
	}
	payload, err := json.Marshal(x402PaymentPayload{
		X402Version: 1,
		Scheme:      terms.Scheme,
		Network:     terms.Network,
		Payload:     x402ExactPayload{Transaction: signed.Base64},
	})
	if err != nil {
		return "", "", fmt.Errorf("encode x402 payment: %w", err)
	}
	return base64.StdEncoding.EncodeToString(payload), signed.Signature, nil
}
