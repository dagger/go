package main

import (
	"bufio"
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
		files := index.packageFiles(root)
		fileKindsOf := make([]string, len(files))
		errs := make([]error, len(files))
		forEach(len(files), func(i int) {
			fileKindsOf[i], errs[i] = fileKinds(filepath.Join(index.root, filepath.FromSlash(files[i])), strings.HasSuffix(files[i], "_test.go"))
		})
		for i, file := range files {
			// A file that can't be read marks nothing here: the module's own
			// scan records why, in its .err, and other modules are unaffected.
			if errs[i] != nil {
				continue
			}
			dir := path.Dir(moduleRelative(root, file))
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
			out.WriteString("package\t" + root + "\t" + dir + "\t" + string(k) + "\n")
		}
	}
	return os.WriteFile(outPath, []byte(out.String()), 0o644)
}

// fileKinds is what one Go file marks its directory as: "t", "m" and "g", as
// discoveryFile describes them.
func fileKinds(file string, test bool) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	tests, main, ignore, generate := false, false, false, false
	for {
		// Lines of any length: generated files can hold very long ones.
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		if line == "" && err == io.EOF {
			break
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case test && testFuncLine.MatchString(line):
			tests = true
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
	return k, nil
}
