// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A clean release checkout has no warpctl binary. Gradle needs that exact
// sibling binary even when the license collector only requests dependencies.
func TestRunBootstrapsWarpctlBeforeLicenseGate(t *testing.T) {
	source := componentRegion(t, "error_trap 'localizations codegen'", "# Dependency vulnerability gate.")
	setup := `
WARP_HOME="$BUILD_HOME/workspace"
mkdir -p "$BUILD_HOME/warp/warpctl"
make() {
    [[ "$PWD" == "$BUILD_HOME/warp/warpctl" ]] || return 51
    record_component_step bootstrap || return $?
    if [[ "$ARTIFACT_MODE" == complete ]]; then
        mkdir -p build/darwin/arm64
        printf '#!/bin/sh\nprintf "warp-version-code\\n" >> "$BUILD_HOME/events"\n' > build/darwin/arm64/warpctl
        chmod +x build/darwin/arm64/warpctl
    fi
}
go() {
    [[ "$PWD" == "$BUILD_HOME/sdk" && "$#" == 6 && "$1 $2 $3" == 'run ./licenses -check' ]] || return 52
    [[ "$5" == -mmm-dir && "$6" == "$WARP_HOME/mmm" ]] || return 53
    record_component_step "license-$4" || return $?
    if [[ "$4" == android ]]; then
        "$BUILD_HOME/warp/warpctl/build/darwin/arm64/warpctl" ls version-code || return $?
    fi
}
`
	steps := []string{"bootstrap", "license-sdk", "license-android", "warp-version-code", "license-apple", "license-web", "license-extension", "release-continued"}
	for _, test := range []struct {
		name, fail, artifacts string
		wantExit, stepCount   int
	}{
		{"clean checkout", "", "complete", 0, len(steps)},
		{"bootstrap failure", "bootstrap", "complete", 37, 1},
		{"bootstrap lacks binary", "", "missing", 1, 1},
		{"SDK license failure", "license-sdk", "complete", 37, 2},
		{"Android license failure", "license-android", "complete", 37, 3},
		{"Apple license failure", "license-apple", "complete", 37, 5},
		{"web license failure", "license-web", "complete", 37, 6},
		{"extension license failure", "license-extension", "complete", 37, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runComponent(t, source, setup, "FAIL_STEP="+test.fail, "ARTIFACT_MODE="+test.artifacts)
			if result.exitCode != test.wantExit {
				t.Fatalf("exit = %d, want %d: %+v", result.exitCode, test.wantExit, result)
			}
			events := strings.ReplaceAll(result.events, "message\n", "")
			want := strings.Join(steps[:test.stepCount], "\n") + "\n"
			if events != want {
				t.Fatalf("gate events = %q, want %q; output=%s", events, want, result.output)
			}
		})
	}
}

func TestRunAllocatesReleaseVersionAfterLicenseAndTestGates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	last := -1
	for _, stage := range []string{
		"error_trap 'build bootstrap warpctl'",
		"error_trap 'license list check'",
		`builder_message "Build all test candidate passed. A version number can now be assigned."`,
		`warpctl stage version next release --message="$HOST build all"`,
		"error_trap 'build warpctl'",
	} {
		if strings.Count(source, stage) != 1 {
			t.Fatalf("expected exactly one %s", stage)
		}
		at := strings.Index(source, stage)
		if at <= last {
			t.Fatalf("release stage %s is out of order", stage)
		}
		last = at
	}
}
