// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const urioTestReleaseVersion = "2026.10.13-1071358670"

// Representative outputs of ur.io/astro's required preflight generators. The
// release must record these alongside the dates, including the docs/API mirrors,
// examples and stats that were left unstaged in the failed 2026-10-13 release.
func urioGeneratedFiles() []string {
	files := []string{
		"ur.io/api/bringyour.yml",
		"ur.io/react/src/data/blog-media-accessibility.js",
		"ur.io/react/src/data/content-dates.js",
		"ur.io/react/src/data/page-dates.js",
		"ur.io/react/src/data/docs.js",
		"ur.io/react/src/data/docs-api-groups.js",
		"ur.io/react/src/data/docs-assets.js",
		"ur.io/react/src/data/docs-seo.js",
		"ur.io/react/src/data/docs-i18n/es.js",
		"ur.io/react/src/data/openapi.js",
		"ur.io/react/src/data/examples.js",
		"ur.io/react/src/data/networkStats.js",
		"ur.io/react/src/data/legal.js",
		"ur.io/react/src/data/pricing.js",
		"ur.io/react/src/i18n/app/en.json",
		"ur.io/react/src/styles/fonts.generated.css",
	}
	for _, renderer := range []string{"react", "astro"} {
		for _, name := range []string{
			"docs-assets/diagram.svg", "docs-md/api.md", "docs-md/guide.md",
			"blog-md/article.md", "fonts/subsets/Geist-Regular.base.woff2",
			"outages-md/example.md", "about.md", "agents.md", "buy-data.md",
			"changelog.md", "index.md", "install.md", "outages.md", "products.md",
			"llms.txt", "llms-full.txt", "openapi.yml",
			"blog/rss.xml", "ar/blog/rss.xml", "de/blog/rss.xml",
			"es/blog/rss.xml", "ru/blog/rss.xml", "zh/blog/rss.xml",
			"images/home-hero-768.webp", "images/home-hero-1024.webp",
			"images/home-hero-1440.webp", "images/home-hero-2048.webp",
		} {
			files = append(files, "ur.io/"+renderer+"/public/"+name)
		}
	}
	return files
}

// None of these sources is owned by the preflight generators, even though
// several sit beside their outputs. Changelog/release metadata is maintained
// separately; original media and hand-written code are not release staging areas.
var urioUnrelatedFiles = []string{
	"ur.io/react/src/data/changelog.js",
	"ur.io/react/src/data/changelog-version.js",
	"ur.io/react/src/data/releases.js",
	"ur.io/react/src/data/networkStatsFeed.js",
	"ur.io/react/src/components/Footer.jsx",
	"ur.io/react/src/i18n/en.json",
	"ur.io/react/src/styles/fonts.css",
	"ur.io/docs/GUIDE.md",
	"ur.io/react/public/images/home-hero.webp",
	"ur.io/react/public/fonts/Geist-Regular.woff2",
	"ur.io/astro/public/.well-known/security.txt",
}

func writeUrioTestFile(t *testing.T, repository, name, contents string) {
	t.Helper()
	filename := filepath.Join(repository, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func urioPushFixture(t *testing.T) (buildHome, repository, remote string) {
	t.Helper()
	temp := t.TempDir()
	buildHome = filepath.Join(temp, "build")
	repository = filepath.Join(buildHome, "mmm")
	remote = filepath.Join(temp, "remote.git")
	runGit(t, temp, "init", "--bare", remote)
	runGit(t, temp, "init", "-b", "main", repository)
	for _, name := range append(urioGeneratedFiles(), urioUnrelatedFiles...) {
		writeUrioTestFile(t, repository, name, "before\n")
	}
	runGit(t, repository, "add", ".")
	commitTestFile(t, repository, "base", "before\n", "seed")
	runGit(t, repository, "remote", "add", "origin", remote)
	runGit(t, repository, "push", "-u", "origin", "main")
	return
}

func runUrioPush(t *testing.T, buildHome string) ([]byte, error) {
	t.Helper()
	region := componentRegion(t, "# Push the content and page dates", "# push the regenerated localizations")
	command := exec.Command("zsh", "-f", "-c", `
builder_message() { print -r -- "$1"; }
eval "$1" || exit $?
eval "$2" || exit $?
print -r -- push-region-continued
`, "urio-push-test", componentFunctions(t, "error_trap"), region)
	command.Env = environmentWith(
		"WARP_HOME="+buildHome,
		"MMM_CANDIDATE_HEAD="+runGit(t, filepath.Join(buildHome, "mmm"), "rev-parse", "HEAD"),
		"EXTERNAL_WARP_VERSION="+urioTestReleaseVersion,
		"GIT_AUTHOR_NAME=Build Harness Test",
		"GIT_AUTHOR_EMAIL=build-harness-test@example.invalid",
		"GIT_COMMITTER_NAME=Build Harness Test",
		"GIT_COMMITTER_EMAIL=build-harness-test@example.invalid",
	)
	return command.CombinedOutput()
}

func TestRunUrioGeneratedPushPreservesUnrelatedChanges(t *testing.T) {
	for _, mode := range []string{"unstaged", "staged", "unstaged-remote-race"} {
		t.Run(mode, func(t *testing.T) {
			buildHome, repository, remote := urioPushFixture(t)
			for _, name := range urioUnrelatedFiles {
				writeUrioTestFile(t, repository, name, "unrelated edit\n")
			}
			untracked := "ur.io/react/src/data/operator-notes.js"
			writeUrioTestFile(t, repository, untracked, "untracked edit\n")
			// Dates may be unchanged even when the preflight refreshes the feed.
			generated := "ur.io/react/src/data/networkStats.js"
			writeUrioTestFile(t, repository, generated, "generated\n")
			if mode == "staged" {
				runGit(t, repository, "add", urioUnrelatedFiles[0])
			}
			if mode == "unstaged-remote-race" {
				concurrent := filepath.Join(t.TempDir(), "concurrent")
				runGit(t, buildHome, "clone", "-b", "main", remote, concurrent)
				commitTestFile(t, concurrent, "remote-source", "concurrent\n", "remote advance")
				runGit(t, concurrent, "push")
			}
			beforeRemote := runGit(t, remote, "rev-parse", "main")
			beforeHead := runGit(t, repository, "rev-parse", "HEAD")
			beforeIndex := runGit(t, repository, "diff", "--cached", "--binary")
			beforeWorktree := runGit(t, repository, "diff", "--binary")
			output, err := runUrioPush(t, buildHome)
			if mode != "staged" {
				if err != nil || !strings.Contains(string(output), "push-region-continued") {
					t.Fatalf("scoped push failed: %v\n%s", err, output)
				}
				if mode == "unstaged" {
					if changed := runGit(t, remote, "diff", "--name-only", beforeRemote, "main"); changed != generated {
						t.Fatalf("pushed paths = %q, want only %s", changed, generated)
					}
				} else if head := runGit(t, remote, "rev-parse", "main"); head != beforeRemote {
					t.Fatalf("concurrent push was overwritten: %s -> %s", beforeRemote, head)
				}
			} else {
				if err == nil || strings.Contains(string(output), "push-region-continued") || !strings.Contains(string(output), "ur.io content and page dates update push") {
					t.Fatalf("unsafe push did not fail the release gate: %v\n%s", err, output)
				}
				if head := runGit(t, remote, "rev-parse", "main"); head != beforeRemote {
					t.Fatalf("failed push changed remote main: %s -> %s", beforeRemote, head)
				}
				if head := runGit(t, repository, "rev-parse", "HEAD"); head != beforeHead {
					t.Fatal("preexisting staged edit was committed")
				}
				if diff := runGit(t, repository, "diff", "--binary"); diff != beforeWorktree {
					t.Fatal("preexisting staged edit changed worktree staging")
				}
			}
			if index := runGit(t, repository, "diff", "--cached", "--binary"); index != beforeIndex {
				t.Fatal("unrelated staged edits were changed")
			}
			for _, name := range append(slices.Clone(urioUnrelatedFiles), untracked) {
				contents, readErr := os.ReadFile(filepath.Join(repository, name))
				want := "unrelated edit\n"
				if name == untracked {
					want = "untracked edit\n"
				}
				if readErr != nil || string(contents) != want {
					t.Fatalf("unrelated %s changed: %v, %q", name, readErr, contents)
				}
			}
			if stashes := runGit(t, repository, "stash", "list"); stashes != "" {
				t.Fatalf("push stashed unrelated edits: %s", stashes)
			}
		})
	}
}

func TestRunUrioGeneratedPushHandlesNoopAndMirrorAdditionsAndRemovals(t *testing.T) {
	buildHome, repository, remote := urioPushFixture(t)
	before := runGit(t, remote, "rev-parse", "main")
	if output, err := runUrioPush(t, buildHome); err != nil {
		t.Fatalf("unchanged generated outputs failed: %v\n%s", err, output)
	}
	if head := runGit(t, repository, "rev-parse", "HEAD"); head != before {
		t.Fatal("unchanged outputs created a commit")
	}
	added := "ur.io/react/public/docs-md/new-guide.md"
	deleted := "ur.io/astro/public/docs-md/guide.md"
	writeUrioTestFile(t, repository, added, "generated guide\n")
	if err := os.Remove(filepath.Join(repository, deleted)); err != nil {
		t.Fatal(err)
	}
	if output, err := runUrioPush(t, buildHome); err != nil {
		t.Fatalf("new and removed mirrors failed: %v\n%s", err, output)
	}
	if contents := runGit(t, remote, "show", "main:"+added); contents != "generated guide" {
		t.Fatalf("new mirror = %q", contents)
	}
	if files := runGit(t, remote, "ls-tree", "--name-only", "main", "--", deleted); files != "" {
		t.Fatalf("removed mirror is still published: %s", files)
	}
	if status := runGit(t, repository, "status", "--porcelain"); status != "" {
		t.Fatalf("mirror changes were left behind: %s", status)
	}
	if branch := runGit(t, repository, "symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Fatalf("successful publication left checkout on %s", branch)
	}
	if head := runGit(t, repository, "rev-parse", "HEAD"); head != runGit(t, remote, "rev-parse", "main") {
		t.Fatal("successful publication did not keep local and remote main aligned")
	}
}

func TestRunUrioGeneratedPushKeepsCandidateSourceOnConcurrentMainAdvance(t *testing.T) {
	const generated = "ur.io/react/src/data/content-dates.js"
	for _, remotePath := range []string{"ur.io/react/src/components/Footer.jsx", generated} {
		t.Run(remotePath, func(t *testing.T) {
			buildHome, repository, remote := urioPushFixture(t)
			candidateHead := runGit(t, repository, "rev-parse", "HEAD")
			writeUrioTestFile(t, repository, generated, "local generated dates\n")
			concurrent := filepath.Join(t.TempDir(), "concurrent")
			runGit(t, buildHome, "clone", "-b", "main", remote, concurrent)
			remoteCommit := commitTestFile(t, concurrent, remotePath, "concurrent change\n", "concurrent MMM update")
			runGit(t, concurrent, "push")
			output, err := runUrioPush(t, buildHome)
			if err != nil || !strings.Contains(string(output), "push-region-continued") || !strings.Contains(string(output), "mmm main advanced") {
				t.Fatalf("concurrent MMM update prevented preparation: %v\n%s", err, output)
			}
			if head := runGit(t, remote, "rev-parse", "main"); head != remoteCommit {
				t.Fatal("candidate overwrote remote main")
			}
			releaseBranch := "v" + urioTestReleaseVersion
			if branch := runGit(t, repository, "symbolic-ref", "--short", "HEAD"); branch != releaseBranch {
				t.Fatalf("candidate checkout is on %s, want %s", branch, releaseBranch)
			}
			releaseHead := runGit(t, repository, "rev-parse", "HEAD")
			if parent := runGit(t, repository, "rev-parse", "HEAD^"); parent != candidateHead {
				t.Fatalf("release parent is %s, want checked source %s", parent, candidateHead)
			}
			if changed := runGit(t, repository, "diff", "--name-only", candidateHead, releaseHead); changed != generated {
				t.Fatalf("release changed paths = %q, want only %s", changed, generated)
			}
			if contents := runGit(t, repository, "show", releaseHead+":"+generated); contents != "local generated dates" {
				t.Fatalf("candidate generated dates = %q", contents)
			}
			if status := runGit(t, repository, "status", "--porcelain"); status != "" {
				t.Fatalf("generated outputs were left uncommitted: %s", status)
			}
			if head := runGit(t, repository, "rev-parse", "main"); head != candidateHead {
				t.Fatal("local main diverged from its original commit")
			}
			runGit(t, repository, "checkout", "main")
			runGit(t, repository, "pull", "--ff-only")
			if head := runGit(t, repository, "rev-parse", "HEAD"); head != remoteCommit {
				t.Fatal("next full release did not acquire the advanced main")
			}
			if head := runGit(t, repository, "rev-parse", releaseBranch); head != releaseHead {
				t.Fatal("pulling main lost the checked release commit")
			}
		})
	}
}

func TestRunUrioGeneratedPushRejectsOtherPushFailures(t *testing.T) {
	const generated = "ur.io/react/src/data/content-dates.js"
	buildHome, repository, remote := urioPushFixture(t)
	candidateHead := runGit(t, repository, "rev-parse", "HEAD")
	writeUrioTestFile(t, repository, generated, "local generated dates\n")
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := runUrioPush(t, buildHome)
	if err == nil || strings.Contains(string(output), "push-region-continued") || !strings.Contains(string(output), "A verified main advance is required") {
		t.Fatalf("unrelated push rejection did not stop preparation: %v\n%s", err, output)
	}
	if branch := runGit(t, repository, "symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Fatalf("failed preparation left checkout on %s", branch)
	}
	if head := runGit(t, repository, "rev-parse", "HEAD"); head != candidateHead {
		t.Fatal("failed preparation changed local main")
	}
	if head := runGit(t, remote, "rev-parse", "main"); head != candidateHead {
		t.Fatal("failed preparation changed remote main")
	}
	if status := runGit(t, repository, "status", "--porcelain"); status != "M "+generated {
		t.Fatalf("generated output was not kept as an unstaged edit: %s", status)
	}
	if contents, err := os.ReadFile(filepath.Join(repository, generated)); err != nil || string(contents) != "local generated dates\n" {
		t.Fatalf("generated output changed: %v, %q", err, contents)
	}
}
