// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func androidSetupObjcopyCheck(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "android", "setup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	const start = "\nndk_objcopy="
	const end = "\necho \">>> provisioning pinned Android SDK build tools\""
	if strings.Count(source, start) != 1 || strings.Count(source, end) != 1 {
		t.Fatal("cannot locate unique production NDK objcopy check")
	}
	begin, finish := strings.Index(source, start)+1, strings.Index(source, end)
	if begin >= finish {
		t.Fatal("NDK objcopy check must precede SDK tool provisioning")
	}
	return source[begin:finish]
}

func TestAndroidSetupObjcopyExecutablePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Android setup requires a POSIX host")
	}
	check := androidSetupObjcopyCheck(t)
	for _, fixture := range []struct {
		name string
		mode os.FileMode
		kind string
		want bool
	}{
		{"owner executable 0744", 0o744, "file", true},
		{"private executable 0700", 0o700, "file", true},
		{"shared executable 0755", 0o755, "file", true},
		{"readable only 0644", 0o644, "file", false},
		{"private nonexecutable 0600", 0o600, "file", false},
		{"group and other only 0655", 0o655, "file", false},
		{"other only 0645", 0o645, "file", false},
		{"directory", 0o755, "directory", false},
		{"symlink", 0o755, "symlink", false},
		{"missing tool", 0, "missing", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			sdk := filepath.Join(t.TempDir(), "Android SDK with spaces")
			const version = "91.2.3"
			bin := filepath.Join(sdk, "ndk", version, "toolchains", "llvm", "prebuilt", "host", "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			tool := filepath.Join(bin, "llvm-objcopy")
			switch fixture.kind {
			case "file":
				if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				// Set the exact fixture mode independently of the campaign's umask.
				if err := os.Chmod(tool, fixture.mode); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(tool, fixture.mode); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(bin, "other-tool")
				if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), fixture.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, tool); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Execute only the real lookup and gate, never SDK installation or AVD
			// setup. Use BSD find on macOS and the system find on Linux.
			body := "set -euo pipefail\nsdk_root=$1\nndk_version=$2\n" + check + "\nprintf '%s\\n' \"$ndk_objcopy\"\n"
			cmd := exec.CommandContext(ctx, "bash", "-c", body, "android-objcopy-test", sdk, version)
			cmd.Env = environmentWith("PATH=/usr/bin:/bin", "BASH_ENV=", "ENV=")
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("NDK lookup exceeded deadline: %v", ctx.Err())
			}
			if fixture.want {
				if err != nil || strings.TrimSpace(string(output)) != tool {
					t.Fatalf("executable NDK tool rejected: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), "has no llvm-objcopy") {
				t.Fatalf("invalid NDK tool not rejected: %v\n%s", err, output)
			}
		})
	}
}
