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
