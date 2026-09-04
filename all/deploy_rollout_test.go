// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const rolloutHarness = `
set -u

event_log="$1"
sample_count_file="$2"
rollout_script="$3"
: > "$event_log"

record_event() {
    printf '%s' "$1" >> "$event_log"
    shift
    for argument in "$@"; do
        printf '\t%s' "$argument" >> "$event_log"
    done
    printf '\n' >> "$event_log"
}

warpctl() {
    record_event warpctl "$@"

    if [[ "$1" = ls ]]; then
        local sample_index=0
        if [[ -r "$sample_count_file" ]]; then
            read sample_index < "$sample_count_file"
        fi
        (( sample_index += 1 ))
        printf '%d\n' "$sample_index" > "$sample_count_file"
        if [[ -n "${FAIL_SAMPLE_INDEX:-}" && "$sample_index" = "$FAIL_SAMPLE_INDEX" ]]; then
            return "${FAIL_CODE:-41}"
        fi
        printf 'sample-%d\n' "$sample_index"
        return 0
    fi

    if [[ -n "${FAIL_SERVICE:-}" && "$3" = "$FAIL_SERVICE" && "$5" = "--percent=${FAIL_PERCENT}" ]]; then
        return "${FAIL_CODE:-37}"
    fi
}

builder_message() {
    record_event message "$1"
}

sleep() {
    record_event sleep "$1"
}

BUILD_ENV=main
WARP_VERSION=2026.9.3+1036806790
EXTERNAL_WARP_VERSION=2026.9.3-1036806790
STAGE_SECONDS=60

source "$rollout_script"
if [[ -n "${SINGLE_SERVICE:-}" ]]; then
    warp_rollout_deploy "$SINGLE_SERVICE" "${SINGLE_PERCENT:-25}"
else
    warp_rollout
fi
`

type rolloutResult struct {
	exitCode int
	events   []string
	output   string
}

// Resolve files beside this test without depending on the invoking directory.
func rolloutRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate rollout test")
	}
	return filepath.Dir(filename)
}

// Execute the production shell functions against deterministic fake boundaries.
func runRollout(t *testing.T, environment ...string) rolloutResult {
	t.Helper()
	tempDir := t.TempDir()
	eventLog := filepath.Join(tempDir, "events")
	sampleCountFile := filepath.Join(tempDir, "sample-count")
	command := exec.Command(
		"zsh",
		"-c",
		rolloutHarness,
		"rollout-test",
		eventLog,
		sampleCountFile,
		filepath.Join(rolloutRoot(t), "deploy-rollout.zsh"),
	)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatal(err)
		}
		exitCode = exitError.ExitCode()
	}

	eventData, readErr := os.ReadFile(eventLog)
	if readErr != nil {
		t.Fatalf("read rollout events: %v; output=%s", readErr, output)
	}
	eventText := strings.TrimSuffix(string(eventData), "\n")
	events := []string{}
	if eventText != "" {
		events = strings.Split(eventText, "\n")
	}
	return rolloutResult{exitCode: exitCode, events: events, output: string(output)}
}

// Format the fake process boundary exactly as the shell harness records it.
func deployEvent(service string, percent int, onlyOlder bool) string {
	event := fmt.Sprintf(
		"warpctl\tdeploy\tmain\t%s\t2026.9.3+1036806790\t--percent=%d",
		service,
		percent,
	)
	if onlyOlder {
		event += "\t--only-older"
	}
	return event
}

// Format the success boundary paired with one successful deploy invocation.
func deployMessage(service string, percent int, onlyOlder bool) string {
	suffix := ""
	if onlyOlder {
		suffix = " (only older)"
	}
	return fmt.Sprintf(
		"message\tmain[%d%%] %s `2026.9.3-1036806790` deployed%s",
		percent,
		service,
		suffix,
	)
}

// Assert the complete cumulative rollout order and every only-older boundary.
func TestRolloutUsesOnlyOlderOnlyForSampleableServicesAcrossEveryWave(t *testing.T) {
	result := runRollout(t)
	if result.exitCode != 0 {
		t.Fatalf("rollout exit = %d, want 0; output=%s", result.exitCode, result.output)
	}

	expected := []string{
		"warpctl\tls\tversions\tmain\t--sample",
		"message\tmain[0%] services: ```sample-1```",
		deployEvent("config-updater", 100, false),
		deployMessage("config-updater", 100, false),
		deployEvent("grafana", 100, true),
		deployMessage("grafana", 100, true),
	}
	services := []string{"lb", "taskworker", "api", "connect", "web", "app", "mcp", "proxy"}
	for _, percent := range []int{25, 50, 75, 100} {
		for _, service := range services {
			onlyOlder := service != "lb" && service != "proxy"
			expected = append(
				expected,
				deployEvent(service, percent, onlyOlder),
				deployMessage(service, percent, onlyOlder),
			)
		}
		sampleIndex := percent/25 + 1
		expected = append(
			expected,
			"warpctl\tls\tversions\tmain\t--sample",
			fmt.Sprintf("message\tmain[%d%%] services: ```sample-%d```", percent, sampleIndex),
		)
		if percent < 100 {
			expected = append(expected, "sleep\t60")
		}
	}

	if strings.Join(result.events, "\n") != strings.Join(expected, "\n") {
		t.Fatalf(
			"rollout events differ\ngot:\n%s\nwant:\n%s",
			strings.Join(result.events, "\n"),
			strings.Join(expected, "\n"),
		)
	}
}

// Prove observed and mid-wave deploy failures preserve their code and stop at
// the failed command, before its success message or the next service.
func TestRolloutDeployFailureStopsImmediately(t *testing.T) {
	tests := []struct {
		service  string
		percent  int
		exitCode int
	}{
		{service: "config-updater", percent: 100, exitCode: 37},
		{service: "lb", percent: 50, exitCode: 38},
		{service: "proxy", percent: 75, exitCode: 39},
	}
	for _, test := range tests {
		result := runRollout(
			t,
			"FAIL_SERVICE="+test.service,
			fmt.Sprintf("FAIL_PERCENT=%d", test.percent),
			fmt.Sprintf("FAIL_CODE=%d", test.exitCode),
		)
		if result.exitCode != test.exitCode {
			t.Errorf(
				"%s %d%% failure exit = %d, want %d; output=%s",
				test.service,
				test.percent,
				result.exitCode,
				test.exitCode,
				result.output,
			)
			continue
		}
		expectedLast := deployEvent(test.service, test.percent, false)
		if len(result.events) == 0 {
			t.Errorf("%s %d%% failure recorded no events", test.service, test.percent)
			continue
		}
		if result.events[len(result.events)-1] != expectedLast {
			t.Errorf(
				"%s %d%% failure last event = %q, want %q; all=%v",
				test.service,
				test.percent,
				result.events[len(result.events)-1],
				expectedLast,
				result.events,
			)
		}
	}
}

// Keep new rollout services fail closed until their observation capability is
// classified explicitly.
func TestRolloutRejectsUnknownServiceBeforeWarpctl(t *testing.T) {
	result := runRollout(t, "SINGLE_SERVICE=unknown-service", "SINGLE_PERCENT=25")
	if result.exitCode != 64 {
		t.Fatalf("unknown service exit = %d, want 64; output=%s", result.exitCode, result.output)
	}
	if len(result.events) != 0 {
		t.Fatalf("unknown service reached an external boundary: %v", result.events)
	}
	if !strings.Contains(result.output, "Unknown rollout service classification: unknown-service") {
		t.Fatalf("unknown service diagnostic missing: %q", result.output)
	}
}

// Prove a failed post-wave observation cannot be labeled successful or allow
// the next wait and deployment wave to start.
func TestRolloutStatusSampleFailureStopsBeforeNextWave(t *testing.T) {
	result := runRollout(t, "FAIL_SAMPLE_INDEX=3", "FAIL_CODE=41")
	if result.exitCode != 41 {
		t.Fatalf("status failure exit = %d, want 41; output=%s", result.exitCode, result.output)
	}

	sampleEvent := "warpctl\tls\tversions\tmain\t--sample"
	sampleCount := 0
	sleepCount := 0
	for _, event := range result.events {
		if event == sampleEvent {
			sampleCount++
		}
		if event == "sleep\t60" {
			sleepCount++
		}
		if strings.HasPrefix(event, "message\tmain[50%] services:") {
			t.Errorf("failed 50%% sample emitted success message %q", event)
		}
		if event == deployEvent("lb", 75, false) {
			t.Errorf("failed 50%% sample advanced into the 75%% wave")
		}
	}
	if sampleCount != 3 || sleepCount != 1 {
		t.Fatalf("failed sample boundaries: samples=%d sleeps=%d, want 3 and 1; events=%v", sampleCount, sleepCount, result.events)
	}
	if result.events[len(result.events)-1] != sampleEvent {
		t.Fatalf("status failure last event = %q, want %q", result.events[len(result.events)-1], sampleEvent)
	}
}

// Pin the release entrypoint to the tested implementation and one immediate
// error trap instead of allowing raw, unchecked rollout commands to return.
func TestRunUsesCanonicalRolloutContract(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSource := string(runData)
	if strings.Contains(runSource, "warpctl deploy") {
		t.Fatal("run.sh contains a deploy outside the canonical rollout wrapper")
	}
	required := `source "$BUILD_HOME/build/all/deploy-rollout.zsh"
    warp_rollout
    error_trap 'warp service rollout'`
	if !strings.Contains(runSource, required) {
		t.Fatal("run.sh does not source, invoke, and immediately check the canonical rollout")
	}
}

// Pin assignment before export so zsh preserves a failed Warpctl substitution
// for the immediately following error trap.
func TestRunPreservesWarpVersionLookupFailures(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSource := string(runData)
	for _, masked := range []string{
		"export WARP_VERSION_BASE=`warpctl ls version`",
		"export WARP_VERSION_CODE=`warpctl ls version-code`",
		"export WARP_VERSION_BASE=$(warpctl ls version)",
		"export WARP_VERSION_CODE=$(warpctl ls version-code)",
	} {
		if strings.Contains(runSource, masked) {
			t.Errorf("run.sh masks Warpctl failure with %q", masked)
		}
	}
	for _, required := range []string{
		"WARP_VERSION_BASE=$(warpctl ls version)\nerror_trap 'warpctl version'\nexport WARP_VERSION_BASE",
		"WARP_VERSION_CODE=$(warpctl ls version-code)\nerror_trap 'warpctl version code'\nexport WARP_VERSION_CODE",
	} {
		if !strings.Contains(runSource, required) {
			t.Errorf("run.sh lacks checked Warp version lookup %q", required)
		}
	}
}
