// SPDX-License-Identifier: MPL-2.0

// urarchive writes ur's release archives (DESIGN.md §4.10; REPO-M1-01): a .tar.gz or a .zip whose
// entries are sorted by name, owned by uid and gid 0 with no owner names, and stamped with one time,
// SOURCE_DATE_EPOCH's, so an archive depends on its inputs and not on the host that packed it. macOS's
// tar cannot sort, which is why this exists. Reproducible builds are a goal, not a gate (R-REPO-7):
// nothing demands byte equality across hosts, and this only keeps the archive from adding variance.
//
//	urarchive tar -o <archive.tar.gz> -mtime <epoch> <entry>...
//	urarchive zip -o <archive.zip> -mtime <epoch> <entry>...
//
// An entry is name=path, the file or directory tree at path stored as name, or, in a tar only,
// name@target, a symbolic link. A zip refuses a link, since Windows takes git-remote-ur as a copy.
// Modes are normalized: 0755 for a directory or an executable file, 0644 for any other file.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// One entry of an archive.
type archiveEntry struct {
	// slash-separated, relative, as it is stored
	name string
	// the file to copy, for a regular file
	sourcePath string
	// the link's target, for a symbolic link
	linkTarget  string
	isDirectory bool
	executable  bool
}

// Expands the command line's entries into every file, directory and link the archive holds, sorted.
func readEntries(arguments []string) ([]*archiveEntry, error) {
	entries := []*archiveEntry{}
	names := map[string]bool{}
	add := func(entry *archiveEntry) error {
		if names[entry.name] {
			return fmt.Errorf("%s is named twice", entry.name)
		}
		names[entry.name] = true
		entries = append(entries, entry)
		return nil
	}
	cleanName := func(name string) (string, error) {
		cleaned := path.Clean(name)
		if name == "" || cleaned == "." || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return "", fmt.Errorf("%q is not a relative name inside the archive", name)
		}
		return cleaned, nil
	}
	for _, argument := range arguments {
		if name, target, isLink := strings.Cut(argument, "@"); isLink && !strings.Contains(name, "=") {
			cleaned, err := cleanName(name)
			if err != nil {
				return nil, err
			}
			if target == "" || path.IsAbs(target) {
				return nil, fmt.Errorf("link %s needs a relative target", name)
			}
			if err := add(&archiveEntry{name: cleaned, linkTarget: target}); err != nil {
				return nil, err
			}
			continue
		}
		name, sourcePath, found := strings.Cut(argument, "=")
		if !found {
			return nil, fmt.Errorf("%q is neither name=path nor name@target", argument)
		}
		cleaned, err := cleanName(name)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s is not a regular file", sourcePath)
			}
			if err := add(&archiveEntry{name: cleaned, sourcePath: sourcePath, executable: info.Mode()&0o111 != 0}); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(sourcePath, func(walkPath string, directoryEntry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relativePath, err := filepath.Rel(sourcePath, walkPath)
			if err != nil {
				return err
			}
			entryName := path.Join(cleaned, filepath.ToSlash(relativePath))
			info, err := directoryEntry.Info()
			if err != nil {
				return err
			}
			switch {
			case info.IsDir():
				return add(&archiveEntry{name: entryName, isDirectory: true})
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(walkPath)
				if err != nil {
					return err
				}
				return add(&archiveEntry{name: entryName, linkTarget: target})
			case info.Mode().IsRegular():
				return add(&archiveEntry{name: entryName, sourcePath: walkPath, executable: info.Mode()&0o111 != 0})
			}
			return fmt.Errorf("%s is neither a file, a directory nor a link", walkPath)
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(entries, func(a *archiveEntry, b *archiveEntry) int {
		return strings.Compare(a.name, b.name)
	})
	return entries, nil
}

// The entry's normalized permission bits.
func (self *archiveEntry) mode() int64 {
	if self.isDirectory || self.executable {
		return 0o755
	}
	if self.linkTarget != "" {
		return 0o777
	}
	return 0o644
}

func writeTar(output io.Writer, entries []*archiveEntry, modified time.Time) error {
	compressor, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return err
	}
	// no name and no time in gzip's own header
	compressor.Header = gzip.Header{OS: 255}
	archive := tar.NewWriter(compressor)
	for _, entry := range entries {
		header := &tar.Header{
			Name:    entry.name,
			Mode:    entry.mode(),
			ModTime: modified,
			Uid:     0,
			Gid:     0,
			Format:  tar.FormatPAX,
		}
		switch {
		case entry.isDirectory:
			header.Typeflag = tar.TypeDir
			header.Name += "/"
		case entry.linkTarget != "":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.linkTarget
		default:
			info, err := os.Stat(entry.sourcePath)
			if err != nil {
				return err
			}
			header.Typeflag = tar.TypeReg
			header.Size = info.Size()
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg {
			if err := copyFile(archive, entry.sourcePath); err != nil {
				return err
			}
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return compressor.Close()
}

func writeZip(output io.Writer, entries []*archiveEntry, modified time.Time) error {
	archive := zip.NewWriter(output)
	for _, entry := range entries {
		if entry.linkTarget != "" {
			return fmt.Errorf("%s is a link, and a zip takes a copy", entry.name)
		}
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate, Modified: modified}
		if entry.isDirectory {
			header.Name += "/"
			header.Method = zip.Store
			header.SetMode(fs.ModeDir | 0o755)
		} else {
			header.SetMode(fs.FileMode(entry.mode()))
		}
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		if !entry.isDirectory {
			if err := copyFile(writer, entry.sourcePath); err != nil {
				return err
			}
		}
	}
	return archive.Close()
}

func copyFile(output io.Writer, sourcePath string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(output, file)
	return err
}

// Writes the archive to a temporary file beside outputPath and renames it into place, so a failure
// never leaves a partial archive under the artifact's name.
func run(arguments []string) error {
	if len(arguments) < 1 || (arguments[0] != "tar" && arguments[0] != "zip") {
		return errors.New("usage: urarchive tar|zip -o <archive> -mtime <epoch> <entry>...")
	}
	kind := arguments[0]
	flags := flag.NewFlagSet("urarchive "+kind, flag.ContinueOnError)
	outputPath := flags.String("o", "", "the archive to write")
	mtime := flags.String("mtime", "", "SOURCE_DATE_EPOCH, in seconds")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if *outputPath == "" || *mtime == "" || flags.NArg() == 0 {
		return errors.New("urarchive needs -o, -mtime and at least one entry")
	}
	seconds, err := strconv.ParseInt(*mtime, 10, 64)
	if err != nil || seconds < 0 {
		return fmt.Errorf("-mtime %q is not a time in seconds", *mtime)
	}
	entries, err := readEntries(flags.Args())
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(*outputPath), ".urarchive-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	modified := time.Unix(seconds, 0).UTC()
	if kind == "tar" {
		err = writeTar(temporary, entries, modified)
	} else {
		err = writeZip(temporary, entries, modified)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(temporary.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), *outputPath)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "urarchive:", err)
		os.Exit(1)
	}
}
