// SPDX-License-Identifier: MPL-2.0

// ursums writes ur's release sums (DESIGN.md §4.10; REPO-M3-01): ur-release.json, the version and the
// names of every artifact all/ur/artifacts.txt lists, and SHA256SUMS over those artifacts and
// ur-release.json, in `shasum -a 256`'s format and sorted by name. The two holders sign SHA256SUMS
// offline (ur-sign-release.zsh), and a promotion publishes the signature and a copy of ur-release.json
// as latest.json (ur-promote.zsh), whose version the signed sums therefore cover
// (plan-REPO-release-signature-flow, decided 2026-11-06).
//
//	ursums -dir <artifacts> -version <EXTERNAL_WARP_VERSION> -table <artifacts.txt> -o <directory> [-also <name>=<path>]...
//
// It runs after every artifact is final, since notarization, stapling and Authenticode all change the
// bytes. -also adds a file beside the table's, as the loader and sidecar.wasm join the sums from M4
// (plan-REPO-small-readings #15). A missing or empty artifact is refused, as is a name in two places.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// What ur-release.json holds.
type urRelease struct {
	Version   string   `json:"version"`
	Artifacts []string `json:"artifacts"`
}

// The release version's shape: run.sh's EXTERNAL_WARP_VERSION, yyyy.m.d-code.
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+$`)

// The name an artifact may have: a plain file name, so a sums line can never point outside its directory.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Repeatable name=path flags.
type alsoFlags []string

func (self *alsoFlags) String() string {
	return strings.Join(*self, ",")
}

func (self *alsoFlags) Set(value string) error {
	*self = append(*self, value)
	return nil
}

// The table's names for a version, as the release scripts expand it.
func readTable(tablePath string, version string) ([]string, error) {
	file, err := os.Open(tablePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	names := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, strings.ReplaceAll(line, "{version}", version))
	}
	return names, scanner.Err()
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("%s is not a nonempty file", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Writes path through a temporary file beside it, so a failure leaves no partial file under its name.
func writeAtomically(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ursums-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporary.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("ursums", flag.ContinueOnError)
	directory := flags.String("dir", "", "the directory holding the table's artifacts")
	version := flags.String("version", "", "EXTERNAL_WARP_VERSION")
	tablePath := flags.String("table", "", "all/ur/artifacts.txt")
	outputDirectory := flags.String("o", "", "where ur-release.json and SHA256SUMS go")
	also := alsoFlags{}
	flags.Var(&also, "also", "name=path of a file the sums also cover")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *directory == "" || *version == "" || *tablePath == "" || *outputDirectory == "" || flags.NArg() != 0 {
		return errors.New("usage: ursums -dir <artifacts> -version <version> -table <artifacts.txt> -o <directory> [-also <name>=<path>]...")
	}
	if !versionPattern.MatchString(*version) {
		return fmt.Errorf("%q is not a release version yyyy.m.d-code", *version)
	}
	names, err := readTable(*tablePath, *version)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("%s names no artifact", *tablePath)
	}
	paths := map[string]string{}
	for _, name := range names {
		paths[name] = filepath.Join(*directory, name)
	}
	for _, entry := range also {
		name, path, found := strings.Cut(entry, "=")
		if !found || path == "" {
			return fmt.Errorf("-also %q is not name=path", entry)
		}
		if _, taken := paths[name]; taken {
			return fmt.Errorf("%s is named twice", name)
		}
		paths[name] = path
	}
	const releaseName = "ur-release.json"
	if _, taken := paths[releaseName]; taken {
		return fmt.Errorf("%s is ursums's own", releaseName)
	}
	artifactNames := slices.Sorted(func(yield func(string) bool) {
		for name := range paths {
			if !yield(name) {
				return
			}
		}
	})
	sums := map[string]string{}
	for _, name := range artifactNames {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("%q is not a plain file name", name)
		}
		sum, err := hashFile(paths[name])
		if err != nil {
			return fmt.Errorf("artifact %s: %w", name, err)
		}
		sums[name] = sum
	}

	release, err := json.MarshalIndent(urRelease{Version: *version, Artifacts: artifactNames}, "", "  ")
	if err != nil {
		return err
	}
	release = append(release, '\n')
	releaseSum := sha256.Sum256(release)
	sums[releaseName] = hex.EncodeToString(releaseSum[:])

	listing := bytes.Buffer{}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range sums {
			if !yield(name) {
				return
			}
		}
	}) {
		fmt.Fprintf(&listing, "%s  %s\n", sums[name], name)
	}
	if err := os.MkdirAll(*outputDirectory, 0o755); err != nil {
		return err
	}
	if err := writeAtomically(filepath.Join(*outputDirectory, releaseName), release); err != nil {
		return err
	}
	return writeAtomically(filepath.Join(*outputDirectory, "SHA256SUMS"), listing.Bytes())
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ursums:", err)
		os.Exit(1)
	}
}
