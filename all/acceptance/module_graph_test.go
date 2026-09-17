package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// A standalone module that replaces the local SDK still owns its own go.sum.
// If any SDK or Connect dependency changes without the acceptance module being
// tidied, the Linux, Windows, and Apple runners fail before the control agent
// can compile. Ask Go for the complete canonical diff instead of pinning
// whichever individual dependency happened to expose the last instance of
// this bug.
//
// This test is intentionally runnable as `go test module_graph_test.go`: that
// stdlib-only command still reaches this diagnostic when the surrounding
// package cannot be loaded because its go.sum is the thing that drifted.
func TestAcceptanceModuleMetadataIsTidy(t *testing.T) {
	root := acceptanceModuleRoot(t)
	command := exec.Command("go", "mod", "tidy", "-diff")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("acceptance module metadata is stale; run go mod tidy in %s: %v\n%s", root, err, output)
	}
}

// TestAcceptanceLinuxAgentResolvesWithReadonlyModuleGraph verifies the Linux
// acceptance agent's exact target-specific dependency graph.
func TestAcceptanceLinuxAgentResolvesWithReadonlyModuleGraph(t *testing.T) {
	testAcceptanceAgentResolvesWithReadonlyModuleGraph(t, "linux")
}

// TestAcceptanceWindowsAgentResolvesWithReadonlyModuleGraph verifies the
// Windows acceptance agent's exact target-specific dependency graph.
func TestAcceptanceWindowsAgentResolvesWithReadonlyModuleGraph(t *testing.T) {
	testAcceptanceAgentResolvesWithReadonlyModuleGraph(t, "windows")
}

// TestAcceptanceAppleAgentResolvesWithReadonlyModuleGraph verifies the Apple
// acceptance peer provider's exact target-specific dependency graph.
func TestAcceptanceAppleAgentResolvesWithReadonlyModuleGraph(t *testing.T) {
	testAcceptanceAgentResolvesWithReadonlyModuleGraph(t, "darwin")
}

// testAcceptanceAgentResolvesWithReadonlyModuleGraph checks that a platform
// can load the control agent without rewriting the standalone module metadata.
func testAcceptanceAgentResolvesWithReadonlyModuleGraph(t *testing.T, goos string) {
	t.Helper()
	root := acceptanceModuleRoot(t)
	command := exec.Command("go", "list", "-mod=readonly", "-deps", ".")
	command.Dir = root
	command.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+goos,
		"GOARCH=arm64",
	)
	var stderr bytes.Buffer
	command.Stdout = io.Discard
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("acceptance %s/arm64 agent dependency graph does not resolve with -mod=readonly: %v\n%s", goos, err, stderr.String())
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
