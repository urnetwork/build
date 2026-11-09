// SPDX-License-Identifier: MPL-2.0

// ur's Linux packages (REPO-M1-03): build-ur-linux.sh with a recording docker stub that plays nfpm,
// writing each package it is asked for and keeping the rendered template, and distro-smoke.sh with the
// same stub. nfpm's real image and the distributions' images live on the release host: this host fetches
// nothing, so a real package build and smoke run are the release's (EXT-5).

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// docker: image inspect passes unless UR_STUB_NO_IMAGE; run records its arguments, and for nfpm writes
// the target and keeps the stage's nfpm.yaml.
const urStubDocker = `#!/bin/sh
printf 'docker %s\n' "$*" >> "$UR_STUB_EVENTS"
case "$1" in
    image)
        [ -z "${UR_STUB_NO_IMAGE:-}" ]
        exit $?
        ;;
    run)
        stage=""; out=""; packager=""; target=""
        while [ "$#" -gt 0 ]; do
            case "$1" in
                -v)
                    case "$2" in
                        *:/stage:ro) stage="${2%:/stage:ro}" ;;
                        *:/out) out="${2%:/out}" ;;
                    esac
                    shift ;;
                --packager) packager="$2"; shift ;;
                --target) target="$2"; shift ;;
            esac
            shift
        done
        if [ -n "$packager" ]; then
            arch=$(sed -n 's/^arch: "\(.*\)"$/\1/p' "$stage/nfpm.yaml")
            cp "$stage/nfpm.yaml" "$UR_STUB_CAPTURE/nfpm-$arch.yaml"
            printf 'package\n' > "$out/${target#/out/}"
        fi
        ;;
    *) exit 41 ;;
esac
`

type urLinuxHarness struct {
	t         *testing.T
	directory string
	events    string
	capture   string
	out       string
}

func newUrLinuxHarness(t *testing.T) *urLinuxHarness {
	t.Helper()
	directory := t.TempDir()
	self := &urLinuxHarness{t: t, directory: directory, events: filepath.Join(directory, "events"), capture: filepath.Join(directory, "capture"), out: filepath.Join(directory, "out")}
	writeTestScript(t, filepath.Join(directory, "stub-bin", "docker"), urStubDocker)
	for _, arch := range []string{"amd64", "arm64"} {
		writeTestScript(t, filepath.Join(directory, "bin", "linux", arch, "ur"), "binary "+arch+"\n")
	}
	copyTestContent(t, filepath.Join(directory, "home", "sidecar", "LICENSE"), "license\n")
	copyTestContent(t, filepath.Join(directory, "home", "sidecar", "NOTICE"), "notice\n")
	for _, path := range []string{self.capture, self.out} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return self
}

func (self *urLinuxHarness) run(overrides ...string) componentResult {
	self.t.Helper()
	command := exec.Command("bash", filepath.Join(rolloutRoot(self.t), "build-ur-linux.sh"))
	command.Env = environmentWith(append([]string{
		"PATH=" + filepath.Join(self.directory, "stub-bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"UR_STUB_EVENTS=" + self.events,
		"UR_STUB_CAPTURE=" + self.capture,
		"UR_BIN_DIR=" + filepath.Join(self.directory, "bin"),
		"OUT_DIR=" + self.out,
		"BUILD_HOME=" + filepath.Join(self.directory, "home"),
		"EXTERNAL_WARP_VERSION=" + urTestVersion,
		"VERSION_BASE=2026.11.6",
		"VERSION_RELEASE=1234567",
		"GO_MOD_VERSION=2026",
		"SOURCE_DATE_EPOCH=1762646400",
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

func (self *urLinuxHarness) rendered(arch string) string {
	self.t.Helper()
	data, err := os.ReadFile(filepath.Join(self.capture, "nfpm-"+arch+".yaml"))
	if err != nil {
		self.t.Fatal(err)
	}
	return string(data)
}

// nfpm reads "-<code>" as a semver prerelease and writes 2026.7.6~985989570, which sorts below the
// base, so the version splits at its last hyphen into nfpm's version and release.
func TestNfpmVersionSplitsAtTheLastHyphen(t *testing.T) {
	for _, c := range []struct {
		version string
		base    string
		release string
	}{
		{version: "2026.7.6-985989570", base: "2026.7.6", release: "985989570"},
		{version: "2026.7.6-rc1-985989570", base: "2026.7.6-rc1", release: "985989570"},
	} {
		command := exec.Command("bash", "-c", `source "$1"; BUILD_HOME=/nonexistent EXTERNAL_WARP_VERSION="$2" WARP_VERSION= ur_versions; printf '%s %s %s\n' "$VERSION_BASE" "$VERSION_RELEASE" "$WARP_VERSION"`,
			"versions", filepath.Join(rolloutRoot(t), "ur", "lib.sh"), c.version)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("ur_versions %s: %v\n%s", c.version, err, output)
		}
		want := c.base + " " + c.release + " " + c.base + "+" + c.release
		if strings.TrimSpace(string(output)) != want {
			t.Errorf("%s splits as %q, want %q", c.version, strings.TrimSpace(string(output)), want)
		}
	}
	harness := newUrLinuxHarness(t)
	if result := harness.run(); result.exitCode != 0 {
		t.Fatalf("build-ur-linux.sh failed: %+v", result)
	}
	rendered := harness.rendered("amd64")
	unfilled := regexp.MustCompile(`@[A-Z_]+@`)
	if !strings.Contains(rendered, "\nversion: \"2026.11.6\"\n") || !strings.Contains(rendered, "\nrelease: \"1234567\"\n") || unfilled.MatchString(rendered) {
		t.Errorf("the rendered template is not filled at the last hyphen:\n%s", rendered)
	}
}

// rpm and pacman spell the arch x86_64 and aarch64, and the deb keeps amd64 and arm64: the names say
// so, and nfpm, given amd64 and arm64, spells the package's own field.
func TestRpmAndArchSpellTheirArch(t *testing.T) {
	harness := newUrLinuxHarness(t)
	result := harness.run()
	if result.exitCode != 0 {
		t.Fatalf("build-ur-linux.sh failed: %+v", result)
	}
	packaged := []string{}
	for _, line := range strings.Split(result.events, "\n") {
		fields := strings.Fields(line)
		if index := slices.Index(fields, "--packager"); index >= 0 {
			packaged = append(packaged, fields[index+1]+" "+fields[len(fields)-1])
		}
	}
	want := []string{
		"deb /out/ur_" + urTestVersion + "_amd64.deb", "rpm /out/ur-" + urTestVersion + ".x86_64.rpm",
		"archlinux /out/ur-" + urTestVersion + "-x86_64.pkg.tar.zst",
		"deb /out/ur_" + urTestVersion + "_arm64.deb", "rpm /out/ur-" + urTestVersion + ".aarch64.rpm",
		"archlinux /out/ur-" + urTestVersion + "-aarch64.pkg.tar.zst",
	}
	if !slices.Equal(packaged, want) {
		t.Errorf("nfpm was asked for %v, want %v", packaged, want)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if !strings.Contains(harness.rendered(arch), "\narch: \""+arch+"\"\n") {
			t.Errorf("the %s template does not hand nfpm %s", arch, arch)
		}
	}
	written, err := os.ReadDir(harness.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 6 {
		t.Errorf("build-ur-linux.sh wrote %d packages, not six", len(written))
	}
}

// The binary is static and the daemon runs as the person (Q-CORE-3), so the packages carry no
// dependency, maintainer script or unit: ur, the git-remote-ur link and the documents.
func TestPackageShipsNoUnitOrScript(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur", "nfpm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	template := string(data)
	for _, forbidden := range []string{"\nscripts:", "\ndepends:", "\nrecommends:", "\nsuggests:", ".service", "systemd", "postinstall", "preremove"} {
		if strings.Contains(template, forbidden) {
			t.Errorf("nfpm.yaml holds %q", forbidden)
		}
	}
	destinations := []string{}
	for _, line := range strings.Split(template, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "dst: ") || strings.HasPrefix(trimmed, "- dst: ") {
			destinations = append(destinations, strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "dst: ")))
		}
	}
	want := []string{"/usr/share/doc/ur", "/usr/bin/ur", "/usr/bin/git-remote-ur", "/usr/share/doc/ur/LICENSE", "/usr/share/doc/ur/NOTICE"}
	if !slices.Equal(destinations, want) {
		t.Errorf("the packages install %v, want %v", destinations, want)
	}
	if !strings.Contains(template, "  - src: /usr/bin/ur\n    dst: /usr/bin/git-remote-ur\n    type: symlink\n") {
		t.Errorf("git-remote-ur is not a link to /usr/bin/ur")
	}
}

// Every container the step runs has no network, so a fetch cannot hide a dependency.
func TestLinuxStepRunsWithNoNetwork(t *testing.T) {
	harness := newUrLinuxHarness(t)
	result := harness.run()
	if result.exitCode != 0 {
		t.Fatalf("build-ur-linux.sh failed: %+v", result)
	}
	runs := 0
	for _, line := range strings.Split(result.events, "\n") {
		if strings.HasPrefix(line, "docker run ") {
			runs += 1
			if !strings.Contains(line, " --rm ") || !strings.Contains(line, " --network none ") {
				t.Errorf("a container ran with a network or was kept: %s", line)
			}
		}
	}
	if runs != 10 {
		t.Errorf("%d containers ran: six packages and four distributions make ten", runs)
	}
}

func TestLinuxStepRefusesAMissingImage(t *testing.T) {
	harness := newUrLinuxHarness(t)
	result := harness.run("UR_STUB_NO_IMAGE=1")
	if result.exitCode == 0 || strings.Contains(result.events, "docker run") || !strings.Contains(result.output, "build-linux.sh builds it") {
		t.Fatalf("build-ur-linux.sh went on without nfpm's image: %+v", result)
	}
}

// distro-smoke.sh installs each package on its distribution with no network, under its
// sidecar-test-ur-smoke-<distro> name, removed after.
func TestDistroSmokeRunsEachDistributionOffline(t *testing.T) {
	harness := newUrLinuxHarness(t)
	for _, name := range []string{"ur_" + urTestVersion + "_amd64.deb", "ur-" + urTestVersion + ".x86_64.rpm", "ur-" + urTestVersion + "-x86_64.pkg.tar.zst"} {
		copyTestContent(t, filepath.Join(harness.out, name), "package\n")
	}
	command := exec.Command("bash", filepath.Join(rolloutRoot(t), "ur", "distro-smoke.sh"), harness.out, urTestVersion)
	command.Env = environmentWith("PATH="+filepath.Join(harness.directory, "stub-bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "UR_STUB_EVENTS="+harness.events, "UR_STUB_CAPTURE="+harness.capture)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("distro-smoke.sh: %v\n%s", err, output)
	}
	events, err := os.ReadFile(harness.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ distro, image, check string }{
		{distro: "ubuntu-22.04", image: "ubuntu:22.04", check: "dpkg --purge ur"},
		{distro: "ubuntu-24.04", image: "ubuntu:24.04", check: "dpkg --purge ur"},
		{distro: "fedora", image: "fedora:latest", check: "rpm -e ur"},
		{distro: "archlinux", image: "archlinux:latest", check: "pacman -R --noconfirm ur"},
	} {
		found := false
		for _, line := range strings.Split(string(events), "docker run ") {
			if strings.Contains(line, "--name sidecar-test-ur-smoke-"+c.distro+" ") {
				found = true
				if !strings.HasPrefix(line, "--rm --network none") || !strings.Contains(line, " "+c.image+" ") || !strings.Contains(line, c.check) || !strings.Contains(line, "ur --version") {
					t.Errorf("%s's run is not offline, removed and checked: %s", c.distro, line)
				}
			}
		}
		if !found {
			t.Errorf("distro-smoke.sh did not run %s", c.distro)
		}
	}
}
