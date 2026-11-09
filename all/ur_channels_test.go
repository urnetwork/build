// SPDX-License-Identifier: MPL-2.0

// ur's v1 package channels (REPO-M6-01): ur-channels.zsh renders a Homebrew cask and formula and
// winget's manifests from a release that ur-sign-release.zsh signed and ur-promote.zsh promoted, both
// run for real here with a throwaway key, and from nothing else. apt and yum are not v1 channels
// (plan-REPO-v1-channels, QD-023), so nothing renders a tree for them.

package allbuild

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The goldens, rewritten from a render when UR_CHANNELS_GOLDEN is "update".
func channelGoldenDir(t *testing.T) string {
	return filepath.Join(rolloutRoot(t), "testdata", "ur-channels")
}

// A release signed with a throwaway key and promoted into a fixture mmm: the key's private half, its
// published half and the mmm checkout.
func promoteChannelsRelease(t *testing.T) (string, string, string) {
	t.Helper()
	key, public := throwawayReleaseKey(t)
	mmm := newPromoteMmm(t)
	if result := signAndPromote(t, key, public, mmm, urTestVersion); result.exitCode != 0 {
		t.Fatalf("promoting %s failed: %+v", urTestVersion, result)
	}
	return key, public, mmm
}

func promotedDir(mmm string, version string) string {
	return filepath.Join(mmm, "ur.io", "astro", "public", "recurd", "ur", version)
}

// ur-channels.zsh with curl and wget ahead of the rest of PATH, each logging its call and failing,
// so any download shows: the result, and the downloads it tried.
func runChannels(t *testing.T, releaseKey string, mmm string, out string, version string) (componentResult, []string) {
	t.Helper()
	stubs := t.TempDir()
	downloads := filepath.Join(stubs, "downloads.log")
	for _, name := range []string{"curl", "wget"} {
		copyTestContent(t, filepath.Join(stubs, name), "#!/bin/sh\nprintf '%s\\n' \""+name+" $*\" >> '"+downloads+"'\nexit 1\n")
		if err := os.Chmod(filepath.Join(stubs, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("zsh", filepath.Join(rolloutRoot(t), "ur-channels.zsh"), version)
	command.Env = environmentWith("PATH="+stubs+":"+os.Getenv("PATH"), "UR_RELEASE_KEY="+releaseKey, "UR_PROMOTE_MMM="+mmm, "UR_CHANNELS_OUT="+out)
	output, err := command.CombinedOutput()
	result := componentResult{output: string(output)}
	if exitError, ok := err.(*exec.ExitError); ok {
		result.exitCode = exitError.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(downloads)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return result, strings.Fields(string(logged))
}

// A promoted release's channels, rendered into a temporary directory.
func renderChannels(t *testing.T) (string, string) {
	t.Helper()
	_, public, mmm := promoteChannelsRelease(t)
	out := filepath.Join(t.TempDir(), "channels")
	result, downloads := runChannels(t, public, mmm, out, urTestVersion)
	if result.exitCode != 0 {
		t.Fatalf("ur-channels.zsh exited %d\n%s", result.exitCode, result.output)
	}
	if len(downloads) != 0 {
		t.Fatalf("ur-channels.zsh downloaded: %v", downloads)
	}
	return out, mmm
}

// Every file under dir, by its slash-separated path inside dir.
func channelFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		files[filepath.ToSlash(relative)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The signed sums, by name.
func promotedSums(t *testing.T, mmm string, version string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(promotedDir(mmm, version), "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		sums[fields[1]] = fields[0]
	}
	return sums
}

func channelPath(name string) string {
	return map[string]string{
		"cask":      "homebrew/Casks/ur.rb",
		"formula":   "homebrew/Formula/ur.rb",
		"installer": "winget/manifests/u/URnetwork/ur/" + urTestVersion + "/URnetwork.ur.installer.yaml",
		"locale":    "winget/manifests/u/URnetwork/ur/" + urTestVersion + "/URnetwork.ur.locale.en-US.yaml",
		"version":   "winget/manifests/u/URnetwork/ur/" + urTestVersion + "/URnetwork.ur.yaml",
	}[name]
}

func TestChannelsRenderTheGoldens(t *testing.T) {
	out, _ := renderChannels(t)
	rendered := channelFiles(t, out)
	if os.Getenv("UR_CHANNELS_GOLDEN") == "update" {
		if err := os.RemoveAll(channelGoldenDir(t)); err != nil {
			t.Fatal(err)
		}
		for name, content := range rendered {
			path := filepath.Join(channelGoldenDir(t), filepath.FromSlash(name))
			copyTestContent(t, path, content)
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Log("rewrote the goldens")
	}
	goldens := channelFiles(t, channelGoldenDir(t))
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range rendered {
			if !yield(name) {
				return
			}
		}
	})
	goldenNames := slices.Sorted(func(yield func(string) bool) {
		for name := range goldens {
			if !yield(name) {
				return
			}
		}
	})
	if !slices.Equal(names, goldenNames) {
		t.Fatalf("rendered %v, and the goldens are %v", names, goldenNames)
	}
	for _, name := range names {
		if rendered[name] != goldens[name] {
			t.Errorf("%s differs from its golden (UR_CHANNELS_GOLDEN=update rewrites them)\nrendered:\n%s\ngolden:\n%s", name, rendered[name], goldens[name])
		}
	}
	for _, name := range []string{"cask", "formula", "installer", "locale", "version"} {
		if _, found := rendered[channelPath(name)]; !found {
			t.Errorf("no %s at %s", name, channelPath(name))
		}
	}
}

// Every checksum a channel carries is the signed sums' line for the artifact beside it, and nothing
// downloads: ur-channels.zsh never sees an artifact at all.
func TestChannelsTakeEveryChecksumFromTheSums(t *testing.T) {
	out, mmm := renderChannels(t)
	sums := promotedSums(t, mmm, urTestVersion)
	rendered := channelFiles(t, out)
	name := func(system string, arch string, extension string) string {
		return "ur-" + urTestVersion + "-" + system + "-" + arch + "." + extension
	}

	pairs := map[string]string{}
	formula := regexp.MustCompile(`url "[^"]*/([^/"]+)"\s*\n\s*sha256 "([0-9a-f]{64})"`).FindAllStringSubmatch(rendered[channelPath("formula")], -1)
	for _, match := range formula {
		pairs[match[1]] = match[2]
	}
	cask := regexp.MustCompile(`sha256 arm:\s+"([0-9a-f]{64})",\s*intel:\s+"([0-9a-f]{64})"`).FindStringSubmatch(rendered[channelPath("cask")])
	if cask == nil || !strings.Contains(rendered[channelPath("cask")], `arch arm: "arm64", intel: "amd64"`) ||
		!strings.Contains(rendered[channelPath("cask")], `/ur-#{version}-darwin-#{arch}.tar.gz"`) {
		t.Fatalf("the cask names no arch's archive and sum:\n%s", rendered[channelPath("cask")])
	}
	pairs[name("darwin", "arm64", "tar.gz")] = cask[1]
	pairs[name("darwin", "amd64", "tar.gz")] = cask[2]
	for _, match := range regexp.MustCompile(`InstallerUrl: \S*/([^/\s]+)\n\s*InstallerSha256: ([0-9A-F]{64})\n`).FindAllStringSubmatch(rendered[channelPath("installer")], -1) {
		pairs[match[1]] = strings.ToLower(match[2])
	}
	want := []string{
		name("darwin", "amd64", "tar.gz"), name("darwin", "arm64", "tar.gz"),
		name("linux", "amd64", "tar.gz"), name("linux", "arm64", "tar.gz"),
		name("windows", "amd64", "zip"), name("windows", "arm64", "zip"),
	}
	if names := slices.Sorted(func(yield func(string) bool) {
		for name := range pairs {
			if !yield(name) {
				return
			}
		}
	}); !slices.Equal(names, want) {
		t.Fatalf("the channels name %v, not %v", names, want)
	}
	for artifact, sum := range pairs {
		if sums[artifact] == "" || sum != sums[artifact] {
			t.Errorf("the channels carry %s for %s, and the signed sums %q", sum, artifact, sums[artifact])
		}
	}
	// no other checksum anywhere
	listed := map[string]bool{}
	for _, sum := range sums {
		listed[sum] = true
	}
	for path, content := range rendered {
		for _, token := range regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`).FindAllString(content, -1) {
			if !listed[strings.ToLower(token)] {
				t.Errorf("%s carries %s, which the signed sums do not list", path, token)
			}
		}
	}
	// nothing in the script could fetch one
	script, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur-channels.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(script), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if regexp.MustCompile(`\b(curl|wget|gh)\b|https?://`).MatchString(line) {
			t.Errorf("ur-channels.zsh may download: %q", line)
		}
	}
}

// A release that is not signed by the published key, or not promoted, renders nothing, and an
// earlier render stays as it was.
func TestChannelsRefuseAnUnsignedRelease(t *testing.T) {
	for _, refusal := range []struct {
		label  string
		spoil  func(t *testing.T, key string, public string, mmm string) (string, string)
		reason string
	}{
		{label: "no signature", reason: "holds no SHA256SUMS.sig", spoil: func(t *testing.T, key string, public string, mmm string) (string, string) {
			if err := os.Remove(filepath.Join(promotedDir(mmm, urTestVersion), "SHA256SUMS.sig")); err != nil {
				t.Fatal(err)
			}
			return public, urTestVersion
		}},
		{label: "another key's signature", reason: "does not verify", spoil: func(t *testing.T, key string, public string, mmm string) (string, string) {
			otherKey, _ := throwawayReleaseKey(t)
			sums := filepath.Join(promotedDir(mmm, urTestVersion), "SHA256SUMS")
			os.Remove(sums + ".sig")
			if output, err := exec.Command("ssh-keygen", "-q", "-Y", "sign", "-f", otherKey, "-n", "ur-release", sums).CombinedOutput(); err != nil {
				t.Fatalf("ssh-keygen: %v\n%s", err, output)
			}
			return public, urTestVersion
		}},
		{label: "sums changed after signing", reason: "does not verify", spoil: func(t *testing.T, key string, public string, mmm string) (string, string) {
			path := filepath.Join(promotedDir(mmm, urTestVersion), "SHA256SUMS")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sums := promotedSums(t, mmm, urTestVersion)
			changed := strings.Replace(string(data), sums["ur-"+urTestVersion+"-darwin-arm64.tar.gz"], strings.Repeat("0", 64), 1)
			copyTestContent(t, path, changed)
			return public, urTestVersion
		}},
		{label: "no published key", reason: "the release key ceremony (EXT-3) has not run", spoil: func(t *testing.T, key string, public string, mmm string) (string, string) {
			return filepath.Join(t.TempDir(), "RELEASE-KEY.pub"), urTestVersion
		}},
		{label: "a release never promoted", reason: "only a promoted release has channels", spoil: func(t *testing.T, key string, public string, mmm string) (string, string) {
			return public, "2026.11.7-7654321"
		}},
	} {
		key, public, mmm := promoteChannelsRelease(t)
		releaseKey, version := refusal.spoil(t, key, public, mmm)
		out := filepath.Join(t.TempDir(), "channels")
		copyTestContent(t, filepath.Join(out, "earlier-render"), "an earlier render\n")
		result, downloads := runChannels(t, releaseKey, mmm, out, version)
		if result.exitCode != 1 || !strings.Contains(result.output, refusal.reason) {
			t.Errorf("%s: ur-channels.zsh exited %d without saying %q\n%s", refusal.label, result.exitCode, refusal.reason, result.output)
		}
		if files := channelFiles(t, out); len(files) != 1 || files["earlier-render"] != "an earlier render\n" {
			t.Errorf("%s: the refusal changed the output: %v", refusal.label, files)
		}
		if leftovers, _ := filepath.Glob(out + ".rendering.*"); len(leftovers) != 0 {
			t.Errorf("%s: the refusal left %v", refusal.label, leftovers)
		}
		if len(downloads) != 0 {
			t.Errorf("%s: ur-channels.zsh downloaded %v", refusal.label, downloads)
		}
	}
}

// Signed sums that lack an artifact a channel needs render nothing either.
func TestChannelsRefuseSumsWithoutAnArtifact(t *testing.T) {
	key, public, mmm := promoteChannelsRelease(t)
	path := filepath.Join(promotedDir(mmm, urTestVersion), "SHA256SUMS")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasSuffix(line, "-windows-arm64.zip") {
			lines = append(lines, line)
		}
	}
	copyTestContent(t, path, strings.Join(lines, "\n"))
	os.Remove(path + ".sig")
	if output, err := exec.Command("ssh-keygen", "-q", "-Y", "sign", "-f", key, "-n", "ur-release", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}
	out := filepath.Join(t.TempDir(), "channels")
	result, _ := runChannels(t, public, mmm, out, urTestVersion)
	if result.exitCode != 1 || !strings.Contains(result.output, "lists no one ur-"+urTestVersion+"-windows-arm64.zip") {
		t.Errorf("ur-channels.zsh exited %d over sums without the arm64 zip\n%s", result.exitCode, result.output)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refusal wrote %s (%v)", out, err)
	}
}

// The cask installs the notarized bundle whole and links both names to its executable, and parses as
// Ruby; the release's darwin archive holds that bundle at its top.
func TestCaskInstallsTheBundleAndBothBinaries(t *testing.T) {
	out, _ := renderChannels(t)
	cask := channelFiles(t, out)[channelPath("cask")]
	binaries := regexp.MustCompile(`(?m)^\s*binary (.*)$`).FindAllStringSubmatch(cask, -1)
	got := []string{}
	for _, binary := range binaries {
		got = append(got, binary[1])
	}
	if want := []string{`"ur.app/Contents/MacOS/ur", target: "ur"`, `"ur.app/Contents/MacOS/ur", target: "git-remote-ur"`}; !slices.Equal(got, want) {
		t.Errorf("the cask's binary stanzas are %v, not %v", got, want)
	}
	for _, unwanted := range []string{"\n  app ", "\n  pkg ", "\n  installer ", "\n  artifact ", "linux"} {
		if strings.Contains(cask, unwanted) {
			t.Errorf("the cask holds %q: it installs the bundle as the archive holds it, links and nothing more", strings.TrimSpace(unwanted))
		}
	}
	for _, want := range []string{
		`cask "ur" do`,
		`url "https://github.com/urnetwork/build/releases/download/v#{version}/ur-#{version}-darwin-#{arch}.tar.gz"`,
		`depends_on macos: ">= :monterey"`,
		`version "` + urTestVersion + `"`,
	} {
		if !strings.Contains(cask, want) {
			t.Errorf("the cask lacks %s", want)
		}
	}
	macos, err := os.ReadFile(filepath.Join(rolloutRoot(t), "build-ur-macos.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(macos), `"ur.app=$app"`) {
		t.Error("build-ur-macos.sh's .tar.gz no longer holds ur.app at its top, where the cask's binary stanzas look")
	}
	assertRubyParses(t, filepath.Join(out, filepath.FromSlash(channelPath("cask"))))
}

// The formula is Linux's alone and installs the release's binary, never a build: on macOS a bare
// binary would lose the bundle's profile and the keychain with it (D-CORE-12).
func TestFormulaIsLinuxOnly(t *testing.T) {
	out, _ := renderChannels(t)
	formula := channelFiles(t, out)[channelPath("formula")]
	for _, want := range []string{
		"class Ur < Formula",
		"depends_on :linux",
		`bin.install "ur"`,
		`bin.install_symlink "ur" => "git-remote-ur"`,
		`url "https://github.com/urnetwork/build/releases/download/v` + urTestVersion + `/ur-` + urTestVersion + `-linux-amd64.tar.gz"`,
		`url "https://github.com/urnetwork/build/releases/download/v` + urTestVersion + `/ur-` + urTestVersion + `-linux-arm64.tar.gz"`,
	} {
		if !strings.Contains(formula, want) {
			t.Errorf("the formula lacks %s", want)
		}
	}
	for _, unwanted := range []string{"darwin", "on_macos", "system ", "go build", "depends_on \"go\"", "head "} {
		if strings.Contains(formula, unwanted) {
			t.Errorf("the formula holds %q", unwanted)
		}
	}
	assertRubyParses(t, filepath.Join(out, filepath.FromSlash(channelPath("formula"))))
}

// winget installs the zip's two exes as portables under their own aliases, so git finds
// git-remote-ur on PATH, and the three manifests agree.
func TestWingetHasBothAliases(t *testing.T) {
	out, _ := renderChannels(t)
	rendered := channelFiles(t, out)
	installer := rendered[channelPath("installer")]
	for _, want := range []string{
		"InstallerType: zip\nNestedInstallerType: portable\n",
		"NestedInstallerFiles:\n" +
			"  - RelativeFilePath: ur.exe\n    PortableCommandAlias: ur\n" +
			"  - RelativeFilePath: git-remote-ur.exe\n    PortableCommandAlias: git-remote-ur\n" +
			"Installers:\n",
		"  - Architecture: x64\n    InstallerUrl: https://github.com/urnetwork/build/releases/download/v" + urTestVersion + "/ur-" + urTestVersion + "-windows-amd64.zip\n",
		"  - Architecture: arm64\n    InstallerUrl: https://github.com/urnetwork/build/releases/download/v" + urTestVersion + "/ur-" + urTestVersion + "-windows-arm64.zip\n",
	} {
		if !strings.Contains(installer, want) {
			t.Errorf("the installer manifest lacks:\n%s", want)
		}
	}
	windows, err := os.ReadFile(filepath.Join(rolloutRoot(t), "build-ur-windows.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{`"ur.exe=`, `"git-remote-ur.exe=`} {
		if !strings.Contains(string(windows), entry) {
			t.Errorf("build-ur-windows.sh's zip no longer holds %s at its top, where the manifest looks", entry)
		}
	}
	for _, name := range []string{"installer", "locale", "version"} {
		manifest := rendered[channelPath(name)]
		for _, want := range []string{"PackageIdentifier: URnetwork.ur\n", "PackageVersion: " + urTestVersion + "\n", "ManifestVersion: 1.6.0\n"} {
			if !strings.Contains(manifest, want) {
				t.Errorf("the %s manifest lacks %q", name, strings.TrimSpace(want))
			}
		}
	}
	if !strings.Contains(rendered[channelPath("version")], "DefaultLocale: en-US\n") || !strings.Contains(rendered[channelPath("locale")], "PackageLocale: en-US\n") {
		t.Error("the version manifest's default locale is not the locale manifest's")
	}
}

// ruby -c, where this host has a ruby.
func assertRubyParses(t *testing.T, path string) {
	t.Helper()
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Logf("no ruby here, so %s is not parsed", filepath.Base(path))
		return
	}
	if output, err := exec.Command(ruby, "-c", path).CombinedOutput(); err != nil {
		t.Errorf("ruby -c %s: %v\n%s", path, err, output)
	}
}
