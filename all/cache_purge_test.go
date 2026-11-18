// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The purge runs with curl and sleep faked: curl records its target URL and
// the config it read on stdin, and answers per zone from CURL_FAIL_ZONE.
const cachePurgeHarness = `
set -u
event_log="$1"
: > "$event_log"
record_event() { print -r -- "$*" >> "$event_log" }
builder_message() { record_event message "$1" }
sleep() { record_event sleep "$1" }
curl() {
    local url config
    config="$(cat)"
    for a in "$@"; do [[ "$a" == https://* ]] && url="$a"; done
    record_event curl "$url" "argv:$*" "stdin:$config"
    if [[ -n "${CURL_FAIL_ZONE:-}" && "$url" == *"${CURL_FAIL_ZONE}"* ]]; then
        print -r -- '{"success":false}'
    else
        print -r -- '{"success":true}'
    fi
}
BUILD_ENV=main
source "$2"
cf_cache_purge
print -r -- "exit:$?" >> "$event_log"
`

func runCachePurge(t *testing.T, env ...string) string {
	t.Helper()
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not installed")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "events")
	harness := filepath.Join(dir, "harness.zsh")
	if err := os.WriteFile(harness, []byte(cachePurgeHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("zsh", harness, log, "cache-purge.zsh")
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("harness: %v\n%s", err, out)
	}
	events, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(events)
}

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cf-cache-purge")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCachePurgeWaitsThenPurgesBothZonesWithoutExposingTheToken(t *testing.T) {
	events := runCachePurge(t, "CACHE_PURGE_TOKEN_FILE="+tokenFile(t, "secret-token\n"), "CACHE_PURGE_DELAY_SECONDS=1200")
	lines := strings.Split(strings.TrimSpace(events), "\n")
	if !strings.HasPrefix(lines[0], "sleep 1200") {
		t.Fatalf("purge must wait for convergence first, got %q", lines[0])
	}
	for _, zone := range []string{"9ee8c03ca7df0dbe6ff8eef56a2fb5cf", "e5fb0c3111fe44c9249c478dd40e10d3"} {
		if !strings.Contains(events, "/zones/"+zone+"/purge_cache") {
			t.Errorf("zone %s not purged:\n%s", zone, events)
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "curl ") {
			argv := line[strings.Index(line, "argv:"):strings.Index(line, "stdin:")]
			if strings.Contains(argv, "secret-token") {
				t.Errorf("token on curl's command line: %s", argv)
			}
			if !strings.Contains(line, `stdin:header = "Authorization: Bearer secret-token"`) {
				t.Errorf("token not passed on stdin: %s", line)
			}
		}
	}
	if !strings.Contains(events, "message main cache purged: ur.io, ur.xyz") || !strings.HasSuffix(strings.TrimSpace(events), "exit:0") {
		t.Fatalf("unexpected outcome:\n%s", events)
	}
}

func TestCachePurgeFailureIsReportedButNeverFailsTheRelease(t *testing.T) {
	events := runCachePurge(t, "CACHE_PURGE_TOKEN_FILE="+tokenFile(t, "t"), "CURL_FAIL_ZONE=e5fb0c3111fe44c9249c478dd40e10d3")
	if !strings.Contains(events, "cache purge failed for ur.xyz (purged: ur.io)") || !strings.HasSuffix(strings.TrimSpace(events), "exit:0") {
		t.Fatalf("unexpected outcome:\n%s", events)
	}
}

func TestCachePurgeWithoutTokenSkipsWithoutWaiting(t *testing.T) {
	events := runCachePurge(t, "CACHE_PURGE_TOKEN_FILE=/nonexistent/cf-cache-purge")
	if strings.Contains(events, "sleep") || strings.Contains(events, "curl") || !strings.Contains(events, "cache purge skipped") || !strings.HasSuffix(strings.TrimSpace(events), "exit:0") {
		t.Fatalf("unexpected outcome:\n%s", events)
	}
}

func TestRunPurgesCacheAfterRollout(t *testing.T) {
	src, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	rollout := strings.Index(s, "error_trap 'warp service rollout'")
	purge := strings.Index(s, "cf_cache_purge\n")
	if rollout < 0 || purge < rollout {
		t.Fatal("run.sh must call cf_cache_purge after the warp service rollout")
	}
}
