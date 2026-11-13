// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"strings"
	"testing"
)

func TestLinuxPreflightFailureStopsReleasePreparation(t *testing.T) {
	source := componentRegion(t, "# These dependency-free Linux regressions", "# Gradle and deployment guard")
	setup := `
BUILD_TEST=
python3() {
    [[ "$*" == "-B $BUILD_HOME/linux/app/tests/Preflight.py" ]] || return 52
    record_component_step linux-preflight
}
`
	for _, failure := range []string{"", "linux-preflight"} {
		result := runComponent(t, source, setup, "FAIL_STEP="+failure)
		want, exit := "linux-preflight\nrelease-continued\n", 0
		if failure != "" {
			want, exit = "linux-preflight\n", 37
		}
		if result.exitCode != exit || strings.ReplaceAll(result.events, "message\n", "") != want {
			t.Fatalf("Linux preflight result = %+v, want exit %d and events %q", result, exit, want)
		}
	}
}
