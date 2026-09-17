// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The extension's locale generator imports the sibling localization checkout
// directly. A clean acceptance host therefore needs that checkout's locked
// dependencies before the web setup can truthfully report success.
func TestWebSetupInstallsSiblingLocalizationDependencies(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "web", "setup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, required := range []string{
		`localizations="$root/localizations"`,
		`install_dependencies "$localizations"`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("web setup can pass without sibling localization dependencies: missing %s", required)
		}
	}

	validation := strings.Index(source, `"$localizations"; do`)
	installation := strings.Index(source, `install_dependencies "$localizations"`)
	browserSetup := strings.Index(source, `case "$(uname -s)" in`)
	if validation < 0 || installation < 0 || browserSetup < 0 ||
		validation > installation || installation > browserSetup {
		t.Fatal("localization package validation and installation must precede browser setup")
	}
}
