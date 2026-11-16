// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"strings"
	"testing"
)

func TestOnboardingTemplatePreflightFailsBeforeProductionWork(t *testing.T) {
	source := componentRegion(t, "# Render every onboarding template/locale", "# These dependency-free Linux regressions")
	setup := `
mkdir -p "$WARP_HOME/mmm/onboarding"
URNETWORK_ROOT=caller-standalone-store
npm() {
    [[ "$PWD" == "$WARP_HOME/mmm/onboarding" && "$*" == 'ci --silent' ]] || return 52
    record_component_step email-dependencies
}
node() {
    [[ "$PWD" == "$WARP_HOME/mmm/onboarding" && "$*" == 'build.mjs check' && "$URNETWORK_ROOT" == "$BUILD_HOME" ]] || return 53
    record_component_step email-check
}
`
	source += `
[[ "$URNETWORK_ROOT" == caller-standalone-store ]] || exit 54
`
	for _, failure := range []string{"", "email-dependencies", "email-check"} {
		result := runComponent(t, source, setup, "FAIL_STEP="+failure)
		want, exit := "email-dependencies\nemail-check\nrelease-continued\n", 0
		if failure == "email-dependencies" {
			want, exit = "email-dependencies\n", 37
		} else if failure == "email-check" {
			want, exit = "email-dependencies\nemail-check\n", 37
		}
		if result.exitCode != exit || strings.ReplaceAll(result.events, "message\n", "") != want {
			t.Fatalf("onboarding preflight = %+v, want exit %d and events %q", result, exit, want)
		}
	}
}

func TestOnboardingExportUsesThePreflightLocalizationRoot(t *testing.T) {
	source := componentRegion(t, "    # Export the release's onboarding email templates", "    error_trap 'onboarding email templates export to Brevo'")
	for _, command := range []string{
		`URNETWORK_ROOT="$BUILD_HOME" ./build.sh upload`,
		`URNETWORK_ROOT="$BUILD_HOME" ./build.sh write-config --config "$WARP_HOME/config/main/onboarding.yml"`,
	} {
		if !strings.Contains(source, command) {
			t.Fatalf("export can select a different localization checkout than preflight: missing %s", command)
		}
	}
}
