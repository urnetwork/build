// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// hdiutil's automatic APFS estimate left only 34 MiB free for a 240 MiB
// release app, and its atomic copy intermittently failed with ENOSPC. Size
// explicitly from both allocated and apparent payload sizes, with room for
// copy-on-write work and filesystem metadata (including small APFS images).
func TestMacosDmgSizeIncludesCopyAndFilesystemReserve(t *testing.T) {
	helper := componentFunctions(t, "macos_dmg_size_mb")
	if helper == "" {
		t.Fatal("missing production macos_dmg_size_mb helper")
	}
	for _, test := range []struct {
		name, allocated, apparent, want string
	}{
		{"empty", "0", "0", "256"},
		{"small", "12", "8", "256"},
		{"release", "246068", "245705", "609"},
		{"apparent larger", "1024", "524288", "1152"},
		{"allocated larger", "262144", "1024", "640"},
		{"round up", "131073", "131073", "385"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("zsh", "-f", "-c", helper+`
fixture_allocated="$1"
fixture_apparent="$2"
du() {
    case "$1" in
        -sk) printf '%s\t%s\n' "$fixture_allocated" "$2" ;;
        -Ask) printf '%s\t%s\n' "$fixture_apparent" "$2" ;;
        *) return 41 ;;
    esac
}
macos_dmg_size_mb "source with spaces"
`, "dmg-size-test", test.allocated, test.apparent)
			output, err := command.CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != test.want {
				t.Fatalf("size = %q, %v; want %s MiB", output, err, test.want)
			}
		})
	}
}

func TestMacosDmgSizeRejectsUnmeasuredPayload(t *testing.T) {
	helper := componentFunctions(t, "macos_dmg_size_mb")
	if helper == "" {
		t.Fatal("missing production macos_dmg_size_mb helper")
	}
	for _, test := range []struct {
		name, du string
		code     int
	}{
		{"allocated read error", `du() { return 37; }`, 37},
		{"apparent read error", `du() { if [[ "$1" == -Ask ]]; then return 38; fi; printf '42\tsource\n'; }`, 38},
		{"missing size", `du() { printf '\tsource\n'; }`, 1},
		{"invalid size", `du() { printf 'not-a-size\tsource\n'; }`, 1},
		{"negative size", `du() { printf '%s\tsource\n' -1; }`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("zsh", "-f", "-c", helper+"\nbuilder_message() { print -r -- \"$*\"; }\n"+test.du+"\nmacos_dmg_size_mb source\n")
			output, err := command.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != test.code {
				t.Fatalf("unmeasured payload returned %v, output %q; want exit %d", err, output, test.code)
			}
		})
	}
}

func TestRunMacosDirectDmgSizingFailureIsFatal(t *testing.T) {
	result := runMacosDirect(t, "du() { return 37; }\n")
	if result.exitCode != 37 || strings.Contains(result.events, "dmg\n") || strings.Contains(result.events, "release-continued") || stepCount(result.events, "publish") != 1 {
		t.Fatalf("DMG sizing failure was masked or reached DMG creation/publication: %+v", result)
	}
}
