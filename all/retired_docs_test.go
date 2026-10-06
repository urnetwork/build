// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseManifestExcludesRetiredDocs(t *testing.T) {
	root := filepath.Dir(rolloutRoot(t))
	paths := runGit(t, root, "config", "--file", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	for _, line := range strings.Split(paths, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == "docs" {
			t.Fatal("the release still initializes the archived GitBook docs repository")
		}
	}
	if tracked := runGit(t, root, "ls-files", "--stage", "--", "docs"); tracked != "" {
		t.Fatalf("the retired docs gitlink remains in the release: %s", tracked)
	}
}

// Execute the production lifecycle regions without a docs checkout. Each still
// has to reach the live components on both sides of the retired entry, and a
// live component's failure must still stop the release.
func TestReleaseRepositoryStagesWithoutRetiredDocs(t *testing.T) {
	root := filepath.Dir(rolloutRoot(t))
	paths := runGit(t, root, "config", "--file", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	var components []string
	for _, line := range strings.Split(paths, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] != "docs" {
			components = append(components, fields[1])
		}
	}
	setup := `
for component in ` + strings.Join(components, " ") + `; do
    mkdir -p "$BUILD_HOME/$component"
done
git_main() { record_component_step "pull:${PWD:t}"; }
git_commit() { record_component_step "commit:${PWD:t}"; }
git_tag() { record_component_step "tag:${PWD:t}"; }
git() {
    [[ "$*" == "checkout -b v${EXTERNAL_WARP_VERSION}" ]] || return 51
    record_component_step "branch:${PWD:t}"
}
`
	for _, stage := range []struct {
		name, start, end, event string
	}{
		{"pull", `(cd $BUILD_HOME/sdk && git_main)`, "# refresh the generated connect IP tables", "pull"},
		{"branch", `(cd $BUILD_HOME/connect && git checkout -b v${EXTERNAL_WARP_VERSION})`, "# apple branch, edit xcodeproject", "branch"},
		{"publish", "(cd $BUILD_HOME/android && \n    git_commit", "(cd $BUILD_HOME/localizations &&\n    npm_fork", "tag"},
	} {
		t.Run(stage.name, func(t *testing.T) {
			source := componentRegion(t, stage.start, stage.end)
			result := runComponent(t, source, setup)
			if result.exitCode != 0 {
				t.Fatalf("release requires the retired checkout: %+v", result)
			}
			for _, event := range []string{stage.event + ":web\n", stage.event + ":warp\n", "release-continued\n"} {
				if !strings.Contains(result.events, event) {
					t.Fatalf("live release step %q missing: %+v", event, result)
				}
			}
			failed := runComponent(t, source, setup, "FAIL_STEP="+stage.event+":web")
			if failed.exitCode != 37 || strings.Contains(failed.events, stage.event+":warp\n") || strings.Contains(failed.events, "release-continued\n") {
				t.Fatalf("live repository failure no longer stops the release: %+v", failed)
			}
		})
	}
}
