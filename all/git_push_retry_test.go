// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Read the production helper itself so the behavioral tests cannot drift onto
// a test-only copy of its retry logic.
func pushRetryFunction(t *testing.T) string {
	t.Helper()
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?ms)^git_push_with_rebase_retry \(\) \{\n.*?^\}\n`)
	matches := pattern.FindAllString(string(runData), -1)
	if len(matches) != 1 {
		t.Fatalf("run.sh push retry function count = %d, want 1", len(matches))
	}
	return matches[0]
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func commitTestFile(t *testing.T, repository, name, contents, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repository, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", name)
	runGit(
		t,
		repository,
		"-c", "user.name=Build Harness Test",
		"-c", "user.email=build-harness-test@example.invalid",
		"commit", "-m", message,
	)
	return runGit(t, repository, "rev-parse", "HEAD")
}

// Reproduce the production failure with a local bare origin: the release clone
// commits while another clone advances main, so its first push must be rejected.
// The exact run.sh helper must rebase the generated change and push both.
func TestGitPushWithRebaseRetryIntegratesConcurrentMainCommit(t *testing.T) {
	tempDir := t.TempDir()
	remote := filepath.Join(tempDir, "remote.git")
	seed := filepath.Join(tempDir, "seed")
	release := filepath.Join(tempDir, "release")
	concurrent := filepath.Join(tempDir, "concurrent")

	runGit(t, tempDir, "init", "--bare", remote)
	runGit(t, tempDir, "init", "-b", "main", seed)
	commitTestFile(t, seed, "base", "base\n", "base")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, tempDir, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, tempDir, "clone", remote, release)
	runGit(t, tempDir, "clone", remote, concurrent)

	commitTestFile(t, release, "generated", "release\n", "generated release pin")
	concurrentCommit := commitTestFile(t, concurrent, "concurrent", "remote\n", "concurrent main change")
	runGit(t, concurrent, "push")

	command := exec.Command("zsh", "-c", `eval "$1"; git_push_with_rebase_retry`, "push-retry-test", pushRetryFunction(t))
	command.Dir = release
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("race-safe push failed: %v\n%s", err, output)
	}
	runGit(t, release, "fetch", "origin", "main")
	ancestorCommand := exec.Command("git", "merge-base", "--is-ancestor", concurrentCommit, "origin/main")
	ancestorCommand.Dir = release
	if output, err := ancestorCommand.CombinedOutput(); err != nil {
		t.Fatalf("concurrent commit was dropped: %v\n%s", err, output)
	}
	for _, name := range []string{"base", "generated", "concurrent"} {
		if _, err := os.Stat(filepath.Join(release, name)); err != nil {
			t.Fatalf("integrated checkout lacks %s: %v", name, err)
		}
	}
}

// A second failure must be returned after exactly two push attempts. This keeps
// the automatic recovery bounded instead of hiding a persistent remote error.
func TestGitPushWithRebaseRetryStopsAfterOneRetry(t *testing.T) {
	eventLog := filepath.Join(t.TempDir(), "events")
	harness := `
set -u
event_log="$1"
: > "$event_log"
push_count=0
git() {
    printf '%s\n' "$*" >> "$event_log"
    if [[ "$1" = push ]]; then
        (( push_count += 1 ))
        if (( push_count == 1 )); then
            return 41
        fi
        return 43
    fi
    if [[ "$1" = pull ]]; then
        return 0
    fi
    return 99
}
eval "$2"
git_push_with_rebase_retry
`
	command := exec.Command("zsh", "-c", harness, "push-retry-test", eventLog, pushRetryFunction(t))
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 43 {
		t.Fatalf("retry exit = %v, want 43; output=%s", err, output)
	}
	events, readErr := os.ReadFile(eventLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	want := "push\npull --rebase\npush\n"
	if string(events) != want {
		t.Fatalf("retry events = %q, want %q", events, want)
	}
}

// Pin every build-repository release mutation to the tested bounded helper,
// including the ungoogle pin that exposed the race in production.
func TestRunUsesRaceSafeBuildRepositoryPushes(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSource := string(runData)
	for _, required := range []string{
		`git commit -m "${EXTERNAL_WARP_VERSION}" &&
    git_push_with_rebase_retry &&
    git_tag`,
		`git commit -m "$HOST build ungoogle" &&
    git_push_with_rebase_retry)`,
		`git commit -m "${EXTERNAL_WARP_VERSION} restore android pin" &&
                git_push_with_rebase_retry)`,
	} {
		if !strings.Contains(runSource, required) {
			t.Errorf("run.sh lacks race-safe build repository push block %q", required)
		}
	}
}
