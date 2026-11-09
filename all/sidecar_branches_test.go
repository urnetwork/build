// SPDX-License-Identifier: MPL-2.0

// sidecar-branches.zsh over a fixture monoroot: each repository a clone of its own bare origin in a temp
// directory, on main and sidecar as §4.8 has them, with server's source-graph lock and build's gitlinks.
// git runs through a recording wrapper, so every test also shows the tool neither pushes, pulls nor
// rebases. Nothing here reaches a real repository or a network.

package allbuild

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The repositories a fixture holds with a sidecar branch, in §4.8's order, and the branch each has as main.
var branchesFixtureRepos = []string{"connect", "sidecar", "sdk", "sn", "server", "minio", "build"}

// The siblings the fixture's lock pins: those with a sidecar branch, and glog, which has none.
var branchesLockedSiblings = []string{"connect", "sidecar", "sdk", "sn", "glog"}

func branchesMainBranch(repo string) string {
	if repo == "minio" {
		return "master"
	}
	return "main"
}

type branchesFixture struct {
	t       *testing.T
	root    string
	remotes string
	// the git subcommand of each git call the tool made
	gitLog string
	bin    string
	// what the fixture committed first on each repository's main
	baseCommits map[string]string
}

// The environment every git call runs in, the tool's included: no configuration of the host's.
func branchesGitEnvironment(extra ...string) []string {
	return environmentWith(append([]string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Branches Fixture", "GIT_AUTHOR_EMAIL=branches-fixture@example.invalid",
		"GIT_COMMITTER_NAME=Branches Fixture", "GIT_COMMITTER_EMAIL=branches-fixture@example.invalid",
	}, extra...)...)
}

func (self *branchesFixture) git(repo string, arguments ...string) string {
	self.t.Helper()
	command := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, arguments...)...)
	command.Dir = filepath.Join(self.root, repo)
	command.Env = branchesGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		self.t.Fatalf("git %s in %s: %v\n%s", strings.Join(arguments, " "), repo, err, output)
	}
	return strings.TrimSpace(string(output))
}

// Writes a file in a repository and commits it on the branch checked out there.
func (self *branchesFixture) commit(repo, name, content, message string) string {
	self.t.Helper()
	path := filepath.Join(self.root, repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		self.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		self.t.Fatal(err)
	}
	self.git(repo, "add", "--", name)
	self.git(repo, "commit", "-q", "-m", message)
	return self.git(repo, "rev-parse", "HEAD")
}

// Commits on a repository's main and returns to sidecar.
func (self *branchesFixture) commitOnMain(repo, name, content, message string) string {
	self.t.Helper()
	self.git(repo, "checkout", "-q", branchesMainBranch(repo))
	commit := self.commit(repo, name, content, message)
	self.git(repo, "checkout", "-q", "sidecar")
	return commit
}

// The fixture's sidecar module: a TestX9ReadyForMain that refuses while connect holds x9-refuses, as the
// real one refuses what X9 does.
const branchesX9Test = `//go:build sidecar_check

package check

import (
	"maps"
	"os"
	"testing"
)

func TestX9ReadyForMain(t *testing.T) {
	if os.Getenv("SIDECAR_X9_GATE") == "" {
		t.Skip("no gate")
	}
	if _, err := os.Stat("../../connect/x9-refuses"); err == nil {
		t.Fatal("connect at the recorded commit is not ready for main")
	}
}
`

func (self *branchesFixture) lock() string {
	self.t.Helper()
	data, err := os.ReadFile(filepath.Join(self.root, "server", "local", "source-graph", "lock.yml"))
	if err != nil {
		self.t.Fatal(err)
	}
	return string(data)
}

// A lock in server's shape, pinning each sibling at commits.
func branchesLock(commits map[string]string) string {
	lock := strings.Builder{}
	lock.WriteString("# The source lock.\ngo: 1.27.3\nsiblings:\n")
	for _, sibling := range branchesLockedSiblings {
		lock.WriteString("  # " + sibling + "'s pin\n  - repository: urnetwork/" + sibling + "\n    path: " + sibling + "\n    ref: " + commits[sibling] + "\n")
	}
	return lock.String()
}

func newBranchesFixture(t *testing.T) *branchesFixture {
	t.Helper()
	directory := t.TempDir()
	self := &branchesFixture{
		t:           t,
		root:        filepath.Join(directory, "root"),
		remotes:     filepath.Join(directory, "remotes"),
		gitLog:      filepath.Join(directory, "git.log"),
		bin:         filepath.Join(directory, "bin"),
		baseCommits: map[string]string{},
	}
	for _, path := range []string{self.root, self.remotes, self.bin} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
sub=""
skip=0
for argument in "$@"; do
    if [ "$skip" = 1 ]; then skip=0; continue; fi
    case "$argument" in
        -C|-c) skip=1 ;;
        -*) ;;
        *) sub="$argument"; break ;;
    esac
done
printf '%s\n' "$sub" >> "` + self.gitLog + `"
exec "` + realGit + `" "$@"
`
	if err := os.WriteFile(filepath.Join(self.bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	initialize := func(repo string, main string) {
		if err := os.MkdirAll(filepath.Join(self.root, repo), 0o700); err != nil {
			t.Fatal(err)
		}
		self.git(repo, "init", "-q", "-b", main)
		self.baseCommits[repo] = self.commit(repo, "README", repo+"\n", "Start "+repo)
	}
	initialize("glog", "main")
	for _, repo := range branchesFixtureRepos {
		initialize(repo, branchesMainBranch(repo))
	}
	self.commit("server", "local/source-graph/lock.yml", branchesLock(self.baseCommits), "Pin the siblings")
	for _, sibling := range []string{"connect", "sidecar", "sdk", "sn", "server", "glog"} {
		// an empty directory, as an uninitialized submodule's is in build's checkout
		if err := os.MkdirAll(filepath.Join(self.root, "build", sibling), 0o700); err != nil {
			t.Fatal(err)
		}
		self.git("build", "update-index", "--add", "--cacheinfo", "160000,"+self.baseCommits[sibling]+","+sibling)
	}
	self.git("build", "commit", "-q", "-m", "Add the submodules")
	for _, repo := range branchesFixtureRepos {
		self.git(repo, "checkout", "-q", "-b", "sidecar")
	}
	self.commit("sidecar", "go.mod", "module example.invalid/sidecarfixture\n\ngo 1.27.3\n", "Start the module")
	self.commit("sidecar", "check/x9_test.go", branchesX9Test, "Add X9's gate")
	for _, repo := range append([]string{"glog"}, branchesFixtureRepos...) {
		bare := filepath.Join(self.remotes, repo+".git")
		command := exec.Command("git", "clone", "-q", "--bare", filepath.Join(self.root, repo), bare)
		command.Env = branchesGitEnvironment()
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("clone %s: %v\n%s", repo, err, output)
		}
		self.git(repo, "remote", "add", "origin", bare)
		self.git(repo, "fetch", "-q", "origin")
	}
	return self
}

type branchesResult struct {
	exitCode int
	output   string
}

// Runs the tool over the fixture, and fails the test if it pushed, pulled or rebased.
func (self *branchesFixture) run(arguments ...string) branchesResult {
	self.t.Helper()
	command := exec.Command("zsh", append([]string{filepath.Join(rolloutRoot(self.t), "sidecar-branches.zsh")}, arguments...)...)
	command.Env = branchesGitEnvironment(
		"SIDECAR_BRANCHES_ROOT="+self.root,
		"PATH="+self.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GOFLAGS=", "GOPROXY=off", "GOWORK=off", "GOTOOLCHAIN=local",
	)
	output, err := command.CombinedOutput()
	result := branchesResult{output: string(output)}
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			self.t.Fatal(err)
		}
		result.exitCode = exitError.ExitCode()
	}
	logged, err := os.ReadFile(self.gitLog)
	if err != nil && !os.IsNotExist(err) {
		self.t.Fatal(err)
	}
	for _, subcommand := range strings.Fields(string(logged)) {
		if subcommand == "push" || subcommand == "pull" || subcommand == "rebase" {
			self.t.Fatalf("the tool ran git %s:\n%s", subcommand, result.output)
		}
	}
	return result
}

// The order lines matching pattern name repositories in.
func branchesOrder(output string, pattern string) []string {
	repos := []string{}
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(output, -1) {
		repos = append(repos, match[1])
	}
	return repos
}

func (self *branchesFixture) contains(repo, branch, commit string) bool {
	command := exec.Command("git", "merge-base", "--is-ancestor", commit, "refs/heads/"+branch)
	command.Dir = filepath.Join(self.root, repo)
	command.Env = branchesGitEnvironment()
	return command.Run() == nil
}

// The lock's ref for each sibling.
func branchesLockRefs(lock string) map[string]string {
	refs := map[string]string{}
	path := ""
	for _, line := range strings.Split(lock, "\n") {
		field := strings.Fields(line)
		if len(field) >= 2 && field[0] == "path:" {
			path = field[1]
		}
		if len(field) >= 2 && field[0] == "ref:" {
			refs[path] = field[1]
		}
	}
	return refs
}

func (self *branchesFixture) gitlinks() map[string]string {
	self.t.Helper()
	links := map[string]string{}
	for _, line := range strings.Split(self.git("build", "ls-tree", "sidecar"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "160000" {
			links[fields[3]] = fields[2]
		}
	}
	return links
}

// After a re-pin the lock and build's gitlinks name each sibling's checked-out commit, and build's names
// server's commit that holds the new lock.
func (self *branchesFixture) requirePinned() {
	self.t.Helper()
	refs := branchesLockRefs(self.lock())
	links := self.gitlinks()
	for _, sibling := range branchesLockedSiblings {
		head := self.git(sibling, "rev-parse", "HEAD")
		if refs[sibling] != head || links[sibling] != head {
			self.t.Errorf("%s is at %s, and the lock pins %s and build's gitlink %s", sibling, head, refs[sibling], links[sibling])
		}
	}
	if server := self.git("server", "rev-parse", "sidecar"); links["server"] != server {
		self.t.Errorf("build pins server at %s, and server's sidecar is %s", links["server"], server)
	}
}

func TestMergeInFollowsTheOrder(t *testing.T) {
	fixture := newBranchesFixture(t)
	mainCommits := map[string]string{}
	for _, repo := range branchesFixtureRepos {
		mainCommits[repo] = fixture.commitOnMain(repo, "main.txt", "main\n", "Change "+repo+"'s main")
	}
	result := fixture.run("merge-in")
	if result.exitCode != 0 {
		t.Fatalf("merge-in failed: %+v", result)
	}
	if order := branchesOrder(result.output, `merge-in: merged \w+ into (\w+)'s sidecar`); !slices.Equal(order, branchesFixtureRepos) {
		t.Errorf("merge-in merged %v, and §4.8's order is %v", order, branchesFixtureRepos)
	}
	for _, repo := range branchesFixtureRepos {
		if !fixture.contains(repo, "sidecar", mainCommits[repo]) {
			t.Errorf("%s's sidecar does not hold its main's commit", repo)
		}
	}
	for _, absent := range []string{"warp", "config", "vault", "xops", "mmm"} {
		if !strings.Contains(result.output, "merge-in: "+absent+" is not checked out here, skipped") {
			t.Errorf("merge-in did not say it skipped %s:\n%s", absent, result.output)
		}
	}
	if !strings.Contains(result.output, "owner: git -C "+fixture.root+"/connect push origin sidecar") {
		t.Errorf("merge-in printed no push for the owner:\n%s", result.output)
	}
	fixture.requirePinned()
}

func TestMergeInStopsAtAConflictAndNamesTheRepo(t *testing.T) {
	fixture := newBranchesFixture(t)
	mainCommits := map[string]string{}
	for _, repo := range branchesFixtureRepos {
		mainCommits[repo] = fixture.commitOnMain(repo, "shared.txt", "main\n", "Change "+repo+"'s main")
	}
	fixture.commit("sdk", "shared.txt", "sidecar\n", "Change sdk's sidecar")
	lockBefore := fixture.lock()
	result := fixture.run("merge-in")
	if result.exitCode == 0 || !strings.Contains(result.output, "merge-in stopped at sdk") {
		t.Fatalf("merge-in did not stop at sdk's conflict: %+v", result)
	}
	for _, repo := range []string{"connect", "sidecar"} {
		if !fixture.contains(repo, "sidecar", mainCommits[repo]) {
			t.Errorf("%s, before the conflict, was not merged", repo)
		}
	}
	for _, repo := range []string{"sn", "server", "minio", "build"} {
		if fixture.contains(repo, "sidecar", mainCommits[repo]) {
			t.Errorf("%s, after the conflict, was merged", repo)
		}
	}
	if fixture.lock() != lockBefore || strings.Contains(result.output, "repin:") {
		t.Errorf("merge-in re-pinned after stopping:\n%s", result.output)
	}
}

func TestRepinWritesTheLockAndTheGitlinks(t *testing.T) {
	fixture := newBranchesFixture(t)
	fixture.commit("connect", "connect.go", "package connect\n", "Change connect's sidecar")
	result := fixture.run("repin")
	if result.exitCode != 0 {
		t.Fatalf("repin failed: %+v", result)
	}
	fixture.requirePinned()
	if check := fixture.run("check"); check.exitCode != 0 {
		t.Errorf("check after a re-pin failed: %+v", check)
	}
}

func TestRepinRefusesADirtySibling(t *testing.T) {
	fixture := newBranchesFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.root, "connect", "stray.go"), []byte("package connect\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockBefore, linksBefore := fixture.lock(), fixture.gitlinks()
	result := fixture.run("repin")
	if result.exitCode == 0 || !strings.Contains(result.output, "repin: connect has changes outside a commit") {
		t.Fatalf("repin took a dirty connect: %+v", result)
	}
	if fixture.lock() != lockBefore || !maps.Equal(fixture.gitlinks(), linksBefore) {
		t.Errorf("a refused re-pin changed the pins")
	}
}

func TestRepinRefusesAnUnreachableCommit(t *testing.T) {
	fixture := newBranchesFixture(t)
	fixture.git("glog", "checkout", "-q", "--detach")
	fixture.commit("glog", "glog.go", "package glog\n", "Change glog off any branch")
	lockBefore := fixture.lock()
	result := fixture.run("repin")
	if result.exitCode == 0 || !strings.Contains(result.output, "repin: glog's") || !strings.Contains(result.output, "is on no branch") {
		t.Fatalf("repin pinned an unreachable commit: %+v", result)
	}
	if fixture.lock() != lockBefore {
		t.Errorf("a refused re-pin changed the lock")
	}
}

// What a gate recorded, written as server/local/sidecar-gate's report writes it.
func (self *branchesFixture) recordCommitSet(gate string) (string, map[string]string) {
	self.t.Helper()
	recorded := map[string]string{}
	report := strings.Builder{}
	report.WriteString("# Gate report: " + gate + "\n\n## Commit set\n\n| repo | branch | commit |\n|---|---|---|\n")
	for _, repo := range branchesFixtureRepos {
		recorded[repo] = self.git(repo, "rev-parse", "sidecar")
		report.WriteString("| " + repo + " | sidecar | " + recorded[repo] + " |\n")
	}
	report.WriteString("| vault | | not read: its commits are the owner's to record |\n")
	path := filepath.Join(filepath.Dir(self.root), "gate-"+gate+".md")
	if err := os.WriteFile(path, []byte(report.String()), 0o600); err != nil {
		self.t.Fatal(err)
	}
	return path, recorded
}

func (self *branchesFixture) mains() map[string]string {
	self.t.Helper()
	mains := map[string]string{}
	for _, repo := range branchesFixtureRepos {
		mains[repo] = self.git(repo, "rev-parse", branchesMainBranch(repo))
	}
	return mains
}

func TestMergeOutTakesTheRecordedCommitNotTheTip(t *testing.T) {
	fixture := newBranchesFixture(t)
	setPath, recorded := fixture.recordCommitSet("M1")
	tips := map[string]string{}
	for _, repo := range branchesFixtureRepos {
		tips[repo] = fixture.commit(repo, "later.txt", "later\n", "Build past the gate in "+repo)
	}
	result := fixture.run("merge-out", "M1", setPath)
	if result.exitCode != 0 {
		t.Fatalf("merge-out failed: %+v", result)
	}
	for _, repo := range branchesFixtureRepos {
		main := branchesMainBranch(repo)
		if !fixture.contains(repo, main, recorded[repo]) {
			t.Errorf("%s's %s does not hold the recorded commit", repo, main)
		}
		if fixture.contains(repo, main, tips[repo]) {
			t.Errorf("%s's %s holds the tip, which the gate did not record", repo, main)
		}
	}
}

func TestMergeOutPutsConnectFirst(t *testing.T) {
	fixture := newBranchesFixture(t)
	setPath, _ := fixture.recordCommitSet("M2")
	result := fixture.run("merge-out", "M2", setPath)
	if result.exitCode != 0 {
		t.Fatalf("merge-out failed: %+v", result)
	}
	if order := branchesOrder(result.output, `merge-out: merged (\w+)'s`); !slices.Equal(order, branchesFixtureRepos) {
		t.Errorf("merge-out merged %v, and §4.8's order, connect first, is %v", order, branchesFixtureRepos)
	}
	if !strings.Contains(result.output, "TestX9ReadyForMain passed at M2") {
		t.Errorf("merge-out did not run X9's gate:\n%s", result.output)
	}
	for _, line := range []string{
		"owner: git -C " + fixture.root + "/connect push origin main",
		"owner: git -C " + fixture.root + "/minio push origin master",
		"merge-out: the commit set records no vault commit, so vault is not merged",
	} {
		if !strings.Contains(result.output, line) {
			t.Errorf("merge-out did not print %q:\n%s", line, result.output)
		}
	}
}

func TestMergeOutRefusesAnAttributedCommit(t *testing.T) {
	fixture := newBranchesFixture(t)
	fixture.commit("sdk", "sdk.go", "package sdk\n", "Add the sdk's wrapper\n\nCo-authored-by: Someone <someone@example.invalid>")
	setPath, _ := fixture.recordCommitSet("M1")
	mainsBefore := fixture.mains()
	result := fixture.run("merge-out", "M1", setPath)
	if result.exitCode == 0 || !strings.Contains(result.output, "merge-out: sdk's commits") {
		t.Fatalf("merge-out took an attributed commit: %+v", result)
	}
	if !maps.Equal(fixture.mains(), mainsBefore) {
		t.Errorf("a refused merge-out moved a main")
	}
}

func TestMergeOutRefusesWhatX9Refuses(t *testing.T) {
	fixture := newBranchesFixture(t)
	fixture.commit("connect", "x9-refuses", "C19 without its sweep\n", "Land C19's marker")
	setPath, _ := fixture.recordCommitSet("M1")
	mainsBefore := fixture.mains()
	result := fixture.run("merge-out", "M1", setPath)
	if result.exitCode == 0 || !strings.Contains(result.output, "TestX9ReadyForMain did not pass at M1") {
		t.Fatalf("merge-out passed what X9 refuses: %+v", result)
	}
	if !maps.Equal(fixture.mains(), mainsBefore) {
		t.Errorf("a refused merge-out moved a main")
	}
}

// A commit set in plain "<repo> <commit>" lines reads as the report's table does, and a gate that is not
// a milestone is refused.
func TestMergeOutReadsPlainLinesAndRefusesABadGate(t *testing.T) {
	fixture := newBranchesFixture(t)
	lines := strings.Builder{}
	for _, repo := range []string{"connect", "sidecar"} {
		lines.WriteString(repo + " " + fixture.git(repo, "rev-parse", "sidecar") + "\n")
	}
	path := filepath.Join(filepath.Dir(fixture.root), "set.txt")
	if err := os.WriteFile(path, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := fixture.run("merge-out", "M11", path); result.exitCode == 0 {
		t.Errorf("merge-out took gate M11: %+v", result)
	}
	result := fixture.run("merge-out", "M1", path)
	if result.exitCode != 0 {
		t.Fatalf("merge-out failed: %+v", result)
	}
	if order := branchesOrder(result.output, `merge-out: merged (\w+)'s`); !slices.Equal(order, []string{"connect", "sidecar"}) {
		t.Errorf("merge-out merged %v", order)
	}
}

func TestCheckReportsPinsThatMoved(t *testing.T) {
	fixture := newBranchesFixture(t)
	result := fixture.run("check")
	if result.exitCode == 0 || !strings.Contains(result.output, "check: sidecar has moved past its pin") {
		t.Fatalf("check passed stale pins: %+v", result)
	}
	if status := fixture.git("server", "status", "--porcelain"); status != "" {
		t.Errorf("check changed server: %s", status)
	}
}

func TestScanReportsAnAttributedCommit(t *testing.T) {
	fixture := newBranchesFixture(t)
	if result := fixture.run("scan"); result.exitCode != 0 {
		t.Fatalf("scan failed on clean branches: %+v", result)
	}
	fixture.commit("sn", "sn.go", "package sn\n", "Add sn's part\n\nGenerated with Claude Code")
	result := fixture.run("scan")
	if result.exitCode == 0 || !strings.Contains(result.output, "scan: sn carries attributed commits") {
		t.Fatalf("scan passed an attributed commit: %+v", result)
	}
}

// Beside the recorded runs above, the script's own text holds no push, pull or rebase outside its
// comments and the lines it prints for the owner.
func TestScriptNeverPushesOrRebases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "sidecar-branches.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	command := regexp.MustCompile(`\b(push|pull|rebase)\b`)
	for index, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "print ") || strings.HasPrefix(trimmed, "print -r -- ") {
			continue
		}
		if command.MatchString(trimmed) {
			t.Errorf("sidecar-branches.zsh:%d runs %q", index+1, trimmed)
		}
	}
}
