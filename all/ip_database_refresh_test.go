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
if [ "$1" = canary-baseline ]; then
  printf '%s\n' "$@" > "$IP_DATABASE_TEST_ARGS.baseline"
  printf '%s\n' "${IP_DATABASE_TEST_BASELINE-copy}"
  exit "${IP_DATABASE_TEST_BASELINE_STATUS:-0}"
fi
if [ "$1" = geolite2 ]; then
  printf '%s\n' "$@" > "$IP_DATABASE_TEST_ARGS.geolite2"
  [ "$2" = refresh ] || exit 9
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --output) shift; target="$1" ;;
    esac
    shift
  done
  mkdir -p "$target"
  printf 'synthetic geolite\n' > "$target/geolite2.mmdb"
  printf 'synthetic places\n' > "$target/places.yml"
  printf '{}\n' > "$target/manifest.json"
  exit 0
fi
printf '%s\n' "$@" > "$IP_DATABASE_TEST_ARGS"
[ "$1" = update ] || exit 8
previous=no
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) shift; target="$1" ;;
    --previous-config|--previous-database) previous=yes ;;
  esac
  shift
done
mkdir -p "$target/mmdb"
printf 'synthetic geolite\n' > "$target/mmdb/geolite2.mmdb"
[ "$IP_DATABASE_TEST_FAIL" = no ] || exit 7
mkdir -p "$target/arindb"
printf 'synthetic arin\n' > "$target/arindb/arin.mmdb"
# the canary copy, which a refused copy leaves out
if [ "$previous" = yes ] && [ "${IP_DATABASE_TEST_COPY:-yes}" = yes ]; then
  printf 'synthetic previous arin\n' > "$target/arindb/arin-old.mmdb"
  printf '{}\n' > "$target/arindb/manifest-old.json"
fi
printf 'synthetic places\n' > "$target/mmdb/places.yml"
printf '{}\n' > "$target/mmdb/manifest.json"
printf '{}\n' > "$target/arindb/manifest.json"
mkdir -p "$target/subscriber-evidence"
printf 'synthetic evidence\n' > "$target/subscriber-evidence/catalog.yml"
printf '%s\n' "${IP_DATABASE_TEST_SUMMARY:-registration and subscriber augmentation applied; unavailable evidence: asdb}" > "$target/update-summary.txt"
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
git_push_generated_paths() { print -r -- push >> "$IP_DATABASE_TEST_EVENTS"; }
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

// Older release directories the refresh must remove or keep: two ARIN
// releases, the second with its canary copy, and an older GeoLite2 release.
func writeOlderIpDatabaseReleases(t *testing.T, warpHome string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join("arindb", "1.0.0", "arin.mmdb"),
		filepath.Join("arindb", "1.1.0", "arin.mmdb"),
		filepath.Join("arindb", "1.1.0", "arin-old.mmdb"),
		filepath.Join("mmdb", "1.1.0", "geolite2.mmdb"),
	} {
		path = filepath.Join(warpHome, "config", "all", path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic older release\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Processes read only the newest ARIN release directory, so publishing one
// removes the older ones in the same commit; GeoLite2 releases are kept.
func TestReleaseIpDatabaseRefreshRemovesOlderArinReleases(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
	writeOlderIpDatabaseReleases(t, warpHome)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, output)
	}
	entries, err := os.ReadDir(filepath.Join(warpHome, "config", "all", "arindb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "1.2.3+4" {
		t.Fatalf("ARIN release directories %v, want only the new release", entries)
	}
	if _, err := os.Stat(filepath.Join(warpHome, "config", "all", "mmdb", "1.1.0", "geolite2.mmdb")); err != nil {
		t.Fatalf("an older GeoLite2 release was removed: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"git:add -A -- all/mmdb/1.2.3+4 all/arindb\n",
		"git:commit -m 1.2.3-4 GeoLite2 and ARIN databases -- all/mmdb/1.2.3+4 all/arindb\n",
		"push\n",
	} {
		if !strings.Contains(string(events), want) {
			t.Errorf("events %q lack %q", events, want)
		}
	}
}

// A paused ARIN refresh publishes no ARIN release, so the selected one and the
// releases before it stay.
func TestReleaseIpDatabaseRefreshPauseKeepsTheArinReleases(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false, "IP_DATABASE_TEST_BASELINE=pause the canary decision is version 2, not 1")
	decision := filepath.Join(dir, "decision.json")
	if err := os.WriteFile(decision, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command.Env = append(command.Env, "ARIN_CANARY_DECISION_FILE="+decision)
	writeOlderIpDatabaseReleases(t, warpHome)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, output)
	}
	for _, release := range []string{"1.0.0", "1.1.0"} {
		if _, err := os.Stat(filepath.Join(warpHome, "config", "all", "arindb", release, "arin.mmdb")); err != nil {
			t.Errorf("a pause removed ARIN release %s: %v", release, err)
		}
	}
	if _, err := os.Stat(filepath.Join(warpHome, "config", "all", "arindb", "1.2.3+4")); !os.IsNotExist(err) {
		t.Fatal("a pause published an ARIN release")
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

// The canary's previous database is exactly what the release's environment
// selects now: the runner names the config root and environment, before this
// release's version exists, and publishes arindbctl's copy with the release.
func TestReleaseIpDatabaseRefreshCopiesThePreviousSelectionForward(t *testing.T) {
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, output)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--previous-config\n"+filepath.Join(warpHome, "config")+"\n--previous-env\nmain\n") || strings.Contains(string(args), "--previous-database") {
		t.Fatalf("refresh did not name the selected previous database: %s", args)
	}
	for _, name := range []string{"arin-old.mmdb", "manifest-old.json"} {
		if _, err := os.Stat(filepath.Join(warpHome, "config", "all", "arindb", "1.2.3+4", name)); err != nil {
			t.Fatalf("the release lost the canary copy %s: %v", name, err)
		}
	}
}

// After a bad release the operator pins the last good database; the pin
// replaces the selection rather than adding to it.
func TestReleaseIpDatabaseRefreshPinsThePreviousDatabase(t *testing.T) {
	pinned := filepath.Join(t.TempDir(), "last good", "arin.mmdb")
	dir, _, command := ipDatabaseRefreshFixture(t, false, "ARIN_PREVIOUS_DATABASE_FILE="+pinned)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, output)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--previous-database\n"+pinned+"\n") || strings.Contains(string(args), "--previous-config") || strings.Contains(string(args), "--previous-env") {
		t.Fatalf("the pin was not passed alone: %s", args)
	}
}

// A copy arindbctl refuses ships the release without a canary: the release
// still publishes and pushes, and its message says so.
func TestReleaseIpDatabaseRefreshShipsWithoutACanaryWhenTheCopyIsRefused(t *testing.T) {
	summary := "registration and subscriber augmentation applied; no canary: previous database refused (the copied bytes do not have the previous manifest's SHA-256)"
	dir, warpHome, command := ipDatabaseRefreshFixture(t, false, "IP_DATABASE_TEST_COPY=no", "IP_DATABASE_TEST_SUMMARY="+summary)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("a refused copy failed the release: %v\n%s", err, output)
	}
	release := filepath.Join(warpHome, "config", "all", "arindb", "1.2.3+4")
	if _, err := os.Stat(filepath.Join(release, "arin.mmdb")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(release, "arin-old.mmdb")); !os.IsNotExist(err) {
		t.Fatal("a refused copy was published")
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "push\n") || !strings.Contains(string(events), "no canary: previous database refused") {
		t.Fatalf("the release did not publish or did not say it ships without a canary: %s", events)
	}
}

// With the published canary decision the copy follows the database it
// protects, from the selected Server's arindbctl: the selection copies as
// before, a fallback's database is pinned, and a decision it cannot follow, or
// a failing answer, pauses the ARIN refresh without failing the release while
// GeoLite2 still refreshes. The operator's pin wins over the decision, and
// without a decision the release says the copy was not gated.
func TestReleaseIpDatabaseRefreshFollowsTheCanaryDecision(t *testing.T) {
	decisionEnv := func(dir string) string {
		path := filepath.Join(dir, "decision.json")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "ARIN_CANARY_DECISION_FILE=" + path
	}
	for _, c := range []struct {
		name      string
		env       []string
		pinned    bool
		published bool
		paused    bool
		update    string
		message   string
	}{
		{name: "the selection is served", env: []string{"IP_DATABASE_TEST_BASELINE=copy"}, published: true,
			update: "--previous-config\n", message: "registration and subscriber augmentation applied; unavailable evidence: asdb"},
		{name: "a fallback is served", env: []string{"IP_DATABASE_TEST_BASELINE=pin /served/arin.mmdb"}, published: true,
			update: "--previous-database\n/served/arin.mmdb\n", message: "(canary baseline pinned to the protected /served/arin.mmdb)"},
		{name: "a decision it cannot follow", env: []string{"IP_DATABASE_TEST_BASELINE=pause the canary decision is version 2, not 1"}, paused: true,
			message: "message:GeoLite2 database refreshed for `1.2.3-4`. ARIN refresh paused, so the previous ARIN release stays selected: the canary decision is version 2, not 1"},
		{name: "an empty answer", env: []string{"IP_DATABASE_TEST_BASELINE="}, paused: true,
			message: "ARIN refresh paused, so the previous ARIN release stays selected: unrecognized canary-baseline response"},
		{name: "an empty pause reason", env: []string{"IP_DATABASE_TEST_BASELINE=pause "}, paused: true,
			message: "ARIN refresh paused, so the previous ARIN release stays selected: unrecognized canary-baseline response"},
		{name: "an unknown answer", env: []string{"IP_DATABASE_TEST_BASELINE=unexpected"}, paused: true,
			message: "ARIN refresh paused, so the previous ARIN release stays selected: unrecognized canary-baseline response"},
		{name: "a failing answer", env: []string{"IP_DATABASE_TEST_BASELINE=copy", "IP_DATABASE_TEST_BASELINE_STATUS=3"}, paused: true,
			message: "message:GeoLite2 database refreshed for `1.2.3-4`. ARIN refresh paused, so the previous ARIN release stays selected: arindbctl canary-baseline failed"},
		{name: "the operator's pin", env: []string{"IP_DATABASE_TEST_BASELINE=pause never asked", "ARIN_PREVIOUS_DATABASE_FILE=/pinned/arin.mmdb"},
			pinned: true, published: true, update: "--previous-database\n/pinned/arin.mmdb\n"},
	} {
		dir, warpHome, command := ipDatabaseRefreshFixture(t, false, c.env...)
		command.Env = append(command.Env, decisionEnv(dir))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s: refresh failed: %v\n%s", c.name, err, output)
		}
		baselineArgs, baselineErr := os.ReadFile(filepath.Join(dir, "args.baseline"))
		if c.pinned != os.IsNotExist(baselineErr) {
			t.Errorf("%s: canary-baseline asked=%t, want %t", c.name, baselineErr == nil, !c.pinned)
		}
		if !c.pinned && !strings.Contains(string(baselineArgs), "canary-baseline\n--decision\n"+filepath.Join(dir, "decision.json")+"\n--previous-config\n"+filepath.Join(warpHome, "config")+"\n--previous-env\nmain\n") {
			t.Errorf("%s: canary-baseline args %q", c.name, baselineArgs)
		}
		args, argsErr := os.ReadFile(filepath.Join(dir, "args"))
		_, published := os.Stat(filepath.Join(warpHome, "config", "all", "arindb", "1.2.3+4", "arin.mmdb"))
		if c.published != (published == nil) || c.published != (argsErr == nil) {
			t.Errorf("%s: published=%v update=%v, want published %t", c.name, published, argsErr, c.published)
		}
		if c.published && !strings.Contains(string(args), c.update) {
			t.Errorf("%s: update args %q lack %q", c.name, args, c.update)
		}
		events, _ := os.ReadFile(filepath.Join(dir, "events"))
		if c.message != "" && !strings.Contains(string(events), c.message) {
			t.Errorf("%s: events %q lack %q", c.name, events, c.message)
		}
		// Both outcomes retain a complete publication for the release checkpoint.
		checkpoint := filepath.Join(dir, "build tree", "out", "ip-databases", "release-1.2.3+4")
		for _, name := range []string{"summary.txt", "mmdb/geolite2.mmdb"} {
			if _, err := os.Stat(filepath.Join(checkpoint, name)); err != nil {
				t.Errorf("%s: missing checkpoint output %s: %v", c.name, name, err)
			}
		}
		_, arinCheckpoint := os.Stat(filepath.Join(checkpoint, "arindb", "arin.mmdb"))
		if c.published != (arinCheckpoint == nil) {
			t.Errorf("%s: checkpoint ARIN=%v, want published %t", c.name, arinCheckpoint, c.published)
		}
		// a pause still publishes GeoLite2, and only GeoLite2
		_, geolite2 := os.Stat(filepath.Join(warpHome, "config", "all", "mmdb", "1.2.3+4", "geolite2.mmdb"))
		if geolite2 != nil {
			t.Errorf("%s: GeoLite2 was not published: %v", c.name, geolite2)
		}
		geolite2Args, geolite2Err := os.ReadFile(filepath.Join(dir, "args.geolite2"))
		if c.paused != (geolite2Err == nil) {
			t.Errorf("%s: GeoLite2-only refresh ran=%t, want %t", c.name, geolite2Err == nil, c.paused)
		}
		if c.paused && (!strings.Contains(string(geolite2Args), "geolite2\nrefresh\n--geoip-config\n"+filepath.Join(warpHome, "vault", "mm-geoip.yml")+"\n") ||
			!strings.Contains(string(events), "git:commit -m 1.2.3-4 GeoLite2 database -- all/mmdb/1.2.3+4\n") || strings.Contains(string(events), "all/arindb/1.2.3+4")) {
			t.Errorf("%s: a pause did not publish GeoLite2 alone: args %q events %q", c.name, geolite2Args, events)
		}
	}
	// without a decision the copy is not gated, and the release says so
	dir, _, command := ipDatabaseRefreshFixture(t, false)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "args.baseline")); !os.IsNotExist(err) {
		t.Fatal("canary-baseline was asked without a decision")
	}
	if events, _ := os.ReadFile(filepath.Join(dir, "events")); !strings.Contains(string(events), "(canary copy-forward not gated: ARIN_CANARY_DECISION_FILE is unset)") {
		t.Fatalf("an ungated copy was not reported: %s", events)
	}
}
