// SPDX-License-Identifier: MPL-2.0

// ur's release integrity (REPO-M3-01): the sums run.sh makes after every artifact, the offline
// ceremony's ur-sign-release.zsh and ur-promote.zsh, round trip and refusals. Every key here is a
// throwaway one, generated in a temp directory and gone with it: no key touches the build host's
// home, the repository or vault.

package allbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A throwaway Ed25519 key in a temp directory, and its public line.
func throwawayReleaseKey(t *testing.T) (string, string) {
	t.Helper()
	key := filepath.Join(t.TempDir(), "release-key")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ur-release", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}
	return key, key + ".pub"
}

// A release as run.sh leaves it: the table's artifacts, and ursums's SHA256SUMS and ur-release.json.
func writeSignTestRelease(t *testing.T, version string) string {
	t.Helper()
	artifacts := t.TempDir()
	for _, name := range urTableNames(t, version) {
		copyTestContent(t, filepath.Join(artifacts, name), "bytes of "+name+"\n")
	}
	ursums := filepath.Join(t.TempDir(), "ursums")
	build := exec.Command("go", "build", "-o", ursums, "./ursums")
	build.Dir = rolloutRoot(t)
	build.Env = environmentWith("GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ursums: %v\n%s", err, output)
	}
	release := t.TempDir()
	if output, err := exec.Command(ursums, "-dir", artifacts, "-version", version, "-table", filepath.Join(rolloutRoot(t), "ur", "artifacts.txt"), "-o", release).CombinedOutput(); err != nil {
		t.Fatalf("ursums: %v\n%s", err, output)
	}
	return release
}

func runSign(t *testing.T, typed string, arguments ...string) componentResult {
	t.Helper()
	command := exec.Command("zsh", append([]string{filepath.Join(rolloutRoot(t), "ur-sign-release.zsh")}, arguments...)...)
	command.Stdin = strings.NewReader(typed + "\n")
	command.Env = environmentWith("UR_RELEASE_KEY=")
	output, err := command.CombinedOutput()
	result := componentResult{output: string(output)}
	if exitError, ok := err.(*exec.ExitError); ok {
		result.exitCode = exitError.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result
}

// A clean mmm checkout to promote into.
func newPromoteMmm(t *testing.T) string {
	t.Helper()
	mmm := t.TempDir()
	copyTestContent(t, filepath.Join(mmm, "ur.io", "astro", "public", "index.md"), "ur.io\n")
	for _, arguments := range [][]string{{"init", "-q", "-b", "sidecar"}, {"add", "-A"}, {"commit", "-q", "-m", "Start the site"}} {
		command := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, arguments...)...)
		command.Dir = mmm
		command.Env = branchesGitEnvironment()
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	return mmm
}

func runPromote(t *testing.T, releaseKey string, mmm string, arguments ...string) componentResult {
	t.Helper()
	command := exec.Command("zsh", append([]string{filepath.Join(rolloutRoot(t), "ur-promote.zsh")}, arguments...)...)
	command.Env = branchesGitEnvironment("UR_RELEASE_KEY="+releaseKey, "UR_PROMOTE_MMM="+mmm)
	output, err := command.CombinedOutput()
	result := componentResult{output: string(output)}
	if exitError, ok := err.(*exec.ExitError); ok {
		result.exitCode = exitError.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result
}

// mmm's HEAD and status, which a refusal leaves as they were.
func mmmState(t *testing.T, mmm string) string {
	t.Helper()
	state := ""
	for _, arguments := range [][]string{{"rev-parse", "HEAD"}, {"status", "--porcelain", "--untracked-files=all"}} {
		output, err := exec.Command("git", append([]string{"-C", mmm}, arguments...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
		state += string(output)
	}
	return state
}

func signAndPromote(t *testing.T, key string, releaseKey string, mmm string, version string) componentResult {
	t.Helper()
	release := writeSignTestRelease(t, version)
	if result := runSign(t, version, "sign", version, key, release); result.exitCode != 0 {
		t.Fatalf("signing %s failed: %+v", version, result)
	}
	return runPromote(t, releaseKey, mmm, version, filepath.Join(release, "SHA256SUMS.sig"))
}

func TestSignThenVerifyRoundTrip(t *testing.T) {
	key, public := throwawayReleaseKey(t)
	release := writeSignTestRelease(t, urTestVersion)
	signed := runSign(t, urTestVersion, "sign", urTestVersion, key, release)
	if signed.exitCode != 0 || !strings.Contains(signed.output, "Both holders compare these sums") {
		t.Fatalf("signing failed: %+v", signed)
	}
	verify := exec.Command("ssh-keygen", "-Y", "check-novalidate", "-n", "ur-release", "-s", filepath.Join(release, "SHA256SUMS.sig"))
	sums, err := os.Open(filepath.Join(release, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	defer sums.Close()
	verify.Stdin = sums
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("the signature is not ur-release's: %v\n%s", err, output)
	}

	mmm := newPromoteMmm(t)
	promoted := runPromote(t, public, mmm, urTestVersion, filepath.Join(release, "SHA256SUMS.sig"))
	if promoted.exitCode != 0 {
		t.Fatalf("promotion failed: %+v", promoted)
	}
	base := filepath.Join(mmm, "ur.io", "astro", "public", "recurd", "ur")
	for _, pair := range [][2]string{
		{filepath.Join(base, urTestVersion, "SHA256SUMS"), filepath.Join(release, "SHA256SUMS")},
		{filepath.Join(base, urTestVersion, "SHA256SUMS.sig"), filepath.Join(release, "SHA256SUMS.sig")},
		{filepath.Join(base, "latest.json"), filepath.Join(release, "ur-release.json")},
	} {
		staged, err := os.ReadFile(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if string(staged) != string(original) {
			t.Errorf("%s is not %s", pair[0], pair[1])
		}
	}
	if status := mmmState(t, mmm); !strings.HasSuffix(status, "\n") || strings.Count(status, "\n") != 1 {
		t.Errorf("the promotion left mmm uncommitted: %q", status)
	}
	if !strings.Contains(promoted.output, "owner: git -C "+mmm+" push origin sidecar") {
		t.Errorf("the promotion printed no push for the owner:\n%s", promoted.output)
	}
}

func TestSignRefusesAVersionTheHoldersDidNotType(t *testing.T) {
	key, _ := throwawayReleaseKey(t)
	release := writeSignTestRelease(t, urTestVersion)
	result := runSign(t, "2026.11.6-7654321", "sign", urTestVersion, key, release)
	if result.exitCode == 0 {
		t.Fatalf("a version nobody typed was signed: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(release, "SHA256SUMS.sig")); !os.IsNotExist(err) {
		t.Errorf("a refused ceremony wrote a signature")
	}
}

func TestPromoteRefusesAWrongKey(t *testing.T) {
	key, _ := throwawayReleaseKey(t)
	_, otherPublic := throwawayReleaseKey(t)
	mmm := newPromoteMmm(t)
	before := mmmState(t, mmm)
	result := signAndPromote(t, key, otherPublic, mmm, urTestVersion)
	if result.exitCode == 0 || !strings.Contains(result.output, "does not verify") {
		t.Fatalf("a signature by another key was promoted: %+v", result)
	}
	if mmmState(t, mmm) != before {
		t.Errorf("a refused promotion changed mmm")
	}
}

func TestPromoteRefusesATamperedSums(t *testing.T) {
	key, public := throwawayReleaseKey(t)
	release := writeSignTestRelease(t, urTestVersion)
	if result := runSign(t, urTestVersion, "sign", urTestVersion, key, release); result.exitCode != 0 {
		t.Fatalf("signing failed: %+v", result)
	}
	sums, err := os.ReadFile(filepath.Join(release, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := regexp.MustCompile(`(?m)^[0-9a-f]{64}(  ur-2026\.11\.6-1234567-linux-amd64\.tar\.gz)$`).ReplaceAll(sums, []byte(strings.Repeat("0", 64)+"$1"))
	if string(tampered) == string(sums) {
		t.Fatal("nothing was tampered")
	}
	if err := os.WriteFile(filepath.Join(release, "SHA256SUMS"), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	mmm := newPromoteMmm(t)
	before := mmmState(t, mmm)
	result := runPromote(t, public, mmm, urTestVersion, filepath.Join(release, "SHA256SUMS.sig"))
	if result.exitCode == 0 {
		t.Fatalf("tampered sums were promoted: %+v", result)
	}
	if mmmState(t, mmm) != before {
		t.Errorf("a refused promotion changed mmm")
	}
}

func TestPromoteRefusesAnOlderVersion(t *testing.T) {
	key, public := throwawayReleaseKey(t)
	mmm := newPromoteMmm(t)
	if result := signAndPromote(t, key, public, mmm, "2026.11.6-1234567"); result.exitCode != 0 {
		t.Fatalf("the first promotion failed: %+v", result)
	}
	before := mmmState(t, mmm)
	for _, older := range []string{"2026.11.5-9999999", "2026.11.6-1234566", "2025.12.31-1"} {
		result := signAndPromote(t, key, public, mmm, older)
		if result.exitCode == 0 || !strings.Contains(result.output, "would move it backward") {
			t.Fatalf("latest.json moved back to %s: %+v", older, result)
		}
	}
	if mmmState(t, mmm) != before {
		t.Errorf("a refused promotion changed mmm")
	}
	if result := signAndPromote(t, key, public, mmm, "2026.11.10-1"); result.exitCode != 0 {
		t.Fatalf("a newer release was refused: %+v", result)
	}
}

// Every refusal leaves mmm as it was: a missing key, a dirty checkout, another version's json.
func TestPromoteTouchesNothingOnRefusal(t *testing.T) {
	key, public := throwawayReleaseKey(t)
	release := writeSignTestRelease(t, urTestVersion)
	if result := runSign(t, urTestVersion, "sign", urTestVersion, key, release); result.exitCode != 0 {
		t.Fatalf("signing failed: %+v", result)
	}
	signature := filepath.Join(release, "SHA256SUMS.sig")
	mmm := newPromoteMmm(t)
	before := mmmState(t, mmm)

	if result := runPromote(t, filepath.Join(t.TempDir(), "RELEASE-KEY.pub"), mmm, urTestVersion, signature); result.exitCode == 0 || !strings.Contains(result.output, "ceremony") {
		t.Errorf("a promotion with no release key went on: %+v", result)
	}
	if result := runPromote(t, public, mmm, "2026.11.7-1234567", signature); result.exitCode == 0 {
		t.Errorf("another version's release was promoted: %+v", result)
	}
	placeholder := filepath.Join(t.TempDir(), "RELEASE-KEY.pub")
	copyTestContent(t, placeholder, "# the key goes here after the ceremony\n")
	if result := runPromote(t, placeholder, mmm, urTestVersion, signature); result.exitCode == 0 {
		t.Errorf("a placeholder key was taken: %+v", result)
	}
	if mmmState(t, mmm) != before {
		t.Fatalf("a refused promotion changed mmm")
	}
	copyTestContent(t, filepath.Join(mmm, "stray.txt"), "stray\n")
	if result := runPromote(t, public, mmm, urTestVersion, signature); result.exitCode == 0 || !strings.Contains(result.output, "changes outside a commit") {
		t.Errorf("a dirty mmm was promoted into: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(mmm, "ur.io", "astro", "public", "recurd")); !os.IsNotExist(err) {
		t.Errorf("a refused promotion wrote into mmm")
	}
}

// RELEASE-KEY.pub does not exist until the ceremony, and its absence is the check that fails closed, so
// no placeholder stands in its place.
func TestNoPlaceholderReleaseKey(t *testing.T) {
	root := rolloutRoot(t)
	found := false
	for _, sidecar := range []string{filepath.Join(root, "..", "sidecar"), filepath.Join(root, "..", "..", "sidecar")} {
		if _, err := os.Stat(filepath.Join(sidecar, "cli", "go.mod")); err != nil {
			continue
		}
		found = true
		path := filepath.Join(sidecar, "RELEASE-KEY.pub")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(data))
		if strings.Count(strings.TrimSpace(string(data)), "\n") != 0 || len(fields) < 2 || fields[0] != "ssh-ed25519" {
			t.Errorf("%s is not one ssh-ed25519 key: a placeholder must not stand in for the ceremony's key", path)
		} else if output, err := exec.Command("ssh-keygen", "-l", "-f", path).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse as a key: %v\n%s", path, err, output)
		}
	}
	if !found {
		t.Skip("no sidecar checkout beside build")
	}
}

// No private key, and no public key standing in for the release key, is in the scripts.
func TestScriptsHoldNoKeyMaterial(t *testing.T) {
	material := regexp.MustCompile(`PRIVATE KEY|BEGIN OPENSSH|ssh-ed25519 AAAA|ssh-rsa AAAA|ecdsa-sha2-nistp[0-9]+ AAAA`)
	for _, name := range []string{"ur-sign-release.zsh", "ur-promote.zsh", "build-ur.sh", "build-ur-macos.sh", "build-ur-linux.sh", "build-ur-windows.sh", "run.sh"} {
		data, err := os.ReadFile(filepath.Join(rolloutRoot(t), name))
		if err != nil {
			t.Fatal(err)
		}
		if location := material.FindIndex(data); location != nil {
			t.Errorf("%s holds key material at byte %d", name, location[0])
		}
	}
}

// The sums come last in run.sh: after build-ur.sh, which signs every artifact, and after every
// artifact's upload, which runs VirusTotal.
func TestSumsRunAfterSigning(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	order := []string{
		`"$BUILD_HOME/all/build-ur.sh"`,
		`github_release_upload "$ur_artifact" "$UR_OUT/$ur_artifact"`,
		`go run ./ursums -dir "$UR_OUT"`,
		`github_release_upload "ur-release.json" "$UR_SUMS_OUT/ur-release.json"`,
		`github_release_upload "SHA256SUMS" "$UR_SUMS_OUT/SHA256SUMS"`,
		"\ngithub_create_release\n",
	}
	previous := -1
	for _, step := range order {
		index := strings.Index(source, step)
		if index < 0 || strings.Count(source, step) != 1 {
			t.Fatalf("run.sh holds %q %d times", step, strings.Count(source, step))
		}
		if index < previous {
			t.Errorf("%q comes before the step it must follow", step)
		}
		previous = index
	}
}
