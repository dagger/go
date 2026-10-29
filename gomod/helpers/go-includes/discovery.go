package main

import (
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
		goFiles := index.goFileFacts()
		for _, file := range index.packageFiles(root) {
			facts := goFiles[file]
			// A file that can't be read marks nothing here: the module's own
			// scan records why, in its .err, and other modules are unaffected.
			if facts.readErr != nil {
				continue
			}
			dir := path.Dir(moduleRelative(root, file))
			for _, name := range facts.tests {
				testNames.WriteString(path.Dir(file) + "\t" + name + "\n")
			}
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
		sort.Strings(dirs)
		for _, dir := range dirs {
			k := []byte(kinds[dir])
			sort.Slice(k, func(i, j int) bool { return k[i] < k[j] })
			out.WriteString("package\t" + root + "\t" + dir + "\t" + string(k) + "\n")
		}
	}
	if err := os.WriteFile(outPath, []byte(out.String()), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(outPath), testNamesFile), []byte(testNames.String()), 0o644)
}

// fileKinds is what one Go file marks its directory as: "t", "m" and "g", as
// discoveryFile describes them, and the test names encountered in source order.
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
