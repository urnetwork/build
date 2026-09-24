// SPDX-License-Identifier: MPL-2.0
package allbuild

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Execute setup's Firefox lifecycle with a virtual updater and process table.
// No browser, host process signal, listener, or public network is involved.
func TestWebSetupFirefoxUpdaterLifecycleRegression(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "--test-concurrency=1", filepath.Join(rolloutRoot(t), "web/firefox-smoke.test.mjs"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Firefox setup lifecycle regression: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
