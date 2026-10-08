// SPDX-License-Identifier: MPL-2.0

// Exercise release gates from run.sh against hermetic component boundaries.
package allbuild

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type componentResult struct {
	exitCode int
	output   string
	events   string
}

// Locate an exact production region without sourcing the mutating release entrypoint.
func componentRegion(t *testing.T, start, end string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	startIndex := strings.Index(source, start)
	if startIndex < 0 {
		t.Fatalf("missing component start %q", start)
	}
	endIndex := strings.Index(source[startIndex+len(start):], end)
	if endIndex < 0 {
		t.Fatalf("missing component end %q", end)
	}
	return source[startIndex : startIndex+len(start)+endIndex]
}

// Helpers remain production-owned; optional lookup permits the before-fix controls.
func componentFunctions(t *testing.T, names ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var source strings.Builder
	for _, name := range names {
		pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + ` \(\) \{\n.*?^\}\n`)
		matches := pattern.FindAllString(string(data), -1)
		if len(matches) > 1 {
			t.Fatalf("duplicate production helper %q", name)
		}
		if len(matches) == 1 {
			source.WriteString(matches[0])
		}
	}
	return source.String()
}

const componentHarness = `
BUILD_HOME="$1"
WARP_HOME="$BUILD_HOME"
BUILD_OUT="$BUILD_HOME/out"
EXTERNAL_WARP_VERSION=0.0.0-123
WARP_VERSION_CODE=123
GO_MOD_SUFFIX=''
APPLE_API_KEY=synthetic-key
APPLE_API_ISSUER=synthetic-issuer
GITHUB_API_KEY=synthetic-token
GITHUB_UPLOAD_URL=https://upload.example.test/assets
XCODEBUILD_AUTH=()
event_log="$BUILD_HOME/events"
: > "$event_log"
builder_message() { printf 'message\n' >> "$event_log"; }
record_component_step() {
    printf '%s\n' "$1" >> "$event_log"
    if [[ "$FAIL_STEP" == "$1" ]]; then return 37; fi
}
virustotal() { record_component_step scan; }
bug_fix_clean_ipa() { record_component_step ipa-clean; }
xcodebuild() {
    local step=archive
    case "$*" in
        *clean*) step=clean ;;
        *-exportArchive*) step=export ;;
    esac
    record_component_step "$step" || return $?
    if [[ "$step" == export ]]; then
        case "$ARTIFACT_MODE" in
            complete) printf 'new artifact\n' > "build/URnetwork.$ARTIFACT_EXTENSION" ;;
            empty) : > "build/URnetwork.$ARTIFACT_EXTENSION" ;;
        esac
    fi
}
xcrun() {
    local step=validate
    case "$*" in *--upload-app*) step=store-upload ;; esac
    record_component_step "$step"
}
node() { record_component_step generate; }
python3() { record_component_step generate; }
go() {
    if [[ "$#" != 6 || "$1" != -C || "$2" != "$BUILD_HOME/sdk/packaging" || "$3" != run || "$4" != . || "$5" != release || "$6" != desktop ]]; then
        printf 'unexpected-go\n' >> "$event_log"
        return 41
    fi
    record_component_step sdk-package-stage
}
eval "$2"
eval "$3"
printf 'release-continued\n' >> "$event_log"
`

// Run only extracted production code; executable fixtures cannot reach a provider.
func runComponent(t *testing.T, source, setup string, overrides ...string) componentResult {
	t.Helper()
	fixture := t.TempDir()
	for _, directory := range []string{"apple/app/build", "sdk/build", "sdk/cgo/build", "all", "mmm/ur.io", "metadata/en-US/changelogs"} {
		if err := os.MkdirAll(filepath.Join(fixture, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	commands := map[string]string{
		"sdk/build/check_apple_size.sh": "#!/bin/sh\nprintf 'size-check\\n' >> \"$BUILD_HOME/events\"\nif [ \"$FAIL_STEP\" = size-check ]; then exit 37; fi\n",
		"all/github-release-upload.zsh": "#!/bin/sh\nprintf 'publish\\n' >> \"$BUILD_HOME/events\"\nif [ \"$FAIL_STEP\" = publish ]; then exit 37; fi\nprintf 'asset:%s\\n' \"$2\" >> \"$BUILD_HOME/events\"\nprintf '{\"id\":1}'\n",
	}
	for name, contents := range commands {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// eval reports a region cut from inside a block on stderr and the harness
	// carries on as if the component had run, so refuse it before running.
	if output, err := exec.Command("zsh", "-f", "-n", "-c", source).CombinedOutput(); err != nil {
		t.Fatalf("extracted component does not parse: %v\n%s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	functions := componentFunctions(t, "error_trap", "warn_trap", "require_build_artifacts", "require_windows_artifacts", "require_linux_artifacts", "github_release_upload")
	if strings.Contains(source, "sdk_package_stage desktop") {
		stage := componentFunctions(t, "sdk_package_stage")
		if stage == "" {
			t.Fatal("missing required production sdk_package_stage helper")
		}
		functions += stage
	}
	command := exec.CommandContext(ctx, "zsh", "-c", componentHarness, "component-test", fixture, functions+setup, source)
	command.Env = environmentWith(append([]string{
		"BUILD_HOME=" + fixture,
		"FAIL_STEP=",
		"ARTIFACT_MODE=complete",
		"ARTIFACT_EXTENSION=ipa",
		"ARCHES=amd64 arm64",
		"ROLES=daemon gui",
		"ARCH=arm64",
		"WINDOWS_BUILD_ARCHITECTURES=amd64,arm64",
	}, overrides...)...)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("component fixture exceeded its cap: %v", ctx.Err())
	}
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatal(err)
		}
		exitCode = exitError.ExitCode()
	}
	events, err := os.ReadFile(filepath.Join(fixture, "events"))
	if err != nil {
		t.Fatalf("read component events: %v; output=%s", err, output)
	}
	return componentResult{exitCode: exitCode, output: string(output), events: string(events)}
}

// The iOS/macOS chain must stop at each failed build, validation, or publication step.
func TestRunAppleComponentFailuresAreFatal(t *testing.T) {
	appleStart := "(cd $BUILD_HOME/apple/app &&"
	ios := componentRegion(t, appleStart, appleStart)
	macos := strings.TrimPrefix(componentRegion(t, appleStart, "# ============================================================================="), ios)
	for _, testCase := range []struct {
		name      string
		extension string
		source    string
		steps     []string
	}{
		{name: "ios", extension: "ipa", source: ios, steps: []string{"clean", "archive", "size-check", "export", "ipa-clean", "validate", "store-upload", "publish"}},
		// the macOS pkg is the App Store export and is never published to GitHub
		{name: "macos", extension: "pkg", source: macos, steps: []string{"clean", "archive", "export", "validate", "store-upload"}},
	} {
		for _, step := range testCase.steps {
			result := runComponent(t, testCase.source, "", "ARTIFACT_EXTENSION="+testCase.extension, "FAIL_STEP="+step)
			if result.exitCode != 37 || strings.Contains(result.events, "release-continued") {
				t.Fatalf("%s/%s failure was masked: %+v", testCase.name, step, result)
			}
		}
	}
}

// Export success without a new nonempty output must not reuse or publish an old artifact.
func TestRunAppleRejectsMissingEmptyAndStaleArtifacts(t *testing.T) {
	appleStart := "(cd $BUILD_HOME/apple/app &&"
	ios := componentRegion(t, appleStart, appleStart)
	allApple := componentRegion(t, appleStart, "# =============================================================================")
	macos := strings.TrimPrefix(allApple, ios)
	for _, testCase := range []struct {
		extension string
		source    string
	}{
		{extension: "ipa", source: ios},
		{extension: "pkg", source: macos},
	} {
		for _, mode := range []string{"missing", "empty", "stale"} {
			setup := ""
			if mode == "stale" {
				setup = `printf 'old artifact\n' > "$BUILD_HOME/apple/app/build/URnetwork.$ARTIFACT_EXTENSION"` + "\n"
			}
			result := runComponent(t, testCase.source, setup, "ARTIFACT_EXTENSION="+testCase.extension, "ARTIFACT_MODE="+mode)
			if result.exitCode == 0 || strings.Contains(result.events, "validate\n") || strings.Contains(result.events, "publish\n") || strings.Contains(result.events, "release-continued") {
				t.Fatalf("%s/%s export falsely succeeded: %+v", testCase.extension, mode, result)
			}
		}
	}
}

// Healthy Apple artifacts still validate and upload to the store. The ipa publishes
// exactly once; the macOS pkg is the Mac App Store export, which does not launch
// when installed directly, so it is never attached to the GitHub release.
func TestRunAppleRequiredComponentsSucceed(t *testing.T) {
	appleStart := "(cd $BUILD_HOME/apple/app &&"
	ios := componentRegion(t, appleStart, appleStart)
	macos := strings.TrimPrefix(componentRegion(t, appleStart, "# ============================================================================="), ios)
	for _, testCase := range []struct {
		extension string
		source    string
		publishes int
	}{
		{extension: "ipa", source: ios, publishes: 1},
		{extension: "pkg", source: macos, publishes: 0},
	} {
		result := runComponent(t, testCase.source, "", "ARTIFACT_EXTENSION="+testCase.extension)
		if result.exitCode != 0 || strings.Count(result.events, "validate\n") != 1 || strings.Count(result.events, "store-upload\n") != 1 || strings.Count(result.events, "publish\n") != testCase.publishes || !strings.Contains(result.events, "release-continued") {
			t.Fatalf("healthy %s component did not publish %d time(s): %+v", testCase.extension, testCase.publishes, result)
		}
	}
}

// The release-note generator starts with its BUILD_URIO_CHANGELOG gate: a region
// cut from inside that if block does not parse.
const releaseNotesStart = `if [ "${BUILD_URIO_CHANGELOG:-1}" = 1 ]; then
    builder_message "generating the changelog for`

// Every attempted generator is a gate; committed or pending content is not a failed-run stand-in.
func TestRunReleaseInputGenerationFailuresAreFatal(t *testing.T) {
	for _, source := range []string{
		componentRegion(t, "# The per-URL content-date record", "# regenerate every app's strings"),
		componentRegion(t, releaseNotesStart, "# metadata -- THE OTHER THREE STOREFRONTS"),
	} {
		setup := `BUILD_CHANGELOG_STORE="$BUILD_HOME/store.txt"
BUILD_CHANGELOG_FULL="$BUILD_HOME/full.md"
BUILD_NOTES_DIR="$BUILD_HOME/changelogs"
BUILD_NOTES_PREFIX=123
FDROID_VERSION_CODE_OFFSETS='0 2 3'
printf 'pending stand-in\n' > "$BUILD_HOME/metadata/en-US/changelogs/pending.txt"
`
		result := runComponent(t, source, setup, "FAIL_STEP=generate")
		if result.exitCode != 37 || strings.Contains(result.events, "release-continued") {
			t.Fatalf("failed generated release input was treated as optional: %+v", result)
		}
	}
}

// Site metadata is a committed input even when the retired refresh flag is set.
// Only the offline freshness check may run, and a failed check still stops release.
func TestRunUrioMetadataUsesCommittedInputsAndChecksFreshness(t *testing.T) {
	start := "error_trap 'connect ip update'\nfi"
	source := strings.TrimPrefix(componentRegion(t, start, "# The per-URL content-date record"), start)
	setup := `
node() {
    [[ "$*" == react/scripts/check-changelog-fresh.mjs ]] || {
        record_component_step unexpected-metadata-refresh
        return 51
    }
    record_component_step freshness
}
curl() { record_component_step unexpected-api-query; return 52; }
`
	for _, testCase := range []struct {
		name      string
		overrides []string
	}{
		{name: "default", overrides: []string{"BUILD_URIO_SITE_CHANGELOG="}},
		{name: "retired flag enabled", overrides: []string{"BUILD_URIO_SITE_CHANGELOG=1"}},
		{name: "retired flag disabled", overrides: []string{"BUILD_URIO_SITE_CHANGELOG=0"}},
		{name: "release notes disabled", overrides: []string{"BUILD_URIO_SITE_CHANGELOG=", "BUILD_URIO_CHANGELOG=0"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, failure := range []string{"", "freshness"} {
				result := runComponent(t, source, setup, append([]string{"FAIL_STEP=" + failure}, testCase.overrides...)...)
				want, wantExit := "freshness\nrelease-continued\n", 0
				if failure != "" {
					want, wantExit = "freshness\n", 37
				}
				if result.exitCode != wantExit || strings.ReplaceAll(result.events, "message\n", "") != want {
					t.Fatalf("ur.io metadata preflight = %+v, want exit %d and events %q", result, wantExit, want)
				}
			}
		})
	}
}

// A zero-exit generator must still produce all requested nonempty release metadata.
func TestRunChangelogRejectsMissingOutputs(t *testing.T) {
	source := componentRegion(t, releaseNotesStart, "go_mod_edit_module () {")
	setup := `BUILD_CHANGELOG_STORE="$BUILD_HOME/store.txt"
BUILD_CHANGELOG_FULL="$BUILD_HOME/full.md"
BUILD_NOTES_DIR="$BUILD_HOME/changelogs"
BUILD_NOTES_PREFIX=123
FDROID_VERSION_CODE_OFFSETS='0 2 3'
printf 'pending stand-in\n' > "$BUILD_HOME/metadata/en-US/changelogs/pending.txt"
`
	result := runComponent(t, source, setup)
	if result.exitCode == 0 || strings.Contains(result.events, "release-continued") {
		t.Fatalf("successful but output-free changelog generator was accepted: %+v", result)
	}
}

const completeChangelogSetup = `
BUILD_CHANGELOG_STORE="$BUILD_HOME/store.txt"
BUILD_CHANGELOG_FULL="$BUILD_HOME/full.md"
BUILD_NOTES_DIR="$BUILD_HOME/changelogs"
BUILD_NOTES_PREFIX=123
FDROID_VERSION_CODE_OFFSETS='0 2 3'
python3() {
    record_component_step generate || return $?
    mkdir -p "$BUILD_NOTES_DIR"
    local output
    for output in \
        "$BUILD_CHANGELOG_STORE" "$BUILD_CHANGELOG_FULL" \
        "$BUILD_NOTES_DIR/123_Full.md" "$BUILD_NOTES_DIR/123_Simple.txt" \
        "$BUILD_NOTES_DIR/123_Android.txt" "$BUILD_NOTES_DIR/123_Apple.txt" \
        "$BUILD_NOTES_DIR/123_Windows.txt" "$BUILD_NOTES_DIR/123_Linux.xml"
    do
        if [[ "${output:t}" != "$MISSING_NOTE" ]]; then printf 'new note\n' > "$output"; fi
    done
}
`

// Each generated note is independently required; old files cannot satisfy a failed refresh.
func TestRunChangelogRejectsPartialAndStaleNotes(t *testing.T) {
	source := componentRegion(t, `rm -f "$BUILD_CHANGELOG_STORE" "$BUILD_CHANGELOG_FULL"`, "go_mod_edit_module () {")
	for _, note := range []string{"store.txt", "full.md", "123_Full.md", "123_Simple.txt", "123_Android.txt", "123_Apple.txt", "123_Windows.txt", "123_Linux.xml"} {
		setup := completeChangelogSetup + `mkdir -p "$BUILD_NOTES_DIR"
printf 'old note\n' > "$BUILD_NOTES_DIR/$MISSING_NOTE"
`
		result := runComponent(t, source, setup, "MISSING_NOTE="+note)
		if result.exitCode == 0 || strings.Contains(result.events, "release-continued") {
			t.Fatalf("missing/stale %s note was accepted: %+v", note, result)
		}
	}
}

// Generated notes stage successfully, but each attempted copy must preserve failure status.
func TestRunChangelogCopyFailuresAreFatal(t *testing.T) {
	source := componentRegion(t, releaseNotesStart, "go_mod_edit_module () {")
	for _, target := range []string{"125.txt", "default.txt", "release-description.xml"} {
		setup := completeChangelogSetup + `mkdir -p "$BUILD_HOME/linux/app/packaging"
printf 'opted-in description\n' > "$BUILD_HOME/linux/app/packaging/release-description.xml"
cp() {
    if [[ "${@: -1:t}" == "$FAIL_COPY" ]]; then return 43; fi
    command cp "$@"
}
`
		result := runComponent(t, source, setup, "MISSING_NOTE=", "FAIL_COPY="+target)
		if result.exitCode != 43 || strings.Contains(result.events, "release-continued") {
			t.Fatalf("failed %s release-input copy was masked: %+v", target, result)
		}
	}
}

// Healthy metadata and the existing Linux opt-in handshake remain supported.
func TestRunChangelogRequiredInputsSucceed(t *testing.T) {
	source := componentRegion(t, releaseNotesStart, "go_mod_edit_module () {")
	setup := completeChangelogSetup + `
`
	result := runComponent(t, source, setup, "MISSING_NOTE=")
	if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("complete release metadata was rejected: %+v", result)
	}
}

// Nullglob loops must not make absent Windows/Linux output sets look successful.
func TestRunDesktopRejectsMissingOutputs(t *testing.T) {
	source := componentRegion(t, `builder_message "building windows app`, "(cd $BUILD_HOME/android/app &&")
	setup := `mkdir -p "$BUILD_HOME/out/desktop/windows" "$BUILD_HOME/out/desktop/linux"
DESKTOP_OUT="$BUILD_HOME/out/desktop"
printf 'sdk zip\n' > "$BUILD_HOME/sdk/cgo/build/URnetworkSdkWindows.zip"
printf 'sdk zip\n' > "$BUILD_HOME/sdk/cgo/build/URnetworkSdkLinux.zip"
printf '#!/bin/sh\nexit 0\n' > "$BUILD_HOME/all/build-windows.sh"
printf '#!/bin/sh\nexit 0\n' > "$BUILD_HOME/all/build-linux.sh"
mkdir -p "$BUILD_HOME/all/linux"
printf '#!/bin/sh\nexit 0\n' > "$BUILD_HOME/all/linux/build-flatpak.sh"
chmod 700 "$BUILD_HOME/all/build-windows.sh" "$BUILD_HOME/all/build-linux.sh" "$BUILD_HOME/all/linux/build-flatpak.sh"
`
	result := runComponent(t, source, setup)
	if result.exitCode == 0 || strings.Contains(result.events, "release-continued") {
		t.Fatalf("absent desktop artifact set was accepted: %+v", result)
	}
}

const completeDesktopSetup = `
DESKTOP_OUT="$BUILD_HOME/out/desktop"
mkdir -p "$DESKTOP_OUT/windows" "$DESKTOP_OUT/linux" "$BUILD_HOME/all/linux"
printf 'sdk zip\n' > "$BUILD_HOME/sdk/cgo/build/URnetworkSdkWindows.zip"
printf 'sdk zip\n' > "$BUILD_HOME/sdk/cgo/build/URnetworkSdkLinux.zip"
for suffix in x64 arm64; do
    printf 'msi\n' > "$DESKTOP_OUT/windows/URnetwork-$EXTERNAL_WARP_VERSION-$suffix.msi"
done
for architecture in amd64 arm64; do
    case "$architecture" in amd64) package_arch=x86_64 ;; arm64) package_arch=aarch64 ;; esac
    for name in \
        "urnetwork-daemon_${EXTERNAL_WARP_VERSION}_${architecture}.deb" \
        "urnetwork-daemon-${EXTERNAL_WARP_VERSION}-${architecture}.install.tar.gz" \
        "urnetwork-daemon-${EXTERNAL_WARP_VERSION}.${package_arch}.rpm" \
        "urnetwork-daemon-${EXTERNAL_WARP_VERSION}-${package_arch}.pkg.tar.zst" \
        "URnetwork-${EXTERNAL_WARP_VERSION}-${architecture}.AppImage"
    do
        printf 'linux artifact\n' > "$DESKTOP_OUT/linux/$name"
    done
done
printf 'flatpak\n' > "$DESKTOP_OUT/linux/URnetwork-$EXTERNAL_WARP_VERSION-arm64.flatpak"
printf '#!/bin/sh\nprintf "windows-build\\n" >> "$BUILD_HOME/events"\nif [ "$FAIL_STEP" = windows-build ]; then exit 37; fi\n' > "$BUILD_HOME/all/build-windows.sh"
printf '#!/bin/sh\nprintf "linux-build\\n" >> "$BUILD_HOME/events"\nif [ "$FAIL_STEP" = linux-build ]; then exit 37; fi\nif [ "$UR_REQUIRE_RPM" != true ] || [ "$UR_REQUIRE_ARCH_PKG" != true ]; then exit 41; fi\n' > "$BUILD_HOME/all/build-linux.sh"
printf '#!/bin/sh\nprintf "flatpak-build\\n" >> "$BUILD_HOME/events"\nif [ "$FAIL_STEP" = flatpak-build ]; then exit 37; fi\n' > "$BUILD_HOME/all/linux/build-flatpak.sh"
chmod 700 "$BUILD_HOME/all/build-windows.sh" "$BUILD_HOME/all/build-linux.sh" "$BUILD_HOME/all/linux/build-flatpak.sh"
`

// Every expected format and architecture is independently required, not just one observed file.
func TestRunDesktopRejectsPartialAndEmptyArtifactMatrix(t *testing.T) {
	for _, testCase := range []struct {
		path   string
		source string
	}{
		{path: "windows/URnetwork-0.0.0-123-x64.msi", source: `require_windows_artifacts "$DESKTOP_OUT/windows" "$EXTERNAL_WARP_VERSION"`},
		{path: "windows/URnetwork-0.0.0-123-arm64.msi", source: `require_windows_artifacts "$DESKTOP_OUT/windows" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon_0.0.0-123_amd64.deb", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon_0.0.0-123_arm64.deb", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon-0.0.0-123-amd64.install.tar.gz", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon-0.0.0-123-arm64.install.tar.gz", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon-0.0.0-123.x86_64.rpm", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon-0.0.0-123.aarch64.rpm", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon-0.0.0-123-x86_64.pkg.tar.zst", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/urnetwork-daemon-0.0.0-123-aarch64.pkg.tar.zst", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/URnetwork-0.0.0-123-amd64.AppImage", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/URnetwork-0.0.0-123-arm64.AppImage", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
		{path: "linux/URnetwork-0.0.0-123-arm64.flatpak", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`},
	} {
		for _, operation := range []string{`rm -f "$DESKTOP_OUT/$MISSING_ARTIFACT"`, `: > "$DESKTOP_OUT/$MISSING_ARTIFACT"`} {
			setup := completeDesktopSetup + operation + "\n"
			result := runComponent(t, testCase.source+"\nerror_trap completeness", setup, "MISSING_ARTIFACT="+testCase.path)
			if result.exitCode != 1 || strings.Contains(result.events, "release-continued") {
				t.Fatalf("incomplete %s matrix was accepted: %+v", testCase.path, result)
			}
		}
	}
}

// Existing explicitly selected plans remain intact; no new architecture or role is built.
func TestRunDesktopSelectedPlansRemainSupported(t *testing.T) {
	for _, testCase := range []struct {
		setup     string
		source    string
		overrides []string
	}{
		{setup: `rm "$DESKTOP_OUT/windows/URnetwork-$EXTERNAL_WARP_VERSION-x64.msi"` + "\n", source: `require_windows_artifacts "$DESKTOP_OUT/windows" "$EXTERNAL_WARP_VERSION"`, overrides: []string{"WINDOWS_BUILD_ARCHITECTURES=arm64"}},
		{setup: `rm "$DESKTOP_OUT/windows/URnetwork-$EXTERNAL_WARP_VERSION-arm64.msi"` + "\n", source: `require_windows_artifacts "$DESKTOP_OUT/windows" "$EXTERNAL_WARP_VERSION"`, overrides: []string{"WINDOWS_BUILD_ARCHITECTURES=amd64"}},
		{setup: `rm "$DESKTOP_OUT/linux/"*amd64* "$DESKTOP_OUT/linux/"*x86_64*` + "\n", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`, overrides: []string{"ARCHES=arm64"}},
		{setup: `rm "$DESKTOP_OUT/linux/"*.AppImage` + "\n", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`, overrides: []string{"ROLES=daemon"}},
		{setup: `rm "$DESKTOP_OUT/linux/"*.deb "$DESKTOP_OUT/linux/"*.install.tar.gz "$DESKTOP_OUT/linux/"*.rpm "$DESKTOP_OUT/linux/"*.pkg.tar.zst` + "\n", source: `require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"`, overrides: []string{"ROLES=gui"}},
	} {
		result := runComponent(t, testCase.source+"\nerror_trap completeness", completeDesktopSetup+testCase.setup, testCase.overrides...)
		if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
			t.Fatalf("existing selected plan failed: %+v", result)
		}
	}
}

// Required desktop components preserve failure status rather than publishing a partial release.
func TestRunDesktopBuildAndPublicationFailuresAreFatal(t *testing.T) {
	source := componentRegion(t, `builder_message "building windows app`, "(cd $BUILD_HOME/android/app &&")
	steps := []string{"windows-build", "linux-build", "flatpak-build", "publish"}
	if strings.Contains(source, "sdk_package_stage desktop") {
		steps = append(steps, "sdk-package-stage")
	}
	for _, step := range steps {
		result := runComponent(t, source, completeDesktopSetup, "FAIL_STEP="+step)
		if result.exitCode != 37 || strings.Contains(result.events, "release-continued") {
			t.Fatalf("%s failure was masked: %+v", step, result)
		}
		if step == "sdk-package-stage" && (strings.Count(result.events, "sdk-package-stage\n") != 1 || strings.Contains(result.events, "flatpak-build\n") || strings.Count(result.events, "publish\n") != 4) {
			t.Fatalf("package stage failure reached later desktop steps: %+v", result)
		}
	}
}

// A complete release still publishes both desktop matrices, even if ambient strict knobs are false.
func TestRunDesktopRequiredComponentsSucceedWithStrictPackaging(t *testing.T) {
	source := componentRegion(t, `builder_message "building windows app`, "(cd $BUILD_HOME/android/app &&")
	stageCount := 0
	if strings.Contains(source, "sdk_package_stage desktop") {
		stageCount = 1
	}
	result := runComponent(t, source, completeDesktopSetup, "UR_REQUIRE_RPM=false", "UR_REQUIRE_ARCH_PKG=false")
	if result.exitCode != 0 || strings.Count(result.events, "sdk-package-stage\n") != stageCount || strings.Count(result.events, "publish\n") != 15 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("complete desktop artifacts did not publish with strict packaging: %+v", result)
	}
}

// The AppImage embeds no update information (linux/packaging/make-appimage.sh),
// so no .AppImage.zsync is required -- the complete matrix above carries none
// -- and a stray sidecar from an older checkout is never uploaded either: the
// Linux GUI updates through its in-app checker against the stable
// urnetwork/linux releases, and a .zsync on the release page would advertise
// a channel that cannot work (GitHub Releases returns 501 on multi-range).
func TestRunDesktopNeitherRequiresNorPublishesAppImageZsync(t *testing.T) {
	source := componentRegion(t, `builder_message "building windows app`, "(cd $BUILD_HOME/android/app &&")
	setup := completeDesktopSetup + `for architecture in amd64 arm64; do
    printf 'stale\n' > "$DESKTOP_OUT/linux/URnetwork-$EXTERNAL_WARP_VERSION-$architecture.AppImage.zsync"
done
`
	result := runComponent(t, source, setup)
	if result.exitCode != 0 || strings.Count(result.events, "publish\n") != 15 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("a stray .zsync changed the desktop publication: %+v", result)
	}
	if strings.Contains(result.events, ".zsync") {
		t.Fatalf("a .AppImage.zsync was uploaded: %+v", result)
	}
	for _, architecture := range []string{"amd64", "arm64"} {
		if !strings.Contains(result.events, "asset:URnetwork-0.0.0-123-"+architecture+".AppImage\n") {
			t.Fatalf("the %s AppImage itself was not uploaded: %+v", architecture, result)
		}
	}
}

// Publication must reject a missing or empty file before scanning or upload transport.
func TestRunPublicationRejectsMissingAndEmptyArtifacts(t *testing.T) {
	for _, setup := range []string{"", `: > "$BUILD_HOME/empty.zip"` + "\n"} {
		result := runComponent(t, `github_release_upload synthetic.zip "$BUILD_HOME/empty.zip"`, setup)
		if result.exitCode == 0 || strings.Contains(result.events, "scan\n") || strings.Contains(result.events, "publish\n") {
			t.Fatalf("invalid publication artifact reached a provider seam: %+v", result)
		}
	}
}

// IPA cleanup distinguishes absent metadata from failed inspection or mutation.
func TestRunIpaCleanupPropagatesUnzipAndZipFailures(t *testing.T) {
	cleanup := componentFunctions(t, "bug_fix_clean_ipa")
	for _, testCase := range []struct {
		setup string
		code  int
	}{
		{setup: "unzip() { return 31; }\nzip() { return 0; }\n", code: 31},
		{setup: "unzip() { printf '._Symbols/\\n'; }\nzip() { return 33; }\n", code: 33},
		{setup: "unzip() { printf 'Payload/app\\n'; }\nzip() { return 35; }\n", code: 0},
		{setup: "unzip() { printf '._SymbolsLookalike/\\n'; }\nzip() { return 35; }\n", code: 0},
		{setup: "unzip() { printf 'Payload/Symbols/\\n'; }\nzip() { return 35; }\n", code: 0},
		{setup: "unzip() { printf '._Symbols/\\n'; }\nzip() { return 0; }\n", code: 0},
	} {
		result := runComponent(t, "bug_fix_clean_ipa synthetic.ipa\nerror_trap cleanup", cleanup+testCase.setup)
		if result.exitCode != testCase.code {
			t.Fatalf("IPA cleanup status = %d, want %d; output=%s", result.exitCode, testCase.code, result.output)
		}
	}
}

// Real zip/unzip controls prove cleanup removes only top-level AppleDouble metadata.
func TestRunIpaCleanupPreservesPayloadAndGenuineSymbols(t *testing.T) {
	for _, metadata := range []string{"._Symbols", "._Symbols/", "._Symbols/note"} {
		path := filepath.Join(t.TempDir(), "synthetic.ipa")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		writer := zip.NewWriter(file)
		for _, entry := range []string{metadata, "Payload/app", "Symbols/data", "._SymbolsLookalike/data"} {
			entryWriter, err := writer.Create(entry)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(entry, "/") {
				if _, err := entryWriter.Write([]byte("fixture\n")); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		result := runComponent(t, `bug_fix_clean_ipa "$IPA_PATH"`+"\nerror_trap cleanup", componentFunctions(t, "bug_fix_clean_ipa"), "IPA_PATH="+path)
		if result.exitCode != 0 {
			t.Fatalf("real IPA cleanup failed: %+v", result)
		}
		reader, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		entries := map[string]bool{}
		for _, entry := range reader.File {
			entries[entry.Name] = true
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if entries[metadata] || !entries["Payload/app"] || !entries["Symbols/data"] || !entries["._SymbolsLookalike/data"] {
			t.Fatalf("IPA cleanup changed the wrong entries: %v", entries)
		}
	}
}

// Reproducible Android publication chains must not let a final echo mask a failed helper.
func TestRunAndroidVariantPublicationFailuresAreFatal(t *testing.T) {
	variantStart := `(BASE_EXTERNAL_WARP_VERSION="$EXTERNAL_WARP_VERSION" &&`
	first := componentRegion(t, variantStart, variantStart)
	second := strings.TrimPrefix(componentRegion(t, variantStart, "# Warp services"), first)
	setup := `WARP_VERSION_BASE=0.0.0
github_create_draft_release() { record_component_step draft; }
github_release_upload() { record_component_step publish; }
github_create_release() { record_component_step finalize; }
`
	for _, source := range []string{first, second} {
		for _, step := range []string{"draft", "publish", "finalize"} {
			result := runComponent(t, source, setup, "FAIL_STEP="+step)
			if result.exitCode != 37 || strings.Contains(result.events, "release-continued") {
				t.Fatalf("failed Android %s helper was masked: %+v", step, result)
			}
		}
	}
}

// Release policy uses existing strict knobs; noncomponent opt-ins remain explicit.
func TestRunHasNoOptionalReleaseComponents(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, forbidden := range []string{"warn_trap", "soft-fail", "committed changelog as a stand-in", "committed releases as a stand-in", "missing optional"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("run.sh retains optional component policy %q", forbidden)
		}
	}
	for _, required := range []string{
		`UR_REQUIRE_RPM=true UR_REQUIRE_ARCH_PKG=true OUT_DIR="$DESKTOP_OUT/linux" "$BUILD_HOME/all/build-linux.sh"`,
		`if [ "$WARP_SKIP_DEPLOY" = "" ]; then`,
		`if [ "$BUILD_TEST" ]; then`,
		`if [ "$CONNECT_IP_UPDATE" ]; then`,
		`# error_trap 'build proxy http'`,
		`# error_trap 'build proxy wg'`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("run.sh changed a strict-package or existing opt-in boundary %q", required)
		}
	}
}

// =============================================================================
// macos-direct: the Developer ID DMG region (run.sh, after the App Store macOS
// block). Every seam is a recorded stub; nothing reaches Xcode, the notary
// service, the keychain or GitHub.

const macosDirectStart = `MACOS_DIRECT_DMG="URnetwork-${EXTERNAL_WARP_VERSION}-macos.dmg"`

func macosDirectRegion(t *testing.T) string {
	t.Helper()
	return componentRegion(t, macosDirectStart, "# =============================================================================")
}

// The healthy chain, in order, with builder messages filtered out: the app is
// built, notarized, stapled and Gatekeeper-assessed, then published twice --
// the updater zip (ditto --keepParent of the stapled app) first, the DMG
// second -- each upload scanned. "scan" and "publish" therefore occur twice.
var macosDirectSteps = []string{"identity", "profile:download", "profile:extension", "profile:splittunnel", "clean", "archive", "export", "zip", "notarize-app", "staple-app", "assess-app", "zip-asset", "scan", "publish", "asset:URnetwork-0.0.0-123-macos.zip", "stage", "dmg", "sign-dmg", "notarize-dmg", "staple-dmg", "assess-dmg", "scan", "publish", "asset:URnetwork-0.0.0-123-macos.dmg", "release-continued"}

// The synthetic Developer ID Application certificate: the identity stub prints
// its SHA-1 (as security find-identity does) and the healthy profile fixtures
// carry its DER in DeveloperCertificates.
const macosDirectCertificate = "synthetic developer id certificate"

var macosDirectCertificateSha1 = fmt.Sprintf("%X", sha1.Sum([]byte(macosDirectCertificate)))

// Manual signing: the three Developer ID profiles are already installed (plain
// plist fixtures; the security stub "decodes" them by printing them).
// DIRECT_PROFILE_MODE breaks the extension profile: missing, expired,
// wrong-name or wrong-cert.
const macosDirectSetup = `
APPLE_API_KEY_P8="$BUILD_HOME/AuthKey_synthetic-key.p8"
printf 'synthetic p8\n' > "$APPLE_API_KEY_P8"
NOTARY_STATUS="${NOTARY_STATUS:-Accepted}"
MACOS_DIRECT_PROFILE_NAMES=("URnetwork Download" "URnetwork Extension Download" "URnetwork Split Tunnel Download")
MACOS_PROFILES_SOURCE_DIR="$BUILD_HOME/provisionprofiles"
MACOS_PROFILES_INSTALL_DIR="$BUILD_HOME/profiles"
mkdir -p "$MACOS_PROFILES_INSTALL_DIR"
write_profile() {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n<plist version="1.0"><dict><key>Name</key><string>%s</string><key>UUID</key><string>%s</string><key>ExpirationDate</key><date>%s</date><key>DeveloperCertificates</key><array><data>%s</data></array></dict></plist>\n' "$2" "$3" "$4" "$5" > "$MACOS_PROFILES_INSTALL_DIR/$1.provisionprofile"
}
write_profile download "URnetwork Download" 11111111-1111-1111-1111-111111111111 2099-01-01T00:00:00Z SYNTHETIC_CERT_BASE64
write_profile splittunnel "URnetwork Split Tunnel Download" 33333333-3333-3333-3333-333333333333 2099-01-01T00:00:00Z SYNTHETIC_CERT_BASE64
case "${DIRECT_PROFILE_MODE:-healthy}" in
    healthy) write_profile extension "URnetwork Extension Download" 22222222-2222-2222-2222-222222222222 2099-01-01T00:00:00Z SYNTHETIC_CERT_BASE64 ;;
    missing) ;;
    expired) write_profile extension "URnetwork Extension Download" 22222222-2222-2222-2222-222222222222 2020-01-01T00:00:00Z SYNTHETIC_CERT_BASE64 ;;
    wrong-name) write_profile extension "URnetwork Extension" 22222222-2222-2222-2222-222222222222 2099-01-01T00:00:00Z SYNTHETIC_CERT_BASE64 ;;
    wrong-cert) write_profile extension "URnetwork Extension Download" 22222222-2222-2222-2222-222222222222 2099-01-01T00:00:00Z b3RoZXIgY2VydGlmaWNhdGU= ;;
esac
security() {
    if [[ "$1" == cms ]]; then
        record_component_step "profile:$(basename "$4" .provisionprofile)" || return $?
        command cat "$4"
        return
    fi
    record_component_step identity || return $?
    printf '  1) SYNTHETIC_CERT_SHA1 "Developer ID Application: Synthetic (6BGU69Q742)"\n'
}
xcodebuild() {
    local step=archive
    case "$*" in
        *clean*) step=clean ;;
        *-exportArchive*) step=export ;;
    esac
    record_component_step "$step" || return $?
    if [[ "$step" == export ]]; then
        mkdir -p build/direct/URnetwork.app/Contents/MacOS
        case "$DIRECT_APP_MODE" in
            complete) printf 'new app\n' > build/direct/URnetwork.app/Contents/MacOS/URnetwork ;;
            empty) : > build/direct/URnetwork.app/Contents/MacOS/URnetwork ;;
        esac
    fi
}
ditto() {
    if [[ "$1" == -c ]]; then
        # the notarization submission zip, or the updater asset (--keepParent
        # of the stapled app, named URnetwork-<version>-macos.zip)
        case "${@: -1}" in
            *-notarize.zip) record_component_step zip || return $? ;;
            *-macos.zip)
                if [[ "$2 $3" != "-k --keepParent" ]]; then printf 'unexpected-ditto\n' >> "$event_log"; return 41; fi
                record_component_step zip-asset || return $?
                case "$DIRECT_ZIP_MODE" in
                    empty) : > "${@: -1}"; return ;;
                    missing) return ;;
                esac
                ;;
            *) printf 'unexpected-ditto\n' >> "$event_log"; return 41 ;;
        esac
        printf 'zip\n' > "${@: -1}"
    else
        record_component_step stage || return $?
        command cp -R "$1" "$2"
    fi
}
xcrun() {
    local step=unexpected-xcrun
    case "$*" in
        "notarytool submit "*-notarize.zip*) step=notarize-app ;;
        "notarytool submit "*.dmg*) step=notarize-dmg ;;
        "stapler staple "*.app) step=staple-app ;;
        "stapler staple "*.dmg) step=staple-dmg ;;
    esac
    record_component_step "$step" || return $?
    if [[ "$step" == notarize-* ]]; then printf '  id: synthetic\n  status: %s\n' "$NOTARY_STATUS"; fi
}
hdiutil() {
    record_component_step dmg || return $?
    case "$DIRECT_DMG_MODE" in
        complete) printf 'new dmg\n' > "${@: -1}" ;;
        empty) : > "${@: -1}" ;;
    esac
}
codesign() {
    if [[ "$2" != --timestamp || "$3" != --sign || "$4" != SYNTHETIC_CERT_SHA1 ]]; then
        printf 'unexpected-codesign\n' >> "$event_log"
        return 41
    fi
    record_component_step sign-dmg
}
spctl() {
    case "$*" in
        "--assess --type open --context context:primary-signature -vv "*.dmg) record_component_step assess-dmg ;;
        "--assess --type execute -vv "*.app) record_component_step assess-app ;;
        *) printf 'unexpected-spctl\n' >> "$event_log"; return 41 ;;
    esac
}
`

func runMacosDirect(t *testing.T, setup string, overrides ...string) componentResult {
	t.Helper()
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("the macOS direct download profile gate reads profiles with plutil")
	}
	helperNames := []string{"macos_developer_id_identity", "macos_notarize_and_staple", "macos_profile_value", "macos_profile_problem", "macos_profile_signs_with", "macos_require_direct_profiles"}
	helpers := componentFunctions(t, helperNames...)
	for _, name := range helperNames {
		if !strings.Contains(helpers, name+" () {") {
			t.Fatalf("missing required production macOS direct download helper %s", name)
		}
	}
	fixtures := strings.NewReplacer(
		"SYNTHETIC_CERT_BASE64", base64.StdEncoding.EncodeToString([]byte(macosDirectCertificate)),
		"SYNTHETIC_CERT_SHA1", macosDirectCertificateSha1,
	).Replace(macosDirectSetup)
	return runComponent(t, macosDirectRegion(t), helpers+fixtures+setup, append([]string{"DIRECT_APP_MODE=complete", "DIRECT_DMG_MODE=complete", "DIRECT_ZIP_MODE=complete"}, overrides...)...)
}

// How many times `step` occurs in `events`.
func stepCount(events, step string) int {
	return strings.Count("\n"+events, "\n"+step+"\n")
}

// How many times `step` is expected among the healthy steps before `index`.
func stepsBefore(index int, step string) int {
	count := 0
	for _, earlier := range macosDirectSteps[:index] {
		if earlier == step {
			count++
		}
	}
	return count
}

func macosDirectEvents(result componentResult) []string {
	events := []string{}
	for _, event := range strings.Split(strings.TrimSpace(result.events), "\n") {
		if event != "message" && event != "" {
			events = append(events, event)
		}
	}
	return events
}

// A healthy direct build runs every step once, in order, and publishes exactly
// two assets: the updater zip, then the DMG.
func TestRunMacosDirectRequiredComponentsSucceed(t *testing.T) {
	result := runMacosDirect(t, "")
	if result.exitCode != 0 || strings.Join(macosDirectEvents(result), " ") != strings.Join(macosDirectSteps, " ") {
		t.Fatalf("healthy macOS direct download did not run the whole chain once: %+v", result)
	}
	if stepCount(result.events, "publish") != 2 || strings.Contains(result.events, "asset:URnetwork.pkg") {
		t.Fatalf("macOS direct download must publish exactly the zip and the DMG: %+v", result)
	}
	zip := strings.Index(result.events, "asset:URnetwork-0.0.0-123-macos.zip\n")
	dmg := strings.Index(result.events, "asset:URnetwork-0.0.0-123-macos.dmg\n")
	if zip < 0 || dmg < 0 || dmg < zip {
		t.Fatalf("the updater zip must be published before the DMG: %+v", result)
	}
}

// Every build, notarization, packaging, assessment and publication step is
// fatal: nothing later runs, and nothing is published past the failure (the
// zip upload that already happened before a DMG-side failure is the one
// exception the step order allows, and is counted exactly).
func TestRunMacosDirectComponentFailuresAreFatal(t *testing.T) {
	seen := map[string]int{}
	for index, step := range macosDirectSteps {
		occurrence := seen[step]
		seen[step]++
		if step == "scan" || strings.HasPrefix(step, "asset:") || step == "release-continued" {
			continue
		}
		if step == "publish" && occurrence > 0 {
			// the fixture's FAIL_STEP fails the FIRST publish; a second
			// publish failure is the same upload transport
			continue
		}
		result := runMacosDirect(t, "", "FAIL_STEP="+step)
		if result.exitCode == 0 || strings.Contains(result.events, "release-continued") {
			t.Fatalf("%s failure was masked: %+v", step, result)
		}
		if step != "identity" && !strings.HasPrefix(step, "profile:") && result.exitCode != 37 {
			t.Fatalf("%s failure status was rewritten: %+v", step, result)
		}
		publishedBefore := stepsBefore(index, "publish")
		if step == "publish" {
			publishedBefore++
		}
		if stepCount(result.events, "publish") != publishedBefore {
			t.Fatalf("%s failure changed what was published: %+v", step, result)
		}
		for _, later := range macosDirectSteps[index+1:] {
			// the failing step itself is recorded once more than the
			// healthy steps before it
			allowed := stepsBefore(index, later)
			if later == step {
				allowed++
			}
			if stepCount(result.events, later) > allowed {
				t.Fatalf("%s failure did not stop before %s: %+v", step, later, result)
			}
		}
	}
}

// The updater zip is the stapled app archived with --keepParent, gated like
// every other artifact: a zero-exit ditto without a new nonempty zip publishes
// nothing, and a stale zip from an earlier run is removed up front.
func TestRunMacosDirectRejectsMissingEmptyAndStaleZip(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "stale"} {
		zipMode := mode
		setup := ""
		if mode == "stale" {
			zipMode = "missing"
			setup = `printf 'old zip\n' > "$BUILD_HOME/apple/app/build/URnetwork-0.0.0-123-macos.zip"` + "\n"
		}
		result := runMacosDirect(t, setup, "DIRECT_ZIP_MODE="+zipMode)
		if result.exitCode == 0 || strings.Contains(result.events, "publish\n") || strings.Contains(result.events, "stage\n") || strings.Contains(result.events, "release-continued") {
			t.Fatalf("%s updater zip falsely succeeded: %+v", mode, result)
		}
		if !strings.Contains(result.events, "assess-app\n") || !strings.Contains(result.events, "zip-asset\n") {
			t.Fatalf("%s updater zip failed before the app was assessed and zipped: %+v", mode, result)
		}
	}
}

// A notary verdict other than Accepted fails even when notarytool exits 0, before stapling.
func TestRunMacosDirectRejectsUnacceptedNotarization(t *testing.T) {
	result := runMacosDirect(t, "", "NOTARY_STATUS=Invalid")
	if result.exitCode == 0 || strings.Contains(result.events, "staple-app\n") || strings.Contains(result.events, "publish\n") || strings.Contains(result.events, "release-continued") {
		t.Fatalf("unaccepted notarization was treated as success: %+v", result)
	}
}

// Without the App Store Connect API key file, nothing is submitted for notarization.
func TestRunMacosDirectRequiresNotaryApiKey(t *testing.T) {
	result := runMacosDirect(t, `APPLE_API_KEY_P8=""`+"\n")
	if result.exitCode == 0 || strings.Contains(result.events, "notarize-app\n") || strings.Contains(result.events, "publish\n") || strings.Contains(result.events, "release-continued") {
		t.Fatalf("missing notary API key was masked: %+v", result)
	}
}

// Without a Developer ID Application identity, nothing is built.
func TestRunMacosDirectRequiresDeveloperIdIdentity(t *testing.T) {
	result := runMacosDirect(t, `security() { record_component_step identity; printf '  1) FFFF "Apple Distribution: Synthetic (6BGU69Q742)"\n'; }`+"\n")
	if result.exitCode == 0 || strings.Contains(result.events, "clean\n") || strings.Contains(result.events, "release-continued") {
		t.Fatalf("missing Developer ID Application identity was masked: %+v", result)
	}
}

// Manual signing: without every Developer ID profile installed, valid and
// issued for the signing certificate, nothing is built.
func TestRunMacosDirectRequiresDeveloperIdProfiles(t *testing.T) {
	for _, mode := range []string{"missing", "expired", "wrong-name", "wrong-cert"} {
		result := runMacosDirect(t, "", "DIRECT_PROFILE_MODE="+mode)
		if result.exitCode == 0 || strings.Contains(result.events, "clean\n") || strings.Contains(result.events, "publish\n") || strings.Contains(result.events, "release-continued") {
			t.Fatalf("%s extension profile was masked: %+v", mode, result)
		}
		if !strings.Contains(result.events, "identity\n") || !strings.Contains(result.events, "profile:download\n") {
			t.Fatalf("%s extension profile failed before the identity/profile gate ran: %+v", mode, result)
		}
	}
}

// A zero-exit export or DMG step without a new nonempty output must not notarize,
// package or publish an old artifact.
func TestRunMacosDirectRejectsMissingEmptyAndStaleArtifacts(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "stale"} {
		appMode, dmgMode := mode, mode
		setup := ""
		if mode == "stale" {
			appMode, dmgMode = "missing", "missing"
			setup = `mkdir -p "$BUILD_HOME/apple/app/build/direct/URnetwork.app/Contents/MacOS"
printf 'old app\n' > "$BUILD_HOME/apple/app/build/direct/URnetwork.app/Contents/MacOS/URnetwork"
printf 'old dmg\n' > "$BUILD_HOME/apple/app/build/URnetwork-0.0.0-123-macos.dmg"
`
		}
		app := runMacosDirect(t, setup, "DIRECT_APP_MODE="+appMode)
		if app.exitCode == 0 || strings.Contains(app.events, "zip\n") || strings.Contains(app.events, "notarize-app\n") || strings.Contains(app.events, "publish\n") || strings.Contains(app.events, "release-continued") {
			t.Fatalf("%s app export falsely succeeded: %+v", mode, app)
		}
		// the updater zip (published before the DMG is made) is the one
		// publish allowed here; the DMG itself never goes out
		dmg := runMacosDirect(t, setup, "DIRECT_DMG_MODE="+dmgMode)
		if dmg.exitCode == 0 || strings.Contains(dmg.events, "sign-dmg\n") || strings.Contains(dmg.events, "notarize-dmg\n") || stepCount(dmg.events, "publish") != 1 || strings.Contains(dmg.events, "asset:URnetwork-0.0.0-123-macos.dmg") || strings.Contains(dmg.events, "release-continued") {
			t.Fatalf("%s dmg creation falsely succeeded: %+v", mode, dmg)
		}
	}
}
