// Windows architecture plan tests exercise the shell boundary without QEMU.
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

// Resolve the production plan and serialize its build-facing values.
func runWindowsArchitecturePlan(t *testing.T, environment ...string) (string, error) {
	t.Helper()
	script := `
set -euo pipefail
source "$1"
win_windows_build_plan
printf '%s|%s|%s|%s|%s' \
  "$WIN_BUILD_ARCHITECTURES" \
  "$WIN_BUILD_SDK_POWERSHELL_ARGUMENTS" \
  "$WIN_BUILD_APP_POWERSHELL_ARGUMENTS" \
  "${WIN_BUILD_MSI_SUFFIXES[*]}" \
  "$WIN_BUILD_SKIP_CONTRACT_TESTS"
`
	command := exec.Command(
		"bash", "-c", script, "windows-architecture-plan-test",
		filepath.Join(windowsBuildRoot(t), "build-plan.sh"),
	)
	command.Env = append(
		environmentWithout("WINDOWS_BUILD_ARCHITECTURES", "WINDOWS_BUILD_SKIP_CONTRACT_TESTS"),
		environment...,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestWindowsArchitecturePlanPreservesReleaseDefaultAndFocusedSelections(t *testing.T) {
	tests := []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "unset release plan", want: "amd64,arm64|||x64 arm64|0"},
		{name: "explicit release plan", environment: []string{"WINDOWS_BUILD_ARCHITECTURES=amd64,arm64"}, want: "amd64,arm64|||x64 arm64|0"},
		{name: "amd64 only", environment: []string{"WINDOWS_BUILD_ARCHITECTURES=amd64"}, want: "amd64|-Architectures amd64|-Platforms x64|x64|0"},
		{name: "arm64 only", environment: []string{"WINDOWS_BUILD_ARCHITECTURES=arm64"}, want: "arm64|-Architectures arm64|-Platforms ARM64|arm64|0"},
		{name: "unit tests explicitly skipped", environment: []string{"WINDOWS_BUILD_SKIP_CONTRACT_TESTS=1"}, want: "amd64,arm64|||x64 arm64|1"},
	}

	for _, test := range tests {
		output, err := runWindowsArchitecturePlan(t, test.environment...)
		if err != nil {
			t.Errorf("%s: resolve plan: %v: %s", test.name, err, output)
			continue
		}
		if output != test.want {
			t.Errorf("%s: plan = %q, want %q", test.name, output, test.want)
		}
	}
}

func TestWindowsArchitecturePlanRejectsInvalidInput(t *testing.T) {
	for _, environment := range [][]string{
		{"WINDOWS_BUILD_ARCHITECTURES=x64"},
		{"WINDOWS_BUILD_ARCHITECTURES=arm64,amd64"},
		{"WINDOWS_BUILD_ARCHITECTURES=arm64;Write-Host injected"},
		{"WINDOWS_BUILD_SKIP_CONTRACT_TESTS=true"},
		{"WINDOWS_BUILD_SKIP_CONTRACT_TESTS=2"},
	} {
		output, err := runWindowsArchitecturePlan(t, environment...)
		if err == nil {
			t.Errorf("environment %q unexpectedly resolved as %q", environment, output)
			continue
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 2 {
			t.Errorf("environment %q exit = %v, want status 2", environment, err)
		}
	}
}

// Exercise exact artifact checks independently from the expensive guest build.
func runWindowsOutputVerification(
	t *testing.T,
	architectures string,
	outputDirectory string,
	version string,
) (string, error) {
	t.Helper()
	script := `
set -euo pipefail
source "$1"
win_windows_build_plan
win_windows_verify_msi_outputs "$2" "$3"
`
	command := exec.Command(
		"bash", "-c", script, "windows-output-test",
		filepath.Join(windowsBuildRoot(t), "build-plan.sh"), outputDirectory, version,
	)
	command.Env = append(
		environmentWithout("WINDOWS_BUILD_ARCHITECTURES", "WINDOWS_BUILD_SKIP_CONTRACT_TESTS"),
		"WINDOWS_BUILD_ARCHITECTURES="+architectures,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestWindowsOutputVerificationRequiresExactlyTheSelectedArtifacts(t *testing.T) {
	const version = "2026.9.7-1"
	tests := []struct {
		name          string
		architectures string
		msiSuffixes   []string
		wantError     string
	}{
		{name: "release outputs", architectures: "amd64,arm64", msiSuffixes: []string{"x64", "arm64"}},
		{name: "arm64 output", architectures: "arm64", msiSuffixes: []string{"arm64"}},
		{name: "missing selected output", architectures: "arm64", wantError: "requested Windows artifact is missing"},
		{name: "unexpected output", architectures: "arm64", msiSuffixes: []string{"arm64", "x64"}, wantError: "unrequested MSI"},
	}

	for _, test := range tests {
		outputDirectory := t.TempDir()
		for _, suffix := range test.msiSuffixes {
			filename := filepath.Join(outputDirectory, "URnetwork-"+version+"-"+suffix+".msi")
			if err := os.WriteFile(filename, []byte("msi"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		output, err := runWindowsOutputVerification(
			t, test.architectures, outputDirectory, version,
		)
		if test.wantError == "" {
			if err != nil {
				t.Errorf("%s: verify output: %v: %s", test.name, err, output)
			}
			continue
		}
		if err == nil || !strings.Contains(output, test.wantError) {
			t.Errorf("%s: result = %v, %q; want error containing %q", test.name, err, output, test.wantError)
		}
	}
}

func TestWindowsBuildScriptsThreadTheValidatedPlan(t *testing.T) {
	lowerBuild := readBuildFile(t, "build.sh")
	for _, required := range []string{
		`source "$here/build-plan.sh"`,
		"win_windows_build_plan",
		`if [ "$WIN_BUILD_SKIP_CONTRACT_TESTS" = 1 ]`,
		`if [ "$WIN_BUILD_SKIP_CONTRACT_TESTS" != 1 ]`,
		"run-windows-lib.test.ps1",
		"$WIN_BUILD_SDK_POWERSHELL_ARGUMENTS",
		"$WIN_BUILD_APP_POWERSHELL_ARGUMENTS",
		`win_windows_verify_msi_outputs "$OUT_DIR" "$VERSION"`,
	} {
		if !strings.Contains(lowerBuild, required) {
			t.Errorf("all/windows/build.sh is missing plan contract %q", required)
		}
	}
	hostSkip := strings.Index(lowerBuild, `if [ "$WIN_BUILD_SKIP_CONTRACT_TESTS" = 1 ]`)
	hostContracts := strings.Index(lowerBuild, "(cd \"$here\" && go test ./...)")
	guestSkip := strings.Index(lowerBuild, `if [ "$WIN_BUILD_SKIP_CONTRACT_TESTS" != 1 ]`)
	guestLibraryCopy := strings.Index(lowerBuild, `win_scp_to "$BUILD_HOME/all/acceptance/run-windows-lib.ps1"`)
	guestTestCopy := strings.Index(lowerBuild, `win_scp_to "$BUILD_HOME/all/acceptance/run-windows-lib.test.ps1"`)
	guestContracts := strings.Index(lowerBuild, `-File $guest_contract_dir/run-windows-lib.test.ps1`)
	sdkBuild := strings.Index(lowerBuild, "build-sdk.ps1 -Version")
	if hostSkip < 0 || hostContracts < 0 || guestSkip < 0 || guestLibraryCopy < 0 || guestTestCopy < 0 || guestContracts < 0 || sdkBuild < 0 ||
		!(hostSkip < hostContracts && hostContracts < guestSkip && guestSkip < guestLibraryCopy &&
			guestLibraryCopy < guestTestCopy && guestTestCopy < guestContracts && guestContracts < sdkBuild) {
		t.Fatalf("default-only contract ownership ordering is invalid: host-skip=%d host=%d guest-skip=%d library-copy=%d test-copy=%d guest=%d sdk=%d", hostSkip, hostContracts, guestSkip, guestLibraryCopy, guestTestCopy, guestContracts, sdkBuild)
	}

	upperBuild := readBuildFile(t, "../build-windows.sh")
	plan := strings.Index(upperBuild, "win_windows_build_plan")
	stage := strings.Index(upperBuild, "stage_local_repos sdk connect glog goidenticons windows")
	build := strings.Index(upperBuild, `"$here/windows/build.sh"`)
	verify := strings.Index(upperBuild, `win_windows_verify_msi_outputs "$OUT_DIR" "$EXTERNAL_WARP_VERSION"`)
	if plan < 0 || stage < 0 || build < 0 || verify < 0 || !(plan < stage && stage < build && build < verify) {
		t.Fatalf("top-level Windows plan ordering is invalid: plan=%d stage=%d build=%d verify=%d", plan, stage, build, verify)
	}
}
