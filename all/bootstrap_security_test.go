// SPDX-License-Identifier: MPL-2.0
package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapSecurityScansBothArchitecturesAndPropagatesFailures(t *testing.T) {
	for _, scenario := range []string{"ready", "amd64 rejected", "arm64 rejected", "missing amd64", "empty arm64", "scanner failed"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			for _, arch := range []string{"amd64", "arm64"} {
				path := filepath.Join(root, "warp/warpctl/build/linux", arch, "warpctl")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if scenario == "missing "+arch {
					continue
				}
				content := []byte("synthetic binary")
				if scenario == "empty "+arch {
					content = nil
				}
				if err := os.WriteFile(path, content, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(bin, "govulncheck"), `#!/bin/sh
set -eu
[ "$1" = -mode=binary ]
printf '%s\n' "$2" >> "$SCAN_LOG"
case "$SCENARIO:$2" in
  "amd64 rejected:"*/amd64/warpctl|"arm64 rejected:"*/arm64/warpctl) exit 3 ;;
  "scanner failed:"*) exit 7 ;;
esac
`)
			command := exec.Command("bash", filepath.Join(rolloutRoot(t), "check-bootstrap-go.sh"))
			log := filepath.Join(root, "scan-log")
			command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "BUILD_HOME=" + root, "SCENARIO=" + scenario, "SCAN_LOG=" + log}
			output, err := command.CombinedOutput()
			if (err == nil) != (scenario == "ready") {
				t.Fatalf("security result: %v\n%s", err, output)
			}
			data, _ := os.ReadFile(log)
			count := strings.Count(string(data), "\n")
			want := 1
			if scenario == "ready" || scenario == "arm64 rejected" {
				want = 2
			} else if scenario == "missing amd64" {
				want = 0
			}
			if count != want {
				t.Fatalf("scans = %d, want %d: %s", count, want, data)
			}
		})
	}
}

func TestEarlyReadinessGatesPrecedeCandidatesAndPublication(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, ordered := range [][]string{
		{"error_trap 'pull mmm'", "error_trap 'Windows image readiness before preparation'", "if [ \"$CONNECT_IP_UPDATE\" ]; then", "error_trap 'build bootstrap warpctl'", "timeout --kill-after=10 300 bash \"$BUILD_HOME/all/check-bootstrap-go.sh\"", "error_trap 'bootstrap Go binary security checks'", "builder_message \"checking web builds after localization updates\"", "warpctl stage version next"},
		{"error_trap 'validate immutable release before resume'", "error_trap 'Windows image readiness before resume'", "# Per-run apple signing keychain"},
	} {
		previous := -1
		for _, required := range ordered {
			position := strings.Index(source, required)
			if position < 0 || position <= previous {
				t.Fatalf("readiness ordering broken at %q: previous=%d current=%d", required, previous, position)
			}
			previous = position
		}
	}
}
