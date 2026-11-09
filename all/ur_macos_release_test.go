// SPDX-License-Identifier: MPL-2.0

// ur's macOS release (REPO-M1-02): build-ur-macos.sh with recorded stubs for codesign, xcrun, pkgbuild,
// productbuild, productsign, spctl and security, as component_required_test.go stubs the direct download,
// so the chain runs in order and each step's failure is fatal. plutil, ditto and urarchive are real.
// This host holds only Apple Development identities, so a real signed run is external (EXT-1, EXT-4).

package allbuild

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const urMacosApplicationIdentity = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const urMacosInstallerIdentity = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

// Every stub records its step and fails it when FAIL_STEP names it.
const urMacosStubHeader = `#!/bin/sh
record() {
    printf '%s\n' "$1" >> "$UR_STUB_EVENTS"
    if [ "$FAIL_STEP" = "$1" ]; then exit 37; fi
}
last() { for argument in "$@"; do :; done; printf '%s' "$argument"; }
`

var urMacosStubs = map[string]string{
	"security": urMacosStubHeader + `
case "$1" in
    cms)
        record "profile:$(basename "$4" .provisionprofile)"
        cat "$4"
        ;;
    find-identity)
        if [ "$3" = "-p" ]; then
            record identity:application
            [ -n "$UR_STUB_NO_APPLICATION" ] || printf '  1) ` + urMacosApplicationIdentity + ` "Developer ID Application: Synthetic (6BGU69Q742)"\n'
        else
            record identity:installer
            printf '  1) ` + urMacosApplicationIdentity + ` "Developer ID Application: Synthetic (6BGU69Q742)"\n'
            [ -n "$UR_STUB_NO_INSTALLER" ] || printf '  2) ` + urMacosInstallerIdentity + ` "Developer ID Installer: Synthetic (6BGU69Q742)"\n'
        fi
        ;;
    *) record unexpected-security; exit 41 ;;
esac
`,
	"codesign": urMacosStubHeader + `
app=$(last "$@")
case "$*" in
    "--force --options runtime --timestamp --entitlements "*" --sign ` + urMacosApplicationIdentity + ` "*)
        if [ -f "$app/Contents/embedded.provisionprofile" ] && [ -f "$app/Contents/MacOS/ur" ] && [ -f "$app/Contents/Info.plist" ]; then
            record sign-app
        else
            record sign-bare-app
        fi
        ;;
    "--verify --strict --verbose=2 "*) record verify-app ;;
    *) record unexpected-codesign; exit 41 ;;
esac
`,
	"xcrun": urMacosStubHeader + `
case "$*" in
    "notarytool submit "*-notarize.zip*) record notarize-app; printf '  status: %s\n' "${NOTARY_STATUS:-Accepted}" ;;
    "notarytool submit "*.pkg*) record notarize-pkg; printf '  status: %s\n' "${NOTARY_STATUS:-Accepted}" ;;
    "stapler staple "*.app) record staple-app ;;
    "stapler staple "*.pkg) record staple-pkg ;;
    *) record unexpected-xcrun; exit 41 ;;
esac
`,
	"spctl": urMacosStubHeader + `
case "$*" in
    "--assess --type execute -vv "*.app) record assess-app ;;
    "--assess --type install -vv "*.pkg) record assess-pkg ;;
    *) record unexpected-spctl; exit 41 ;;
esac
`,
	"pkgbuild": urMacosStubHeader + `
root=""; scripts=""
while [ "$#" -gt 1 ]; do
    case "$1" in
        --root) root="$2"; shift ;;
        --scripts) scripts="$2"; shift ;;
    esac
    shift
done
if [ -f "$root/usr/local/lib/ur/ur.app/Contents/embedded.provisionprofile" ] && [ -x "$scripts/postinstall" ]; then
    record pkgbuild
else
    record pkgbuild-without-the-bundle
fi
printf 'component\n' > "$1"
`,
	"productbuild": urMacosStubHeader + `
record productbuild
printf 'product\n' > "$(last "$@")"
`,
	"productsign": urMacosStubHeader + `
if [ "$2" = ` + urMacosInstallerIdentity + ` ]; then record productsign; else record productsign-with-another-identity; fi
printf 'signed product\n' > "$(last "$@")"
`,
}

// The healthy chain: the identities and the ur profile once, then per architecture the bundle signed with
// its profile, verified, notarized, stapled and assessed, packed, and the .pkg built, signed with the
// Installer identity, notarized, stapled and assessed.
var urMacosArchSteps = []string{
	"sign-app", "verify-app", "notarize-app", "staple-app", "assess-app",
	"pkgbuild", "productbuild", "productsign", "notarize-pkg", "staple-pkg", "assess-pkg",
}

func urMacosSteps() []string {
	steps := []string{"identity:application", "identity:installer", "profile:other", "profile:ur"}
	steps = append(steps, urMacosArchSteps...)
	return append(steps, urMacosArchSteps...)
}

type urMacosHarness struct {
	t         *testing.T
	directory string
	events    string
	out       string
}

func newUrMacosHarness(t *testing.T) *urMacosHarness {
	t.Helper()
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("the profile lookup reads profiles with plutil")
	}
	directory := t.TempDir()
	self := &urMacosHarness{t: t, directory: directory, events: filepath.Join(directory, "events"), out: filepath.Join(directory, "out")}
	for name, script := range urMacosStubs {
		writeTestScript(t, filepath.Join(directory, "stub-bin", name), script)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		writeTestScript(t, filepath.Join(directory, "bin", "darwin", arch, "ur"), "#!/bin/sh\necho ur darwin/"+arch+"\n")
	}
	copyTestContent(t, filepath.Join(directory, "home", "sidecar", "LICENSE"), "license\n")
	copyTestContent(t, filepath.Join(directory, "home", "sidecar", "NOTICE"), "notice\n")
	copyTestContent(t, filepath.Join(directory, "AuthKey_synthetic.p8"), "synthetic p8\n")
	writeProfile := func(file string, name string, expires string) {
		copyTestContent(t, filepath.Join(directory, "profiles", file+".provisionprofile"),
			`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<plist version="1.0"><dict><key>Name</key><string>`+name+`</string><key>UUID</key><string>44444444-4444-4444-4444-444444444444</string><key>ExpirationDate</key><date>`+expires+`</date><key>DeveloperCertificates</key><array><data>`+base64.StdEncoding.EncodeToString([]byte("certificate"))+`</data></array></dict></plist>`+"\n")
	}
	writeProfile("other", "URnetwork Download", "2099-01-01T00:00:00Z")
	writeProfile("ur", "ur Download", "2099-01-01T00:00:00Z")
	if err := os.MkdirAll(self.out, 0o700); err != nil {
		t.Fatal(err)
	}
	return self
}

func (self *urMacosHarness) run(overrides ...string) componentResult {
	self.t.Helper()
	command := exec.Command("zsh", filepath.Join(rolloutRoot(self.t), "build-ur-macos.sh"))
	command.Env = environmentWith(append([]string{
		"PATH=" + filepath.Join(self.directory, "stub-bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"UR_STUB_EVENTS=" + self.events,
		"FAIL_STEP=",
		"UR_BIN_DIR=" + filepath.Join(self.directory, "bin"),
		"OUT_DIR=" + self.out,
		"BUILD_HOME=" + filepath.Join(self.directory, "home"),
		"EXTERNAL_WARP_VERSION=" + urTestVersion,
		"VERSION_BASE=2026.11.6",
		"VERSION_RELEASE=1234567",
		"GO_MOD_VERSION=2026",
		"SOURCE_DATE_EPOCH=1762646400",
		"URARCHIVE=" + buildTestUrarchive(self.t),
		"APPLE_API_KEY_P8=" + filepath.Join(self.directory, "AuthKey_synthetic.p8"),
		"APPLE_API_KEY=synthetic",
		"APPLE_API_ISSUER=synthetic-issuer",
		"MACOS_PROFILES_INSTALL_DIR=" + filepath.Join(self.directory, "profiles"),
		"UR_APPLICATION_IDENTITY=",
		"UR_INSTALLER_IDENTITY=",
	}, overrides...)...)
	output, err := command.CombinedOutput()
	result := componentResult{output: string(output)}
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			self.t.Fatal(err)
		}
		result.exitCode = exitError.ExitCode()
	}
	events, err := os.ReadFile(self.events)
	if err != nil && !os.IsNotExist(err) {
		self.t.Fatal(err)
	}
	result.events = string(events)
	return result
}

func (self *urMacosHarness) written() []string {
	entries, err := os.ReadDir(self.out)
	if err != nil {
		self.t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestUrMacosChainRunsInOrder(t *testing.T) {
	harness := newUrMacosHarness(t)
	result := harness.run()
	if result.exitCode != 0 {
		t.Fatalf("build-ur-macos.sh failed: %+v", result)
	}
	if events := strings.Fields(result.events); !slices.Equal(events, urMacosSteps()) {
		t.Errorf("the chain ran %v, want %v", events, urMacosSteps())
	}
	want := []string{
		"ur-" + urTestVersion + "-darwin-amd64.pkg", "ur-" + urTestVersion + "-darwin-amd64.tar.gz",
		"ur-" + urTestVersion + "-darwin-arm64.pkg", "ur-" + urTestVersion + "-darwin-arm64.tar.gz",
	}
	if written := harness.written(); !slices.Equal(written, want) {
		t.Errorf("build-ur-macos.sh wrote %v, want %v", written, want)
	}
}

// The .tar.gz holds the bundle as it was stapled: its profile, its executable and its Info.plist with
// the version filled, beside LICENSE and NOTICE.
func TestUrBundleHoldsItsProfile(t *testing.T) {
	harness := newUrMacosHarness(t)
	if result := harness.run(); result.exitCode != 0 {
		t.Fatalf("build-ur-macos.sh failed: %+v", result)
	}
	file, err := os.Open(filepath.Join(harness.out, "ur-"+urTestVersion+"-darwin-arm64.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decompressor, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(decompressor)
	contents := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		contents[header.Name] = string(data)
	}
	if !strings.Contains(contents["ur.app/Contents/embedded.provisionprofile"], "ur Download") {
		t.Errorf("the bundle does not hold the ur Download profile: %v", slices.Sorted(maps.Keys(contents)))
	}
	if contents["ur.app/Contents/MacOS/ur"] != "#!/bin/sh\necho ur darwin/arm64\n" {
		t.Errorf("the arm64 bundle holds %q", contents["ur.app/Contents/MacOS/ur"])
	}
	plist := contents["ur.app/Contents/Info.plist"]
	if !strings.Contains(plist, "<string>network.ur.cli</string>") || !strings.Contains(plist, "<string>2026.11.6</string>") || strings.Contains(plist, "@VERSION") {
		t.Errorf("Info.plist is not ur's, filled:\n%s", plist)
	}
	if contents["LICENSE"] != "license\n" || contents["NOTICE"] != "notice\n" {
		t.Errorf("the archive lacks LICENSE or NOTICE")
	}
}

// Each signing, notarization, stapling, assessment and packaging step is fatal, and nothing after it runs.
func TestUrMacosStepFailuresAreFatal(t *testing.T) {
	steps := urMacosSteps()
	seen := map[string]bool{}
	for index, step := range steps {
		if seen[step] || strings.HasPrefix(step, "profile:other") {
			continue
		}
		seen[step] = true
		harness := newUrMacosHarness(t)
		result := harness.run("FAIL_STEP=" + step)
		if result.exitCode == 0 {
			t.Fatalf("%s failure was masked: %+v", step, result)
		}
		events := strings.Fields(result.events)
		if len(events) != index+1 || events[index] != step {
			t.Errorf("%s failure did not stop the chain: %v", step, events)
		}
	}
}

func TestUrMacosRejectsUnacceptedNotarization(t *testing.T) {
	harness := newUrMacosHarness(t)
	result := harness.run("NOTARY_STATUS=Invalid")
	if result.exitCode == 0 || strings.Contains(result.events, "staple-app") || len(harness.written()) != 0 {
		t.Fatalf("an unaccepted notarization went on: %+v", result)
	}
}

func TestUrMacosRequiresTheUrProfile(t *testing.T) {
	harness := newUrMacosHarness(t)
	if err := os.Remove(filepath.Join(harness.directory, "profiles", "ur.provisionprofile")); err != nil {
		t.Fatal(err)
	}
	result := harness.run()
	if result.exitCode == 0 || strings.Contains(result.events, "sign-") || !strings.Contains(result.output, `no unexpired "ur Download" profile`) {
		t.Fatalf("ur.app was signed without its profile: %+v", result)
	}
}

func TestUrMacosRequiresAnInstallerIdentity(t *testing.T) {
	harness := newUrMacosHarness(t)
	result := harness.run("UR_STUB_NO_INSTALLER=1")
	if result.exitCode == 0 || strings.Contains(result.events, "sign-") || !strings.Contains(result.output, "Developer ID Installer") {
		t.Fatalf("the chain ran with no Installer identity: %+v", result)
	}
}

// The values codesign signs in are literal: outside Xcode nothing expands a build variable.
func TestUrEntitlementsAreLiteral(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur", "macos", "ur.entitlements"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "$(") || strings.Contains(string(data), "${") {
		t.Errorf("ur.entitlements holds a build variable")
	}
	command := exec.Command("plutil", "-lint", filepath.Join(rolloutRoot(t), "ur", "macos", "ur.entitlements"))
	if _, err := exec.LookPath("plutil"); err == nil {
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("ur.entitlements is not a property list: %v\n%s", err, output)
		}
	}
	for _, value := range []string{
		"<key>com.apple.application-identifier</key>\n\t<string>6BGU69Q742.network.ur.cli</string>",
		"<key>com.apple.developer.team-identifier</key>\n\t<string>6BGU69Q742</string>",
		"<key>keychain-access-groups</key>\n\t<array>\n\t\t<string>6BGU69Q742.network.ur.cli</string>\n\t</array>",
	} {
		if !strings.Contains(string(data), value) {
			t.Errorf("ur.entitlements lacks %q", value)
		}
	}
}

// Reading 8: the bundle's own group and the direct-download app's, or that app cannot reach the daemon.
func TestUrEntitlementsNameBothAppGroups(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "ur", "macos", "ur.entitlements"))
	if err != nil {
		t.Fatal(err)
	}
	want := "<key>com.apple.security.application-groups</key>\n\t<array>\n\t\t<string>6BGU69Q742.group.network.ur</string>\n\t\t<string>6BGU69Q742.group.com.bringyour.urnetwork</string>\n\t</array>"
	if !strings.Contains(string(data), want) {
		t.Errorf("ur.entitlements does not name both app groups")
	}
}

// The postinstall makes both names on PATH links into the bundle, and copies nothing.
func TestPostinstallLinksAndNeverCopies(t *testing.T) {
	script := filepath.Join(rolloutRoot(t), "ur", "macos", "postinstall")
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	for index, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") && (strings.HasPrefix(trimmed, "cp ") || strings.Contains(trimmed, " cp ") || strings.HasPrefix(trimmed, "ditto") || strings.HasPrefix(trimmed, "install ")) {
			t.Errorf("postinstall:%d copies: %s", index+1, trimmed)
		}
	}
	volume := t.TempDir()
	if output, err := exec.Command("sh", script, "/tmp/ur.pkg", "/", volume).CombinedOutput(); err != nil {
		t.Fatalf("postinstall: %v\n%s", err, output)
	}
	for _, name := range []string{"ur", "git-remote-ur"} {
		target, err := os.Readlink(filepath.Join(volume, "usr", "local", "bin", name))
		if err != nil || target != "/usr/local/lib/ur/ur.app/Contents/MacOS/ur" {
			t.Errorf("%s is %q (%v), not a link to the bundle's ur", name, target, err)
		}
	}
	// a second install replaces the links, as an upgrade does
	if output, err := exec.Command("sh", script, "/tmp/ur.pkg", "/", volume).CombinedOutput(); err != nil {
		t.Fatalf("postinstall again: %v\n%s", err, output)
	}
}

// run.sh's direct-download profile gate names ur's profile, so it is proved at the release's start.
func TestRunRequiresTheUrProfile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `MACOS_DIRECT_PROFILE_NAMES=("URnetwork Download" "URnetwork Extension Download" "URnetwork Split Tunnel Download" "ur Download")`) {
		t.Errorf("run.sh's profile names do not hold ur Download")
	}
	for _, mode := range []string{"missing", "expired", "wrong-name", "wrong-cert"} {
		result := runMacosDirect(t, "", "UR_PROFILE_MODE="+mode)
		if result.exitCode == 0 || strings.Contains(result.events, "clean\n") || strings.Contains(result.events, "release-continued") {
			t.Fatalf("%s ur profile was masked: %+v", mode, result)
		}
	}
}

// run.sh proves a Developer ID Installer identity at its start, as it does the Application one.
func TestRunRequiresAnInstallerIdentity(t *testing.T) {
	helper := componentFunctions(t, "macos_developer_id_installer_identity")
	if helper == "" {
		t.Fatal("run.sh has no macos_developer_id_installer_identity")
	}
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	gate := strings.Index(string(data), "\nUR_INSTALLER_IDENTITY=$(macos_developer_id_installer_identity)\n")
	stage := strings.Index(string(data), "\nwarpctl stage version")
	if gate < 0 || gate > stage {
		t.Errorf("the Installer identity is not proved before the release starts")
	}
	for _, present := range []bool{true, false} {
		lines := `printf '  1) ` + urMacosApplicationIdentity + ` "Developer ID Application: Synthetic (6BGU69Q742)"\n'`
		if present {
			lines += `; printf '  2) ` + urMacosInstallerIdentity + ` "Developer ID Installer: Synthetic (6BGU69Q742)"\n'`
		}
		command := exec.Command("zsh", "-c", `builder_message() { print -u2 -r -- "$1" }
security() { `+lines+` }
eval "$1"
macos_developer_id_installer_identity`, "installer", helper)
		output, err := command.Output()
		if present && (err != nil || strings.TrimSpace(string(output)) != urMacosInstallerIdentity) {
			t.Errorf("with an Installer identity the gate gave %q, %v", output, err)
		}
		if !present && err == nil {
			t.Errorf("with no Installer identity the gate passed: %q", output)
		}
	}
}
