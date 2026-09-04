// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeLinuxSdkMake = `#!/bin/sh
set -eu
for artifact in \
  "$FAKE_BUILD_HOME/sdk/cgo/build/linux/amd64/libURnetworkSdk.so" \
  "$FAKE_BUILD_HOME/sdk/cgo/build/linux/arm64/libURnetworkSdk.so" \
  "$FAKE_BUILD_HOME/sdk/cgo/build/URnetworkSdkLinux.zip"
do
  if [ -e "$artifact" ]; then
    echo "stale SDK artifact reached make: $artifact" >&2
    exit 91
  fi
done
printf 'make invoked\n' >"$FAKE_MAKE_LOG"
if [ "$FAKE_MAKE_MODE" = fail ]; then
  exit 37
fi
mkdir -p "$FAKE_BUILD_HOME/sdk/cgo/build/linux/arm64"
printf 'new arm64\n' >"$FAKE_BUILD_HOME/sdk/cgo/build/linux/arm64/libURnetworkSdk.so"
printf 'partial archive\n' >"$FAKE_BUILD_HOME/sdk/cgo/build/URnetworkSdkLinux.zip"
`

func buildLinuxScript(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Linux build contract test")
	}
	return filepath.Join(filepath.Dir(currentFile), "build-linux.sh")
}

func linuxSdkBuildFixture(t *testing.T) (buildHome, binDir, makeLog string) {
	t.Helper()
	buildHome = t.TempDir()
	for _, path := range []string{
		"linux/packaging/make-deb.sh",
		"linux/packaging/make-install-tarball.sh",
		"linux/packaging/make-appimage.sh",
		"sdk/cgo/go.sum",
	} {
		path = filepath.Join(buildHome, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		"sdk/cgo/build/linux/amd64/libURnetworkSdk.so",
		"sdk/cgo/build/linux/arm64/libURnetworkSdk.so",
		"sdk/cgo/build/URnetworkSdkLinux.zip",
	} {
		path = filepath.Join(buildHome, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stale artifact\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binDir = filepath.Join(buildHome, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "make"), []byte(fakeLinuxSdkMake), 0o700); err != nil {
		t.Fatal(err)
	}
	makeLog = filepath.Join(buildHome, "make.log")
	return buildHome, binDir, makeLog
}

func environmentWith(overrides ...string) []string {
	replaced := map[string]bool{}
	for _, override := range overrides {
		if key, _, ok := strings.Cut(override, "="); ok {
			replaced[key] = true
		}
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		key, _, ok := strings.Cut(value, "=")
		if !ok || !replaced[key] {
			env = append(env, value)
		}
	}
	return append(env, overrides...)
}

func runLinuxSdkBuildFixture(
	t *testing.T,
	mode string,
) (buildHome string, output []byte, err error) {
	t.Helper()
	buildHome, binDir, makeLog := linuxSdkBuildFixture(t)
	cmd := exec.Command(buildLinuxScript(t))
	cmd.Env = environmentWith(
		"BUILD_HOME="+buildHome,
		"BUILD_OUT="+filepath.Join(buildHome, "out"),
		"EXTERNAL_WARP_VERSION=0.0.0-0",
		"WARP_VERSION=0.0.0+0",
		"FAKE_BUILD_HOME="+buildHome,
		"FAKE_MAKE_LOG="+makeLog,
		"FAKE_MAKE_MODE="+mode,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	output, err = cmd.CombinedOutput()
	if data, readErr := os.ReadFile(makeLog); readErr != nil || string(data) != "make invoked\n" {
		t.Fatalf("make invocation proof = %q, %v; output:\n%s", data, readErr, output)
	}
	return buildHome, output, err
}

// build-linux.sh is a second trust boundary around the SDK Makefile. The
// staging rsync intentionally preserves build/, so every owned output must be
// removed before make and make's own status must stop the pipeline unchanged.
func TestBuildLinuxClearsSdkOutputsAndPropagatesMakeFailure(t *testing.T) {
	_, output, err := runLinuxSdkBuildFixture(t, "fail")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 37 {
		t.Fatalf("exit = %v, want 37; output:\n%s", err, output)
	}
}

// Reproduces the observed masked amd64 failure: make reports success and
// leaves only arm64 plus a partial zip. A stale amd64 library must not survive
// long enough to satisfy the packaging gate.
func TestBuildLinuxRejectsPartialSdkAfterMaskedArchitectureFailure(t *testing.T) {
	buildHome, output, err := runLinuxSdkBuildFixture(t, "partial")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("exit = %v, want 1; output:\n%s", err, output)
	}
	missing := filepath.Join(buildHome, "sdk/cgo/build/linux/amd64/libURnetworkSdk.so")
	if !strings.Contains(string(output), "ERROR: "+missing+" not built.") {
		t.Fatalf("partial build did not name missing amd64 output:\n%s", output)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale amd64 output survived partial build: %v", statErr)
	}
}
