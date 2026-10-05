// SPDX-License-Identifier: MPL-2.0

// The gomobile CI workflows build the sdk with the Go that the release host
// check in run.sh requires and that build-fdroid.sh installs, so a green CI
// bind speaks for the toolchain a release uses.

package allbuild

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// run.sh's preflight is the reference. The F-Droid download and version check,
// and each gomobile workflow's UR_GO_VERSION, must name the same version, and
// every setup-go step in those workflows must install UR_GO_VERSION.
func TestGomobileWorkflowsUseReleaseGoVersion(t *testing.T) {
	root := rolloutRoot(t)
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// one match per source, so a second spelling cannot hide a stale one
	single := func(name string, source string, pattern *regexp.Regexp) string {
		t.Helper()
		matches := pattern.FindAllStringSubmatch(source, -1)
		if len(matches) != 1 {
			t.Fatalf("%s: %d matches of %q, want 1", name, len(matches), pattern)
		}
		return matches[0][1]
	}

	releaseGoVersion := single(
		"run.sh go check",
		read("run.sh"),
		regexp.MustCompile("`go version` =~ 'go version go([0-9]+\\.[0-9]+\\.[0-9]+)'"),
	)

	fdroid := read("build-fdroid.sh")
	for _, check := range []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{
			name:    "build-fdroid.sh go download",
			pattern: regexp.MustCompile(`https://go\.dev/dl/go([0-9]+\.[0-9]+\.[0-9]+)\.linux-amd64\.tar\.gz`),
		},
		{
			name:    "build-fdroid.sh go check",
			pattern: regexp.MustCompile(`"go version go([0-9]+\.[0-9]+\.[0-9]+)"`),
		},
	} {
		if goVersion := single(check.name, fdroid, check.pattern); goVersion != releaseGoVersion {
			t.Errorf("%s names go %s, want the release host's %s", check.name, goVersion, releaseGoVersion)
		}
	}

	urGoVersionPattern := regexp.MustCompile(`(?m)^  UR_GO_VERSION: "([^"]*)"$`)
	setupGoVersionPattern := regexp.MustCompile(`(?m)^[ \t]+(go-version(?:-file)?):[ \t]*(.*?)[ \t]*$`)
	for _, workflow := range []string{"android-build.yml", "apple-build.yml"} {
		source := read(filepath.Join("..", ".github", "workflows", workflow))
		if goVersion := single(workflow+" UR_GO_VERSION", source, urGoVersionPattern); goVersion != releaseGoVersion {
			t.Errorf("%s UR_GO_VERSION is %s, want the release host's %s", workflow, goVersion, releaseGoVersion)
		}
		setupGoVersions := setupGoVersionPattern.FindAllStringSubmatch(source, -1)
		if len(setupGoVersions) == 0 {
			t.Errorf("%s sets up no Go", workflow)
		}
		for _, setupGoVersion := range setupGoVersions {
			if setupGoVersion[1] != "go-version" || setupGoVersion[2] != "${{ env.UR_GO_VERSION }}" {
				t.Errorf("%s: setup-go %s: %s, want go-version: ${{ env.UR_GO_VERSION }}", workflow, setupGoVersion[1], setupGoVersion[2])
			}
		}
	}
}
