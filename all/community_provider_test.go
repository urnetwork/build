// SPDX-License-Identifier: MPL-2.0
package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The community provider image's files, all/community/provider.
func communityProviderDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(rolloutRoot(t), "community", "provider")
}

func writeCommunityExecutable(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}

// The image's wrapper runs as root on the image's base, so its tests run
// there: bringyour-provider.test.sh in an ubuntu:22.04 container, with a
// read-only mount for the read-only case.
func TestCommunityProviderWrapper(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker required")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon required")
	}
	readOnly := t.TempDir()
	command := exec.Command("docker", "run", "--rm",
		"-v", communityProviderDir(t)+":/t:ro",
		"-v", readOnly+":/ro:ro",
		"ubuntu:22.04", "sh", "/t/bringyour-provider.test.sh")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "PASS: bringyour-provider.sh") {
		t.Fatalf("wrapper tests failed: %v\n%s", err, output)
	}
}

// A fake registry for promote.sh: $FAKE_REGISTRY/tag-<tag> and
// digest-<digest> hold "<digest> <version label>", and every create is
// logged to $FAKE_LOG.
const fakeRegistryDocker = `#!/usr/bin/env bash
[ "$1 $2" = "buildx imagetools" ] || exit 99
case "$3" in
inspect)
    ref=$4
    format=$6
    if [ -n "${FAKE_INSPECT_ERROR:-}" ]; then
        echo "$FAKE_INSPECT_ERROR" >&2
        exit 1
    fi
    if [[ "$ref" == *@* ]]; then
        file="$FAKE_REGISTRY/digest-${ref##*@}"
    else
        file="$FAKE_REGISTRY/tag-${ref##*:}"
    fi
    if [ ! -f "$file" ]; then
        echo "ERROR: docker.io/$ref: not found" >&2
        exit 1
    fi
    read -r digest label < "$file"
    case "$format" in
    '{{json .Manifest}}') printf '{"digest":"%s"}\n' "$digest" ;;
    '{{json .Image}}') printf '{"linux/amd64":{"config":{"Labels":{"org.opencontainers.image.version":"%s"}}},"linux/arm64":{"config":{"Labels":{"org.opencontainers.image.version":"%s"}}}}\n' "$label" "$label" ;;
    *) exit 98 ;;
    esac
    ;;
create)
    [ "$4" = -t ] || exit 97
    tag=${5##*:}
    digest=${6##*@}
    cp "$FAKE_REGISTRY/digest-$digest" "$FAKE_REGISTRY/tag-$tag"
    echo "create $tag $digest" >> "$FAKE_LOG"
    ;;
*)
    exit 96
    ;;
esac
`

type fakeRegistry struct {
	t    *testing.T
	root string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	root := t.TempDir()
	writeCommunityExecutable(t, filepath.Join(root, "bin", "docker"), fakeRegistryDocker)
	if err := os.MkdirAll(filepath.Join(root, "registry"), 0755); err != nil {
		t.Fatal(err)
	}
	return &fakeRegistry{t: t, root: root}
}

// image records a pushed image and, when tags are given, the tags naming it.
func (self *fakeRegistry) image(digest string, label string, tags ...string) {
	line := digest + " " + label + "\n"
	for _, name := range append([]string{"digest-" + digest}, prefixAll("tag-", tags)...) {
		if err := os.WriteFile(filepath.Join(self.root, "registry", name), []byte(line), 0644); err != nil {
			self.t.Fatal(err)
		}
	}
}

func prefixAll(prefix string, values []string) []string {
	out := []string{}
	for _, value := range values {
		out = append(out, prefix+value)
	}
	return out
}

func (self *fakeRegistry) tagDigest(tag string) string {
	data, err := os.ReadFile(filepath.Join(self.root, "registry", "tag-"+tag))
	if err != nil {
		return ""
	}
	return strings.Fields(string(data))[0]
}

func (self *fakeRegistry) creates() string {
	data, _ := os.ReadFile(filepath.Join(self.root, "creates"))
	return string(data)
}

func (self *fakeRegistry) promote(version string, extraEnv ...string) (string, error) {
	command := exec.Command("bash", filepath.Join(communityProviderDir(self.t), "promote.sh"), version)
	command.Env = append(os.Environ(),
		"PATH="+filepath.Join(self.root, "bin")+":"+os.Getenv("PATH"),
		"FAKE_REGISTRY="+filepath.Join(self.root, "registry"),
		"FAKE_LOG="+filepath.Join(self.root, "creates"),
		"TMPDIR="+self.t.TempDir(),
	)
	command.Env = append(command.Env, extraEnv...)
	output, err := command.CombinedOutput()
	return string(output), err
}

var communityProviderTags = []string{"beta-latest", "g1-latest", "g2-latest", "g3-latest", "g4-latest", "latest"}

func TestCommunityProviderPromote(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " required")
		}
	}
	const version = "2026.11.19-1103608200"
	const label = "2026.11.19+1103608200"

	t.Run("moves every tag off the unlabelled old image, then is a no-op", func(t *testing.T) {
		registry := newFakeRegistry(t)
		// the images up to 2026.7.2 carry only the base image's label
		registry.image("sha256:old", "22.04", append([]string{"2026.7.2-982300760"}, communityProviderTags...)...)
		registry.image("sha256:new", label, version)
		output, err := registry.promote(version)
		if err != nil {
			t.Fatalf("promote failed: %v\n%s", err, output)
		}
		for _, tag := range communityProviderTags {
			if got := registry.tagDigest(tag); got != "sha256:new" {
				t.Errorf("%s names %q", tag, got)
			}
		}
		if got := registry.tagDigest("2026.7.2-982300760"); got != "sha256:old" {
			t.Errorf("the old version tag moved to %q", got)
		}
		if strings.Count(registry.creates(), "create ") != len(communityProviderTags) {
			t.Fatalf("creates:\n%s", registry.creates())
		}
		output, err = registry.promote(version)
		if err != nil {
			t.Fatalf("second promote failed: %v\n%s", err, output)
		}
		if strings.Count(registry.creates(), "create ") != len(communityProviderTags) {
			t.Fatalf("a repeated promote created tags again:\n%s", registry.creates())
		}
	})

	t.Run("never moves a tag back from a newer version", func(t *testing.T) {
		registry := newFakeRegistry(t)
		registry.image("sha256:old", "", communityProviderTags...)
		registry.image("sha256:newer", "2026.12.1+1110000000", "latest", "g4-latest")
		registry.image("sha256:new", label, version)
		output, err := registry.promote(version)
		if err != nil {
			t.Fatalf("promote failed: %v\n%s", err, output)
		}
		for _, tag := range []string{"latest", "g4-latest"} {
			if got := registry.tagDigest(tag); got != "sha256:newer" {
				t.Errorf("%s moved back to %q", tag, got)
			}
		}
		for _, tag := range []string{"beta-latest", "g1-latest", "g2-latest", "g3-latest"} {
			if got := registry.tagDigest(tag); got != "sha256:new" {
				t.Errorf("%s names %q", tag, got)
			}
		}
		if !strings.Contains(output, "left at the newer version 2026.12.1+1110000000") {
			t.Fatalf("output:\n%s", output)
		}
	})

	t.Run("creates a missing tag", func(t *testing.T) {
		registry := newFakeRegistry(t)
		registry.image("sha256:new", label, version)
		output, err := registry.promote(version)
		if err != nil {
			t.Fatalf("promote failed: %v\n%s", err, output)
		}
		if got := registry.tagDigest("latest"); got != "sha256:new" {
			t.Fatalf("latest names %q\n%s", got, output)
		}
	})

	t.Run("refuses a version whose image is another build", func(t *testing.T) {
		registry := newFakeRegistry(t)
		registry.image("sha256:old", "", communityProviderTags...)
		registry.image("sha256:other", "2026.11.19+1103608210", version)
		output, err := registry.promote(version)
		if err == nil || !strings.Contains(output, "not version code 1103608200") {
			t.Fatalf("promote did not refuse: %v\n%s", err, output)
		}
		if registry.creates() != "" {
			t.Fatalf("tags moved:\n%s", registry.creates())
		}
	})

	t.Run("refuses a version without a version code", func(t *testing.T) {
		registry := newFakeRegistry(t)
		if output, err := registry.promote("latest"); err == nil {
			t.Fatalf("promote accepted a tag name\n%s", output)
		}
	})

	t.Run("a registry error is not a missing tag", func(t *testing.T) {
		registry := newFakeRegistry(t)
		registry.image("sha256:new", label, version)
		output, err := registry.promote(version, "FAKE_INSPECT_ERROR=error getting credentials: unauthorized")
		if err == nil {
			t.Fatalf("promote ignored the registry error\n%s", output)
		}
		if registry.creates() != "" {
			t.Fatalf("tags moved:\n%s", registry.creates())
		}
	})
}

// make build stages the release's binaries and refuses a binary that is not
// the provider of WARP_VERSION.
func TestCommunityProviderMakefileVersionGate(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make required")
	}
	binaries := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		writeCommunityExecutable(t, filepath.Join(binaries, "linux", arch, "provider"),
			"\x7fELF fake provider main.Version=2026.11.19+1103608200 "+arch+"\n")
	}
	run := func(version string) (string, error) {
		work := t.TempDir()
		for _, name := range []string{"Makefile", "Dockerfile", "bringyour-provider.sh"} {
			data, err := os.ReadFile(filepath.Join(communityProviderDir(t), name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, name), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		command := exec.Command("make", "all", "PROVIDER_BINARIES="+binaries, "WARP_VERSION="+version)
		command.Dir = work
		output, err := command.CombinedOutput()
		if err == nil {
			for _, arch := range []string{"amd64", "arm64"} {
				if _, statErr := os.Stat(filepath.Join(work, "build", "linux", arch, "provider")); statErr != nil {
					t.Fatalf("%s not staged: %v", arch, statErr)
				}
			}
		}
		return string(output), err
	}
	if output, err := run("2026.11.19+1103608200"); err != nil {
		t.Fatalf("the matching version was refused: %v\n%s", err, output)
	}
	if output, err := run("2026.11.19+1103608201"); err == nil || !strings.Contains(output, "is not provider 2026.11.19+1103608201") {
		t.Fatalf("another version was accepted: %v\n%s", err, output)
	}
}

// The release stage, run from run.sh with its release helpers faked: the
// order of its effects, what each failure leaves, and that it never stops
// the release.
func TestCommunityProviderReleaseStage(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh required")
	}
	stage := componentRegion(t, "community_provider_image () {", "\n\n\n# Warp services")
	harness := `
release_build() { print -r -- "release_build $*" >> "$EVENTS"; return "${FAKE_BUILD:-0}"; }
release_complete() { print -r -- "release_complete $*" >> "$EVENTS"; }
release_image_publication_begin() { print -r -- "begin $*" >> "$EVENTS"; }
release_image_publication_complete() { print -r -- "complete $*" >> "$EVENTS"; }
release_image_receipt() { print -r -- "receipt $*" >> "$EVENTS"; }
builder_message() { print -r -- "message $*" >> "$EVENTS"; }
govulncheck() { print -r -- "govulncheck $*" >> "$EVENTS"; return "${FAKE_VULN:-0}"; }
warpctl() { print -r -- "warpctl $* PROVIDER_BINARIES=$PROVIDER_BINARIES pwd=$PWD" >> "$EVENTS"; return "${FAKE_WARPCTL:-0}"; }
`
	run := func(t *testing.T, env ...string) []string {
		home := t.TempDir()
		events := filepath.Join(home, "events")
		writeCommunityExecutable(t, filepath.Join(home, "all", "community", "provider", "promote.sh"),
			"#!/bin/sh\necho \"promote $*\" >> \"$EVENTS\"\n")
		command := exec.Command(zsh, "-c", harness+stage+"\nprint -r -- 'release continues' >> \"$EVENTS\"\n")
		command.Env = append(os.Environ(),
			"BUILD_HOME="+home, "WARP_HOME="+home, "EVENTS="+events,
			"EXTERNAL_WARP_VERSION=2026.11.19-1103608200", "GO_MOD_SUFFIX=/v2026", "BUILD_ENV=main")
		command.Env = append(command.Env, env...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("stage failed: %v\n%s", err, output)
		}
		data, err := os.ReadFile(events)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for i, line := range lines {
			lines[i] = strings.ReplaceAll(line, home, "$BUILD_HOME")
		}
		return lines
	}
	expectEvents := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	receipt := "$BUILD_HOME/.release-checkpoints/2026.11.19-1103608200/image-community-provider.json"
	binaries := "$BUILD_HOME/sn/v2026/cli/miner/build"

	t.Run("publishes and promotes", func(t *testing.T) {
		expectEvents(t, run(t),
			"release_build image-community-provider "+receipt,
			"govulncheck -mode=binary "+binaries+"/linux/amd64/provider",
			"govulncheck -mode=binary "+binaries+"/linux/arm64/provider",
			"begin community-provider community-provider",
			"warpctl build community all/community/provider/Makefile PROVIDER_BINARIES="+binaries+" pwd=$BUILD_HOME",
			"complete community-provider community-provider",
			"release_complete image-community-provider "+receipt,
			"receipt community-provider community-provider",
			"promote 2026.11.19-1103608200",
			"message community provider image `2026.11.19-1103608200` published: bringyour/community-provider latest, beta-latest and g1-latest to g4-latest",
			"release continues")
	})

	t.Run("a resumed release promotes the retained image", func(t *testing.T) {
		expectEvents(t, run(t, "FAKE_BUILD=1"),
			"release_build image-community-provider "+receipt,
			"receipt community-provider community-provider",
			"promote 2026.11.19-1103608200",
			"message community provider image `2026.11.19-1103608200` published: bringyour/community-provider latest, beta-latest and g1-latest to g4-latest",
			"release continues")
	})

	t.Run("a govulncheck refusal records no publication and moves no tag", func(t *testing.T) {
		got := run(t, "FAKE_VULN=3")
		for _, line := range got {
			if strings.HasPrefix(line, "begin ") || strings.HasPrefix(line, "warpctl ") || strings.HasPrefix(line, "promote ") {
				t.Fatalf("refused binaries reached %q:\n%s", line, strings.Join(got, "\n"))
			}
		}
		if got[len(got)-2] != "message warning: community provider image `2026.11.19-1103608200` not published (errors above); its tags stay on the last image. Build will continue." ||
			got[len(got)-1] != "release continues" {
			t.Fatalf("events:\n%s", strings.Join(got, "\n"))
		}
	})

	t.Run("a failed build moves no tag", func(t *testing.T) {
		got := run(t, "FAKE_WARPCTL=1")
		for _, line := range got {
			if strings.HasPrefix(line, "complete ") || strings.HasPrefix(line, "release_complete ") || strings.HasPrefix(line, "promote ") {
				t.Fatalf("a failed build reached %q:\n%s", line, strings.Join(got, "\n"))
			}
		}
		if got[len(got)-1] != "release continues" {
			t.Fatalf("events:\n%s", strings.Join(got, "\n"))
		}
	})

	t.Run("only the main release publishes it", func(t *testing.T) {
		expectEvents(t, run(t, "BUILD_ENV=beta"), "release continues")
	})
}
