// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ur.xyz's checks run in web and ur.io's in the mmm service; either failing
// stops the release before any app or service work.
func TestRunWebAndMmmChecksFailBeforeReleaseWork(t *testing.T) {
	source := componentRegion(t, `builder_message "checking web builds`, "# license list gate:")
	setup := `
mkdir -p "$BUILD_HOME/web/web" "$WARP_HOME/mmm/mmm"
python3() { return 0; } # Gate evidence storage is covered by real entrypoint fixtures.
make() {
    [[ "$*" == check && "$URNETWORK_ROOT" == "$BUILD_HOME" ]] || return 52
    case "$PWD" in
        "$BUILD_HOME/web/web") record_component_step web-checks ;;
        "$WARP_HOME/mmm/mmm") record_component_step mmm-checks ;;
        *) return 51 ;;
    esac
}
`
	for _, testCase := range []struct {
		failure  string
		want     string
		wantExit int
	}{
		{"", "web-checks\nmmm-checks\nrelease-continued\n", 0},
		{"web-checks", "web-checks\n", 37},
		{"mmm-checks", "web-checks\nmmm-checks\n", 37},
	} {
		t.Run("failure="+testCase.failure, func(t *testing.T) {
			result := runComponent(t, source, setup, "FAIL_STEP="+testCase.failure)
			if result.exitCode != testCase.wantExit || strings.ReplaceAll(result.events, "message\n", "") != testCase.want {
				t.Fatalf("site preflight result = %+v, want exit %d and events %q", result, testCase.wantExit, testCase.want)
			}
		})
	}
}

// The final web and mmm image builds skip the checks that already ran, and mmm
// builds against this release's warp through a workspace, not the developer's
// checkout its go.mod names. None of those settings outlive their command.
func TestRunDisablesSiteChecksOnlyForFinalWebAndMmmBuilds(t *testing.T) {
	source := componentRegion(t, `if release_build image-web `, `if release_build image-manager `)
	setup := `
WEB_BUILD_CHECKS=caller-value
URNETWORK_ROOT=caller-root
BUILD_ENV=main
mkdir -p "$WARP_HOME/mmm/mmm"
printf 'module github.com/urnetwork/mmm/mmm\n\ngo 1.25.0\n\nrequire github.com/urnetwork/warp v0.0.0\n' > "$WARP_HOME/mmm/mmm/go.mod"
warpctl() {
    [[ "$WEB_BUILD_CHECKS" == 0 && "$URNETWORK_ROOT" == "$BUILD_HOME" ]] || return 52
    case "$*" in
        'build main web/web/Makefile')
            [[ -z "${GOWORK:-}" ]] || return 54
            record_component_step web-image
            ;;
        "build main $WARP_HOME/mmm/mmm/Makefile")
            [[ "$(<"$GOWORK")" == "go 1.25.0

use $WARP_HOME/mmm/mmm

replace github.com/urnetwork/warp => $BUILD_HOME/warp" ]] || return 55
            print -r -- "$GOWORK" > "$BUILD_HOME/gowork-path"
            record_component_step mmm-image
            ;;
        *)
            return 51
            ;;
    esac
}
`
	source += `
[[ "$WEB_BUILD_CHECKS" == caller-value && "$URNETWORK_ROOT" == caller-root && -z "${GOWORK:-}" ]] || exit 53
[[ ! -e "$(<"$BUILD_HOME/gowork-path")" ]] || exit 56
`
	result := runComponent(t, source, setup)
	if result.exitCode != 0 || strings.ReplaceAll(result.events, "message\n", "") != "web-image\nmmm-image\nrelease-continued\n" {
		t.Fatalf("final site build settings are wrong or leaked outside their build commands: %+v", result)
	}
}

func TestRunSiteChecksFollowLocalizationAndPrecedeReleaseAllocation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	previous := -1
	for _, step := range []string{
		"error_trap 'ur.io content dates update'",
		"error_trap 'localizations codegen'",
		"error_trap 'build bootstrap warpctl'",
		"error_trap 'early read-only deployment preflight'",
		"error_trap 'web build checks'",
		"error_trap 'mmm build checks'",
		`warpctl stage version next release --message="$HOST build all"`,
		`URNETWORK_ROOT="$BUILD_HOME" WEB_BUILD_CHECKS=0 warpctl build $BUILD_ENV web/web/Makefile`,
		`URNETWORK_ROOT="$BUILD_HOME" WEB_BUILD_CHECKS=0 warpctl build $BUILD_ENV "$WARP_HOME/mmm/mmm/Makefile"`,
		`source "$BUILD_HOME/all/deploy-rollout.zsh"`,
	} {
		at := strings.Index(source, step)
		if at <= previous || strings.Count(source, step) != 1 {
			t.Fatalf("release step %q is missing, repeated or out of order", step)
		}
		previous = at
	}
}
