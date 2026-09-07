// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func runFunction(t *testing.T, name string) string {
	t.Helper()
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + ` \(\) \{\n.*?^\}\n`)
	matches := pattern.FindAllString(string(runData), -1)
	if len(matches) != 1 {
		t.Fatalf("run.sh %s function count = %d, want 1", name, len(matches))
	}
	return matches[0]
}

// A release module points at a private tag pushed moments earlier. Exercise the
// production environment helper itself and prove its settings reach Git while
// preserving configuration supplied by the caller.
func TestConfigurePrivateGoModulesUsesBuildSSHCredentials(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "remote", "add", "sdk", "https://github.com/urnetwork/sdk")
	runGit(t, repository, "remote", "add", "sn", "https://github.com/urfoundation/sn")

	harness := `
set -eu
eval "$1"
configure_private_go_modules
print -r -- "$GOPRIVATE"
print -r -- "$GONOPROXY"
print -r -- "$GONOSUMDB"
print -r -- "$GIT_CONFIG_COUNT"
git config --get test.preexisting
git config --get-regexp '^url\..*\.insteadof$' | LC_ALL=C sort
git -C "$2" remote get-url sdk
git -C "$2" remote get-url sn
`
	command := exec.Command("zsh", "-c", harness, "private-go-module-test", runFunction(t, "configure_private_go_modules"), repository)
	command.Env = append(os.Environ(),
		"GOPRIVATE=private.example",
		"GONOPROXY=proxy.example",
		"GONOSUMDB=sum.example",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=test.preexisting",
		"GIT_CONFIG_VALUE_0=preserved",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("private module setup failed: %v\n%s", err, output)
	}
	want := strings.Join([]string{
		"private.example,github.com/urnetwork,github.com/urfoundation",
		"proxy.example,github.com/urnetwork,github.com/urfoundation",
		"sum.example,github.com/urnetwork,github.com/urfoundation",
		"3",
		"preserved",
		"url.ssh://git@github.com/urfoundation/.insteadof https://github.com/urfoundation/",
		"url.ssh://git@github.com/urnetwork/.insteadof https://github.com/urnetwork/",
		"ssh://git@github.com/urnetwork/sdk",
		"ssh://git@github.com/urfoundation/sn",
		"",
	}, "\n")
	if string(output) != want {
		t.Fatalf("private module environment:\n%s\nwant:\n%s", output, want)
	}
}

func TestGoModForkUpdateRunsTidyBeforeGet(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "js", "go.mod"), []byte("module example.invalid/js\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	harness := `
set -eu
event_log="$1"
go() {
    print -r -- "$*" >> "$event_log"
}
GO_MOD_VERSION=2026
eval "$2"
go_mod_fork_update js
`
	eventLog := filepath.Join(root, "events")
	command := exec.Command("zsh", "-c", harness, "go-module-update-test", eventLog, runFunction(t, "go_mod_fork_update"))
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go module update failed: %v\n%s", err, output)
	}
	events, err := os.ReadFile(eventLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(events) != "mod tidy\nget -t ./...\n" {
		t.Fatalf("go module commands = %q, want %q", events, "mod tidy\nget -t ./...\n")
	}
}

// Reproduce the old false success: js sorts before a later non-module entry,
// so the loop used to replace go mod tidy's failure status with zero. The
// release must stop on the original status and must not advance to go get.
func TestGoModForkUpdateReturnsTidyFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "js", "go.mod"), []byte("module example.invalid/js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "zz-after-js"), 0o755); err != nil {
		t.Fatal(err)
	}

	harness := `
set -u
event_log="$1"
go() {
    print -r -- "$*" >> "$event_log"
    if [[ "$1 $2" = 'mod tidy' ]]; then
        return 37
    fi
    return 0
}
GO_MOD_VERSION=2026
eval "$2"
go_mod_fork_update js
`
	eventLog := filepath.Join(root, "events")
	command := exec.Command("zsh", "-c", harness, "go-module-update-test", eventLog, runFunction(t, "go_mod_fork_update"))
	command.Dir = root
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 37 {
		t.Fatalf("go_mod_fork_update exit = %v, want 37; output=%s", err, output)
	}
	events, err := os.ReadFile(eventLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(events) != "mod tidy\n" {
		t.Fatalf("go module commands = %q, want %q", events, "mod tidy\n")
	}
}

func TestGoModForkUpdateRejectsMissingModule(t *testing.T) {
	command := exec.Command(
		"zsh", "-c",
		`GO_MOD_VERSION=2026; eval "$1"; go_mod_fork_update missing`,
		"go-module-update-test", runFunction(t, "go_mod_fork_update"),
	)
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "missing forked module: missing/go.mod") {
		t.Fatalf("missing module exit = %v, output=%q", err, output)
	}
}
