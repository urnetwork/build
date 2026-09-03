package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// A standalone module that replaces the local SDK still owns its own go.sum.
// If any SDK or Connect dependency changes without the acceptance module being
// tidied, the Linux and Windows runners fail before the control agent can
// compile. Ask Go for the complete canonical diff instead of pinning whichever
// individual dependency happened to expose the last instance of this bug.
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

func acceptanceModuleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate acceptance module test source")
	}
	return filepath.Dir(filename)
}
