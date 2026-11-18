// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The command run.sh's smtpedge stage reads the published image's file capability with (QD-1066, owner decision
// 597; sidecar DESIGN §19.4): server/cli/smtpedge's filecap helper, over the image the receipt names.
const smtpEdgeCapabilityCheck = `(cd "$BUILD_HOME/server${GO_MOD_SUFFIX}/cli/smtpedge" &&
    GOWORK=off GOFLAGS= go run ./filecap/cmd check-image "bringyour/${BUILD_ENV}-smtpedge:${EXTERNAL_WARP_VERSION}")
error_trap 'smtpedge image file capability'`

// run.sh's smtpedge image stage, from its release_build to its availability message.
func smtpEdgeImageStage(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "if release_build image-smtpedge ")
	end := strings.Index(source, "builder_message \"service smtpedge \\`${EXTERNAL_WARP_VERSION}\\` available\"\n")
	if start < 0 || end < start {
		t.Fatalf("run.sh has no smtpedge image stage: %d, %d", start, end)
	}
	return source[start : end+strings.Index(source[end:], "\n")+1]
}

// The image is checked after it is built and before the release records it: the build, the check and its
// trap, then the publication's completion, with the image the receipt reads.
func TestRunChecksTheSmtpEdgeImagesFileCapability(t *testing.T) {
	stage := smtpEdgeImageStage(t)
	buildAt := strings.Index(stage, "(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/cli/smtpedge/Makefile)\nerror_trap 'warpctl build smtpedge'\n")
	checkAt := strings.Index(stage, smtpEdgeCapabilityCheck+"\n")
	completeAt := strings.Index(stage, "release_image_publication_complete smtpedge\n")
	if buildAt < 0 || checkAt < buildAt || completeAt < checkAt || strings.Count(stage, "check-image") != 1 {
		t.Fatalf("the smtpedge stage does not build, check, then record: build %d, check %d, complete %d", buildAt, checkAt, completeAt)
	}
	checkpoints, err := os.ReadFile(filepath.Join(rolloutRoot(t), "release-checkpoints.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checkpoints), `docker buildx imagetools inspect --raw "bringyour/${BUILD_ENV}-${service}:${EXTERNAL_WARP_VERSION}"`) {
		t.Fatal("the receipt reads another image than the check")
	}
}

// Run the stage itself in zsh with the release's helpers, warpctl and go answering from the test: a check that
// fails stops the release with its trap before the image is recorded, and one that passes lets it go on.
func TestRunStopsAtAnSmtpEdgeImageWithoutItsFileCapability(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("no zsh here")
	}
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	trap := regexp.MustCompile(`(?ms)^error_trap \(\) \{\n.*?\n\}\n`).FindString(string(data))
	if trap == "" {
		t.Fatal("run.sh has no error_trap")
	}
	harness := `set -u
events="$1"
record() { print -r -- "$*" >> "$events" }
builder_message() { record message "$@" }
release_build() { record release_build "$1"; return 0 }
release_image_publication_begin() { record begin "$1" }
release_image_publication_complete() { record complete "$1" }
release_complete() { record release_complete "$1" }
release_image_receipt() { record receipt "$1" }
warpctl() { record warpctl "$@" }
go() { record go "$PWD" "GOWORK=$GOWORK" "GOFLAGS=${GOFLAGS-unset}" "$@"; return "$CHECK_EXIT" }
` + trap + smtpEdgeImageStage(t)

	for _, checkExit := range []string{"0", "1"} {
		home := t.TempDir()
		smtpedge := filepath.Join(home, "server", "cli", "smtpedge")
		if err := os.MkdirAll(smtpedge, 0755); err != nil {
			t.Fatal(err)
		}
		events := filepath.Join(home, "events")
		command := exec.CommandContext(t.Context(), "zsh", "-c", harness, "harness", events)
		command.Env = []string{
			"PATH=" + os.Getenv("PATH"), "BUILD_HOME=" + home, "WARP_HOME=" + home, "BUILD_ENV=main", "GO_MOD_SUFFIX=",
			"EXTERNAL_WARP_VERSION=2026.11.18-1", "GOFLAGS=-trimpath", "CHECK_EXIT=" + checkExit,
		}
		output, runErr := command.CombinedOutput()
		raw, err := os.ReadFile(events)
		if err != nil {
			t.Fatalf("exit %s: %v: %s", checkExit, err, output)
		}
		got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		want := []string{
			"release_build image-smtpedge",
			"begin smtpedge",
			"warpctl build main server/cli/smtpedge/Makefile",
			"go " + smtpedge + " GOWORK=off GOFLAGS= run ./filecap/cmd check-image bringyour/main-smtpedge:2026.11.18-1",
		}
		if checkExit == "0" {
			want = append(want,
				"complete smtpedge",
				"release_complete image-smtpedge",
				"receipt smtpedge",
				"message service smtpedge `2026.11.18-1` available")
			if runErr != nil {
				t.Errorf("a passing check failed the stage: %v: %s", runErr, output)
			}
		} else {
			want = append(want, "message error(1): smtpedge image file capability")
			if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
				t.Errorf("a failing check answered %v: %s", runErr, output)
			}
		}
		// the stage's paths resolve through the temp dir's real path on hosts whose temp dir is a link
		resolved, err := filepath.EvalSymlinks(smtpedge)
		if err != nil {
			t.Fatal(err)
		}
		for i := range got {
			got[i] = strings.Replace(got[i], resolved, smtpedge, 1)
		}
		if !slices.Equal(got, want) {
			t.Errorf("check exit %s: ran %q, want %q", checkExit, got, want)
		}
	}
}
