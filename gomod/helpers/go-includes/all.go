package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// runAll computes includes for every module in one local pass over a mounted
// workspace snapshot (default: the current directory), writing one file of
// include patterns per module. It never touches the workspace gateway; all
// reads are os.ReadFile.
func runAll(cliArgs []string) error {
	flags := flag.NewFlagSet("go-includes --all", flag.ExitOnError)
	flags.Bool("all", false, "compute includes for every module")
	test := flags.Bool("test", false, "also follow //go:test:include directives")
	generate := flags.Bool("generate", false, "also follow //go:generate:include directives and go:generate go -C modules")
	root := flags.String("root", ".", "workspace root to scan")
	outputDir := flags.String("output-dir", "", "directory to write one file of include patterns per module to")
	if err := flags.Parse(cliArgs); err != nil {
		return err
	}
	if *outputDir == "" {
		return fmt.Errorf("--output-dir is required")
	}

	index, err := indexLocal(*root)
	if err != nil {
		return err
	}
	return index.writeAllDir(*outputDir, *test, *generate)
}

// moduleIncludeFile returns the per-module output filename for a module root.
// The ".inc" suffix keeps a module's file distinct from a nested module's
// subdirectory (e.g. "sdk/go.inc" never collides with the "sdk/go/" tree).
func moduleIncludeFile(moduleRoot string) string {
	return moduleOutputFile(moduleRoot, ".inc")
}

// moduleTestDirectoriesFile returns the per-module test-directory output file.
func moduleTestDirectoriesFile(moduleRoot string) string {
	return moduleOutputFile(moduleRoot, ".testdirs")
}

func moduleOutputFile(moduleRoot, suffix string) string {
	name := moduleRoot
	if moduleRoot == "." {
		name = "_root_"
	}
	return filepath.FromSlash(name) + suffix
}

// writeAllDir writes one file of include patterns per module, so each consumer
// reads only its own slice instead of re-scanning a combined blob.
//
// A module whose inputs cannot be computed -- an unreadable go.mod, a Go file
// that will not parse -- records the reason in its own .err file, and the scan
// carries on to the next module. One scan serves every tool in the workspace,
// so failing the whole pass here would let a single bad module stop every
// other module's checks. The tools read the reason back and raise it only when
// something actually asks for that module's sources.
func (index *localIndex) writeAllDir(dir string, test, generate bool) error {
	if err := writeModuleOutput(dir, "_modules_", linesWithTrailer(index.goModules())); err != nil {
		return err
	}
	for _, moduleRoot := range index.moduleRoots {
		scan := index.scanModule(moduleRoot, test, generate)
		files := map[string]string{
			moduleOutputFile(moduleRoot, ".err"):       scan.failure,
			moduleOutputFile(moduleRoot, ".goversion"): index.goVersion(moduleRoot),
			moduleIncludeFile(moduleRoot):              linesWithTrailer(scan.includes),
			moduleTestDirectoriesFile(moduleRoot):      linesWithTrailer(scan.testDirs),
		}
		if generate {
			files[moduleOutputFile(moduleRoot, ".generatedirs")] = strings.Join(scan.generateDirs, "\n")
		}
		for name, contents := range files {
			if err := writeModuleOutput(dir, name, contents); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeModuleOutput(dir, name, contents string) error {
	outPath := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(contents), 0o644)
}

// goModules returns the discovered roots that are Go modules a tool can work
// on, in the order they were found.
//
// A directory holding a go.mod is not automatically one. The file may not
// parse, or may hold no module line, in which case no Go command will accept
// it; or the module may hold no Go files at all, in which case there is
// nothing for a tool to test, lint or generate. Handing either to a tool
// produces a failure about the tool rather than about the directory --
// golangci-lint reports an error on a module with no packages -- so discovery
// leaves them out and callers never see them.
func (index *localIndex) goModules() []string {
	modules := make([]string, 0, len(index.moduleRoots))
	for _, moduleRoot := range index.moduleRoots {
		if index.moduleSkipReason(moduleRoot) == "" {
			modules = append(modules, moduleRoot)
		}
	}
	return modules
}

// goVersion is the Go minor series this module's go directive asks for, or ""
// when it declares none or its go.mod cannot be read.
//
// The series rather than the exact version: a go directive states a minimum,
// "golang:1.26-alpine" is always that series' newest patch, and a go.mod
// bumped to a patch Docker has not published yet would otherwise stop the
// module being checked at all.
func (index *localIndex) goVersion(moduleRoot string) string {
	goModPath := path.Join(moduleRoot, "go.mod")
	data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(goModPath)))
	if err != nil {
		return ""
	}
	goMod, err := parseGoMod(goModPath, data)
	if err != nil || goMod.Go == nil {
		return ""
	}
	return minorSeries(goMod.Go.Version)
}

// minorSeries truncates a Go version to major.minor.
func minorSeries(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// moduleSkipReason says why a directory holding a go.mod is not a Go module a
// tool can work on, or "" when it is one.
func (index *localIndex) moduleSkipReason(moduleRoot string) string {
	goModPath := path.Join(moduleRoot, "go.mod")
	data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(goModPath)))
	if err != nil {
		return err.Error()
	}
	if _, err := parseGoMod(goModPath, data); err != nil {
		return err.Error()
	}
	if len(index.packageFiles(moduleRoot)) == 0 {
		return goModPath + ": module holds no Go files"
	}
	return ""
}

// packageFiles returns the module's Go files that Go itself would look at.
func (index *localIndex) packageFiles(moduleRoot string) []string {
	var files []string
	for _, file := range index.goFilesByModule[moduleRoot] {
		if goIgnores(moduleRelative(moduleRoot, file)) {
			continue
		}
		files = append(files, file)
	}
	return files
}

// goIgnores reports whether Go skips a module-relative path when it looks for
// packages. The rules are Go's: a testdata directory, and any name starting
// with "." or "_", hold no packages.
func goIgnores(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if segment == "testdata" || strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_") {
			return true
		}
	}
	return false
}

// moduleRelative re-roots a workspace-relative path at its module, because
// Go's ignore rules apply below the module root: a module that itself lives
// under a testdata directory still holds packages.
func moduleRelative(moduleRoot, file string) string {
	if moduleRoot == "." {
		return file
	}
	return strings.TrimPrefix(file, moduleRoot+"/")
}

// linesWithTrailer joins lines, keeping the trailing newline an empty list
// does not get.
func linesWithTrailer(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// moduleScan is one module's slice of the workspace scan, or the reason there
// is none.
type moduleScan struct {
	includes     []string
	testDirs     []string
	generateDirs []string
	failure      string
}

// scanModule computes one module's inputs, returning the failure as data
// rather than an error so that the modules after it are still scanned.
func (index *localIndex) scanModule(moduleRoot string, test, generate bool) moduleScan {
	includes, err := index.includesFor(moduleRoot, test, generate)
	if err != nil {
		return moduleScan{failure: err.Error() + "\n"}
	}
	var generateDirs []string
	if generate {
		generateDirs, err = index.generateDirectoriesFor(moduleRoot)
		if err != nil {
			return moduleScan{failure: err.Error() + "\n"}
		}
	}
	return moduleScan{
		includes:     includes,
		testDirs:     index.testDirectoriesFor(moduleRoot),
		generateDirs: generateDirs,
	}
}

// localIndex holds a workspace snapshot indexed for local include computation.
type localIndex struct {
	root            string
	moduleRoots     []string
	moduleSet       map[string]bool
	goFilesByModule map[string][]string
}

// indexLocal walks the snapshot once, recording modules and their Go files.
func indexLocal(root string) (*localIndex, error) {
	index := &localIndex{
		root:            root,
		moduleSet:       map[string]bool{},
		goFilesByModule: map[string][]string{},
	}
	var goMods, goFiles []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Name() == "go.mod":
			goMods = append(goMods, rel)
		case strings.HasSuffix(d.Name(), ".go"):
			goFiles = append(goFiles, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(goMods)
	for _, goModPath := range goMods {
		moduleRoot := strings.TrimSuffix(goModPath, "/go.mod")
		if goModPath == "go.mod" {
			moduleRoot = "."
		}
		index.moduleRoots = append(index.moduleRoots, moduleRoot)
		index.moduleSet[moduleRoot] = true
	}

	for _, goFile := range goFiles {
		moduleRoot, ok := containingModuleDir(index.moduleSet, path.Dir(goFile))
		if !ok {
			continue
		}
		index.goFilesByModule[moduleRoot] = append(index.goFilesByModule[moduleRoot], goFile)
	}
	return index, nil
}

// includesFor mirrors targetModule.includes using local file reads.
func (index *localIndex) includesFor(moduleRoot string, test, generate bool) ([]string, error) {
	queued := map[string]bool{moduleRoot: true}
	queue := []string{moduleRoot}
	var includes []string

	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]

		directives, err := index.directives(module)
		if err != nil {
			return nil, err
		}

		includes = append(includes, includeBasePatterns(module)...)
		for _, directive := range directives {
			switch {
			case directive.isEmbed():
			case generate && directive.isGenerateInclude():
			case test && directive.isTestInclude():
			default:
				continue
			}
			patterns, err := directive.includePatterns()
			if err != nil {
				return nil, err
			}
			includes = append(includes, patterns...)
		}

		next, err := index.replaceModules(module)
		if err != nil {
			return nil, err
		}
		if generate {
			generateModules, err := index.generateModules(directives)
			if err != nil {
				return nil, err
			}
			next = append(next, generateModules...)
		}
		for _, nextModule := range next {
			if !queued[nextModule] {
				queued[nextModule] = true
				queue = append(queue, nextModule)
			}
		}
	}

	deduped := make([]string, 0, len(includes))
	seen := map[string]bool{}
	for _, include := range includes {
		if seen[include] {
			continue
		}
		seen[include] = true
		deduped = append(deduped, include)
	}
	return deduped, nil
}

// directives parses every Go file belonging to a module root.
func (index *localIndex) directives(moduleRoot string) ([]goDirective, error) {
	files := index.goFilesByModule[moduleRoot]
	sort.Strings(files)

	var directives []goDirective
	for _, filePath := range files {
		data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(filePath)))
		if err != nil {
			return nil, err
		}
		fileDirectives, err := goDirectivesInFile(filePath, data)
		if err != nil {
			return nil, err
		}
		directives = append(directives, fileDirectives...)
	}
	return directives, nil
}

func (index *localIndex) testDirectoriesFor(moduleRoot string) []string {
	var testFiles []string
	for _, filePath := range index.goFilesByModule[moduleRoot] {
		if strings.HasSuffix(filePath, "_test.go") {
			testFiles = append(testFiles, filePath)
		}
	}
	return testDirectoriesFromFiles(testFiles)
}

// replaceModules resolves local go.mod replace targets to module roots.
func (index *localIndex) replaceModules(moduleRoot string) ([]string, error) {
	goModPath := path.Join(moduleRoot, "go.mod")
	data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(goModPath)))
	if err != nil {
		return nil, err
	}
	goMod, err := parseGoMod(goModPath, data)
	if err != nil {
		return nil, err
	}

	var roots []string
	for _, replace := range goMod.Replace {
		if !isLocalReplace(replace) {
			continue
		}
		target := strings.TrimSuffix(replace.New.Path, "/")
		root, ok := containingModuleDir(index.moduleSet, path.Join(path.Dir(goModPath), target))
		if !ok {
			return nil, fmt.Errorf("%s: no Go module found for local replace target: %s", replacePosition(goModPath, replace), replace.New.Path)
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// generateModules resolves go:generate go -C targets to module roots.
func (index *localIndex) generateModules(directives []goDirective) ([]string, error) {
	var roots []string
	for _, directive := range directives {
		workdir, ok, err := directive.generateGoDashC()
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		root, ok := containingModuleDir(index.moduleSet, path.Join(directive.dir(), workdir))
		if !ok {
			return nil, fmt.Errorf("%s: no Go module found for go -C directory: %s", directive.position, workdir)
		}
		roots = append(roots, root)
	}
	return roots, nil
}
