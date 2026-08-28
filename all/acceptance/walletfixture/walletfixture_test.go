package walletfixture

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"
)

const testMnemonic = "bottom drive obey lake curtain smoke basket hold race lonely fit walk"

func TestSolanaSignerValidatesAddressAndSigns(t *testing.T) {
	privateKey, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSolana(privateKey.PublicKey().String(), privateKey.String())
	if err != nil {
		t.Fatal(err)
	}
	signatureText, err := signer.Sign("challenge")
	if err != nil {
		t.Fatal(err)
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signatureText)
	if err != nil {
		t.Fatal(err)
	}
	var signature solana.Signature
	copy(signature[:], signatureBytes)
	if !signature.Verify(privateKey.PublicKey(), []byte("challenge")) {
		t.Fatal("signature did not verify")
	}
	if _, err := NewSolana(solana.SystemProgramID.String(), privateKey.String()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched address error = %v", err)
	}
}

func TestBittensorSignerValidatesAddressAndSignsWrappedPayload(t *testing.T) {
	keyPair, err := subkey.DeriveKeyPair(sr25519.Scheme{}, testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	address := keyPair.SS58Address(42)
	signer, err := NewBittensor(address, testMnemonic, 42)
	if err != nil {
		t.Fatal(err)
	}
	signatureText, err := signer.Sign("challenge")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := hex.DecodeString(signatureText)
	if err != nil {
		t.Fatal(err)
	}
	if !keyPair.Verify([]byte("<Bytes>challenge</Bytes>"), signature) {
		t.Fatal("signature did not verify")
	}
	if _, err := NewBittensor(address, testMnemonic, 0); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched prefix error = %v", err)
	}
}
