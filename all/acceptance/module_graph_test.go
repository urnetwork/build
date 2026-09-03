package main

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A standalone module that replaces the local SDK still owns its own go.sum.
// If an SDK dependency changes without the acceptance module being tidied, the
// Linux and Windows acceptance runners fail before the control agent can
// compile. Keep the signing dependency that exposed that drift explicit here.
func TestAcceptanceModuleTracksSdkSigningDependency(t *testing.T) {
	const module = "github.com/decred/dcrd/dcrec/secp256k1/v4"
	root := acceptanceModuleRoot(t)
	want := requiredModuleVersion(t, filepath.Join(root, "..", "..", "..", "sdk", "go.mod"), module)
	got := requiredModuleVersion(t, filepath.Join(root, "go.mod"), module)
	if got != want {
		t.Fatalf("%s version = %q, want SDK version %q; run go mod tidy in build/all/acceptance", module, got, want)
	}
}

func acceptanceModuleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate acceptance module test source")
	}
	return filepath.Dir(filename)
}

func requiredModuleVersion(t *testing.T, filename string, module string) string {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatalf("open %s: %v", filename, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == module {
			return fields[1]
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	t.Fatalf("%s does not require %s", filename, module)
	return ""
}
