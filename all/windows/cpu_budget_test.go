// CPU budget tests exercise the shared shell boundary without starting QEMU.
// SPDX-License-Identifier: MPL-2.0

package windowsbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Remove selected inherited variables so each shell case has a deterministic
// environment even when the surrounding test command carries a CPU budget.
func environmentWithout(names ...string) []string {
	excludedNames := map[string]bool{}
	for _, name := range names {
		excludedNames[name] = true
	}

	environment := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !excludedNames[name] {
			environment = append(environment, entry)
		}
	}
	return environment
}

// Source the production helper and return the resolved QEMU CPU value.
func runWinInit(t *testing.T, environment ...string) (string, error) {
	t.Helper()
	script := `
set -euo pipefail
source "$1"
win_init
printf '%s' "$CPUS"
`
	command := exec.Command("bash", "-c", script, "win-cpu-test", filepath.Join(windowsBuildRoot(t), "lib.sh"))
	command.Env = append(environmentWithout("CPUS", "GOMAXPROCS"), environment...)
	output, err := command.CombinedOutput()
	return string(output), err
}

// A valid Go process budget is also the VM ceiling, while standalone callers
// without one retain the pre-existing CPUS/default behavior.
func TestWinInitAppliesCPUBudget(t *testing.T) {
	tests := []struct {
		name        string
		environment []string
		wantCPUs    string
	}{
		{name: "Go budget supplies an unset CPUS", environment: []string{"GOMAXPROCS=1"}, wantCPUs: "1"},
		{name: "Go budget caps a higher explicit CPUS", environment: []string{"GOMAXPROCS=2", "CPUS=6"}, wantCPUs: "2"},
		{name: "lower explicit CPUS is preserved", environment: []string{"GOMAXPROCS=2", "CPUS=1"}, wantCPUs: "1"},
		{name: "both unset use the legacy default", environment: nil, wantCPUs: "6"},
		{name: "invalid Go budget uses the legacy default", environment: []string{"GOMAXPROCS=invalid"}, wantCPUs: "6"},
		{name: "invalid Go budget preserves explicit CPUS", environment: []string{"GOMAXPROCS=-2", "CPUS=4"}, wantCPUs: "4"},
		{name: "valid Go budget replaces rather than raises the legacy default", environment: []string{"GOMAXPROCS=8"}, wantCPUs: "8"},
	}

	for _, test := range tests {
		gotCPUs, err := runWinInit(t, test.environment...)
		if err != nil {
			t.Errorf("%s: win_init failed: %v: %s", test.name, err, gotCPUs)
			continue
		}
		if gotCPUs != test.wantCPUs {
			t.Errorf("%s: CPUS = %q, want %q", test.name, gotCPUs, test.wantCPUs)
		}
	}
}

// Invalid explicit values fail initialization, so no caller can hand malformed
// CPU syntax to QEMU.
func TestWinInitRejectsMalformedCPUs(t *testing.T) {
	for _, cpus := range []string{"0", "-1", "invalid", "1.5", " 2"} {
		output, err := runWinInit(t, "GOMAXPROCS=2", "CPUS="+cpus)
		if err == nil {
			t.Errorf("CPUS=%q: win_init unexpectedly succeeded with %q", cpus, output)
			continue
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Errorf("CPUS=%q: exit = %v, want status 1", cpus, err)
		}
		if !strings.Contains(output, "CPUS must be a positive integer") {
			t.Errorf("CPUS=%q: error output = %q", cpus, output)
		}
	}
}

// Parse direct multi-line QEMU commands so a newly added launch site cannot
// silently bypass the shared effective CPU value.
func directQEMULaunches(source string) []string {
	lines := strings.Split(source, "\n")
	launches := []string{}
	for lineIndex := 0; lineIndex < len(lines); lineIndex++ {
		line := strings.TrimSpace(lines[lineIndex])
		if line != `qemu-system-aarch64 \` && line != `exec qemu-system-aarch64 \` {
			continue
		}
		launchLines := []string{line}
		for strings.HasSuffix(strings.TrimSpace(lines[lineIndex]), `\`) && lineIndex+1 < len(lines) {
			lineIndex++
			launchLines = append(launchLines, strings.TrimSpace(lines[lineIndex]))
		}
		launches = append(launches, strings.Join(launchLines, "\n"))
	}
	return launches
}

// Every direct lifecycle and diagnostic command expands the value normalized by
// win_init rather than carrying an independent vCPU default.
func TestEveryDirectQEMULaunchUsesEffectiveCPUs(t *testing.T) {
	tests := []struct {
		filename     string
		wantLaunches int
	}{
		{filename: "lib.sh", wantLaunches: 3},
		{filename: "TESTCOMMAND.txt", wantLaunches: 1},
	}
	for _, test := range tests {
		source := readBuildFile(t, test.filename)
		launches := directQEMULaunches(source)
		if len(launches) != test.wantLaunches {
			t.Errorf("%s: found %d direct QEMU launches, want %d", test.filename, len(launches), test.wantLaunches)
			continue
		}
		for launchIndex, launch := range launches {
			if !strings.Contains(launch, `-smp "$CPUS"`) {
				t.Errorf("%s launch %d does not use effective CPUS:\n%s", test.filename, launchIndex+1, launch)
			}
		}
	}

	diagnostic := readBuildFile(t, "TESTCOMMAND.txt")
	initPosition := strings.Index(diagnostic, "win_init")
	launchPosition := strings.Index(diagnostic, "exec qemu-system-aarch64")
	if initPosition < 0 || launchPosition < 0 || initPosition > launchPosition {
		t.Errorf("diagnostic QEMU command is not preceded by win_init: init=%d launch=%d", initPosition, launchPosition)
	}
}

// Invoke each production lifecycle function with a recording QEMU substitute;
// this proves the resolved value reaches every actual -smp argument.
func TestVMLaunchersConsumeEffectiveCPUs(t *testing.T) {
	temporaryDirectory := t.TempDir()
	eventLog := filepath.Join(temporaryDirectory, "qemu-cpus.log")
	uefiVariables := filepath.Join(temporaryDirectory, "uefi-vars.fd")
	if err := os.WriteFile(uefiVariables, []byte("test vars"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
set -euo pipefail
source "$1"
event_log="$2"

qemu-system-aarch64() {
  local smp=""
  while [ "$#" -gt 0 ]; do
    if [ "$1" = -smp ]; then
      shift
      [ "$#" -gt 0 ] || return 64
      smp="$1"
      break
    fi
    shift
  done
  [ -n "$smp" ] || return 65
  printf '%s:%s\n' "$WIN_CPU_TEST_PATH" "$smp" >>"$event_log"
}
qemu-img() { return 0; }
win_make_autounattend_iso() { printf '%s\n' "$WIN_RUN_DIR/autounattend.iso"; }
win_press_any_key() { return 0; }
win_wait_ssh() { return 0; }

win_init
UEFI_VARS_TEMPLATE="$3"
IMAGE="$4"

WIN_CPU_TEST_PATH=install
win_install_image windows.iso virtio.iso
wait
WIN_CPU_TEST_PATH=overlay
win_boot_vm
wait
WIN_CPU_TEST_PATH=image-rw
win_boot_image_rw
wait
`
	command := exec.Command(
		"bash", "-c", script, "win-launch-test",
		filepath.Join(windowsBuildRoot(t), "lib.sh"), eventLog, uefiVariables,
		filepath.Join(temporaryDirectory, "windows.qcow2"),
	)
	command.Env = append(
		environmentWithout("CPUS", "GOMAXPROCS", "TMPDIR"),
		"GOMAXPROCS=1", "TMPDIR="+temporaryDirectory,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("exercise VM launchers: %v\n%s", err, output)
	}

	events, err := os.ReadFile(eventLog)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := "install:1\noverlay:1\nimage-rw:1\n"
	if string(events) != wantEvents {
		t.Fatalf("QEMU CPU events = %q, want %q", events, wantEvents)
	}
}
