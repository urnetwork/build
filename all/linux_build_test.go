// SPDX-License-Identifier: MPL-2.0

// all/linux/build.sh runs one builder container per arch and role and then
// checks the artifacts on the host. These tests run it with a fake docker that
// records the environment each container would get and writes the artifacts
// of its role into the directory mounted at /out, and a fake fetch-deps.sh.
// Nothing here needs Docker.

package allbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Stands in for docker. build succeeds. run appends to FAKE_DOCKER_LOG each
// variable the container would get, resolving a bare -e NAME from this
// process's environment as docker does, and writes the artifacts of the
// container's role into the directory mounted at /out.
const fakeLinuxBuildDocker = `#!/bin/bash
set -euo pipefail
case "$1" in
  build) exit 0 ;;
  run) shift ;;
  *) echo "fake docker: unexpected command $1" >&2; exit 2 ;;
esac
out='' arch='' role='' version=''
while [ $# -gt 0 ]; do
  case "$1" in
    --rm) shift ;;
    --platform) shift 2 ;;
    -v)
      case "$2" in *:/out) out="${2%:/out}" ;; esac
      shift 2
      ;;
    -e)
      entry=''
      case "$2" in
        *=*) entry="$2" ;;
        *) if value="$(printenv "$2")"; then entry="$2=${value}"; fi ;;
      esac
      if [ -n "${entry}" ]; then
        printf '%s\n' "${entry}" >>"${FAKE_DOCKER_LOG}"
        case "${entry}" in
          ARCH=*) arch="${entry#ARCH=}" ;;
          ROLE=*) role="${entry#ROLE=}" ;;
          VERSION=*) version="${entry#VERSION=}" ;;
        esac
      fi
      shift 2
      ;;
    *) break ;;
  esac
done
printf 'run %s %s\n' "${role}" "${arch}" >>"${FAKE_DOCKER_LOG}"
case "${arch}" in
  amd64) package_arch=x86_64 ;;
  arm64) package_arch=aarch64 ;;
esac
if [ "${role}" = daemon ]; then
  for name in \
    "urnetwork-daemon_${version}_${arch}.deb" \
    "urnetwork-daemon-${version}-${arch}.install.tar.gz" \
    "urnetwork-daemon-${version}.${package_arch}.rpm" \
    "urnetwork-daemon-${version}-${package_arch}.pkg.tar.zst"
  do
    printf 'package\n' >"${out}/${name}"
  done
else
  printf 'appimage\n' >"${out}/URnetwork-${version}-${arch}.AppImage"
fi
`

// One build.sh run with the fake docker.
type linuxBuildRun struct {
	containerLogLines []string
	output            string
	exitCode          int
}

// Runs build.sh for arm64 and the given ROLES; overrides are extra
// environment entries, such as the UR_REQUIRE_* knobs.
func runLinuxBuildFixture(t *testing.T, roles string, overrides ...string) linuxBuildRun {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the build.sh test")
	}
	script := filepath.Join(filepath.Dir(currentFile), "linux", "build.sh")

	buildHome := t.TempDir()
	binDir := filepath.Join(buildHome, "bin")
	scriptsDir := filepath.Join(buildHome, "linux", "app", "scripts")
	for _, dir := range []string{binDir, scriptsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(binDir, "docker"), fakeLinuxBuildDocker)
	writeExecutable(t, filepath.Join(scriptsDir, "fetch-deps.sh"), "#!/bin/sh\nexit 0\n")
	dockerLog := filepath.Join(buildHome, "docker.log")

	env := []string{
		"BUILD_HOME=" + buildHome,
		"LINUX_DIR=" + filepath.Join(buildHome, "linux"),
		"SDK_ZIP=" + filepath.Join(buildHome, "URnetworkSdkLinux.zip"),
		"OUT_DIR=" + filepath.Join(buildHome, "out"),
		"VERSION=0.0.0-1",
		"ARCHES=arm64",
		"ROLES=" + roles,
		"UR_REQUIRE_RPM=",
		"UR_REQUIRE_ARCH_PKG=",
		"FAKE_DOCKER_LOG=" + dockerLog,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	for _, override := range overrides {
		key, _, _ := strings.Cut(override, "=")
		env = slices.DeleteFunc(env, func(entry string) bool {
			return strings.HasPrefix(entry, key+"=")
		})
		env = append(env, override)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = environmentWith(env...)
	output, err := cmd.CombinedOutput()
	run := linuxBuildRun{
		output: string(output),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run build.sh: %v", err)
	}
	if data, readErr := os.ReadFile(dockerLog); readErr == nil {
		run.containerLogLines = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return run
}

// The daemon container decides whether a failed .rpm or Arch package is fatal,
// so both knobs have to reach it. run.sh sets both; before, UR_REQUIRE_ARCH_PKG
// stopped at the host and build-arch.sh tolerated a failed make-arch.sh.
func TestLinuxBuildForwardsRequireKnobsToTheDaemonContainer(t *testing.T) {
	run := runLinuxBuildFixture(t, "daemon", "UR_REQUIRE_RPM=true", "UR_REQUIRE_ARCH_PKG=true")
	if run.exitCode != 0 {
		t.Fatalf("exit %d, want 0; output:\n%s", run.exitCode, run.output)
	}
	if !slices.Contains(run.containerLogLines, "run daemon arm64") {
		t.Fatalf("no daemon container ran; container log %v", run.containerLogLines)
	}
	for _, knob := range []string{"UR_REQUIRE_RPM=true", "UR_REQUIRE_ARCH_PKG=true"} {
		if !slices.Contains(run.containerLogLines, knob) {
			t.Errorf("the daemon container did not get %s; it got %v", knob, run.containerLogLines)
		}
	}
}

// A GUI-only build produces no Arch package, as it produces no .rpm, so the
// host check neither fails nor warns about one, whatever the knobs say.
func TestLinuxBuildGuiRoleDoesNotExpectTheArchPackage(t *testing.T) {
	cases := []struct {
		requireArchPackage string
	}{
		{requireArchPackage: "UR_REQUIRE_ARCH_PKG=true"},
		{requireArchPackage: "UR_REQUIRE_ARCH_PKG="},
	}
	for _, c := range cases {
		run := runLinuxBuildFixture(t, "gui", "UR_REQUIRE_RPM=true", c.requireArchPackage)
		if run.exitCode != 0 {
			t.Errorf("%s: exit %d, want 0; output:\n%s", c.requireArchPackage, run.exitCode, run.output)
			continue
		}
		if strings.Contains(run.output, "Arch package") || strings.Contains(run.output, ".pkg.tar.zst") {
			t.Errorf("%s: a gui-only build reported the Arch package; output:\n%s", c.requireArchPackage, run.output)
		}
	}
}
