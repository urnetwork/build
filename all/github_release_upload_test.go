// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type uploadHTTPResponse struct {
	status  int
	headers map[string]string
	body    string
}

type uploadScriptResult struct {
	exitCode int
	stdout   string
	stderr   string
	requests int
	sleeps   string
}

func runUploadScript(t *testing.T, responses []uploadHTTPResponse, attempts int) uploadScriptResult {
	t.Helper()
	var mu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		responseIndex := requestCount
		requestCount++
		mu.Unlock()

		if request.Method != http.MethodPost {
			t.Errorf("upload method = %s, want POST", request.Method)
		}
		if request.URL.Query().Get("name") != "artifact.pkg" {
			t.Errorf("upload name = %q, want artifact.pkg", request.URL.Query().Get("name"))
		}
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("upload authorization header was not passed")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read upload body: %v", err)
		}
		if string(body) != "asset payload" {
			t.Errorf("upload body = %q, want asset payload", body)
		}

		index := responseIndex
		if index >= len(responses) {
			index = len(responses) - 1
		}
		response := responses[index]
		for name, value := range response.headers {
			writer.Header().Set(name, value)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(response.status)
		_, _ = io.WriteString(writer, response.body)
	}))
	defer server.Close()

	tempDir := t.TempDir()
	assetPath := filepath.Join(tempDir, "artifact.pkg")
	if err := os.WriteFile(assetPath, []byte("asset payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	sleepLog := filepath.Join(tempDir, "sleep.log")
	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSleep := filepath.Join(fakeBin, "sleep")
	if err := os.WriteFile(fakeSleep, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$SLEEP_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(
		filepath.Join(rolloutRoot(t), "github-release-upload.zsh"),
		server.URL+"/assets",
		"artifact.pkg",
		assetPath,
	)
	environment := make([]string, 0, len(os.Environ())+6)
	for _, value := range os.Environ() {
		name := strings.SplitN(value, "=", 2)[0]
		switch name {
		case "GITHUB_API_KEY", "GITHUB_UPLOAD_MAX_ATTEMPTS", "GITHUB_UPLOAD_MAX_WAIT_SECONDS", "GITHUB_UPLOAD_RETRY_DELAY_SECONDS", "PATH", "SLEEP_LOG":
			continue
		}
		environment = append(environment, value)
	}
	command.Env = append(
		environment,
		"GITHUB_API_KEY=test-token",
		"GITHUB_UPLOAD_MAX_ATTEMPTS="+strconv.Itoa(attempts),
		"GITHUB_UPLOAD_MAX_WAIT_SECONDS=10",
		"GITHUB_UPLOAD_RETRY_DELAY_SECONDS=0",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SLEEP_LOG="+sleepLog,
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
	sleepData, readErr := os.ReadFile(sleepLog)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	mu.Lock()
	requests := requestCount
	mu.Unlock()
	return uploadScriptResult{
		exitCode: exitCode,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		requests: requests,
		sleeps:   string(sleepData),
	}
}

func TestGitHubReleaseUploadWaitsForPrimaryRateLimitReset(t *testing.T) {
	reset := time.Now().Unix()
	result := runUploadScript(t, []uploadHTTPResponse{
		{
			status: http.StatusForbidden,
			headers: map[string]string{
				"X-GitHub-Request-Id":   "safe-request-id",
				"X-RateLimit-Remaining": "0",
				"X-RateLimit-Reset":     fmt.Sprint(reset),
				"X-RateLimit-Resource":  "core",
			},
			body: `{"message":"API rate limit exceeded"}`,
		},
		{status: http.StatusCreated, body: `{"id":123}`},
	}, 2)
	if result.exitCode != 0 || result.requests != 2 || result.stdout != `{"id":123}` {
		t.Fatalf("rate-limit recovery = %+v", result)
	}
	if result.sleeps != "1\n" {
		t.Fatalf("rate-limit sleeps = %q, want 1 second", result.sleeps)
	}
	for _, want := range []string{"HTTP 403", "API rate limit exceeded", "safe-request-id", "rate-remaining=0", "retrying in 1s"} {
		if !strings.Contains(result.stderr, want) {
			t.Errorf("rate-limit diagnostic lacks %q: %s", want, result.stderr)
		}
	}
	if strings.Contains(result.stderr, "test-token") {
		t.Fatal("rate-limit diagnostic exposed the bearer token")
	}
}

func TestGitHubReleaseUploadRetriesSecondaryLimitUsingRetryAfter(t *testing.T) {
	result := runUploadScript(t, []uploadHTTPResponse{
		{
			status:  http.StatusForbidden,
			headers: map[string]string{"Retry-After": "3"},
			body:    `{"message":"You have exceeded a secondary rate limit"}`,
		},
		{status: http.StatusCreated, body: `{"id":456}`},
	}, 2)
	if result.exitCode != 0 || result.requests != 2 || result.sleeps != "3\n" {
		t.Fatalf("secondary-limit recovery = %+v", result)
	}
}

func TestGitHubReleaseUploadDoesNotRetryPermanentForbidden(t *testing.T) {
	result := runUploadScript(t, []uploadHTTPResponse{{
		status: http.StatusForbidden,
		body:   `{"message":"Resource not accessible by personal access token"}`,
	}}, 4)
	if result.exitCode != 22 || result.requests != 1 || result.sleeps != "" {
		t.Fatalf("permanent 403 handling = %+v", result)
	}
	if !strings.Contains(result.stderr, "not retrying a non-rate-limit response") {
		t.Fatalf("permanent 403 diagnostic missing: %s", result.stderr)
	}
}

func TestGitHubReleaseUploadRetryIsBounded(t *testing.T) {
	result := runUploadScript(t, []uploadHTTPResponse{{
		status:  http.StatusTooManyRequests,
		headers: map[string]string{"Retry-After": "0"},
		body:    `{"message":"rate limit"}`,
	}}, 3)
	if result.exitCode != 22 || result.requests != 3 || result.sleeps != "0\n0\n" {
		t.Fatalf("bounded retry = %+v", result)
	}
	if !strings.Contains(result.stderr, "retry limit reached (3/3)") {
		t.Fatalf("bounded retry diagnostic missing: %s", result.stderr)
	}
}

func TestGitHubReleaseUploadLeavesDuplicateFailClosed(t *testing.T) {
	result := runUploadScript(t, []uploadHTTPResponse{{
		status: http.StatusUnprocessableEntity,
		body:   `{"message":"Validation Failed","errors":[{"code":"already_exists"}]}`,
	}}, 4)
	if result.exitCode != 22 || result.requests != 1 {
		t.Fatalf("duplicate handling = %+v", result)
	}
	if !strings.Contains(result.stderr, "Validation Failed") || !strings.Contains(result.stderr, "not retrying") {
		t.Fatalf("duplicate diagnostic missing: %s", result.stderr)
	}
}

func TestRunUsesCheckedGitHubReleaseUploadTransport(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	required := `GITHUB_UPLOAD=$("$BUILD_HOME/all/github-release-upload.zsh" \
        "$GITHUB_UPLOAD_URL" "$1" "$2")
    error_trap "github release upload $1"`
	if !strings.Contains(string(runData), required) {
		t.Fatal("run.sh does not use and immediately check the tested GitHub upload transport")
	}
}
