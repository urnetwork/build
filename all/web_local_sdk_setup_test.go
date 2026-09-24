// SPDX-License-Identifier: MPL-2.0
package allbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebSetupBuildsCheckedLocalSDKBeforeInstallingExtension(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(rolloutRoot(t), "web/setup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	build := strings.Index(source, "timeout 1200 make SHELL='bash -e' package check-package")
	install := strings.Index(source, `node "$here/install-local-extension-sdk.mjs" "$extension" "$sdk"`)
	browsers := strings.Index(source, `case "$(uname -s)" in`)
	if build < 0 || install < build || browsers < install {
		t.Fatal("web setup must build and check the canonical SDK before installing extension dependencies and before any browser setup")
	}
	if strings.Contains(source, `install_dependencies "$extension"`) {
		t.Fatal("web setup still installs the unpublished extension SDK from the registry")
	}
}

// Exercise production setup through dependency provisioning with real npm ci,
// an ephemeral loopback-only registry, and an explicit pre-browser sentinel.
// No public registry, installed browser, host mapping, or live checkout is used.
func TestWebSetupLocalSDKOfflineRegistryRegression(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "--test-concurrency=1", filepath.Join(rolloutRoot(t), "web/install-local-extension-sdk.test.mjs"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local SDK setup regression: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
