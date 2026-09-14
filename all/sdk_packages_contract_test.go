package allbuild

import (
	"os"
	"strings"
	"testing"
)

// Checks ordering without invoking the release script's external mutations.
func TestSDKPackageReleaseOrdering(t *testing.T) {
	data, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	previous := -1
	for _, marker := range []string{
		"sdk_package_stage plan", "make package check-package publish",
		"sdk_package_stage mobile", "sdk_package_stage desktop",
		`for sdk_package_asset in "$BUILD_HOME/sdk/packaging/release/assets/"`,
		"sdk_package_stage public",
	} {
		index := strings.Index(s, marker)
		if index <= previous {
			t.Fatalf("missing or incorrectly ordered SDK stage %q", marker)
		}
		previous = index
	}
	if !strings.Contains(s, `'packaging'`) {
		t.Fatal("versioned Go source staging lost the Go packaging module")
	}
}

func TestSDKMissingNpmCredentialsKeepResolvableExtensionDependencies(t *testing.T) {
	data, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Count(s, `if [ "$SDK_NPM_PUBLISH" = yes ]; then`) != 2 {
		t.Fatal("npm publication and extension dependency edits must share the checked credential result")
	}
	if !strings.Contains(s, `SDK_NPM_PUBLISH="$(go -C "$BUILD_HOME/sdk/packaging" run . credential npm)"`) {
		t.Fatal("missing Go credential probe")
	}
	if !strings.Contains(s, `error_trap 'npm SDK publishing credential check'`) {
		t.Fatal("credential probe errors must not become missing-credential skips")
	}
}
