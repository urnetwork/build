// Release refresh tests run the owning shell function with synthetic offline
// tools; they never execute the build runner, read real credentials, or publish.
package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// One independent fake build home records exact source and command ordering.
func ipDatabaseRefreshFixture(t *testing.T, fail bool, env ...string) (string, string, *exec.Cmd) {
	t.Helper()
	dir := t.TempDir()
	buildHome, warpHome := filepath.Join(dir, "build tree"), filepath.Join(dir, "config owner")
	for _, path := range []string{filepath.Join(buildHome, "server", "v2026"), filepath.Join(warpHome, "root"), filepath.Join(warpHome, "vault"), filepath.Join(warpHome, "config", "main", "arindb-subscribers")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(warpHome, "vault", "mm-geoip.yml"), filepath.Join(warpHome, "vault", "arin.yml"), filepath.Join(warpHome, "config", "main", "arindb.yml"), filepath.Join(warpHome, "config", "main", "arindb-subscribers", "catalog.yml")} {
		if err := os.WriteFile(path, []byte("synthetic test input\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(dir, "fake-arindbctl")
	if err := os.WriteFile(fake, []byte(`#!/bin/sh
printf '%s\n' "$@" > "$IP_DATABASE_TEST_ARGS"
[ "$1" = update ] || exit 8
while [ "$#" -gt 0 ]; do
  if [ "$1" = --output ]; then shift; target="$1"; fi
  shift
done
mkdir -p "$target/mmdb"
printf 'synthetic geolite\n' > "$target/mmdb/geolite2.mmdb"
[ "$IP_DATABASE_TEST_FAIL" = no ] || exit 7
mkdir -p "$target/arindb"
printf 'synthetic arin\n' > "$target/arindb/arin.mmdb"
printf 'synthetic places\n' > "$target/mmdb/places.yml"
printf '{}\n' > "$target/mmdb/manifest.json"
printf '{}\n' > "$target/arindb/manifest.json"
mkdir -p "$target/subscriber-evidence"
printf 'synthetic evidence\n' > "$target/subscriber-evidence/catalog.yml"
printf 'registration and subscriber augmentation applied; unavailable evidence: asdb\n' > "$target/update-summary.txt"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	harness := `
set -eu
go() {
    print -r -- "go-source:$PWD" >> "$IP_DATABASE_TEST_EVENTS"
    local target='' previous='' argument
    for argument in "$@"; do
        if [ "$previous" = '-o' ]; then target="$argument"; fi
        previous="$argument"
    done
    cp "$IP_DATABASE_TEST_BINARY" "$target"
}
git() {
    print -r -- "git:$*" >> "$IP_DATABASE_TEST_EVENTS"
    if [ "$1" = diff ]; then return 1; fi
}
git_push_with_rebase_retry() { print -r -- push >> "$IP_DATABASE_TEST_EVENTS"; }
builder_message() { print -r -- "message:$*" >> "$IP_DATABASE_TEST_EVENTS"; }
eval "$1"
refresh_ip_databases
`
	command := exec.Command("zsh", "-f", "-c", harness, "ip-database-test", runFunction(t, "refresh_ip_databases"))
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "BUILD_HOME=" + buildHome, "WARP_HOME=" + warpHome, "BUILD_ENV=main", "GO_MOD_SUFFIX=/v2026", "WARP_VERSION=1.2.3+4", "EXTERNAL_WARP_VERSION=1.2.3-4", "IP_DATABASE_TEST_BINARY=" + fake, "IP_DATABASE_TEST_ARGS=" + filepath.Join(dir, "args"), "IP_DATABASE_TEST_EVENTS=" + filepath.Join(dir, "events"), "IP_DATABASE_TEST_FAIL=no"}
	if fail {
		command.Env[len(command.Env)-1] = "IP_DATABASE_TEST_FAIL=yes"
	}
	command.Env = append(command.Env, env...)
	return dir, warpHome, command
}

// The refresh binary comes from the selected versioned Server checkout and
// publishes both resources before the config updater can package them.
func TestReleaseRefreshesIpDatabasesFromVersionedServer(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, output)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"update\n", "--timeout\n2h\n", "--geoip-config\n" + filepath.Join(warpHome, "vault", "mm-geoip.yml") + "\n", "--credentials\n" + filepath.Join(warpHome, "vault", "arin.yml") + "\n", "--rules\n" + filepath.Join(warpHome, "config", "main", "arindb.yml") + "\n", "--subscriber-catalog\n" + filepath.Join(warpHome, "config", "main", "arindb-subscribers", "catalog.yml") + "\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("refresh lost explicit input %q", want)
		}
	}
	for _, path := range []string{filepath.Join("mmdb", "1.2.3+4", "geolite2.mmdb"), filepath.Join("mmdb", "1.2.3+4", "places.yml"), filepath.Join("arindb", "1.2.3+4", "arin.mmdb")} {
		if _, err := os.Stat(filepath.Join(warpHome, "config", "all", path)); err != nil {
			t.Fatal(err)
		}
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), filepath.Join("build tree", "server", "v2026")) || !strings.Contains(string(events), "push\n") {
		t.Fatal("release lost selected source or config publication")
	}
	if !strings.Contains(string(events), "message:GeoLite2 and ARIN databases refreshed for `1.2.3-4`: registration and subscriber augmentation applied; unavailable evidence: asdb") {
		t.Fatalf("release message lost the update summary: %s", events)
	}
	if strings.Contains(string(args), "--relay-geofeeds") {
		t.Fatalf("absent relay opt-in produced a flag: %s", args)
	}
	// The evidence directory stays in the build output; it is never config.
	if _, err := os.Stat(filepath.Join(warpHome, "config", "all", "arindb", "1.2.3+4", "subscriber-evidence")); !os.IsNotExist(err) {
		t.Fatal("subscriber evidence was published as config")
	}
	runner, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	refresh := strings.Index(string(runner), "\nrefresh_ip_databases\n")
	configImage := strings.Index(string(runner), "\nbuild_config_updater ()")
	if refresh < 0 || configImage < refresh {
		t.Fatal("all-release refresh is not before config image construction")
	}
}

// Download/build failure must not publish even the already-generated GeoLite
// half, commit config, or advance the existing cache versions.
func TestReleaseIpDatabaseRefreshFailurePreservesCachedVersions(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, true)
	old := filepath.Join(warpHome, "config", "all", "mmdb", "1.2.2", "geolite2.mmdb")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("old complete cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("failed refresh succeeded: %s", output)
	}
	for _, path := range []string{filepath.Join("mmdb", "1.2.3+4"), filepath.Join("arindb", "1.2.3+4")} {
		if _, err := os.Stat(filepath.Join(warpHome, "config", "all", path)); !os.IsNotExist(err) {
			t.Fatal("failed refresh published a partial config version")
		}
	}
	content, err := os.ReadFile(old)
	if err != nil || string(content) != "old complete cache" {
		t.Fatal("old cache changed")
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), "git:") || strings.Contains(string(events), "push\n") {
		t.Fatal("failed refresh committed or pushed config")
	}
}

// Missing classifier/secret paths fail clearly before starting any tool.
func TestReleaseIpDatabaseRefreshRequiresReviewedInputs(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
	if err := os.Remove(filepath.Join(warpHome, "config", "main", "arindb.yml")); err != nil {
		t.Fatal(err)
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "ARIN_RULES_FILE") {
		t.Fatalf("missing reviewed classifier was not reported: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "events")); !os.IsNotExist(err) {
		t.Fatal("missing input started the release tools")
	}
}

// A retry must select a new resource version instead of replacing a complete
// cached release, even before the expensive source downloads begin.
func TestReleaseIpDatabaseRefreshRejectsExistingVersion(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
	old := filepath.Join(warpHome, "config", "all", "arindb", "1.2.3+4", "arin.mmdb")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("existing complete version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "version already exists") {
		t.Fatalf("existing version was not rejected: %v %s", err, output)
	}
	content, err := os.ReadFile(old)
	if err != nil || string(content) != "existing complete version" {
		t.Fatal("existing version changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "events")); !os.IsNotExist(err) {
		t.Fatal("existing version started the release tools")
	}
}

// Failure between the two local publication renames rolls the first resource
// back to the private stage and cannot commit or push an incomplete bundle.
func TestReleaseIpDatabaseRefreshRollsBackSecondRenameFailure(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
	command.Args[3] = `mv() {
    case "$2" in */bundle/arindb) return 9 ;; esac
    command mv "$@"
}
` + command.Args[3]
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("failed publication succeeded: %s", output)
	}
	for _, resource := range []string{"mmdb", "arindb"} {
		if _, err := os.Stat(filepath.Join(warpHome, "config", "all", resource, "1.2.3+4")); !os.IsNotExist(err) {
			t.Fatal("failed publication left a partial version visible")
		}
	}
	restored, err := filepath.Glob(filepath.Join(dir, "build tree", "out", "ip-databases", "refresh.*", "bundle", "mmdb", "geolite2.mmdb"))
	if err != nil || len(restored) != 1 {
		t.Fatal("first publication was not restored to its private stage")
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), "git:") || strings.Contains(string(events), "push\n") {
		t.Fatal("failed publication committed or pushed config")
	}
}

// A reviewed subscriber catalog at the default path is passed to update, and
// the relay geofeeds are pinned only when explicitly requested.
func TestReleaseIpDatabaseUpdateUsesReviewedSubscriberCatalog(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false, "ARIN_RELAY_GEOFEEDS=1")
	catalog := filepath.Join(warpHome, "config", "main", "arindb-subscribers", "catalog.yml")
	if err := os.MkdirAll(filepath.Dir(catalog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, []byte("synthetic reviewed catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("update: %v\n%s", err, output)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--subscriber-catalog\n"+catalog+"\n") || !strings.Contains(string(args), "--relay-geofeeds\n") {
		t.Fatalf("update lost the reviewed catalog or relay opt-in: %s", args)
	}
}

// An explicitly configured catalog that cannot be read stops the release
// instead of silently publishing a registration-only resource.
func TestReleaseIpDatabaseUpdateRejectsUnreadableConfiguredCatalog(t *testing.T) {
	dir, _, command := ipDatabaseRefreshFixture(t, false, "ARIN_SUBSCRIBER_CATALOG_FILE="+filepath.Join(t.TempDir(), "missing.yml"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "ARIN_SUBSCRIBER_CATALOG_FILE") {
		t.Fatalf("unreadable configured catalog was not reported: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "events")); !os.IsNotExist(err) {
		t.Fatal("unreadable catalog started the release tools")
	}
}

// A missing default must never turn the normal full release into a
// registration-only build. Check before any tool or output directory starts.
func TestReleaseIpDatabaseUpdateRequiresDefaultCatalog(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "broken-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
			catalog := filepath.Join(warpHome, "config", "main", "arindb-subscribers", "catalog.yml")
			if err := os.Remove(catalog); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory":
				if err := os.Mkdir(catalog, 0o700); err != nil {
					t.Fatal(err)
				}
			case "broken-symlink":
				if err := os.Symlink(filepath.Join(dir, "absent-reviewed-catalog.yml"), catalog); err != nil {
					t.Fatal(err)
				}
			}
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "ARIN_SUBSCRIBER_CATALOG_FILE") {
				t.Fatalf("invalid default catalog was not refused: %v %s", err, output)
			}
			for _, path := range []string{filepath.Join(dir, "events"), filepath.Join(dir, "args"), filepath.Join(dir, "build tree", "out")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("invalid default catalog started tools or staging: %s", path)
				}
			}
		})
	}
}

// An explicit reviewed catalog remains supported without a default file.
func TestReleaseIpDatabaseUpdateUsesExplicitCatalogWithoutDefault(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
	defaultCatalog := filepath.Join(warpHome, "config", "main", "arindb-subscribers", "catalog.yml")
	if err := os.Remove(defaultCatalog); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(dir, "reviewed override.yml")
	if err := os.WriteFile(catalog, []byte("synthetic reviewed override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command.Env = append(command.Env, "ARIN_SUBSCRIBER_CATALOG_FILE="+catalog)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("explicit catalog failed: %v %s", err, output)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--subscriber-catalog\n"+catalog+"\n") || strings.Contains(string(args), defaultCatalog) {
		t.Fatalf("explicit catalog was not preserved: %s", args)
	}
}
