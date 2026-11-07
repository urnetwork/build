// SPDX-License-Identifier: MPL-2.0

// A file:// module proxy for this package's tests, written from git tags, so a module can be
// resolved at its tag with nothing published. It writes the layout the go command reads:
// <module>/@v/list, and <version>.info, .mod and .zip. Plain archive/zip, since this module requires
// nothing. A nested module is left out of its parent's zip, as Go leaves it out.

package allbuild

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// A proxy's module paths are escaped for case, and these tests only publish lowercase ones.
var fileProxyModulePattern = regexp.MustCompile(`(?m)^module[ \t]+([^ \t\n]+)`)

// One directory the go command reads as a module proxy.
type fileProxy struct {
	directory string
}

// The GOPROXY entry for this proxy.
func (self *fileProxy) url() string {
	return "file://" + self.directory
}

// Every regular file of commit's tree under directory, by slash path from that directory. Symlinks
// are dropped, as Go drops them from a module.
func fileProxyTreeFiles(repository string, commit string, directory string) (map[string][]byte, error) {
	treeish := commit
	if directory != "" {
		treeish = commit + ":" + directory
	}
	command := exec.Command("git", "archive", "--format=tar", treeish)
	command.Dir = repository
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git archive %s: %w", treeish, err)
	}
	files := map[string][]byte{}
	reader := tar.NewReader(bytes.NewReader(output))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		files[header.Name] = content
	}
}

// Drops every file at or below a directory holding its own go.mod, the module's root aside.
func fileProxyOmitNestedModules(files map[string][]byte) {
	nested := []string{}
	for name := range files {
		if path.Base(name) == "go.mod" && name != "go.mod" {
			nested = append(nested, path.Dir(name)+"/")
		}
	}
	for name := range maps.Clone(files) {
		if slices.ContainsFunc(nested, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
			delete(files, name)
		}
	}
}

// Writes the module whose go.mod is at directory in commit, as version, and returns its path.
func (self *fileProxy) publish(repository string, commit string, directory string, version string) (string, error) {
	files, err := fileProxyTreeFiles(repository, commit, directory)
	if err != nil {
		return "", err
	}
	fileProxyOmitNestedModules(files)
	goMod, present := files["go.mod"]
	if !present {
		return "", fmt.Errorf("%s:%s holds no go.mod", commit, directory)
	}
	match := fileProxyModulePattern.FindSubmatch(goMod)
	if match == nil {
		return "", fmt.Errorf("%s:%s/go.mod names no module", commit, directory)
	}
	modulePath := string(match[1])
	if strings.ToLower(modulePath) != modulePath || strings.ToLower(version) != version {
		return "", fmt.Errorf("%s@%s needs escaping, which this proxy does not do", modulePath, version)
	}

	versions := filepath.Join(self.directory, filepath.FromSlash(modulePath), "@v")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return "", err
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		entry, err := writer.Create(modulePath + "@" + version + "/" + name)
		if err != nil {
			return "", err
		}
		if _, err := entry.Write(files[name]); err != nil {
			return "", err
		}
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	info := fmt.Sprintf(`{"Version":%q,"Time":%q}`+"\n", version, time.Unix(0, 0).UTC().Format(time.RFC3339))
	for name, content := range map[string][]byte{version + ".mod": goMod, version + ".zip": archive.Bytes(), version + ".info": []byte(info)} {
		if err := os.WriteFile(filepath.Join(versions, name), content, 0o644); err != nil {
			return "", err
		}
	}
	list, err := os.OpenFile(filepath.Join(versions, "list"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer list.Close()
	if _, err := fmt.Fprintln(list, version); err != nil {
		return "", err
	}
	return modulePath, nil
}

// A nested module must not ride in its parent's zip: that is how Go reads the release's tags, and a
// proxy that kept one would let a fixture pass on a package no consumer can reach.
func TestFileProxyLeavesNestedModulesOut(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	runGit(t, root, "init", "-b", "main", repository)
	runGit(t, repository, "config", "user.name", "Build Harness Test")
	runGit(t, repository, "config", "user.email", "build-harness-test@example.invalid")
	files := map[string]string{
		"go.mod":              "module modules.example/parent/v2026\n\ngo 1.27.3\n",
		"parent.go":           "package parent\n",
		"sub/sub.go":          "package sub\n",
		"nested/go.mod":       "module modules.example/parent/nested\n\ngo 1.27.3\n",
		"nested/nested.go":    "package nested\n",
		"nested/deep/deep.go": "package deep\n",
	}
	for name, content := range files {
		path := filepath.Join(repository, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("parent.go", filepath.Join(repository, "link.go")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", ".")
	runGit(t, repository, "commit", "-m", "module")

	proxy := &fileProxy{directory: filepath.Join(root, "proxy")}
	version := "v2026.11.6-1234567"
	modulePath, err := proxy.publish(repository, "HEAD", "", version)
	if err != nil {
		t.Fatal(err)
	}
	if modulePath != "modules.example/parent/v2026" {
		t.Fatalf("published %s", modulePath)
	}
	archive, err := zip.OpenReader(filepath.Join(proxy.directory, filepath.FromSlash(modulePath), "@v", version+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	names := []string{}
	for _, file := range archive.File {
		names = append(names, strings.TrimPrefix(file.Name, modulePath+"@"+version+"/"))
	}
	if want := []string{"go.mod", "parent.go", "sub/sub.go"}; !slices.Equal(names, want) {
		t.Fatalf("zip holds %q, want %q", names, want)
	}

	// the go command itself takes this layout, with nothing else to fall back on
	command := exec.Command("go", "mod", "download", "-json", modulePath+"@"+version)
	command.Dir = root
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "GOSUMDB=off", "GOFLAGS=-modcacherw",
		"GOPROXY="+proxy.url(), "GOMODCACHE="+filepath.Join(root, "cache"), "GONOPROXY=", "GOPRIVATE=")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go mod download through the proxy: %v\n%s", err, output)
	}
}
