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

	"golang.org/x/mod/modfile"
)

// runAll computes results for every module in one local pass over a mounted
// workspace snapshot (default: the current directory). It never touches the
// workspace gateway; all reads are os.ReadFile.
func runAll(cliArgs []string) error {
	flags := flag.NewFlagSet("go-includes --all", flag.ExitOnError)
	flags.Bool("all", false, "compute results for every module")
	lint := flags.Bool("lint", false, "include lint inputs")
	test := flags.Bool("test", false, "include test inputs")
	generate := flags.Bool("generate", false, "include generate inputs")
	testDirs := flags.Bool("test-dirs", false, "write directories containing Go tests")
	root := flags.String("root", ".", "workspace root to scan")
	outputDir := flags.String("output-dir", "", "directory to write one file per module to")
	if err := flags.Parse(cliArgs); err != nil {
		return err
	}
	if *outputDir == "" {
		return fmt.Errorf("--output-dir is required")
	}
	if !*lint && !*test && !*generate && !*testDirs {
		*test = true
	}

	index, err := indexLocal(*root)
	if err != nil {
		return err
	}
	if *testDirs {
		return index.writeAllTestDirsDir(*outputDir)
	}
	return index.writeAllIncludesDir(*outputDir, *lint, *test, *generate)
}

// moduleOutputFile returns the per-module output filename for a module root.
// The ".inc" suffix keeps a module's file distinct from a nested module's
// subdirectory (e.g. "sdk/go.inc" never collides with the "sdk/go/" tree).
func moduleOutputFile(moduleRoot string) string {
	name := moduleRoot
	if moduleRoot == "." {
		name = "_root_"
	}
	return filepath.FromSlash(name) + ".inc"
}

func writeLines(filePath string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return err
	}
	data := ""
	if len(lines) > 0 {
		data = strings.Join(lines, "\n") + "\n"
	}
	return os.WriteFile(filePath, []byte(data), 0o644)
}

// writeAllIncludesDir writes one include-pattern file per module, so each
// consumer reads only its own slice instead of re-scanning a combined blob.
func (index *localIndex) writeAllIncludesDir(dir string, lint, test, generate bool) error {
	for _, moduleRoot := range index.moduleRoots {
		includes, err := index.includesFor(moduleRoot, lint, test, generate)
		if err != nil {
			return err
		}
		if err := writeLines(filepath.Join(dir, moduleOutputFile(moduleRoot)), includes); err != nil {
			return err
		}
	}
	return nil
}

// writeAllTestDirsDir writes one test-directory file per module.
func (index *localIndex) writeAllTestDirsDir(dir string) error {
	for _, moduleRoot := range index.moduleRoots {
		if err := writeLines(filepath.Join(dir, moduleOutputFile(moduleRoot)), index.testDirectoriesFor(moduleRoot)); err != nil {
			return err
		}
	}
	return nil
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
func (index *localIndex) includesFor(moduleRoot string, lint, test, generate bool) ([]string, error) {
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

// testDirectoriesFor returns module directories containing at least one *_test.go file.
func (index *localIndex) testDirectoriesFor(moduleRoot string) []string {
	dirs := make([]string, 0, len(index.goFilesByModule[moduleRoot]))
	seen := map[string]bool{}
	for _, goFile := range index.goFilesByModule[moduleRoot] {
		if !strings.HasSuffix(goFile, "_test.go") {
			continue
		}
		dir := path.Dir(goFile)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
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

// replaceModules resolves local go.mod replace targets to module roots.
func (index *localIndex) replaceModules(moduleRoot string) ([]string, error) {
	goModPath := path.Join(moduleRoot, "go.mod")
	data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(goModPath)))
	if err != nil {
		return nil, err
	}
	goMod, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, err
	}

	var roots []string
	for _, replace := range goMod.Replace {
		if replace.New.Version != "" || (!strings.HasPrefix(replace.New.Path, "./") && !strings.HasPrefix(replace.New.Path, "../")) {
			continue
		}
		target := strings.TrimSuffix(replace.New.Path, "/")
		root, ok := containingModuleDir(index.moduleSet, path.Join(path.Dir(goModPath), target))
		if !ok {
			return nil, fmt.Errorf("no Go module found for local replace target: %s", replace.New.Path)
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
