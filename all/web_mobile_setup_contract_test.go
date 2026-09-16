// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebSetupInstallsAndVerifiesLockedMobileBrowserEngines(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "web", "setup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, required := range []string{
		`npm ci --no-audit --no-fund`,
		`./node_modules/.bin/playwright install chromium webkit`,
		`timeout 120 node tests/browser-surface-smoke.mjs`,
		`required Chromium/WebKit browser installation failed`,
		`required Chromium/WebKit surface smoke failed`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("web setup may omit locked mobile browser capability proof: missing %s", required)
		}
	}
	dependencies := strings.Index(source, `install_dependencies "$react"`)
	installation := strings.Index(source, `./node_modules/.bin/playwright install chromium webkit`)
	smoke := strings.Index(source, `timeout 120 node tests/browser-surface-smoke.mjs`)
	if dependencies < 0 || installation < dependencies || smoke < installation {
		t.Fatal("locked dependencies and browser installation must precede behavioral touch smoke")
	}
	for _, obsolete := range []string{`npx playwright install`, `navigator.maxTouchPoints > 0`} {
		if strings.Contains(source, obsolete) {
			t.Fatalf("web setup retained an unpinned installer or nonportable touch proxy: %s", obsolete)
		}
	}
}
