package main

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
		goFiles := index.goFileFacts()
		for _, file := range index.packageFiles(root) {
			facts := goFiles[file]
			// A file that can't be read marks nothing here: the module's own
			// scan records why, in its .err, and other modules are unaffected.
			if facts.readErr != nil {
				continue
			}
			dir := path.Dir(moduleRelative(root, file))
			packageTests[dir] = append(packageTests[dir], facts.tests...)
			for _, c := range facts.kinds {
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
}

// A module's catalogue. Its lines are
//
//	module	<root>	<module path>	<go version>
//	package	<root>	<module-relative directory>	<kinds>
//
// a module line, followed by a package line for each of its package
// directories with something to do. kinds holds "t" when a _test.go file there
// declares a test, fuzz or example function, "m" when a non-test file is
// package main without an ignore build tag, and "g" when a file has a
// go:generate line. The files are the module's own, as packageFiles lists
// them, and each line is matched on its own, as a search would. Packages
// holds the same directories as workspace paths, in that order, each with its
// test names in file and source order.
type discoveryModule struct {
	Lines    []string           `json:"lines"`
	Packages []discoveryPackage `json:"packages"`
}

type discoveryPackage struct {
	Directory string   `json:"directory"`
	Tests     []string `json:"tests"`
}

// fileKinds is what one Go file marks its directory as: "t", "m" and "g", as
// discoveryModule describes them, and the test names encountered in source order.
// Each pattern is anchored on a literal prefix, so a line without it is
// passed over before any match is tried.
func fileKinds(src []byte, test bool) (string, []string) {
	var tests []string
	main, ignore, generate := false, false, false
	for line := range strings.SplitSeq(string(src), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case test && strings.HasPrefix(line, "func ") && testFuncLine.MatchString(line):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "func "), "(")
			tests = append(tests, name)
		case !test && strings.HasPrefix(line, "package main") && mainLine.MatchString(line):
			main = true
		case !test && strings.HasPrefix(line, "//") && ignoreLine.MatchString(line):
			ignore = true
		}
		if strings.HasPrefix(line, "//go:generate") && generateLine.MatchString(line) {
			generate = true
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
