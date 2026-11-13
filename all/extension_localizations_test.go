// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real preparation region: both npm-publication paths must
// generate tracked locales before committing/tagging, and generation failure
// must stop preparation before a stale release can be sealed.
func TestExtensionLocalesGeneratedBeforeCommit(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, step := range []string{
		"error_trap 'localizations push branch'",
		"error_trap 'extension edit'",
		"error_trap 'extension localizations codegen'",
		"error_trap 'extension push branch'",
		`release_state seal "${RELEASE_PREPARED_OUTPUTS[@]}"`,
		`if release_build extension `,
	} {
		at := strings.Index(string(runData), step)
		if at <= previous || strings.Count(string(runData), step) != 1 {
			t.Fatalf("release step %q is missing, repeated or out of order", step)
		}
		previous = at
	}
	source := componentRegion(t, "error_trap 'localizations push branch'", "error_trap 'extension push branch'")
	setup := `
mkdir -p "$BUILD_HOME/extension" "$BUILD_HOME/all"
printf '#!/bin/sh\nexit 0\n' > "$BUILD_HOME/all/npm-package-ready.zsh"
chmod +x "$BUILD_HOME/all/npm-package-ready.zsh"
npm_edit_module() { record_component_step pin; }
npm_fork_version() { record_component_step install; }
node() {
    [[ "$PWD" == "$BUILD_HOME/extension" && "$*" == scripts/build-locales.js ]] || return 52
    record_component_step locales || return $?
    mkdir -p public/_locales/en
    print -r -- '{"new_key":{"message":"new release string"}}' > public/_locales/en/messages.json
}
git_commit() {
    [[ "$(<public/_locales/en/messages.json)" == '{"new_key":{"message":"new release string"}}' ]] || return 53
    record_component_step commit
}
git_tag() { record_component_step tag; }
`
	for _, publication := range []string{"yes", "no"} {
		for _, failure := range []string{"", "locales"} {
			t.Run(publication+"/failure="+failure, func(t *testing.T) {
				result := runComponent(t, source, setup, "SDK_NPM_PUBLISH="+publication, "FAIL_STEP="+failure)
				want := "install\nlocales\n"
				if publication == "yes" {
					want = "pin\n" + want
				}
				exit := 37
				if failure == "" {
					want += "commit\ntag\nrelease-continued\n"
					exit = 0
				}
				if result.exitCode != exit || strings.ReplaceAll(result.events, "message\n", "") != want {
					t.Fatalf("extension preparation = %+v, want exit %d and events %q", result, exit, want)
				}
			})
		}
	}
}
