// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// F-Droid tracks v<version>-fdroid tags. Exercise the production helper
// against a local origin: the tag must peel to the -ungoogle commit (not the
// branch head), and a second publication must fail without moving it.
func TestGitTagFdroidAliasesUngoogleCommit(t *testing.T) {
	tempDir := t.TempDir()
	remote := filepath.Join(tempDir, "remote.git")
	repository := filepath.Join(tempDir, "build")
	version := "2026.9.7-9999999990"
	runGit(t, tempDir, "init", "--bare", remote)
	runGit(t, tempDir, "init", "-b", "main", repository)
	runGit(t, repository, "config", "user.name", "Build Harness Test")
	runGit(t, repository, "config", "user.email", "build-harness-test@example.invalid")
	commitTestFile(t, repository, "release", "release\n", "release tree")
	runGit(t, repository, "remote", "add", "origin", remote)
	runGit(t, repository, "push", "-u", "origin", "main")
	ungoogleCommit := commitTestFile(t, repository, "ungoogle", "ungoogle\n", "builder build ungoogle")
	runGit(t, repository, "tag", "-a", "v"+version+"-ungoogle", "-m", version+"-ungoogle")
	runGit(t, repository, "push", "origin", "refs/tags/v"+version+"-ungoogle")
	// the android pin is restored after the ungoogle build, moving main on
	commitTestFile(t, repository, "restore", "restore\n", "restore android pin")

	harness := `
set -u
EXTERNAL_WARP_VERSION="$1"
builder_message() { print -r -- "$*" }
eval "$2"
git_tag_fdroid
`
	run := func() (string, error) {
		command := exec.Command("zsh", "-c", harness, "fdroid-tag-test", version, runFunction(t, "git_tag_fdroid"))
		command.Dir = repository
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if output, err := run(); err != nil {
		t.Fatalf("fdroid tag publication failed: %v\n%s", err, output)
	}
	tagRef := "refs/tags/v" + version + "-fdroid"
	remoteCommit := runGit(t, tempDir, "--git-dir", remote, "rev-parse", tagRef+"^{commit}")
	if remoteCommit != ungoogleCommit {
		t.Fatalf("fdroid tag peels to %s, want the -ungoogle commit %s", remoteCommit, ungoogleCommit)
	}
	if objectType := runGit(t, tempDir, "--git-dir", remote, "cat-file", "-t", tagRef); objectType != "tag" {
		t.Fatalf("fdroid tag object type = %q, want an annotated tag", objectType)
	}

	before := runGit(t, repository, "ls-remote", "--tags", "origin", tagRef, tagRef+"^{}")
	output, err := run()
	if err == nil || !strings.Contains(output, "refusing to overwrite") {
		t.Fatalf("second fdroid tag publication exit = %v, output=%q", err, output)
	}
	if after := runGit(t, repository, "ls-remote", "--tags", "origin", tagRef, tagRef+"^{}"); after != before {
		t.Fatalf("published fdroid tag moved:\nbefore: %s\nafter:  %s", before, after)
	}
}

// The -fdroid tag is the only tag F-Droid should build from, so it must be
// pushed after the last APK release it verifies against, and never before a
// build or upload that can still fail.
func TestRunPushesFdroidTagAfterApkReleases(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(runData)

	call := "(cd $BUILD_HOME &&\n    git_tag_fdroid)\nerror_trap 'push fdroid tag'"
	if got := strings.Count(source, call); got != 1 {
		t.Fatalf("run.sh fdroid tag publication count = %d, want 1", got)
	}
	tagIndex := strings.Index(source, call)
	for _, before := range []string{
		"error_trap 'push ungoogle tag'",
		"error_trap 'android github build'",
		`"com.bringyour.network-${EXTERNAL_WARP_VERSION}.apk"`,
		"\ngithub_create_release\n",
		"error_trap 'android github armeabi-v7a reproducible pre-release'",
		"error_trap 'android github arm64-v8a reproducible pre-release'",
	} {
		index := strings.Index(source, before)
		if index < 0 {
			t.Fatalf("run.sh is missing release step %q", before)
		}
		if index > tagIndex {
			t.Errorf("fdroid tag is pushed before release step %q", before)
		}
	}
	sectionEnd := strings.Index(source[tagIndex:], "# Warp services")
	if sectionEnd < 0 {
		t.Fatal("run.sh has no Warp services section after the fdroid tag")
	}
	androidRest := source[tagIndex : tagIndex+sectionEnd]
	for _, after := range []string{"github_release_upload", "github_create_release", "gradlew"} {
		if strings.Contains(androidRest, after) {
			t.Errorf("run.sh still runs %q after pushing the fdroid tag", after)
		}
	}
}
