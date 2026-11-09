// SPDX-License-Identifier: MPL-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testVersion = "2026.11.6-1234567"

// A table of three names, as all/ur/artifacts.txt is written, and the artifacts it names.
func writeTestRelease(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	table := filepath.Join(directory, "artifacts.txt")
	if err := os.WriteFile(table, []byte("# a table\nur-{version}-linux-arm64.tar.gz\nur_{version}_amd64.deb\nur-{version}-darwin-arm64.pkg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(directory, "artifacts")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ur-" + testVersion + "-linux-arm64.tar.gz", "ur_" + testVersion + "_amd64.deb", "ur-" + testVersion + "-darwin-arm64.pkg"} {
		if err := os.WriteFile(filepath.Join(artifacts, name), []byte("bytes of "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return table, artifacts
}

func TestSumsAreSortedAndComplete(t *testing.T) {
	table, artifacts := writeTestRelease(t)
	output := t.TempDir()
	if err := run([]string{"-dir", artifacts, "-version", testVersion, "-table", table, "-o", output}); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(output, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(sums), "\n"), "\n") {
		sum, name, found := strings.Cut(line, "  ")
		if !found || len(sum) != 64 {
			t.Fatalf("%q is not a shasum -a 256 line", line)
		}
		data, err := os.ReadFile(filepath.Join(artifacts, name))
		if name == "ur-release.json" {
			data, err = os.ReadFile(filepath.Join(output, name))
		}
		if err != nil {
			t.Fatal(err)
		}
		if want := sha256.Sum256(data); hex.EncodeToString(want[:]) != sum {
			t.Errorf("%s's sum is %s", name, sum)
		}
		names = append(names, name)
	}
	want := []string{"ur-" + testVersion + "-darwin-arm64.pkg", "ur-" + testVersion + "-linux-arm64.tar.gz", "ur-release.json", "ur_" + testVersion + "_amd64.deb"}
	if !slices.Equal(names, want) {
		t.Errorf("SHA256SUMS lists %v, want %v, sorted", names, want)
	}
	var release urRelease
	data, err := os.ReadFile(filepath.Join(output, "ur-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &release); err != nil {
		t.Fatal(err)
	}
	if release.Version != testVersion || len(release.Artifacts) != 3 || !slices.IsSorted(release.Artifacts) {
		t.Errorf("ur-release.json is %+v", release)
	}
	// shasum itself checks the file, where this host has it
	if shasum, err := exec.LookPath("shasum"); err == nil {
		check := exec.Command(shasum, "-a", "256", "-c", filepath.Join(output, "SHA256SUMS"))
		check.Dir = artifacts
		if err := os.WriteFile(filepath.Join(artifacts, "ur-release.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if combined, err := check.CombinedOutput(); err != nil {
			t.Errorf("shasum -c refused the sums: %v\n%s", err, combined)
		}
	}
}

func TestSumsRefuseAMissingArtifact(t *testing.T) {
	table, artifacts := writeTestRelease(t)
	if err := os.Remove(filepath.Join(artifacts, "ur_"+testVersion+"_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	err := run([]string{"-dir", artifacts, "-version", testVersion, "-table", table, "-o", output})
	if err == nil || !strings.Contains(err.Error(), "ur_"+testVersion+"_amd64.deb") {
		t.Fatalf("a missing artifact was summed: %v", err)
	}
	if entries, _ := os.ReadDir(output); len(entries) != 0 {
		t.Errorf("a refused run wrote %d files", len(entries))
	}
}

func TestSumsRefuseAnEmptyArtifact(t *testing.T) {
	table, artifacts := writeTestRelease(t)
	if err := os.WriteFile(filepath.Join(artifacts, "ur-"+testVersion+"-darwin-arm64.pkg"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-dir", artifacts, "-version", testVersion, "-table", table, "-o", t.TempDir()}); err == nil {
		t.Fatal("an empty artifact was summed")
	}
}

// From M4 the loader and sidecar.wasm join the same sums (reading 15).
func TestSumsTakeAFileBesideTheTable(t *testing.T) {
	table, artifacts := writeTestRelease(t)
	wasm := filepath.Join(t.TempDir(), "sidecar.wasm")
	if err := os.WriteFile(wasm, []byte("wasm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	if err := run([]string{"-dir", artifacts, "-version", testVersion, "-table", table, "-o", output, "-also", "sidecar.wasm=" + wasm}); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(output, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sums), "  sidecar.wasm\n") {
		t.Errorf("the sums lack sidecar.wasm:\n%s", sums)
	}
	if err := run([]string{"-dir", artifacts, "-version", testVersion, "-table", table, "-o", output, "-also", "../escape=" + wasm}); err == nil {
		t.Errorf("a name outside the directory was summed")
	}
}

func TestSumsRefuseAVersionOfAnotherShape(t *testing.T) {
	table, artifacts := writeTestRelease(t)
	for _, version := range []string{"", "2026.11.6", "2026.11.6+1234567", "v2026.11.6-1"} {
		if err := run([]string{"-dir", artifacts, "-version", version, "-table", table, "-o", t.TempDir()}); err == nil {
			t.Errorf("version %q was taken", version)
		}
	}
}
