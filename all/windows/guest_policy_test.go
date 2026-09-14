// SPDX-License-Identifier: MPL-2.0

package windowsbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func windowsBuildRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate Windows build test")
	}
	return filepath.Dir(filename)
}

func readBuildFile(t *testing.T, relative string) string {
	t.Helper()
	filename := filepath.Join(windowsBuildRoot(t), filepath.FromSlash(relative))
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	return string(data)
}

func TestReleaseBuildEnforcesGuestPolicyBeforeSourceOrBuild(t *testing.T) {
	source := readBuildFile(t, "build.sh")
	prepare := strings.Index(source, "win_prepare_hermetic_guest")
	syncSource := strings.Index(source, "win_sync_source")
	buildSDK := strings.Index(source, `win_ssh "powershell -ExecutionPolicy Bypass -File $WIN_DIR/windows/build-sdk.ps1`)
	if prepare < 0 || syncSource < 0 || buildSDK < 0 {
		t.Fatalf("build boundary missing: prepare=%d sync=%d sdk=%d", prepare, syncSource, buildSDK)
	}
	if !(prepare < syncSource && prepare < buildSDK) {
		t.Fatalf("interruptible work can start before guest policy verification: prepare=%d sync=%d sdk=%d", prepare, syncSource, buildSDK)
	}
}

func TestGuestPolicyStopsAndVerifiesUpdateServices(t *testing.T) {
	source := readBuildFile(t, "disable-auto-servicing.ps1")
	for _, required := range []string{
		"NoAutoUpdate",
		"NoAutoRebootWithLoggedOnUsers",
		`@("UsoSvc", "wuauserv")`,
		"start= disabled",
		"WaitForStatus(",
		"ServiceControllerStatus]::Stopped",
		`CurrentControlSet\Services\$Name`,
		"expected 4 (Disabled)",
		"DoNotConnectToWindowsUpdateInternetLocations",
		"DisableWindowsUpdateAccess",
		"UpdateServiceUrlAlternate",
		"UseWUServer",
		"Start-Sleep -Seconds 2",
		"New-NetFirewallRule",
		`@("UsoSvc", "wuauserv", "WaaSMedicSvc")`,
		"Get-NetFirewallServiceFilter",
		`C:\Windows\SoftwareDistribution\Download`,
		`Component Based Servicing\RebootPending`,
		`WindowsUpdate\Auto Update\RebootRequired`,
		`Stop-RequiredService "TrustedInstaller"`,
		`Get-Process -Name "TiWorker"`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("guest policy is missing required enforcement %q", required)
		}
	}
	if strings.Contains(source, "sc.exe stop   $s") {
		t.Fatal("guest policy regressed to fire-and-forget service stops")
	}
	configureBoth := strings.Index(source, "foreach ($serviceName in $requiredServices) {\n    Set-RequiredServiceStartDisabled")
	stopBoth := strings.Index(source, "foreach ($serviceName in $requiredServices) {\n    Stop-RequiredService")
	reconfigureBoth := -1
	if stopBoth >= 0 {
		reconfigureRelative := strings.Index(source[stopBoth+1:], "foreach ($serviceName in $requiredServices) {\n    Set-RequiredServiceStartDisabled")
		if reconfigureRelative >= 0 {
			reconfigureBoth = stopBoth + 1 + reconfigureRelative
		}
	}
	verifyBoth := strings.LastIndex(source, "Assert-RequiredServiceDisabledAndStopped $serviceName")
	if configureBoth < 0 || stopBoth < 0 || reconfigureBoth < 0 || verifyBoth < 0 ||
		configureBoth > stopBoth || stopBoth > reconfigureBoth || reconfigureBoth > verifyBoth {
		t.Fatalf("service quiescence ordering is wrong: configure=%d stop=%d reconfigure=%d verify=%d", configureBoth, stopBoth, reconfigureBoth, verifyBoth)
	}
}

func TestProvisioningUsesTheRuntimeGuestPolicy(t *testing.T) {
	provision := readBuildFile(t, "packer/scripts/provision.ps1")
	if !strings.Contains(provision, `Join-Path $PSScriptRoot "disable-auto-servicing.ps1"`) ||
		!strings.Contains(provision, "& $guestPolicy") {
		t.Fatal("provisioning and runtime builds do not share one guest policy implementation")
	}
	setup := readBuildFile(t, "setup.sh")
	policyCopy := strings.Index(setup, `win_scp_to "$here/disable-auto-servicing.ps1"`)
	provisionCopy := strings.Index(setup, `win_scp_to "$here/packer/scripts/provision.ps1"`)
	if policyCopy < 0 || provisionCopy < 0 || policyCopy > provisionCopy {
		t.Fatalf("setup does not stage the shared policy beside provision.ps1 first: policy=%d provision=%d", policyCopy, provisionCopy)
	}
	packer := readBuildFile(t, "packer/windows-arm64.pkr.hcl")
	policyUpload := strings.Index(packer, `source      = "${path.root}/../disable-auto-servicing.ps1"`)
	powershellProvisioner := strings.Index(packer, `provisioner "powershell"`)
	if policyUpload < 0 || powershellProvisioner < 0 || policyUpload > powershellProvisioner {
		t.Fatalf("Packer does not upload the shared policy before provision.ps1: policy=%d provisioner=%d", policyUpload, powershellProvisioner)
	}
}

func TestProvisioningModifiesAnExistingVisualStudioInstallation(t *testing.T) {
	provision := readBuildFile(t, "packer/scripts/provision.ps1")
	for _, required := range []string{
		`Microsoft.VisualStudio.Product.BuildTools`,
		`-property installationPath`,
		`$quotedVsInstallPath = '"' + $vsInstallPath + '"'`,
		`@("modify", "--installPath", $quotedVsInstallPath) + $vsCommonArgs`,
	} {
		if !strings.Contains(provision, required) {
			t.Errorf("Visual Studio reprovisioning is missing %q", required)
		}
	}
	installDetection := strings.Index(provision, `-property installationPath`)
	modifyArguments := strings.Index(provision, `@("modify", "--installPath", $quotedVsInstallPath) + $vsCommonArgs`)
	installerStart := strings.Index(provision, `Start-Process -FilePath $vsBootstrap`)
	if installDetection < 0 || modifyArguments < 0 || installerStart < 0 ||
		!(installDetection < modifyArguments && modifyArguments < installerStart) {
		t.Fatalf("existing-install selection must precede the installer: detection=%d modify=%d start=%d", installDetection, modifyArguments, installerStart)
	}
}

func TestProvisioningInstallsAndExportsCMake(t *testing.T) {
	provision := readBuildFile(t, "packer/scripts/provision.ps1")
	for _, required := range []string{
		`"--add", "Microsoft.VisualStudio.Component.VC.CMake.Project"`,
		`-requires Microsoft.VisualStudio.Component.VC.CMake.Project`,
		`-find "Common7\IDE\CommonExtensions\Microsoft\CMake\CMake\bin\cmake.exe"`,
		`if ([string]::IsNullOrWhiteSpace($cmakeExe) -or -not (Test-Path $cmakeExe))`,
		`$cmakeVersion = ((& $cmakeExe --version)`,
		`[Environment]::SetEnvironmentVariable('Path', $machPath, 'Machine')`,
		`$env:PATH = "$cmakeBin;$env:PATH"`,
	} {
		if !strings.Contains(provision, required) {
			t.Errorf("CMake provisioning is missing %q", required)
		}
	}
	component := strings.Index(provision, `"--add", "Microsoft.VisualStudio.Component.VC.CMake.Project"`)
	installer := strings.Index(provision, `Start-Process -FilePath $vsBootstrap`)
	discovery := strings.Index(provision, `$cmakeExe = [string]`)
	pathExport := strings.Index(provision, `$env:PATH = "$cmakeBin;$env:PATH"`)
	if component < 0 || installer < 0 || discovery < 0 || pathExport < 0 ||
		!(component < installer && installer < discovery && discovery < pathExport) {
		t.Fatalf("CMake must be requested before VS runs, then verified before PATH export: component=%d installer=%d discovery=%d path=%d", component, installer, discovery, pathExport)
	}

	smoke := readBuildFile(t, "smoke-test.ps1")
	for _, required := range []string{
		`Get-Command cmake -ErrorAction SilentlyContinue`,
		`& $cmake.Source --version`,
		`Bad 'cmake' 'cmake not on PATH (zxing-cpp source build requires it)'`,
	} {
		if !strings.Contains(smoke, required) {
			t.Errorf("Windows smoke test is missing CMake check %q", required)
		}
	}

	build := readBuildFile(t, "build.sh")
	cmakeCheck := strings.Index(build, `win_assert_guest_cmake`)
	sourceSync := strings.Index(build, `win_sync_source "$BUILD_HOME"`)
	if cmakeCheck < 0 || sourceSync < 0 || cmakeCheck > sourceSync {
		t.Fatalf("standalone builds must reject missing CMake before source sync: check=%d sync=%d", cmakeCheck, sourceSync)
	}
}

func TestProvisioningRetriesTransientGoToolchainRemoval(t *testing.T) {
	provision := readBuildFile(t, "packer/scripts/provision.ps1")
	for _, required := range []string{
		`for ($removeAttempt = 1; $removeAttempt -le 30; $removeAttempt++)`,
		`if ($removeAttempt -eq 30) { throw }`,
		`Start-Sleep -Seconds 2`,
		`if (Test-Path $goRoot) { throw "old Go toolchain still exists after removal retries: $goRoot" }`,
	} {
		if !strings.Contains(provision, required) {
			t.Errorf("Go replacement is missing %q", required)
		}
	}
	versionProbe := strings.Index(provision, `$goInstalled = (& "$goRoot\bin\go.exe" version)`)
	removeRetry := strings.Index(provision, `for ($removeAttempt = 1; $removeAttempt -le 30; $removeAttempt++)`)
	expandArchive := strings.Index(provision, `Expand-Archive -Path $goZip -DestinationPath "C:\" -Force`)
	if versionProbe < 0 || removeRetry < 0 || expandArchive < 0 ||
		!(versionProbe < removeRetry && removeRetry < expandArchive) {
		t.Fatalf("Go replacement retry must be between version detection and extraction: version=%d retry=%d extract=%d", versionProbe, removeRetry, expandArchive)
	}
}

func TestProvisioningUsesResumableAtomicDownloads(t *testing.T) {
	provision := readBuildFile(t, "packer/scripts/provision.ps1")
	for _, required := range []string{
		`function Get-RemoteFile($Uri, $OutFile)`,
		`$partial = "$OutFile.partial"`,
		`"--retry", "12", "--retry-all-errors"`,
		`"--continue-at", "-"`,
		`Move-Item -Force $partial $OutFile`,
		`Get-RemoteFile "https://go.dev/dl/go$goVersion.windows-arm64.zip" $goZip`,
	} {
		if !strings.Contains(provision, required) {
			t.Errorf("resumable provisioning downloads are missing %q", required)
		}
	}
	for _, obsolete := range []string{"Invoke-WebRequest", "DownloadString"} {
		if strings.Contains(provision, obsolete) {
			t.Errorf("provisioning still has a one-shot download via %s", obsolete)
		}
	}
}

func TestWindowsImageRejectsSdkGoVersionDrift(t *testing.T) {
	smoke := readBuildFile(t, "smoke-test.ps1")
	for _, required := range []string{
		`param([Parameter(Mandatory=$true)][string]$ExpectedGoVersion)`,
		`$expectedGo = "go version go$ExpectedGoVersion windows/arm64"`,
		`Bad 'go' "expected '$expectedGo', got '$actualGo'"`,
	} {
		if !strings.Contains(smoke, required) {
			t.Errorf("Windows smoke test is missing %q", required)
		}
	}

	setup := readBuildFile(t, "setup.sh")
	if !strings.Contains(setup, `expected_go_version="$(win_sdk_go_version "$root/sdk/cgo/go.mod")"`) ||
		!strings.Contains(setup, `smoke-test.ps1 -ExpectedGoVersion $expected_go_version`) {
		t.Error("setup does not compare the reusable image with the current SDK Go directive")
	}

	build := readBuildFile(t, "build.sh")
	versionCheck := strings.Index(build, `win_assert_guest_go_version "$expected_go_version"`)
	sourceSync := strings.Index(build, `win_sync_source "$BUILD_HOME"`)
	if versionCheck < 0 || sourceSync < 0 || versionCheck > sourceSync {
		t.Fatalf("standalone builds must reject Go drift before source sync: check=%d sync=%d", versionCheck, sourceSync)
	}
}

func TestSdkGoVersionComesFromTheSelectedModule(t *testing.T) {
	module := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(module, []byte("module example.invalid/sdk\n\ngo 1.26.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", `source "$1"; win_sdk_go_version "$2"`,
		"sdk-go-version-test", filepath.Join(windowsBuildRoot(t), "lib.sh"), module)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("version read failed: %v: %s", err, output)
	}
	if string(output) != "1.26.5\n" {
		t.Fatalf("version = %q, want 1.26.5", output)
	}
}

func TestGuestGoVersionGuardRejectsDrift(t *testing.T) {
	script := `
source "$1"
WIN_HERE=/synthetic/windows
win_ssh() { printf '%s\n' "$FAKE_GUEST_GO_VERSION"; }
win_assert_guest_go_version 1.26.5
`
	for _, test := range []struct {
		version string
		wantOK  bool
	}{
		{version: "go version go1.26.5 windows/arm64", wantOK: true},
		{version: "go version go1.26.4 windows/arm64", wantOK: false},
	} {
		command := exec.Command("bash", "-c", script,
			"guest-go-version-test", filepath.Join(windowsBuildRoot(t), "lib.sh"))
		command.Env = append(os.Environ(), "FAKE_GUEST_GO_VERSION="+test.version)
		err := command.Run()
		if (err == nil) != test.wantOK {
			t.Errorf("version %q: success = %t, want %t", test.version, err == nil, test.wantOK)
		}
	}
}

func TestGuestCMakeGuardRejectsMissingOrInvalidTool(t *testing.T) {
	script := `
source "$1"
WIN_HERE=/synthetic/windows
win_ssh() {
  printf '%s\n' "$FAKE_GUEST_CMAKE_VERSION"
  return "${FAKE_GUEST_CMAKE_RC:-0}"
}
win_assert_guest_cmake
`
	for _, test := range []struct {
		name    string
		version string
		rc      string
		wantOK  bool
	}{
		{name: "usable", version: "cmake version 4.1.1", rc: "0", wantOK: true},
		{name: "missing", version: "", rc: "1", wantOK: false},
		{name: "invalid output", version: "unexpected", rc: "0", wantOK: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("bash", "-c", script,
				"guest-cmake-test", filepath.Join(windowsBuildRoot(t), "lib.sh"))
			command.Env = append(os.Environ(),
				"FAKE_GUEST_CMAKE_VERSION="+test.version,
				"FAKE_GUEST_CMAKE_RC="+test.rc)
			err := command.Run()
			if (err == nil) != test.wantOK {
				t.Errorf("version %q rc %s: success = %t, want %t", test.version, test.rc, err == nil, test.wantOK)
			}
		})
	}
}

func TestGuestPolicyTransportFailuresAreFatal(t *testing.T) {
	root := windowsBuildRoot(t)
	script := `
set -u
source "$1"
WIN_HERE="$2"
event_log="$3"
win_scp_to() {
  printf 'scp\n' >>"$event_log"
  return "${FAKE_SCP_RC:-0}"
}
win_ssh() {
  printf 'ssh\n' >>"$event_log"
  return "${FAKE_SSH_RC:-0}"
}
win_prepare_hermetic_guest
`
	for _, test := range []struct {
		name      string
		scpRC     string
		sshRC     string
		wantRC    int
		wantCalls string
	}{
		{name: "copy failure", scpRC: "17", sshRC: "0", wantRC: 17, wantCalls: "scp\n"},
		{name: "policy failure", scpRC: "0", sshRC: "23", wantRC: 23, wantCalls: "scp\nssh\n"},
		{name: "success", scpRC: "0", sshRC: "0", wantRC: 0, wantCalls: "scp\nssh\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "events")
			command := exec.Command("bash", "-c", script, "guest-policy-test", filepath.Join(root, "lib.sh"), root, logPath)
			command.Env = append(os.Environ(), "FAKE_SCP_RC="+test.scpRC, "FAKE_SSH_RC="+test.sshRC)
			err := command.Run()
			gotRC := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatal(err)
				}
				gotRC = exitError.ExitCode()
			}
			if gotRC != test.wantRC {
				t.Fatalf("exit = %d, want %d", gotRC, test.wantRC)
			}
			calls, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(calls) != test.wantCalls {
				t.Fatalf("calls = %q, want %q", calls, test.wantCalls)
			}
		})
	}
}
