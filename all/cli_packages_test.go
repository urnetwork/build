// SPDX-License-Identifier: MPL-2.0

// The CLI's package-manager assets: all/cli-packages.zsh and its non-fatal
// place in run.sh.
package allbuild

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const cliTestVersion = "2026.11.22-1105365180"

// cliMinerBuild lays out sn_cli_release's output for the package's platforms.
func cliMinerBuild(t *testing.T) (build, license string) {
	t.Helper()
	dir := t.TempDir()
	for _, platform := range []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "linux/arm", "windows/amd64", "windows/arm64"} {
		name := "provider"
		if strings.HasPrefix(platform, "windows/") {
			name += ".exe"
		}
		path := filepath.Join(dir, "build", filepath.FromSlash(platform), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("miner "+platform+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	license = filepath.Join(dir, "LICENSE")
	if err := os.WriteFile(license, []byte("synthetic license\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "build"), license
}

// runCLIPackages runs the script with nfpm replaced by nfpm (a path) and
// returns the output directory's files.
func runCLIPackages(t *testing.T, nfpm, build, license string) (string, map[string][]byte) {
	t.Helper()
	out := t.TempDir()
	cmd := exec.Command("zsh", "-f", filepath.Join(rolloutRoot(t), "cli-packages.zsh"), cliTestVersion, build, license, out)
	cmd.Env = environmentWith("NFPM=" + nfpm)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cli-packages.zsh: %v\n%s", err, output)
	}
	files := map[string][]byte{}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(out, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = data
	}
	return out, files
}

// fakeNfpm writes its config to the target, so the test reads what the
// script asked nfpm to package.
func fakeNfpm(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nfpm")
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in --config) config=$2; shift ;; --packager) packager=$2; shift ;; --target) target=$2; shift ;; esac; shift; done\n" +
		"{ printf 'packager: %s\\n' \"$packager\"; cat \"$config\"; for src in $(sed -n 's/^  - src: //p' \"$config\"); do printf 'file %s: ' \"$src\"; cat \"$src\"; done; } > \"$target\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func tarGzMembers(t *testing.T, data []byte) map[string]*tar.Header {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		h.PAXRecords = map[string]string{"body": string(body)}
		out[h.Name] = h
	}
}

func TestCLIPackagesNamesContentsAndMarkers(t *testing.T) {
	build, license := cliMinerBuild(t)
	_, files := runCLIPackages(t, fakeNfpm(t), build, license)
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{
		"urnetwork-provider-" + cliTestVersion + "-darwin-amd64.tar.gz",
		"urnetwork-provider-" + cliTestVersion + "-darwin-arm64.tar.gz",
		"urnetwork-provider-" + cliTestVersion + "-linux-amd64.tar.gz",
		"urnetwork-provider-" + cliTestVersion + "-linux-arm64.tar.gz",
		"urnetwork-provider-" + cliTestVersion + "-windows-amd64.zip",
		"urnetwork-provider-" + cliTestVersion + "-windows-arm64.zip",
		"urnetwork-provider-" + cliTestVersion + ".aarch64.rpm",
		"urnetwork-provider-" + cliTestVersion + ".x86_64.rpm",
		"urnetwork-provider_" + cliTestVersion + "_amd64.deb",
		"urnetwork-provider_" + cliTestVersion + "_arm64.deb",
		"urnetwork-provider_" + cliTestVersion + "_armhf.deb",
	}
	sort.Strings(want)
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("assets:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}

	// the archives: the miner binary renamed, mode 0755, and LICENSE, at the top level
	members := tarGzMembers(t, files["urnetwork-provider-"+cliTestVersion+"-linux-arm64.tar.gz"])
	if len(members) != 2 || members["urnetwork-provider"] == nil || members["LICENSE"] == nil {
		t.Fatalf("tar members %v", members)
	}
	if h := members["urnetwork-provider"]; h.Mode&0o777 != 0o755 || h.PAXRecords["body"] != "miner linux/arm64\n" || h.Uid != 0 || h.Gid != 0 {
		t.Fatalf("binary member %+v", h)
	}
	zipData := files["urnetwork-provider-"+cliTestVersion+"-windows-arm64.zip"]
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatal(err)
	}
	var zipNames []string
	for _, f := range zr.File {
		zipNames = append(zipNames, f.Name)
		if f.Name == "urnetwork-provider.exe" {
			rc, _ := f.Open()
			body, _ := io.ReadAll(rc)
			if string(body) != "miner windows/arm64\n" {
				t.Fatalf("exe %q", body)
			}
		}
	}
	if strings.Join(zipNames, " ") != "urnetwork-provider.exe LICENSE" {
		t.Fatalf("zip members %v", zipNames)
	}

	// the packages: name, version split, binary, license path and the managed-by command
	deb := string(files["urnetwork-provider_"+cliTestVersion+"_armhf.deb"])
	mustContainAll(t, deb, "packager: deb", "name: urnetwork-provider", "arch: arm7", `version: "2026.11.22"`, `release: "1105365180"`,
		"dst: /usr/bin/urnetwork-provider", "dst: /usr/share/doc/urnetwork-provider/copyright", "dst: /usr/share/urnetwork-provider/managed-by",
		"miner linux/arm\n", "sudo apt-get install --only-upgrade urnetwork-provider\n", "recommends:\n  - ca-certificates")
	rpm := string(files["urnetwork-provider-"+cliTestVersion+".aarch64.rpm"])
	mustContainAll(t, rpm, "packager: rpm", "arch: arm64", "dst: /usr/share/licenses/urnetwork-provider/LICENSE",
		"miner linux/arm64\n", "sudo dnf upgrade urnetwork-provider\n")
}

// A resumed nightly rebuilds the bytes it already uploaded. (The fake nfpm
// echoes its temporary config paths, so the real-nfpm test checks the
// packages; this one checks the archives the script writes itself.)
func TestCLIPackagesAreDeterministic(t *testing.T) {
	build, license := cliMinerBuild(t)
	nfpm := fakeNfpm(t)
	_, a := runCLIPackages(t, nfpm, build, license)
	_, b := runCLIPackages(t, nfpm, build, license)
	checked := 0
	for name, data := range a {
		if strings.HasSuffix(name, ".deb") || strings.HasSuffix(name, ".rpm") {
			continue
		}
		checked++
		if !bytes.Equal(data, b[name]) {
			t.Errorf("%s differs between two builds", name)
		}
	}
	if checked != 6 {
		t.Fatalf("%d archives checked", checked)
	}
}

func TestCLIPackagesRefuseMissingInputs(t *testing.T) {
	build, license := cliMinerBuild(t)
	os.Remove(filepath.Join(build, "windows", "arm64", "provider.exe"))
	out := t.TempDir()
	cmd := exec.Command("zsh", "-f", filepath.Join(rolloutRoot(t), "cli-packages.zsh"), cliTestVersion, build, license, out)
	cmd.Env = environmentWith("NFPM=" + fakeNfpm(t))
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "windows/arm64/provider.exe") {
		t.Fatalf("a missing binary was not refused: %v\n%s", err, output)
	}
	cmd = exec.Command("zsh", "-f", filepath.Join(rolloutRoot(t), "cli-packages.zsh"), "2026.11.22", build, license, out)
	if err := cmd.Run(); err == nil {
		t.Fatal("a version without a code was accepted")
	}
}

// With the real nfpm on PATH (as the build host's go run gives), the .deb is
// a package dpkg reads: the control file and the installed paths.
func TestCLIPackagesWithRealNfpm(t *testing.T) {
	nfpm, err := exec.LookPath("nfpm")
	if err != nil {
		t.Skip("nfpm is not on PATH")
	}
	build, license := cliMinerBuild(t)
	_, files := runCLIPackages(t, nfpm, build, license)
	deb := files["urnetwork-provider_"+cliTestVersion+"_amd64.deb"]
	if !bytes.HasPrefix(deb, []byte("!<arch>\n")) {
		t.Fatal("not an ar archive")
	}
	members := map[string][]byte{}
	for off := 8; off+60 <= len(deb); {
		header := deb[off : off+60]
		name := strings.TrimRight(strings.TrimSpace(string(header[0:16])), "/")
		size, err := strconv.Atoi(strings.TrimSpace(string(header[48:58])))
		if err != nil {
			t.Fatal(err)
		}
		members[name] = deb[off+60 : off+60+size]
		off += 60 + size + size%2
	}
	control := tarGzMembers(t, members["control.tar.gz"])
	var text string
	for name, h := range control {
		if strings.TrimPrefix(name, "./") == "control" {
			text = h.PAXRecords["body"]
		}
	}
	mustContainAll(t, text, "Package: urnetwork-provider\n", "Version: 2026.11.22-1105365180\n", "Architecture: amd64\n", "Section: net\n", "Recommends: ca-certificates\n")
	data := tarGzMembers(t, members["data.tar.gz"])
	if h := data["./usr/bin/urnetwork-provider"]; h == nil || h.Mode&0o777 != 0o755 || h.PAXRecords["body"] != "miner linux/amd64\n" {
		t.Fatalf("binary %+v", h)
	}
	if h := data["./usr/share/urnetwork-provider/managed-by"]; h == nil || h.PAXRecords["body"] != "sudo apt-get install --only-upgrade urnetwork-provider\n" {
		t.Fatalf("managed-by %+v", h)
	}
	rpm := files["urnetwork-provider-"+cliTestVersion+".x86_64.rpm"]
	if !bytes.HasPrefix(rpm, []byte{0xed, 0xab, 0xee, 0xdb}) || !bytes.Contains(rpm, []byte("urnetwork-provider")) {
		t.Fatal("not an rpm")
	}
	// and nfpm's packages are the same bytes on a rebuild
	_, again := runCLIPackages(t, nfpm, build, license)
	for name, data := range files {
		if !bytes.Equal(data, again[name]) {
			t.Errorf("%s differs between two builds", name)
		}
	}
}

func mustContainAll(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, s)
		}
	}
}

// The run.sh wiring, run on extracted production code: the packages build
// after the miner, a failed build only warns, and each asset uploads staged,
// so a failed upload only warns too; the nightly continues either way.
const cliPackagesSetup = `
mkdir -p "$BUILD_HOME/sn/cli/miner/build"
cat > "$BUILD_HOME/all/cli-packages.zsh" <<'SCRIPT'
#!/bin/sh
printf 'cli-packages %s %s %s\n' "$1" "$2" "$3" >> "$BUILD_HOME/events"
if [ "$FAIL_STEP" = cli-packages ]; then exit 3; fi
mkdir -p "$4"
for name in urnetwork-provider-$1-linux-amd64.tar.gz urnetwork-provider_$1_amd64.deb; do printf 'package %s\n' "$name" > "$4/$name"; done
SCRIPT
chmod +x "$BUILD_HOME/all/cli-packages.zsh"
builder_message() { printf 'message:%s\n' "$*" >> "$event_log"; }
`

func cliPackagesRegion(t *testing.T) string {
	t.Helper()
	build := componentRegion(t, `cli_packages_dir="$BUILD_HOME`, "\n\n")
	upload := componentRegion(t, "for cli_package in ", "\n\n")
	return build + "\n" + upload + "\n"
}

func TestRunCLIPackagesUploadStaged(t *testing.T) {
	setup := componentFunctions(t, "github_release_upload_staged") + cliPackagesSetup
	result := runComponent(t, cliPackagesRegion(t), setup)
	if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("%+v", result)
	}
	mustContainAll(t, result.events, "cli-packages 0.0.0-123 "+"", "asset:urnetwork-provider-0.0.0-123-linux-amd64.tar.gz\n", "asset:urnetwork-provider_0.0.0-123_amd64.deb\n")
	if strings.Contains(result.events, "message:") {
		t.Fatalf("a good build warned: %+v", result)
	}
}

func TestRunCLIPackagesFailuresOnlyWarn(t *testing.T) {
	setup := componentFunctions(t, "github_release_upload_staged") + cliPackagesSetup
	// the build fails: a warning, no uploads, the nightly goes on
	result := runComponent(t, cliPackagesRegion(t), setup, "FAIL_STEP=cli-packages")
	if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") || strings.Contains(result.events, "asset:") {
		t.Fatalf("build failure: %+v", result)
	}
	mustContainAll(t, result.events, "message:warning: the urnetwork-provider packages for `0.0.0-123` did not build")
	// an upload fails: a warning naming the asset kind, the nightly goes on
	result = runComponent(t, cliPackagesRegion(t), setup, "FAIL_STEP=publish")
	if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("upload failure: %+v", result)
	}
	mustContainAll(t, result.events, "message:warning: urnetwork-provider package `urnetwork-provider-0.0.0-123-linux-amd64.tar.gz` was not attached")
}

// In run.sh the packages build right after the miner's own upload and upload
// before the release is published.
func TestRunCLIPackagesOrder(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	miner := strings.Index(source, "\nsn_cli_release miner provider ")
	minerUpload := strings.Index(source, `github_release_upload "urnetwork-provider-${EXTERNAL_WARP_VERSION}.tar.gz"`)
	build := strings.Index(source, `"$BUILD_HOME/all/cli-packages.zsh" "$EXTERNAL_WARP_VERSION"`)
	upload := strings.Index(source, `github_release_upload_staged "$(basename "$cli_package")" "$cli_package" "urnetwork-provider package"`)
	publish := strings.Index(source, "\ngithub_create_release\n")
	if miner < 0 || minerUpload < 0 || build < 0 || upload < 0 || publish < 0 {
		t.Fatalf("missing a step: miner %d upload %d build %d staged %d publish %d", miner, minerUpload, build, upload, publish)
	}
	if !(miner < minerUpload && minerUpload < build && build < upload && upload < publish) {
		t.Fatalf("order: miner %d, its upload %d, packages %d, staged uploads %d, publish %d", miner, minerUpload, build, upload, publish)
	}
}
