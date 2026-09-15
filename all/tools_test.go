// SPDX-License-Identifier: MPL-2.0
package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute only the production preflight, with synthetic host commands. The
// release entrypoint itself would pull, build, publish, and send notifications.
func TestSDKBuildToolsPreflight(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh required")
	}
	preflight := componentRegion(t, "# Check the complete host toolchain", "\n\nbuilder_message ()")
	for _, test := range []struct {
		name, nuget, cargo, missing, dotnetFailure, want string
		wantProbe                                        bool
	}{
		{"registries skipped", "", "", "", "1", "preflight passed", false},
		{"NuGet ready without dotnet on PATH", "test-token", "", "", "", "preflight passed", true},
		{"NuGet SDK incompatible", "test-token", "", "", "1", "sdk/csharp\" init", true},
		{"required tool absent", "test-token", "", "zig", "", "Missing required build tool(s)", false},
		{"enabled Cargo absent", "", "test-token", "", "", "  - cargo", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "build host with spaces")
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			// Keep an independent inventory: omitting a newly required command from
			// the fixture should fail the successful preflight tests.
			for _, name := range strings.Fields("bash codesign curl docker ffprobe git go gsed hdiutil java jq make mktemp nc node npm npx openssl pandoc pip productbuild productsign python python3 qemu-img qemu-system-aarch64 realpath rsync scp sdkmanager security shasum ssh ssh-keygen tar timeout unzip xcodebuild xcrun zip zig") {
				if name == test.missing {
					continue
				}
				body := `#!/bin/sh
case "${0##*/}" in
  go)
    if [ "$1" = version ]; then
      printf 'go version go1.26.7 darwin/arm64\n'
    else
      printf '%s\n' "$@" >> "$SDK_TOOL_TEST_LOG"
      [ "$SDK_TOOL_TEST_FAILURE" != 1 ] || exit 7
    fi ;;
  java) printf 'openjdk version "21.0.8"\n' ;;
  node) printf 'v24.14.1\n' ;;
  npm) printf '11.11.0\n' ;;
esac
`
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "nvm.sh"), nil, 0644); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "probe")
			cmd := exec.Command(zsh, "-f", "-c", preflight+"\nprintf 'preflight passed\\n'\n")
			// Do not inherit publishing credentials or shell startup files.
			cmd.Env = []string{
				"PATH=" + bin, "WARP_HOME=" + dir, "NVM_DIR=" + dir,
				"NUGET_API_KEY=" + test.nuget, "CARGO_REGISTRY_TOKEN=" + test.cargo,
				"SDK_TOOL_TEST_LOG=" + log, "SDK_TOOL_TEST_FAILURE=" + test.dotnetFailure,
			}
			b, err := cmd.CombinedOutput()
			if !strings.Contains(string(b), test.want) {
				t.Fatalf("want %q, got %s: %v", test.want, b, err)
			}
			if wantSuccess := test.want == "preflight passed"; (err == nil) != wantSuccess {
				t.Fatalf("unexpected preflight exit: %v\n%s", err, b)
			}
			probe, _ := os.ReadFile(log)
			if test.wantProbe {
				want := "-C\n" + filepath.Join(dir, "sdk/packaging") + "\nrun\n.\ncheck-tools\ncsharp\n"
				if string(probe) != want {
					t.Fatalf("incorrect shared tool probe: %q", probe)
				}
			} else if len(probe) != 0 {
				t.Fatalf("unexpected tool probe: %s", probe)
			}
		})
	}
}
