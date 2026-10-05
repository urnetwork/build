// SPDX-License-Identifier: MPL-2.0

// The CONNECT_IP_UPDATE push step, run from run.sh against a local connect
// repository and a bare origin.
package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The CONNECT_IP_UPDATE push must commit every table the connect generators
// write: security/main.go writes the CFAA blocklists and the Meta prefixes of
// the WhatsApp exception, blocker/main.go the blocker tables. A table left out
// stays modified in the build's connect checkout, so the next build's pull
// (git_main refuses a dirty tree) fails, and the release would ship a table no
// commit records.
const connectIpUpdatePushStart = `# push the connect IP table update`
const connectIpUpdatePushEnd = `# Push the changelog and the desktop releases`

var connectGeneratedTables = []string{
	"ip_blocker_block.go",
	"ip_security_cfaa_block.go",
	"ip_security_messaging_meta.go",
}

// A connect checkout whose generated tables (and one hand-written file) the
// generators have just rewritten, with a local bare origin to push to.
func connectIpUpdateFixture(t *testing.T, tables []string) (buildHome string, connectDir string, remoteDir string) {
	t.Helper()
	tempDir := t.TempDir()
	remoteDir = filepath.Join(tempDir, "connect.git")
	buildHome = filepath.Join(tempDir, "build")
	connectDir = filepath.Join(buildHome, "connect")
	runGit(t, tempDir, "init", "--bare", remoteDir)
	runGit(t, tempDir, "init", "-b", "main", connectDir)
	names := append(slices.Clone(tables), "ip_security.go")
	for _, name := range names {
		commitTestFile(t, connectDir, name, "before\n", "seed "+name)
	}
	runGit(t, connectDir, "remote", "add", "origin", remoteDir)
	runGit(t, connectDir, "push", "-u", "origin", "main")
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(connectDir, name), []byte("after\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return
}

// Runs the production push region with the production error_trap.
func runConnectIpUpdatePush(t *testing.T, buildHome string) ([]byte, error) {
	t.Helper()
	region := componentRegion(t, connectIpUpdatePushStart, connectIpUpdatePushEnd)
	if output, err := exec.Command("zsh", "-f", "-n", "-c", region).CombinedOutput(); err != nil {
		t.Fatalf("extracted push region does not parse: %v\n%s", err, output)
	}
	harness := `
builder_message() { print -r -- "$1"; }
eval "$1"
eval "$2"
print -r -- push-region-continued
`
	command := exec.Command("zsh", "-c", harness, "connect-ip-update-test", componentFunctions(t, "error_trap"), region)
	command.Env = environmentWith(
		"BUILD_HOME="+buildHome,
		"CONNECT_IP_UPDATE=1",
		"EXTERNAL_WARP_VERSION=2026.10.5-1061000000",
		"GIT_AUTHOR_NAME=Build Harness Test",
		"GIT_AUTHOR_EMAIL=build-harness-test@example.invalid",
		"GIT_COMMITTER_NAME=Build Harness Test",
		"GIT_COMMITTER_EMAIL=build-harness-test@example.invalid",
	)
	return command.CombinedOutput()
}

// The push commits the generated tables, and only them, with the release
// version in the message: a hand-written change stays unstaged, and tables
// regenerated unchanged make no commit.
func TestRunConnectIpUpdateCommitsEveryGeneratedTable(t *testing.T) {
	buildHome, connectDir, remoteDir := connectIpUpdateFixture(t, connectGeneratedTables)
	output, err := runConnectIpUpdatePush(t, buildHome)
	if err != nil || !strings.Contains(string(output), "push-region-continued") {
		t.Fatalf("push region failed: %v\n%s", err, output)
	}

	head := runGit(t, remoteDir, "rev-parse", "main")
	committed := strings.Fields(runGit(t, remoteDir, "diff-tree", "--no-commit-id", "--name-only", "-r", head))
	slices.Sort(committed)
	if !slices.Equal(committed, connectGeneratedTables) {
		t.Fatalf("pushed commit holds %v, want exactly the generated tables %v", committed, connectGeneratedTables)
	}
	if message := runGit(t, remoteDir, "log", "-1", "--format=%s", head); message != "2026.10.5-1061000000 ip security and blocker update" {
		t.Fatalf("commit message = %q", message)
	}
	// only the hand-written file is left for a human; no generated table stays dirty
	if status := runGit(t, connectDir, "status", "--porcelain"); status != "M ip_security.go" {
		t.Fatalf("connect status after the push = %q, want only the hand-written change", status)
	}

	// regenerated tables identical to the committed ones make no commit
	output, err = runConnectIpUpdatePush(t, buildHome)
	if err != nil {
		t.Fatalf("unchanged push region failed: %v\n%s", err, output)
	}
	if again := runGit(t, remoteDir, "rev-parse", "main"); again != head {
		t.Fatalf("unchanged tables made a commit: %s -> %s", head, again)
	}
}

// A connect checkout without one of the generated tables (connect older than
// this step) fails the step loudly and pushes nothing, rather than releasing
// without that table's refresh.
func TestRunConnectIpUpdateRequiresEveryGeneratedTable(t *testing.T) {
	tables := slices.DeleteFunc(slices.Clone(connectGeneratedTables), func(name string) bool {
		return name == "ip_security_messaging_meta.go"
	})
	buildHome, _, remoteDir := connectIpUpdateFixture(t, tables)
	before := runGit(t, remoteDir, "rev-parse", "main")
	output, err := runConnectIpUpdatePush(t, buildHome)
	if err == nil || strings.Contains(string(output), "push-region-continued") || !strings.Contains(string(output), "connect ip update push") {
		t.Fatalf("push region without the Meta table: err=%v\n%s", err, output)
	}
	if after := runGit(t, remoteDir, "rev-parse", "main"); after != before {
		t.Fatalf("a failed push region still pushed: %s -> %s", before, after)
	}
}
