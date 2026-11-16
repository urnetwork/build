// SPDX-License-Identifier: MPL-2.0
package allbuild

import (
	"strings"
	"testing"
)

func TestRuntimePayloadPreflightFailsBeforeCandidateAndReleaseWork(t *testing.T) {
	source := componentRegion(t, "# Warm the byte-verified package cache", "# Check the provisioning receipt")
	setup := `
python3() {
    [[ "$*" == "-B $BUILD_HOME/server/cli/prepare-runtime-packages.py --all" ]] || return 52
    record_component_step runtime-payloads
}
`
	for _, failure := range []string{"", "runtime-payloads"} {
		result := runComponent(t, source, setup, "FAIL_STEP="+failure)
		want, exit := "runtime-payloads\nrelease-continued\n", 0
		if failure != "" {
			want, exit = "runtime-payloads\n", 37
		}
		if result.exitCode != exit || strings.ReplaceAll(result.events, "message\n", "") != want {
			t.Fatalf("runtime payload preflight = %+v, want exit %d and events %q", result, exit, want)
		}
	}
	ordered := componentRegion(t, "error_trap 'pull mmm'", "# license list gate:")
	previous := -1
	for _, step := range []string{"error_trap 'pull mmm'", "error_trap 'locked service runtime payloads before preparation'", "error_trap 'localizations codegen'", "error_trap 'web build checks'", "error_trap 'mmm build checks'"} {
		at := strings.Index(ordered, step)
		if at <= previous {
			t.Fatalf("missing or out-of-order early payload gate: %s", step)
		}
		previous = at
	}
}
