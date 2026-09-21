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
	result := runNPMReadiness(t, "delayed", "@urnetwork/localizations", "2026.9.17-1048721260", 3)
	if result.exitCode != 0 {
		t.Fatalf("delayed readiness exit = %d\nstdout=%s\nstderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	if len(result.calls) != 3 || len(result.sleeps) != 2 {
		t.Fatalf("delayed readiness calls=%d sleeps=%v, want 3 calls and 2 sleeps", len(result.calls), result.sleeps)
	}
	if !strings.Contains(result.stdout, "metadata and tarball verified") {
		t.Fatalf("success diagnostic does not confirm tarball: %s", result.stdout)
	}
	cachePaths := make(map[string]struct{}, len(result.calls))
	for callNumber, call := range result.calls {
		fields := strings.Fields(call)
		cachePath := ""
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] == "--cache" {
				cachePath = fields[index+1]
				break
			}
		}
		if cachePath == "" {
			t.Fatalf("npm call %d has no explicit cache: %q", callNumber+1, call)
		}
		if _, reused := cachePaths[cachePath]; reused {
			t.Fatalf("npm readiness reused cache %q across attempts: %v", cachePath, result.calls)
		}
		cachePaths[cachePath] = struct{}{}
	}
}

func TestNPMPackageReadinessPersistentETARGETFailsAtBound(t *testing.T) {
	result := runNPMReadiness(t, "persistent", "@urnetwork/localizations", "2026.9.17-1048721260", 3)
	if result.exitCode != 75 || len(result.calls) != 3 || len(result.sleeps) != 2 {
		t.Fatalf("persistent ETARGET result = %+v, want EX_TEMPFAIL after 3 attempts", result)
	}
	if !strings.Contains(result.stderr, "still unavailable after 3 attempts") {
		t.Fatalf("persistent ETARGET diagnostic missing bound: %s", result.stderr)
	}
}

type npmPublishReadinessResult struct {
	exitCode int
	stdout   string
	stderr   string
	calls    []string
}

func runNPMPublishReadiness(t *testing.T, mode string, publishAttempts, readinessAttempts int) npmPublishReadinessResult {
	t.Helper()
	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeNPM := `#!/bin/sh
set -u
printf '%s\n' "$*" >> "$NPM_CALL_LOG"
case "$1" in
    publish)
        count=0
        if [ -f "$NPM_PUBLISH_COUNT_FILE" ]; then
            IFS= read -r count < "$NPM_PUBLISH_COUNT_FILE"
        fi
        count=$((count + 1))
        printf '%s\n' "$count" > "$NPM_PUBLISH_COUNT_FILE"
        case "$NPM_TEST_MODE" in
            initial-publish-failure)
                printf 'npm error code E401\n' >&2
                exit 17
                ;;
            resubmit-conflict)
                if [ "$count" -eq 2 ]; then
                    printf 'npm error code EPUBLISHCONFLICT\n' >&2
                    exit 23
                fi
                ;;
        esac
        exit 0
        ;;
    --cache)
        count=0
        if [ -f "$NPM_PACK_COUNT_FILE" ]; then
            IFS= read -r count < "$NPM_PACK_COUNT_FILE"
        fi
        count=$((count + 1))
        printf '%s\n' "$count" > "$NPM_PACK_COUNT_FILE"
        publish_count=0
        if [ -f "$NPM_PUBLISH_COUNT_FILE" ]; then
            IFS= read -r publish_count < "$NPM_PUBLISH_COUNT_FILE"
        fi
        case "$NPM_TEST_MODE" in
            dropped-then-ready|resubmit-conflict)
                if [ "$publish_count" -lt 2 ]; then
                    printf 'npm error code ETARGET\nnpm error notarget No matching version found for %s.\n' "$NPM_EXPECTED_SPEC" >&2
                    exit 1
                fi
                ;;
            dropped-both)
                printf 'npm error code ETARGET\nnpm error notarget No matching version found for %s.\n' "$NPM_EXPECTED_SPEC" >&2
                exit 1
                ;;
            readiness-auth)
                printf 'npm error code E401\nnpm error Incorrect or missing password.\n' >&2
                exit 19
                ;;
        esac
        printf '[{"name":"%s","version":"%s","filename":"package-%s.tgz"}]\n' \
            "$NPM_EXPECTED_PACKAGE" "$NPM_EXPECTED_VERSION" "$NPM_EXPECTED_VERSION"
        exit 0
        ;;
esac
exit 91
`
	if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(fakeNPM), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSleep := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "sleep"), []byte(fakeSleep), 0o755); err != nil {
		t.Fatal(err)
	}

	const packageName = "@urnetwork/localizations"
	const version = "2026.9.18-1049683510"
	callLog := filepath.Join(tempDir, "npm-calls")
	command := exec.Command(
		filepath.Join(rolloutRoot(t), "npm-publish-ready.zsh"),
		packageName, version, "npm", "publish", "--tag", "nightly",
	)
	environment := make([]string, 0, len(os.Environ())+12)
	for _, value := range os.Environ() {
		name := strings.SplitN(value, "=", 2)[0]
		switch name {
		case "NPM_CALL_LOG", "NPM_EXPECTED_PACKAGE", "NPM_EXPECTED_SPEC", "NPM_EXPECTED_VERSION", "NPM_PACK_COUNT_FILE", "NPM_PACKAGE_READY_MAX_ATTEMPTS", "NPM_PACKAGE_READY_RETRY_DELAY_SECONDS", "NPM_PUBLISH_COUNT_FILE", "NPM_PUBLISH_READY_MAX_ATTEMPTS", "NPM_TEST_MODE", "PATH":
			continue
		}
		environment = append(environment, value)
	}
	command.Env = append(environment,
		"NPM_CALL_LOG="+callLog,
		"NPM_EXPECTED_PACKAGE="+packageName,
		"NPM_EXPECTED_SPEC="+packageName+"@"+version,
		"NPM_EXPECTED_VERSION="+version,
		"NPM_PACK_COUNT_FILE="+filepath.Join(tempDir, "pack-count"),
		fmt.Sprintf("NPM_PACKAGE_READY_MAX_ATTEMPTS=%d", readinessAttempts),
		"NPM_PACKAGE_READY_RETRY_DELAY_SECONDS=0",
		"NPM_PUBLISH_COUNT_FILE="+filepath.Join(tempDir, "publish-count"),
		fmt.Sprintf("NPM_PUBLISH_READY_MAX_ATTEMPTS=%d", publishAttempts),
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
	data, readErr := os.ReadFile(callLog)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	var calls []string
	if trimmed := strings.TrimSuffix(string(data), "\n"); trimmed != "" {
		calls = strings.Split(trimmed, "\n")
	}
	return npmPublishReadinessResult{
		exitCode: exitCode,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		calls:    calls,
	}
}

func countNPMCallsWithPrefix(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func TestNPMPublishReadinessResubmitsDroppedAcceptedPublish(t *testing.T) {
	result := runNPMPublishReadiness(t, "dropped-then-ready", 2, 2)
	if result.exitCode != 0 {
		t.Fatalf("dropped publish exit = %d\nstdout=%s\nstderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	if got := countNPMCallsWithPrefix(result.calls, "publish "); got != 2 {
		t.Fatalf("publish calls = %d, want 2: %v", got, result.calls)
	}
	if got := countNPMCallsWithPrefix(result.calls, "--cache "); got != 3 {
		t.Fatalf("readiness calls = %d, want 3: %v", got, result.calls)
	}
	if !strings.Contains(result.stderr, "accepted publish did not complete; resubmitting (2/2)") {
		t.Fatalf("resubmission diagnostic missing: %s", result.stderr)
	}
}

func TestNPMPublishReadinessDoesNotRetryHardFailures(t *testing.T) {
	for _, testCase := range []struct {
		mode       string
		exitCode   int
		packCalls  int
		wantStderr string
	}{
		{mode: "initial-publish-failure", exitCode: 17, packCalls: 0, wantStderr: "initial publish failed"},
		{mode: "readiness-auth", exitCode: 19, packCalls: 1, wantStderr: "not resubmitting"},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			result := runNPMPublishReadiness(t, testCase.mode, 2, 2)
			if result.exitCode != testCase.exitCode {
				t.Fatalf("exit = %d, want %d\n%s", result.exitCode, testCase.exitCode, result.stderr)
			}
			if got := countNPMCallsWithPrefix(result.calls, "publish "); got != 1 {
				t.Fatalf("publish calls = %d, want 1: %v", got, result.calls)
			}
			if got := countNPMCallsWithPrefix(result.calls, "--cache "); got != testCase.packCalls {
				t.Fatalf("readiness calls = %d, want %d: %v", got, testCase.packCalls, result.calls)
			}
			if !strings.Contains(result.stderr, testCase.wantStderr) {
				t.Fatalf("diagnostic lacks %q: %s", testCase.wantStderr, result.stderr)
			}
		})
	}
}

func TestNPMPublishReadinessAcceptsOriginalAfterResubmitRace(t *testing.T) {
	result := runNPMPublishReadiness(t, "resubmit-conflict", 2, 2)
	if result.exitCode != 0 {
		t.Fatalf("resubmit race exit = %d\nstdout=%s\nstderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	if got := countNPMCallsWithPrefix(result.calls, "publish "); got != 2 {
		t.Fatalf("publish calls = %d, want 2: %v", got, result.calls)
	}
	if !strings.Contains(result.stderr, "checking whether the accepted publish completed") {
		t.Fatalf("race diagnostic missing: %s", result.stderr)
	}
}

func TestNPMPublishReadinessBoundsAcceptedPublishAttempts(t *testing.T) {
	result := runNPMPublishReadiness(t, "dropped-both", 2, 2)
	if result.exitCode != 75 {
		t.Fatalf("persistent dropped publish exit = %d, want EX_TEMPFAIL\n%s", result.exitCode, result.stderr)
	}
	if got := countNPMCallsWithPrefix(result.calls, "publish "); got != 2 {
		t.Fatalf("publish calls = %d, want 2: %v", got, result.calls)
	}
	if got := countNPMCallsWithPrefix(result.calls, "--cache "); got != 4 {
		t.Fatalf("readiness calls = %d, want 4: %v", got, result.calls)
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

type npmInstallReadinessResult struct {
	exitCode int
	stdout   string
	stderr   string
	calls    []string
	sleeps   []string
}

func runNPMInstallReadiness(t *testing.T, mode string, attempts int) npmInstallReadinessResult {
	t.Helper()
	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeNPM := `#!/bin/sh
set -u
printf '%s\n' "$*" >> "$NPM_INSTALL_CALL_LOG"
count=0
if [ -f "$NPM_INSTALL_COUNT_FILE" ]; then
    IFS= read -r count < "$NPM_INSTALL_COUNT_FILE"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$NPM_INSTALL_COUNT_FILE"
case "$NPM_INSTALL_TEST_MODE" in
    delayed-exact)
        if [ "$count" -eq 1 ]; then
            printf 'npm error code ETARGET\nnpm error notarget No matching version found for %s.\n' "$NPM_INSTALL_EXPECTED_SPEC" >&2
            exit 1
        fi
        ;;
    persistent-exact)
        printf 'npm error code ETARGET\nnpm error notarget No matching version found for %s.\n' "$NPM_INSTALL_EXPECTED_SPEC" >&2
        exit 1
        ;;
    unrelated-etarget)
        printf 'npm error code ETARGET\nnpm error notarget No matching version found for @urnetwork/unrelated@0.0.0.\n' >&2
        exit 41
        ;;
    auth)
        printf 'npm error code E401\nnpm error Incorrect or missing password.\n' >&2
        exit 42
        ;;
esac
printf 'install complete\n'
`
	if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(fakeNPM), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSleep := `#!/bin/sh
printf '%s\n' "$1" >> "$NPM_INSTALL_SLEEP_LOG"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "sleep"), []byte(fakeSleep), 0o755); err != nil {
		t.Fatal(err)
	}

	const localizations = "@urnetwork/localizations"
	const sdk = "@urnetwork/sdk"
	const version = "2026.9.21-1051760150"
	callLog := filepath.Join(tempDir, "npm-install-calls")
	sleepLog := filepath.Join(tempDir, "npm-install-sleeps")
	command := exec.Command(
		filepath.Join(rolloutRoot(t), "npm-install-ready.zsh"),
		localizations, version, sdk, version,
	)
	command.Dir = tempDir
	environment := make([]string, 0, len(os.Environ())+8)
	for _, value := range os.Environ() {
		name := strings.SplitN(value, "=", 2)[0]
		switch name {
		case "NPM_INSTALL_CALL_LOG", "NPM_INSTALL_COUNT_FILE", "NPM_INSTALL_EXPECTED_SPEC", "NPM_INSTALL_READY_MAX_ATTEMPTS", "NPM_INSTALL_READY_RETRY_DELAY_SECONDS", "NPM_INSTALL_SLEEP_LOG", "NPM_INSTALL_TEST_MODE", "PATH":
			continue
		}
		environment = append(environment, value)
	}
	command.Env = append(environment,
		"NPM_INSTALL_CALL_LOG="+callLog,
		"NPM_INSTALL_COUNT_FILE="+filepath.Join(tempDir, "npm-install-count"),
		"NPM_INSTALL_EXPECTED_SPEC="+localizations+"@"+version,
		fmt.Sprintf("NPM_INSTALL_READY_MAX_ATTEMPTS=%d", attempts),
		"NPM_INSTALL_READY_RETRY_DELAY_SECONDS=0",
		"NPM_INSTALL_SLEEP_LOG="+sleepLog,
		"NPM_INSTALL_TEST_MODE="+mode,
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

	return npmInstallReadinessResult{
		exitCode: exitCode,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		calls:    readLines(callLog),
		sleeps:   readLines(sleepLog),
	}
}

func TestNPMInstallReadinessRetriesExactReleaseETARGETWithFreshCaches(t *testing.T) {
	result := runNPMInstallReadiness(t, "delayed-exact", 3)
	if result.exitCode != 0 || len(result.calls) != 2 || len(result.sleeps) != 1 {
		t.Fatalf("delayed install result = %+v, want success after one retry", result)
	}
	if !strings.Contains(result.stderr, "exact release dependency not yet resolvable") {
		t.Fatalf("retry diagnostic missing: %s", result.stderr)
	}
	cachePaths := make(map[string]struct{}, len(result.calls))
	for callNumber, call := range result.calls {
		fields := strings.Fields(call)
		if len(fields) != 3 || fields[0] != "--cache" || fields[2] != "install" {
			t.Fatalf("npm call %d = %q, want --cache <path> install", callNumber+1, call)
		}
		if _, reused := cachePaths[fields[1]]; reused {
			t.Fatalf("npm install reused cache %q across attempts: %v", fields[1], result.calls)
		}
		cachePaths[fields[1]] = struct{}{}
	}
}

func TestNPMInstallReadinessBoundsPersistentExactETARGET(t *testing.T) {
	result := runNPMInstallReadiness(t, "persistent-exact", 3)
	if result.exitCode != 1 || len(result.calls) != 3 || len(result.sleeps) != 2 {
		t.Fatalf("persistent install result = %+v, want original failure after 3 attempts", result)
	}
	if !strings.Contains(result.stderr, "remained unavailable after 3 attempts") {
		t.Fatalf("bounded failure diagnostic missing: %s", result.stderr)
	}
}

func TestNPMInstallReadinessDoesNotRetryUnrelatedFailures(t *testing.T) {
	for _, testCase := range []struct {
		mode     string
		exitCode int
	}{
		{mode: "unrelated-etarget", exitCode: 41},
		{mode: "auth", exitCode: 42},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			result := runNPMInstallReadiness(t, testCase.mode, 3)
			if result.exitCode != testCase.exitCode || len(result.calls) != 1 || len(result.sleeps) != 0 {
				t.Fatalf("unrelated install result = %+v, want immediate exit %d", result, testCase.exitCode)
			}
			if !strings.Contains(result.stderr, "not retrying a non-propagation error") {
				t.Fatalf("immediate failure diagnostic missing: %s", result.stderr)
			}
		})
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
	sdkWait := strings.Index(runSource, `@urnetwork/sdk "$EXTERNAL_WARP_VERSION"`)
	extensionEdit := strings.Index(runSource, "npm_edit_module @urnetwork/localizations")
	if waitStart < 0 || sdkWait <= waitStart || extensionEdit <= sdkWait {
		t.Fatalf("run.sh does not wait for both exact npm packages before extension edit")
	}
	if strings.Contains(runSource, `@urnetwork/sdk-js "$EXTERNAL_WARP_VERSION"`) {
		t.Fatal("run.sh probes the compatibility package instead of the extension's @urnetwork/sdk dependency")
	}
	installCall := `npm_fork_version "$EXTENSION_VERSION" \
            @urnetwork/localizations "$EXTERNAL_WARP_VERSION" \
            @urnetwork/sdk "$EXTERNAL_WARP_VERSION"`
	if !strings.Contains(runSource, installCall) {
		t.Fatal("run.sh does not install the exact verified extension dependencies through npm_fork_version")
	}
	if !strings.Contains(runSource, `"$BUILD_HOME/all/npm-install-ready.zsh" "$@"`) {
		t.Fatal("npm_fork_version does not use the tested install retry helper when exact dependencies are supplied")
	}
	if strings.Contains(runSource, "sleep 30\n\n\n(cd $BUILD_HOME/extension") {
		t.Fatal("run.sh still relies on the fixed npm propagation sleep")
	}
	if !strings.Contains(runSource, `npm_publish @urnetwork/localizations "$EXTERNAL_WARP_VERSION"`) {
		t.Fatal("run.sh does not publish and verify the exact localizations release")
	}
	if !strings.Contains(runSource, `"$BUILD_HOME/all/npm-publish-ready.zsh"`) {
		t.Fatal("run.sh does not use the tested accepted-publish recovery helper")
	}
}
