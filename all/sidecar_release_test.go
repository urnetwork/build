// SPDX-License-Identifier: MPL-2.0

// The sidecar repository's release: run.sh's real functions over a synthetic tree, with real git
// against a local bare origin and the real go mod edit. Only tidy and get, the two steps that need
// published modules, are stubs here. The release fixture (sidecar_release_fixture_test.go) runs the
// same functions with those two real, against tags served from a file proxy.

package allbuild

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	sidecarTestVersion         = "2026.11.6-1234567"
	sidecarTestInternalVersion = "2026.11.6+1234567"
	sidecarTestGvisorVersion   = "v0.0.0-20260909230408-9b1144b679cb"
	sidecarRootModule          = "github.com/urnetwork/sidecar"
	smtpproofModule            = "github.com/urnetwork/sidecar/smtpproof"
)

// Every function the release step calls, in dependency order.
var sidecarReleaseFunctionNames = []string{
	"go_edit_require", "go_edit_require_subpackages", "go_mod_drop_require", "go_mod_edit_module",
	"go_mod_edit_require", "go_mod_edit_gvisor", "go_mod_fork_rebase_local_replaces",
	"go_mod_fork_prepare", "go_mod_fork_tidy", "go_mod_fork", "git_commit", "git_tag",
	"sidecar_release_edit", "smtpproof_release_edit", "smtpproof_git_tag", "sidecar_cli_release_edit",
	"sidecar_release_publish", "sidecar_release_consumer_edit",
}

// The release step's source, as run.sh and release-checkpoints.zsh hold it.
func sidecarReleaseFunctions(t *testing.T) string {
	t.Helper()
	var source strings.Builder
	for _, name := range sidecarReleaseFunctionNames {
		source.WriteString(runFunction(t, name))
	}
	return source.String()
}

// gsed on the release host, which is a Mac; GNU sed where this runs elsewhere.
func sidecarTestSed(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"gsed", "sed"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		output, err := exec.Command(path, "--version").CombinedOutput()
		if err == nil && strings.Contains(string(output), "GNU sed") {
			return path
		}
	}
	t.Fatal("no GNU sed: run.sh's rewrites need one")
	return ""
}

// One checkout of the sidecar repository beside a connect holding its sctp fork, as $BUILD_HOME
// holds them, and the bare origin it pushes to.
type sidecarReleaseTree struct {
	buildHome string
	sidecar   string
	remote    string
	eventLog  string
}

// The shape run.sh meets: a root module, three nested modules, the documents reading 5 keeps out
// of the module zip, and quoted first-party paths in Go files and a Makefile.
var sidecarReleaseTreeFiles = map[string]string{
	"go.mod": `module github.com/urnetwork/sidecar

go 1.27.3

require (
	github.com/urnetwork/connect v0.0.0
	github.com/urnetwork/glog v0.0.0
)

replace github.com/urnetwork/connect => ../connect

replace github.com/pion/sctp => ../connect/sctp

replace github.com/urnetwork/glog => ../glog

replace gvisor.dev/gvisor => ../gvisor
`,
	"sidecar.go": `package sidecar

import (
	_ "github.com/urnetwork/connect"
	_ "github.com/urnetwork/connect/protocol"
	_ "github.com/urnetwork/glog"
	_ "github.com/urnetwork/sidecar/protocol"
)

var Version string = ""
`,
	"protocol/protocol.go":   "package protocol\n",
	"Makefile":               "check:\n\tgo list \"github.com/urnetwork/sidecar/protocol\"\n",
	"LICENSE":                "license\n",
	"NOTICE":                 "notice\n",
	"README.md":              "readme\n",
	"RECORD.md":              "record\n",
	"DESIGN.md":              "design\n",
	"DESIGN-REVIEW1.md":      "review\n",
	"HANDOFF.md":             "handoff\n",
	"FEEDBACK-SYSTEM.md":     "feedback\n",
	"design/source/a.md":     "section\n",
	"research_notes/note.md": "note\n",
	"smtpproof/go.mod": `module github.com/urnetwork/sidecar/smtpproof

go 1.27.3

require github.com/urnetwork/sidecar v0.0.0

replace github.com/urnetwork/sidecar => ..

replace github.com/urnetwork/connect => ../../connect

replace github.com/pion/sctp => ../../connect/sctp

replace github.com/urnetwork/glog => ../../glog

replace gvisor.dev/gvisor => ../../gvisor
`,
	"smtpproof/doc.go": "package smtpproof\n\nimport _ \"github.com/urnetwork/sidecar\"\n",
	"cli/go.mod": `module github.com/urnetwork/sidecar/cli

go 1.27.3

require (
	github.com/urnetwork/sidecar v0.0.0
	github.com/urnetwork/sidecar/smtpproof v0.0.0
)

replace github.com/urnetwork/sidecar => ..

replace github.com/urnetwork/sidecar/smtpproof => ../smtpproof

replace github.com/urnetwork/connect => ../../connect

replace github.com/pion/sctp => ../../connect/sctp

replace github.com/urnetwork/glog => ../../glog

replace gvisor.dev/gvisor => ../../gvisor
`,
	"cli/main.go": `package main

import (
	_ "github.com/urnetwork/sidecar"
	_ "github.com/urnetwork/sidecar/smtpproof"
)

func main() {}
`,
	"mlsinterop/go.mod": `module github.com/urnetwork/sidecar/mlsinterop

go 1.27.3

replace github.com/urnetwork/sidecar => ..

replace github.com/urnetwork/connect => ../../connect

replace github.com/pion/sctp => ../../connect/sctp

replace github.com/urnetwork/glog => ../../glog

replace gvisor.dev/gvisor => ../../gvisor
`,
	"mlsinterop/doc.go": "package mlsinterop\n",
}

// A committed checkout on its version branch, with sidecar.go as given.
func newSidecarReleaseTree(t *testing.T, sidecarSource string) *sidecarReleaseTree {
	t.Helper()
	root := t.TempDir()
	tree := &sidecarReleaseTree{
		buildHome: filepath.Join(root, "build"),
		sidecar:   filepath.Join(root, "build", "sidecar"),
		remote:    filepath.Join(root, "remote.git"),
		eventLog:  filepath.Join(root, "events"),
	}
	files := map[string]string{"../connect/sctp/go.mod": "module github.com/pion/sctp\n\ngo 1.27.3\n"}
	for name, contents := range sidecarReleaseTreeFiles {
		files[name] = contents
	}
	if sidecarSource != "" {
		files["sidecar.go"] = sidecarSource
	}
	for name, contents := range files {
		path := filepath.Join(tree.sidecar, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "init", "--bare", tree.remote)
	runGit(t, root, "init", "-b", "v"+sidecarTestVersion, tree.sidecar)
	runGit(t, tree.sidecar, "config", "user.name", "Build Harness Test")
	runGit(t, tree.sidecar, "config", "user.email", "build-harness-test@example.invalid")
	runGit(t, tree.sidecar, "remote", "add", "origin", tree.remote)
	runGit(t, tree.sidecar, "add", ".")
	runGit(t, tree.sidecar, "commit", "-m", "main")
	return tree
}

// Runs script over the release's functions from the checkout, with tidy and get recorded and
// skipped and every push recorded, so a test reads the order they happened in.
func (self *sidecarReleaseTree) run(t *testing.T, script string) (string, error) {
	t.Helper()
	harness := `
set -u
event_log="$1"
eval "$2"
BUILD_SED="$3"
EXTERNAL_WARP_VERSION="$4"
WARP_VERSION="$5"
GVISOR_GO_VERSION="$6"
GO_MOD_VERSION=2026
GO_MOD_SUFFIX=/v2026
sidecar_top="$PWD"
go() {
    case "$1 $2" in
        'mod tidy'|'get -t')
            print -r -- "go $1 $2 in ${PWD#$sidecar_top}" >> "$event_log"
            return 0
            ;;
    esac
    command go "$@"
}
git() {
    if [[ "$1" = push ]]; then
        print -r -- "git $*" >> "$event_log"
    fi
    command git "$@"
}
eval "$7"
`
	command := exec.Command("zsh", "-c", harness, "sidecar-release-test",
		self.eventLog, sidecarReleaseFunctions(t), sidecarTestSed(t),
		sidecarTestVersion, sidecarTestInternalVersion, sidecarTestGvisorVersion, script)
	command.Dir = self.sidecar
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=")
	output, err := command.CombinedOutput()
	return string(output), err
}

// Runs script, which must succeed.
func (self *sidecarReleaseTree) mustRun(t *testing.T, script string) {
	t.Helper()
	if output, err := self.run(t, script); err != nil {
		t.Fatalf("%s failed: %v\n%s", script, err, output)
	}
}

// The tree's file at name, which must exist.
func (self *sidecarReleaseTree) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(self.sidecar, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The recorded tidies, gets and pushes, in order.
func (self *sidecarReleaseTree) events(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(self.eventLog)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// What each go.mod names, read with the real parser's view of a line: the release must leave no
// unversioned first-party require or local replace of a tagged module anywhere.
func requireGoMod(t *testing.T, content string, want []string, forbidden []string) {
	t.Helper()
	for _, line := range want {
		if !strings.Contains(content, line) {
			t.Errorf("go.mod lacks %q:\n%s", line, content)
		}
	}
	for _, line := range forbidden {
		if strings.Contains(content, line) {
			t.Errorf("go.mod still has %q:\n%s", line, content)
		}
	}
}

func TestSidecarReleaseFunctionsEditASyntheticTree(t *testing.T) {
	tree := newSidecarReleaseTree(t, "")
	tree.mustRun(t, "sidecar_release_edit && sidecar_release_publish")

	suffix := "/v2026 v" + sidecarTestVersion
	requireGoMod(t, tree.read(t, "v2026/go.mod"),
		[]string{"module github.com/urnetwork/sidecar/v2026", "github.com/urnetwork/connect" + suffix, "github.com/urnetwork/glog" + suffix,
			"gvisor.dev/gvisor => github.com/urnetwork/gvisor " + sidecarTestGvisorVersion, "github.com/pion/sctp => ../../connect/sctp"},
		[]string{"github.com/urnetwork/connect v0.0.0", "github.com/urnetwork/glog v0.0.0", "=> ../connect", "=> ../glog", "=> ../gvisor"})
	requireGoMod(t, tree.read(t, "smtpproof/v2026/go.mod"),
		[]string{"module github.com/urnetwork/sidecar/smtpproof/v2026", "github.com/urnetwork/sidecar" + suffix,
			"github.com/pion/sctp => ../../../connect/sctp", "gvisor.dev/gvisor => github.com/urnetwork/gvisor " + sidecarTestGvisorVersion},
		[]string{"github.com/urnetwork/sidecar v0.0.0", "github.com/urnetwork/sidecar =>", "=> ..\n"})
	requireGoMod(t, tree.read(t, "cli/go.mod"),
		[]string{"github.com/urnetwork/sidecar" + suffix, "github.com/urnetwork/sidecar/smtpproof" + suffix,
			"github.com/pion/sctp => ../../connect/sctp", "gvisor.dev/gvisor => github.com/urnetwork/gvisor " + sidecarTestGvisorVersion},
		[]string{"github.com/urnetwork/sidecar v0.0.0", "github.com/urnetwork/sidecar/smtpproof v0.0.0", "=> ..\n", "=> ../smtpproof"})
	requireGoMod(t, tree.read(t, "mlsinterop/go.mod"),
		[]string{"gvisor.dev/gvisor => github.com/urnetwork/gvisor " + sidecarTestGvisorVersion},
		[]string{"github.com/urnetwork/sidecar =>", "=> ../../connect\n", "=> ../../glog"})

	// quoted paths, the root's own included, in Go files and Makefiles of every module
	source := tree.read(t, "v2026/sidecar.go")
	for _, want := range []string{
		`"github.com/urnetwork/connect/v2026"`, `"github.com/urnetwork/connect/v2026/protocol"`, `"github.com/urnetwork/glog/v2026"`,
		`"github.com/urnetwork/sidecar/v2026/protocol"`, `var Version string = "` + sidecarTestInternalVersion + `"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("sidecar.go lacks %s:\n%s", want, source)
		}
	}
	if makefile := tree.read(t, "v2026/Makefile"); !strings.Contains(makefile, `"github.com/urnetwork/sidecar/v2026/protocol"`) {
		t.Errorf("the Makefile's quoted path was not rewritten:\n%s", makefile)
	}
	main := tree.read(t, "cli/main.go")
	for _, want := range []string{`"github.com/urnetwork/sidecar/v2026"`, `"github.com/urnetwork/sidecar/smtpproof/v2026"`} {
		if !strings.Contains(main, want) {
			t.Errorf("cli/main.go lacks %s:\n%s", want, main)
		}
	}

	// the tree moved, the nested modules stayed, and the documents stayed out of the module
	for _, name := range []string{"v2026/LICENSE", "v2026/NOTICE", "v2026/README.md", "v2026/RECORD.md", "v2026/protocol/protocol.go",
		"DESIGN.md", "DESIGN-REVIEW1.md", "HANDOFF.md", "FEEDBACK-SYSTEM.md", "design/source/a.md", "research_notes/note.md",
		"cli/main.go", "mlsinterop/doc.go", "smtpproof/v2026/doc.go"} {
		if _, err := os.Stat(filepath.Join(tree.sidecar, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s is not where the release leaves it: %v", name, err)
		}
	}
	for _, name := range []string{"v2026/design", "v2026/research_notes", "v2026/DESIGN.md", "v2026/HANDOFF.md", "v2026/cli", "v2026/mlsinterop", "v2026/smtpproof", "smtpproof/doc.go", "sidecar.go"} {
		if _, err := os.Stat(filepath.Join(tree.sidecar, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should not exist on the version branch: %v", name, err)
		}
	}
}

// A module's tag is immutable, and its consumers tidy against the published tag: root, then
// smtpproof, then cli and mlsinterop, which are never tagged.
func TestSidecarReleasePublishesEachTagBeforeItsConsumerTidies(t *testing.T) {
	tree := newSidecarReleaseTree(t, "")
	tree.mustRun(t, "sidecar_release_edit && sidecar_release_publish")

	rootTag := "git push origin refs/tags/v" + sidecarTestVersion
	smtpproofTag := "git push origin refs/tags/smtpproof/v" + sidecarTestVersion
	events := tree.events(t)
	want := []string{"go mod tidy in /v2026", "go get -t in /v2026", rootTag, "go mod tidy in /smtpproof/v2026", "go get -t in /smtpproof/v2026",
		smtpproofTag, "go mod tidy in /cli", "go mod tidy in /mlsinterop"}
	position := -1
	for _, event := range want {
		index := slices.Index(events, event)
		if index <= position {
			t.Fatalf("%q is out of order in:\n%s", event, strings.Join(events, "\n"))
		}
		position = index
	}

	tags := strings.Fields(runGit(t, tree.buildHome, "--git-dir", tree.remote, "for-each-ref", "--format=%(refname)", "refs/tags"))
	slices.Sort(tags)
	if wantTags := []string{"refs/tags/smtpproof/v" + sidecarTestVersion, "refs/tags/v" + sidecarTestVersion}; !slices.Equal(tags, wantTags) {
		t.Fatalf("published tags = %v, want %v", tags, wantTags)
	}
	branchHead := runGit(t, tree.buildHome, "--git-dir", tree.remote, "rev-parse", "refs/heads/v"+sidecarTestVersion)
	if head := runGit(t, tree.sidecar, "rev-parse", "HEAD"); branchHead != head {
		t.Fatalf("the version branch pushed %s, and HEAD is %s", branchHead, head)
	}
	for tag, directory := range map[string]string{"v" + sidecarTestVersion: "v2026", "smtpproof/v" + sidecarTestVersion: "smtpproof/v2026"} {
		// a version branch shares its tag's name, so the ref is spelled out
		tagged := runGit(t, tree.sidecar, "rev-parse", "refs/tags/"+tag+"^{commit}:"+directory)
		if head := runGit(t, tree.sidecar, "rev-parse", "HEAD:"+directory); tagged != head {
			t.Errorf("%s holds %s at its tag and %s at HEAD", directory, tagged, head)
		}
	}
}

// Go finds .../smtpproof/v2026 through a tag under smtpproof/, without the /v2026 directory.
func TestRunTagsSmtpproofUnderItsDirectory(t *testing.T) {
	tree := newSidecarReleaseTree(t, "")
	tree.mustRun(t, `
release_git_tag() { print -r -- "tag $1 $2" >> "$event_log" }
smtpproof_git_tag`)
	head := runGit(t, tree.sidecar, "rev-parse", "HEAD")
	if want := "tag smtpproof/v" + sidecarTestVersion + " " + head; !slices.Contains(tree.events(t), want) {
		t.Fatalf("events %v do not hold %q", tree.events(t), want)
	}
}

// A sed that matches nothing still exits zero, so the step reads its own result. Nothing may move
// or commit after a miss.
func TestRunFailsWhenTheVersionSedMisses(t *testing.T) {
	for _, source := range []string{"package sidecar\n\nvar Version = \"\"\n", "package sidecar\n\nconst Version string = \"1\"\n", "package sidecar\n"} {
		tree := newSidecarReleaseTree(t, source)
		output, err := tree.run(t, "sidecar_release_edit")
		if err == nil {
			t.Errorf("a sidecar.go of %q passed the Version check:\n%s", source, output)
		}
		if _, err := os.Stat(filepath.Join(tree.sidecar, "v2026")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the tree moved after the sed missed: %v", err)
		}
	}
}

// An empty version would fill the declaration with what it already holds, and read as a hit.
func TestRunFailsOnAnEmptyVersion(t *testing.T) {
	tree := newSidecarReleaseTree(t, "")
	if output, err := tree.run(t, `WARP_VERSION=; sidecar_release_edit`); err == nil {
		t.Fatalf("an empty WARP_VERSION passed:\n%s", output)
	}
}

// The order is the point: the other one writes a package that does not exist.
func TestRunRewritesSmtpproofBeforeTheRootInEveryRepo(t *testing.T) {
	tree := newSidecarReleaseTree(t, "")
	tree.mustRun(t, "go_edit_require_subpackages github.com/urnetwork/sidecar/smtpproof && go_edit_require_subpackages github.com/urnetwork/sidecar")
	if main := tree.read(t, "cli/main.go"); !strings.Contains(main, `"github.com/urnetwork/sidecar/smtpproof/v2026"`) || strings.Contains(main, `sidecar/v2026/smtpproof`) {
		t.Fatalf("smtpproof first did not hold its path:\n%s", main)
	}
	reversed := newSidecarReleaseTree(t, "")
	reversed.mustRun(t, "go_edit_require_subpackages github.com/urnetwork/sidecar && go_edit_require_subpackages github.com/urnetwork/sidecar/smtpproof")
	if main := reversed.read(t, "cli/main.go"); !strings.Contains(main, `sidecar/v2026/smtpproof`) {
		t.Fatalf("the reversed order should write the broken path, or this test shows nothing:\n%s", main)
	}

	// each repository that names both, in the order its own block runs them
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	blocks := map[string]string{"sidecar": runFunction(t, "sidecar_release_edit"), "server": componentRegion(t, "(cd $BUILD_HOME/server &&\n    go_mod_edit_module", "error_trap 'server edit'")}
	for repository, block := range blocks {
		smtpproof := strings.Index(block, "go_edit_require_subpackages "+smtpproofModule)
		root := strings.Index(block, "go_edit_require_subpackages "+sidecarRootModule+" ")
		if repository == "server" {
			root = strings.Index(block, "sidecar_release_consumer_edit")
		}
		if smtpproof < 0 || root < 0 || smtpproof > root {
			t.Errorf("%s's block rewrites smtpproof at %d and the root at %d", repository, smtpproof, root)
		}
	}
	// sn drops both, so it names neither in a sed, and nothing else may name smtpproof
	if count := strings.Count(source, "go_edit_require_subpackages "+smtpproofModule); count != 2 {
		t.Errorf("smtpproof's rewrite appears %d times, and each new one needs this test's order check", count)
	}
}

// Each consumer takes the tags, in its own block, and sn takes neither: it drops server, whose
// requirements these are, and reaches the root through the sdk's tag.
func TestRunPointsEachSidecarConsumerAtTheTags(t *testing.T) {
	consumers := map[string]string{
		"sdk build": "error_trap 'sdk build edit'",
		"sdk cgo":   "error_trap 'sdk cgo edit'",
		"sdk js":    "error_trap 'sdk js edit'",
		"sdk":       "error_trap 'sdk edit'",
		"server":    "error_trap 'server edit'",
	}
	starts := map[string]string{
		"sdk build": "(cd $BUILD_HOME/sdk/build &&\n    go_mod_edit_gvisor",
		"sdk cgo":   "(cd $BUILD_HOME/sdk/cgo &&\n    go_mod_edit_gvisor",
		"sdk js":    "(cd $BUILD_HOME/sdk/js &&\n    go_mod_edit_gvisor",
		"sdk":       "(cd $BUILD_HOME/sdk &&\n    go_mod_edit_module",
		"server":    "(cd $BUILD_HOME/server &&\n    go_mod_edit_module",
	}
	for name, end := range consumers {
		if block := componentRegion(t, starts[name], end); strings.Count(block, "sidecar_release_consumer_edit") != 1 {
			t.Errorf("%s's block does not take the sidecar tag once:\n%s", name, block)
		}
	}
	sn := componentRegion(t, "(cd $BUILD_HOME/sn &&\n    go_mod_edit_module", "error_trap 'sn edit'")
	server := strings.Index(sn, "go_mod_drop_require github.com/urnetwork/server")
	smtpproof := strings.Index(sn, "go_mod_drop_require "+smtpproofModule)
	root := strings.Index(sn, "go_mod_drop_require "+sidecarRootModule+" ")
	fork := strings.Index(sn, "go_mod_fork 'sim-testnet'")
	if server < 0 || smtpproof < 0 || root < 0 || fork < 0 || !(server < smtpproof && smtpproof < root && root < fork) {
		t.Errorf("sn drops server at %d, smtpproof at %d and the root at %d, before its fork at %d", server, smtpproof, root, fork)
	}
	if strings.Contains(sn, "sidecar_release_consumer_edit") || strings.Contains(sn, "go_mod_edit_require "+sidecarRootModule) {
		t.Error("sn requires a sidecar tag of its own, and imports nothing of sidecar's")
	}
	if server := componentRegion(t, starts["server"], consumers["server"]); !strings.Contains(server, "go_mod_edit_require "+smtpproofModule) {
		t.Error("server does not take smtpproof's tag")
	}
	for _, name := range []string{"sdk build", "sdk cgo", "sdk js", "sdk"} {
		if strings.Contains(componentRegion(t, starts[name], consumers[name]), "smtpproof") {
			t.Errorf("%s names smtpproof, and sdk's graph must hold no prover", name)
		}
	}
}

// §4.7's order: after connect's and glog's tags, which it requires, and before sdk's first edit,
// which requires its tag, all inside the block a resume never replays.
func TestRunReleasesSidecarBetweenConnectAndSdk(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	order := []string{
		"error_trap 'glog push branch'", "error_trap 'connect push branch'", "error_trap 'sidecar edit'",
		"error_trap 'sidecar push branch'", "error_trap 'sidecar make check'", "error_trap 'sdk build edit'",
		"error_trap 'sdk edit'", "fi # immutable release preparation is complete",
	}
	position := -1
	for _, stage := range order {
		if strings.Count(source, stage) != 1 {
			t.Fatalf("run.sh holds %q %d times, want once", stage, strings.Count(source, stage))
		}
		index := strings.Index(source, stage)
		if index < position {
			t.Fatalf("%q is out of order", stage)
		}
		position = index
	}
	// the block that opens at glog's edit is the one the comment above closes
	opening := "if [[ -z \"${BUILD_RESUME:-}\" ]]; then\n(cd $BUILD_HOME/glog &&"
	start := strings.LastIndex(source[:strings.Index(source, "error_trap 'sidecar edit'")], `if [[ -z "${BUILD_RESUME:-}" ]]; then`)
	if start < 0 || !strings.HasPrefix(source[start:], opening) {
		t.Fatal("the sidecar step sits outside the block a resume skips, and a resume would replay a rewrite")
	}
	for _, pair := range [][2]string{
		{"(cd $BUILD_HOME/connect && git_main)", "(cd $BUILD_HOME/sidecar && git_main)"},
		{"(cd $BUILD_HOME/connect && git checkout -b v${EXTERNAL_WARP_VERSION})", "(cd $BUILD_HOME/sidecar && git checkout -b v${EXTERNAL_WARP_VERSION})"},
	} {
		connect := strings.Index(source, pair[0])
		sidecar := strings.Index(source, pair[1])
		if connect < 0 || sidecar < 0 || sidecar < connect {
			t.Errorf("sidecar does not follow connect: %q at %d, %q at %d", pair[0], connect, pair[1], sidecar)
		}
	}
	for _, want := range []string{"(cd $BUILD_HOME && rm -rf sidecar)", "for scan_dir in warp sdk server sidecar; do"} {
		if !strings.Contains(source, want) {
			t.Errorf("run.sh lacks %q", want)
		}
	}
}

// §4.9: output is committed, so nothing regenerates it on a version branch. run.sh names protoc
// only as the tool make check needs, and a required tool is checked before any work starts.
func TestRunNeverRegeneratesSidecarProtobuf(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	lines := regexp.MustCompile(`(?m)^[^#\n]*\bprotoc\b.*$`).FindAllString(source, -1)
	if !slices.Equal(lines, []string{"    protoc"}) {
		t.Errorf("run.sh mentions protoc outside required_build_tools: %q", lines)
	}
	if tools := componentRegion(t, "required_build_tools=(", ")"); !strings.Contains(tools, "\n    protoc\n") {
		t.Error("protoc is not a required build tool, and make check fails without it")
	}
	region := componentRegion(t, "(cd $BUILD_HOME/sidecar &&\n    sidecar_release_edit)", "(cd $BUILD_HOME/sdk/build &&")
	for _, name := range sidecarReleaseFunctionNames {
		if strings.HasPrefix(name, "sidecar") || strings.HasPrefix(name, "smtpproof") {
			region += runFunction(t, name)
		}
	}
	for _, forbidden := range []string{"make proto", "protoc", "go generate", "protoc-gen-go"} {
		if strings.Contains(region, forbidden) {
			t.Errorf("the release step runs %q", forbidden)
		}
	}
}

// A later commit that changes a tagged tree would leave the version branch disagreeing with an
// immutable tag, so the step fails rather than publishing that.
func TestSidecarReleaseRefusesAChangedTaggedTree(t *testing.T) {
	for _, directory := range []string{"v2026", "smtpproof/v2026"} {
		tree := newSidecarReleaseTree(t, "")
		script := fmt.Sprintf(`
sidecar_release_edit || exit 7
sidecar_cli_release_edit() { print -r -- changed >> %s/late.txt }
sidecar_release_publish`, directory)
		output, err := tree.run(t, script)
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() == 7 {
			t.Errorf("a change to %s passed: %v\n%s", directory, err, output)
		}
	}
}

// A failed tidy stops the step before any later tag.
func TestSidecarReleaseStopsAtAFailedSmtpproofTidy(t *testing.T) {
	tree := newSidecarReleaseTree(t, "")
	script := `
sidecar_release_edit || exit 7
go_mod_fork_tidy() { if [[ "$PWD" = */smtpproof ]]; then return 41; fi }
sidecar_release_publish`
	output, err := tree.run(t, script)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 41 {
		t.Fatalf("exit = %v, want 41:\n%s", err, output)
	}
	if tags := runGit(t, tree.buildHome, "--git-dir", tree.remote, "tag"); tags != "v"+sidecarTestVersion {
		t.Fatalf("tags after a failed smtpproof tidy = %q, want only the root's", tags)
	}
}

// The consumer's two lines leave no unversioned require or replace, and smtpproof's first pass
// holds its path against the root's.
func TestSidecarReleaseConsumerEditRewritesAConsumer(t *testing.T) {
	tree := newSidecarReleaseTree(t, "")
	consumer := filepath.Join(tree.buildHome, "server")
	if err := os.MkdirAll(consumer, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod": `module github.com/urnetwork/server

go 1.27.3

require (
	github.com/urnetwork/sidecar v0.0.0
	github.com/urnetwork/sidecar/smtpproof v0.0.0
)

replace github.com/urnetwork/sidecar => ../sidecar

replace github.com/urnetwork/sidecar/smtpproof => ../sidecar/smtpproof
`,
		"sidecar_require.go": "//go:build tools\n\npackage server\n\nimport (\n\t_ \"github.com/urnetwork/sidecar\"\n\t_ \"github.com/urnetwork/sidecar/protocol\"\n\t_ \"github.com/urnetwork/sidecar/smtpproof\"\n)\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(consumer, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree.mustRun(t, `cd ../server &&
go_mod_edit_require github.com/urnetwork/sidecar/smtpproof &&
go_edit_require_subpackages github.com/urnetwork/sidecar/smtpproof &&
sidecar_release_consumer_edit`)
	goMod, err := os.ReadFile(filepath.Join(consumer, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	suffix := "/v2026 v" + sidecarTestVersion
	requireGoMod(t, string(goMod), []string{"github.com/urnetwork/sidecar" + suffix, "github.com/urnetwork/sidecar/smtpproof" + suffix},
		[]string{"github.com/urnetwork/sidecar v0.0.0", "github.com/urnetwork/sidecar/smtpproof v0.0.0", "=> ../sidecar"})
	source, err := os.ReadFile(filepath.Join(consumer, "sidecar_require.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"github.com/urnetwork/sidecar/v2026"`, `"github.com/urnetwork/sidecar/v2026/protocol"`, `"github.com/urnetwork/sidecar/smtpproof/v2026"`} {
		if !strings.Contains(string(source), want) {
			t.Errorf("sidecar_require.go lacks %s:\n%s", want, source)
		}
	}
}

// A broken rewrite must stop a release before any tag exists, so the fixture and §4.9's size gates
// run in the candidate gate, and a version is allocated only after them.
func TestRunGatesTheSidecarFixtureBeforeAVersionExists(t *testing.T) {
	region := componentRegion(t, "    builder_message \"Build all test candidate\"\n", "warpctl stage version next release")
	fixture := strings.Index(region, "-tags sidecar_release_fixture -run 'Fixture' .)\n    error_trap 'sidecar release fixture'")
	size := strings.Index(region, "(cd \"$BUILD_HOME/sidecar\" && make size)\n    error_trap 'sidecar make size'")
	passed := strings.Index(region, "A version number can now be assigned.")
	if fixture < 0 || size < 0 || passed < 0 || !(fixture < size && size < passed) {
		t.Fatalf("candidate gate order is fixture=%d size=%d passed=%d", fixture, size, passed)
	}
}
