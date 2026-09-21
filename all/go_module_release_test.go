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

func runFunction(t *testing.T, name string) string {
	t.Helper()
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + ` \(\) \{\n.*?^\}\n`)
	matches := pattern.FindAllString(string(runData), -1)
	if len(matches) != 1 {
		t.Fatalf("run.sh %s function count = %d, want 1", name, len(matches))
	}
	return matches[0]
}

// A release module points at a private tag pushed moments earlier. Exercise the
// production environment helper itself and prove its settings reach Git while
// preserving configuration supplied by the caller.
func TestConfigurePrivateGoModulesUsesBuildSSHCredentials(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "remote", "add", "sdk", "https://github.com/urnetwork/sdk")
	runGit(t, repository, "remote", "add", "sn", "https://github.com/urfoundation/sn")

	harness := `
set -eu
eval "$1"
configure_private_go_modules
print -r -- "$GOPRIVATE"
print -r -- "$GONOPROXY"
print -r -- "$GONOSUMDB"
print -r -- "$GIT_CONFIG_COUNT"
git config --get test.preexisting
git config --get-regexp '^url\..*\.insteadof$' | LC_ALL=C sort
git -C "$2" remote get-url sdk
git -C "$2" remote get-url sn
`
	command := exec.Command("zsh", "-c", harness, "private-go-module-test", runFunction(t, "configure_private_go_modules"), repository)
	command.Env = append(os.Environ(),
		"GOPRIVATE=private.example",
		"GONOPROXY=proxy.example",
		"GONOSUMDB=sum.example",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=test.preexisting",
		"GIT_CONFIG_VALUE_0=preserved",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("private module setup failed: %v\n%s", err, output)
	}
	want := strings.Join([]string{
		"private.example,github.com/urnetwork,github.com/urfoundation",
		"proxy.example,github.com/urnetwork,github.com/urfoundation",
		"sum.example,github.com/urnetwork,github.com/urfoundation",
		"3",
		"preserved",
		"url.ssh://git@github.com/urfoundation/.insteadof https://github.com/urfoundation/",
		"url.ssh://git@github.com/urnetwork/.insteadof https://github.com/urnetwork/",
		"ssh://git@github.com/urnetwork/sdk",
		"ssh://git@github.com/urfoundation/sn",
		"",
	}, "\n")
	if string(output) != want {
		t.Fatalf("private module environment:\n%s\nwant:\n%s", output, want)
	}
}

func TestGoModForkUpdateRunsTidyBeforeGet(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "js", "go.mod"), []byte("module example.invalid/js\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	harness := `
set -eu
event_log="$1"
go() {
    print -r -- "$*" >> "$event_log"
}
GO_MOD_VERSION=2026
eval "$2"
go_mod_fork_update js
`
	eventLog := filepath.Join(root, "events")
	command := exec.Command("zsh", "-c", harness, "go-module-update-test", eventLog, runFunction(t, "go_mod_fork_update"))
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go module update failed: %v\n%s", err, output)
	}
	events, err := os.ReadFile(eventLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(events) != "mod tidy\nget -t ./...\n" {
		t.Fatalf("go module commands = %q, want %q", events, "mod tidy\nget -t ./...\n")
	}
}

// Reproduce the old false success: js sorts before a later non-module entry,
// so the loop used to replace go mod tidy's failure status with zero. The
// release must stop on the original status and must not advance to go get.
func TestGoModForkUpdateReturnsTidyFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "js", "go.mod"), []byte("module example.invalid/js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "zz-after-js"), 0o755); err != nil {
		t.Fatal(err)
	}

	harness := `
set -u
event_log="$1"
go() {
    print -r -- "$*" >> "$event_log"
    if [[ "$1 $2" = 'mod tidy' ]]; then
        return 37
    fi
    return 0
}
GO_MOD_VERSION=2026
eval "$2"
go_mod_fork_update js
`
	eventLog := filepath.Join(root, "events")
	command := exec.Command("zsh", "-c", harness, "go-module-update-test", eventLog, runFunction(t, "go_mod_fork_update"))
	command.Dir = root
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 37 {
		t.Fatalf("go_mod_fork_update exit = %v, want 37; output=%s", err, output)
	}
	events, err := os.ReadFile(eventLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(events) != "mod tidy\n" {
		t.Fatalf("go module commands = %q, want %q", events, "mod tidy\n")
	}
}

func TestGoModForkUpdateRejectsMissingModule(t *testing.T) {
	command := exec.Command(
		"zsh", "-c",
		`GO_MOD_VERSION=2026; eval "$1"; go_mod_fork_update missing`,
		"go-module-update-test", runFunction(t, "go_mod_fork_update"),
	)
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "missing forked module: missing/go.mod") {
		t.Fatalf("missing module exit = %v, output=%q", err, output)
	}
}

// sim-testnet imports server, while server imports the published SN libraries.
// Keeping sim-testnet in the SN module therefore makes each module zip contain
// the checksum of the other, which cannot be finalized without moving a public
// tag. Exercise the production fork helpers and prove the operator harness is
// preserved in Git but omitted from the versioned SN module graph.
func TestGoModForkCanPreserveSimulatorOutsidePublishedModule(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sim-testnet"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod": `module example.invalid/sn

go 1.26.7

require example.invalid/server v0.0.0

replace example.invalid/server => ./missing-server
`,
		"sn.go":                    "package sn\n",
		"sim-testnet/main.go":      "package main\nfunc main() {}\n",
		"sim-testnet/evidence.txt": "preserved release evidence\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	harness := `
set -eu
GO_MOD_VERSION=2026
# The fixture has no retract directive, so a no-op keeps this test independent
# of whether GNU sed is named sed or gsed on the test host.
BUILD_SED=true
eval "$1"
eval "$2"
eval "$3"
eval "$4"
eval "$5"
go_mod_drop_require example.invalid/server
go_mod_fork sim-testnet
`
	command := exec.Command(
		"zsh", "-c", harness, "go-module-cycle-test",
		runFunction(t, "go_mod_drop_require"),
		runFunction(t, "go_mod_fork_rebase_local_replaces"),
		runFunction(t, "go_mod_fork_prepare"),
		runFunction(t, "go_mod_fork_tidy"),
		runFunction(t, "go_mod_fork"),
	)
	command.Dir = root
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("SN module fork failed: %v\n%s", err, output)
	}
	for _, name := range []string{"sim-testnet/main.go", "sim-testnet/evidence.txt", "v2026/sn.go", "v2026/go.mod"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("forked release lacks %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "v2026", "sim-testnet")); !os.IsNotExist(err) {
		t.Fatalf("sim-testnet entered the published SN module: %v", err)
	}
	goMod, err := os.ReadFile(filepath.Join(root, "v2026", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(goMod), "example.invalid/server") {
		t.Fatalf("published SN module retained the server edge:\n%s", goMod)
	}
}

// The release fork moves go.mod into vNNNN. Replacements whose targets remain
// outside that directory must climb one more level afterward. This includes a
// child-relative root nested module (the Connect/SCTP layout), while paths that
// move inside the versioned module and module-version replacements stay intact.
func TestGoModForkRebasesLocalReplacementsLeftOutsideVersionedModule(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "server")
	for _, directory := range []string{
		root,
		filepath.Join(root, "release", "local"),
		filepath.Join(root, "sctp"),
		filepath.Join(root, "third_party", "local"),
		filepath.Join(outer, "warp"),
		filepath.Join(outer, "shared"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "go.mod"): `module example.invalid/server

go 1.26.7

require (
	example.invalid/local v0.0.0
	example.invalid/obsolete v0.0.0
	example.invalid/preserved v0.0.0
	example.invalid/sctp v0.0.0
	example.invalid/shared v0.0.0
	example.invalid/warp v0.0.0
)

replace example.invalid/local => ./third_party/local

replace example.invalid/obsolete => ../obsolete

replace example.invalid/preserved => ./release/local

replace example.invalid/sctp => ./sctp

replace example.invalid/shared v0.0.0 => ../shared

replace example.invalid/versioned => example.invalid/versioned-fork v1.2.3

replace example.invalid/warp => ../warp
`,
		filepath.Join(root, "server.go"): `package server

import (
	"example.invalid/local"
	"example.invalid/preserved"
	"example.invalid/sctp"
	"example.invalid/shared"
	"example.invalid/warp"
)

const Value = local.Value + preserved.Value + sctp.Value + shared.Value + warp.Value
`,
		filepath.Join(root, "release", "local", "go.mod"):       "module example.invalid/preserved\n\ngo 1.26.7\n",
		filepath.Join(root, "release", "local", "preserved.go"): "package preserved\n\nconst Value = 5\n",
		filepath.Join(root, "sctp", "go.mod"):                   "module example.invalid/sctp\n\ngo 1.26.7\n",
		filepath.Join(root, "sctp", "sctp.go"):                  "package sctp\n\nconst Value = 4\n",
		filepath.Join(root, "third_party", "local", "go.mod"):   "module example.invalid/local\n\ngo 1.26.7\n",
		filepath.Join(root, "third_party", "local", "local.go"): "package local\n\nconst Value = 1\n",
		filepath.Join(outer, "warp", "go.mod"):                  "module example.invalid/warp\n\ngo 1.26.7\n",
		filepath.Join(outer, "warp", "warp.go"):                 "package warp\n\nconst Value = 2\n",
		filepath.Join(outer, "shared", "go.mod"):                "module example.invalid/shared\n\ngo 1.26.7\n",
		filepath.Join(outer, "shared", "shared.go"):             "package shared\n\nconst Value = 3\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(name, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	harness := `
set -eu
GO_MOD_VERSION=2026
BUILD_SED=true
eval "$1"
eval "$2"
eval "$3"
eval "$4"
eval "$5"
go_mod_drop_require example.invalid/obsolete
go_mod_fork 'release/local'
`
	command := exec.Command(
		"zsh", "-c", harness, "go-module-relative-replace-test",
		runFunction(t, "go_mod_drop_require"),
		runFunction(t, "go_mod_fork_rebase_local_replaces"),
		runFunction(t, "go_mod_fork_prepare"),
		runFunction(t, "go_mod_fork_tidy"),
		runFunction(t, "go_mod_fork"),
	)
	command.Dir = root
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("server module fork failed: %v\n%s", err, output)
	}

	goMod, err := os.ReadFile(filepath.Join(root, "v2026", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	wantDirectives := []string{
		"replace example.invalid/local => ./third_party/local",
		"replace example.invalid/preserved => ../release/local",
		"replace example.invalid/sctp => ../sctp",
		"replace example.invalid/shared v0.0.0 => ../../shared",
		"replace example.invalid/versioned => example.invalid/versioned-fork v1.2.3",
		"replace example.invalid/warp => ../../warp",
	}
	for _, directive := range wantDirectives {
		if !strings.Contains(string(goMod), directive) {
			t.Errorf("forked go.mod lacks %q:\n%s", directive, goMod)
		}
	}
	if strings.Contains(string(goMod), "example.invalid/obsolete") {
		t.Errorf("forked go.mod restored dropped dependency:\n%s", goMod)
	}

	testCommand := exec.Command("go", "test", "./...")
	testCommand.Dir = filepath.Join(root, "v2026")
	testCommand.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if output, err := testCommand.CombinedOutput(); err != nil {
		t.Fatalf("forked module does not resolve its replacements: %v\n%s", err, output)
	}
}

// Public Go module versions are content-addressed by go.sum and the checksum
// database. Deleting and recreating a tag can make a consumer retain the first
// archive forever, so release choreography must be append-only even when a
// later commit only updates a nested build module.
func TestRunPublishesImmutableAcyclicGoModuleTags(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(runData)
	for _, forbidden := range []string{
		"git push --delete origin",
		"git_tag recreate",
		"git tag -d v${EXTERNAL_WARP_VERSION}",
		"sn bootstrap push branch",
		"sn finalize branch",
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("run.sh can move a published module tag through %q", forbidden)
		}
	}

	snFork := strings.Index(source, "go_mod_fork 'sim-testnet'")
	snTag := strings.Index(source, "error_trap 'sn push branch'")
	serverFork := strings.Index(source, "go_mod_fork 'connect/sim-latency/baseline'")
	serverTag := strings.Index(source, "error_trap 'server push branch'")
	if snFork < 0 || snTag < 0 || serverFork < 0 || serverTag < 0 || !(snFork < snTag && snTag < serverFork && serverFork < serverTag) {
		t.Fatalf("immutable module order is sn-fork=%d sn-tag=%d server-fork=%d server-tag=%d", snFork, snTag, serverFork, serverTag)
	}
	if got := strings.Count(source, "(cd $BUILD_HOME/sn &&\n    git_commit &&\n    git_tag)"); got != 1 {
		t.Fatalf("SN root tag publication count = %d, want 1", got)
	}
	snStart := strings.Index(source, "(cd $BUILD_HOME/sn &&\n    go_mod_edit_module")
	if snStart < 0 {
		t.Fatal("SN release edit block is missing")
	}
	snEndOffset := strings.Index(source[snStart:], "error_trap 'sn edit'")
	if snEndOffset < 0 {
		t.Fatal("SN release edit block has no checked terminal stage")
	}
	snBlock := source[snStart : snStart+snEndOffset]
	if strings.Contains(snBlock, "go_mod_edit_require github.com/urnetwork/server") {
		t.Fatal("SN release publishes a server requirement before the server tag exists")
	}
	serverDrop := strings.Index(snBlock, "go_mod_drop_require github.com/urnetwork/server")
	serverRewrite := strings.Index(snBlock, "go_edit_require_subpackages github.com/urnetwork/server")
	simulatorFork := strings.Index(snBlock, "go_mod_fork 'sim-testnet'")
	if serverDrop < 0 || serverRewrite < 0 || simulatorFork < 0 || !(serverDrop < serverRewrite && serverRewrite < simulatorFork) {
		t.Fatalf("SN server-edge order is drop=%d rewrite=%d fork=%d", serverDrop, serverRewrite, simulatorFork)
	}
	sdkStart := strings.Index(source, "(cd $BUILD_HOME/sdk &&\n    git_commit &&\n    sdk_tagged_module_tree=")
	if sdkStart < 0 {
		t.Fatal("SDK release does not record its root module tree before publication")
	}
	sdkEndOffset := strings.Index(source[sdkStart:], "error_trap 'sdk push branch'")
	if sdkEndOffset < 0 {
		t.Fatal("SDK release block has no checked terminal stage")
	}
	sdkBlock := source[sdkStart : sdkStart+sdkEndOffset]
	if got := strings.Count(sdkBlock, "git_tag"); got != 1 {
		t.Fatalf("SDK public tag publication count = %d, want 1", got)
	}
	if !strings.Contains(sdkBlock, `test "$sdk_tagged_module_tree" = `+"`"+`git rev-parse HEAD:v${GO_MOD_VERSION}`+"`"+`)`) {
		t.Fatal("SDK release does not prove nested lock commits preserve the tagged root module tree")
	}
}

// SDK commits its nested build-module locks after its root module tag has been
// published. The release branch and tag deliberately have the same name, so
// the production commit helper must push an explicit heads ref instead of an
// ambiguous short refname.
func TestGitCommitPushesBranchWhenReleaseTagHasSameName(t *testing.T) {
	tempDir := t.TempDir()
	remote := filepath.Join(tempDir, "remote.git")
	repository := filepath.Join(tempDir, "release")
	version := "2026.9.7-9999999999"
	releaseRef := "v" + version

	runGit(t, tempDir, "init", "--bare", remote)
	runGit(t, tempDir, "init", "-b", releaseRef, repository)
	runGit(t, repository, "config", "user.name", "Build Harness Test")
	runGit(t, repository, "config", "user.email", "build-harness-test@example.invalid")
	taggedCommit := commitTestFile(t, repository, "root", "root module\n", "root release tree")
	runGit(t, repository, "remote", "add", "origin", remote)
	runGit(t, repository, "push", "-u", "origin", "HEAD:refs/heads/"+releaseRef)
	runGit(t, repository, "tag", "-a", releaseRef, "-m", version)
	runGit(t, repository, "push", "origin", "refs/tags/"+releaseRef)

	if err := os.WriteFile(filepath.Join(repository, "go.sum"), []byte("nested module lock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := `
set -eu
EXTERNAL_WARP_VERSION="$1"
eval "$2"
git_commit
`
	command := exec.Command("zsh", "-c", harness, "branch-tag-collision-test", version, runFunction(t, "git_commit"))
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("post-tag branch commit failed: %v\n%s", err, output)
	}

	head := runGit(t, repository, "rev-parse", "HEAD")
	remoteBranch := runGit(t, tempDir, "--git-dir", remote, "rev-parse", "refs/heads/"+releaseRef)
	if remoteBranch != head {
		t.Fatalf("remote release branch = %s, want post-tag commit %s", remoteBranch, head)
	}
	remoteTag := runGit(t, tempDir, "--git-dir", remote, "rev-parse", "refs/tags/"+releaseRef+"^{}")
	if remoteTag != taggedCommit {
		t.Fatalf("immutable release tag moved to %s, want %s", remoteTag, taggedCommit)
	}
	upstream := runGit(t, repository, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if upstream != "origin/"+releaseRef {
		t.Fatalf("release branch upstream = %q, want %q", upstream, "origin/"+releaseRef)
	}
}

// Exercise the exact production tag helper against a local origin. A second
// publication attempt must fail and leave both the tag object and peeled commit
// unchanged.
func TestGitTagRefusesToMovePublishedVersion(t *testing.T) {
	tempDir := t.TempDir()
	remote := filepath.Join(tempDir, "remote.git")
	repository := filepath.Join(tempDir, "release")
	runGit(t, tempDir, "init", "--bare", remote)
	runGit(t, tempDir, "init", "-b", "release", repository)
	runGit(t, repository, "config", "user.name", "Build Harness Test")
	runGit(t, repository, "config", "user.email", "build-harness-test@example.invalid")
	commitTestFile(t, repository, "first", "first\n", "first release tree")
	runGit(t, repository, "remote", "add", "origin", remote)
	runGit(t, repository, "push", "-u", "origin", "release")

	harness := `
set -u
EXTERNAL_WARP_VERSION=2026.9.7-9999999999
builder_message() { print -r -- "$*" }
eval "$1"
git_tag
`
	first := exec.Command("zsh", "-c", harness, "immutable-tag-test", runFunction(t, "git_tag"))
	first.Dir = repository
	if output, err := first.CombinedOutput(); err != nil {
		t.Fatalf("first tag publication failed: %v\n%s", err, output)
	}
	tagRef := "refs/tags/v2026.9.7-9999999999"
	before := runGit(t, repository, "ls-remote", "--tags", "origin", tagRef, tagRef+"^{}")
	commitTestFile(t, repository, "second", "second\n", "later release tree")

	second := exec.Command("zsh", "-c", harness, "immutable-tag-test", runFunction(t, "git_tag"))
	second.Dir = repository
	output, err := second.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "refusing to overwrite") {
		t.Fatalf("second tag publication exit = %v, output=%q", err, output)
	}
	after := runGit(t, repository, "ls-remote", "--tags", "origin", tagRef, tagRef+"^{}")
	if after != before {
		t.Fatalf("published tag moved:\nbefore: %s\nafter:  %s", before, after)
	}
}
