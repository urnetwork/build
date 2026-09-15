// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Executes the actual workflow assertion, including its runner-install gate.
func androidNdkWorkflowAssertion(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Android workflow contract test")
	}
	path := filepath.Join(filepath.Dir(filename), "..", ".github", "workflows", "android-build.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "      - name: Assert the app's version matches the aar the SDK job stamped\n        run: |\n"
	if count := strings.Count(string(data), anchor); count != 1 {
		t.Fatalf("Android version assertion count = %d, want 1", count)
	}
	tail := strings.SplitN(string(data), anchor, 2)[1]
	var lines []string
	for _, line := range strings.Split(tail, "\n") {
		if line == "" {
			lines = append(lines, "")
			continue
		}
		if !strings.HasPrefix(line, "          ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "          "))
	}
	body := strings.Join(lines, "\n")
	if !strings.Contains(body, "UR_ANDROID_NDK_VERSION") || !strings.Contains(body, "exit $fail") {
		t.Fatal("extracted workflow assertion is incomplete")
	}
	return body
}

// Only synthetic Gradle/properties and an empty SDK directory are consumed.
func runAndroidNdkWorkflowFixture(
	t *testing.T,
	gradle string,
	ndkInstalled bool,
) (output []byte, err error) {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "android", "app")
	sdk := filepath.Join(root, "synthetic-sdk")
	for _, directory := range []string{filepath.Join(app, "app"), sdk} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if ndkInstalled {
		if err := os.MkdirAll(filepath.Join(sdk, "ndk", "91.2.3"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct {
		path string
		body string
	}{
		{path: filepath.Join(app, "local.properties"), body: "warp.version=0.0.0-synthetic\nwarp.version_code=17\n"},
		{path: filepath.Join(app, "app", "build.gradle"), body: gradle},
	} {
		if err := os.WriteFile(fixture.path, []byte(fixture.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-c", androidNdkWorkflowAssertion(t))
	command.Dir = root
	command.Env = environmentWith(
		"UR_APP_VERSION=0.0.0-synthetic",
		"UR_APP_VERSION_CODE=17",
		"UR_ANDROID_NDK_VERSION=91.2.3",
		"ANDROID_SDK_ROOT="+sdk,
		"BASH_ENV=",
		"ENV=",
	)
	output, err = command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("synthetic workflow assertion deadline: %v", ctx.Err())
	}
	return output, err
}

// Groovy permits both a method call and assignment for the literal NDK pin.
func TestAndroidNdkWorkflowAcceptsGroovyLiteralForms(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		gradle string
	}{
		{name: "method single quote", gradle: "android {\n    ndkVersion '91.2.3'\n}\n"},
		{name: "assignment single quote", gradle: "android {\n    ndkVersion = '91.2.3'\n}\n"},
		{name: "compact assignment", gradle: "android {\n    ndkVersion='91.2.3'\n}\n"},
		{name: "tabbed assignment", gradle: "android {\n\tndkVersion\t=\t'91.2.3'\n}\n"},
		{name: "method double quote", gradle: "android {\n    ndkVersion \"91.2.3\"\n}\n"},
		{name: "assignment double quote", gradle: "android {\n    ndkVersion = \"91.2.3\"\n}\n"},
		{name: "trailing comment", gradle: "android {\n    ndkVersion = '91.2.3'; // synthetic pin\n}\n"},
	} {
		if output, err := runAndroidNdkWorkflowFixture(t, fixture.gradle, true); err != nil {
			t.Errorf("%s: actual workflow rejected valid literal pin: %v\n%s", fixture.name, err, output)
		}
	}
}

// Commented text, wildcard lookalikes and malformed literals are not a pin.
func TestAndroidNdkWorkflowRejectsNonPins(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		gradle string
	}{
		{name: "wrong version", gradle: "android {\n    ndkVersion '91.2.4'\n}\n"},
		{name: "regex lookalike", gradle: "android {\n    ndkVersion '91x2x3'\n}\n"},
		{name: "line comment only", gradle: "android {\n    // ndkVersion '91.2.3'\n}\n"},
		{name: "comment cannot conceal wrong pin", gradle: "android {\n    // ndkVersion '91.2.3'\n    ndkVersion = '91.2.4'\n}\n"},
		{name: "mismatched quotes", gradle: "android {\n    ndkVersion '91.2.3\"\n}\n"},
		{name: "unterminated quote", gradle: "android {\n    ndkVersion '91.2.3\n}\n"},
		{name: "computed suffix", gradle: "android {\n    ndkVersion '91.2.3' + '.synthetic'\n}\n"},
		{name: "absent property", gradle: "android {\n}\n"},
	} {
		output, err := runAndroidNdkWorkflowFixture(t, fixture.gradle, true)
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 ||
			!strings.Contains(string(output), "no longer pins ndkVersion") {
			t.Errorf("%s: actual workflow exit = %v, want pin rejection1\n%s", fixture.name, err, output)
		}
	}
}

// A matching source pin cannot bypass the existing runner-install check.
func TestAndroidNdkWorkflowRequiresInstalledRunnerNdk(t *testing.T) {
	output, err := runAndroidNdkWorkflowFixture(t, "android {\n    ndkVersion '91.2.3'\n}\n", false)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 ||
		!strings.Contains(string(output), "is not installed") {
		t.Fatalf("missing runner NDK exit = %v, want install rejection1\n%s", err, output)
	}
}
