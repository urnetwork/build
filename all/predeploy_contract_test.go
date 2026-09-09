// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunAppliesVersionedMainStateBeforeServiceRollout pins the release
// ordering without executing a production command. The runtime behavior of
// run-main-lan.sh is covered in the Server repository with a synthetic binary.
func TestRunAppliesVersionedMainStateBeforeServiceRollout(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSource := string(runData)
	deployGuard := `if [ "$WARP_SKIP_DEPLOY" = "" ]; then`
	buildTrap := "error_trap 'build bringyourctl'"
	migrate := `BRINGYOURCTL_BINARY="$MAIN_BRINGYOURCTL_BINARY" \
        "$MAIN_BRINGYOURCTL_DIR/run-main-lan.sh" db migrate
    error_trap 'Main database migration before service rollout'`
	grafana := `BRINGYOURCTL_BINARY="$MAIN_BRINGYOURCTL_BINARY" \
        "$MAIN_BRINGYOURCTL_DIR/run-main-lan.sh" grafana load-defaults
    error_trap 'Main Grafana defaults before service rollout'`
	rollout := `source "$BUILD_HOME/all/deploy-rollout.zsh"`
	versionedBinary := `MAIN_BRINGYOURCTL_DIR="$BUILD_HOME/server${GO_MOD_SUFFIX}/bringyourctl"
    MAIN_BRINGYOURCTL_BINARY="$MAIN_BRINGYOURCTL_DIR/build/$BUILD_HOST_GOOS/$BUILD_HOST_GOARCH/bringyourctl"`

	indices := []struct {
		name  string
		value int
	}{
		{name: "bringyourctl build", value: strings.Index(runSource, buildTrap)},
		{name: "deployment guard", value: strings.Index(runSource, deployGuard)},
		{name: "versioned binary", value: strings.Index(runSource, versionedBinary)},
		{name: "database migration", value: strings.Index(runSource, migrate)},
		{name: "Grafana defaults", value: strings.Index(runSource, grafana)},
		{name: "service rollout", value: strings.Index(runSource, rollout)},
	}
	for _, item := range indices {
		if item.value < 0 {
			t.Fatalf("run.sh missing %s boundary", item.name)
		}
	}
	for i := 1; i < len(indices); i++ {
		if indices[i].value <= indices[i-1].value {
			t.Fatalf(
				"run.sh boundary %s is not after %s",
				indices[i].name,
				indices[i-1].name,
			)
		}
	}

	deployBody := runSource[indices[1].value:]
	if strings.Count(deployBody, `"$MAIN_BRINGYOURCTL_DIR/run-main-lan.sh"`) != 2 {
		t.Fatal("Main LAN runner must execute exactly the migration and Grafana commands")
	}
	if strings.Contains(deployBody[:indices[5].value-indices[1].value], "go run") {
		t.Fatal("Main pre-deploy commands can fall back to unversioned go run source")
	}
}
