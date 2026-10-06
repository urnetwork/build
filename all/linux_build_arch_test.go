// SPDX-License-Identifier: MPL-2.0

// all/linux/build-arch.sh runs inside the linux builder containers. These
// tests run it outside one: UR_CONTAINER_ROOT stands a scratch tree in for the
// container's /, a fake meson stages an install tree, and fake packaging
// scripts write the artifact names the linux repo's scripts write. They pin
// what a failed .rpm or Arch package leaves in /out, where build.sh, run.sh
// and linux-build.yml pick up every *.rpm and *.pkg.tar.zst. Nothing here
// needs Docker, meson or nfpm.

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

// Stands in for meson: setup, compile and test succeed, and install stages
// the files build-arch.sh looks for in the install tree.
const fakeBuildArchMeson = `#!/bin/sh
set -eu
if [ "$1" = install ]; then
  mkdir -p "$DESTDIR/usr/lib/urnetwork" "$DESTDIR/usr/share/locale/de/LC_MESSAGES"
  printf 'sdk\n' >"$DESTDIR/usr/lib/urnetwork/libURnetworkSdk.so"
  printf 'catalog\n' >"$DESTDIR/usr/share/locale/de/LC_MESSAGES/urnetwork.mo"
fi
`

// Stands in for make-deb.sh and make-install-tarball.sh, which always
// succeed here. ARTIFACT_NAME is replaced with the script's artifact name.
const fakeBuildArchContractPackager = `#!/bin/bash
set -euo pipefail
name="ARTIFACT_NAME"
printf 'package\n' >"${OUT_DIR}/${name}"
printf 'sum  %s\n' "${name}" >"${OUT_DIR}/${name}.sha256"
`

// Stands in for make-rpm.sh and make-arch.sh. MODE_VARIABLE picks what it
// does: ok writes the package and its checksum; fail-after-write writes both
// and then fails, as the real scripts do when their payload check fails after
// nfpm wrote the package; wrong-name exits 0 having written the package under
// another name. ARTIFACT_NAME and OTHER_NAME are replaced per script.
const fakeBuildArchOptionalPackager = `#!/bin/bash
set -euo pipefail
case "${ARCH}" in
  amd64) package_arch=x86_64 ;;
  arm64) package_arch=aarch64 ;;
esac
name="ARTIFACT_NAME"
case "${MODE_VARIABLE}" in
  ok)
    printf 'package\n' >"${OUT_DIR}/${name}"
    printf 'sum  %s\n' "${name}" >"${OUT_DIR}/${name}.sha256"
    ;;
  fail-after-write)
    printf 'package\n' >"${OUT_DIR}/${name}"
    printf 'sum  %s\n' "${name}" >"${OUT_DIR}/${name}.sha256"
    echo "error: payload verification failed" >&2
    exit 1
    ;;
  wrong-name)
    printf 'package\n' >"${OUT_DIR}/OTHER_NAME"
    ;;
  *)
    echo "unknown MODE_VARIABLE '${MODE_VARIABLE}'" >&2
    exit 2
    ;;
esac
`

// One build-arch.sh run against a scratch root.
type buildArchRun struct {
	outDir   string
	output   string
	exitCode int
}

// Runs build-arch.sh for the arm64 daemon role with fake packaging scripts.
// rpmMode and archMode are the fake make-rpm.sh and make-arch.sh modes;
// overrides are extra environment entries, such as the UR_REQUIRE_* knobs.
func runBuildArchFixture(t *testing.T, rpmMode string, archMode string, overrides ...string) buildArchRun {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the build-arch.sh test")
	}
	script := filepath.Join(filepath.Dir(currentFile), "linux", "build-arch.sh")

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	packagingDir := filepath.Join(root, "src", "packaging")
	sdkDir := filepath.Join(root, "src", "app", "third_party", "urnetwork-sdk", "arm64")
	outDir := filepath.Join(root, "out")
	for _, dir := range []string{binDir, packagingDir, sdkDir, outDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sdkDir, "libURnetworkSdk.so"), []byte("sdk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "meson"), fakeBuildArchMeson)
	for _, packager := range []struct {
		script string
		name   string
	}{
		{script: "make-deb.sh", name: "urnetwork-daemon_${VERSION}_${ARCH}.deb"},
		{script: "make-install-tarball.sh", name: "urnetwork-daemon-${VERSION}-${ARCH}.install.tar.gz"},
	} {
		writeExecutable(
			t,
			filepath.Join(packagingDir, packager.script),
			strings.ReplaceAll(fakeBuildArchContractPackager, "ARTIFACT_NAME", packager.name),
		)
	}
	for _, packager := range []struct {
		script       string
		modeVariable string
		name         string
		otherName    string
	}{
		{
			script:       "make-rpm.sh",
			modeVariable: "FAKE_RPM_MODE",
			name:         "urnetwork-daemon-${VERSION}.${package_arch}.rpm",
			otherName:    "urnetwork-daemon-0.0.0-0.1.${package_arch}.rpm",
		},
		{
			script:       "make-arch.sh",
			modeVariable: "FAKE_ARCH_MODE",
			name:         "urnetwork-daemon-${VERSION}-${package_arch}.pkg.tar.zst",
			otherName:    "urnetwork-daemon-0.0.0.1-1-${package_arch}.pkg.tar.zst",
		},
	} {
		source := strings.NewReplacer(
			"MODE_VARIABLE", packager.modeVariable,
			"ARTIFACT_NAME", packager.name,
			"OTHER_NAME", packager.otherName,
		).Replace(fakeBuildArchOptionalPackager)
		writeExecutable(t, filepath.Join(packagingDir, packager.script), source)
	}

	env := []string{
		"ARCH=arm64",
		"VERSION=0.0.0-1",
		"ROLE=daemon",
		"UR_CONTAINER_ROOT=" + root,
		"UR_SKIP_VERIFY=1",
		"UR_REQUIRE_RPM=",
		"UR_REQUIRE_ARCH_PKG=",
		"FAKE_RPM_MODE=" + rpmMode,
		"FAKE_ARCH_MODE=" + archMode,
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
	run := buildArchRun{
		outDir: outDir,
		output: string(output),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run build-arch.sh: %v", err)
	}
	return run
}

// Returns what an upload glob over /out would pick up for pattern.
func (self buildArchRun) uploads(t *testing.T, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(self.outDir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}
	return names
}

// The release names of the arm64 daemon artifacts in the fixture.
const (
	buildArchDebName     = "urnetwork-daemon_0.0.0-1_arm64.deb"
	buildArchTarballName = "urnetwork-daemon-0.0.0-1-arm64.install.tar.gz"
	buildArchRpmName     = "urnetwork-daemon-0.0.0-1.aarch64.rpm"
	buildArchPackageName = "urnetwork-daemon-0.0.0-1-aarch64.pkg.tar.zst"
)

// A make-rpm.sh or make-arch.sh that writes its package and then fails, or
// exits 0 having written it under another name, is tolerated by default. The
// build carries on with the contracted .deb and tarball, and nothing of the
// failed package is left in /out for an upload glob to find.
func TestBuildArchContinuesWithoutAFailedOptionalPackage(t *testing.T) {
	cases := []struct {
		rpmMode         string
		archMode        string
		failedPattern   string
		problem         string
		keptPattern     string
		keptName        string
		continueMessage string
	}{
		{
			rpmMode:         "fail-after-write",
			archMode:        "ok",
			failedPattern:   "*.rpm*",
			problem:         "make-rpm.sh failed",
			keptPattern:     "*.pkg.tar.zst",
			keptName:        buildArchPackageName,
			continueMessage: "continuing without the .rpm",
		},
		{
			rpmMode:         "ok",
			archMode:        "fail-after-write",
			failedPattern:   "*.pkg.tar.zst*",
			problem:         "make-arch.sh failed",
			keptPattern:     "*.rpm",
			keptName:        buildArchRpmName,
			continueMessage: "continuing without it",
		},
		{
			rpmMode:         "wrong-name",
			archMode:        "ok",
			failedPattern:   "*.rpm*",
			problem:         "make-rpm.sh reported success but wrote no " + buildArchRpmName,
			keptPattern:     "*.pkg.tar.zst",
			keptName:        buildArchPackageName,
			continueMessage: "continuing without the .rpm",
		},
		{
			rpmMode:         "ok",
			archMode:        "wrong-name",
			failedPattern:   "*.pkg.tar.zst*",
			problem:         "make-arch.sh reported success but wrote no " + buildArchPackageName,
			keptPattern:     "*.rpm",
			keptName:        buildArchRpmName,
			continueMessage: "continuing without it",
		},
	}
	for _, c := range cases {
		run := runBuildArchFixture(t, c.rpmMode, c.archMode)
		if run.exitCode != 0 {
			t.Errorf("rpm %s, arch %s: exit %d, want 0; output:\n%s", c.rpmMode, c.archMode, run.exitCode, run.output)
			continue
		}
		if !strings.Contains(run.output, c.problem) || !strings.Contains(run.output, c.continueMessage) {
			t.Errorf("rpm %s, arch %s: no %q warning; output:\n%s", c.rpmMode, c.archMode, c.problem, run.output)
		}
		if leftovers := run.uploads(t, c.failedPattern); len(leftovers) != 0 {
			t.Errorf("rpm %s, arch %s: the failed package was left in /out: %v", c.rpmMode, c.archMode, leftovers)
		}
		for _, kept := range []struct {
			pattern string
			name    string
		}{
			{pattern: "*.deb", name: buildArchDebName},
			{pattern: "*.install.tar.gz", name: buildArchTarballName},
			{pattern: c.keptPattern, name: c.keptName},
		} {
			if got := run.uploads(t, kept.pattern); len(got) != 1 || got[0] != kept.name {
				t.Errorf("rpm %s, arch %s: %s in /out = %v, want [%s]", c.rpmMode, c.archMode, kept.pattern, got, kept.name)
			}
		}
	}
}

// With UR_REQUIRE_RPM or UR_REQUIRE_ARCH_PKG set, the same failure stops the
// build, and still leaves no part of the failed package in /out.
func TestBuildArchRequiredPackageFailureLeavesNoPackage(t *testing.T) {
	cases := []struct {
		rpmMode       string
		archMode      string
		knob          string
		failedPattern string
		problem       string
	}{
		{
			rpmMode:       "fail-after-write",
			archMode:      "ok",
			knob:          "UR_REQUIRE_RPM=true",
			failedPattern: "*.rpm*",
			problem:       "make-rpm.sh failed — UR_REQUIRE_RPM=true",
		},
		{
			rpmMode:       "ok",
			archMode:      "fail-after-write",
			knob:          "UR_REQUIRE_ARCH_PKG=true",
			failedPattern: "*.pkg.tar.zst*",
			problem:       "make-arch.sh failed — UR_REQUIRE_ARCH_PKG=true",
		},
	}
	for _, c := range cases {
		run := runBuildArchFixture(t, c.rpmMode, c.archMode, c.knob)
		if run.exitCode != 1 {
			t.Errorf("%s: exit %d, want 1; output:\n%s", c.knob, run.exitCode, run.output)
		}
		if !strings.Contains(run.output, c.problem) {
			t.Errorf("%s: no %q error; output:\n%s", c.knob, c.problem, run.output)
		}
		if leftovers := run.uploads(t, c.failedPattern); len(leftovers) != 0 {
			t.Errorf("%s: the failed package was left in /out: %v", c.knob, leftovers)
		}
	}
}

// Packages that build and pass their scripts' checks land in /out with their
// checksums, as when the scripts wrote to /out directly.
func TestBuildArchMovesBuiltOptionalPackagesToOut(t *testing.T) {
	run := runBuildArchFixture(t, "ok", "ok", "UR_REQUIRE_RPM=true", "UR_REQUIRE_ARCH_PKG=true")
	if run.exitCode != 0 {
		t.Fatalf("exit %d, want 0; output:\n%s", run.exitCode, run.output)
	}
	for _, name := range []string{
		buildArchDebName,
		buildArchTarballName,
		buildArchRpmName,
		buildArchRpmName + ".sha256",
		buildArchPackageName,
		buildArchPackageName + ".sha256",
	} {
		if _, err := os.Stat(filepath.Join(run.outDir, name)); err != nil {
			t.Errorf("%s not in /out: %v", name, err)
		}
	}
	if strings.Contains(run.output, "WARN:") {
		t.Errorf("a clean build warned; output:\n%s", run.output)
	}
}
