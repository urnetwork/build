// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWebChecksFailBeforeReleaseWork(t *testing.T) {
	source := componentRegion(t, "error_trap 'localizations codegen'", "# Gradle evaluates")
	setup := `
mkdir -p "$BUILD_HOME/web/web"
make() {
    [[ "$PWD" == "$BUILD_HOME/web/web" && "$*" == check ]] || return 51
    [[ "$URNETWORK_ROOT" == "$BUILD_HOME" ]] || return 52
    record_component_step web-checks
}
`
	for _, failure := range []string{"", "web-checks"} {
		t.Run("failure="+failure, func(t *testing.T) {
			result := runComponent(t, source, setup, "FAIL_STEP="+failure)
			want := "web-checks\nrelease-continued\n"
			wantExit := 0
			if failure != "" {
				want, wantExit = "web-checks\n", 37
			}
			if result.exitCode != wantExit || strings.ReplaceAll(result.events, "message\n", "") != want {
				t.Fatalf("web preflight result = %+v, want exit %d and events %q", result, wantExit, want)
			}
		})
	}
}

func TestRunDisablesWebChecksOnlyForFinalWebBuild(t *testing.T) {
	source := componentRegion(t, `(cd $BUILD_HOME && URNETWORK_ROOT="$BUILD_HOME" WEB_BUILD_CHECKS=0`, `(cd $BUILD_HOME && warpctl build $BUILD_ENV web/manager/Makefile)`)
	setup := `
WEB_BUILD_CHECKS=caller-value
URNETWORK_ROOT=caller-root
BUILD_ENV=main
warpctl() {
    [[ "$*" == 'build main web/web/Makefile' ]] || return 51
    [[ "$WEB_BUILD_CHECKS" == 0 && "$URNETWORK_ROOT" == "$BUILD_HOME" ]] || return 52
    record_component_step web-image
}
`
	source += `
[[ "$WEB_BUILD_CHECKS" == caller-value && "$URNETWORK_ROOT" == caller-root ]] || exit 53
`
	result := runComponent(t, source, setup)
	if result.exitCode != 0 || strings.ReplaceAll(result.events, "message\n", "") != "web-image\nrelease-continued\n" {
		t.Fatalf("final web settings leaked outside the build command: %+v", result)
	}
}

func TestRunWebChecksFollowLocalizationAndPrecedeReleaseAllocation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	previous := -1
	for _, step := range []string{
		"error_trap 'ur.io content dates update'",
		"error_trap 'localizations codegen'",
		"error_trap 'web build checks'",
		"error_trap 'build bootstrap warpctl'",
		`warpctl stage version next release --message="$HOST build all"`,
		`URNETWORK_ROOT="$BUILD_HOME" WEB_BUILD_CHECKS=0 warpctl build $BUILD_ENV web/web/Makefile`,
	} {
		at := strings.Index(source, step)
		if at <= previous || strings.Count(source, step) != 1 {
			t.Fatalf("release step %q is missing, repeated or out of order", step)
		}
		previous = at
	}
}
