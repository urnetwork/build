// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// A release cannot silently select an older compiler for Windows or for the
// controller's nested modules while the host and F-Droid require Go 1.27.3.
func TestReleaseGoPolicyConsistentAcrossPlatforms(t *testing.T) {
	root := rolloutRoot(t)
	for _, contract := range []struct {
		path    string
		pattern string
	}{
		{"run.sh", `"\$\(go version\)" != "go version go([0-9.]+) "\*`},
		{"windows/packer/scripts/provision.ps1", `(?m)^\$goVersion = "([0-9.]+)"$`},
		{"go.mod", `(?m)^go ([0-9.]+)$`},
		{"windows/go.mod", `(?m)^go ([0-9.]+)$`},
		{"acceptance/go.mod", `(?m)^go ([0-9.]+)$`},
	} {
		t.Run(contract.path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, contract.path))
			if err != nil {
				t.Fatal(err)
			}
			matches := regexp.MustCompile(contract.pattern).FindAllSubmatch(data, -1)
			if len(matches) != 1 || string(matches[0][1]) != "1.27.3" {
				t.Fatalf("release Go contract = %q, want exactly one Go 1.27.3 pin", matches)
			}
		})
	}
}
