// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Extract the production container function so the test exercises the same
// environment setup that Docker uses, rather than a test-only copy.
func fdroidContainerBuildFunction(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate F-Droid build contract test")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "build-fdroid.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?ms)^container_build\(\) \{\n.*?^\}\n`)
	matches := pattern.FindAllString(string(data), -1)
	if len(matches) != 1 {
		t.Fatalf("build-fdroid.sh container_build function count = %d, want 1", len(matches))
	}
	return matches[0]
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Reproduce the production base-image condition: no `go` is initially on
// PATH, while the downloaded tool exists under /usr/local/go. The fake Gradle
// process launches another /bin/sh, matching the gradle -> make -> go process
// boundary that failed in production, and proves the exported PATH reaches it.
func TestFdroidContainerExportsInstalledGoToSdkBuild(t *testing.T) {
	tempDir := t.TempDir()
	fakeGoRoot := filepath.Join(tempDir, "go")
	buildRoot := filepath.Join(tempDir, "build")
	androidApp := filepath.Join(buildRoot, "android", "app")
	home := filepath.Join(tempDir, "home")
	for _, directory := range []string{filepath.Join(fakeGoRoot, "bin"), androidApp, home} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeExecutable(t, filepath.Join(fakeGoRoot, "bin", "go"), `#!/bin/sh
printf '%s\n' 'go version go1.26.7 linux/amd64'
`)
	pathLog := filepath.Join(tempDir, "gradle-path")
	writeExecutable(t, filepath.Join(androidApp, "gradlew"), `#!/bin/sh
set -eu
[ "$*" = "clean buildSdk assembleGithub" ]
printf '%s\n' "$PATH" > "$FDROID_PATH_LOG"
[ "$(/bin/sh -c 'go version')" = "go version go1.26.7 linux/amd64" ]
`)

	// Remove every directory that already supplies Go while retaining the
	// ordinary base PATH. This makes the fixture independent of the host's Go
	// installation and accurately models the current F-Droid image.
	pathEntries := make([]string, 0)
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(directory, "go")); err == nil {
			continue
		}
		pathEntries = append(pathEntries, directory)
	}
	initialPath := strings.Join(pathEntries, string(os.PathListSeparator))

	functionSource := fdroidContainerBuildFunction(t)
	// Remap only the container's fixed mount/install locations into the test
	// directory. The function body and PATH behavior otherwise run verbatim.
	functionSource = strings.ReplaceAll(functionSource, "/usr/local/go", "${FAKE_GO_ROOT}")
	functionSource = strings.ReplaceAll(functionSource, "/urnetwork/build", "${FDROID_BUILD_ROOT}")
	functionSource = strings.ReplaceAll(functionSource, "/urnetwork", "${FDROID_ROOT}")

	harness := `
set -euo pipefail
sudo() { return 0; }
curl() { return 0; }
sdkmanager() { return 0; }
keytool() { return 0; }
java() { printf '%s\n' 'openjdk version "21.0.12"' >&2; }
mkdir() { /bin/mkdir "$@"; }
eval "$1"
container_build
`
	command := exec.Command("bash", "-c", harness, "fdroid-path-test", functionSource)
	environment := make([]string, 0, len(os.Environ())+6)
	for _, value := range os.Environ() {
		name := strings.SplitN(value, "=", 2)[0]
		switch name {
		case "PATH", "HOME", "FAKE_GO_ROOT", "FDROID_BUILD_ROOT", "FDROID_ROOT", "FDROID_PATH_LOG":
			continue
		}
		environment = append(environment, value)
	}
	command.Env = append(environment,
		"PATH="+initialPath,
		"HOME="+home,
		"FAKE_GO_ROOT="+fakeGoRoot,
		"FDROID_BUILD_ROOT="+buildRoot,
		"FDROID_ROOT="+tempDir,
		"FDROID_PATH_LOG="+pathLog,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("F-Droid container PATH contract failed: %v\n%s", err, output)
	}

	pathData, err := os.ReadFile(pathLog)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(fakeGoRoot, "bin") + string(os.PathListSeparator)
	if !strings.HasPrefix(strings.TrimSpace(string(pathData))+string(os.PathListSeparator), wantPrefix) {
		t.Fatalf("Gradle child PATH = %q, want installed Go first", strings.TrimSpace(string(pathData)))
	}
}
