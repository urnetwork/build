// SPDX-License-Identifier: MPL-2.0

// ur's Windows zips (REPO-M1-03): build-ur-windows.sh with build/all/windows's VM helpers stubbed over
// a directory that plays the guest, where "signing" appends a signature to each ur.exe. The real
// signature needs the VM's certificate (EXT-2), and sign-ur.ps1 runs only there.

package allbuild

import (
	"archive/zip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The VM's helpers over $UR_STUB_GUEST: the thumbprint check fails without UR_STUB_THUMBPRINT, and
// sign-ur.ps1 appends a signature unless UR_STUB_SIGN says not to.
const urStubWindowsLib = `
win_init() { :; }
win_boot_vm() { printf 'boot\n' >> "$UR_STUB_EVENTS"; }
win_wait_ssh() { return 0; }
win_shutdown_vm() { printf 'shutdown\n' >> "$UR_STUB_EVENTS"; }
ur_stub_guest_path() { printf '%s/%s' "$UR_STUB_GUEST" "${1#C:/}"; }
win_scp_to() { mkdir -p "$(dirname "$(ur_stub_guest_path "$2")")"; cp "$1" "$(ur_stub_guest_path "$2")"; }
win_scp_from() { cp "$(ur_stub_guest_path "$1")" "$2"; }
win_ssh() {
    case "$*" in
        *URN_SIGN_THUMBPRINT*)
            [ -n "${UR_STUB_THUMBPRINT:-}" ] || return 3
            mkdir -p "$UR_STUB_GUEST/Windows/Temp/ur-sign/amd64" "$UR_STUB_GUEST/Windows/Temp/ur-sign/arm64"
            printf 'thumbprint\n' >> "$UR_STUB_EVENTS"
            ;;
        *sign-ur.ps1*)
            printf 'sign\n' >> "$UR_STUB_EVENTS"
            [ "${UR_STUB_SIGN:-}" = fail ] && return 1
            for arch in amd64 arm64; do
                [ "${UR_STUB_SIGN:-}" = none ] || printf 'SIGNATURE' >> "$UR_STUB_GUEST/Windows/Temp/ur-sign/$arch/ur.exe"
            done
            ;;
        *) printf 'unexpected win_ssh %s\n' "$*" >&2; return 41 ;;
    esac
}
`

type urWindowsHarness struct {
	t         *testing.T
	directory string
	events    string
	out       string
}

// A copy of build-ur-windows.sh beside the stubbed VM helpers, with ur/lib.sh and the zip's documents.
func newUrWindowsHarness(t *testing.T) *urWindowsHarness {
	t.Helper()
	directory := t.TempDir()
	self := &urWindowsHarness{t: t, directory: directory, events: filepath.Join(directory, "events"), out: filepath.Join(directory, "out")}
	root := rolloutRoot(t)
	all := filepath.Join(directory, "home", "all")
	copyTestFile(t, filepath.Join(root, "build-ur-windows.sh"), filepath.Join(all, "build-ur-windows.sh"), 0o700)
	copyTestFile(t, filepath.Join(root, "ur", "lib.sh"), filepath.Join(all, "ur", "lib.sh"), 0o600)
	copyTestFile(t, filepath.Join(root, "ur", "windows", "sign-ur.ps1"), filepath.Join(all, "ur", "windows", "sign-ur.ps1"), 0o600)
	copyTestFile(t, filepath.Join(root, "ur", "windows", "README.txt"), filepath.Join(all, "ur", "windows", "README.txt"), 0o600)
	copyTestContent(t, filepath.Join(all, "windows", "lib.sh"), urStubWindowsLib)
	copyTestContent(t, filepath.Join(directory, "home", "sidecar", "LICENSE"), "license\n")
	copyTestContent(t, filepath.Join(directory, "home", "sidecar", "NOTICE"), "notice\n")
	for _, arch := range []string{"amd64", "arm64"} {
		copyTestContent(t, filepath.Join(directory, "bin", "windows", arch, "ur.exe"), "MZ ur "+arch+"\n")
	}
	if err := os.MkdirAll(self.out, 0o700); err != nil {
		t.Fatal(err)
	}
	return self
}

func (self *urWindowsHarness) run(overrides ...string) componentResult {
	self.t.Helper()
	command := exec.Command("bash", filepath.Join(self.directory, "home", "all", "build-ur-windows.sh"))
	command.Env = environmentWith(append([]string{
		"UR_STUB_EVENTS=" + self.events,
		"UR_STUB_GUEST=" + filepath.Join(self.directory, "guest"),
		"UR_STUB_THUMBPRINT=0123456789ABCDEF0123456789ABCDEF01234567",
		"UR_STUB_SIGN=",
		"UR_BIN_DIR=" + filepath.Join(self.directory, "bin"),
		"OUT_DIR=" + self.out,
		"BUILD_HOME=" + filepath.Join(self.directory, "home"),
		"EXTERNAL_WARP_VERSION=" + urTestVersion,
		"GO_MOD_VERSION=2026",
		"SOURCE_DATE_EPOCH=1762646400",
		"URARCHIVE=" + buildTestUrarchive(self.t),
	}, overrides...)...)
	output, err := command.CombinedOutput()
	result := componentResult{output: string(output)}
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			self.t.Fatal(err)
		}
		result.exitCode = exitError.ExitCode()
	}
	events, err := os.ReadFile(self.events)
	if err != nil && !os.IsNotExist(err) {
		self.t.Fatal(err)
	}
	result.events = string(events)
	return result
}

func (self *urWindowsHarness) zipContents(arch string) map[string]string {
	self.t.Helper()
	reader, err := zip.OpenReader(filepath.Join(self.out, "ur-"+urTestVersion+"-windows-"+arch+".zip"))
	if err != nil {
		self.t.Fatal(err)
	}
	defer reader.Close()
	contents := map[string]string{}
	for _, file := range reader.File {
		opened, err := file.Open()
		if err != nil {
			self.t.Fatal(err)
		}
		data, err := io.ReadAll(opened)
		opened.Close()
		if err != nil {
			self.t.Fatal(err)
		}
		contents[file.Name] = string(data)
	}
	return contents
}

// Signing is the gate (owner decision 77): with no thumbprint on the VM nothing is zipped.
func TestWindowsStepFailsWithoutAThumbprint(t *testing.T) {
	harness := newUrWindowsHarness(t)
	result := harness.run("UR_STUB_THUMBPRINT=")
	if result.exitCode == 0 || strings.Contains(result.events, "sign\n") || !strings.Contains(result.output, "no URN_SIGN_THUMBPRINT") {
		t.Fatalf("the Windows step went on without a thumbprint: %+v", result)
	}
	if entries, _ := os.ReadDir(harness.out); len(entries) != 0 {
		t.Errorf("a refused step wrote %d files", len(entries))
	}
	if !strings.Contains(result.events, "shutdown\n") {
		t.Errorf("the VM was left running")
	}
	script, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur", "windows", "sign-ur.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "if (-not $Thumbprint) {\n  throw ") || !strings.Contains(string(script), "signtool verify /pa /all $exe") {
		t.Errorf("sign-ur.ps1 does not refuse an empty thumbprint and verify what it signs")
	}
}

// Both exes are the signed bytes: git-remote-ur.exe is copied from ur.exe after signing.
func TestZipHoldsBothExesByteEqual(t *testing.T) {
	harness := newUrWindowsHarness(t)
	if result := harness.run(); result.exitCode != 0 {
		t.Fatalf("build-ur-windows.sh failed: %+v", result)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		contents := harness.zipContents(arch)
		if contents["ur.exe"] != "MZ ur "+arch+"\nSIGNATURE" {
			t.Errorf("%s's ur.exe is %q, not the signed one", arch, contents["ur.exe"])
		}
		if contents["git-remote-ur.exe"] != contents["ur.exe"] {
			t.Errorf("%s's git-remote-ur.exe differs from ur.exe", arch)
		}
	}
}

// No wintun.dll until plan-BRIDGE-windows-tun-driver's checked loader takes one (reading 12).
func TestZipHoldsNoWintun(t *testing.T) {
	harness := newUrWindowsHarness(t)
	if result := harness.run(); result.exitCode != 0 {
		t.Fatalf("build-ur-windows.sh failed: %+v", result)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		names := slices.Sorted(func(yield func(string) bool) {
			for name := range harness.zipContents(arch) {
				if !yield(name) {
					return
				}
			}
		})
		if want := []string{"LICENSE", "NOTICE", "README.txt", "git-remote-ur.exe", "ur.exe"}; !slices.Equal(names, want) {
			t.Errorf("%s's zip holds %v, want %v", arch, names, want)
		}
		for _, name := range names {
			if strings.HasSuffix(strings.ToLower(name), ".dll") {
				t.Errorf("%s's zip holds %s", arch, name)
			}
		}
	}
	readme, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur", "windows", "README.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), `%LOCALAPPDATA%\Programs\URnetwork\`) || !strings.Contains(string(readme), "PATH") {
		t.Errorf("README.txt does not name the folder and the PATH step")
	}
}

func TestWindowsStepRefusesAnUnsignedExe(t *testing.T) {
	for _, mode := range []string{"none", "fail"} {
		harness := newUrWindowsHarness(t)
		result := harness.run("UR_STUB_SIGN=" + mode)
		if result.exitCode == 0 {
			t.Fatalf("an exe the VM did not sign (%s) was zipped: %+v", mode, result)
		}
		if entries, _ := os.ReadDir(harness.out); len(entries) != 0 {
			t.Errorf("%s: a refused step wrote %d files", mode, len(entries))
		}
	}
}
