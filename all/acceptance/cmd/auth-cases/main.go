package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/build/all/acceptance/authcases"
	"github.com/urnetwork/build/all/acceptance/testconfig"
)

func main() {
	configPath := flag.String("config", "", "resolved private tests.json")
	resultPath := flag.String("result", "", "private TSV result matrix")
	platform := flag.String("platform", "auth-api", "result matrix platform name")
	apiURL := flag.String("api", "https://api.bringyour.com", "API base URL")
	caseList := flag.String("cases", "email,phone,solana,bittensor", "comma-separated cases")
	timeout := flag.Duration("timeout", 5*time.Minute, "whole lifecycle deadline")
	flag.Parse()
	if *configPath == "" || *resultPath == "" {
		fmt.Fprintln(os.Stderr, "auth-cases: --config and --result are required")
		os.Exit(2)
	}
	config, err := testconfig.LoadJSON(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth-cases: %v\n", err)
		os.Exit(1)
	}
	if !config.Lifecycle.AllowAccountCreateDelete {
		fmt.Fprintln(os.Stderr, "auth-cases: destructive lifecycle is not authorized")
		os.Exit(1)
	}
	cases := strings.Split(*caseList, ",")
	for i := range cases {
		cases[i] = strings.TrimSpace(cases[i])
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	results := (&authcases.Runner{APIURL: *apiURL, Config: config}).Run(ctx, cases)

	if err := os.MkdirAll(filepath.Dir(*resultPath), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "auth-cases: create result directory: %v\n", err)
		os.Exit(1)
	}
	file, err := os.OpenFile(*resultPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth-cases: open result: %v\n", err)
		os.Exit(1)
	}
	failed := false
	for _, result := range results {
		fmt.Fprintf(file, "%s\t%s\t%s\t%s\n", *platform, result.Case, result.Status, result.Detail)
		fmt.Fprintf(os.Stderr, "auth-cases: %s %s\n", result.Case, result.Status)
		failed = failed || result.Status != "PASS"
	}
	if err := file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "auth-cases: close result: %v\n", err)
		os.Exit(1)
	}
	if failed {
		os.Exit(1)
	}
}
