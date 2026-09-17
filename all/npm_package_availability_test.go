// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type npmReadinessResult struct {
	exitCode int
	stdout   string
	stderr   string
	calls    []string
	sleeps   []string
}

func runNPMReadiness(t *testing.T, mode, packageName, version string, attempts int) npmReadinessResult {
	t.Helper()
	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeNPM := `#!/bin/sh
set -u
printf '%s\n' "$*" >> "$NPM_CALL_LOG"
count=0
if [ -f "$NPM_COUNT_FILE" ]; then
    IFS= read -r count < "$NPM_COUNT_FILE"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$NPM_COUNT_FILE"
case "$NPM_TEST_MODE" in
    delayed)
        if [ "$count" -eq 1 ]; then
            printf 'npm error code ETARGET\nnpm error notarget No matching version found for %s.\n' "$NPM_EXPECTED_SPEC" >&2
            exit 1
        fi
        if [ "$count" -eq 2 ]; then
            printf 'npm error code E404\nnpm error 404 Not Found - GET https://registry.npmjs.test/package/-/package-%s.tgz\n' "$NPM_EXPECTED_VERSION" >&2
            exit 1
        fi
        ;;
    persistent)
        printf 'npm error code ETARGET\nnpm error notarget No matching version found for %s.\n' "$NPM_EXPECTED_SPEC" >&2
        exit 1
        ;;
    auth)
        printf 'npm error code E401\nnpm error Incorrect or missing password.\n' >&2
        exit 17
        ;;
    server)
        printf 'npm error code E500\nnpm error Internal registry failure.\n' >&2
        exit 19
        ;;
    unrelated-404)
        printf 'npm error code E404\nnpm error 404 Not Found - GET https://registry.npmjs.test/unrelated\n' >&2
        exit 18
        ;;
    wrong-version)
        printf '[{"name":"%s","version":"0.0.0","filename":"wrong.tgz"}]\n' "$NPM_EXPECTED_PACKAGE"
        exit 0
        ;;
esac
printf '[{"name":"%s","version":"%s","filename":"package-%s.tgz"}]\n' \
    "$NPM_EXPECTED_PACKAGE" "$NPM_EXPECTED_VERSION" "$NPM_EXPECTED_VERSION"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(fakeNPM), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSleep := `#!/bin/sh
printf '%s\n' "$1" >> "$NPM_SLEEP_LOG"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "sleep"), []byte(fakeSleep), 0o755); err != nil {
		t.Fatal(err)
	}

	callLog := filepath.Join(tempDir, "npm-calls")
	sleepLog := filepath.Join(tempDir, "sleeps")
	exactSpec := packageName + "@" + version
	command := exec.Command(filepath.Join(rolloutRoot(t), "npm-package-ready.zsh"), packageName, version)
	environment := make([]string, 0, len(os.Environ())+10)
	for _, value := range os.Environ() {
		name := strings.SplitN(value, "=", 2)[0]
		switch name {
		case "NPM_CALL_LOG", "NPM_COUNT_FILE", "NPM_EXPECTED_PACKAGE", "NPM_EXPECTED_SPEC", "NPM_EXPECTED_VERSION", "NPM_PACKAGE_READY_MAX_ATTEMPTS", "NPM_PACKAGE_READY_RETRY_DELAY_SECONDS", "NPM_SLEEP_LOG", "NPM_TEST_MODE", "PATH":
			continue
		}
		environment = append(environment, value)
	}
	command.Env = append(environment,
		"NPM_CALL_LOG="+callLog,
		"NPM_COUNT_FILE="+filepath.Join(tempDir, "npm-count"),
		"NPM_EXPECTED_PACKAGE="+packageName,
		"NPM_EXPECTED_SPEC="+exactSpec,
		"NPM_EXPECTED_VERSION="+version,
		fmt.Sprintf("NPM_PACKAGE_READY_MAX_ATTEMPTS=%d", attempts),
		"NPM_PACKAGE_READY_RETRY_DELAY_SECONDS=0",
		"NPM_SLEEP_LOG="+sleepLog,
		"NPM_TEST_MODE="+mode,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatal(err)
		}
		exitCode = exitError.ExitCode()
	}

	readLines := func(path string) []string {
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			return nil
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		trimmed := strings.TrimSuffix(string(data), "\n")
		if trimmed == "" {
			return nil
		}
		return strings.Split(trimmed, "\n")
	}

	return npmReadinessResult{
		exitCode: exitCode,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		calls:    readLines(callLog),
		sleeps:   readLines(sleepLog),
	}
}

func TestNPMPackageReadinessWaitsForMetadataAndTarballPropagation(t *testing.T) {
	result := runNPMReadiness(t, "delayed", "@urnetwork/localizations", "2026.9.17-1048721260", 5)
	if result.exitCode != 0 {
		t.Fatalf("delayed readiness exit = %d\nstdout=%s\nstderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	if len(result.calls) != 3 || len(result.sleeps) != 2 {
		t.Fatalf("delayed readiness calls=%d sleeps=%v, want 3 calls and 2 sleeps", len(result.calls), result.sleeps)
	}
	if !strings.Contains(result.stdout, "metadata and tarball verified") {
		t.Fatalf("success diagnostic does not confirm tarball: %s", result.stdout)
	}
}

func TestNPMPackageReadinessPersistentETARGETFailsAtBound(t *testing.T) {
	result := runNPMReadiness(t, "persistent", "@urnetwork/localizations", "2026.9.17-1048721260", 3)
	if result.exitCode != 1 || len(result.calls) != 3 || len(result.sleeps) != 2 {
		t.Fatalf("persistent ETARGET result = %+v, want exit 1 after 3 attempts", result)
	}
	if !strings.Contains(result.stderr, "still unavailable after 3 attempts") {
		t.Fatalf("persistent ETARGET diagnostic missing bound: %s", result.stderr)
	}
}

func TestNPMPackageReadinessUnrelatedErrorsAreImmediateAndFatal(t *testing.T) {
	for _, testCase := range []struct {
		mode      string
		exitCode  int
		errorCode string
	}{
		{mode: "auth", exitCode: 17, errorCode: "E401"},
		{mode: "server", exitCode: 19, errorCode: "E500"},
		{mode: "unrelated-404", exitCode: 18, errorCode: "E404"},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			result := runNPMReadiness(t, testCase.mode, "@urnetwork/localizations", "2026.9.17-1048721260", 5)
			if result.exitCode != testCase.exitCode || len(result.calls) != 1 || len(result.sleeps) != 0 {
				t.Fatalf("unrelated error result = %+v, want immediate exit %d", result, testCase.exitCode)
			}
			for _, want := range []string{"not retrying a non-propagation error", "code " + testCase.errorCode} {
				if !strings.Contains(result.stderr, want) {
					t.Errorf("unrelated error diagnostic lacks %q: %s", want, result.stderr)
				}
			}
		})
	}
}

func TestNPMPackageReadinessRequestsAndValidatesExactVersion(t *testing.T) {
	const packageName = "@urnetwork/localizations"
	const version = "2026.9.17-1048721260"
	result := runNPMReadiness(t, "success", packageName, version, 2)
	if result.exitCode != 0 || len(result.calls) != 1 {
		t.Fatalf("exact-version readiness result = %+v", result)
	}
	call := result.calls[0]
	for _, want := range []string{"pack " + packageName + "@" + version, "--dry-run", "--json", "--ignore-scripts"} {
		if !strings.Contains(call, want) {
			t.Errorf("npm call %q lacks %q", call, want)
		}
	}

	wrong := runNPMReadiness(t, "wrong-version", packageName, version, 2)
	if wrong.exitCode != 65 || len(wrong.calls) != 1 || len(wrong.sleeps) != 0 {
		t.Fatalf("wrong-version result = %+v, want immediate data failure", wrong)
	}
	if !strings.Contains(wrong.stderr, "without the requested package and version") {
		t.Fatalf("wrong-version diagnostic missing: %s", wrong.stderr)
	}
}

func TestRunWaitsForBothExactNPMDependenciesBeforeExtensionEdit(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSource := string(runData)
	if strings.Count(runSource, `"$BUILD_HOME/all/npm-package-ready.zsh"`) != 2 {
		t.Fatal("run.sh must use the tested npm readiness helper for both extension dependencies")
	}
	waitStart := strings.Index(runSource, `@urnetwork/localizations "$EXTERNAL_WARP_VERSION"`)
	sdkWait := strings.Index(runSource, `@urnetwork/sdk-js "$EXTERNAL_WARP_VERSION"`)
	extensionEdit := strings.Index(runSource, "npm_edit_module @urnetwork/localizations")
	if waitStart < 0 || sdkWait <= waitStart || extensionEdit <= sdkWait {
		t.Fatalf("run.sh does not wait for both exact npm packages before extension edit")
	}
	if strings.Contains(runSource, "sleep 30\n\n\n(cd $BUILD_HOME/extension") {
		t.Fatal("run.sh still relies on the fixed npm propagation sleep")
	}
}
