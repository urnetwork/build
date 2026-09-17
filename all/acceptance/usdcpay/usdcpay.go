// Package usdcpay sends real USDC on Solana from the acceptance payer wallet.
//
// Every other Solana test in the tree stops short of the chain: the webhook
// tests hand-build a *SolanaTransaction and feed it straight to HeliusWebhook,
// which proves the grant logic but never proves that a payment a customer
// actually makes is seen. This package closes that gap -- it builds, signs and
// broadcasts an SPL transfer of USDC on Solana mainnet, then waits for the
// transaction to finalize, so an acceptance case can assert the whole path:
// quote -> transfer -> Helius -> grant.
//
// Because that means spending real money, nothing here runs unless the caller
// has already cleared testconfig.Payments -- see Guard. The package itself
// refuses a payment above the configured ceiling as a second backstop.
//
// Reference matching. The server matches a transfer to its payment intent by
// solanaReferenceCandidates(): the transaction's account keys (how Solana Pay
// carries a reference) OR the memo text (how a transfer sent by hand from an
// exchange carries it). We attach the reference BOTH ways, which is what a
// wallet following the Solana Pay spec produces and what the panel on the site
// tells a human to do, so the same transfer is matched by either path.
package usdcpay

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/memo"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
)

// UsdcMintMainnet is USDC on Solana mainnet. It is the same constant the
// server matches on (server/controller/subscription_controller.go
// solanaUsdcMint) and the same one the site and the apps build payment urls
// with. USDC exists on many chains; this is the only one URnetwork accepts.
const UsdcMintMainnet = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// UsdcDecimals is USDC's atomic scale on Solana: $1.00 is 1_000_000.
const UsdcDecimals = 6

// lamportsPerSol is used only to report a readable SOL balance.
const lamportsPerSol = 1_000_000_000

// minFeeLamports is a floor on the payer's SOL balance before we try to send.
// A transfer costs ~5000 lamports; creating a recipient token account costs
// ~2_040_000 lamports of rent. Refusing early gives a legible failure instead
// of an "insufficient funds for fee" deep inside the RPC response.
const minFeeLamports = 3_000_000

// confirmPollInterval and confirmTimeout bound the wait for finalization.
// Mainnet finalizes in ~13s typically; the timeout is generous because the
// acceptance campaign would rather wait than report a false failure.
const (
	confirmPollInterval = 2 * time.Second
	confirmTimeout      = 3 * time.Minute
)

// Payment is what a caller asks for: an amount in USD, to an owner address,
// tagged with a reference the server can match back to its intent.
type Payment struct {
	// Recipient is the OWNER address, not its token account. The server
	// matches Helius's tokenTransfer.ToUserAccount, which is the owner.
	Recipient string
	// AmountUsd is the price the SERVER quoted. Never a number the client
	// chose -- see sdk/solana_pay.go and the note in
	// solana-pay-shared-in-sdk: the client never names its own price.
	AmountUsd float64
	// Reference is the base58 32-byte pubkey from the payment intent. It is
	// attached as an account key and as the memo.
	Reference string
}

// Result reports what landed on chain.
type Result struct {
	Signature string
	Slot      uint64
	AmountUsd float64
	Reference string
	Recipient string
}

// Payer holds the funded acceptance wallet and an RPC connection.
type Payer struct {
	client   *rpc.Client
	key      solana.PrivateKey
	mint     solana.PublicKey
	maxUsd   float64
	endpoint string
}

// NewPayer opens an RPC client and loads the payer key, refusing to proceed
// unless the key derives the address the vault says it should. That check is
// the same one walletfixture makes for the signing wallet: a mismatch means
// the vault is describing a different wallet than the one we would spend from,
// and we would rather fail than move money from an unexpected account.
func NewPayer(rpcURL, privateKeyBase58, expectedAddress string, maxPaymentUsd float64) (*Payer, error) {
	if strings.TrimSpace(rpcURL) == "" {
		return nil, errors.New("solana rpc url is not configured")
	}
	if strings.TrimSpace(privateKeyBase58) == "" {
		return nil, errors.New("payer private key is not configured")
	}
	if strings.TrimSpace(expectedAddress) == "" {
		return nil, errors.New("payer address is not configured")
	}
	if maxPaymentUsd <= 0 {
		return nil, errors.New("max payment must be greater than zero")
	}

	key, err := solana.PrivateKeyFromBase58(privateKeyBase58)
	if err != nil {
		return nil, fmt.Errorf("parse payer private key: %w", err)
	}
	if len(key) != 64 {
		return nil, fmt.Errorf("payer private key must be 64 bytes, got %d", len(key))
	}
	if got := key.PublicKey().String(); got != expectedAddress {
		// Deliberately does not print either key.
		return nil, errors.New("payer private key does not derive the configured address")
	}

	mint, err := solana.PublicKeyFromBase58(UsdcMintMainnet)
	if err != nil {
		return nil, fmt.Errorf("parse usdc mint: %w", err)
	}

	return &Payer{
		client:   rpc.New(rpcURL),
		key:      key,
		mint:     mint,
		maxUsd:   maxPaymentUsd,
		endpoint: rpcURL,
	}, nil
}

// Address is the payer's wallet address -- what the user funds.
func (p *Payer) Address() string {
	return p.key.PublicKey().String()
}

// TokenAccount is the payer's USDC associated token account.
func (p *Payer) TokenAccount() (solana.PublicKey, error) {
	ata, _, err := solana.FindAssociatedTokenAddress(p.key.PublicKey(), p.mint)
	return ata, err
}

// Balances reports what the payer holds. usdc is a dollar amount; sol is whole
// SOL. A caller reports these before refusing to spend so the operator can see
// what is missing rather than guessing.
func (p *Payer) Balances(ctx context.Context) (usdc float64, sol float64, err error) {
	lamports, err := p.client.GetBalance(ctx, p.key.PublicKey(), rpc.CommitmentConfirmed)
	if err != nil {
		return 0, 0, fmt.Errorf("read sol balance: %w", err)
	}
	sol = float64(lamports.Value) / lamportsPerSol

	ata, err := p.TokenAccount()
	if err != nil {
		return 0, sol, fmt.Errorf("derive payer token account: %w", err)
	}
	// An unfunded wallet has no token account at all, which is a zero
	// balance rather than an error -- that is the state the wallet is in
	// before anyone sends it USDC.
	out, err := p.client.GetTokenAccountBalance(ctx, ata, rpc.CommitmentConfirmed)
	if err != nil {
		if isAccountNotFound(err) {
			return 0, sol, nil
		}
		return 0, sol, fmt.Errorf("read usdc balance: %w", err)
	}
	if out == nil || out.Value == nil {
		return 0, sol, nil
	}
	usdc = atomicToUsd(parseUint(out.Value.Amount))
	return usdc, sol, nil
}

// Send broadcasts the payment and waits for it to finalize.
//
// The transfer is TransferChecked rather than Transfer: it names the mint and
// the decimals, so a wrong-mint or wrong-scale transfer is rejected by the
// token program instead of silently moving the wrong asset.
func (p *Payer) Send(ctx context.Context, payment Payment) (*Result, error) {
	if payment.AmountUsd <= 0 {
		return nil, errors.New("payment amount must be greater than zero")
	}
	if payment.AmountUsd > p.maxUsd {
		return nil, fmt.Errorf(
			"payment of $%.2f exceeds the configured ceiling of $%.2f",
			payment.AmountUsd, p.maxUsd,
		)
	}
	reference, err := solana.PublicKeyFromBase58(payment.Reference)
	if err != nil {
		// The server requires a base58 32-byte pubkey here. A hex uuid is
		// the exact bug that once made web payments unmatchable.
		return nil, fmt.Errorf("payment reference must be a base58 32-byte pubkey: %w", err)
	}
	recipient, err := solana.PublicKeyFromBase58(payment.Recipient)
	if err != nil {
		return nil, fmt.Errorf("parse recipient: %w", err)
	}

	usdc, sol, err := p.Balances(ctx)
	if err != nil {
		return nil, err
	}
	if usdc+1e-9 < payment.AmountUsd {
		return nil, fmt.Errorf(
			"payer holds %.2f USDC, needs %.2f -- fund %s",
			usdc, payment.AmountUsd, p.Address(),
		)
	}
	if sol*lamportsPerSol < minFeeLamports {
		return nil, fmt.Errorf(
			"payer holds %.6f SOL, needs at least %.6f for fees -- fund %s",
			sol, float64(minFeeLamports)/lamportsPerSol, p.Address(),
		)
	}

	source, err := p.TokenAccount()
	if err != nil {
		return nil, err
	}
	destination, _, err := solana.FindAssociatedTokenAddress(recipient, p.mint)
	if err != nil {
		return nil, fmt.Errorf("derive recipient token account: %w", err)
	}

	instructions := []solana.Instruction{}

	// Create the recipient's token account only if it is genuinely absent.
	// The production merchant always has one; a scratch recipient in a
	// rehearsal may not. CreateIdempotent rather than Create: the account can
	// appear between the check and the send, and the idempotent form treats
	// that as success instead of failing the whole transfer.
	exists, err := p.accountExists(ctx, destination)
	if err != nil {
		return nil, err
	}
	if !exists {
		instructions = append(instructions,
			associatedtokenaccount.NewCreateIdempotentInstruction(
				p.key.PublicKey(), recipient, p.mint,
			).Build(),
		)
	}

	transfer, err := p.transferInstruction(source, destination, payment.AmountUsd, reference)
	if err != nil {
		return nil, err
	}
	instructions = append(instructions, transfer)

	// The memo carries the same reference, for the hand-sent path.
	instructions = append(instructions,
		memo.NewMemoInstruction([]byte(payment.Reference), p.key.PublicKey()).Build(),
	)

	blockhash, err := p.client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return nil, fmt.Errorf("get blockhash: %w", err)
	}

	tx, err := solana.NewTransaction(
		instructions,
		blockhash.Value.Blockhash,
		solana.TransactionPayer(p.key.PublicKey()),
	)
	if err != nil {
		return nil, fmt.Errorf("build transaction: %w", err)
	}
	if _, err := tx.Sign(func(candidate solana.PublicKey) *solana.PrivateKey {
		if candidate.Equals(p.key.PublicKey()) {
			return &p.key
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("sign transaction: %w", err)
	}

	signature, err := p.client.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		// Let the cluster reject a bad transfer rather than discovering it
		// only after it is irreversibly on chain.
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return nil, fmt.Errorf("send transaction: %w", err)
	}

	slot, err := p.confirm(ctx, signature)
	if err != nil {
		// The signature is returned even on a confirmation failure: the
		// transfer may still land, and an operator needs the id to look it
		// up rather than a bare timeout.
		return &Result{
			Signature: signature.String(),
			AmountUsd: payment.AmountUsd,
			Reference: payment.Reference,
			Recipient: payment.Recipient,
		}, err
	}

	return &Result{
		Signature: signature.String(),
		Slot:      slot,
		AmountUsd: payment.AmountUsd,
		Reference: payment.Reference,
		Recipient: payment.Recipient,
	}, nil
}

// transferInstruction builds TransferChecked and attaches the Solana Pay
// reference as a read-only, non-signer account key. The token program ignores
// the extra key; the indexer does not, which is exactly the point.
func (p *Payer) transferInstruction(
	source, destination solana.PublicKey,
	amountUsd float64,
	reference solana.PublicKey,
) (solana.Instruction, error) {
	built := token.NewTransferCheckedInstruction(
		usdToAtomic(amountUsd),
		UsdcDecimals,
		source,
		p.mint,
		destination,
		p.key.PublicKey(),
		nil,
	).Build()

	data, err := built.Data()
	if err != nil {
		return nil, fmt.Errorf("encode transfer: %w", err)
	}
	accounts := append(built.Accounts(), &solana.AccountMeta{
		PublicKey:  reference,
		IsSigner:   false,
		IsWritable: false,
	})
	return solana.NewInstruction(token.ProgramID, accounts, data), nil
}

func (p *Payer) accountExists(ctx context.Context, address solana.PublicKey) (bool, error) {
	out, err := p.client.GetAccountInfoWithOpts(ctx, address, &rpc.GetAccountInfoOpts{
		Commitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		if isAccountNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read account %s: %w", address, err)
	}
	return out != nil && out.Value != nil, nil
}

// confirm polls until the transaction is finalized, or the transaction fails,
// or the deadline passes. Finalized, not confirmed: the grant on the other
// side is irreversible, so the test should not call a payment good until the
// chain will not reorg it away.
func (p *Payer) confirm(ctx context.Context, signature solana.Signature) (uint64, error) {
	deadline := time.Now().Add(confirmTimeout)
	for {
		out, err := p.client.GetSignatureStatuses(ctx, true, signature)
		if err == nil && out != nil && len(out.Value) == 1 && out.Value[0] != nil {
			status := out.Value[0]
			if status.Err != nil {
				return 0, fmt.Errorf("transaction %s failed on chain: %v", signature, status.Err)
			}
			if status.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
				return status.Slot, nil
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf(
				"transaction %s did not finalize within %s", signature, confirmTimeout,
			)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(confirmPollInterval):
		}
	}
}

// usdToAtomic converts dollars to USDC atomic units. It rounds rather than
// truncates so $0.10 is 100000 and not 99999: the server checks the amount
// against its quote with a one-cent tolerance, and a truncated transfer of a
// price like $39.99 would otherwise read as an underpayment.
func usdToAtomic(usd float64) uint64 {
	return uint64(math.Round(usd * math.Pow10(UsdcDecimals)))
}

func atomicToUsd(atomic uint64) float64 {
	return float64(atomic) / math.Pow10(UsdcDecimals)
}

func parseUint(s string) uint64 {
	var out uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return out
		}
		out = out*10 + uint64(c-'0')
	}
	return out
}

// isAccountNotFound distinguishes "this account has never existed" from a real
// RPC failure. solana-go reports the former as a typed sentinel, but some
// providers phrase it in the error text instead.
func isAccountNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, rpc.ErrNotFound) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "could not find account")
}

// SignedTransfer is a signed but unbroadcast USDC transfer, serialized for
// something else to submit. x402 works this way: the agent signs, the
// facilitator settles.
type SignedTransfer struct {
	// Signature is the transaction id the transfer will have once submitted.
	Signature string
	// Base64 is the serialized signed transaction.
	Base64 string
	// AmountUsd is what it moves.
	AmountUsd float64
}

// SignTransfer builds and signs a USDC transfer without broadcasting it.
//
// The amount is given in atomic units rather than dollars because the caller
// is relaying a figure another system quoted -- x402 terms carry
// maxAmountRequired as an integer -- and converting through a float on the way
// in would be a rounding step nobody asked for.
//
// The balance checks still run: signing a transfer the payer cannot cover
// hands the facilitator something that can only fail, and the reason is much
// harder to read from there.
func (p *Payer) SignTransfer(ctx context.Context, recipient string, amountAtomic uint64) (*SignedTransfer, error) {
	if amountAtomic == 0 {
		return nil, errors.New("transfer amount must be greater than zero")
	}
	amountUsd := atomicToUsd(amountAtomic)
	if amountUsd > p.maxUsd {
		return nil, fmt.Errorf(
			"transfer of $%.2f exceeds the configured ceiling of $%.2f",
			amountUsd, p.maxUsd,
		)
	}
	to, err := solana.PublicKeyFromBase58(recipient)
	if err != nil {
		return nil, fmt.Errorf("parse recipient: %w", err)
	}

	usdc, sol, err := p.Balances(ctx)
	if err != nil {
		return nil, err
	}
	if usdc+1e-9 < amountUsd {
		return nil, fmt.Errorf(
			"payer holds %.2f USDC, needs %.2f -- fund %s", usdc, amountUsd, p.Address(),
		)
	}
	if sol*lamportsPerSol < minFeeLamports {
		return nil, fmt.Errorf(
			"payer holds %.6f SOL, needs at least %.6f for fees -- fund %s",
			sol, float64(minFeeLamports)/lamportsPerSol, p.Address(),
		)
	}

	source, err := p.TokenAccount()
	if err != nil {
		return nil, err
	}
	destination, _, err := solana.FindAssociatedTokenAddress(to, p.mint)
	if err != nil {
		return nil, fmt.Errorf("derive recipient token account: %w", err)
	}

	transfer := token.NewTransferCheckedInstruction(
		amountAtomic, UsdcDecimals, source, p.mint, destination, p.key.PublicKey(), nil,
	).Build()

	blockhash, err := p.client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return nil, fmt.Errorf("get blockhash: %w", err)
	}
	tx, err := solana.NewTransaction(
		[]solana.Instruction{transfer},
		blockhash.Value.Blockhash,
		solana.TransactionPayer(p.key.PublicKey()),
	)
	if err != nil {
		return nil, fmt.Errorf("build transaction: %w", err)
	}
	if _, err := tx.Sign(func(candidate solana.PublicKey) *solana.PrivateKey {
		if candidate.Equals(p.key.PublicKey()) {
			return &p.key
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("sign transaction: %w", err)
	}

	encoded, err := tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("serialize transaction: %w", err)
	}
	signatures := tx.Signatures
	if len(signatures) == 0 {
		return nil, errors.New("signed transaction carries no signature")
	}
	return &SignedTransfer{
		Signature: signatures[0].String(),
		Base64:    base64.StdEncoding.EncodeToString(encoded),
		AmountUsd: amountUsd,
	}, nil
}
