//go:build ur_release_host

// SPDX-License-Identifier: MPL-2.0

// ur built for real on this host (REPO-M1-01, REPO-M1-02): build-ur.sh compiles the six targets from
// the sidecar checkout beside this one, checks their forks, runs TestHygieneReleaseBinary over them and
// runs this host's ur, with the three platform scripts stubbed, since signing, nfpm's image and the
// Windows VM belong to the release host. Then this host's ur reaches its git-remote-ur dispatch through
// a link, and on macOS an ad hoc signed ur.app, without the restricted entitlements, runs through a link
// as /usr/local/bin/ur will. It compiles ur seven times, so it stays out of plain go test:
//
//	cd all && go test -tags ur_release_host -timeout 30m -run UrOnThisHost .

package allbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// The sidecar checkout to build: build's populated submodule, or the monoroot's sibling of build.
func urHostSidecar(t *testing.T) string {
	t.Helper()
	root := rolloutRoot(t)
	if !filepath.IsAbs(root) {
		// A trimpath build records the module path. Go runs this package's tests in all/.
		var err error
		root, err = os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, candidate := range []string{filepath.Join(root, "..", "sidecar"), filepath.Join(root, "..", "..", "sidecar")} {
		if _, err := os.Stat(filepath.Join(candidate, "cli", "go.mod")); err == nil {
			absolute, err := filepath.Abs(candidate)
			if err != nil {
				t.Fatal(err)
			}
			return absolute
		}
	}
	t.Fatal("no sidecar checkout with cli/go.mod beside build")
	return ""
}

// The Windows archive's two filenames exercised by a native build: stock Git configuration points its
// transfer process at ur.exe, and the first Git connection starts that sibling with the daemon command.
func TestCopiedGitHelperOnThisHostRunsUrChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the cold-spawn argument probe uses a Unix script")
	}
	binary := buildHostUr(t)
	bin, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	helper, ur := filepath.Join(bin, "git-remote-ur.exe"), filepath.Join(bin, "ur.exe")
	copyTestFile(t, binary, helper, 0o700)
	copyTestFile(t, binary, ur, 0o700)
	// Keep the local socket path below sun_path's length on both host legs.
	runtimeDir, err := os.MkdirTemp("/tmp", "ur-git-copy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	gitDir := t.TempDir()
	environment := environmentWith("HOME="+t.TempDir(), "GIT_DIR="+gitDir, "XDG_STATE_HOME="+t.TempDir(),
		"TMPDIR="+runtimeDir, "XDG_RUNTIME_DIR="+runtimeDir, "UR_INSTANCE=copy-test")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	repo := "copy/id:00000000000000000000000000000001"
	invoke := func(input string) *exec.Cmd {
		command := exec.CommandContext(ctx, helper, "origin", "ur://"+repo)
		command.Env, command.Stdin = environment, strings.NewReader(input)
		return command
	}
	if output, err := invoke("capabilities\n\n").CombinedOutput(); err != nil {
		t.Fatalf("helper capabilities: %v\n%s", err, output)
	}
	git := exec.CommandContext(ctx, "git", "config", "--file", filepath.Join(gitDir, "config"), "lfs.customtransfer.ur.path")
	value, err := git.Output()
	if err != nil {
		t.Fatal(err)
	}
	agentPath := strings.TrimSpace(string(value))
	if agentPath != ur {
		t.Fatalf("configured LFS executable %q, want %q", agentPath, ur)
	}
	agent := exec.CommandContext(ctx, agentPath, "lfs-agent", "--repo="+repo)
	agent.Env = environment
	agent.Stdin = strings.NewReader("{\"event\":\"init\",\"operation\":\"download\"}\n{\"event\":\"terminate\"}\n")
	if output, err := agent.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "{}" {
		t.Fatalf("configured LFS agent: %v\n%s", err, output)
	}
	// The probe exits immediately after recording the actual detached child's arguments; the helper then
	// reaches its ordinary cold-start timeout on the private socket. No system daemon or keychain is opened.
	probe := filepath.Join(bin, "spawned")
	writeTestScript(t, ur, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$UR_TEST_SPAWN_ARGS\"\n")
	environment = append(environment, "UR_TEST_SPAWN_ARGS="+probe)
	if output, err := invoke("connect git-upload-pack\n").CombinedOutput(); err == nil || !strings.Contains(string(output), "did not answer") {
		t.Fatalf("cold Git session: %v\n%s", err, output)
	}
	spawned, err := os.ReadFile(probe)
	if err != nil || string(spawned) != "daemon\n--instance\ncopy-test\n--auto-spawned\n" {
		t.Fatalf("detached ur.exe: %q, %v", spawned, err)
	}
}

// A build home whose siblings are links to the real checkouts beside the sidecar, so cli's replaces
// resolve, and whose all/ holds the real build-ur.sh with the platform scripts stubbed.
func newUrHostBuildHome(t *testing.T) *urBuildHome {
	t.Helper()
	directory := t.TempDir()
	home := &urBuildHome{t: t, directory: directory, bin: filepath.Join(directory, "stub-bin"), log: filepath.Join(directory, "stub.log"), out: filepath.Join(directory, "out", "ur")}
	sidecar := urHostSidecar(t)
	entries, err := os.ReadDir(filepath.Dir(sidecar))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "all" || entry.Name() == "build" || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := os.Symlink(filepath.Join(filepath.Dir(sidecar), entry.Name()), filepath.Join(directory, entry.Name())); err != nil {
			t.Fatal(err)
		}
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
	return home
}

func TestBuildUrOnThisHost(t *testing.T) {
	home := newUrHostBuildHome(t)
	code, output := home.run("GO=go", "MAKE=make")
	if code != 0 {
		t.Fatalf("build-ur.sh failed on this host: %d\n%s", code, output)
	}
	if written := home.written(); !slices.Equal(written, urReadingNineNames(urTestVersion)) {
		t.Errorf("build-ur.sh wrote %v", written)
	}
	if !strings.Contains(output, ">>> "+runtime.GOOS+"/"+runtime.GOARCH+": ur ") {
		t.Errorf("this host's ur did not print its version line:\n%s", output)
	}
	t.Logf("build-ur.sh on this host:\n%s", output)
}

// This host's ur, built as the release builds it.
func buildHostUr(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	command := exec.Command("make", "-C", filepath.Join(urHostSidecar(t), "cli"), "build", "OUT="+out, "TARGETS="+runtime.GOOS+"/"+runtime.GOARCH)
	command.Env = environmentWith("GOFLAGS=")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("make build: %v\n%s", err, output)
	}
	return filepath.Join(out, runtime.GOOS, runtime.GOARCH, "ur")
}

// Run as git-remote-ur through a link, ur goes to its remote-helper dispatch on argv[0], never to its own
// commands: until EVS1-M2-38 registers the helper, the dispatch says it is not built yet.
func TestGitRemoteUrOnThisHostReachesTheDispatchThroughALink(t *testing.T) {
	binary := buildHostUr(t)
	link := filepath.Join(t.TempDir(), "git-remote-ur")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	output, _ := exec.Command(link, "origin", "ur://example.invalid/repo").CombinedOutput()
	if strings.Contains(string(output), "Usage: ur") || !strings.Contains(string(output), "git-remote-ur") {
		t.Errorf("git-remote-ur through a link did not reach the dispatch:\n%s", output)
	}
	t.Logf("git-remote-ur through a link: %s", strings.TrimSpace(string(output)))
}

// REPO-M1-02: a local ur.app, ad hoc signed without the restricted entitlements, which need the
// Developer ID profile this host does not hold, runs `ur --version` through a link.
func TestUrBundleOnThisHostRunsThroughALink(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("ur.app is macOS's")
	}
	binary := buildHostUr(t)
	app := filepath.Join(t.TempDir(), "ur.app")
	plist, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur", "macos", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.NewReplacer("@VERSION_BASE@", "2026.11.6", "@VERSION_RELEASE@", "1234567").Replace(string(plist))
	copyTestContent(t, filepath.Join(app, "Contents", "Info.plist"), rendered)
	copyTestFile(t, binary, filepath.Join(app, "Contents", "MacOS", "ur"), 0o755)
	if output, err := exec.Command("codesign", "--force", "--options", "runtime", "--sign", "-", app).CombinedOutput(); err != nil {
		t.Fatalf("ad hoc codesign: %v\n%s", err, output)
	}
	if output, err := exec.Command("codesign", "--verify", "--strict", app).CombinedOutput(); err != nil {
		t.Fatalf("codesign --verify: %v\n%s", err, output)
	}
	link := filepath.Join(t.TempDir(), "ur")
	if err := os.Symlink(filepath.Join(app, "Contents", "MacOS", "ur"), link); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(link, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(output), "ur ") {
		t.Fatalf("ur.app through a link: %v\n%s", err, output)
	}
}
