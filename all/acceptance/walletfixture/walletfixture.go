// Package walletfixture validates and signs with the dedicated deterministic
// wallets stored in vault/main/tests.yml. It never logs private key material.
package walletfixture

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"
)

const (
	SolanaBlockchain    = "SOL"
	BittensorBlockchain = "TAO"
)

// Signer is the common representation used by the acceptance lifecycle.
type Signer interface {
	Blockchain() string
	Address() string
	Sign(message string) (string, error)
}

type solanaSigner struct {
	privateKey solana.PrivateKey
	address    string
}

// NewSolana validates that the configured private key owns expectedAddress.
func NewSolana(expectedAddress, privateKeyBase58 string) (Signer, error) {
	privateKey, err := solana.PrivateKeyFromBase58(strings.TrimSpace(privateKeyBase58))
	if err != nil {
		return nil, errors.New("invalid Solana private key")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Solana private key length")
	}
	address := privateKey.PublicKey().String()
	if address != strings.TrimSpace(expectedAddress) {
		return nil, errors.New("configured Solana address does not match its private key")
	}
	return &solanaSigner{privateKey: privateKey, address: address}, nil
}

func (s *solanaSigner) Blockchain() string { return SolanaBlockchain }
func (s *solanaSigner) Address() string    { return s.address }

func (s *solanaSigner) Sign(message string) (string, error) {
	signature, err := s.privateKey.Sign([]byte(message))
	if err != nil {
		return "", fmt.Errorf("sign Solana challenge: %w", err)
	}
	return base64.StdEncoding.EncodeToString(signature[:]), nil
}

type bittensorSigner struct {
	keyPair subkey.KeyPair
	address string
}

// NewBittensor derives the sr25519 key from the configured secret URI or
// mnemonic and validates its SS58 address before it may contact main.
func NewBittensor(expectedAddress, mnemonic string, ss58Prefix int) (Signer, error) {
	if ss58Prefix < 0 || ss58Prefix > 16383 {
		return nil, errors.New("invalid Bittensor SS58 prefix")
	}
	keyPair, err := subkey.DeriveKeyPair(sr25519.Scheme{}, strings.TrimSpace(mnemonic))
	if err != nil {
		return nil, errors.New("invalid Bittensor mnemonic")
	}
	address := keyPair.SS58Address(uint16(ss58Prefix))
	if address != strings.TrimSpace(expectedAddress) {
		return nil, errors.New("configured Bittensor address does not match its mnemonic and SS58 prefix")
	}
	return &bittensorSigner{keyPair: keyPair, address: address}, nil
}

func (s *bittensorSigner) Blockchain() string { return BittensorBlockchain }
func (s *bittensorSigner) Address() string    { return s.address }

func (s *bittensorSigner) Sign(message string) (string, error) {
	// Browser-extension Substrate signRaw implementations sign the wrapped
	// payload. This matches server/model/auth_bittensor.go.
	signature, err := s.keyPair.Sign([]byte("<Bytes>" + message + "</Bytes>"))
	if err != nil {
		return "", fmt.Errorf("sign Bittensor challenge: %w", err)
	}
	return hex.EncodeToString(signature), nil
}
