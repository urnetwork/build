//go:build sidecar_release_fixture

// SPDX-License-Identifier: MPL-2.0

// §22.4's release fixture: the release's real code, over copies of the repositories, with each tag
// served from a file proxy, then §4.9's consumers at those tags. run.sh's regions and functions run
// as they stand, with four stand-ins that keep it local: commits and tags stay in the copies, a tag
// also writes its module into the proxy, gVisor keeps its local path, and npm has nothing to do.
// Nothing is pushed, and no value comes from vault.
//
// The real run goes on through run.sh's make check in the moved module, as the release does once
// both tags exist, so a rule or test that only the release layout breaks stops a release here, while
// no version does. That takes the whole of make check, smtpproof's circuits included.
//
// A consumer here has no replaces, so it links upstream gVisor and pion/sctp, which compile. This
// proves module paths and nothing about the forks: `ur` as the release builds it keeps both.
//
// The real module cache is read through the proxy and never written. This stays out of plain go
// test, as make size does, because it builds connect twice. run.sh runs it before a version exists,
// so a broken rewrite stops a release before any tag does.
//
//	go test -tags sidecar_release_fixture -timeout 60m -run 'Fixture' .
//
// run from this directory without -trimpath, because rolloutRoot reads this file's path.

package allbuild

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	fixtureExternalVersion = "2026.11.6-1234567"
	fixtureInternalVersion = "2026.11.6+1234567"
	fixtureSidecarRoot     = "github.com/urnetwork/sidecar/v2026"
	fixtureSmtpproof       = "github.com/urnetwork/sidecar/smtpproof/v2026"
	fixtureConnectRoot     = "github.com/urnetwork/connect/v2026"
	fixtureSdkRoot         = "github.com/urnetwork/sdk/v2026"
	// how a shell step calls back into this binary to publish a tag
	fixturePublishTagName        = "SIDECAR_FIXTURE_PUBLISH_TAG"
	fixturePublishRepositoryName = "SIDECAR_FIXTURE_PUBLISH_REPOSITORY"
	fixturePublishProxyName      = "SIDECAR_FIXTURE_PUBLISH_PROXY"
)

// run.sh's functions the regions call. go_mod_edit_gvisor, git_commit and release_git_tag are the
// harness's stand-ins, and npm_fork_update has nothing to do here.
var fixtureFunctionNames = []string{
	"go_edit_require", "go_edit_require_subpackages", "go_mod_drop_require", "go_mod_edit_module",
	"go_mod_edit_require", "go_mod_fork_rebase_local_replaces", "go_mod_fork_prepare", "go_mod_fork_tidy",
	"go_mod_fork", "go_mod_fork_update", "git_tag", "sidecar_release_edit", "smtpproof_release_edit",
	"smtpproof_git_tag", "sidecar_cli_release_edit", "sidecar_release_publish", "sidecar_release_consumer_edit",
}

// Where sidecar's region ends, and where it ends once it runs on to make check, which run.sh runs
// from the moved module after both tags are pushed.
const (
	fixtureSidecarRegionEnd = "error_trap 'sidecar push branch'"
	fixtureMakeCheckEnd     = "error_trap 'sidecar make check'"
)

// run.sh's own blocks, start and end, in the order it runs them. proxy and userwireguard are left
// out, because nothing here requires them.
var fixtureRegions = [][2]string{
	{"(cd $BUILD_HOME/glog &&\n    go_mod_edit_module", "error_trap 'glog push branch'"},
	{"(cd $BUILD_HOME/connect &&\n    go_mod_edit_module", "error_trap 'connect push branch'"},
	{"(cd $BUILD_HOME/goidenticons &&\n    go_mod_edit_module", "error_trap 'goidenticons push branch'"},
	{"(cd $BUILD_HOME/sidecar &&\n    sidecar_release_edit)", fixtureSidecarRegionEnd},
	{"(cd $BUILD_HOME/sdk/build &&\n    go_mod_edit_gvisor", "error_trap 'sdk push branch'"},
}

const fixtureHarness = `
set -u
eval "$1"
error_trap () {
    code=$?
    if [ $code != 0 ]; then
        print -u2 -- "fixture step failed ($code): $1"
        exit $code
    fi
}
go_mod_edit_gvisor () { return 0 }
npm_fork_update () { return 0 }
git_commit () {
    git add . &&
    if ! (git diff --quiet && git diff --cached --quiet); then
        git commit -q -m "${EXTERNAL_WARP_VERSION}"
    fi
}
release_git_tag () {
    git tag -a "$1" -m "$1" "$2" &&
    SIDECAR_FIXTURE_PUBLISH_TAG="$1" SIDECAR_FIXTURE_PUBLISH_REPOSITORY="$PWD" \
        "$FIXTURE_BINARY" -test.run='^TestFixturePublishesATag$'
}
eval "$2"
`

// One run: copies of the repositories, a proxy and an empty module cache.
type releaseFixture struct {
	t         *testing.T
	root      string
	buildHome string
	proxy     *fileProxy
	// the go environment every step and check runs under
	environment []string
	// whether the release runs on to run.sh's make check, which reads the real tree's siblings
	makeCheck bool
}

// The module cache's download directory, which the proxy falls back on for third-party modules.
func fixtureRealCache(t *testing.T) string {
	t.Helper()
	command := exec.Command("go", "env", "GOMODCACHE")
	command.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(strings.TrimSpace(string(output)), "cache", "download")
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	root := t.TempDir()
	proxy := &fileProxy{directory: filepath.Join(root, "tags")}
	if err := os.MkdirAll(proxy.directory, 0o755); err != nil {
		t.Fatal(err)
	}
	// The go command leaves the module cache's directories read-only, and make check's rules run go
	// with GOFLAGS of their own, without -modcacherw, so the temporary directory could not be removed.
	// Cleanups run last first, so this opens the directories before t.TempDir's removal.
	modcache := filepath.Join(root, "modcache")
	t.Cleanup(func() {
		filepath.WalkDir(modcache, func(walkPath string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				os.Chmod(walkPath, 0o755)
			}
			return nil
		})
	})
	return &releaseFixture{
		t:         t,
		root:      root,
		buildHome: filepath.Join(root, "work"),
		proxy:     proxy,
		environment: append(os.Environ(),
			"GOWORK=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0",
			// -modcacherw only so the temp directory can be removed afterwards
			"GOFLAGS=-mod=mod -modcacherw",
			"GOPROXY="+proxy.url()+",file://"+fixtureRealCache(t),
			"GOMODCACHE="+modcache,
			"GONOPROXY=", "GOPRIVATE=", "GONOSUMDB=", "GONOSUMCHECK=1", "GOVCS=*:off",
		),
	}
}

// Commits directory as the repository's start, on the version branch, keeping what its own
// .gitignore would hide: the copy must equal the tracked tree it came from.
func (self *releaseFixture) commitCopy(directory string) {
	self.t.Helper()
	runGit(self.t, directory, "init", "-q", "-b", "v"+fixtureExternalVersion)
	runGit(self.t, directory, "config", "user.name", "Build Harness Test")
	runGit(self.t, directory, "config", "user.email", "build-harness-test@example.invalid")
	runGit(self.t, directory, "config", "core.excludesFile", "/dev/null")
	runGit(self.t, directory, "add", "-f", ".")
	runGit(self.t, directory, "commit", "-q", "-m", "main")
}

// Where a repository's checkout is: a populated submodule of this repository on the release host,
// and otherwise the sibling of this repository, as in the monoroot.
func fixtureCheckout(t *testing.T, name string) string {
	t.Helper()
	build := filepath.Dir(rolloutRoot(t))
	for _, candidate := range []string{filepath.Join(build, name), filepath.Join(filepath.Dir(build), name)} {
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			return candidate
		}
	}
	t.Fatalf("no checkout of %s beside %s", name, build)
	return ""
}

// Copies a repository's HEAD into the work tree with its own new history, so its tags stay local.
func (self *releaseFixture) archive(name string) {
	self.t.Helper()
	destination := filepath.Join(self.buildHome, name)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		self.t.Fatal(err)
	}
	command := exec.Command("sh", "-c", `git -C "$1" archive HEAD | tar -x -C "$2"`, "fixture-archive", fixtureCheckout(self.t, name), destination)
	if output, err := command.CombinedOutput(); err != nil {
		self.t.Fatalf("archive %s: %v\n%s", name, err, output)
	}
	self.commitCopy(destination)
}

// Writes a small repository in place of a real one, for the runs that plant a failure.
func (self *releaseFixture) write(name string, files map[string]string) {
	self.t.Helper()
	destination := filepath.Join(self.buildHome, name)
	for file, content := range files {
		filePath := filepath.Join(destination, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			self.t.Fatal(err)
		}
		if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
			self.t.Fatal(err)
		}
	}
	self.commitCopy(destination)
}

// run.sh's regions, as one script. With withMakeCheck, sidecar's region runs on through run.sh's
// make check: a failure only the moved module shows would otherwise come after both tags exist, and
// burn the version, so the fixture meets it first, where nothing is lost.
func fixtureRegionSource(t *testing.T, withMakeCheck bool) string {
	t.Helper()
	var source strings.Builder
	for _, region := range fixtureRegions {
		end := region[1]
		if withMakeCheck && end == fixtureSidecarRegionEnd {
			end = fixtureMakeCheckEnd
		}
		source.WriteString(componentRegion(t, region[0], end))
		source.WriteString(end + "\n")
	}
	return source.String()
}

// Runs the release over the copies. patch, when given, plants a change in the functions' text.
func (self *releaseFixture) release(patch func(string) string) (string, error) {
	self.t.Helper()
	var functions strings.Builder
	for _, name := range fixtureFunctionNames {
		functions.WriteString(runFunction(self.t, name))
	}
	source := functions.String()
	if patch != nil {
		source = patch(source)
	}
	binary, err := os.Executable()
	if err != nil {
		self.t.Fatal(err)
	}
	command := exec.Command("zsh", "-c", fixtureHarness, "sidecar-release-fixture", source, fixtureRegionSource(self.t, self.makeCheck))
	command.Dir = self.buildHome
	command.Env = append(slices.Clone(self.environment),
		"BUILD_HOME="+self.buildHome, "BUILD_SED="+sidecarTestSed(self.t), "FIXTURE_BINARY="+binary,
		"EXTERNAL_WARP_VERSION="+fixtureExternalVersion, "WARP_VERSION="+fixtureInternalVersion,
		"GO_MOD_VERSION=2026", "GO_MOD_SUFFIX=/v2026", fixturePublishProxyName+"="+self.proxy.directory)
	output, err := command.CombinedOutput()
	return string(output), err
}

// Not a test of its own: a shell step calls back into this binary, so a tag reaches the proxy before
// the next module tidies against it. Go looks for a tag's module in the major version's directory,
// then in the directory the tag names, which is where a repository with no fork keeps it.
func TestFixturePublishesATag(t *testing.T) {
	tag := os.Getenv(fixturePublishTagName)
	if tag == "" {
		t.Skip("only a fixture step calls this")
	}
	repository := os.Getenv(fixturePublishRepositoryName)
	prefix, version := path.Split(tag)
	commit := runGit(t, repository, "rev-parse", "refs/tags/"+tag+"^{commit}")
	directory := strings.TrimSuffix(prefix, "/")
	if exec.Command("git", "-C", repository, "cat-file", "-e", commit+":"+prefix+"v2026/go.mod").Run() == nil {
		directory = prefix + "v2026"
	}
	proxy := &fileProxy{directory: os.Getenv(fixturePublishProxyName)}
	if _, err := proxy.publish(repository, commit, directory, version); err != nil {
		t.Fatal(err)
	}
}

// Runs the go command in directory, with the fixture's environment and any extra.
func (self *releaseFixture) goCommand(directory string, extra []string, arguments ...string) (string, error) {
	command := exec.Command("go", arguments...)
	command.Dir = directory
	command.Env = append(slices.Clone(self.environment), extra...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("go %s: %w\n%s", strings.Join(arguments, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// go list -m all in directory. -e because the go command asks the proxy for a .info it does not need
// here, and the real cache holds only the .mod of a module nothing compiles. The proxy stays offline
// on purpose: a network fallback could resolve a path the rewrite got wrong, and pass. A module that
// does not resolve still prints its path, which is what the checks read.
func (self *releaseFixture) moduleList(directory string) (string, error) {
	return self.goCommand(directory, nil, "list", "-e", "-m", "all")
}

// §4.9: the modules go list -m all shows hold one root path, the versioned one, and neither the
// unversioned path nor the one a misordered rewrite writes.
func sidecarPathFindings(label string, moduleList string) []string {
	findings := []string{}
	roots := 0
	for _, line := range strings.Split(moduleList, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch modulePath := fields[0]; {
		case modulePath == fixtureSidecarRoot:
			roots += 1
		case modulePath == sidecarRootModule, modulePath == smtpproofModule:
			findings = append(findings, fmt.Sprintf("%s: the unversioned %s is in the graph", label, modulePath))
		case strings.HasPrefix(modulePath, fixtureSidecarRoot+"/"):
			findings = append(findings, fmt.Sprintf("%s: %s is in the graph, and a rewrite wrote it", label, modulePath))
		}
	}
	if roots != 1 {
		findings = append(findings, fmt.Sprintf("%s: the graph holds %s %d times, want once", label, fixtureSidecarRoot, roots))
	}
	return findings
}

// §4.9: the program listing protoregistry.GlobalFiles names each descriptor once. A descriptor that
// a second module identity registers panics at init under the policy the caller sets, before this
// listing exists, which fixtureDescriptorFindings reports.
func descriptorListingFindings(listing string, wantPaths []string) []string {
	findings := []string{}
	lines := strings.Fields(listing)
	for _, want := range wantPaths {
		if count := strings.Count("\n"+strings.Join(lines, "\n")+"\n", "\n"+want+"\n"); count != 1 {
			findings = append(findings, fmt.Sprintf("%s is registered %d times, want once", want, count))
		}
	}
	return findings
}

// §4.9: sdk's module graph holds no smtpproof, and no module of a prover. graph is go mod graph's
// output, and proverModules the paths that only smtpproof brings.
func proverFindings(label string, graph string, proverModules []string) []string {
	findings := []string{}
	seen := map[string]bool{}
	for _, field := range strings.Fields(graph) {
		modulePath, _, _ := strings.Cut(field, "@")
		if seen[modulePath] {
			continue
		}
		seen[modulePath] = true
		if strings.Contains(modulePath, "smtpproof") || slices.Contains(proverModules, modulePath) {
			findings = append(findings, fmt.Sprintf("%s: %s is in sdk's module graph", label, modulePath))
		}
	}
	return findings
}

// The package paths and registered names of the module's .proto files, by §4.9's naming.
func (self *releaseFixture) protoPaths() []string {
	self.t.Helper()
	module := filepath.Join(self.buildHome, "sidecar", "v2026")
	names := []string{}
	for _, pattern := range []string{"protocol/*.proto", "protocol/bridge/*.proto"} {
		matches, err := filepath.Glob(filepath.Join(module, filepath.FromSlash(pattern)))
		if err != nil {
			self.t.Fatal(err)
		}
		for _, match := range matches {
			relative, err := filepath.Rel(module, match)
			if err != nil {
				self.t.Fatal(err)
			}
			names = append(names, filepath.ToSlash(relative))
		}
	}
	return names
}

// The first <svc>service package at the tag.
func (self *releaseFixture) enginePackage() string {
	self.t.Helper()
	matches, err := filepath.Glob(filepath.Join(self.buildHome, "sidecar", "v2026", "*service"))
	if err != nil {
		self.t.Fatal(err)
	}
	slices.Sort(matches)
	if len(matches) == 0 {
		self.t.Fatal("no <svc>service package at the tag, and §4.9's fixture imports one")
	}
	return filepath.Base(matches[0])
}

// The module versions root's go.mod selects for protobuf, so a scratch module takes the same one.
func (self *releaseFixture) protobufVersion() string {
	self.t.Helper()
	data, err := os.ReadFile(filepath.Join(self.buildHome, "sidecar", "v2026", "go.mod"))
	if err != nil {
		self.t.Fatal(err)
	}
	match := regexp.MustCompile(`google\.golang\.org/protobuf (v[^ \n]+)`).FindSubmatch(data)
	if match == nil {
		self.t.Fatal("the root's go.mod requires no protobuf")
	}
	return string(match[1])
}

// A consumer with no replace line: it imports the root, protocol and an engine package at the tag,
// and a second main lists what registered.
func (self *releaseFixture) scratchFindings() []string {
	self.t.Helper()
	findings := []string{}
	scratch := filepath.Join(self.root, "scratch")
	files := map[string]string{
		"go.mod": fmt.Sprintf("module fixture.example/scratch\n\ngo 1.27.3\n\nrequire (\n\t%s v%s\n\t%s v%s\n\tgoogle.golang.org/protobuf %s\n)\n",
			fixtureSidecarRoot, fixtureExternalVersion, fixtureConnectRoot, fixtureExternalVersion, self.protobufVersion()),
		"main.go": fmt.Sprintf("package main\n\nimport (\n\t_ %q\n\t_ %q\n\t_ %q\n)\n\nfunc main() {}\n",
			fixtureSidecarRoot, fixtureSidecarRoot+"/protocol", fixtureSidecarRoot+"/"+self.enginePackage()),
		"descriptors/main.go": fmt.Sprintf(`package main

import (
	"fmt"

	_ %q
	_ %q
	_ %q
	_ %q
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func main() {
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		fmt.Println(file.Path())
		return true
	})
}
`, fixtureSidecarRoot, fixtureSidecarRoot+"/protocol", fixtureSidecarRoot+"/protocol/bridge", fixtureConnectRoot+"/protocol"),
	}
	for name, content := range files {
		filePath := filepath.Join(scratch, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			self.t.Fatal(err)
		}
		if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
			self.t.Fatal(err)
		}
	}
	if _, err := self.goCommand(scratch, nil, "mod", "tidy"); err != nil {
		return append(findings, fmt.Sprintf("the scratch module does not resolve at the tag: %v", err))
	}
	if _, err := self.goCommand(scratch, nil, "build", "-trimpath", "-o", filepath.Join(self.root, "scratch-bin"), "."); err != nil {
		return append(findings, fmt.Sprintf("the scratch module does not build at the tag: %v", err))
	}
	moduleList, err := self.moduleList(scratch)
	if err != nil {
		return append(findings, fmt.Sprintf("go list -m all in the scratch module: %v", err))
	}
	findings = append(findings, sidecarPathFindings("the scratch module", moduleList)...)
	return append(findings, self.descriptorFindings(scratch, "./descriptors", self.protoPaths())...)
}

// Runs the listing program in module under the panic policy.
func (self *releaseFixture) descriptorFindings(module string, packagePath string, wantPaths []string) []string {
	self.t.Helper()
	listing, err := self.goCommand(module, []string{"GOLANG_PROTOBUF_REGISTRATION_CONFLICT=panic"}, "run", "-trimpath", packagePath)
	if err != nil {
		return []string{fmt.Sprintf("the descriptor program failed, which a descriptor registered twice does: %v", err)}
	}
	return descriptorListingFindings(listing, wantPaths)
}

// ur from a copy of cli/ with its replaces stripped: it builds at the tags, prints the release's
// version, and shows one root path.
func (self *releaseFixture) urFindings() []string {
	self.t.Helper()
	findings := []string{}
	source := filepath.Join(self.buildHome, "sidecar", "cli")
	copied := filepath.Join(self.root, "ur")
	if output, err := exec.Command("cp", "-R", source, copied).CombinedOutput(); err != nil {
		self.t.Fatalf("copy cli: %v\n%s", err, output)
	}
	edits, err := self.goCommand(copied, nil, "mod", "edit", "-json")
	if err != nil {
		self.t.Fatal(err)
	}
	arguments := []string{"mod", "edit"}
	for _, match := range regexp.MustCompile(`"Old": \{\s*"Path": "([^"]+)"`).FindAllStringSubmatch(edits, -1) {
		arguments = append(arguments, "-dropreplace="+match[1])
	}
	if len(arguments) > 2 {
		if _, err := self.goCommand(copied, nil, arguments...); err != nil {
			self.t.Fatal(err)
		}
	}
	binary := filepath.Join(self.root, "ur-bin")
	if _, err := self.goCommand(copied, nil, "build", "-trimpath", "-o", binary, "."); err != nil {
		return append(findings, fmt.Sprintf("ur does not build from cli/ at the tags: %v", err))
	}
	output, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		findings = append(findings, fmt.Sprintf("ur --version failed: %v\n%s", err, output))
	} else if want := "ur " + fixtureInternalVersion + "\n"; string(output) != want {
		findings = append(findings, fmt.Sprintf("ur --version prints %q, want %q", output, want))
	}
	moduleList, err := self.moduleList(copied)
	if err != nil {
		return append(findings, fmt.Sprintf("go list -m all in ur's module: %v", err))
	}
	if !strings.Contains(moduleList, fixtureSmtpproof+" ") {
		findings = append(findings, fmt.Sprintf("ur's graph holds no %s", fixtureSmtpproof))
	}
	return append(findings, sidecarPathFindings("ur's module", moduleList)...)
}

// What only smtpproof brings: the third-party modules its go.mod requires directly, less what the
// root's graph holds. A module further down its graph can reach sdk by another route, as yaml.v2
// does, and sdk holding that is not a prover in sdk. smtpproof itself is matched by name.
func (self *releaseFixture) proverModules() []string {
	self.t.Helper()
	rootGraph, err := self.goCommand(filepath.Join(self.buildHome, "sidecar", "v2026"), nil, "mod", "graph")
	if err != nil {
		self.t.Fatal(err)
	}
	editJson, err := self.goCommand(filepath.Join(self.buildHome, "sidecar", "smtpproof", "v2026"), nil, "mod", "edit", "-json")
	if err != nil {
		self.t.Fatal(err)
	}
	provers, err := proverPaths(editJson, rootGraph)
	if err != nil {
		self.t.Fatal(err)
	}
	return provers
}

// The third-party modules a go.mod requires directly, from go mod edit -json, less those the root's
// go mod graph output holds.
func proverPaths(editJson string, rootGraph string) ([]string, error) {
	root := map[string]bool{}
	for _, field := range strings.Fields(rootGraph) {
		modulePath, _, _ := strings.Cut(field, "@")
		root[modulePath] = true
	}
	var goMod struct {
		Require []struct {
			Path     string
			Indirect bool
		}
	}
	if err := json.Unmarshal([]byte(editJson), &goMod); err != nil {
		return nil, err
	}
	provers := []string{}
	for _, require := range goMod.Require {
		if !require.Indirect && !root[require.Path] && !strings.HasPrefix(require.Path, "github.com/urnetwork/") {
			provers = append(provers, require.Path)
		}
	}
	slices.Sort(provers)
	return provers, nil
}

// Each of sdk's four modules at its tag.
func (self *releaseFixture) sdkFindings() []string {
	self.t.Helper()
	findings := []string{}
	provers := self.proverModules()
	for _, module := range []string{"v2026", "build", "cgo", "js"} {
		directory := filepath.Join(self.buildHome, "sdk", module)
		graph, err := self.goCommand(directory, nil, "mod", "graph")
		if err != nil {
			findings = append(findings, fmt.Sprintf("go mod graph in sdk's %s: %v", module, err))
			continue
		}
		if !strings.Contains(graph, fixtureSidecarRoot+"@v"+fixtureExternalVersion) {
			findings = append(findings, fmt.Sprintf("sdk's %s does not require the root's tag", module))
		}
		findings = append(findings, proverFindings("sdk's "+module, graph, provers)...)
	}
	return findings
}

// Everything §4.9's fixture line shows.
func (self *releaseFixture) findings() []string {
	self.t.Helper()
	findings := self.scratchFindings()
	findings = append(findings, self.urFindings()...)
	return append(findings, self.sdkFindings()...)
}

// The real repositories at HEAD, through the real release code.
func TestSidecarReleaseFixture(t *testing.T) {
	fixture := newReleaseFixture(t)
	for _, name := range []string{"glog", "connect", "goidenticons", "sidecar", "sdk"} {
		fixture.archive(name)
	}
	fixture.makeCheck = true
	// go_mod_edit_gvisor is a stand-in here, so the local replace stays and must resolve. make check
	// compares against server's requirements and holds toRelease to run.sh's functions, which the
	// release host's $BUILD_HOME holds as server/ and all/.
	for link, target := range map[string]string{"gvisor": fixtureCheckout(t, "gvisor"), "server": fixtureCheckout(t, "server"), "all": rolloutRoot(t)} {
		if err := os.Symlink(target, filepath.Join(fixture.buildHome, link)); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := fixture.release(nil); err != nil {
		t.Fatalf("the release step failed: %v\n%s", err, output)
	}
	for _, finding := range fixture.findings() {
		t.Error(finding)
	}
	// the sdk check reads a list, so an empty one would pass it, as would a prover the root's graph
	// already holds
	if provers := fixture.proverModules(); !slices.Contains(provers, "github.com/consensys/gnark") {
		t.Errorf("the prover modules are %q, which leaves gnark out", provers)
	}
}

// Small repositories of the real names, so a planted run takes seconds and the same path through
// the same code as the real one.
func newSmallReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	fixture := newReleaseFixture(t)
	fixture.write("glog", map[string]string{"go.mod": "module github.com/urnetwork/glog\n\ngo 1.27.3\n", "glog.go": "package glog\n"})
	fixture.write("goidenticons", map[string]string{"go.mod": "module github.com/urnetwork/goidenticons\n\ngo 1.27.3\n", "identicon.go": "package goidenticons\n"})
	fixture.write("connect", map[string]string{
		"go.mod":               "module github.com/urnetwork/connect\n\ngo 1.27.3\n\nrequire github.com/urnetwork/glog v0.0.0\n\nreplace github.com/urnetwork/glog => ../glog\n",
		"connect.go":           "package connect\n\nimport _ \"github.com/urnetwork/glog\"\n",
		"protocol/protocol.go": "package protocol\n",
		"sctp/go.mod":          "module github.com/pion/sctp\n\ngo 1.27.3\n",
	})
	sidecarFiles := map[string]string{}
	for name, content := range sidecarReleaseTreeFiles {
		sidecarFiles[name] = content
	}
	sidecarFiles["protocol/protocol.go"] = "package protocol\n\nimport _ \"google.golang.org/protobuf/proto\"\n"
	sidecarFiles["protocol/bridge/bridge.go"] = "package bridge\n"
	sidecarFiles["dnsservice/doc.go"] = "package dnsservice\n"
	sidecarFiles["go.mod"] = strings.Replace(sidecarFiles["go.mod"], "\tgithub.com/urnetwork/glog v0.0.0\n", "\tgithub.com/urnetwork/glog v0.0.0\n\tgoogle.golang.org/protobuf v1.36.11\n", 1)
	sidecarFiles["cli/main.go"] = `package main

import (
	"fmt"

	"github.com/urnetwork/sidecar"
	_ "github.com/urnetwork/sidecar/smtpproof"
)

func main() {
	fmt.Printf("ur %s\n", sidecar.Version)
}
`
	fixture.write("sidecar", sidecarFiles)
	sdkRequires := "require (\n\tgithub.com/urnetwork/connect v0.0.0\n\tgithub.com/urnetwork/glog v0.0.0\n\tgithub.com/urnetwork/goidenticons v0.0.0\n\tgithub.com/urnetwork/sidecar v0.0.0\n)\n"
	sdkReplaces := func(climb string) string {
		return fmt.Sprintf("\nreplace github.com/urnetwork/connect => %[1]sconnect\n\nreplace github.com/urnetwork/glog => %[1]sglog\n\nreplace github.com/urnetwork/goidenticons => %[1]sgoidenticons\n\nreplace github.com/urnetwork/sidecar => %[1]ssidecar\n", climb)
	}
	sdkImports := "package sdk\n\nimport (\n\t_ \"github.com/urnetwork/connect\"\n\t_ \"github.com/urnetwork/glog\"\n\t_ \"github.com/urnetwork/goidenticons\"\n\t_ \"github.com/urnetwork/sidecar\"\n)\n\nconst Version string = \"\"\n"
	sdkNested := func(name string) string {
		return "module github.com/urnetwork/sdk/" + name + "\n\ngo 1.27.3\n\nrequire github.com/urnetwork/sdk v0.0.0\n\n" + sdkRequires + "\nreplace github.com/urnetwork/sdk => ..\n" + sdkReplaces("../../")
	}
	fixture.write("sdk", map[string]string{
		"go.mod":        "module github.com/urnetwork/sdk\n\ngo 1.27.3\n\n" + sdkRequires + sdkReplaces("../"),
		"sdk.go":        sdkImports,
		"build/go.mod":  sdkNested("build"),
		"build/main.go": "package main\n\nimport _ \"github.com/urnetwork/sdk\"\n\nfunc main() {}\n",
		"cgo/go.mod":    sdkNested("cgo"),
		"cgo/main.go":   "package main\n\nimport _ \"github.com/urnetwork/sdk\"\n\nfunc main() {}\n",
		"js/go.mod":     sdkNested("js"),
		"js/main.go":    "package main\n\nimport _ \"github.com/urnetwork/sdk\"\n\nfunc main() {}\n",
	})
	return fixture
}

// The real run's script holds run.sh's own make check between sidecar's tags and sdk's first edit,
// and a planted run's does not, since a small tree holds nothing for it to read.
func TestFixtureRunsRunShsMakeCheckAfterSidecarsTags(t *testing.T) {
	withMakeCheck := fixtureRegionSource(t, true)
	publish := strings.Index(withMakeCheck, "sidecar_release_publish)\n")
	makeCheck := strings.Index(withMakeCheck, "make check)\n"+fixtureMakeCheckEnd+"\n")
	sdk := strings.Index(withMakeCheck, "(cd $BUILD_HOME/sdk/build &&")
	if publish < 0 || makeCheck < 0 || sdk < 0 || !(publish < makeCheck && makeCheck < sdk) {
		t.Fatalf("the real run's script holds sidecar's publish at %d, make check at %d and sdk's edit at %d", publish, makeCheck, sdk)
	}
	if strings.Contains(fixtureRegionSource(t, false), "make check") {
		t.Fatal("a planted run's script runs make check")
	}
}

// The small tree passes, so what a planted run finds is what it planted.
func TestFixturePassesTheSmallTree(t *testing.T) {
	fixture := newSmallReleaseFixture(t)
	if output, err := fixture.release(nil); err != nil {
		t.Fatalf("the release step failed: %v\n%s", err, output)
	}
	for _, finding := range fixture.findings() {
		t.Error(finding)
	}
}

// The root's pass first writes .../sidecar/v2026/smtpproof, which no module holds.
func TestFixtureFailsWhenTheRootRewriteRunsFirst(t *testing.T) {
	smtpproofPass := "    go_edit_require_subpackages " + smtpproofModule + " &&\n"
	rootPass := "    go_edit_require_subpackages " + sidecarRootModule + " &&\n"
	fixture := newSmallReleaseFixture(t)
	output, err := fixture.release(func(source string) string {
		if !strings.Contains(source, smtpproofPass) || !strings.Contains(source, rootPass) {
			t.Fatal("the rewrite lines moved, so this plants nothing")
		}
		source = strings.Replace(source, smtpproofPass, "", 1)
		return strings.Replace(source, rootPass, rootPass+smtpproofPass, 1)
	})
	if err == nil {
		t.Fatalf("the misordered release succeeded:\n%s", output)
	}
	if !strings.Contains(output, fixtureSidecarRoot+"/smtpproof") {
		t.Fatalf("the failure does not name the broken path:\n%s", output)
	}
}

// With the sed gone, nothing else in the release notices, so ur's own output has to.
func TestFixtureFailsOnAnEmptyVersion(t *testing.T) {
	fixture := newSmallReleaseFixture(t)
	output, err := fixture.release(func(source string) string {
		kept := []string{}
		dropped := 0
		for _, line := range strings.Split(source, "\n") {
			if strings.Contains(line, `Version string = \"`) {
				dropped += 1
				continue
			}
			kept = append(kept, line)
		}
		if dropped != 2 {
			t.Fatalf("dropped %d lines, so this plants the wrong thing", dropped)
		}
		return strings.Join(kept, "\n")
	})
	if err != nil {
		t.Fatalf("the release step failed: %v\n%s", err, output)
	}
	findings := fixture.urFindings()
	if !slices.ContainsFunc(findings, func(finding string) bool { return strings.Contains(finding, "ur --version prints") }) {
		t.Fatalf("an empty version passed: %q", findings)
	}
}

// Both the stale identity and the path a misordered rewrite writes are read off the real command's
// format.
func TestFixtureFailsOnTwoSidecarPaths(t *testing.T) {
	version := " v" + fixtureExternalVersion + "\n"
	clean := "fixture.example/scratch\n" + fixtureSidecarRoot + version
	if findings := sidecarPathFindings("planted", clean); len(findings) != 0 {
		t.Fatalf("a clean list has findings: %q", findings)
	}
	for _, planted := range []string{sidecarRootModule + " v0.0.0\n", smtpproofModule + " v0.0.0\n", fixtureSidecarRoot + "/smtpproof" + version} {
		if findings := sidecarPathFindings("planted", clean+planted); len(findings) != 1 {
			t.Errorf("%q gave %q", planted, findings)
		}
	}
	if findings := sidecarPathFindings("planted", "fixture.example/scratch\n"); len(findings) != 1 {
		t.Errorf("a graph with no root gave %q", findings)
	}
}

// A real program: two packages registering one path, under the policy §4.9's listing runs with.
func TestFixtureFailsOnADescriptorRegisteredTwice(t *testing.T) {
	fixture := newReleaseFixture(t)
	registering := `package %s

import (
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func init() {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{Name: proto.String("protocol/twice.proto"), Syntax: proto.String("proto3")}, nil)
	if err != nil {
		panic(err)
	}
	if err := protoregistry.GlobalFiles.RegisterFile(file); err != nil {
		panic(err)
	}
}
`
	files := map[string]string{
		"go.mod":         "module fixture.example/twice\n\ngo 1.27.3\n\nrequire google.golang.org/protobuf v1.36.11\n",
		"once/once.go":   fmt.Sprintf(registering, "once"),
		"twice/twice.go": fmt.Sprintf(registering, "twice"),
		"listing/main.go": `package main

import (
	"fmt"

	_ "fixture.example/twice/once"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func main() {
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		fmt.Println(file.Path())
		return true
	})
}
`,
	}
	module := filepath.Join(fixture.root, "twice")
	for name, content := range files {
		filePath := filepath.Join(module, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if findings := fixture.descriptorFindings(module, "./listing", []string{"protocol/twice.proto"}); len(findings) != 0 {
		t.Fatalf("one registration has findings: %q", findings)
	}
	listing := filepath.Join(module, "listing", "main.go")
	data, err := os.ReadFile(listing)
	if err != nil {
		t.Fatal(err)
	}
	both := strings.Replace(string(data), "\t_ \"fixture.example/twice/once\"\n", "\t_ \"fixture.example/twice/once\"\n\t_ \"fixture.example/twice/twice\"\n", 1)
	if err := os.WriteFile(listing, []byte(both), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := fixture.descriptorFindings(module, "./listing", []string{"protocol/twice.proto"})
	if len(findings) != 1 || !strings.Contains(findings[0], "registered twice") {
		t.Fatalf("a path registered twice gave %q", findings)
	}
	if findings := descriptorListingFindings("", []string{"protocol/absent.proto"}); len(findings) != 1 {
		t.Fatalf("a descriptor that never registered gave %q", findings)
	}
}

// A prover is what smtpproof requires itself. What it requires indirectly, and what the root's graph
// already holds, can reach sdk by another route, so neither is one.
func TestFixtureProversAreSmtpproofsOwnDirectRequires(t *testing.T) {
	editJson := `{"Module": {"Path": "` + fixtureSmtpproof + `"}, "Require": [
		{"Path": "github.com/consensys/gnark", "Version": "v0.15.0"},
		{"Path": "github.com/consensys/gnark-crypto", "Version": "v0.20.1"},
		{"Path": "` + fixtureSidecarRoot + `", "Version": "v` + fixtureExternalVersion + `"},
		{"Path": "golang.org/x/crypto", "Version": "v0.58.0"},
		{"Path": "gopkg.in/yaml.v2", "Version": "v2.4.0", "Indirect": true}]}`
	rootGraph := fixtureSidecarRoot + " " + fixtureConnectRoot + "@v" + fixtureExternalVersion + "\n" +
		fixtureConnectRoot + "@v" + fixtureExternalVersion + " golang.org/x/crypto@v0.58.0\n"
	provers, err := proverPaths(editJson, rootGraph)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github.com/consensys/gnark", "github.com/consensys/gnark-crypto"}; !slices.Equal(provers, want) {
		t.Fatalf("provers = %q, want %q", provers, want)
	}
	if _, err := proverPaths("not json", rootGraph); err == nil {
		t.Fatal("an unreadable go.mod gave provers")
	}
}

// smtpproof by any path, and a module only it brings, read off go mod graph's format.
func TestFixtureFailsWhenSdkGraphHoldsAProver(t *testing.T) {
	sdkLine := fixtureSdkRoot + "@v" + fixtureExternalVersion
	clean := sdkLine + " " + fixtureSidecarRoot + "@v" + fixtureExternalVersion + "\n"
	provers := []string{"github.com/consensys/gnark"}
	if findings := proverFindings("planted", clean, provers); len(findings) != 0 {
		t.Fatalf("a clean graph has findings: %q", findings)
	}
	for _, planted := range []string{
		sdkLine + " " + fixtureSmtpproof + "@v" + fixtureExternalVersion + "\n",
		sdkLine + " github.com/consensys/gnark@v0.15.0\n",
	} {
		if findings := proverFindings("planted", clean+planted, provers); len(findings) != 1 {
			t.Errorf("%q gave %q", planted, findings)
		}
	}
}
