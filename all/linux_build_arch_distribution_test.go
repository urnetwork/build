// SPDX-License-Identifier: MPL-2.0

// The distribution build-arch.sh stamps into the linux binaries (-Ddistribution,
// linux app/meson_options.txt): the binaries report it in their client info,
// and the server reads it for the account's onboarding platform. Run as
// linux_build_arch_test.go runs the script, with a fake meson that logs its
// arguments.

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The linux checkout's option line, as app/meson_options.txt declares it.
const buildArchDistributionOption = "option('distribution', type : 'combo', choices : ['linux', 'linux_appimage', 'linux_flatpak'], value : 'linux',\n" +
	"       description : 'Distribution stamped into both binaries (UR_DISTRIBUTION): linux, linux_appimage or linux_flatpak')\n"

// Runs build-arch.sh for one role against a scratch root whose linux checkout
// declares the distribution option or not, and returns the meson setup line.
func buildArchMesonSetup(t *testing.T, role string, declaresDistribution bool) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the build-arch.sh test")
	}
	script := filepath.Join(filepath.Dir(currentFile), "linux", "build-arch.sh")

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	appDir := filepath.Join(root, "src", "app")
	packagingDir := filepath.Join(root, "src", "packaging")
	sdkDir := filepath.Join(appDir, "third_party", "urnetwork-sdk", "arm64")
	outDir := filepath.Join(root, "out")
	for _, dir := range []string{binDir, packagingDir, sdkDir, outDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sdkDir, "libURnetworkSdk.so"), []byte("sdk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := "option('app_version', type : 'string', value : '0.0.0')\n"
	if declaresDistribution {
		options += buildArchDistributionOption
	}
	if err := os.WriteFile(filepath.Join(appDir, "meson_options.txt"), []byte(options), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "meson"), fakeBuildArchMeson)
	packagers := map[string]string{
		"make-deb.sh":             "urnetwork-daemon_${VERSION}_${ARCH}.deb",
		"make-install-tarball.sh": "urnetwork-daemon-${VERSION}-${ARCH}.install.tar.gz",
		"make-rpm.sh":             "urnetwork-daemon-${VERSION}.aarch64.rpm",
		"make-arch.sh":            "urnetwork-daemon-${VERSION}-aarch64.pkg.tar.zst",
		"make-appimage.sh":        "URnetwork-${VERSION}-${ARCH}.AppImage",
	}
	for packager, name := range packagers {
		writeExecutable(t, filepath.Join(packagingDir, packager), strings.ReplaceAll(fakeBuildArchContractPackager, "ARTIFACT_NAME", name))
	}

	logPath := filepath.Join(root, "meson.log")
	cmd := exec.Command("bash", script)
	cmd.Env = environmentWith(
		"ARCH=arm64",
		"VERSION=0.0.0-1",
		"ROLE="+role,
		"UR_CONTAINER_ROOT="+root,
		"UR_SKIP_VERIFY=1",
		"UR_REQUIRE_RPM=",
		"UR_REQUIRE_ARCH_PKG=",
		"MESON_LOG="+logPath,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s build: %v\n%s", role, err, output)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "setup ") {
			return line
		}
	}
	t.Fatalf("%s build ran no meson setup: %s", role, raw)
	return ""
}

// The GUI image's urnetwork-gui ships only in the AppImage, so it reports
// linux_appimage; the daemon image's one build goes into four packages, so it
// reports linux, the package not known.
func TestBuildArchStampsEachRolesDistribution(t *testing.T) {
	for _, c := range []struct {
		role string
		want string
	}{
		{role: "gui", want: "-Ddistribution=linux_appimage"},
		{role: "daemon", want: "-Ddistribution=linux"},
	} {
		setup := buildArchMesonSetup(t, c.role, true)
		if !strings.Contains(" "+setup+" ", " "+c.want+" ") || strings.Count(setup, "-Ddistribution=") != 1 {
			t.Errorf("%s: meson %s, want %s", c.role, setup, c.want)
		}
	}
}

// A checkout from before the option builds as it did: its meson would refuse
// an unknown -D, which would cost the release its Linux artifacts.
func TestBuildArchPassesNoDistributionToAnOlderCheckout(t *testing.T) {
	for _, role := range []string{"gui", "daemon"} {
		if setup := buildArchMesonSetup(t, role, false); strings.Contains(setup, "-Ddistribution") {
			t.Errorf("%s: meson %s names a distribution the checkout does not declare", role, setup)
		}
	}
}
