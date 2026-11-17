// SPDX-License-Identifier: MPL-2.0

// sdk's release edits: run.sh's sdk region, from its nested modules' edits through the root's, as it
// stands, over a synthetic tree, with the real seds and go mod edit. Only tidy and get, the two steps
// that need published modules, are stubs here. The release fixture (sidecar_release_fixture_test.go)
// runs the whole region with both real, against tags served from a file proxy.

package allbuild

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	sdkRootModule = "github.com/urnetwork/sdk"
	sdkTagRoot    = sdkRootModule + "/v2026"
	// run.sh's sdk region from its first edit through the root's, which ends the edits; sdk's tag and
	// the nested modules' tidies follow it
	sdkReleaseEditStart = "(cd $BUILD_HOME/sdk/build &&\n    go_mod_edit_gvisor"
	sdkReleaseEditEnd   = "error_trap 'sdk edit'"
	// the restore's call in the root's edit, as run.sh holds it
	sdkRestoreCall = "    go_edit_restore_nested_modules github.com/urnetwork/sdk \"${sdk_kept_trees[@]}\" &&\n"
)

// Every function the sdk region's edits call.
var sdkReleaseEditFunctionNames = []string{
	"go_edit_require", "go_edit_require_subpackages", "go_edit_restore_nested_modules", "go_mod_drop_require",
	"go_mod_edit_module", "go_mod_edit_require", "go_mod_edit_gvisor", "go_mod_fork_rebase_local_replaces",
	"go_mod_fork_prepare", "go_mod_fork_tidy", "go_mod_fork", "sidecar_release_consumer_edit",
	"operator_core_release_consumer_edit",
}

// sdk's nested modules, which go_mod_fork leaves beside the versioned module and the release never
// tags, so their packages keep their unversioned paths.
var sdkNestedModules = []string{"build", "cgo", "js", "packaging"}

// The shape run.sh meets: a root package with subpackages, one named so that a nested module's name is
// a prefix of it, and four nested modules. cgo's tests import a package of cgo's own, as the real cgo's
// import abitest (QD-844, QD-1004), js's main imports one of js's own, as the real sidecarwasm imports
// sidecarworker, and a root test names js's packages, as the real dependency_boundary_test.go does.
var sdkReleaseTreeFiles = map[string]string{
	"go.mod": `module github.com/urnetwork/sdk

go 1.27.3

require (
	github.com/urnetwork/connect v0.0.0
	github.com/urnetwork/glog v0.0.0
	github.com/urnetwork/sidecar v0.0.0
)

replace github.com/urnetwork/connect => ../connect

replace github.com/urnetwork/glog => ../glog

replace github.com/urnetwork/sidecar => ../sidecar
`,
	"sdk.go": `package sdk

import (
	_ "github.com/urnetwork/connect"
	_ "github.com/urnetwork/sdk/jsutil"
	_ "github.com/urnetwork/sdk/sub"
	_ "github.com/urnetwork/sidecar"
)

const Version string = ""
`,
	"sub/sub.go":       "package sub\n",
	"jsutil/jsutil.go": "package jsutil\n",
	"boundary_test.go": `package sdk

const (
	workerPath = "github.com/urnetwork/sdk/js/worker"
	cgoPath    = "github.com/urnetwork/sdk/cgo"
)
`,
	"Makefile": "check:\n\tgo list \"github.com/urnetwork/sdk/sub\"\n",
	"build/go.mod": `module github.com/urnetwork/sdk/build

go 1.27.3

require github.com/urnetwork/sdk v0.0.0

replace github.com/urnetwork/sdk => ..
`,
	"build/main.go": "package main\n\nimport _ \"github.com/urnetwork/sdk\"\n\nfunc main() {}\n",
	"cgo/go.mod": `module github.com/urnetwork/sdk/cgo

go 1.27.3

require github.com/urnetwork/sdk v0.0.0

replace github.com/urnetwork/sdk => ..
`,
	"cgo/main.go":        "package main\n\nimport (\n\t_ \"github.com/urnetwork/sdk\"\n\t_ \"github.com/urnetwork/sdk/sub\"\n)\n\nfunc main() {}\n",
	"cgo/abitest/doc.go": "package abitest\n",
	"cgo/main_test.go":   "package main\n\nimport _ \"github.com/urnetwork/sdk/cgo/abitest\"\n",
	// one line naming the nested module's package and the root's, which the root's pass rewrites together
	"cgo/Makefile": "check:\n\tgo vet \"github.com/urnetwork/sdk/cgo/abitest\" \"github.com/urnetwork/sdk/sub\"\n",
	"js/go.mod": `module github.com/urnetwork/sdk/js

go 1.27.3

require github.com/urnetwork/sdk v0.0.0

replace github.com/urnetwork/sdk => ..
`,
	"js/main.go":                   "package main\n\nimport (\n\t_ \"github.com/urnetwork/sdk\"\n\t_ \"github.com/urnetwork/sdk/js/worker\"\n)\n\nfunc main() {}\n",
	"js/worker/worker.go":          "package worker\n",
	"packaging/go.mod":             "module github.com/urnetwork/sdk/packaging\n\ngo 1.27.3\n",
	"packaging/main.go":            "package main\n\nimport _ \"github.com/urnetwork/sdk/packaging/release\"\n\nfunc main() {}\n",
	"packaging/release/release.go": "package release\n",
	"python/README.md":             "python\n",
}

// A $BUILD_HOME holding sdk's checkout.
type sdkReleaseTree struct {
	buildHome string
	sdk       string
	eventLog  string
}

func newSdkReleaseTree(t *testing.T) *sdkReleaseTree {
	t.Helper()
	root := t.TempDir()
	tree := &sdkReleaseTree{
		buildHome: filepath.Join(root, "build"),
		sdk:       filepath.Join(root, "build", "sdk"),
		eventLog:  filepath.Join(root, "events"),
	}
	for name, contents := range sdkReleaseTreeFiles {
		path := filepath.Join(tree.sdk, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

// Runs script over the release's functions from $BUILD_HOME, with tidy and get recorded and skipped.
func (self *sdkReleaseTree) run(t *testing.T, script string) (string, error) {
	t.Helper()
	var functions strings.Builder
	for _, name := range sdkReleaseEditFunctionNames {
		functions.WriteString(runFunction(t, name))
	}
	harness := `
set -u
event_log="$1"
eval "$2"
BUILD_SED="$3"
BUILD_HOME="$4"
EXTERNAL_WARP_VERSION="$5"
WARP_VERSION="$6"
GVISOR_GO_VERSION="$7"
GO_MOD_VERSION=2026
GO_MOD_SUFFIX=/v2026
error_trap () {
    code=$?
    if [ $code != 0 ]; then
        print -u2 -- "step failed ($code): $1"
        exit $code
    fi
}
go() {
    case "$1 $2" in
        'mod tidy'|'get -t')
            print -r -- "go $1 $2 in ${PWD#$BUILD_HOME/sdk}" >> "$event_log"
            return 0
            ;;
    esac
    command go "$@"
}
eval "$8"
`
	command := exec.Command("zsh", "-c", harness, "sdk-release-test", self.eventLog, functions.String(), sidecarTestSed(t),
		self.buildHome, sidecarTestVersion, sidecarTestInternalVersion, sidecarTestGvisorVersion, script)
	command.Dir = self.buildHome
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=")
	output, err := command.CombinedOutput()
	return string(output), err
}

// The tree's file at name, which must exist.
func (self *sdkReleaseTree) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(self.sdk, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The sdk region's edits as run.sh holds them, through their error_trap.
func sdkReleaseEditRegion(t *testing.T) string {
	t.Helper()
	return componentRegion(t, sdkReleaseEditStart, sdkReleaseEditEnd) + sdkReleaseEditEnd + "\n"
}

// Each quoted sdk path in the tree's Go files and Makefiles that does not name what the tags hold: a
// package of a nested module by anything but its own unversioned path, or a package of the root by
// anything but the root's versioned one.
func sdkPathFindings(t *testing.T, sdk string) []string {
	t.Helper()
	quoted := regexp.MustCompile(`"` + regexp.QuoteMeta(sdkRootModule) + `(/[^"]*)?"`)
	findings := []string{}
	err := filepath.WalkDir(sdk, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || (!strings.HasSuffix(path, ".go") && entry.Name() != "Makefile") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(sdk, path)
		if err != nil {
			return err
		}
		for _, match := range quoted.FindAllStringSubmatch(string(data), -1) {
			rest := match[1]
			nested := ""
			for _, module := range sdkNestedModules {
				if rest == "/"+module || strings.HasPrefix(rest, "/"+module+"/") ||
					rest == "/v2026/"+module || strings.HasPrefix(rest, "/v2026/"+module+"/") {
					nested = module
				}
			}
			switch {
			case nested != "" && strings.HasPrefix(rest, "/v2026/"):
				findings = append(findings, filepath.ToSlash(name)+": "+match[0]+" versions the untagged "+nested+"'s own path")
			case nested == "" && rest != "/v2026" && !strings.HasPrefix(rest, "/v2026/"):
				findings = append(findings, filepath.ToSlash(name)+": "+match[0]+" names the root unversioned")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// QD-1004: after the region's edits the root's paths carry /v2026 and each nested module's own stay as
// they were, whichever module's file holds them.
func TestSdkReleaseEditKeepsNestedModulePaths(t *testing.T) {
	tree := newSdkReleaseTree(t)
	if output, err := tree.run(t, sdkReleaseEditRegion(t)); err != nil {
		t.Fatalf("the sdk region's edits failed: %v\n%s", err, output)
	}

	for name, wants := range map[string][]string{
		"v2026/go.mod": {"module " + sdkTagRoot + "\n"},
		"v2026/sdk.go": {`"` + sdkTagRoot + `/sub"`, `"` + sdkTagRoot + `/jsutil"`, `"github.com/urnetwork/connect/v2026"`,
			`"github.com/urnetwork/sidecar/v2026"`, `Version string = "` + sidecarTestInternalVersion + `"`},
		"v2026/Makefile":         {`"` + sdkTagRoot + `/sub"`},
		"v2026/boundary_test.go": {`"` + sdkRootModule + `/js/worker"`, `"` + sdkRootModule + `/cgo"`},
		"build/main.go":          {`"` + sdkTagRoot + `"`},
		"cgo/main.go":            {`"` + sdkTagRoot + `"`, `"` + sdkTagRoot + `/sub"`},
		"cgo/main_test.go":       {`"` + sdkRootModule + `/cgo/abitest"`},
		"cgo/Makefile":           {`"` + sdkRootModule + `/cgo/abitest" "` + sdkTagRoot + `/sub"`},
		"js/main.go":             {`"` + sdkTagRoot + `"`, `"` + sdkRootModule + `/js/worker"`},
		"packaging/main.go":      {`"` + sdkRootModule + `/packaging/release"`},
	} {
		source := tree.read(t, name)
		for _, want := range wants {
			if !strings.Contains(source, want) {
				t.Errorf("%s lacks %s:\n%s", name, want, source)
			}
		}
	}
	for _, finding := range sdkPathFindings(t, tree.sdk) {
		t.Error(finding)
	}

	// the root moved, and the nested modules and the other kept trees stayed beside it
	for _, name := range []string{"v2026/sub/sub.go", "v2026/jsutil/jsutil.go", "cgo/abitest/doc.go", "js/worker/worker.go",
		"packaging/release/release.go", "build/main.go", "python/README.md"} {
		if _, err := os.Stat(filepath.Join(tree.sdk, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s is not where the release leaves it: %v", name, err)
		}
	}
	for _, name := range []string{"v2026/cgo", "v2026/js", "v2026/build", "v2026/packaging", "v2026/python", "sdk.go"} {
		if _, err := os.Stat(filepath.Join(tree.sdk, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should not exist on the version branch: %v", name, err)
		}
	}
}

// The twin: the same region with the restore left out writes the paths QD-1004 found, or the test
// above shows nothing.
func TestSdkReleaseEditWithoutTheRestoreVersionsNestedModulePaths(t *testing.T) {
	region := sdkReleaseEditRegion(t)
	if strings.Count(region, sdkRestoreCall) != 1 {
		t.Fatalf("the sdk region does not call the restore once as %q, so this plants nothing", sdkRestoreCall)
	}
	tree := newSdkReleaseTree(t)
	if output, err := tree.run(t, strings.Replace(region, sdkRestoreCall, "", 1)); err != nil {
		t.Fatalf("the sdk region's edits failed: %v\n%s", err, output)
	}
	if source := tree.read(t, "cgo/main_test.go"); !strings.Contains(source, `"`+sdkTagRoot+`/cgo/abitest"`) {
		t.Fatalf("without the restore cgo's tests should import a path sdk's tag does not hold:\n%s", source)
	}
	findings := sdkPathFindings(t, tree.sdk)
	for _, want := range []string{"cgo/main_test.go", "cgo/Makefile", "js/main.go", "packaging/main.go", "v2026/boundary_test.go"} {
		found := false
		for _, finding := range findings {
			found = found || strings.HasPrefix(finding, want+": ")
		}
		if !found {
			t.Errorf("without the restore nothing is found in %s: %q", want, findings)
		}
	}
}

// A sed that fails stops the step rather than leaving a versioned path for cgo's tidy to meet after
// sdk's tag is pushed.
func TestSdkReleaseRestoreFailsWhenItsSedFails(t *testing.T) {
	tree := newSdkReleaseTree(t)
	output, err := tree.run(t, `cd "$BUILD_HOME/sdk" && BUILD_SED=false && go_edit_restore_nested_modules github.com/urnetwork/sdk cgo`)
	if err == nil {
		t.Fatalf("the restore passed with a failing sed:\n%s", output)
	}
}

// One list names the trees that stay beside sdk's versioned module, and both the restore and the fork
// take it, after the root's rewrite, so a tree added to one is in the other.
func TestRunRestoresSdkNestedModulesFromTheForksTrees(t *testing.T) {
	block := componentRegion(t, "(cd $BUILD_HOME/sdk &&\n    go_mod_edit_module", sdkReleaseEditEnd)
	rootPass := strings.Index(block, "    go_edit_require_subpackages "+sdkRootModule+" &&\n")
	trees := strings.Index(block, "    sdk_kept_trees=(")
	restore := strings.Index(block, sdkRestoreCall)
	fork := strings.Index(block, "    go_mod_fork \"${sdk_kept_trees[@]}\")\n")
	if rootPass < 0 || trees < 0 || restore < 0 || fork < 0 || !(rootPass < restore && trees < restore && restore < fork) {
		t.Fatalf("sdk's root edit holds the root's pass at %d, the trees at %d, the restore at %d and the fork at %d", rootPass, trees, restore, fork)
	}
	if count := strings.Count(block, "go_mod_fork"); count != 1 {
		t.Errorf("sdk's root edit forks %d times, want once", count)
	}
	for _, module := range sdkNestedModules {
		if !strings.Contains(block[trees:restore], "'"+module+"'") {
			t.Errorf("the kept trees do not name %s", module)
		}
	}
}
