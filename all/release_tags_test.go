// SPDX-License-Identifier: MPL-2.0

package allbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// A release leg never links the sidecar's pending migration groups (DESIGN.md
// §19.7, D-OPS-6): only local and canary builds set the sidecar_pending tag. A
// binary built with it already panics at start outside local, canary and tests,
// but only once it starts; this holds the release at build time. It reads run.sh
// for the env a release builds under and for every Makefile it builds, and it
// dry-runs each server Makefile's warpctl targets under that env, so a Makefile
// that would set the tag on a main build fails here.

// The tag a release leg must never carry.
const sidecarPendingTag = "sidecar_pending"

// How long one dry run may take before the test calls it hung.
const releaseTagsDryRunTimeout = 2 * time.Minute

// The Makefiles run.sh builds with warpctl, in order, as run.sh names them.
var releaseMakefilePattern = regexp.MustCompile(`(?m)^[^#\n]*warpctl build \$BUILD_ENV "?([^" \n]+/Makefile)"?`)

// What a dry run of one Makefile's warpctl targets, all and warp_build_image,
// prints under the env, with every variable warpctl sets given a synthetic value.
func dryRunWarpTargets(t *testing.T, directory string, env string) string {
	t.Helper()
	output := []byte{}
	for _, target := range []string{"all", "warp_build_image"} {
		ctx, cancel := context.WithTimeout(context.Background(), releaseTagsDryRunTimeout)
		command := exec.CommandContext(ctx, "make", "-n", target)
		command.Dir = directory
		command.Env = append(slices.DeleteFunc(os.Environ(), func(variable string) bool {
			return strings.HasPrefix(variable, "WARP_") || strings.HasPrefix(variable, "GOFLAGS=")
		}),
			"WARP_ENV="+env,
			"WARP_SERVICE="+filepath.Base(directory),
			"WARP_VERSION=2026.11.8-0",
			"WARP_DOCKER_NAMESPACE=example",
			"WARP_DOCKER_IMAGE="+env+"-"+filepath.Base(directory),
			"WARP_DOCKER_VERSION=2026.11.8-0",
			"SOURCE_DATE_EPOCH=1700000000",
		)
		targetOutput, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("make -n %s in %s under WARP_ENV=%s: %s\n%s", target, directory, env, err, targetOutput)
		}
		output = append(output, targetOutput...)
	}
	return string(output)
}

// Whether a dry run's commands carry the tag.
func dryRunSetsSidecarPending(output string) bool {
	return strings.Contains(output, sidecarPendingTag)
}

// The server checkout the release builds: the build repo's submodule when it is
// checked out, else the monoroot's sibling checkout.
func releaseServerDirectory(t *testing.T) string {
	t.Helper()
	root := rolloutRoot(t)
	for _, candidate := range []string{filepath.Join(root, "..", "server"), filepath.Join(root, "..", "..", "server")} {
		if _, err := os.Stat(filepath.Join(candidate, "cli", "api", "Makefile")); err == nil {
			return candidate
		}
	}
	t.Skip("no server checkout as a submodule or beside this one")
	return ""
}

// run.sh builds its release under BUILD_ENV main and never names the tag, in a
// GOFLAGS or anywhere else.
func TestReleaseBuildsUnderMainWithoutThePendingTag(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runSource := string(runData)
	if strings.Contains(runSource, sidecarPendingTag) {
		t.Fatalf("run.sh names %s", sidecarPendingTag)
	}
	envs := regexp.MustCompile(`(?m)^[\t ]*(?:export[\t ]+)?BUILD_ENV=(\S*)`).FindAllStringSubmatch(runSource, -1)
	if len(envs) == 0 {
		t.Fatal("run.sh sets no BUILD_ENV")
	}
	for _, env := range envs {
		if strings.Trim(env[1], `"'`) != "main" {
			t.Errorf("run.sh builds under BUILD_ENV=%s", env[1])
		}
	}
}

// Every server Makefile run.sh builds keeps the tag off under main and every
// env but local and canary, and sets it under those two, which shows the dry run
// sees the tag where it is: its absence under main means something.
func TestEveryReleaseMakefileKeepsThePendingTagOffMain(t *testing.T) {
	runData, err := os.ReadFile(filepath.Join(rolloutRoot(t), "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	serverDirectory := releaseServerDirectory(t)
	serverMakefiles := 0
	for _, match := range releaseMakefilePattern.FindAllStringSubmatch(string(runData), -1) {
		makefile := match[1]
		relative, isServer := strings.CutPrefix(makefile, "server${GO_MOD_SUFFIX}/")
		if !isServer {
			// another repo's Makefile builds no server binary, so it reads no pending group
			continue
		}
		serverMakefiles += 1
		directory := filepath.Dir(filepath.Join(serverDirectory, relative))
		for _, env := range []string{"main", "community", "beta", ""} {
			if output := dryRunWarpTargets(t, directory, env); dryRunSetsSidecarPending(output) {
				t.Errorf("%s sets %s under WARP_ENV=%q:\n%s", makefile, sidecarPendingTag, env, output)
			}
		}
		for _, env := range []string{"local", "canary"} {
			if output := dryRunWarpTargets(t, directory, env); !dryRunSetsSidecarPending(output) {
				t.Errorf("%s does not set %s under WARP_ENV=%s, so cli/sidecar-tags.mk is not in it", makefile, sidecarPendingTag, env)
			}
		}
	}
	if serverMakefiles < 7 {
		t.Fatalf("run.sh builds %d server Makefiles; the guard expected every one of api, taskworker, connect, mcp, gossip, alt and proxy", serverMakefiles)
	}
}

// The guard itself: a Makefile that sets the tag unconditionally, or under main,
// is found.
func TestTheReleaseTagGuardFailsOnAMakefileThatSetsTheTag(t *testing.T) {
	for name, makefile := range map[string]string{
		"unconditional": "all:\n\tgo build -trimpath -tags sidecar_pending -o build/planted .\n\nwarp_build_image:\n\t@true\n",
		"under main":    "TAGS :=\nifeq ($(WARP_ENV),main)\nTAGS := -tags sidecar_pending\nendif\n\nall:\n\tgo build $(TAGS) -o build/planted .\n\nwarp_build_image:\n\t@true\n",
		"in the image":  "all:\n\t@true\n\nwarp_build_image:\n\tdocker buildx build --build-arg GOFLAGS=-tags=sidecar_pending .\n",
	} {
		directory := filepath.Join(t.TempDir(), "planted")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "Makefile"), []byte(makefile), 0o600); err != nil {
			t.Fatal(err)
		}
		if !dryRunSetsSidecarPending(dryRunWarpTargets(t, directory, "main")) {
			t.Errorf("%s: the guard passed a Makefile that sets the tag on a main build", name)
		}
	}

	twin := "TAGS :=\nifneq ($(filter local canary,$(WARP_ENV)),)\nTAGS := -tags sidecar_pending\nendif\n\nall:\n\tgo build $(TAGS) -o build/planted .\n\nwarp_build_image:\n\t@true\n"
	directory := filepath.Join(t.TempDir(), "twin")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "Makefile"), []byte(twin), 0o600); err != nil {
		t.Fatal(err)
	}
	if dryRunSetsSidecarPending(dryRunWarpTargets(t, directory, "main")) {
		t.Error("the guard refused a Makefile that sets the tag only for local and canary")
	}
}
