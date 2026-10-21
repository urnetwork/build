// SPDX-License-Identifier: MPL-2.0
package windowsbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflineImageReadinessFailsBeforeAnyGuestOperation(t *testing.T) {
	for _, scenario := range []string{"ready", "missing image", "empty image", "missing receipt", "stale receipt", "missing key", "missing firmware", "missing vars", "SDK newer than pin"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			build := filepath.Join(root, "build with spaces")
			module := filepath.Join(build, "sdk/cgo/go.mod")
			if err := os.MkdirAll(filepath.Dir(module), 0o700); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{
				module:                             "module example.invalid/sdk\n\ngo 1.27.3\n",
				filepath.Join(root, "image.qcow2"): "synthetic base image\n",
				filepath.Join(root, "key"):         "synthetic key\n",
				filepath.Join(root, "firmware"):    "synthetic code\n",
				filepath.Join(root, "vars"):        "synthetic vars\n",
			}
			for path, content := range files {
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			image := filepath.Join(root, "image.qcow2")
			marker := image + ".provision.sha256"
			fingerprint := exec.Command("bash", "-c", `source "$1"; WIN_HERE="$2"; win_provision_contract_fingerprint 1.27.3`, "offline-fixture", filepath.Join(windowsBuildRoot(t), "lib.sh"), windowsBuildRoot(t))
			data, err := fingerprint.Output()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, data, 0o600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing image":
				err = os.Remove(image)
			case "empty image":
				err = os.WriteFile(image, nil, 0o600)
			case "missing receipt":
				err = os.Remove(marker)
			case "stale receipt":
				err = os.WriteFile(marker, []byte(strings.Repeat("0", 64)+"\n"), 0o600)
			case "missing key":
				err = os.Remove(filepath.Join(root, "key"))
			case "missing firmware":
				err = os.Remove(filepath.Join(root, "firmware"))
			case "missing vars":
				err = os.Remove(filepath.Join(root, "vars"))
			case "SDK newer than pin":
				err = os.WriteFile(module, []byte("module example.invalid/sdk\n\ngo 1.27.4\n"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			beforeImage, _ := os.ReadFile(image)
			beforeMarker, _ := os.ReadFile(marker)
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"qemu-system-aarch64", "qemu-img", "ssh", "scp", "ssh-keygen", "go"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf 'guest operation\\n' >> \"$ATTEMPT_LOG\"\nexit 99\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("bash", filepath.Join(windowsBuildRoot(t), "preflight.sh"))
			command.Env = []string{
				"PATH=" + bin + ":/usr/bin:/bin", "BUILD_HOME=" + build,
				"IMAGE=" + image, "SSH_KEY=" + filepath.Join(root, "key"),
				"UEFI_CODE=" + filepath.Join(root, "firmware"), "UEFI_VARS_TEMPLATE=" + filepath.Join(root, "vars"),
				"ATTEMPT_LOG=" + filepath.Join(root, "attempts"),
			}
			output, err := command.CombinedOutput()
			if (err == nil) != (scenario == "ready") {
				t.Fatalf("readiness result: %v\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(root, "attempts")); !os.IsNotExist(err) {
				t.Fatal("offline preflight attempted a guest/toolchain operation")
			}
			afterImage, _ := os.ReadFile(image)
			afterMarker, _ := os.ReadFile(marker)
			if string(beforeImage) != string(afterImage) || string(beforeMarker) != string(afterMarker) {
				t.Fatal("offline preflight changed the base image or provisioning receipt")
			}
		})
	}
}

func TestStandaloneImageReadinessPrecedesVMAndKeepsGuestChecks(t *testing.T) {
	source := readBuildFile(t, "build.sh")
	readiness := strings.Index(source, `win_assert_image_ready "$expected_go_version"`)
	boot := strings.Index(source, "\nwin_boot_vm\n")
	guest := strings.Index(source, `win_assert_guest_go_version "$expected_go_version"`)
	cmake := strings.Index(source, "\nwin_assert_guest_cmake")
	sync := strings.Index(source, `win_sync_source "$BUILD_HOME"`)
	if readiness < 0 || boot < 0 || guest < 0 || cmake < 0 || sync < 0 || !(readiness < boot && boot < guest && guest < cmake && cmake < sync) {
		t.Fatalf("required readiness/guest ordering: %d %d %d %d %d", readiness, boot, guest, cmake, sync)
	}
}
