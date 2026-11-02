package main

import (
	"bufio"
	"go/build/constraint"
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
// search would. A file whose build constraint no target could satisfy, as
// buildable decides, marks nothing, so a directory whose files all need a tag
// that isn't set has no package line.
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
	generateLine = regexp.MustCompile(`^//go:generate\s.*$`)
)

func (index *localIndex) writeDiscovery(outPath string, tags map[string]bool) error {
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
		files := index.packageFiles(root)
		fileKindsOf := make([]string, len(files))
		fileTests := make([][]string, len(files))
		errs := make([]error, len(files))
		forEach(len(files), func(i int) {
			fileKindsOf[i], fileTests[i], errs[i] = fileKinds(filepath.Join(index.root, filepath.FromSlash(files[i])), strings.HasSuffix(files[i], "_test.go"), tags)
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
// A file that buildable rules out under tags marks nothing.
func fileKinds(file string, test bool, tags map[string]bool) (string, []string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var tests []string
	var goBuild constraint.Expr
	var plusBuild []constraint.Expr
	header, main, generate := true, false, false
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
		if header {
			// Constraints count only above the package clause, as Go reads
			// them; one that won't parse is left for the toolchain to report.
			switch {
			case constraint.IsGoBuild(line):
				if x, err := constraint.Parse(line); err == nil {
					goBuild = x
				}
			case constraint.IsPlusBuild(line):
				if x, err := constraint.Parse(line); err == nil {
					plusBuild = append(plusBuild, x)
				}
			case strings.HasPrefix(line, "package "):
				header = false
				if !buildable(goBuild, plusBuild, tags) {
					return "", nil, nil
				}
			}
		}
		switch {
		case test && testFuncLine.MatchString(line):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "func "), "(")
			tests = append(tests, name)
		case !test && mainLine.MatchString(line):
			main = true
		}
		if generateLine.MatchString(line) {
			generate = true
		}
	}
	k := ""
	if len(tests) > 0 {
		k += "t"
	}
	if main {
		k += "m"
	}
	if generate {
		k += "g"
	}
	return k, tests, nil
}

// buildable reports whether a file's build constraint could hold for some
// target, so that discovery drops only the files Go will never compile here.
// //go:build wins over // +build lines, as it does in Go, and a file with
// neither is always buildable.
//
// A tag in tags, from -tags and the like in GOFLAGS, is set. A tag the
// toolchain or target decides, as platformTag lists them, may be either: tests
// run on the engine's platform but binaries may be built for any, and a Go
// version or experiment is the module's toolchain's to set. Any other tag, such
// as ignore, e2e or mage, is unset. File name suffixes such as _windows.go are
// platform constraints too, and are left alone.
func buildable(goBuild constraint.Expr, plusBuild []constraint.Expr, tags map[string]bool) bool {
	if goBuild != nil {
		return possible(goBuild, tags, true)
	}
	for _, x := range plusBuild {
		if !possible(x, tags, true) {
			return false
		}
	}
	return true
}

// possible reports whether x can evaluate to want. Each operand is judged on
// its own, so x && !x looks possible: discovery errs towards listing a package,
// as it did before constraints were read, and never drops one Go would build.
func possible(x constraint.Expr, tags map[string]bool, want bool) bool {
	switch x := x.(type) {
	case *constraint.TagExpr:
		switch {
		case tags[x.Tag]:
			return want
		case platformTag(x.Tag):
			return true
		default:
			return !want
		}
	case *constraint.NotExpr:
		return possible(x.X, tags, !want)
	case *constraint.AndExpr:
		if want {
			return possible(x.X, tags, true) && possible(x.Y, tags, true)
		}
		return possible(x.X, tags, false) || possible(x.Y, tags, false)
	case *constraint.OrExpr:
		if want {
			return possible(x.X, tags, true) || possible(x.Y, tags, true)
		}
		return possible(x.X, tags, false) && possible(x.Y, tags, false)
	}
	return true
}

// platformTag reports whether the toolchain or target sets tag: a GOOS or
// GOARCH, unix, cgo, the compiler, a Go release or a GOEXPERIMENT. The lists
// are cmd/dist's, as internal/syslist holds them for Go 1.26, and include
// names Go reserves without supporting, as go/build matches them.
func platformTag(tag string) bool {
	return knownOS[tag] || knownArch[tag] ||
		tag == "unix" || tag == "cgo" || tag == "gc" || tag == "gccgo" ||
		strings.HasPrefix(tag, "go1.") || strings.HasPrefix(tag, "goexperiment.")
}

var knownOS = setOf(
	"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios",
	"js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1",
	"windows", "zos",
)

var knownArch = setOf(
	"386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64",
	"mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le", "ppc",
	"ppc64", "ppc64le", "riscv", "riscv64", "s390", "s390x", "sparc", "sparc64",
	"wasm",
)

func setOf(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}
