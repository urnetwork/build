// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The module edits of one run.sh stage, in order: each go_mod_edit_require
// and go_edit_require_subpackages line between the stage's opening cd and its
// error trap.
func runStageModuleEdits(t *testing.T, opening string, trap string) []string {
	t.Helper()
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(runData)
	start := strings.Index(source, opening)
	if start < 0 {
		t.Fatalf("run.sh has no stage opening %q", opening)
	}
	length := strings.Index(source[start:], "\nerror_trap '"+trap+"'\n")
	if length < 0 {
		t.Fatalf("run.sh stage %q has no error trap", trap)
	}
	edits := []string{}
	editLine := regexp.MustCompile(`(?m)^\s+((?:go_mod_edit_require|go_edit_require_subpackages) \S+?)(?: &&|\))?$`)
	for _, match := range editLine.FindAllStringSubmatch(source[start:start+length], -1) {
		edits = append(edits, match[1])
	}
	if len(edits) == 0 {
		t.Fatalf("run.sh stage %q has no module edits", trap)
	}
	return edits
}

// Runs the stage's module edits with the production helpers in dir.
func runReleaseModuleEdits(t *testing.T, dir string, edits []string) {
	t.Helper()
	harness := `
set -eu
GO_MOD_SUFFIX=/v2026
EXTERNAL_WARP_VERSION=2026.11.28-1110000000
BUILD_SED="$1"
eval "$2"
eval "$3"
eval "$4"
eval "$5"
` + strings.Join(edits, "\n") + "\n"
	command := exec.Command(
		"zsh", "-c", harness, "sdk-hotspot-release-edit-test",
		buildSed(t),
		runFunction(t, "go_edit_require"),
		runFunction(t, "go_edit_require_subpackages"),
		runFunction(t, "go_mod_drop_require"),
		runFunction(t, "go_mod_edit_require"),
	)
	command.Dir = dir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release module edits failed: %v\n%s", err, output)
	}
}

func writeFixtureFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The sdk's hotspot proxy (sdk/hotspot) imports the proxy repo, which brings
// userwireguard. A release that left their sibling replacements in the sdk's
// go.mod would point them at checkouts already renamed to their versioned
// module paths, and the sdk's tidy would fail. The sdk stage must require the
// tagged proxy and userwireguard instead, and rewrite the hotspot import and
// the android bind's package paths.
func TestSdkReleaseRequiresTheTaggedProxyForTheHotspotProxy(t *testing.T) {
	root := t.TempDir()
	writeFixtureFiles(t, root, map[string]string{
		"go.mod": "module github.com/urnetwork/sdk\n\ngo 1.27.4\n\n" +
			"require (\n\tgithub.com/urnetwork/connect v0.0.0\n\tgithub.com/urnetwork/proxy v0.0.0\n\tgithub.com/urnetwork/userwireguard v0.0.0 // indirect\n)\n\n" +
			"replace github.com/urnetwork/connect => ../connect\n\n" +
			"replace github.com/urnetwork/proxy => ../proxy\n\n" +
			"replace github.com/urnetwork/userwireguard => ../userwireguard\n",
		"hotspot/hotspot_proxy.go": "package hotspot\n\nimport (\n\t\"github.com/urnetwork/proxy\"\n\t\"github.com/urnetwork/sdk\"\n)\n\nvar _ = proxy.DefaultHttpProxySettings\nvar _ = sdk.Version\n",
		"build/Makefile": "_build_android:\n\tgomobile bind \\\n\t\t\"github.com/urnetwork/sdk\" \\\n\t\t\"github.com/urnetwork/sdk/hotspot\"; \\\n",
	})

	edits := runStageModuleEdits(t, "(cd $BUILD_HOME/sdk &&\n    go_mod_edit_module github.com/urnetwork/sdk &&", "sdk edit")
	runReleaseModuleEdits(t, root, edits)

	goMod := readFixtureFile(t, filepath.Join(root, "go.mod"))
	for _, forbidden := range []string{
		"replace github.com/urnetwork/proxy",
		"replace github.com/urnetwork/userwireguard",
		"github.com/urnetwork/proxy v0.0.0",
		"github.com/urnetwork/userwireguard v0.0.0",
	} {
		if strings.Contains(goMod, forbidden) {
			t.Errorf("the released sdk go.mod keeps %q:\n%s", forbidden, goMod)
		}
	}
	for _, required := range []string{
		"github.com/urnetwork/proxy/v2026 v2026.11.28-1110000000",
		"github.com/urnetwork/userwireguard/v2026 v2026.11.28-1110000000",
	} {
		if !strings.Contains(goMod, required) {
			t.Errorf("the released sdk go.mod does not require %q:\n%s", required, goMod)
		}
	}
	hotspot := readFixtureFile(t, filepath.Join(root, "hotspot", "hotspot_proxy.go"))
	if !strings.Contains(hotspot, "\"github.com/urnetwork/proxy/v2026\"") || !strings.Contains(hotspot, "\"github.com/urnetwork/sdk/v2026\"") {
		t.Errorf("the released hotspot proxy imports the unversioned modules:\n%s", hotspot)
	}
	makefile := readFixtureFile(t, filepath.Join(root, "build", "Makefile"))
	if !strings.Contains(makefile, "\"github.com/urnetwork/sdk/v2026\" \\") || !strings.Contains(makefile, "\"github.com/urnetwork/sdk/v2026/hotspot\"") {
		t.Errorf("the released android bind names the unversioned packages:\n%s", makefile)
	}
}

// The sdk build module runs the android bind, so it requires what the hotspot
// proxy builds with, as tagged releases rather than sibling checkouts.
func TestSdkBuildReleaseRequiresTheTaggedProxyForTheAndroidBind(t *testing.T) {
	root := t.TempDir()
	writeFixtureFiles(t, root, map[string]string{
		"go.mod": "module github.com/urnetwork/sdk/build\n\ngo 1.27.4\n\n" +
			"require github.com/urnetwork/sdk v0.0.0\n\n" +
			"require (\n\tgithub.com/urnetwork/connect v0.0.0 // indirect\n\tgithub.com/urnetwork/proxy v0.0.0 // indirect\n\tgithub.com/urnetwork/userwireguard v0.0.0 // indirect\n)\n\n" +
			"replace github.com/urnetwork/sdk => ..\n\n" +
			"replace github.com/urnetwork/connect => ../../connect\n\n" +
			"replace github.com/urnetwork/proxy => ../../proxy\n\n" +
			"replace github.com/urnetwork/userwireguard => ../../userwireguard\n",
	})

	edits := runStageModuleEdits(t, "(cd $BUILD_HOME/sdk/build &&\n", "sdk build edit")
	runReleaseModuleEdits(t, root, edits)

	goMod := readFixtureFile(t, filepath.Join(root, "go.mod"))
	if strings.Contains(goMod, "replace") {
		t.Errorf("the released sdk build go.mod keeps a sibling replacement:\n%s", goMod)
	}
	for _, required := range []string{
		"github.com/urnetwork/proxy/v2026 v2026.11.28-1110000000",
		"github.com/urnetwork/userwireguard/v2026 v2026.11.28-1110000000",
		"github.com/urnetwork/sdk/v2026 v2026.11.28-1110000000",
	} {
		if !strings.Contains(goMod, required) {
			t.Errorf("the released sdk build go.mod does not require %q:\n%s", required, goMod)
		}
	}
}
