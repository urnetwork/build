// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The localizations generator also writes android's res/xml/locales_config.xml
// (the app's languages for the per-app language settings) and apple's
// <language>.lproj/AppShortcuts.strings (the App Shortcuts phrases). The push
// regions staged only the strings catalogs, so a new language's file stayed
// untracked, or a changed phrase stayed modified, in the release checkout:
// git_main's next pull refuses a modified tree, and an untracked file the next
// main adds aborts the pull. Each region now commits them with the catalogs and
// leaves the checkout clean, and still runs when none of them exists.
func TestRunGeneratedPushStagesEveryGeneratedLocaleFile(t *testing.T) {
	stages := []struct {
		repository string
		start      string
		end        string
		seeded     []string
		// written by the generator: new files, or seeded ones changed
		generated []string
	}{
		{
			repository: "android",
			start:      "(cd $BUILD_HOME/android &&\n    git add",
			end:        "(cd $BUILD_HOME/apple &&\n    git add",
			seeded:     []string{"app/app/src/main/res/values/strings.xml"},
			generated: []string{
				"app/app/src/main/res/values/strings.xml",
				"app/app/src/main/res/values-tr/strings.xml",
				"app/app/src/main/res/xml/locales_config.xml",
			},
		},
		{
			repository: "apple",
			start:      "(cd $BUILD_HOME/apple &&\n    git add",
			end:        "(cd $BUILD_HOME/windows &&\n    git add",
			seeded: []string{
				"app/network/Shared/Resources/Localizable.xcstrings",
				"app/network/Shared/Resources/de.lproj/AppShortcuts.strings",
			},
			generated: []string{
				"app/network/Shared/Resources/Localizable.xcstrings",
				"app/network/Shared/Resources/de.lproj/AppShortcuts.strings",
				"app/network/Shared/Resources/tr.lproj/AppShortcuts.strings",
			},
		},
	}
	for _, stage := range stages {
		for _, generate := range []bool{true, false} {
			name := stage.repository + "/generated"
			if !generate {
				name = stage.repository + "/unchanged"
			}
			t.Run(name, func(t *testing.T) {
				tempDir := t.TempDir()
				remote := filepath.Join(tempDir, "remote.git")
				buildHome := filepath.Join(tempDir, "build")
				repository := filepath.Join(buildHome, stage.repository)
				runGit(t, tempDir, "init", "--bare", remote)
				runGit(t, tempDir, "init", "-b", "main", repository)
				for _, path := range stage.seeded {
					if err := os.MkdirAll(filepath.Dir(filepath.Join(repository, path)), 0o755); err != nil {
						t.Fatal(err)
					}
					commitTestFile(t, repository, path, "before\n", "seed "+path)
				}
				runGit(t, repository, "remote", "add", "origin", remote)
				runGit(t, repository, "push", "-u", "origin", "main")
				before := runGit(t, repository, "rev-parse", "HEAD")
				if generate {
					for _, path := range stage.generated {
						if err := os.MkdirAll(filepath.Dir(filepath.Join(repository, path)), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(repository, path), []byte("generated\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}

				region := componentRegion(t, stage.start, stage.end)
				command := exec.Command("zsh", "-f", "-c", `
builder_message() { print -r -- "$1"; }
eval "$1" || exit $?
eval "$2" || exit $?
print -r -- push-region-continued
`, "generated-locale-files-test", componentFunctions(t, "error_trap")+pushRetryFunction(t), region)
				command.Dir = tempDir
				command.Env = environmentWith(
					"BUILD_HOME="+buildHome,
					"WARP_HOME="+buildHome,
					"EXTERNAL_WARP_VERSION=2026.11.28-1100000000",
					"GIT_AUTHOR_NAME=Build Harness Test",
					"GIT_AUTHOR_EMAIL=build-harness-test@example.invalid",
					"GIT_COMMITTER_NAME=Build Harness Test",
					"GIT_COMMITTER_EMAIL=build-harness-test@example.invalid",
				)
				output, err := command.CombinedOutput()
				if err != nil || !strings.Contains(string(output), "push-region-continued") {
					t.Fatalf("the push region failed: %v\n%s", err, output)
				}
				if status := runGit(t, repository, "status", "--porcelain"); status != "" {
					t.Fatalf("the push region left the release checkout dirty: %s", status)
				}
				if !generate {
					if head := runGit(t, repository, "rev-parse", "HEAD"); head != before {
						t.Fatalf("an unchanged checkout made a commit")
					}
					return
				}
				for _, path := range stage.generated {
					if contents := runGit(t, remote, "show", "main:"+path); contents != "generated" {
						t.Fatalf("pushed %s contents = %q, want generated", path, contents)
					}
				}
			})
		}
	}
}
