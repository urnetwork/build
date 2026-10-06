// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise each production commit/push region against a local bare origin.
// A second clone advances main after checkout but before the generated commit,
// reproducing the window between generation/tests and release version staging.
func TestRunGeneratedPushIntegratesConcurrentMainCommit(t *testing.T) {
	stages := []struct {
		repository string
		start      string
		end        string
		paths      []string
	}{
		{
			repository: "connect",
			start:      connectIpUpdatePushStart,
			end:        connectIpUpdatePushEnd,
			paths:      connectGeneratedTables,
		},
		{
			repository: "mmm",
			start:      "# Push the changelog and the desktop releases",
			end:        "# push the regenerated localizations",
			paths: []string{
				"ur.io/react/src/data/changelog.js",
				"ur.io/react/src/data/changelog-version.js",
				"ur.io/react/src/data/releases.js",
				"ur.io/react/src/data/content-dates.js",
				"ur.io/react/src/data/page-dates.js",
			},
		},
		{
			repository: "android",
			start:      "(cd $BUILD_HOME/android &&\n    git add",
			end:        "(cd $BUILD_HOME/apple &&\n    git add",
			paths:      []string{"app/app/src/main/res/values/strings.xml", "app/app/src/main/res/values-es/strings.xml"},
		},
		{
			repository: "apple",
			start:      "(cd $BUILD_HOME/apple &&\n    git add",
			end:        "(cd $BUILD_HOME/windows &&\n    git add",
			paths:      []string{"app/network/Shared/Resources/Localizable.xcstrings"},
		},
		{
			repository: "windows",
			start:      "(cd $BUILD_HOME/windows &&\n    git add",
			end:        "(cd $BUILD_HOME/linux &&\n    git add",
			paths:      []string{"app/src/App/Strings/en-US/Resources.resw"},
		},
		{
			repository: "linux",
			start:      "(cd $BUILD_HOME/linux &&\n    git add",
			end:        "(cd $BUILD_HOME/connect && git checkout -b",
			paths:      []string{"app/po/en.po"},
		},
	}
	for _, stage := range stages {
		t.Run(stage.repository, func(t *testing.T) {
			tempDir := t.TempDir()
			remote := filepath.Join(tempDir, "remote.git")
			buildHome := filepath.Join(tempDir, "build")
			repository := filepath.Join(buildHome, stage.repository)
			concurrent := filepath.Join(tempDir, "concurrent")
			runGit(t, tempDir, "init", "--bare", remote)
			runGit(t, tempDir, "init", "-b", "main", repository)
			for _, name := range stage.paths {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(repository, name)), 0o755); err != nil {
					t.Fatal(err)
				}
				commitTestFile(t, repository, name, "before\n", "seed "+name)
			}
			runGit(t, repository, "remote", "add", "origin", remote)
			runGit(t, repository, "push", "-u", "origin", "main")
			runGit(t, tempDir, "clone", "-b", "main", remote, concurrent)
			concurrentCommit := commitTestFile(t, concurrent, "source.go", "concurrent code\n", "concurrent source update")
			runGit(t, concurrent, "push")
			for _, name := range stage.paths {
				if err := os.WriteFile(filepath.Join(repository, name), []byte("generated\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			region := componentRegion(t, stage.start, stage.end)
			command := exec.Command("zsh", "-f", "-c", `
builder_message() { print -r -- "$1"; }
eval "$1" || exit $?
eval "$2" || exit $?
print -r -- push-region-continued
`, "generated-push-test", componentFunctions(t, "error_trap")+pushRetryFunction(t), region)
			command.Env = environmentWith(
				"BUILD_HOME="+buildHome,
				"WARP_HOME="+buildHome,
				"CONNECT_IP_UPDATE=1",
				"EXTERNAL_WARP_VERSION=2026.10.6-1065270230",
				"GIT_AUTHOR_NAME=Build Harness Test",
				"GIT_AUTHOR_EMAIL=build-harness-test@example.invalid",
				"GIT_COMMITTER_NAME=Build Harness Test",
				"GIT_COMMITTER_EMAIL=build-harness-test@example.invalid",
			)
			output, err := command.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "push-region-continued") {
				t.Fatalf("generated push did not recover from concurrent main advance: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "[rejected]") {
				t.Fatalf("fixture did not reject the first push: %s", output)
			}
			runGit(t, remote, "merge-base", "--is-ancestor", concurrentCommit, "main")
			for _, name := range stage.paths {
				if contents := runGit(t, remote, "show", "main:"+name); contents != "generated" {
					t.Fatalf("pushed %s contents = %q, want generated", name, contents)
				}
			}
			if head, pushed := runGit(t, repository, "rev-parse", "HEAD"), runGit(t, remote, "rev-parse", "main"); head != pushed {
				t.Fatalf("release checkout %s differs from pushed main %s", head, pushed)
			}
			if status := runGit(t, repository, "status", "--porcelain"); status != "" {
				t.Fatalf("generated push left a dirty release checkout: %s", status)
			}
		})
	}
}
