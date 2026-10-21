// SPDX-License-Identifier: MPL-2.0

package windowsbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShippingGoSelectsPatchedProvisionedCompiler(t *testing.T) {
	for _, test := range []struct {
		minimum string
		wantOK  bool
	}{
		{minimum: "1.26.5", wantOK: true},
		{minimum: "1.26.10", wantOK: true},
		{minimum: "1.26", wantOK: true},
		{minimum: "1.26.11", wantOK: false},
		{minimum: "1.27.0", wantOK: false},
		{minimum: "invalid", wantOK: false},
	} {
		t.Run(test.minimum, func(t *testing.T) {
			module := filepath.Join(t.TempDir(), "go.mod")
			if err := os.WriteFile(module, []byte("module example.invalid/sdk\n\ngo "+test.minimum+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", `source "$1"; win_sdk_go_version "$2"`,
				"shipping-go-test", filepath.Join(windowsBuildRoot(t), "lib.sh"), module)
			output, err := command.CombinedOutput()
			if (err == nil) != test.wantOK {
				t.Fatalf("minimum %q: success=%t, want %t: %s", test.minimum, err == nil, test.wantOK, output)
			}
			if test.wantOK && string(output) != "1.26.10\n" {
				t.Fatalf("selected compiler = %q, want patched provisioned 1.26.10", output)
			}
		})
	}
}

func TestShippingGoRejectsVulnerableGuestBeforeBuild(t *testing.T) {
	module := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(module, []byte("module example.invalid/sdk\n\ngo 1.26.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
set -eu
source "$1"
WIN_HERE=/synthetic/windows
expected="$(win_sdk_go_version "$2")"
win_ssh() { printf '%s\n' "$FAKE_GUEST_GO_VERSION"; }
win_assert_guest_go_version "$expected"
`
	for _, test := range []struct {
		version string
		wantOK  bool
	}{
		{version: "go version go1.26.5 windows/arm64", wantOK: false},
		{version: "go version go1.26.10 windows/arm64", wantOK: true},
		{version: "go version go1.26.10 windows/amd64", wantOK: false},
		{version: "go version go1.27.3 windows/arm64", wantOK: false},
	} {
		t.Run(strings.ReplaceAll(test.version, "/", "_"), func(t *testing.T) {
			command := exec.Command("bash", "-c", script, "shipping-go-guest-test",
				filepath.Join(windowsBuildRoot(t), "lib.sh"), module)
			command.Env = append(os.Environ(), "FAKE_GUEST_GO_VERSION="+test.version)
			output, err := command.CombinedOutput()
			if (err == nil) != test.wantOK {
				t.Fatalf("guest %q: success=%t, want %t: %s", test.version, err == nil, test.wantOK, output)
			}
		})
	}
}
