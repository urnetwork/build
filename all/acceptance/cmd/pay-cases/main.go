// Command pay-cases runs the real-money USDC acceptance cases and appends
// their rows to the campaign result matrix.
//
// It is the payment twin of auth-cases, with one difference that matters: a
// SKIP is a normal, expected outcome here. Most machines do not have
// payments.allow_real_usdc_spend set, and x402 is switched off on every
// deployment today, so the usual result of this command is three SKIP rows and
// exit 0. Only a case that was asked to run and then went wrong is a failure.
//
// Usage:
//
//	pay-cases --config tests.json --result results.tsv --platform server/proxy
//	pay-cases --config tests.json --balances       # report the payer, spend nothing
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/build/all/acceptance/paycases"
	"github.com/urnetwork/build/all/acceptance/testconfig"
)

func main() {
	configPath := flag.String("config", "", "resolved private tests.json")
	resultPath := flag.String("result", "", "private TSV result matrix")
	platform := flag.String("platform", "server/proxy", "result matrix platform name")
	apiURL := flag.String("api", "https://api.bringyour.com", "API base URL")
	caseList := flag.String("cases", strings.Join(paycases.AllCases, ","), "comma-separated cases")
	// Generous by default: a case waits for a mainnet transfer to finalize and
	// then for the grant to appear. Too short a deadline reports a failure for
	// a payment that was fine.
	timeout := flag.Duration("timeout", 30*time.Minute, "whole payment phase deadline")
	balances := flag.Bool("balances", false, "report the payer address and balances, then exit")
	// Single-payment mode, for the browser acceptance cases: the PAGE builds
	// the payment and registers the intent, and this pays the reference it
	// produced. Prints the transaction signature on success.
	pay := flag.Bool("pay", false, "pay one reference, then exit")
	reference := flag.String("reference", "", "with --pay: the base58 payment reference to pay")
	amountUsd := flag.Float64("amount", 0, "with --pay: the amount in USD the server quoted")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "pay-cases: --config is required")
		os.Exit(2)
	}
	config, err := testconfig.LoadJSON(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pay-cases: %v\n", err)
		os.Exit(1)
	}

	runner, err := paycases.NewRunner(*apiURL, config, nil)
	if err != nil {
		// An enabled-but-broken payments section. Somebody asked for these
		// cases; refusing loudly beats spending from an unexpected wallet.
		fmt.Fprintf(os.Stderr, "pay-cases: %v\n", err)
		os.Exit(1)
	}

	if *balances {
		reportBalances(runner)
		return
	}

	if *pay {
		payOnce(runner, *reference, *amountUsd, *timeout)
		return
	}

	if *resultPath == "" {
		fmt.Fprintln(os.Stderr, "pay-cases: --result is required")
		os.Exit(2)
	}
	if config.Payments.Enabled() && !config.Lifecycle.AllowAccountCreateDelete {
		// Every payment case creates a throwaway network to pay into.
		fmt.Fprintln(os.Stderr, "pay-cases: destructive lifecycle is not authorized")
		os.Exit(1)
	}

	cases := strings.Split(*caseList, ",")
	for i := range cases {
		cases[i] = strings.TrimSpace(cases[i])
	}

	if config.Payments.Enabled() {
		// Say out loud what this run may spend, before it spends it.
		fmt.Fprintf(os.Stderr,
			"pay-cases: REAL USDC on Solana mainnet from %s (ceilings: $%.2f per payment, $%.2f per campaign)\n",
			runner.PayerAddress(), config.Payments.MaxSpendUsd, config.Payments.MaxCampaignSpendUsd,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	results := runner.Run(ctx, cases)

	if err := os.MkdirAll(filepath.Dir(*resultPath), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "pay-cases: create result directory: %v\n", err)
		os.Exit(1)
	}
	file, err := os.OpenFile(*resultPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pay-cases: open result: %v\n", err)
		os.Exit(1)
	}
	failed := false
	for _, result := range results {
		fmt.Fprintf(file, "%s\t%s\t%s\t%s\n", *platform, result.Case, result.Status, result.Detail)
		fmt.Fprintf(os.Stderr, "pay-cases: %s %s %s\n", result.Case, result.Status, result.Detail)
		// A SKIP is the normal outcome on a machine that does not spend, so
		// only a FAIL is a failure.
		failed = failed || result.Status == "FAIL"
	}
	if err := file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "pay-cases: close result: %v\n", err)
		os.Exit(1)
	}
	if failed {
		os.Exit(1)
	}
}

// reportBalances is the operator's pre-flight: what to fund, and with how
// much. It spends nothing and never fails the campaign.
func reportBalances(runner *paycases.Runner) {
	address := runner.PayerAddress()
	if address == "" {
		fmt.Println("payments are not enabled in the tests vault; nothing to fund")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	usdc, sol, err := runner.Preflight(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pay-cases: read balances for %s: %v\n", address, err)
		os.Exit(1)
	}
	fmt.Printf("payer   %s\n", address)
	fmt.Printf("usdc    %.2f\n", usdc)
	fmt.Printf("sol     %.6f\n", sol)
}

// payOnce pays a reference another process registered and prints the signature.
// A disabled payments section is an error here rather than a skip: the caller
// asked for a payment, and answering with silence would let a browser case sit
// waiting for money that is never coming.
func payOnce(runner *paycases.Runner, reference string, amountUsd float64, timeout time.Duration) {
	if reference == "" || amountUsd <= 0 {
		fmt.Fprintln(os.Stderr, "pay-cases: --pay needs --reference and a positive --amount")
		os.Exit(2)
	}
	if runner.PayerAddress() == "" {
		fmt.Fprintln(os.Stderr, "pay-cases: payments are not enabled in the tests vault")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := runner.PayReference(ctx, reference, amountUsd)
	if err != nil {
		// A result with an error means the transfer was broadcast but not
		// confirmed. Print the signature anyway so the money is traceable.
		if result != nil && result.Signature != "" {
			fmt.Fprintf(os.Stderr, "pay-cases: broadcast %s but: %v\n", result.Signature, err)
		} else {
			fmt.Fprintf(os.Stderr, "pay-cases: %v\n", err)
		}
		os.Exit(1)
	}
	fmt.Println(result.Signature)
}
