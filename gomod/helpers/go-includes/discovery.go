package main

import (
	"encoding/json"
	"fmt"
	"go/build"
	"go/build/constraint"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// discoveryCatalogFile names the scan's package discovery: what the go module
// needs to list and build a workspace's packages, found in this one pass over
// the files instead of an engine search, glob or field read per result. It is
// a small index of the modules the scan accepts, each pointing at its own
// catalogue in discoveryModulesDir, so a consumer reads only its module's data
// rather than the workspace's entire test catalogue. Ordinals name the
// catalogue files, rather than arbitrary module paths.
const discoveryCatalogFile = "_discovery_index_.json"
const discoveryModulesDir = "_discovery_modules_"

var (
	// The go module's test search pattern, with ripgrep's Unicode \w spelled
	// out: Go's \w is ASCII alone, and a test may be named in any script.
	testFuncLine = regexp.MustCompile(`^func (Test([^a-z(][\p{L}\p{N}_]*)?\(\s*[\p{L}\p{N}_]+\s+\*testing\.T\s*\)|Fuzz([^a-z(][\p{L}\p{N}_]*)?\(\s*[\p{L}\p{N}_]+\s+\*testing\.F\s*\)|Example([^a-z(][\p{L}\p{N}_]*)?\(\s*\))`)
	mainLine     = regexp.MustCompile(`^package main\s*(//.*)?$`)
	ignoreLine   = regexp.MustCompile(`^//(go:build| \+build) ignore\s*$`)
	generateLine = regexp.MustCompile(`^//go:generate\s.*$`)
)

func (index *localIndex) writeDiscovery(outDir string) error {
	roots := append([]string{}, index.goModules()...)
	catalog := discoveryCatalog{Roots: roots, Modules: map[string]int{}}
	goFiles := index.goFileFacts()
	moduleDir := filepath.Join(outDir, discoveryModulesDir)
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		return err
	}
	for ordinal, root := range roots {
		module := discoveryModule{Packages: []discoveryPackage{}}
		var moduleDiscovery strings.Builder
		modPath := ""
		if goMod, err := index.goMod(root); err == nil {
			modPath = goMod.Module.Mod.Path
		}
		moduleDiscovery.WriteString("module\t" + root + "\t" + modPath + "\t" + index.goVersion(root) + "\n")
		kinds := map[string]string{}
		packageTests := map[string][]string{}
		files := index.packageFiles(root)
		fileKindsOf := make([]string, len(files))
		fileTests := make([][]string, len(files))
		buildContext := index.testBuildContext(root)
		forEach(len(files), func(i int) {
			facts := goFiles[files[i]]
			fileKindsOf[i], fileTests[i] = facts.kinds, facts.tests
			if len(fileTests[i]) > 0 && !testFileBuilds(buildContext, filepath.Join(index.root, filepath.FromSlash(files[i]))) {
				fileKindsOf[i], fileTests[i] = strings.ReplaceAll(fileKindsOf[i], "t", ""), nil
			}
		})
		for i, file := range files {
			// A file that can't be read marks nothing here: the module's own
			// scan records why, in its .err, and other modules are unaffected.
			if goFiles[file].readErr != nil {
				continue
			}
			if len(goFiles[file].tests) > 0 {
				catalog.Toolchain.Release = catalog.Toolchain.Release || goFiles[file].toolchain.Release
				catalog.Toolchain.Cgo = catalog.Toolchain.Cgo || goFiles[file].toolchain.Cgo
			}
			dir := path.Dir(moduleRelative(root, file))
			packageTests[dir] = append(packageTests[dir], fileTests[i]...)
			for _, c := range fileKindsOf[i] {
				if !strings.ContainsRune(kinds[dir], c) {
					kinds[dir] += string(c)
				}
			}
		}
		dirs := make([]string, 0, len(kinds))
		for dir, k := range kinds {
			if k != "" {
				dirs = append(dirs, dir)
			}
		}
		// In workspace-path order, which go.dang's package selection merges
		// against: there the module's own directory sorts first, while among
		// relative directories "-tools" would sort before ".".
		sort.Slice(dirs, func(i, j int) bool { return path.Join(root, dirs[i]) < path.Join(root, dirs[j]) })
		for _, dir := range dirs {
			k := []byte(kinds[dir])
			sort.Slice(k, func(i, j int) bool { return k[i] < k[j] })
			moduleDiscovery.WriteString("package\t" + root + "\t" + dir + "\t" + string(k) + "\n")
			pkg := path.Join(root, dir)
			names := append([]string{}, packageTests[dir]...)
			module.Packages = append(module.Packages, discoveryPackage{Directory: pkg, Tests: names})
		}
		module.Lines = strings.Split(strings.TrimSuffix(moduleDiscovery.String(), "\n"), "\n")
		data, err := json.Marshal(module)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(moduleDir, strconv.Itoa(ordinal)+".json"), data, 0o644); err != nil {
			return err
		}
		catalog.Modules[root] = ordinal
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, discoveryCatalogFile), data, 0o644)
}

// The index is small regardless of the number of tests.
type discoveryCatalog struct {
	Roots   []string       `json:"roots"`
	Modules map[string]int `json:"modules"`
	// Toolchain is which of the toolchain's own build tags the constraints of
	// the test files that declare tests name, across every module. Discovery
	// with a custom base asks the base's go only when one is named: GOFLAGS and
	// an explicit CGO_ENABLED come from its environment, but only its go knows
	// its version, and whether cgo is on when CGO_ENABLED is unset.
	Toolchain toolchainTags `json:"toolchain"`
}

// toolchainTags is which build tags only the Go toolchain decides a file's
// constraints name: a release tag (go1.N), or cgo.
type toolchainTags struct {
	Release bool `json:"release"`
	Cgo     bool `json:"cgo"`
}

// constraintToolchainTags reports which toolchain tags the build constraint
// lines of a Go file name: the //go:build and // +build comments before its
// package clause, read by go/parser as go/build reads the header. A file whose
// package clause doesn't parse names both, so that discovery asks the
// toolchain rather than guess.
func constraintToolchainTags(name string, src []byte) toolchainTags {
	file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return toolchainTags{Release: true, Cgo: true}
	}
	var tags toolchainTags
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) && !constraint.IsPlusBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err != nil {
				continue
			}
			expr.Eval(func(tag string) bool {
				if tag == "cgo" {
					tags.Cgo = true
				} else if isReleaseTag(tag) {
					tags.Release = true
				}
				return false
			})
		}
	}
	return tags
}

// isReleaseTag reports whether tag is a Go release tag such as go1.21.
func isReleaseTag(tag string) bool {
	minor, ok := strings.CutPrefix(tag, "go1.")
	if !ok || minor == "" {
		return false
	}
	_, err := strconv.Atoi(minor)
	return err == nil
}

// A module's catalogue. Its lines are
//
//	module	<root>	<module path>	<go version>
//	package	<root>	<module-relative directory>	<kinds>
//
// a module line, followed by a package line for each of its package
// directories with something to do. kinds holds "t" when a _test.go file there
// that go test builds declares a test or fuzz function, or an example with an
// output comment; "m" when a non-test file is package main without an ignore
// build tag; and "g" when a file has a go:generate line. The files are the
// module's own, as packageFiles lists them, and each line is matched on its
// own, as a search would. Packages holds the same directories as workspace
// paths, in that order, each with its test names in file and source order.
type discoveryModule struct {
	Lines    []string           `json:"lines"`
	Packages []discoveryPackage `json:"packages"`
}

type discoveryPackage struct {
	Directory string   `json:"directory"`
	Tests     []string `json:"tests"`
}

// testConfig is how go test builds a workspace's tests, beyond the platform
// the scan runs on, which is the engine's and so the test container's: the
// build tags its GOFLAGS set, the Go toolchain when not each module's go.mod
// version, and whether cgo is on.
type testConfig struct {
	tags      []string
	goVersion string
	cgo       bool
}

func newTestConfig(goflags, goVersion string, cgo bool) (testConfig, error) {
	config := testConfig{goVersion: goVersion, cgo: cgo}
	flags, err := splitQuoted(goflags)
	if err != nil {
		return config, fmt.Errorf("GOFLAGS: %w", err)
	}
	// The last -tags wins, and its value is parsed as cmd/go's -tags flag
	// parses it: comma-separated, or space-separated in the old syntax. The
	// -race, -msan and -asan modes add their own tag, as cmd/go does.
	var modes []string
	for _, flag := range flags {
		name, value, hasValue := strings.Cut(flag, "=")
		switch name = "-" + strings.TrimLeft(name, "-"); name {
		case "-race", "-msan", "-asan":
			on, err := strconv.ParseBool(value)
			if !hasValue {
				on, err = true, nil
			}
			if err != nil {
				return config, fmt.Errorf("GOFLAGS %s: %w", name, err)
			}
			modes = slices.DeleteFunc(modes, func(mode string) bool { return mode == name[1:] })
			if on {
				modes = append(modes, name[1:])
			}
		case "-tags":
			config.tags = nil
			if strings.ContainsAny(value, " '") {
				if config.tags, err = splitQuoted(value); err != nil {
					return config, fmt.Errorf("GOFLAGS -tags: %w", err)
				}
				continue
			}
			for _, tag := range strings.Split(value, ",") {
				if tag != "" {
					config.tags = append(config.tags, tag)
				}
			}
		}
	}
	config.tags = append(config.tags, modes...)
	return config, nil
}

// splitQuoted splits s into fields as cmd/go splits GOFLAGS: at whitespace,
// except inside a field wholly quoted in single or double quotes.
func splitQuoted(s string) ([]string, error) {
	var fields []string
	for {
		s = strings.TrimLeft(s, " \t\n\r")
		if s == "" {
			return fields, nil
		}
		if quote := s[0]; quote == '"' || quote == '\'' {
			end := strings.IndexByte(s[1:], quote)
			if end < 0 {
				return nil, fmt.Errorf("unterminated %c string", quote)
			}
			fields = append(fields, s[1:end+1])
			s = s[end+2:]
			continue
		}
		end := strings.IndexAny(s, " \t\n\r")
		if end < 0 {
			end = len(s)
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
}

// testBuildContext is what go test builds a module's test files for: the scan's
// platform, the configured tags and cgo, and the Go toolchain's release tags.
func (index *localIndex) testBuildContext(root string) *build.Context {
	ctx := build.Default
	ctx.CgoEnabled = index.testConfig.cgo
	ctx.BuildTags = index.testConfig.tags
	version := index.testConfig.goVersion
	if version == "" {
		version = index.goVersion(root)
	}
	var minor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(version, "go"), "1.%d", &minor); err == nil {
		ctx.ReleaseTags = nil
		for i := 1; i <= minor; i++ {
			ctx.ReleaseTags = append(ctx.ReleaseTags, fmt.Sprintf("go1.%d", i))
		}
	}
	return &ctx
}

// testFileBuilds reports whether go test builds a _test.go file: its name's
// GOOS and GOARCH suffixes and its build constraints, as go/build reads them.
// A file whose constraints can't be read counts as built, so that go test
// reports the problem rather than discovery hiding the file's tests.
func testFileBuilds(ctx *build.Context, file string) bool {
	match, err := ctx.MatchFile(filepath.Dir(file), filepath.Base(file))
	return err != nil || match
}

// runnableExamples is the names of a test file's examples that go test runs:
// those go/doc finds an output comment for, as go test decides. A file that
// does not parse keeps every example, for go test to report.
func runnableExamples(file string, src []byte) (map[string]bool, bool) {
	f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ParseComments)
	if err != nil {
		return nil, false
	}
	runnable := map[string]bool{}
	for _, example := range doc.Examples(f) {
		if example.Output != "" || example.EmptyOutput {
			runnable["Example"+example.Name] = true
		}
	}
	return runnable, true
}

// fileKinds is what one Go file marks its directory as: "t", "m" and "g", as
// discoveryModule describes them, and the test names encountered in source order.
// An example counts only when go test runs it: see runnableExamples. Each
// pattern is anchored on a literal prefix, so a line without it is passed
// over before any match is tried.
func fileKinds(file string, src []byte, test bool) (string, []string) {
	var tests []string
	examples := false
	main, ignore, generate := false, false, false
	for line := range strings.SplitSeq(string(src), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case test && strings.HasPrefix(line, "func ") && testFuncLine.MatchString(line):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "func "), "(")
			tests = append(tests, name)
			examples = examples || strings.HasPrefix(name, "Example")
		case !test && strings.HasPrefix(line, "package main") && mainLine.MatchString(line):
			main = true
		case !test && strings.HasPrefix(line, "//") && ignoreLine.MatchString(line):
			ignore = true
		}
		if strings.HasPrefix(line, "//go:generate") && generateLine.MatchString(line) {
			generate = true
		}
	}
	if examples {
		if runnable, ok := runnableExamples(file, src); ok {
			tests = slices.DeleteFunc(tests, func(name string) bool {
				return strings.HasPrefix(name, "Example") && !runnable[name]
			})
		}
	}
	k := ""
	if len(tests) > 0 {
		k += "t"
	}
	if main && !ignore {
		k += "m"
	}
	if generate {
		k += "g"
	}
	return k, tests
}
