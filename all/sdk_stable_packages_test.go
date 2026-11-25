// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stable SDK block of run.sh through the loop that publishes the nightly's
// own SDK assets. Both markers predate the checkpointed stage, so the same
// region also runs against the code before it.
func sdkStableRegion(t *testing.T) string {
	t.Helper()
	return componentRegion(t, "# Stable SDK packages: every native registry package", "\n# THE FLATPAK")
}

const (
	sdkStableBundle  = "URnetworkSdk-stable-python-0.0.0-123.tar.gz"
	sdkStableIndex   = "URnetworkSdk-stable-0.0.0-123.json"
	sdkStableArchive = "URnetworkSdk-C-0.0.0-linux-amd64.zip"
	sdkNightlyAsset  = "URnetworkSdk-0.0.0-123-linux-amd64.gz"
)

var sdkStableAssets = []string{sdkStableBundle, sdkStableIndex, sdkStableArchive}

// A journal, a GitHub release and an sdk packaging tool that keep their state
// in $STATE, with the refusals of release-state.py and release-publication.py
// that the stable block can meet. The packaging tool writes new bytes on every
// build, as the real tarballs get new mtimes.
const sdkStableSetup = `
WARP_VERSION_BASE=0.0.0
GITHUB_RELEASE_ID=1
mkdir -p "$BUILD_HOME/sdk/packaging/release/assets" "$STATE/github" "$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION"
if [[ "$SDK_PHASE" == stable ]]; then
    printf 'package main\n\nfunc releaseStage(phase string) {\n\tswitch phase {\n\tcase "stable":\n\t\treleaseStable()\n\t}\n}\n' > "$BUILD_HOME/sdk/packaging/release.go"
else
    printf 'package main\n\nfunc releaseStage(phase string) {\n\tswitch phase {\n\tdefault:\n\t\tpanic("unknown SDK release phase")\n\t}\n}\n' > "$BUILD_HOME/sdk/packaging/release.go"
fi
for name in ${=NIGHTLY_ASSETS}; do printf 'nightly %s\n' "$name" > "$BUILD_HOME/sdk/packaging/release/assets/$name"; done
# An earlier attempt was interrupted while the packaging tool ran: it recorded
# the nightly's asset names, started the stage and left stable files behind.
if [[ -n "$INTERRUPTED" ]]; then
    print -rl -- ${=NIGHTLY_ASSETS} > "$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/sdk-stable-nightly-assets.txt"
    : > "$STATE/sdk-stable.running"
    for name in ${=LEFTOVER_ASSETS}; do printf 'interrupted attempt %s\n' "$name" > "$BUILD_HOME/sdk/packaging/release/assets/$name"; done
fi
cat > "$BUILD_HOME/all/github-release-upload.zsh" <<'UPLOAD'
#!/bin/sh
printf 'publish\n' >> "$BUILD_HOME/events"
if [ "$FAIL_STEP" = publish ] || [ "$2" = "$FAIL_ASSET" ]; then exit 37; fi
printf 'asset:%s\n' "$2" >> "$BUILD_HOME/events"
cp "$3" "$STATE/github/$2" || exit 1
printf '{"id":1}'
UPLOAD
builder_message() { printf 'message:%s\n' "$1" >> "$event_log"; }
virustotal() {
    record_component_step scan
    if [[ "$1" == "$VERDICT_ASSET" ]]; then
        builder_message "virustotal analysis $1 failed: suspicious"
        exit 1
    fi
    VIRUSTOTAL_ARTIFACTS+=("|$1|scanned|")
}
python3() {
    if [[ "$1" != */release-publication.py || "$2" != asset ]]; then
        printf 'unexpected-python3\n' >> "$event_log"
        return 41
    fi
    if [[ "$4" == "$REFUSE_ASSET" ]] || { [[ -f "$STATE/github/$4" ]] && ! cmp -s "$STATE/github/$4" "$5"; }; then
        print -u2 "release publication refused: existing release asset $4 has conflicting/unverifiable content; never delete or replace it"
        return 65
    fi
    [[ -f "$STATE/github/$4" ]] || return 10
    print -r -- '{"id":1}'
}
release_build() {
    printf 'release_build %s\n' "$1" >> "$event_log"
    if [[ -f "$STATE/$1.complete" ]]; then
        rm -rf -- "$2" && cp -R "$STATE/$1.retained" "$2" || exit 65
        return 1
    fi
    : > "$STATE/$1.running"
}
release_complete() {
    printf 'release_complete %s\n' "$1" >> "$event_log"
    local -a nonempty=("$2/"*(N.L+0))
    if [[ ! -f "$STATE/$1.running" ]] || (( ! ${#nonempty} )); then exit 65; fi
    rm -rf -- "$STATE/$1.retained" && cp -R "$2" "$STATE/$1.retained" && : > "$STATE/$1.complete" || exit 65
}
go() {
    if [[ "$*" != "-C $BUILD_HOME/sdk/packaging run . release stable" || "$SDK_STABLE_VERSION" != 0.0.0 || "$SDK_RELEASE_TAG" != v0.0.0-123 ]]; then
        printf 'unexpected-go\n' >> "$event_log"
        return 41
    fi
    printf 'go-stable\n' >> "$event_log"
    if [[ "$SDK_PHASE" != stable ]]; then
        print -u2 'panic: unknown SDK release phase'
        return 2
    fi
    local build=$(( $(cat "$STATE/builds" 2>/dev/null || print 0) + 1 )) name
    print -r -- "$build" > "$STATE/builds"
    if [[ "$GO_MODE" != none ]]; then
        for name in ` + sdkStableBundle + ` ` + sdkStableIndex + ` ` + sdkStableArchive + `; do
            printf 'stable %s build %s\n' "$name" "$build" > "$BUILD_HOME/sdk/packaging/release/assets/$name"
        done
    fi
    return "${GO_STATUS:-0}"
}
`

// Record what a run left behind for the assertions.
const sdkStableEpilogue = `print -rl -- "${VIRUSTOTAL_ARTIFACTS[@]/#/row:}" >> "$event_log"
for name in "$BUILD_HOME/sdk/packaging/release/assets/"*(N.:t); do print -r -- "assets-dir:$name"; done >> "$event_log"
for name in "$BUILD_HOME/sdk/packaging/release/stable/"*(N.:t); do print -r -- "stable-dir:$name"; done >> "$event_log"
`

// Run the region `runs` times in one shell: a nightly, then its resumes.
func runSDKStable(t *testing.T, runs int, overrides ...string) (componentResult, string) {
	t.Helper()
	state := t.TempDir()
	source := strings.Repeat(sdkStableRegion(t)+"printf 'run-finished\\n' >> \"$event_log\"\n", runs) + sdkStableEpilogue
	setup := componentFunctions(t, "github_release_upload_staged") + sdkStableSetup
	defaults := []string{"STATE=" + state, "SDK_PHASE=stable", "NIGHTLY_ASSETS=" + sdkNightlyAsset, "INTERRUPTED=", "LEFTOVER_ASSETS=",
		"GO_MODE=", "GO_STATUS=", "FAIL_ASSET=", "REFUSE_ASSET=", "VERDICT_ASSET="}
	return runComponent(t, source, setup, append(defaults, overrides...)...), state
}

func sdkStableAttached(state, name string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(state, "github", name))
	return string(data), err == nil
}

// sdk main has no stable phase yet: one info line per run, no warning, no
// stage, and the nightly's own SDK assets still publish.
func TestRunSDKStableSkipsQuietlyWithoutThePhase(t *testing.T) {
	result, _ := runSDKStable(t, 2, "SDK_PHASE=main")
	if result.exitCode != 0 || strings.Count(result.events, "run-finished\n") != 2 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("a missing stable phase stopped the nightly: %+v", result)
	}
	if strings.Count(result.output, "info: the sdk has no stable release phase") != 2 {
		t.Fatalf("each run must log exactly one info line: %+v", result)
	}
	for _, unexpected := range []string{"message:", "go-stable", "release_build sdk-stable", "panic"} {
		if strings.Contains(result.events+result.output, unexpected) {
			t.Fatalf("a missing stable phase was not skipped quietly (%q): %+v", unexpected, result)
		}
	}
	if strings.Count(result.events, "asset:"+sdkNightlyAsset+"\n") != 1 {
		t.Fatalf("the nightly SDK asset was not published once: %+v", result)
	}
}

// A resumed nightly restores the stage's exact bytes instead of rebuilding
// them, so each asset reconciles with its first upload: nothing is rebuilt,
// refused or uploaded twice, and each resume keeps the asset's table row.
func TestRunSDKStableResumeReusesUploadedBytes(t *testing.T) {
	result, state := runSDKStable(t, 3)
	if result.exitCode != 0 || strings.Count(result.events, "run-finished\n") != 3 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("a resumed nightly failed: %+v", result)
	}
	if strings.Count(result.events, "go-stable\n") != 1 || strings.Count(result.events, "release_build sdk-stable\n") != 3 || strings.Count(result.events, "release_complete sdk-stable\n") != 1 {
		t.Fatalf("a resume rebuilt the completed stable stage: %+v", result)
	}
	if strings.Contains(result.events, "message:") || strings.Contains(result.output, "refused") {
		t.Fatalf("a resume warned or refused: %+v", result)
	}
	for _, name := range append(sdkStableAssets, sdkNightlyAsset) {
		if strings.Count(result.events, "asset:"+name+"\n") != 1 || strings.Count(result.events, "row:|"+name+"|scanned|") != 1 ||
			strings.Count(result.events, "row:|["+name+"](") != 2 {
			t.Fatalf("%s was not uploaded once and then retained: %+v", name, result)
		}
	}
	if got, _ := sdkStableAttached(state, sdkStableBundle); got != "stable "+sdkStableBundle+" build 1\n" {
		t.Fatalf("the release holds %q", got)
	}
}

// The stable files leave release/assets for the stage's own directory, so the
// nightly's loop publishes only the nightly's assets, and as strictly as before.
func TestRunSDKStableKeepsNightlyAssetsStrict(t *testing.T) {
	result, _ := runSDKStable(t, 1)
	if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("the stable stage failed: %+v", result)
	}
	if !strings.Contains(result.events, "assets-dir:"+sdkNightlyAsset+"\n") || strings.Count(result.events, "assets-dir:") != 1 {
		t.Fatalf("release/assets holds more than the nightly's assets: %+v", result)
	}
	for _, name := range sdkStableAssets {
		if !strings.Contains(result.events, "stable-dir:"+name+"\n") || strings.Count(result.events, "asset:"+name+"\n") != 1 {
			t.Fatalf("%s was not staged and attached: %+v", name, result)
		}
	}
	if !strings.Contains(sdkStableRegion(t), `for sdk_package_asset in "$BUILD_HOME/sdk/packaging/release/assets/"*(N.); do
    github_release_upload "$(basename "$sdk_package_asset")" "$sdk_package_asset"
done
`) {
		t.Fatal("the nightly's SDK asset loop changed")
	}
	for _, override := range []string{"FAIL_ASSET=" + sdkNightlyAsset, "REFUSE_ASSET=" + sdkNightlyAsset, "VERDICT_ASSET=" + sdkNightlyAsset} {
		failed, _ := runSDKStable(t, 1, override)
		if failed.exitCode == 0 || strings.Contains(failed.events, "release-continued") || !strings.Contains(failed.events, "asset:"+sdkStableBundle+"\n") {
			t.Fatalf("%s: a nightly SDK asset failure was masked: %+v", override, failed)
		}
	}
}

// Each failure of the stable stage is reported once and the nightly goes on:
// the packaging tool, a refused reconciliation, an upload error and a
// VirusTotal verdict. A failed asset stays off the release and its table.
func TestRunSDKStableFailuresDoNotStopTheNightly(t *testing.T) {
	staged := "warning: not every stable SDK package was staged"
	detached := "warning: stable SDK package asset `" + sdkStableBundle + "` was not attached"
	for _, testCase := range []struct {
		overrides []string
		warning   string
		complete  int
		attached  []string
	}{
		{overrides: []string{"GO_STATUS=2"}, warning: staged, complete: 1, attached: sdkStableAssets},
		{overrides: []string{"GO_STATUS=2", "GO_MODE=none"}, warning: staged, complete: 0},
		{overrides: []string{"REFUSE_ASSET=" + sdkStableBundle}, warning: detached, complete: 1, attached: []string{sdkStableIndex, sdkStableArchive}},
		{overrides: []string{"FAIL_ASSET=" + sdkStableBundle}, warning: detached, complete: 1, attached: []string{sdkStableIndex, sdkStableArchive}},
		{overrides: []string{"VERDICT_ASSET=" + sdkStableBundle}, warning: detached, complete: 1, attached: []string{sdkStableIndex, sdkStableArchive}},
	} {
		result, state := runSDKStable(t, 1, testCase.overrides...)
		if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
			t.Fatalf("%v stopped the nightly: %+v", testCase.overrides, result)
		}
		if strings.Count(result.events, "message:"+testCase.warning) != 1 {
			t.Fatalf("%v was not reported once: %+v", testCase.overrides, result)
		}
		if got := strings.Count(result.events, "release_complete sdk-stable\n"); got != testCase.complete {
			t.Fatalf("%v: stage completed %d times, want %d: %+v", testCase.overrides, got, testCase.complete, result)
		}
		if strings.Count(result.events, "asset:"+sdkNightlyAsset+"\n") != 1 {
			t.Fatalf("%v: the nightly SDK asset was not published: %+v", testCase.overrides, result)
		}
		for _, name := range sdkStableAssets {
			_, attached := sdkStableAttached(state, name)
			listed := strings.Contains(result.events, "row:|"+name+"|")
			want := false
			for _, expected := range testCase.attached {
				want = want || expected == name
			}
			if attached != want || listed != want {
				t.Fatalf("%v: %s attached=%v listed=%v, want %v: %+v", testCase.overrides, name, attached, listed, want, result)
			}
		}
	}
}

// An interrupted attempt leaves stable files in release/assets. The resumed
// stage removes them before it rebuilds, so neither a stale file nor one this
// attempt did not rebuild reaches the release, with or without nightly assets.
func TestRunSDKStableResumeAfterInterruptedAttempt(t *testing.T) {
	stale := "URnetworkSdk-stable-ruby-0.0.0-123.tar.gz"
	for _, nightly := range []string{sdkNightlyAsset, ""} {
		result, state := runSDKStable(t, 1, "INTERRUPTED=1", "NIGHTLY_ASSETS="+nightly, "LEFTOVER_ASSETS="+stale+" "+sdkStableBundle)
		if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
			t.Fatalf("a resume after an interrupted attempt failed: %+v", result)
		}
		if strings.Contains(result.events, stale) {
			t.Fatalf("a stale stable file was kept or published: %+v", result)
		}
		if got, _ := sdkStableAttached(state, sdkStableBundle); got != "stable "+sdkStableBundle+" build 1\n" {
			t.Fatalf("the release holds the interrupted attempt's bundle %q", got)
		}
		if nightly != "" && (strings.Count(result.events, "asset:"+nightly+"\n") != 1 || !strings.Contains(result.events, "assets-dir:"+nightly+"\n")) {
			t.Fatalf("the nightly SDK asset was not kept and published: %+v", result)
		}
	}
}
