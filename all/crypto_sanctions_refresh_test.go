// The OFAC crypto-address refresh runs the owning shell function with a
// synthetic offline cryptoctl; it never downloads, builds Server or publishes.
package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const cryptoSanctionsTestSummary = "OFAC SDN list of 2026-11-09 (sha256 329a9b9c6e55, 1081 digital currency identifiers): btc 536, eth 127, sol 4, usdc_solana 4, xmr 10, zec 4, tao 0"

const cryptoSanctionsTestError = "cryptoctl: download https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML failed 3 times: answered 503 Service Unavailable"

// One independent fake build home. The fake cryptoctl writes a partial table
// before it fails, so a failure that published anything would show.
func cryptoSanctionsRefreshFixture(t *testing.T, env ...string) (string, string, *exec.Cmd) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq, a required build tool, is not installed")
	}
	dir := t.TempDir()
	buildHome, warpHome := filepath.Join(dir, "build tree"), filepath.Join(dir, "config owner")
	for _, path := range []string{filepath.Join(buildHome, "server", "v2026"), filepath.Join(warpHome, "config", "all")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(dir, "fake-cryptoctl")
	if err := os.WriteFile(fake, []byte(`#!/bin/sh
printf '%s\n' "$@" > "$CRYPTO_TEST_ARGS"
[ "$1" = update ] || exit 8
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) shift; target="$1" ;;
  esac
  shift
done
mkdir -p "$target"
printf '{"format":"urnetwork-crypto-sanctions-1","source":{"publish_date":"2026-11-09"}}\n' > "$target/manifest.json"
if [ "${CRYPTO_TEST_FAIL:-no}" = yes ]; then
  echo 'synthetic progress line'
  echo 'synthetic diagnostic line' >&2
  echo "$CRYPTO_TEST_ERROR" >&2
  exit 1
fi
printf '{"btc": []}\n' > "$target/addresses.json"
printf '%s\n' "$CRYPTO_TEST_SUMMARY"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	harness := `
set -eu
go() {
    print -r -- "go-source:$PWD" >> "$CRYPTO_TEST_EVENTS"
    print -r -- "go:$*" >> "$CRYPTO_TEST_EVENTS"
    if [ "${CRYPTO_TEST_GO_FAIL:-no}" = yes ]; then
        print -u2 -- 'cryptoctl/main.go:1:1: synthetic compile error'
        return 1
    fi
    local target='' previous='' argument
    for argument in "$@"; do
        if [ "$previous" = '-o' ]; then target="$argument"; fi
        previous="$argument"
    done
    cp "$CRYPTO_TEST_BINARY" "$target"
}
git() {
    print -r -- "git:$*" >> "$CRYPTO_TEST_EVENTS"
    if [ "$1" = diff ]; then return 1; fi
    if [ "$1" = commit ] && [ "${CRYPTO_TEST_COMMIT_FAIL:-no}" = yes ]; then return 1; fi
    return 0
}
git_push_generated_paths() { print -r -- "push:$*" >> "$CRYPTO_TEST_EVENTS"; }
builder_message() { print -r -- "message:$*" >> "$CRYPTO_TEST_EVENTS"; }
eval "$1"
refresh_crypto_sanctions
print -r -- returned >> "$CRYPTO_TEST_EVENTS"
`
	command := exec.Command("zsh", "-f", "-c", harness, "crypto-sanctions-test", runFunction(t, "refresh_crypto_sanctions"))
	command.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"BUILD_HOME=" + buildHome,
		"WARP_HOME=" + warpHome,
		"BUILD_ENV=main",
		"GO_MOD_SUFFIX=/v2026",
		"WARP_VERSION=1.2.3+4",
		"EXTERNAL_WARP_VERSION=1.2.3-4",
		"CRYPTO_TEST_BINARY=" + fake,
		"CRYPTO_TEST_ARGS=" + filepath.Join(dir, "args"),
		"CRYPTO_TEST_EVENTS=" + filepath.Join(dir, "events"),
		"CRYPTO_TEST_SUMMARY=" + cryptoSanctionsTestSummary,
		"CRYPTO_TEST_ERROR=" + cryptoSanctionsTestError,
	}, env...)
	return dir, warpHome, command
}

// Older lists the refresh removes, or keeps when it publishes nothing.
func writeOlderCryptoSanctionsReleases(t *testing.T, warpHome string) {
	t.Helper()
	for release, date := range map[string]string{"1.0.0": "2026-10-01", "1.1.0": "2026-11-01"} {
		dir := filepath.Join(warpHome, "config", "all", "crypto", release)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"manifest.json":  `{"format": "urnetwork-crypto-sanctions-1", "source": {"publish_date": "` + date + `"}}` + "\n",
			"addresses.json": "{}\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func readCryptoSanctionsFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func cryptoSanctionsReleases(t *testing.T, warpHome string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(warpHome, "config", "all", "crypto"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// Every release builds cryptoctl from the selected versioned Server, publishes
// the table as the release's directory, removes the older ones in the same
// commit, pushes it, and says what it published.
func TestReleaseRefreshesCryptoSanctionsFromVersionedServer(t *testing.T) {
	dir, warpHome, command := cryptoSanctionsRefreshFixture(t)
	writeOlderCryptoSanctionsReleases(t, warpHome)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, output)
	}
	if releases := cryptoSanctionsReleases(t, warpHome); strings.Join(releases, ",") != "1.2.3+4" {
		t.Fatalf("crypto release directories %v, want only the new release", releases)
	}
	for _, name := range []string{"manifest.json", "addresses.json"} {
		if _, err := os.Stat(filepath.Join(warpHome, "config", "all", "crypto", "1.2.3+4", name)); err != nil {
			t.Fatal(err)
		}
	}
	args := readCryptoSanctionsFile(t, filepath.Join(dir, "args"))
	if !strings.HasPrefix(args, "update\n--output\n") || !strings.Contains(args, "\n--timeout\n15m\n") {
		t.Fatalf("cryptoctl args %q", args)
	}
	events := readCryptoSanctionsFile(t, filepath.Join(dir, "events"))
	for _, want := range []string{
		"go-source:" + filepath.Join(dir, "build tree", "server", "v2026") + "\n",
		"go:build -ldflags -X main.Version=1.2.3+4 -o ",
		"/cryptoctl ./cryptoctl\n",
		"git:add -A -- all/crypto\n",
		"git:commit -m 1.2.3-4 OFAC crypto addresses -- all/crypto\n",
		"push:all/crypto\n",
		"message:OFAC crypto addresses refreshed for `1.2.3-4`: " + cryptoSanctionsTestSummary + "\n",
		"returned\n",
	} {
		if !strings.Contains(events, want) {
			t.Errorf("events %q lack %q", events, want)
		}
	}
	// the stage's checkpoint holds what was published and the summary
	checkpoint := filepath.Join(dir, "build tree", "out", "crypto-sanctions", "release-1.2.3+4")
	if summary := readCryptoSanctionsFile(t, filepath.Join(checkpoint, "summary.txt")); summary != cryptoSanctionsTestSummary+"\n" {
		t.Fatalf("checkpoint summary %q", summary)
	}
	if _, err := os.Stat(filepath.Join(checkpoint, "crypto", "addresses.json")); err != nil {
		t.Fatal(err)
	}
}

// A refused download, or data cryptoctl refuses, keeps the last committed
// list without failing the release: nothing is published or committed, and
// the release message says which list stays selected.
func TestReleaseCryptoSanctionsFailureKeepsTheLastList(t *testing.T) {
	for _, c := range []struct {
		name    string
		env     string
		failure string
		ran     bool
	}{
		{name: "download", env: "CRYPTO_TEST_FAIL=yes", failure: cryptoSanctionsTestError, ran: true},
		{name: "build", env: "CRYPTO_TEST_GO_FAIL=yes", failure: "cryptoctl did not build from the selected Server (errors above)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, warpHome, command := cryptoSanctionsRefreshFixture(t, c.env)
			writeOlderCryptoSanctionsReleases(t, warpHome)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("a failed refresh failed the release: %v\n%s", err, output)
			}
			if releases := cryptoSanctionsReleases(t, warpHome); strings.Join(releases, ",") != "1.0.0,1.1.0" {
				t.Fatalf("crypto release directories %v, want the older lists untouched", releases)
			}
			events := readCryptoSanctionsFile(t, filepath.Join(dir, "events"))
			if strings.Contains(events, "git:") || strings.Contains(events, "push:") {
				t.Fatalf("a failed refresh committed or pushed config: %s", events)
			}
			message := "message:warning: OFAC crypto addresses not refreshed for `1.2.3-4`: " + c.failure +
				"; the last committed list stays selected: `1.0.0` (OFAC SDN list of 2026-10-01), `1.1.0` (OFAC SDN list of 2026-11-01)\n"
			if !strings.Contains(events, message) || !strings.HasSuffix(events, "returned\n") {
				t.Fatalf("events %q lack %q", events, message)
			}
			if _, err := os.Stat(filepath.Join(dir, "args")); c.ran != (err == nil) {
				t.Fatalf("cryptoctl ran=%t, want %t", err == nil, c.ran)
			}
			if c.ran && !strings.Contains(string(output), "synthetic diagnostic line\n") {
				t.Fatalf("cryptoctl's errors are not in the build log: %s", output)
			}
			checkpoint := filepath.Join(dir, "build tree", "out", "crypto-sanctions", "release-1.2.3+4", "summary.txt")
			if summary := readCryptoSanctionsFile(t, checkpoint); !strings.HasPrefix(summary, "not refreshed: "+c.failure+"; the last committed list stays selected: ") {
				t.Fatalf("checkpoint summary %q", summary)
			}
		})
	}
}

// Before the first list is published, a failure says payments go to review.
func TestReleaseCryptoSanctionsFailureWithoutAList(t *testing.T) {
	dir, warpHome, command := cryptoSanctionsRefreshFixture(t, "CRYPTO_TEST_FAIL=yes")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("a failed refresh failed the release: %v\n%s", err, output)
	}
	if releases := cryptoSanctionsReleases(t, warpHome); len(releases) != 0 {
		t.Fatalf("a failed refresh published %v", releases)
	}
	events := readCryptoSanctionsFile(t, filepath.Join(dir, "events"))
	if !strings.Contains(events, "; no list is committed, so screened crypto payments are held for review until one is\n") {
		t.Fatalf("events %q", events)
	}
}

// A release never replaces a published version, and starts no tool when one
// exists; a failed commit stops the release as the IP refresh's does.
func TestReleaseCryptoSanctionsRefusesExistingVersionAndFailedCommit(t *testing.T) {
	dir, warpHome, command := cryptoSanctionsRefreshFixture(t)
	existing := filepath.Join(warpHome, "config", "all", "crypto", "1.2.3+4", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(existing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("existing complete version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "version already exists") {
		t.Fatalf("an existing version was not refused: %v %s", err, output)
	}
	if content := readCryptoSanctionsFile(t, existing); content != "existing complete version\n" {
		t.Fatal("the existing version changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "events")); !os.IsNotExist(err) {
		t.Fatal("an existing version started the release tools")
	}

	dir, _, command = cryptoSanctionsRefreshFixture(t, "CRYPTO_TEST_COMMIT_FAIL=yes")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("a failed commit succeeded: %s", output)
	}
	if events := readCryptoSanctionsFile(t, filepath.Join(dir, "events")); strings.Contains(events, "push:") || strings.Contains(events, "returned") {
		t.Fatalf("a failed commit pushed or continued: %s", events)
	}
}

// The step is a top-level release stage, on by default, after the IP refresh
// and before the config image that carries the release's config.
func TestReleaseCryptoSanctionsStageRunsOnEveryRelease(t *testing.T) {
	runner, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	stage := regexp.MustCompile(`(?m)^if release_build crypto-sanctions "\$\{BUILD_OUT:-\$BUILD_HOME/out\}/crypto-sanctions/release-\$WARP_VERSION"; then
refresh_crypto_sanctions
error_trap 'OFAC crypto address release update'
release_transition crypto-sanctions
release_complete crypto-sanctions "\$\{BUILD_OUT:-\$BUILD_HOME/out\}/crypto-sanctions/release-\$WARP_VERSION"
fi
`)
	location := stage.FindIndex(runner)
	if location == nil || len(stage.FindAll(runner, -1)) != 1 {
		t.Fatal("run.sh has no single crypto-sanctions stage")
	}
	ipRefresh := strings.Index(string(runner), "\nrefresh_ip_databases\n")
	configImage := strings.Index(string(runner), "\nbuild_config_updater ()")
	if ipRefresh < 0 || location[0] < ipRefresh || configImage < location[0] {
		t.Fatal("the crypto-sanctions stage is not between the IP refresh and the config image")
	}
	// nothing between the IP stage and this one can skip it
	between := string(runner[ipRefresh:location[0]])
	if strings.Count(between, "\nfi\n") != 1 || strings.Contains(between, "\nif [") {
		t.Fatalf("the crypto-sanctions stage is conditional:\n%s", between)
	}
}
