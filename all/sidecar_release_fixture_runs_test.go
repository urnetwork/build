// SPDX-License-Identifier: MPL-2.0

// run.sh's candidate gate runs the release fixture in two go tests (QD-1056): the small planted runs
// under a tight hang guard of 60 minutes, then the real run, whose make check takes about an hour on
// the build host and grows with the tree, alone with 3 hours. Each names its tests with -run, so none
// runs twice. go test passes a -run that selects nothing, so a test renamed out of its run's -run
// would leave the gate without a word. This reads run.sh and the package's test files as text and
// needs no tag: plain go test runs it, and its names put it in the planted runs too, which go first,
// so a release meets a stale name before the real run could pass on nothing.

package allbuild

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// the release fixture's build tag, the file that holds its tests, and the real run's test
	sidecarFixtureTag      = "sidecar_release_fixture"
	sidecarFixtureFile     = "sidecar_release_fixture_test.go"
	sidecarFixtureRealTest = "TestSidecarReleaseFixture"
)

// One of run.sh's go tests of the fixture.
type sidecarFixtureRun struct {
	command string
	timeout time.Duration
	// selects top-level tests by name, as go test's -run does when it holds no slash
	pattern *regexp.Regexp
}

// The go tests of the fixture that a region of run.sh runs, in order: each go test line that names
// the fixture's tag, with its one -timeout and its one single-quoted -run. A -run with a slash names
// subtests, and -skip takes tests back out. This reads neither, so either is an error, not a guess.
func sidecarFixtureRunsIn(region string) ([]sidecarFixtureRun, error) {
	timeoutFlag := regexp.MustCompile(`(?:^|\s)-timeout[= ](\S+)`)
	runFlag := regexp.MustCompile(`(?:^|\s)-run[= ]'([^']*)'`)
	skipFlag := regexp.MustCompile(`(?:^|\s)-skip[= ]`)
	runs := []sidecarFixtureRun{}
	for _, line := range strings.Split(region, "\n") {
		command := strings.TrimSpace(line)
		if !strings.HasPrefix(command, "go test ") || !strings.Contains(command, " -tags "+sidecarFixtureTag+" ") {
			continue
		}
		timeoutMatches := timeoutFlag.FindAllStringSubmatch(command, -1)
		runMatches := runFlag.FindAllStringSubmatch(command, -1)
		if len(timeoutMatches) != 1 || len(runMatches) != 1 {
			return nil, fmt.Errorf("%q names %d -timeout and %d single-quoted -run, want one of each", command, len(timeoutMatches), len(runMatches))
		}
		if skipFlag.MatchString(command) || strings.Contains(runMatches[0][1], "/") {
			return nil, fmt.Errorf("%q uses -skip or a -run of subtests, which this does not read", command)
		}
		timeout, err := time.ParseDuration(timeoutMatches[0][1])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", command, err)
		}
		pattern, err := regexp.Compile(runMatches[0][1])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", command, err)
		}
		runs = append(runs, sidecarFixtureRun{command: command, timeout: timeout, pattern: pattern})
	}
	return runs, nil
}

// What breaks QD-1056's split, given run.sh's go tests of the fixture and the tests go test finds in
// this package under the fixture's tag, each with the file that holds it. The real run's test runs in
// a go test of its own with 3 hours, which runs nothing else. Each test of the fixture's file runs in
// exactly one go test, and every go test that does not run the real run's test has 60 minutes.
func sidecarFixtureRunFindings(runs []sidecarFixtureRun, testFiles map[string]string) []string {
	findings := []string{}
	if len(runs) != 2 {
		findings = append(findings, fmt.Sprintf("the fixture's go tests in run.sh's candidate gate: %d, want 2", len(runs)))
	}
	if testFiles[sidecarFixtureRealTest] != sidecarFixtureFile {
		findings = append(findings, fmt.Sprintf("%s holds no %s, the real run's test", sidecarFixtureFile, sidecarFixtureRealTest))
	}
	names := slices.Sorted(maps.Keys(testFiles))
	for _, run := range runs {
		if !run.pattern.MatchString(sidecarFixtureRealTest) {
			if run.timeout != 60*time.Minute {
				findings = append(findings, fmt.Sprintf("%q runs the planted runs with a limit of %s, want 1h0m0s", run.command, run.timeout))
			}
			continue
		}
		if run.timeout != 3*time.Hour {
			findings = append(findings, fmt.Sprintf("%q runs %s with a limit of %s, want 3h0m0s", run.command, sidecarFixtureRealTest, run.timeout))
		}
		for _, name := range names {
			if name != sidecarFixtureRealTest && run.pattern.MatchString(name) {
				findings = append(findings, fmt.Sprintf("%q runs %s and also %s", run.command, sidecarFixtureRealTest, name))
			}
		}
	}
	for _, name := range names {
		if testFiles[name] != sidecarFixtureFile {
			continue
		}
		selecting := 0
		for _, run := range runs {
			if run.pattern.MatchString(name) {
				selecting += 1
			}
		}
		if selecting != 1 {
			findings = append(findings, fmt.Sprintf("the fixture's go tests that select %s: %d, want 1", name, selecting))
		}
	}
	return findings
}

// run.sh as it stands runs each test of the fixture's file once, the real run's alone with 3 hours
// and the rest with 60 minutes.
func TestFixtureRunsInRunShRunEachTestOnce(t *testing.T) {
	runs, err := sidecarFixtureRunsIn(componentRegion(t, "    builder_message \"Build all test candidate\"\n", "warpctl stage version next release"))
	if err != nil {
		t.Fatal(err)
	}
	// The package's tests as go test finds them under the fixture's tag: the files the tag builds, and
	// in each the functions that go test's rule names tests, Test followed by nothing or by a character
	// that is not lower case. TestMain is go test's own entry point, not a test.
	buildContext := build.Default
	buildContext.BuildTags = append(slices.Clone(buildContext.BuildTags), sidecarFixtureTag)
	directory := rolloutRoot(t)
	buildPackage, err := buildContext.ImportDir(directory, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(buildPackage.TestGoFiles, sidecarFixtureFile) {
		t.Fatalf("the %s tag does not build %s", sidecarFixtureTag, sidecarFixtureFile)
	}
	isTestName := func(name string) bool {
		suffix, found := strings.CutPrefix(name, "Test")
		if !found || name == "TestMain" {
			return false
		}
		first, _ := utf8.DecodeRuneInString(suffix)
		return suffix == "" || !unicode.IsLower(first)
	}
	fileSet := token.NewFileSet()
	testFiles := map[string]string{}
	for _, name := range slices.Concat(buildPackage.TestGoFiles, buildPackage.XTestGoFiles) {
		file, err := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && isTestName(function.Name.Name) {
				testFiles[function.Name.Name] = name
			}
		}
	}
	for _, finding := range sidecarFixtureRunFindings(runs, testFiles) {
		t.Error(finding)
	}
}

// Each way QD-1056's split can come undone is named: the one go test with 60 minutes that the split
// replaced, that go test kept beside the real run's, a real run's -run that takes more than its test,
// limits swapped, a test renamed out of both -runs, and the real run's test renamed out from under
// its -run.
func TestFixtureRunFindingsNameEachPlantedMistake(t *testing.T) {
	plantedCommand := "go test -count=1 -timeout 60m -tags sidecar_release_fixture -run '^TestFixture' .)"
	realCommand := "go test -count=1 -timeout 3h -tags sidecar_release_fixture -run '^TestSidecarReleaseFixture$' .)"
	// the go test that ran every test of the fixture before the split
	oneCommand := "go test -count=1 -timeout 60m -tags sidecar_release_fixture -run 'Fixture' .)"
	realTakesMoreCommand := "go test -count=1 -timeout 3h -tags sidecar_release_fixture -run '^TestSidecarReleaseFixture$|^TestFixturePasses' .)"
	swappedPlantedCommand := "go test -count=1 -timeout 3h -tags sidecar_release_fixture -run '^TestFixture' .)"
	swappedRealCommand := "go test -count=1 -timeout 60m -tags sidecar_release_fixture -run '^TestSidecarReleaseFixture$' .)"
	// run.sh's shape around each go test, which the reading skips
	region := func(commands ...string) string {
		var source strings.Builder
		for _, command := range commands {
			source.WriteString("    (cd \"$BUILD_HOME/all\" &&\n        " + command + "\n    error_trap 'sidecar release fixture'\n")
		}
		return source.String()
	}
	testFiles := func(renames map[string]string) map[string]string {
		files := map[string]string{}
		for name, file := range map[string]string{
			sidecarFixtureRealTest:                              sidecarFixtureFile,
			"TestFixturePassesTheSmallTree":                     sidecarFixtureFile,
			"TestRunGatesTheSidecarFixtureBeforeAVersionExists": "sidecar_release_test.go",
		} {
			if renamed, found := renames[name]; found {
				name = renamed
			}
			files[name] = file
		}
		return files
	}
	cases := []struct {
		label     string
		region    string
		testFiles map[string]string
		want      []string
	}{
		{
			label:     "the split",
			region:    region(plantedCommand, realCommand),
			testFiles: testFiles(nil),
			want:      []string{},
		},
		{
			label:     "the one go test before the split",
			region:    region(oneCommand),
			testFiles: testFiles(nil),
			want: []string{
				"the fixture's go tests in run.sh's candidate gate: 1, want 2",
				fmt.Sprintf("%q runs TestSidecarReleaseFixture with a limit of 1h0m0s, want 3h0m0s", oneCommand),
				fmt.Sprintf("%q runs TestSidecarReleaseFixture and also TestFixturePassesTheSmallTree", oneCommand),
				fmt.Sprintf("%q runs TestSidecarReleaseFixture and also TestRunGatesTheSidecarFixtureBeforeAVersionExists", oneCommand),
			},
		},
		{
			label:     "the one go test kept beside the real run's",
			region:    region(oneCommand, realCommand),
			testFiles: testFiles(nil),
			want: []string{
				fmt.Sprintf("%q runs TestSidecarReleaseFixture with a limit of 1h0m0s, want 3h0m0s", oneCommand),
				fmt.Sprintf("%q runs TestSidecarReleaseFixture and also TestFixturePassesTheSmallTree", oneCommand),
				fmt.Sprintf("%q runs TestSidecarReleaseFixture and also TestRunGatesTheSidecarFixtureBeforeAVersionExists", oneCommand),
				"the fixture's go tests that select TestSidecarReleaseFixture: 2, want 1",
			},
		},
		{
			label:     "a real run that takes a planted run",
			region:    region(plantedCommand, realTakesMoreCommand),
			testFiles: testFiles(nil),
			want: []string{
				fmt.Sprintf("%q runs TestSidecarReleaseFixture and also TestFixturePassesTheSmallTree", realTakesMoreCommand),
				"the fixture's go tests that select TestFixturePassesTheSmallTree: 2, want 1",
			},
		},
		{
			label:     "limits swapped",
			region:    region(swappedPlantedCommand, swappedRealCommand),
			testFiles: testFiles(nil),
			want: []string{
				fmt.Sprintf("%q runs the planted runs with a limit of 3h0m0s, want 1h0m0s", swappedPlantedCommand),
				fmt.Sprintf("%q runs TestSidecarReleaseFixture with a limit of 1h0m0s, want 3h0m0s", swappedRealCommand),
			},
		},
		{
			label:     "a planted run renamed out of the planted runs' -run",
			region:    region(plantedCommand, realCommand),
			testFiles: testFiles(map[string]string{"TestFixturePassesTheSmallTree": "TestSmallTreePasses"}),
			want:      []string{"the fixture's go tests that select TestSmallTreePasses: 0, want 1"},
		},
		{
			label:     "the real run's test renamed",
			region:    region(plantedCommand, realCommand),
			testFiles: testFiles(map[string]string{sidecarFixtureRealTest: "TestRealReleaseFixture"}),
			want: []string{
				"sidecar_release_fixture_test.go holds no TestSidecarReleaseFixture, the real run's test",
				"the fixture's go tests that select TestRealReleaseFixture: 0, want 1",
			},
		},
	}
	for _, c := range cases {
		runs, err := sidecarFixtureRunsIn(c.region)
		if err != nil {
			t.Errorf("%s: %v", c.label, err)
			continue
		}
		if findings := sidecarFixtureRunFindings(runs, c.testFiles); !slices.Equal(findings, c.want) {
			t.Errorf("%s: the findings are\n%s\nwant\n%s", c.label, strings.Join(findings, "\n"), strings.Join(c.want, "\n"))
		}
	}
}

// A go test of the fixture whose selection the reading cannot tell is refused rather than guessed at.
func TestFixtureRunsInRunShRefuseWhatTheyCannotRead(t *testing.T) {
	for _, command := range []string{
		"go test -count=1 -tags sidecar_release_fixture -run '^TestFixture' .)",
		"go test -count=1 -timeout 60m -tags sidecar_release_fixture .)",
		"go test -count=1 -timeout 60m -tags sidecar_release_fixture -run ^TestFixture .)",
		"go test -count=1 -timeout 60m -tags sidecar_release_fixture -run '^TestFixture' -run '^TestSidecar' .)",
		"go test -count=1 -timeout 60m -tags sidecar_release_fixture -run 'Fixture' -skip '^TestSidecarReleaseFixture$' .)",
		"go test -count=1 -timeout 60m -tags sidecar_release_fixture -run '^TestFixture/small' .)",
		"go test -count=1 -timeout sixty -tags sidecar_release_fixture -run '^TestFixture' .)",
		"go test -count=1 -timeout 60m -tags sidecar_release_fixture -run '^TestFixture(' .)",
	} {
		if runs, err := sidecarFixtureRunsIn("        " + command + "\n"); err == nil {
			t.Errorf("%q read as %d runs", command, len(runs))
		}
	}
	// a go test without the fixture's tag is not one of its runs
	if runs, err := sidecarFixtureRunsIn("        go test -count=1 -timeout 10m -run 'Fixture' .\n"); err != nil || len(runs) != 0 {
		t.Errorf("an untagged go test read as %d runs: %v", len(runs), err)
	}
}
