// SPDX-License-Identifier: MPL-2.0

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testEpoch = 1762646400

// A tree as build-ur.sh hands one over: a binary, documents, and a bundle directory with a link inside.
func writeTestInputs(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	write := func(name string, content string, mode os.FileMode) {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		// a host's clock and umask must not reach the archive
		if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/ur", "binary\n", 0o700)
	write("LICENSE", "license\n", 0o600)
	write("NOTICE", "notice\n", 0o640)
	write("ur.app/Contents/MacOS/ur", "bundled binary\n", 0o755)
	write("ur.app/Contents/Info.plist", "plist\n", 0o644)
	if err := os.Symlink("MacOS/ur", filepath.Join(directory, "ur.app", "Contents", "current")); err != nil {
		t.Fatal(err)
	}
	return directory
}

type testTarEntry struct {
	header  *tar.Header
	content string
}

func readTestTar(t *testing.T, archivePath string) []testTarEntry {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decompressor, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	if decompressor.Header.Name != "" || !decompressor.Header.ModTime.IsZero() {
		t.Errorf("gzip's header holds name %q and time %v", decompressor.Header.Name, decompressor.Header.ModTime)
	}
	reader := tar.NewReader(decompressor)
	entries := []testTarEntry{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, testTarEntry{header: header, content: string(content)})
	}
}

func TestTarIsSortedOwnedByRootAndStampedOnce(t *testing.T) {
	inputs := writeTestInputs(t)
	output := filepath.Join(t.TempDir(), "ur.tar.gz")
	err := run([]string{"tar", "-o", output, "-mtime", "1762646400",
		"ur=" + filepath.Join(inputs, "bin", "ur"), "git-remote-ur@ur",
		"NOTICE=" + filepath.Join(inputs, "NOTICE"), "LICENSE=" + filepath.Join(inputs, "LICENSE")})
	if err != nil {
		t.Fatal(err)
	}
	entries := readTestTar(t, output)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.header.Name)
		if entry.header.Uid != 0 || entry.header.Gid != 0 || entry.header.Uname != "" || entry.header.Gname != "" {
			t.Errorf("%s is owned by %d:%d (%q:%q)", entry.header.Name, entry.header.Uid, entry.header.Gid, entry.header.Uname, entry.header.Gname)
		}
		if !entry.header.ModTime.Equal(time.Unix(testEpoch, 0)) {
			t.Errorf("%s is stamped %v", entry.header.Name, entry.header.ModTime)
		}
	}
	if want := []string{"LICENSE", "NOTICE", "git-remote-ur", "ur"}; !slices.Equal(names, want) {
		t.Errorf("the tar holds %v, want %v in order", names, want)
	}
	link := entries[slices.IndexFunc(entries, func(entry testTarEntry) bool { return entry.header.Name == "git-remote-ur" })]
	if link.header.Typeflag != tar.TypeSymlink || link.header.Linkname != "ur" {
		t.Errorf("git-remote-ur is %c to %q, not a link to ur", link.header.Typeflag, link.header.Linkname)
	}
	modes := map[string]int64{}
	for _, entry := range entries {
		modes[entry.header.Name] = entry.header.Mode
	}
	if modes["ur"] != 0o755 || modes["LICENSE"] != 0o644 || modes["NOTICE"] != 0o644 {
		t.Errorf("modes are %v", modes)
	}
}

func TestTarKeepsABundleTreeAndItsLinks(t *testing.T) {
	inputs := writeTestInputs(t)
	output := filepath.Join(t.TempDir(), "ur.tar.gz")
	if err := run([]string{"tar", "-o", output, "-mtime", "1762646400", "ur.app=" + filepath.Join(inputs, "ur.app")}); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range readTestTar(t, output) {
		names = append(names, entry.header.Name)
		if entry.header.Name == "ur.app/Contents/current" && (entry.header.Typeflag != tar.TypeSymlink || entry.header.Linkname != "MacOS/ur") {
			t.Errorf("the bundle's link became %c to %q", entry.header.Typeflag, entry.header.Linkname)
		}
	}
	want := []string{"ur.app/", "ur.app/Contents/", "ur.app/Contents/Info.plist", "ur.app/Contents/MacOS/", "ur.app/Contents/MacOS/ur", "ur.app/Contents/current"}
	if !slices.Equal(names, want) {
		t.Errorf("the bundle's tar holds %v, want %v", names, want)
	}
}

func TestZipTakesACopyAndRefusesALink(t *testing.T) {
	inputs := writeTestInputs(t)
	output := filepath.Join(t.TempDir(), "ur.zip")
	exe := filepath.Join(inputs, "bin", "ur")
	if err := run([]string{"zip", "-o", output, "-mtime", "1762646400", "ur.exe=" + exe, "git-remote-ur.exe=" + exe, "LICENSE=" + filepath.Join(inputs, "LICENSE")}); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	names := []string{}
	contents := map[string]string{}
	for _, file := range reader.File {
		names = append(names, file.Name)
		if !file.Modified.Equal(time.Unix(testEpoch, 0)) {
			t.Errorf("%s is stamped %v", file.Name, file.Modified)
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(opened)
		opened.Close()
		if err != nil {
			t.Fatal(err)
		}
		contents[file.Name] = string(data)
	}
	if want := []string{"LICENSE", "git-remote-ur.exe", "ur.exe"}; !slices.Equal(names, want) {
		t.Errorf("the zip holds %v, want %v in order", names, want)
	}
	if contents["git-remote-ur.exe"] != contents["ur.exe"] || contents["ur.exe"] != "binary\n" {
		t.Errorf("git-remote-ur.exe is not a copy of ur.exe")
	}

	err = run([]string{"zip", "-o", filepath.Join(t.TempDir(), "link.zip"), "-mtime", "1762646400", "ur.exe=" + exe, "git-remote-ur.exe@ur.exe"})
	if err == nil || !strings.Contains(err.Error(), "a zip takes a copy") {
		t.Errorf("a zip took a link: %v", err)
	}
}

// Two runs over the same inputs at different host times write the same bytes.
func TestArchivesDependOnlyOnTheirInputs(t *testing.T) {
	for _, kind := range []string{"tar", "zip"} {
		archives := [][]byte{}
		for range 2 {
			inputs := writeTestInputs(t)
			output := filepath.Join(t.TempDir(), "ur."+kind)
			if err := run([]string{kind, "-o", output, "-mtime", "1762646400", "ur=" + filepath.Join(inputs, "bin", "ur"), "LICENSE=" + filepath.Join(inputs, "LICENSE")}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			archives = append(archives, data)
		}
		if !bytes.Equal(archives[0], archives[1]) {
			t.Errorf("two %s archives of one input differ", kind)
		}
	}
}

func TestRefusesNamesOutsideTheArchive(t *testing.T) {
	inputs := writeTestInputs(t)
	for _, entry := range []string{"../ur=" + filepath.Join(inputs, "bin", "ur"), "/ur=" + filepath.Join(inputs, "bin", "ur"), "=" + filepath.Join(inputs, "bin", "ur"), "link@/etc/passwd"} {
		if err := run([]string{"tar", "-o", filepath.Join(t.TempDir(), "x.tar.gz"), "-mtime", "1", entry}); err == nil {
			t.Errorf("%q was taken", entry)
		}
	}
	output := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := run([]string{"tar", "-o", output, "-mtime", "1", "ur=" + filepath.Join(inputs, "missing")}); err == nil {
		t.Errorf("a missing input was taken")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Errorf("a failed run left %s", output)
	}
}
