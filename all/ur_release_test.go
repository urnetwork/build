// SPDX-License-Identifier: MPL-2.0

// ur's release step (REPO-M1-01): build-ur.sh over a synthetic build home, with recording stubs for
// make and go and for the three platform scripts, which their own tests take apart; and run.sh's ur
// region in component_required_test.go's harness. The real six-target build on this host is
// ur_release_host_test.go's, under its own tag, since it compiles ur six times.

package allbuild

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const urTestVersion = "2026.11.6-1234567"

// Reading 9's names (plan-REPO-small-readings #9), written out apart from all/ur/artifacts.txt.
func urReadingNineNames(version string) []string {
	names := []string{}
	for _, arch := range []string{"amd64", "arm64"} {
		names = append(names,
			"ur-"+version+"-darwin-"+arch+".tar.gz", "ur-"+version+"-darwin-"+arch+".pkg",
			"ur-"+version+"-linux-"+arch+".tar.gz", "ur_"+version+"_"+arch+".deb",
			"ur-"+version+"-windows-"+arch+".zip")
	}
	for _, arch := range []string{"x86_64", "aarch64"} {
		names = append(names, "ur-"+version+"."+arch+".rpm", "ur-"+version+"-"+arch+".pkg.tar.zst")
	}
	slices.Sort(names)
	return names
}

// all/ur/artifacts.txt, expanded as the scripts expand it.
func urTableNames(t *testing.T, version string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur", "artifacts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, strings.ReplaceAll(line, "{version}", version))
	}
	return names
}

func TestUrArtifactTableIsReadingNine(t *testing.T) {
	names := urTableNames(t, urTestVersion)
	if len(names) != 14 {
		t.Errorf("the table names %d artifacts, and six targets make 14", len(names))
	}
	sorted := slices.Sorted(slices.Values(names))
	if !slices.Equal(sorted, urReadingNineNames(urTestVersion)) {
		t.Errorf("the table is %v, and reading 9 is %v", sorted, urReadingNineNames(urTestVersion))
	}
}

// A build home with the real build-ur.sh and its lib, stubs for the three platform scripts, and a
// sidecar checkout whose commit time is SOURCE_DATE_EPOCH.
type urBuildHome struct {
	t         *testing.T
	directory string
	bin       string
	log       string
	out       string
}

func copyTestFile(t *testing.T, from string, to string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, mode); err != nil {
		t.Fatal(err)
	}
}

func writeTestScript(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

// make: the six targets' ur, each a script printing its version line.
const urStubMake = `#!/bin/sh
printf 'make %s\n' "$*" >> "$UR_STUB_LOG"
out=""
for argument in "$@"; do
    case "$argument" in OUT=*) out="${argument#OUT=}" ;; esac
done
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
    name=ur
    case "$target" in windows/*) name=ur.exe ;; esac
    mkdir -p "$out/$target"
    printf '#!/bin/sh\necho "ur %s"\n' "$target" > "$out/$target/$name"
    chmod 0755 "$out/$target/$name"
done
`

// go: version -m with or without each fork's => line, and test, which passes.
const urStubGo = `#!/bin/sh
case "$1" in
    version)
        printf '%s: go1.27.3\n\tpath\tgithub.com/urnetwork/sidecar/cli\n' "$3"
        printf '\tdep\tgithub.com/pion/sctp\tv1.11.1\n'
        if [ -z "${UR_STUB_NO_SCTP_FORK:-}" ]; then printf '\t=>\t../../connect/sctp\t(devel)\t\n'; fi
        printf '\tdep\tgvisor.dev/gvisor\tv0.0.0-20260909230408-9b1144b679cb\n'
        if [ -z "${UR_STUB_NO_GVISOR_FORK:-}" ]; then printf '\t=>\tgithub.com/urnetwork/gvisor\tv0.0.0-20260909230408-9b1144b679cb\th1:x=\n'; fi
        ;;
    test)
        printf 'go test %s\n' "$RELEASE_BINARIES" >> "$UR_STUB_LOG"
        printf '%s\n' "--- PASS: TestHygieneReleaseBinary (0.01s)"
        ;;
    *)
        echo "unexpected go $*" >&2
        exit 41
        ;;
esac
`

// A platform script's stub: it records itself and writes the names it owns.
func urStubPlatform(step string, names ...string) string {
	script := "#!/bin/sh\nset -e\nprintf '%s\\n' " + step + " >> \"$UR_STUB_LOG\"\n"
	for _, name := range names {
		script += "printf 'artifact\\n' > \"$OUT_DIR/" + strings.ReplaceAll(name, urTestVersion, "${EXTERNAL_WARP_VERSION}") + "\"\n"
	}
	return script
}

func newUrBuildHome(t *testing.T) *urBuildHome {
	t.Helper()
	directory := t.TempDir()
	self := &urBuildHome{
		t:         t,
		directory: directory,
		bin:       filepath.Join(directory, "stub-bin"),
		log:       filepath.Join(directory, "stub.log"),
		out:       filepath.Join(directory, "out", "ur"),
	}
	root := rolloutRoot(t)
	copyTestFile(t, filepath.Join(root, "build-ur.sh"), filepath.Join(directory, "all", "build-ur.sh"), 0o700)
	copyTestFile(t, filepath.Join(root, "ur", "lib.sh"), filepath.Join(directory, "all", "ur", "lib.sh"), 0o600)
	copyTestFile(t, filepath.Join(root, "ur", "artifacts.txt"), filepath.Join(directory, "all", "ur", "artifacts.txt"), 0o600)
	writeTestScript(t, filepath.Join(directory, "all", "build-ur-macos.sh"), urStubPlatform("macos",
		"ur-"+urTestVersion+"-darwin-amd64.tar.gz", "ur-"+urTestVersion+"-darwin-amd64.pkg",
		"ur-"+urTestVersion+"-darwin-arm64.tar.gz", "ur-"+urTestVersion+"-darwin-arm64.pkg"))
	writeTestScript(t, filepath.Join(directory, "all", "build-ur-linux.sh"), urStubPlatform("linux",
		"ur_"+urTestVersion+"_amd64.deb", "ur_"+urTestVersion+"_arm64.deb",
		"ur-"+urTestVersion+".x86_64.rpm", "ur-"+urTestVersion+".aarch64.rpm",
		"ur-"+urTestVersion+"-x86_64.pkg.tar.zst", "ur-"+urTestVersion+"-aarch64.pkg.tar.zst"))
	writeTestScript(t, filepath.Join(directory, "all", "build-ur-windows.sh"), urStubPlatform("windows",
		"ur-"+urTestVersion+"-windows-amd64.zip", "ur-"+urTestVersion+"-windows-arm64.zip"))
	writeTestScript(t, filepath.Join(self.bin, "make"), urStubMake)
	writeTestScript(t, filepath.Join(self.bin, "go"), urStubGo)

	sidecar := filepath.Join(directory, "sidecar")
	for name, content := range map[string]string{"LICENSE": "license\n", "NOTICE": "notice\n", "check/doc.go": "package check\n", "cli/main.go": "package main\n"} {
		copyTestContent(t, filepath.Join(sidecar, filepath.FromSlash(name)), content)
	}
	git := func(arguments ...string) {
		command := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, arguments...)...)
		command.Dir = sidecar
		command.Env = branchesGitEnvironment("GIT_COMMITTER_DATE=@1762646400 +0000", "GIT_AUTHOR_DATE=@1762646400 +0000")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	git("init", "-q", "-b", "sidecar")
	git("add", "-A")
	git("commit", "-q", "-m", "Start the sidecar")
	return self
}

func copyTestContent(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A built urarchive, from this checkout.
func buildTestUrarchive(t *testing.T) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "urarchive")
	command := exec.Command("go", "build", "-o", output, "./urarchive")
	command.Dir = rolloutRoot(t)
	command.Env = environmentWith("GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=", "CGO_ENABLED=0")
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build urarchive: %v\n%s", err, combined)
	}
	return output
}

func (self *urBuildHome) run(overrides ...string) (int, string) {
	self.t.Helper()
	command := exec.Command("bash", filepath.Join(self.directory, "all", "build-ur.sh"))
	command.Env = environmentWith(append([]string{
		"BUILD_HOME=" + self.directory,
		"EXTERNAL_WARP_VERSION=" + urTestVersion,
		"WARP_VERSION=",
		"OUT_DIR=" + self.out,
		"GO=" + filepath.Join(self.bin, "go"),
		"MAKE=" + filepath.Join(self.bin, "make"),
		"URARCHIVE=" + buildTestUrarchive(self.t),
		"UR_STUB_LOG=" + self.log,
		"SOURCE_DATE_EPOCH=1",
	}, overrides...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			self.t.Fatal(err)
		}
		return exitError.ExitCode(), string(output)
	}
	return 0, string(output)
}

func (self *urBuildHome) written() []string {
	self.t.Helper()
	entries, err := os.ReadDir(self.out)
	if err != nil && !os.IsNotExist(err) {
		self.t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestBuildUrWritesEveryTargetsName(t *testing.T) {
	home := newUrBuildHome(t)
	code, output := home.run()
	if code != 0 {
		t.Fatalf("build-ur.sh failed: %d\n%s", code, output)
	}
	if written := home.written(); !slices.Equal(written, urReadingNineNames(urTestVersion)) {
		t.Errorf("build-ur.sh wrote %v, and reading 9 is %v", written, urReadingNineNames(urTestVersion))
	}
	log, err := os.ReadFile(home.log)
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(steps) != 5 || !strings.HasPrefix(steps[0], "make -C "+home.directory+"/sidecar/cli build OUT=") || !strings.HasPrefix(steps[1], "go test ") ||
		steps[2] != "macos" || steps[3] != "linux" || steps[4] != "windows" {
		t.Errorf("build-ur.sh ran %q", steps)
	}
	if binaries := strings.Fields(strings.TrimPrefix(steps[1], "go test ")); len(binaries) != 6 {
		t.Errorf("TestHygieneReleaseBinary read %v, not the six", binaries)
	}
}

// Each Linux .tar.gz holds ur, git-remote-ur as a link to it, LICENSE and NOTICE, stamped with the
// sidecar commit's time and not the caller's SOURCE_DATE_EPOCH.
func TestBuildUrLinuxArchiveLinksGitRemoteUr(t *testing.T) {
	home := newUrBuildHome(t)
	if code, output := home.run(); code != 0 {
		t.Fatalf("build-ur.sh failed: %d\n%s", code, output)
	}
	file, err := os.Open(filepath.Join(home.out, "ur-"+urTestVersion+"-linux-arm64.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decompressor, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(decompressor)
	names := []string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if header.ModTime.Unix() != 1762646400 {
			t.Errorf("%s is stamped %v, not the sidecar commit's time", header.Name, header.ModTime)
		}
		if header.Name == "git-remote-ur" && (header.Typeflag != tar.TypeSymlink || header.Linkname != "ur") {
			t.Errorf("git-remote-ur is %c to %q, not a link to ur", header.Typeflag, header.Linkname)
		}
		if header.Name == "ur" {
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), "ur linux/arm64") {
				t.Errorf("the arm64 archive holds another target's ur: %q", content)
			}
		}
	}
	if want := []string{"LICENSE", "NOTICE", "git-remote-ur", "ur"}; !slices.Equal(names, want) {
		t.Errorf("the archive holds %v, want %v", names, want)
	}
}

// go version -m must show cli's gVisor fork: the release fixture, which strips every replace, cannot
// see a ur that lost it (reading 3).
func TestBuildUrFailsWithoutTheGvisorFork(t *testing.T) {
	home := newUrBuildHome(t)
	code, output := home.run("UR_STUB_NO_GVISOR_FORK=1")
	if code == 0 || !strings.Contains(output, "does not link cli's gVisor fork") {
		t.Fatalf("build-ur.sh passed a ur without the gVisor fork: %d\n%s", code, output)
	}
	if written := home.written(); len(written) != 0 {
		t.Errorf("a refused build wrote %v", written)
	}
}

func TestBuildUrFailsWithoutTheSctpFork(t *testing.T) {
	home := newUrBuildHome(t)
	code, output := home.run("UR_STUB_NO_SCTP_FORK=1")
	if code == 0 || !strings.Contains(output, "does not link connect's sctp fork") {
		t.Fatalf("build-ur.sh passed a ur without the sctp fork: %d\n%s", code, output)
	}
}

// A platform step that leaves a file the table does not name fails the build, so nothing unlisted
// rides to the release.
func TestBuildUrRefusesAnArtifactOutsideTheTable(t *testing.T) {
	home := newUrBuildHome(t)
	writeTestScript(t, filepath.Join(home.directory, "all", "build-ur-windows.sh"), urStubPlatform("windows",
		"ur-"+urTestVersion+"-windows-amd64.zip", "ur-"+urTestVersion+"-windows-arm64.zip", "wintun.dll"))
	code, output := home.run()
	if code == 0 || !strings.Contains(output, "wintun.dll is not one of all/ur/artifacts.txt's names") {
		t.Fatalf("build-ur.sh took an unlisted file: %d\n%s", code, output)
	}
}

// A platform step that fails stops the build.
func TestBuildUrStopsAtAFailedPlatform(t *testing.T) {
	home := newUrBuildHome(t)
	writeTestScript(t, filepath.Join(home.directory, "all", "build-ur-linux.sh"), "#!/bin/sh\nexit 37\n")
	code, output := home.run()
	if code == 0 {
		t.Fatalf("build-ur.sh went on past a failed platform step:\n%s", output)
	}
	log, err := os.ReadFile(home.log)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(strings.Split(string(log), "\n"), "windows") {
		t.Errorf("the Windows step ran after the Linux one failed")
	}
}

// run.sh builds ur after the draft release exists and before it is finalized, and preflights it before
// any build starts.
func TestRunBuildsUrAfterTheDraftAndBeforeFinalizing(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	draft := strings.Index(source, "\ngithub_create_draft_release\n")
	region := strings.Index(source, "\nif release_build cli-ur \"$UR_OUT\"; then\n")
	finalize := strings.Index(source, "\ngithub_create_release\n")
	if draft < 0 || region < 0 || finalize < 0 || !(draft < region && region < finalize) {
		t.Errorf("draft at %d, ur at %d, finalize at %d: ur must sit between them", draft, region, finalize)
	}
	preflight := strings.Index(source, "\nur_release_preflight\n")
	stage := strings.Index(source, "\nwarpctl stage version")
	if preflight < 0 || stage < 0 || preflight > stage {
		t.Errorf("ur's preflight at %d does not run before the release starts at %d", preflight, stage)
	}
}

// The ur region in component_required_test.go's harness, with a build-ur.sh stub writing what
// UR_STUB_MODE says.
func runUrRegion(t *testing.T, setup string, overrides ...string) componentResult {
	t.Helper()
	region := componentRegion(t, `UR_OUT="${BUILD_OUT:-$BUILD_HOME/out}/ur"`, "builder_message \"ur \\`")
	names := urTableNames(t, "0.0.0-123")
	stub := `#!/bin/sh
printf 'build-ur\n' >> "$BUILD_HOME/events"
mkdir -p "$OUT_DIR"
case "${UR_STUB_MODE:-complete}" in
    none) exit 0 ;;
    fail) exit 37 ;;
esac
`
	for index, name := range names {
		if index == 0 {
			stub += "if [ \"$UR_STUB_MODE\" = empty ]; then : > \"$OUT_DIR/" + name + "\"; else printf 'new\\n' > \"$OUT_DIR/" + name + "\"; fi\n"
			continue
		}
		if index == len(names)-1 {
			stub += "[ \"$UR_STUB_MODE\" = missing ] || "
		}
		stub += "printf 'new\\n' > \"$OUT_DIR/" + name + "\"\n"
	}
	setup = componentFunctions(t, "ur_artifact_names", "require_ur_artifacts") + `
mkdir -p "$BUILD_HOME/all/ur"
command cp "` + filepath.Join(rolloutRoot(t), "ur", "artifacts.txt") + `" "$BUILD_HOME/all/ur/artifacts.txt"
cat > "$BUILD_HOME/all/build-ur.sh" <<'UR_STUB'
` + stub + `UR_STUB
chmod 0700 "$BUILD_HOME/all/build-ur.sh"
MACOS_DIRECT_IDENTITY=SYNTHETIC
UR_INSTALLER_IDENTITY=SYNTHETIC
APPLE_API_KEY_P8=
# ursums, after every artifact's upload
go() {
    [[ "$1 $2" == "run ./ursums" && "$PWD" == "$BUILD_HOME/all" ]] || { printf 'unexpected-go\n' >> "$event_log"; return 41; }
    record_component_step sums || return $?
    local output="${@: -1}"
    mkdir -p "$output" && printf 'release\n' > "$output/ur-release.json" && printf 'sums\n' > "$output/SHA256SUMS"
}
` + setup
	return runComponent(t, region, setup, overrides...)
}

func TestRunUploadsEveryUrArtifact(t *testing.T) {
	result := runUrRegion(t, "")
	if result.exitCode != 0 || !strings.Contains(result.events, "release-continued") {
		t.Fatalf("the ur region failed: %+v", result)
	}
	for _, name := range urTableNames(t, "0.0.0-123") {
		if !strings.Contains(result.events, "asset:"+name+"\n") {
			t.Errorf("the region did not upload %s: %+v", name, result)
		}
	}
	if stepCount(result.events, "scan") != 16 {
		t.Errorf("VirusTotal ran %d times, not once a file", stepCount(result.events, "scan"))
	}
	// the sums come after every artifact's upload, and go up last
	sums := strings.Index(result.events, "sums\n")
	lastArtifact := strings.LastIndex(result.events, "asset:ur-0.0.0-123-windows-arm64.zip\n")
	release := strings.Index(result.events, "asset:ur-release.json\n")
	listing := strings.Index(result.events, "asset:SHA256SUMS\n")
	if !(lastArtifact >= 0 && lastArtifact < sums && sums < release && release < listing) {
		t.Errorf("the sums did not follow every artifact's upload: %+v", result)
	}
}

func TestRunRejectsMissingUrArtifacts(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "none", "fail"} {
		result := runUrRegion(t, "", "UR_STUB_MODE="+mode)
		if result.exitCode == 0 || strings.Contains(result.events, "publish\n") || strings.Contains(result.events, "release-continued") {
			t.Fatalf("%s ur artifacts falsely succeeded: %+v", mode, result)
		}
	}
}

// Artifacts an earlier run left are removed before the build, so a build that writes nothing cannot
// publish them.
func TestRunRejectsStaleUrArtifacts(t *testing.T) {
	setup := `mkdir -p "$BUILD_HOME/out/ur"
`
	for _, name := range urTableNames(t, "0.0.0-123") {
		setup += `printf 'old\n' > "$BUILD_HOME/out/ur/` + name + `"
`
	}
	result := runUrRegion(t, setup, "UR_STUB_MODE=none")
	if result.exitCode == 0 || strings.Contains(result.events, "publish\n") || strings.Contains(result.events, "release-continued") {
		t.Fatalf("stale ur artifacts were published: %+v", result)
	}
}

// The preflight fails on a missing script or a cli without its build, before any work.
func TestRunPreflightRefusesAMissingUrScript(t *testing.T) {
	preflight := componentFunctions(t, "ur_artifact_names", "ur_release_preflight")
	for _, missing := range []string{"build-ur-windows.sh", "cli Makefile"} {
		home := t.TempDir()
		for _, script := range []string{"build-ur.sh", "build-ur-macos.sh", "build-ur-linux.sh", "build-ur-windows.sh", "ur/distro-smoke.sh", "macos-notarize.zsh"} {
			if script != missing {
				writeTestScript(t, filepath.Join(home, "all", script), "#!/bin/sh\n")
			}
		}
		copyTestFile(t, filepath.Join(rolloutRoot(t), "ur", "artifacts.txt"), filepath.Join(home, "all", "ur", "artifacts.txt"), 0o600)
		if missing != "cli Makefile" {
			copyTestContent(t, filepath.Join(home, "sidecar", "cli", "Makefile"), "build:\n\ttrue\n")
		}
		command := exec.Command("zsh", "-c", `builder_message() { print -r -- "$1" }
eval "$1"
ur_release_preflight`, "preflight", preflight)
		command.Env = environmentWith("BUILD_HOME=" + home)
		output, err := command.CombinedOutput()
		if err == nil {
			t.Errorf("the preflight passed without %s:\n%s", missing, output)
		}
	}
}
