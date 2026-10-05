// SPDX-License-Identifier: MPL-2.0

// The release publishes proxy-socks MIPS binaries only when they are soft
// float: run.sh's proxy socks region, run with real MIPS binaries.
package allbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The release's proxy socks region runs with a synthetic make that places real
// MIPS Go binaries, so the guard reads the float mode the toolchain recorded.
// mips64 and mips64le read GOMIPS64 and ignore GOMIPS, which is how the socks
// Makefile shipped hardfloat binaries.
const proxySocksSoftfloatHarness = `
BUILD_HOME="$1"
GO_MOD_SUFFIX=/v2026
builder_message() { print -r -- "message:$*" >> "$BUILD_HOME/events"; }
source "$2"
eval "$3"
eval "$4"
print -r -- release-continued >> "$BUILD_HOME/events"
`

const proxySocksFakeMake = `#!/bin/sh
set -eu
for arch in mips mipsle mips64 mips64le; do
  [ -e "$PROXY_SOCKS_BINARIES/$arch" ] || continue
  mkdir -p "build/linux/$arch"
  cp "$PROXY_SOCKS_BINARIES/$arch" "build/linux/$arch/socks"
done
`

// Builds the module at moduleDir for linux into out, with env on top of a
// float-neutral MIPS environment.
func buildMipsBinary(t *testing.T, moduleDir string, out string, env ...string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = moduleDir
	cmd.Env = environmentWith(append([]string{"CGO_ENABLED=0", "GOOS=linux", "GOWORK=off", "GOFLAGS=", "GOMIPS=", "GOMIPS64="}, env...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %v: %v\n%s", env, err, output)
	}
}

// Each MIPS proxy-socks binary must record soft float, or the release stops
// before the upload with a builder message that names the architecture.
func TestProxySocksReleaseRequiresMipsSoftFloat(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh required")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go required")
	}

	work := t.TempDir()
	moduleDir := filepath.Join(work, "module")
	if err := os.MkdirAll(moduleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module mipsfloattest\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	soft := filepath.Join(work, "soft")
	buildMipsBinary(t, moduleDir, filepath.Join(soft, "mips"), "GOARCH=mips", "GOMIPS=softfloat")
	buildMipsBinary(t, moduleDir, filepath.Join(soft, "mipsle"), "GOARCH=mipsle", "GOMIPS=softfloat")
	buildMipsBinary(t, moduleDir, filepath.Join(soft, "mips64"), "GOARCH=mips64", "GOMIPS64=softfloat")
	buildMipsBinary(t, moduleDir, filepath.Join(soft, "mips64le"), "GOARCH=mips64le", "GOMIPS64=softfloat")
	// The released Makefile's mistake: GOMIPS on a 64-bit target.
	hard := filepath.Join(work, "hard")
	buildMipsBinary(t, moduleDir, filepath.Join(hard, "mips64"), "GOARCH=mips64", "GOMIPS=softfloat")
	buildMipsBinary(t, moduleDir, filepath.Join(hard, "mips64le"), "GOARCH=mips64le", "GOMIPS=softfloat")

	region := componentRegion(t, "(cd $BUILD_HOME/proxy${GO_MOD_SUFFIX}/socks && make)", "\ngithub_release_upload \"urnetwork-proxy-socks-")
	errorTrap := componentFunctions(t, "error_trap")
	mipsEnv := filepath.Join(rolloutRoot(t), "go-mips-env.sh")

	for _, test := range []struct {
		name     string
		binaries map[string]string
		wantFail string
	}{
		{name: "all softfloat", binaries: map[string]string{"mips": soft, "mipsle": soft, "mips64": soft, "mips64le": soft}, wantFail: ""},
		{name: "mips64 hardfloat", binaries: map[string]string{"mips": soft, "mipsle": soft, "mips64": hard, "mips64le": soft}, wantFail: "mips64"},
		{name: "mips64le hardfloat", binaries: map[string]string{"mips": soft, "mipsle": soft, "mips64": soft, "mips64le": hard}, wantFail: "mips64le"},
		{name: "mips64 missing", binaries: map[string]string{"mips": soft, "mipsle": soft, "mips64le": soft}, wantFail: "mips64"},
	} {
		buildHome := filepath.Join(t.TempDir(), "build home")
		socksDir := filepath.Join(buildHome, "proxy", "v2026", "socks")
		binDir := filepath.Join(buildHome, "bin")
		selected := filepath.Join(buildHome, "selected")
		for _, dir := range []string{socksDir, binDir, selected} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for arch, from := range test.binaries {
			data, err := os.ReadFile(filepath.Join(from, arch))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(selected, arch), data, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(binDir, "make"), []byte(proxySocksFakeMake), 0o700); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, zsh, "-f", "-c", proxySocksSoftfloatHarness, "proxy-socks-softfloat-test", buildHome, mipsEnv, errorTrap, region)
		cmd.Env = environmentWith(
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"PROXY_SOCKS_BINARIES="+selected,
			"GOFLAGS=",
		)
		output, err := cmd.CombinedOutput()
		cancel()
		eventData, _ := os.ReadFile(filepath.Join(buildHome, "events"))
		events := string(eventData)

		if test.wantFail == "" {
			if err != nil || !strings.Contains(events, "release-continued") {
				t.Errorf("%s: softfloat release stopped: %v\n%s\n%s", test.name, err, output, events)
			}
			continue
		}
		if err == nil || strings.Contains(events, "release-continued") {
			t.Errorf("%s: release continued past a %s binary that is not softfloat\n%s\n%s", test.name, test.wantFail, output, events)
			continue
		}
		if want := "message:error(1): proxy socks " + test.wantFail + " softfloat"; !strings.Contains(events, want) {
			t.Errorf("%s: builder message missing %q\n%s\n%s", test.name, want, output, events)
		}
	}
}
