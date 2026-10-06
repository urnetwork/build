// SPDX-License-Identifier: MPL-2.0

// The F-Droid build installs the Go that the release host check in run.sh
// requires, so the APK F-Droid rebuilds comes from the same toolchain as the
// release.

package allbuild

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// run.sh's preflight is the reference. build-fdroid.sh's Go download and its
// quoted version check must name the same version.
func TestFdroidBuildUsesReleaseGoVersion(t *testing.T) {
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
}
