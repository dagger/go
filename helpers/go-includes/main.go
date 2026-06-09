// go.dang runs this helper to discover workspace include patterns.
// Results are written to the path given by --output, one entry per line, so
// they don't pollute the user's terminal as exec "logs".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"dagger.io/dagger"
	telemetry "github.com/dagger/otel-go"
	"golang.org/x/mod/modfile"
)

var errNotRegularFile = errors.New("not a regular file")

func main() {
	ctx := telemetry.Init(context.Background(), telemetry.Config{Detect: true})
	defer telemetry.Close()

	targetModule, testDirs, outputPath, err := newTargetModuleFromArgs(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := os.Create(outputPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if testDirs {
		err = targetModule.printTestDirectories(ctx, out)
	} else {
		err = targetModule.printIncludes(ctx, out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = out.Close()
		os.Exit(1)
	}
	if err := out.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newTargetModuleFromArgs parses CLI flags and resolves the requested module.
func newTargetModuleFromArgs(ctx context.Context, cliArgs []string) (*targetModule, bool, string, error) {
	flags := flag.NewFlagSet("go-includes", flag.ExitOnError)
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go-includes --output PATH [--lint] [--test] [--generate] [--test-dirs] [/DIR]")
		flags.PrintDefaults()
	}
	lint := flags.Bool("lint", false, "include lint inputs")
	test := flags.Bool("test", false, "include test inputs")
	generate := flags.Bool("generate", false, "include generate inputs")
	testDirs := flags.Bool("test-dirs", false, "print directories containing Go tests")
	output := flags.String("output", "", "file to write results to (one entry per line)")
	if err := flags.Parse(cliArgs); err != nil {
		return nil, false, "", err
	}
	if *output == "" {
		return nil, false, "", fmt.Errorf("--output is required")
	}
	if flags.NArg() > 1 {
		return nil, false, "", fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if !*lint && !*test && !*generate && !*testDirs {
		*test = true
	}
	modulePath := "/"
	if flags.NArg() == 1 {
		modulePath = flags.Arg(0)
		if !strings.HasPrefix(modulePath, "/") {
			return nil, false, "", fmt.Errorf("workspace path must be absolute: %s", modulePath)
		}
	}
	ws, err := newWorkspace(ctx)
	if err != nil {
		return nil, false, "", err
	}
	moduleRoot, ok := ws.containingModuleDir(modulePath)
	if !ok {
		return nil, false, "", fmt.Errorf("no go.mod found containing path: %s", modulePath)
	}
	module, err := newTargetModule(ws, moduleRoot, *lint, *test, *generate)
	if err != nil {
		return nil, false, "", err
	}
	return module, *testDirs, *output, nil
}

// newTargetModule builds one target module with shared workspace and modes.
func newTargetModule(ws *workspace, moduleRoot string, lint, test, generate bool) (*targetModule, error) {
	if !ws.moduleSet[moduleRoot] {
		return nil, fmt.Errorf("no go.mod found for module root: %s", moduleRoot)
	}
	return &targetModule{
		workspace:  ws,
		moduleRoot: moduleRoot,
		lint:       lint,
		test:       test,
		generate:   generate,
	}, nil
}

// workspace wraps a Dagger workspace with indexes shared by include targets.
type workspace struct {
	*dagger.Workspace

	moduleRoots []string
	moduleSet   map[string]bool
}

// newWorkspace loads the current Dagger workspace and indexes its Go modules.
func newWorkspace(ctx context.Context) (*workspace, error) {
	dagWS, err := currentWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	ws := &workspace{Workspace: dagWS}
	if err := ws.indexModules(ctx); err != nil {
		return nil, err
	}
	return ws, nil
}

// directory returns a workspace-root directory filtered by include globs.
func (w *workspace) directory(include, exclude []string) *dagger.Directory {
	return w.Directory("/", dagger.WorkspaceDirectoryOpts{
		Include: append([]string(nil), include...),
		Exclude: append([]string(nil), exclude...),
	})
}

// indexModules records every module root, plus a set for ancestor lookups.
func (w *workspace) indexModules(ctx context.Context) error {
	goMods, err := w.directory([]string{"**/go.mod"}, nil).Glob(ctx, "**/go.mod")
	if err != nil {
		return err
	}
	sort.Strings(goMods)
	w.moduleRoots = make([]string, 0, len(goMods))
	w.moduleSet = map[string]bool{}
	for _, goModPath := range goMods {
		moduleRoot := strings.TrimSuffix(goModPath, "/go.mod")
		if goModPath == "go.mod" {
			moduleRoot = "."
		}
		w.moduleRoots = append(w.moduleRoots, moduleRoot)
		w.moduleSet[moduleRoot] = true
	}
	return nil
}

// containingModuleDir finds the nearest ancestor module root for a workspace path.
func (w *workspace) containingModuleDir(dir string) (string, bool) {
	dir = path.Clean(strings.TrimPrefix(dir, "/"))
	for {
		if w.moduleSet[dir] {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// nestedModuleExcludes returns globs that keep a scan within one module.
func (w *workspace) nestedModuleExcludes(moduleRoot string) []string {
	var excludes []string
	for _, nestedRoot := range w.moduleRoots {
		if moduleRoot == "." {
			if nestedRoot != "." {
				excludes = append(excludes, nestedRoot+"/**")
			}
			continue
		}
		if nestedRoot != moduleRoot && strings.HasPrefix(nestedRoot, strings.TrimSuffix(moduleRoot, "/")+"/") {
			excludes = append(excludes, nestedRoot+"/**")
		}
	}
	sort.Strings(excludes)
	return excludes
}

// targetModule is one module root and operation-specific include behavior.
type targetModule struct {
	workspace  *workspace
	moduleRoot string
	lint       bool
	test       bool
	generate   bool
}

// subpath resolves a path under this module root.
func (t targetModule) subpath(subpath string) string {
	return path.Join(t.moduleRoot, subpath)
}

// includes traverses module roots discovered from replaces and generate workdirs.
func (t targetModule) includes(ctx context.Context) ([]string, error) {
	// Walk the initial module plus module roots discovered from replaces and directives.
	queued := map[string]bool{t.moduleRoot: true}
	queue := []*targetModule{&t}
	var includes []string

	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]

		directIncludes, err := module.directIncludes(ctx)
		if err != nil {
			return nil, err
		}
		includes = append(includes, directIncludes...)

		generateModules, err := module.modulesFromGoGenerateGoDashC(ctx)
		if err != nil {
			return nil, err
		}
		replaceModules, err := module.modulesFromGoModLocalReplace(ctx)
		if err != nil {
			return nil, err
		}

		// Local replaces and go:generate -C targets join the same module queue.
		for _, nextModule := range append(replaceModules, generateModules...) {
			if !queued[nextModule.moduleRoot] {
				queued[nextModule.moduleRoot] = true
				queue = append(queue, nextModule)
			}
		}
	}
	// Preserve first-seen order while removing duplicate patterns.
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

// directIncludes returns all non-recursive include patterns for this module.
func (t targetModule) directIncludes(ctx context.Context) ([]string, error) {
	includes := t.includeBase()

	directives, err := t.goDirectives(ctx)
	if err != nil {
		return nil, err
	}

	for _, directive := range directives {
		switch {
		case directive.isEmbed():
		case t.generate && directive.isGenerateInclude():
		case t.test && directive.isTestInclude():
		default:
			continue
		}
		patterns, err := directive.includePatterns()
		if err != nil {
			return nil, err
		}
		includes = append(includes, patterns...)
	}

	return includes, nil
}

// printIncludes writes the target include patterns, one per line.
func (t targetModule) printIncludes(ctx context.Context, w io.Writer) error {
	includes, err := t.includes(ctx)
	if err != nil {
		return err
	}
	for _, include := range includes {
		if _, err := fmt.Fprintln(w, include); err != nil {
			return err
		}
	}
	return nil
}

// printTestDirectories writes directories containing Go tests, one per line.
func (t targetModule) printTestDirectories(ctx context.Context, w io.Writer) error {
	dirs, err := t.testDirectories(ctx)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if _, err := fmt.Fprintln(w, dir); err != nil {
			return err
		}
	}
	return nil
}

// testDirectories returns module directories containing at least one *_test.go file.
func (t targetModule) testDirectories(ctx context.Context) ([]string, error) {
	excludes := t.workspace.nestedModuleExcludes(t.moduleRoot)
	dir := t.workspace.directory([]string{t.subpath("**/*_test.go")}, excludes)
	testFiles, err := dir.Glob(ctx, "**/*_test.go")
	if err != nil {
		return nil, err
	}
	return testDirectoriesFromFiles(testFiles), nil
}

func testDirectoriesFromFiles(testFiles []string) []string {
	dirs := make([]string, 0, len(testFiles))
	seen := map[string]bool{}
	for _, testFile := range testFiles {
		dir := path.Dir(testFile)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// readRegularFile reads a file from a Dagger directory and reports directories distinctly.
func readRegularFile(ctx context.Context, dir *dagger.Directory, filePath string) ([]byte, error) {
	contents, err := dir.File(filePath).Contents(ctx)
	if err != nil {
		fileType, statErr := dir.Stat(filePath).FileType(ctx)
		if statErr == nil && fileType == dagger.FileTypeDirectory {
			return nil, errNotRegularFile
		}
		return nil, err
	}
	return []byte(contents), nil
}

// includeBase returns the static Go source patterns for this module root.
func (t targetModule) includeBase() []string {
	patterns := []string{
		"**/*.go",
		"**/*.c",
		"**/*.h",
		"**/*.s",
		"**/*.S",
		"**/*.syso",
		"go.mod",
		// FIXME: exclude nested module trees instead of uploading their Go
		// files just to preserve their module boundaries.
		"**/go.mod",
		"go.sum",
		"**/go.sum",
		"go.work",
		"go.work.sum",
	}
	for i, pattern := range patterns {
		patterns[i] = t.subpath(pattern)
	}
	return patterns
}

// modulesFromGoGenerateGoDashC resolves go:generate go -C targets to modules.
func (t targetModule) modulesFromGoGenerateGoDashC(ctx context.Context) ([]*targetModule, error) {
	if !t.generate {
		return nil, nil
	}
	directives, err := t.goDirectives(ctx)
	if err != nil {
		return nil, err
	}

	var moduleRoots []string
	for _, directive := range directives {
		workdir, ok, err := directive.generateGoDashC()
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		moduleRoot, ok := t.workspace.containingModuleDir(path.Join(directive.dir(), workdir))
		if !ok {
			return nil, fmt.Errorf("%s: no Go module found for go -C directory: %s", directive.position, workdir)
		}
		moduleRoots = append(moduleRoots, moduleRoot)
	}
	return t.targetModules(moduleRoots)
}

// modulesFromGoModLocalReplace resolves local go.mod replace targets to modules.
func (t targetModule) modulesFromGoModLocalReplace(ctx context.Context) ([]*targetModule, error) {
	goModPath := t.subpath("go.mod")
	dir := t.workspace.directory([]string{goModPath}, nil)
	data, err := readRegularFile(ctx, dir, goModPath)
	if err != nil {
		return nil, err
	}
	goMod, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, err
	}

	var moduleRoots []string
	for _, replace := range goMod.Replace {
		if replace.New.Version != "" || (!strings.HasPrefix(replace.New.Path, "./") && !strings.HasPrefix(replace.New.Path, "../")) {
			continue
		}
		target := strings.TrimSuffix(replace.New.Path, "/")
		moduleRoot, ok := t.workspace.containingModuleDir(path.Join(path.Dir(goModPath), target))
		if !ok {
			return nil, fmt.Errorf("no Go module found for local replace target: %s", replace.New.Path)
		}
		moduleRoots = append(moduleRoots, moduleRoot)
	}
	return t.targetModules(moduleRoots)
}

// targetModules resolves module roots using this module's workspace and modes.
func (t targetModule) targetModules(moduleRoots []string) ([]*targetModule, error) {
	modules := make([]*targetModule, 0, len(moduleRoots))
	for _, moduleRoot := range moduleRoots {
		module, err := newTargetModule(t.workspace, moduleRoot, t.lint, t.test, t.generate)
		if err != nil {
			return nil, err
		}
		modules = append(modules, module)
	}
	return modules, nil
}

// goDirectives returns parsed Go comment directives for one module.
func (t targetModule) goDirectives(ctx context.Context) ([]goDirective, error) {
	excludes := t.workspace.nestedModuleExcludes(t.moduleRoot)
	dir := t.workspace.directory([]string{t.subpath("**/*.go")}, excludes)
	goFiles, err := dir.Glob(ctx, "**/*.go")
	if err != nil {
		return nil, err
	}
	sort.Strings(goFiles)

	var directives []goDirective
	for _, filePath := range goFiles {
		data, err := readRegularFile(ctx, dir, filePath)
		if errors.Is(err, errNotRegularFile) {
			continue
		}
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

// goDirectivesInFile extracts Go comment directives from one parsed Go file.
func goDirectivesInFile(filePath string, data []byte) ([]goDirective, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var directives []goDirective
	for _, group := range file.Comments {
		for _, comment := range group.List {
			directive := goDirective{
				filePath: filePath,
				position: fset.Position(comment.Slash).String(),
				comment:  comment.Text,
			}
			if directive.isEmbed() || directive.isTestInclude() || directive.isGenerateInclude() || directive.isGenerate() {
				directives = append(directives, directive)
			}
		}
	}
	return directives, nil
}

// goDirective is one supported Go line directive comment.
type goDirective struct {
	filePath string
	position string
	comment  string
}

// dir returns the directive's workspace directory.
func (d goDirective) dir() string {
	dir := path.Dir(d.filePath)
	if dir == "." {
		return ""
	}
	return dir
}

// isEmbed reports whether the directive is //go:embed.
func (d goDirective) isEmbed() bool {
	return d.hasName("go:embed")
}

// isTestInclude reports whether the directive is //go:test:include.
func (d goDirective) isTestInclude() bool {
	return d.hasName("go:test:include")
}

// isGenerateInclude reports whether the directive is //go:generate:include.
func (d goDirective) isGenerateInclude() bool {
	return d.hasName("go:generate:include")
}

// isGenerate reports whether the directive is //go:generate.
func (d goDirective) isGenerate() bool {
	return d.hasName("go:generate")
}

// args parses the directive arguments.
func (d goDirective) args() ([]string, error) {
	name, argString, ok := d.line()
	if !ok {
		return nil, nil
	}

	var args []string
	for argString = strings.TrimLeftFunc(argString, unicode.IsSpace); argString != ""; argString = strings.TrimLeftFunc(argString, unicode.IsSpace) {
		switch argString[0] {
		case '`', '"':
			quoted, err := strconv.QuotedPrefix(argString)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid quoted string in //%s: %s", d.position, name, argString)
			}
			arg, err := strconv.Unquote(quoted)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid quoted string in //%s: %s", d.position, name, quoted)
			}
			args = append(args, arg)
			argString = argString[len(quoted):]
			if argString != "" && strings.TrimLeftFunc(argString, unicode.IsSpace) == argString {
				return nil, fmt.Errorf("%s: invalid quoted string in //%s: %s", d.position, name, argString)
			}
		default:
			i := strings.IndexFunc(argString, unicode.IsSpace)
			if i < 0 {
				i = len(argString)
			}
			args = append(args, argString[:i])
			argString = argString[i:]
		}
	}
	return args, nil
}

// includePatterns returns include patterns from this directive.
func (d goDirective) includePatterns() ([]string, error) {
	if d.isEmbed() {
		patterns, err := d.args()
		if err != nil {
			return nil, err
		}
		for i, pattern := range patterns {
			patterns[i] = strings.TrimPrefix(pattern, "all:")
		}
		return d.prefixed(patterns), nil
	}
	if d.isTestInclude() || d.isGenerateInclude() {
		patterns, err := d.args()
		if err != nil {
			return nil, err
		}
		return d.prefixed(patterns), nil
	}
	return nil, nil
}

// prefixed resolves directive patterns relative to the directive's file.
func (d goDirective) prefixed(patterns []string) []string {
	for i, pattern := range patterns {
		if strings.HasPrefix(pattern, "/") {
			patterns[i] = strings.TrimPrefix(pattern, "/")
			continue
		}
		patterns[i] = path.Join(d.dir(), pattern)
	}
	return patterns
}

// hasName reports whether the directive has the exact directive name.
func (d goDirective) hasName(name string) bool {
	directiveName, _, ok := d.line()
	return ok && directiveName == name
}

// line splits a known //go: directive into its name and argument tail.
func (d goDirective) line() (string, string, bool) {
	if !strings.HasPrefix(d.comment, "//") {
		return "", "", false
	}
	line := strings.TrimPrefix(d.comment, "//")
	nameEnd := strings.IndexFunc(line, unicode.IsSpace)
	if nameEnd < 0 {
		nameEnd = len(line)
	}
	name := line[:nameEnd]
	if name != "go:embed" && name != "go:test:include" && name != "go:generate:include" && name != "go:generate" {
		return "", "", false
	}
	return name, line[nameEnd:], true
}

// generateGoDashC recognizes go generate commands that change directory with -C.
func (d goDirective) generateGoDashC() (string, bool, error) {
	if !d.isGenerate() {
		return "", false, nil
	}
	args, err := d.args()
	if err != nil {
		return "", false, err
	}
	if len(args) == 0 || args[0] != "go" {
		return "", false, nil
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-C" {
			if i+1 >= len(args) {
				return "", false, nil
			}
			return args[i+1], true, nil
		}
		if dir, ok := strings.CutPrefix(arg, "-C="); ok {
			return dir, true, nil
		}
		if !strings.HasPrefix(arg, "-") {
			return "", false, nil
		}
	}
	return "", false, nil
}
