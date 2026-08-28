// Command wallet-sign is a bounded stdin/stdout bridge used only by local
// acceptance harnesses. It validates the configured address before signing.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/urnetwork/build/all/acceptance/testconfig"
	"github.com/urnetwork/build/all/acceptance/walletfixture"
)

const maxMessageBytes = 16 * 1024

func main() {
	configPath := flag.String("config", "", "resolved private tests.json")
	blockchain := flag.String("blockchain", "", "solana or bittensor")
	flag.Parse()
	if *configPath == "" || (*blockchain != "solana" && *blockchain != "bittensor") {
		fmt.Fprintln(os.Stderr, "wallet-sign: --config and --blockchain=solana|bittensor are required")
		os.Exit(2)
	}
	config, err := testconfig.LoadJSON(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wallet-sign: %v\n", err)
		os.Exit(1)
	}
	message, err := io.ReadAll(io.LimitReader(bufio.NewReader(os.Stdin), maxMessageBytes+1))
	if err != nil || len(message) == 0 || len(message) > maxMessageBytes {
		fmt.Fprintln(os.Stderr, "wallet-sign: challenge must be 1..16384 bytes")
		os.Exit(1)
	}
	var signer walletfixture.Signer
	if *blockchain == "solana" {
		signer, err = walletfixture.NewSolana(config.Wallets.Solana.Address, config.Wallets.Solana.PrivateKeyBase58)
	} else {
		signer, err = walletfixture.NewBittensor(
			config.Wallets.Bittensor.Address,
			config.Wallets.Bittensor.Mnemonic,
			config.Wallets.Bittensor.SS58Prefix,
		)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "wallet-sign: %v\n", err)
		os.Exit(1)
	}
	signature, err := signer.Sign(string(message))
	if err != nil {
		fmt.Fprintf(os.Stderr, "wallet-sign: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"address": signer.Address(), "signature": signature,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "wallet-sign: encode result: %v\n", err)
		os.Exit(1)
	}
}
