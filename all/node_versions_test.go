// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"strings"
	"testing"
)

func TestComponentNodeVersionFailuresStopPreparationAndResume(t *testing.T) {
	regions := []string{
		componentRegion(t, "# Read pins from the selected component recipes", "# Check the provisioning receipt"),
		componentRegion(t, "    error_trap 'validate immutable release before resume'", "    windows_stage_state="),
	}
	setup := `
python3() {
    [[ "$*" == "-B $BUILD_HOME/all/check-node-versions.py $BUILD_HOME $WARP_HOME/mmm" ]] || return 52
    record_component_step component-node-versions
}
`
	for _, source := range regions {
		for _, failure := range []string{"", "component-node-versions"} {
			result := runComponent(t, source, setup, "FAIL_STEP="+failure)
			want, exit := "component-node-versions\nrelease-continued\n", 0
			if failure != "" {
				want, exit = "component-node-versions\n", 37
			}
			if result.exitCode != exit || strings.ReplaceAll(result.events, "message\n", "") != want {
				t.Fatalf("component Node preflight = %+v, want exit %d and events %q", result, exit, want)
			}
		}
	}
}

func TestComponentNodeVersionCheckFollowsSourceSelectionBeforeCandidates(t *testing.T) {
	source := componentRegion(t, "error_trap 'pull mmm'", "# license list gate:")
	previous := -1
	for _, step := range []string{
		"error_trap 'pull mmm'",
		"error_trap 'component Node versions before preparation'",
		"error_trap 'localizations codegen'",
		"error_trap 'web build checks'",
		"error_trap 'mmm build checks'",
	} {
		at := strings.Index(source, step)
		if at <= previous || strings.Count(source, step) != 1 {
			t.Fatalf("release step %q is missing, repeated or out of order", step)
		}
		previous = at
	}
}
