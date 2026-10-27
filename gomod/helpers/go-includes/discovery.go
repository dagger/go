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

type testCatalogPackage struct {
	PackagePath string   `json:"packagePath"`
	Names       []string `json:"names"`
}

type fileDiscovery struct {
	kinds string
	names []string
}

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
	allTests := []testCatalogPackage{}
	for _, root := range index.goModules() {
		modPath := ""
		goModPath := path.Join(root, "go.mod")
		if data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(goModPath))); err == nil {
			if goMod, err := parseGoMod(goModPath, data); err == nil {
				modPath = goMod.Module.Mod.Path
			}
		}
		out.WriteString("module\t" + root + "\t" + modPath + "\t" + index.goVersion(root) + "\n")
		kinds := map[string]string{}
		// Search also sees underscore/dot-prefixed files in an otherwise valid
		// package. Keep their declarations without marking them as packages.
		files := index.goFilesByModule[root]
		fileKindsOf := make([]fileDiscovery, len(files))
		errs := make([]error, len(files))
		forEach(len(files), func(i int) {
			fileKindsOf[i], errs[i] = discoverFile(filepath.Join(index.root, filepath.FromSlash(files[i])), strings.HasSuffix(files[i], "_test.go"))
		})
		tests := map[string][]string{}
		for i, file := range files {
			// A file that can't be read marks nothing here: the module's own
			// scan records why, in its .err, and other modules are unaffected.
			if errs[i] != nil {
				continue
			}
			dir := path.Dir(moduleRelative(root, file))
			// Workspace.search does not follow file symlinks. Keep the kinds
			// behavior unchanged, but do not add symlink-only declarations.
			info, statErr := os.Lstat(filepath.Join(index.root, filepath.FromSlash(file)))
			if statErr == nil && info.Mode()&os.ModeSymlink == 0 {
				tests[dir] = append(tests[dir], fileKindsOf[i].names...)
			}
			if goIgnores(moduleRelative(root, file)) {
				continue
			}
			for _, c := range fileKindsOf[i].kinds {
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
			out.WriteString("package\t" + root + "\t" + dir + "\t" + string(k) + "\n")
		}
		catalog := []testCatalogPackage{}
		for _, dir := range dirs {
			names := []string{}
			seen := map[string]bool{}
			for _, name := range tests[dir] {
				if !seen[name] {
					names = append(names, name)
					seen[name] = true
				}
			}
			catalog = append(catalog, testCatalogPackage{PackagePath: path.Join(root, dir), Names: names})
		}
		data, err := json.Marshal(catalog)
		if err != nil {
			return err
		}
		if err := writeModuleOutput(filepath.Dir(outPath), moduleOutputFile(root, ".tests.json"), string(data)); err != nil {
			return err
		}
		allTests = append(allTests, catalog...)
	}
	data, err := json.Marshal(allTests)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(outPath), "_tests_.json"), data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(out.String()), 0o644)
}

// fileKinds is what one Go file marks its directory as: "t", "m" and "g", as
// discoveryFile describes them.
func fileKinds(file string, test bool) (string, error) {
	found, err := discoverFile(file, test)
	return found.kinds, err
}

func discoverFile(file string, test bool) (fileDiscovery, error) {
	f, err := os.Open(file)
	if err != nil {
		return fileDiscovery{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	tests, main, ignore, generate := false, false, false, false
	names := []string{}
	for {
		// Lines of any length: generated files can hold very long ones.
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return fileDiscovery{}, err
		}
		if line == "" && err == io.EOF {
			break
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case test && testFuncLine.MatchString(line):
			tests = true
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "func "), "(")
			names = append(names, name)
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
	if tests {
		k += "t"
	}
	if main && !ignore {
		k += "m"
	}
	if generate {
		k += "g"
	}
	return fileDiscovery{kinds: k, names: names}, nil
}
