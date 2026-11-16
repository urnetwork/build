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
python3() {
    [[ "$*" == "-B $BUILD_HOME/all/release-state.py check-brevo-contract $WARP_HOME" ]] || return 55
    record_component_step email-state-contract
}
`
	source += `
[[ "$URNETWORK_ROOT" == caller-standalone-store ]] || exit 54
`
	for _, failure := range []string{"", "email-dependencies", "email-check", "email-state-contract"} {
		result := runComponent(t, source, setup, "FAIL_STEP="+failure)
		want, exit := "email-dependencies\nemail-check\nemail-state-contract\nrelease-continued\n", 0
		if failure == "email-dependencies" {
			want, exit = "email-dependencies\n", 37
		} else if failure == "email-check" {
			want, exit = "email-dependencies\nemail-check\n", 37
		} else if failure == "email-state-contract" {
			want, exit = "email-dependencies\nemail-check\nemail-state-contract\n", 37
		}
		if result.exitCode != exit || strings.ReplaceAll(result.events, "message\n", "") != want {
			t.Fatalf("onboarding preflight = %+v, want exit %d and events %q", result, exit, want)
		}
	}
}

func TestBrevoProgramsCheckpointTheirOwnConfigCommitImmediately(t *testing.T) {
	source := componentRegion(t, "    # Export the release's onboarding email templates", "    # the release's config, template ids included")
	previous := -1
	for _, step := range []string{
		"error_trap 'onboarding templates config push'",
		"release_transition onboarding",
		`URNETWORK_ROOT="$BUILD_HOME" ./build.sh upload`,
		`URNETWORK_ROOT="$BUILD_HOME" ./build.sh write-config --config "$WARP_HOME/config/main/reengagement.yml"`,
		"error_trap 're-engagement templates config push'",
		"release_transition reengagement",
	} {
		// Start after the preceding checkpoint so the two upload invocations
		// cannot accidentally satisfy one another's program contract.
		at := strings.Index(source[previous+1:], step)
		if at < 0 {
			t.Fatalf("missing or out-of-order Brevo program step: %s", step)
		}
		previous += at + 1
	}
}

func TestReengagementPreflightUsesSelectedStoreAndFailsBeforeContract(t *testing.T) {
	source := componentRegion(t, "error_trap 'onboarding email templates before release preparation'", "# These dependency-free Linux regressions")
	setup := `
mkdir -p "$WARP_HOME/config/main" "$WARP_HOME/mmm/reengagement"
: > "$WARP_HOME/config/main/reengagement.yml"
node() {
    [[ "$PWD" == "$WARP_HOME/mmm/reengagement" && "$*" == 'build.mjs check' && "$URNETWORK_ROOT" == "$BUILD_HOME" ]] || return 52
    record_component_step reengagement-check
}
python3() { record_component_step state-contract; }
`
	for _, failure := range []string{"", "reengagement-check"} {
		result := runComponent(t, source, setup, "FAIL_STEP="+failure)
		want, exit := "reengagement-check\nstate-contract\nrelease-continued\n", 0
		if failure != "" {
			want, exit = "reengagement-check\n", 37
		}
		if result.exitCode != exit || strings.ReplaceAll(result.events, "message\n", "") != want {
			t.Fatalf("reengagement preflight = %+v, want exit %d and events %q", result, exit, want)
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
