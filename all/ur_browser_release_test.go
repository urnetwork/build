// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the candidate and image recipes against a single staged release. Only
// the compiler and the network asset transport are stand-ins. The manifest, offline
// signing, promotion, signature verification and image input copies are real.
func TestBrowserOriginServesTheSignedPromotion(t *testing.T) {
	mmmSource := filepath.Join(rolloutRoot(t), "..", "..", "mmm")
	makefile := filepath.Join(mmmSource, "mmm", "Makefile")
	if _, err := os.Stat(filepath.Join(mmmSource, "sidecar.ur.io", "promoted.py")); os.IsNotExist(err) {
		t.Skip("the mmm checkout is required for the browser image fixture")
	}
	key, public := throwawayReleaseKey(t)
	mmm := newPromoteMmm(t)
	promotion := filepath.Join(mmm, "ur.io", "astro", "public", "recurd", "ur")
	image := t.TempDir()
	origin := filepath.Join(image, "build", "sidecar.ur.io")
	builder := t.TempDir()
	cache := t.TempDir()
	release := t.TempDir()
	candidate := filepath.Join(cache, urTestVersion, "ur-browser")
	hostTable := filepath.Join(builder, "hosts.json")
	copyTestContent(t, filepath.Join(builder, "nvm.sh"), "nvm() { test \"$1\" = use; }\n")
	copyTestContent(t, hostTable, `{"hosts":[{"service":"api","host":"api.example.test","tcp":443}]}`)
	copyTestContent(t, filepath.Join(builder, "Makefile"), "build_sidecar_wasm:\n\ttest ! -e wasm/sidecar.wasm\n\tmkdir -p wasm\n\tprintf 'candidate wasm' > wasm/sidecar.wasm\n\tprintf '// matching runtime' > wasm/wasm_exec.js\n")
	transport := t.TempDir()
	curl := filepath.Join(transport, "curl")
	copyTestContent(t, curl, `#!/bin/sh
set -eu
while [ "$1" != "--output" ]; do shift; done
asset=${3##*/}
test "$3" = "https://github.com/urnetwork/build/releases/download/v$TEST_RELEASE/$asset"
cp "$TEST_ASSETS/$asset" "$2"
`)
	if err := os.Chmod(curl, 0o700); err != nil {
		t.Fatal(err)
	}
	makeTarget := func(target string, wantSuccess bool, overrides ...string) {
		t.Helper()
		args := []string{"-f", makefile, target, "SDK_JS_DIR=" + builder, "SIDECAR_HOST_TABLE=" + hostTable,
			"SIDECAR_CANDIDATE_DIR=" + candidate, "SIDECAR_RELEASES=" + cache, "SIDECAR_RELEASE_KEY=" + public, "SIDECAR_PROMOTION=" + promotion}
		cmd := exec.Command("make", append(args, overrides...)...)
		cmd.Dir = image
		cmd.Env = environmentWith("PATH="+transport+":"+os.Getenv("PATH"), "NVM_DIR="+builder, "TEST_RELEASE="+urTestVersion,
			"TEST_ASSETS="+release, "STAGE_SIDECAR=1")
		if output, err := cmd.CombinedOutput(); (err == nil) != wantSuccess {
			t.Fatalf("%s: %v\n%s", target, err, output)
		}
	}
	makeTarget("stage-sidecar-candidate", true)
	copyTestContent(t, filepath.Join(origin, "v1", "loader.js"), "stale unchecked loader")
	makeTarget("stage-sidecar-default", true)
	if files, err := os.ReadDir(origin); err != nil || len(files) != 0 {
		t.Fatalf("key origin before promotion: %v, %v", files, err)
	}
	native := t.TempDir()
	for _, name := range urTableNames(t, urTestVersion) {
		copyTestContent(t, filepath.Join(native, name), "final native artifact")
	}
	section := componentRegion(t, "ur_browser_files=(", "builder_message \"ur ")
	publish := exec.Command("zsh", "-f", "-c", `set -e
github_release_upload() { cp "$2" "$PUBLISHED/$1"; }
error_trap() { return $?; }
`+section)
	publish.Env = environmentWith("BUILD_HOME="+filepath.Dir(rolloutRoot(t)), "BUILD_OUT="+t.TempDir(),
		"EXTERNAL_WARP_VERSION="+urTestVersion, "UR_BROWSER_OUT="+candidate, "UR_OUT="+native, "PUBLISHED="+release)
	if output, err := publish.CombinedOutput(); err != nil {
		t.Fatalf("publish browser assets and signed manifest inputs: %v\n%s", err, output)
	}
	if result := runSign(t, urTestVersion, "sign", urTestVersion, key, release); result.exitCode != 0 {
		t.Fatal(result)
	}
	if result := runPromote(t, public, mmm, urTestVersion, filepath.Join(release, "SHA256SUMS.sig")); result.exitCode != 0 {
		t.Fatal(result)
	}
	// The clean builder can hydrate from the exact assets published above.
	freshCache := "SIDECAR_RELEASES=" + t.TempDir()
	makeTarget("stage-sidecar", false, freshCache)
	makeTarget("fetch-promoted-sidecar", true, freshCache)
	makeTarget("stage-sidecar-image", false, freshCache)
	for name, data := range channelFiles(t, promotion) {
		copyTestContent(t, filepath.Join(image, "build", "preview.ur.io", "recurd", "ur", name), data)
	}
	makeTarget("stage-sidecar-image", true, freshCache)
	served := channelFiles(t, origin)
	if len(served) != len(browserTestFiles) {
		t.Fatalf("served %d files", len(served))
	}
	for _, name := range browserTestFiles {
		staged, err := os.ReadFile(filepath.Join(candidate, "v1", name))
		if err != nil || !bytes.Equal(staged, []byte(served["v1/"+name])) {
			t.Fatalf("%s differs from the promoted candidate: %v", name, err)
		}
	}
	wasmHash := fmt.Sprintf("%x", sha256.Sum256([]byte(served["v1/sidecar.wasm"])))
	if !strings.Contains(served["v1/loader.js"], wasmHash) || !strings.HasPrefix(served["v1/worker.js"], "// matching runtime") {
		t.Fatal("served loader pin or Worker runtime differs from the candidate")
	}
	// A different snapshot can supply neither the loader nor its Worker. Failed
	// attempts must leave the last complete image input available.
	for _, name := range browserTestFiles {
		path := filepath.Join(candidate, "v1", name)
		copyTestContent(t, path, "later candidate")
		makeTarget("stage-sidecar", false)
		copyTestContent(t, path, served["v1/"+name])
	}
	_, wrongKey := throwawayReleaseKey(t)
	makeTarget("stage-sidecar", false, "SIDECAR_RELEASE_KEY="+wrongKey)
	latest := filepath.Join(promotion, "latest.json")
	metadata, err := os.ReadFile(latest)
	if err != nil {
		t.Fatal(err)
	}
	copyTestContent(t, latest, string(metadata)+"\n")
	makeTarget("stage-sidecar", false)
	for name, data := range channelFiles(t, origin) {
		if data != served[name] {
			t.Fatalf("%s changed on a refused image build", name)
		}
	}
}
