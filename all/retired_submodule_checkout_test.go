// SPDX-License-Identifier: MPL-2.0

// Behavioral tests for run.sh's removal of a retired submodule's leftover
// checkout, run against local repositories only.
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

// Reads the production helper itself so the tests cannot drift onto a
// test-only copy of it.
func retiredSubmoduleCheckoutFunction(t *testing.T) string {
	t.Helper()
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?ms)^remove_retired_submodule_checkouts \(\) \{\n.*?^\}\n`)
	matches := pattern.FindAllString(string(runData), -1)
	if len(matches) != 1 {
		t.Fatalf("run.sh retired submodule function count = %d, want 1", len(matches))
	}
	return matches[0]
}

// Runs the helper on buildHome the way run.sh does, with builder_message
// printing instead of posting.
func removeRetiredSubmoduleCheckouts(t *testing.T, buildHome string) (string, error) {
	t.Helper()
	harness := `
set -u
builder_message () {
    print -r -- "$1"
}
eval "$1"
remove_retired_submodule_checkouts "$2"
`
	command := exec.Command("zsh", "-c", harness, "retired-submodule-test", retiredSubmoduleCheckoutFunction(t), buildHome)
	output, err := command.CombinedOutput()
	return string(output), err
}

// Runs a git command with a fixed test identity and file-protocol submodule
// clones allowed.
func runSubmoduleGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	return runGit(t, directory, append([]string{
		"-c", "protocol.file.allow=always",
		"-c", "user.name=Build Harness Test",
		"-c", "user.email=build-harness-test@example.invalid",
	}, args...)...)
}

// Reproduces the build host after a submodule was dropped upstream: the build
// clone had live and retired initialized, then its pull removed retired from
// the index and .gitmodules but left the populated checkout behind. Returns
// the build clone and the upstream seed checkout.
func newRetiredSubmoduleBuildHome(t *testing.T) (string, string) {
	t.Helper()
	tempDir := t.TempDir()
	for _, name := range []string{"live", "retired"} {
		source := filepath.Join(tempDir, name+"-source")
		runGit(t, tempDir, "init", "-b", "main", source)
		commitTestFile(t, source, "README", name+"\n", name+" base")
		runGit(t, tempDir, "clone", "--bare", source, filepath.Join(tempDir, name+".git"))
	}
	remote := filepath.Join(tempDir, "build.git")
	seed := filepath.Join(tempDir, "seed")
	buildHome := filepath.Join(tempDir, "build")
	runGit(t, tempDir, "init", "--bare", remote)
	runGit(t, tempDir, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, tempDir, "init", "-b", "main", seed)
	for _, name := range []string{"live", "retired"} {
		runSubmoduleGit(t, seed, "submodule", "add", filepath.Join(tempDir, name+".git"), name)
	}
	runSubmoduleGit(t, seed, "commit", "-m", "add submodules")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "-u", "origin", "main")

	runGit(t, tempDir, "clone", remote, buildHome)
	runSubmoduleGit(t, buildHome, "submodule", "update", "--init", "--recursive")

	runSubmoduleGit(t, seed, "rm", "retired")
	runSubmoduleGit(t, seed, "commit", "-m", "retire a submodule")
	runGit(t, seed, "push")
	runSubmoduleGit(t, buildHome, "pull", "--rebase")
	runSubmoduleGit(t, buildHome, "submodule", "update", "--init", "--recursive")
	if _, err := os.Stat(filepath.Join(buildHome, "retired", ".git")); err != nil {
		t.Fatalf("pull removed the retired checkout itself, so this is no reproduction: %v", err)
	}
	return buildHome, seed
}

// The release commit's `git add .` must not record the retired checkout again
// as a gitlink with no .gitmodules entry; that broke every later `git
// submodule update --init` when elements was dropped.
func TestRemoveRetiredSubmoduleCheckoutsKeepsTheReleaseCommitFromReaddingIt(t *testing.T) {
	buildHome, _ := newRetiredSubmoduleBuildHome(t)
	gitDir := runGit(t, buildHome, "rev-parse", "--absolute-git-dir")
	if output, err := removeRetiredSubmoduleCheckouts(t, buildHome); err != nil {
		t.Fatalf("removal failed: %v\n%s", err, output)
	}
	runGit(t, buildHome, "add", ".")
	if staged := runGit(t, buildHome, "ls-files", "--stage", "--", "retired"); staged != "" {
		t.Fatalf("release commit would record the retired submodule again: %s", staged)
	}
	if _, err := os.Stat(filepath.Join(buildHome, "retired")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired checkout still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "modules", "retired", "HEAD")); err != nil {
		t.Fatalf("retired git directory was not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(buildHome, "live", "README")); err != nil {
		t.Fatalf("live submodule checkout was touched: %v", err)
	}
	if status := runGit(t, buildHome, "status", "--porcelain"); status != "" {
		t.Fatalf("build clone not clean after removal: %q", status)
	}
	runSubmoduleGit(t, buildHome, "commit", "--allow-empty", "-m", "release")
	freshClone := filepath.Join(t.TempDir(), "fresh")
	runGit(t, filepath.Dir(freshClone), "clone", buildHome, freshClone)
	runSubmoduleGit(t, freshClone, "submodule", "update", "--init", "--recursive")
}

// Local changes in a retired checkout are a person's to keep or discard: the
// helper fails and leaves the checkout as it was.
func TestRemoveRetiredSubmoduleCheckoutsRefusesLocalChanges(t *testing.T) {
	buildHome, _ := newRetiredSubmoduleBuildHome(t)
	edited := filepath.Join(buildHome, "retired", "README")
	if err := os.WriteFile(edited, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := removeRetiredSubmoduleCheckouts(t, buildHome)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("removal exit = %v, want 1; output=%s", err, output)
	}
	if !strings.Contains(output, "has local changes") {
		t.Fatalf("refusal does not say why: %q", output)
	}
	contents, readErr := os.ReadFile(edited)
	if readErr != nil || string(contents) != "local edit\n" {
		t.Fatalf("local change was not kept: %q, %v", contents, readErr)
	}
}

// Only a checkout of this repository's own dropped submodule goes: a nested
// repository with its own .git directory and a linked worktree of another
// repository are not retired submodules and stay.
func TestRemoveRetiredSubmoduleCheckoutsLeavesOtherNestedRepositories(t *testing.T) {
	buildHome, seed := newRetiredSubmoduleBuildHome(t)
	standalone := filepath.Join(buildHome, "standalone")
	runGit(t, buildHome, "init", "-b", "main", standalone)
	commitTestFile(t, standalone, "README", "standalone\n", "standalone base")
	linked := filepath.Join(buildHome, "linked")
	runGit(t, seed, "worktree", "add", "--detach", linked)

	if output, err := removeRetiredSubmoduleCheckouts(t, buildHome); err != nil {
		t.Fatalf("removal failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(buildHome, "retired")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("retired checkout still present: %v", err)
	}
	for _, path := range []string{
		filepath.Join(standalone, "README"),
		filepath.Join(linked, ".gitmodules"),
		filepath.Join(buildHome, "live", "README"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
}

// The removal has to run after the build repository's own pull and before
// anything commits in it, and its failure has to stop the release.
func TestRunRemovesRetiredSubmoduleCheckoutsAfterTheBuildRepositoryPull(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSource := string(runData)
	pullAndRemoval := `    git pull --rebase &&
    git submodule update --init --recursive)
error_trap 'pull'
remove_retired_submodule_checkouts "$BUILD_HOME"
error_trap 'remove retired submodule checkouts'
BUILD_COMMIT=`
	if strings.Count(runSource, pullAndRemoval) != 1 {
		t.Fatalf("run.sh does not remove retired submodule checkouts right after the build repository pull")
	}
	releaseCommit := `(cd $BUILD_HOME &&
    git add . &&`
	releaseIndex := strings.Index(runSource, releaseCommit)
	if releaseIndex < 0 {
		t.Fatalf("run.sh lacks the build repository release commit")
	}
	if strings.Index(runSource, pullAndRemoval) > releaseIndex {
		t.Fatalf("run.sh removes retired submodule checkouts after the release commit")
	}
}
