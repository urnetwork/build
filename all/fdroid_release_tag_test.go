// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// F-Droid tracks v<version>-fdroid tags. Exercise the production helper
// against a local origin: the tag must peel to the -ungoogle commit (not the
// branch head). Exact re-observation is safe; a conflicting target must fail.
func TestGitTagFdroidAliasesUngoogleCommit(t *testing.T) {
	tempDir := t.TempDir()
	remote := filepath.Join(tempDir, "remote.git")
	repository := filepath.Join(tempDir, "build")
	version := "2026.9.7-9999999990"
	runGit(t, tempDir, "init", "--bare", remote)
	runGit(t, tempDir, "init", "-b", "main", repository)
	runGit(t, repository, "config", "user.name", "Build Harness Test")
	runGit(t, repository, "config", "user.email", "build-harness-test@example.invalid")
	commitTestFile(t, repository, "release", "release\n", "release tree")
	runGit(t, repository, "remote", "add", "origin", remote)
	runGit(t, repository, "push", "-u", "origin", "main")
	ungoogleCommit := commitTestFile(t, repository, "ungoogle", "ungoogle\n", "builder build ungoogle")
	runGit(t, repository, "tag", "-a", "v"+version+"-ungoogle", "-m", version+"-ungoogle")
	runGit(t, repository, "push", "origin", "refs/tags/v"+version+"-ungoogle")
	// the android pin is restored after the ungoogle build, moving main on
	commitTestFile(t, repository, "restore", "restore\n", "restore android pin")

	harness := `
set -u
EXTERNAL_WARP_VERSION="$1"
builder_message() { print -r -- "$*" }
eval "$2"
git_tag_fdroid
`
	run := func() (string, error) {
		command := exec.Command("zsh", "-c", harness, "fdroid-tag-test", version, runFunction(t, "git_tag_fdroid"))
		command.Dir = repository
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if output, err := run(); err != nil {
		t.Fatalf("fdroid tag publication failed: %v\n%s", err, output)
	}
	tagRef := "refs/tags/v" + version + "-fdroid"
	remoteCommit := runGit(t, tempDir, "--git-dir", remote, "rev-parse", tagRef+"^{commit}")
	if remoteCommit != ungoogleCommit {
		t.Fatalf("fdroid tag peels to %s, want the -ungoogle commit %s", remoteCommit, ungoogleCommit)
	}
	if objectType := runGit(t, tempDir, "--git-dir", remote, "cat-file", "-t", tagRef); objectType != "tag" {
		t.Fatalf("fdroid tag object type = %q, want an annotated tag", objectType)
	}

	before := runGit(t, repository, "ls-remote", "--tags", "origin", tagRef, tagRef+"^{}")
	output, err := run()
	if err != nil {
		t.Fatalf("verified identical fdroid tag could not resume: %v, output=%q", err, output)
	}
	if after := runGit(t, repository, "ls-remote", "--tags", "origin", tagRef, tagRef+"^{}"); after != before {
		t.Fatalf("published fdroid tag moved:\nbefore: %s\nafter:  %s", before, after)
	}
	// A different local ungoogle target must never move the published marker.
	runGit(t, repository, "tag", "-f", "v"+version+"-ungoogle", "HEAD")
	if output, err := run(); err == nil || !strings.Contains(output, "refusing to overwrite") {
		t.Fatalf("conflicting fdroid tag was accepted: %v, output=%q", err, output)
	}
	if after := runGit(t, repository, "ls-remote", "--tags", "origin", tagRef, tagRef+"^{}"); after != before {
		t.Fatal("conflicting retry moved the published fdroid tag")
	}
}

// The -fdroid tag is the only tag F-Droid should build from, so it must be
// pushed after the last APK release it verifies against, and never before a
// build or upload that can still fail.
func TestRunPushesFdroidTagAfterApkReleases(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(runData)

	call := "(cd $BUILD_HOME &&\n    git_tag_fdroid)\nerror_trap 'push fdroid tag'"
	if got := strings.Count(source, call); got != 1 {
		t.Fatalf("run.sh fdroid tag publication count = %d, want 1", got)
	}
	tagIndex := strings.Index(source, call)
	for _, before := range []string{
		"error_trap 'push ungoogle tag'",
		"error_trap 'android github build'",
		`"com.bringyour.network-${EXTERNAL_WARP_VERSION}.apk"`,
		"\ngithub_create_release\n",
		"error_trap 'android github armeabi-v7a reproducible pre-release'",
		"error_trap 'android github arm64-v8a reproducible pre-release'",
	} {
		index := strings.Index(source, before)
		if index < 0 {
			t.Fatalf("run.sh is missing release step %q", before)
		}
		if index > tagIndex {
			t.Errorf("fdroid tag is pushed before release step %q", before)
		}
	}
	sectionEnd := strings.Index(source[tagIndex:], "# Warp services")
	if sectionEnd < 0 {
		t.Fatal("run.sh has no Warp services section after the fdroid tag")
	}
	androidRest := source[tagIndex : tagIndex+sectionEnd]
	for _, after := range []string{"github_release_upload", "github_create_release", "gradlew"} {
		if strings.Contains(androidRest, after) {
			t.Errorf("run.sh still runs %q after pushing the fdroid tag", after)
		}
	}
}

// fdroidGoLibrary builds a small linux/arm64 Go program the way gomobile builds
// libgojni.so: its generated go.mod replaces a module with an absolute local
// directory under root (here root/connect/sctp, as the sdk does for
// github.com/pion/sctp), with -trimpath on. -trimpath leaves that directory in
// the module info, exactly as it does in libgojni.so.
func fdroidGoLibrary(t *testing.T, root string) []byte {
	t.Helper()
	work := t.TempDir()
	dependency := filepath.Join(root, "connect", "sctp")
	gobind := filepath.Join(work, "gobind")
	files := map[string]string{
		filepath.Join(dependency, "go.mod"):  "module github.com/pion/sctp\n\ngo 1.22\n",
		filepath.Join(dependency, "sctp.go"): "package sctp\n\nconst Name = \"sctp\"\n",
		filepath.Join(gobind, "go.mod"): "module gobind\n\ngo 1.22\n\nrequire github.com/pion/sctp v1.8.39\n\n" +
			"replace github.com/pion/sctp => " + dependency + "\n",
		filepath.Join(gobind, "main.go"): "package main\n\nimport \"github.com/pion/sctp\"\n\nfunc main() { println(sctp.Name) }\n",
	}
	for path, contents := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(work, "libgojni.so")
	command := exec.Command("go", "build", "-trimpath", "-o", output, ".")
	command.Dir = gobind
	command.Env = append(os.Environ(),
		"GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0",
		"GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off", "GOTOOLCHAIN=local")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Go library at %s: %v\n%s", root, err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFdroidTestApk(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, data := range entries {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// F-Droid rebuilds at its build path and compares byte for byte, and gomobile's
// absolute replace paths put the build tree's path into libgojni.so. The 10-18
// APKs were built at /urnetwork/build and F-Droid's rebuild could not match
// them. Exercise the production check on real Go module info: only APKs whose
// every library was built under F-Droid's path may pass.
func TestFdroidBuildPathCheckRejectsApksBuiltElsewhere(t *testing.T) {
	tempDir := t.TempDir()
	fdroidRoot := filepath.Join(tempDir, "home", "vagrant", "build", "com.bringyour.network")
	elsewhereRoot := filepath.Join(tempDir, "urnetwork", "build")
	atFdroidPath := fdroidGoLibrary(t, fdroidRoot)
	elsewhere := fdroidGoLibrary(t, elsewhereRoot)

	apk := func(name string, entries map[string][]byte) string {
		path := filepath.Join(tempDir, name)
		writeFdroidTestApk(t, path, entries)
		return path
	}
	good := apk("good.apk", map[string][]byte{
		"classes.dex":                 []byte("dex"),
		"lib/armeabi-v7a/libgojni.so": atFdroidPath,
		"lib/arm64-v8a/libgojni.so":   atFdroidPath,
	})
	bad := apk("bad.apk", map[string][]byte{
		"classes.dex":                 []byte("dex"),
		"lib/armeabi-v7a/libgojni.so": elsewhere,
	})
	oneBadAbi := apk("one-bad-abi.apk", map[string][]byte{
		"lib/armeabi-v7a/libgojni.so": atFdroidPath,
		"lib/arm64-v8a/libgojni.so":   elsewhere,
	})
	noLibrary := apk("no-library.apk", map[string][]byte{"classes.dex": []byte("dex")})
	notGo := apk("not-go.apk", map[string][]byte{"lib/arm64-v8a/libgojni.so": []byte("not a Go binary")})

	harness := `
set -u
FDROID_BUILD_PATH="$1"
builder_message() { print -r -- "$*" }
eval "$2"
shift 2
fdroid_build_path_check "$@"
`
	check := func(apks ...string) (string, error) {
		arguments := append([]string{"-c", harness, "fdroid-build-path-test", fdroidRoot, runFunction(t, "fdroid_build_path_check")}, apks...)
		command := exec.Command("zsh", arguments...)
		command.Dir = tempDir
		command.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
		output, err := command.CombinedOutput()
		return string(output), err
	}

	if output, err := check(good); err != nil {
		t.Fatalf("APK built at F-Droid's path was rejected: %v\n%s", err, output)
	}
	elsewhereSctp := filepath.Join(elsewhereRoot, "connect", "sctp")
	for name, apks := range map[string][]string{
		"built elsewhere":      {bad},
		"good then elsewhere":  {good, bad},
		"elsewhere then good":  {bad, good},
		"one ABI elsewhere":    {oneBadAbi},
		"no libgojni.so":       {noLibrary},
		"not a Go library":     {notGo},
		"missing APK":          {filepath.Join(tempDir, "missing.apk")},
		"missing then good":    {filepath.Join(tempDir, "missing.apk"), good},
		"elsewhere among many": {good, good, oneBadAbi, good},
	} {
		output, err := check(apks...)
		if err == nil {
			t.Errorf("%s: check passed, want a failure\n%s", name, output)
			continue
		}
		if strings.Contains(name, "elsewhere") || strings.Contains(name, "ABI") {
			if !strings.Contains(output, elsewhereSctp) || !strings.Contains(output, "outside F-Droid's build path") {
				t.Errorf("%s: output does not name the foreign build path %s:\n%s", name, elsewhereSctp, output)
			}
		}
	}
	if _, err := check(); err == nil {
		t.Error("check with no APKs passed, want a failure")
	}
}

// run.sh's check and all/build-fdroid.sh must agree on F-Droid's build path,
// or every release would either fail the check or build where F-Droid does not.
func TestFdroidBuildPathCheckMatchesTheContainerBuildPath(t *testing.T) {
	const fdroidBuildPath = "/home/vagrant/build/com.bringyour.network"
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`(?m)^FDROID_BUILD_PATH=(.*)$`).FindAllStringSubmatch(string(runData), -1)
	if len(matches) != 1 || matches[0][1] != fdroidBuildPath {
		t.Fatalf("run.sh FDROID_BUILD_PATH assignments = %q, want exactly one, %s", matches, fdroidBuildPath)
	}
	scriptData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "build-fdroid.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`-v "$BUILD_HOME:` + fdroidBuildPath + `:Z"`,
		"export BRINGYOUR_HOME=" + fdroidBuildPath + "\n",
	} {
		if !strings.Contains(string(scriptData), want) {
			t.Errorf("build-fdroid.sh does not contain %q", want)
		}
	}
}

// The -fdroid tag asks F-Droid to rebuild and verify this version, so it may be
// pushed only when the per-ABI APKs F-Droid compares against pass the build
// path check. A failed check must leave the published releases alone.
func TestRunTagsFdroidOnlyWhenItsApksWereBuiltAtFdroidsPath(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(runData)
	apkDir := "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/"
	gate := "if fdroid_build_path_check \\\n" +
		"    \"" + apkDir + "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-armeabi-v7a-release.apk\" \\\n" +
		"    \"" + apkDir + "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-arm64-v8a-release.apk\"; then\n" +
		"(cd $BUILD_HOME &&\n    git_tag_fdroid)\nerror_trap 'push fdroid tag'\n"
	if got := strings.Count(source, gate); got != 1 {
		t.Fatalf("run.sh gated fdroid tag publication count = %d, want 1", got)
	}
	if got := strings.Count(source, "git_tag_fdroid)"); got != 1 {
		t.Fatalf("run.sh calls git_tag_fdroid %d times, want once, behind the check", got)
	}
	// The per-ABI pre-releases upload exactly the APKs the gate checks.
	for _, abi := range []string{"armeabi-v7a", "arm64-v8a"} {
		upload := "\"" + apkDir + "com.bringyour.network-${BASE_EXTERNAL_WARP_VERSION}-github-" + abi + "-release.apk\""
		if !strings.Contains(source, upload) {
			t.Errorf("run.sh no longer uploads %s, which the gate checks", upload)
		}
	}
	gateIndex := strings.Index(source, gate)
	rest := source[gateIndex+len(gate):]
	elseIndex := strings.Index(rest, "\nelse\n")
	fiIndex := strings.Index(rest, "\nfi\n")
	if elseIndex < 0 || fiIndex < elseIndex {
		t.Fatal("the fdroid tag gate has no else branch before its fi")
	}
	otherwise := rest[elseIndex:fiIndex]
	if !strings.Contains(otherwise, "builder_message \"warning: no \\`v${EXTERNAL_WARP_VERSION}-fdroid\\` tag") {
		t.Errorf("a failed check does not report the missing tag: %q", otherwise)
	}
	for _, forbidden := range []string{"exit", "error_trap", "git_tag", "github_"} {
		if strings.Contains(otherwise, forbidden) {
			t.Errorf("a failed check runs %q; it must only report", forbidden)
		}
	}
}
