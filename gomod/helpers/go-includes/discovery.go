package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// discoveryFile names the scan's package discovery: what the go module needs
// to list and build a workspace's packages, found in this one pass over the
// files instead of an engine search, glob or field read per result.
//
//	module	<root>	<module path>	<go version>
//	package	<root>	<module-relative directory>	<kinds>
//
// There is a module line for every module the scan accepts, followed by a
// package line for each of its package directories with something to do.
// kinds holds "t" when a _test.go file there declares a test, fuzz or example
// function, "m" when a non-test file is package main without an ignore build
// tag, and "g" when a file has a go:generate line. The files are the module's
// own, as packageFiles lists them, and each line is matched on its own, as a
// search would.
const discoveryFile = "_discovery_"

// Test names in file and source order, keyed by workspace-relative package
// directory. Keeping these alongside discovery avoids searching the entire
// module and reading every match again for each package's test collection.
const testNamesFile = "_testnames_"

var (
	// The go module's test search pattern, with ripgrep's Unicode \w spelled
	// out: Go's \w is ASCII alone, and a test may be named in any script.
	testFuncLine = regexp.MustCompile(`^func (Test([^a-z(][\p{L}\p{N}_]*)?\(\s*[\p{L}\p{N}_]+\s+\*testing\.T\s*\)|Fuzz([^a-z(][\p{L}\p{N}_]*)?\(\s*[\p{L}\p{N}_]+\s+\*testing\.F\s*\)|Example([^a-z(][\p{L}\p{N}_]*)?\(\s*\))`)
	mainLine     = regexp.MustCompile(`^package main\s*(//.*)?$`)
	ignoreLine   = regexp.MustCompile(`^//(go:build| \+build) ignore\s*$`)
	generateLine = regexp.MustCompile(`^//go:generate\s.*$`)
)

func (index *localIndex) writeDiscovery(outPath string) error {
	var out strings.Builder
	var testNames strings.Builder
	roots := append([]string{}, index.goModules()...)
	catalog := discoveryCatalog{Roots: roots, Modules: map[string]int{}}
	moduleDir := filepath.Join(filepath.Dir(outPath), discoveryModulesDir)
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		return err
	}
	for ordinal, root := range roots {
		module := discoveryModule{Packages: []discoveryPackage{}}
		var moduleDiscovery strings.Builder
		modPath := ""
		goModPath := path.Join(root, "go.mod")
		if data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(goModPath))); err == nil {
			if goMod, err := parseGoMod(goModPath, data); err == nil {
				modPath = goMod.Module.Mod.Path
			}
		}
		moduleDiscovery.WriteString("module\t" + root + "\t" + modPath + "\t" + index.goVersion(root) + "\n")
		kinds := map[string]string{}
		packageTests := map[string][]string{}
		files := index.packageFiles(root)
		fileKindsOf := make([]string, len(files))
		fileTests := make([][]string, len(files))
		errs := make([]error, len(files))
		forEach(len(files), func(i int) {
			fileKindsOf[i], fileTests[i], errs[i] = fileKinds(filepath.Join(index.root, filepath.FromSlash(files[i])), strings.HasSuffix(files[i], "_test.go"))
		})
		for i, file := range files {
			// A file that can't be read marks nothing here: the module's own
			// scan records why, in its .err, and other modules are unaffected.
			if errs[i] != nil {
				continue
			}
			dir := path.Dir(moduleRelative(root, file))
			for _, name := range fileTests[i] {
				testNames.WriteString(path.Dir(file) + "\t" + name + "\n")
				packageTests[dir] = append(packageTests[dir], name)
			}
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
		sort.Strings(dirs)
		for _, dir := range dirs {
			k := []byte(kinds[dir])
			sort.Slice(k, func(i, j int) bool { return k[i] < k[j] })
			moduleDiscovery.WriteString("package\t" + root + "\t" + dir + "\t" + string(k) + "\n")
			pkg := path.Join(root, dir)
			names := append([]string{}, packageTests[dir]...)
			module.Packages = append(module.Packages, discoveryPackage{Directory: pkg, Tests: names})
		}
		out.WriteString(moduleDiscovery.String())
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
	if err := os.WriteFile(outPath, []byte(out.String()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(outPath), testNamesFile), []byte(testNames.String()), 0o644); err != nil {
		return err
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(outPath), discoveryCatalogFile), data, 0o644)
}

const discoveryCatalogFile = "_discovery_index_.json"
const discoveryModulesDir = "_discovery_modules_"

// The index is small regardless of the number of tests. A consumer reads only
// its module's data, rather than decoding the workspace's entire test catalogue.
// Ordinals avoid turning arbitrary module paths into output file names.
type discoveryCatalog struct {
	Roots   []string       `json:"roots"`
	Modules map[string]int `json:"modules"`
}

type discoveryModule struct {
	Lines    []string           `json:"lines"`
	Packages []discoveryPackage `json:"packages"`
}

type discoveryPackage struct {
	Directory string   `json:"directory"`
	Tests     []string `json:"tests"`
}

// fileKinds is what one Go file marks its directory as: "t", "m" and "g", as
// discoveryFile describes them, and the test names encountered in source order.
func fileKinds(file string, test bool) (string, []string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var tests []string
	main, ignore, generate := false, false, false
	for {
		// Lines of any length: generated files can hold very long ones.
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", nil, err
		}
		if line == "" && err == io.EOF {
			break
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case test && testFuncLine.MatchString(line):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "func "), "(")
			tests = append(tests, name)
		case !test && mainLine.MatchString(line):
			main = true
		case !test && ignoreLine.MatchString(line):
			ignore = true
		}
		if generateLine.MatchString(line) {
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
	return k, tests, nil
}
