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
		`for (const engine of [chromium, webkit])`,
		`devices["iPad (gen 7)"]`,
		`innerWidth === 810 && navigator.maxTouchPoints > 0 && /iPad/.test(navigator.userAgent)`,
		`required Chromium/WebKit browser installation failed`,
		`required Chromium/WebKit smoke test failed`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("web setup may omit locked mobile browser provisioning: missing %s", required)
		}
	}
	dependencies := strings.Index(source, `install_dependencies "$react"`)
	installation := strings.Index(source, `./node_modules/.bin/playwright install chromium webkit`)
	smoke := strings.Index(source, `for (const engine of [chromium, webkit])`)
	if dependencies < 0 || installation < dependencies || smoke < installation {
		t.Fatal("locked dependencies and both browser installations must precede the WebKit smoke test")
	}
	if strings.Contains(source, `npx playwright install`) {
		t.Fatal("browser revisions must come from the installed locked package, never a downloaded CLI")
	}
}
