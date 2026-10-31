// The gomod module runs this helper to discover workspace include patterns.
// Results are written to a file, one entry per line, so they don't pollute the
// user's terminal as exec "logs".
package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/mod/modfile"
)

func main() {
	if err := runAll(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// containingModuleDir finds the nearest ancestor module root in moduleSet.
func containingModuleDir(moduleSet map[string]bool, dir string) (string, bool) {
	dir = path.Clean(strings.TrimPrefix(dir, "/"))
	for {
		if moduleSet[dir] {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
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

// includeBasePatterns returns the static Go source patterns for a module root.
func includeBasePatterns(moduleRoot string) []string {
	patterns := []string{
		"**/*.go",
		"**/*.c",
		"**/*.cc",
		"**/*.cpp",
		"**/*.cxx",
		"**/*.h",
		"**/*.hh",
		"**/*.hpp",
		"**/*.hxx",
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
		// A vendor directory puts the go command in vendor mode, which reads
		// this to check the vendored modules against go.mod.
		"vendor/modules.txt",
		// go build uses a main package's default.pgo for profile-guided
		// optimization, unless a -pgo flag says otherwise.
		"**/default.pgo",
	}
	for i, pattern := range patterns {
		patterns[i] = path.Join(moduleRoot, pattern)
	}
	return patterns
}

func isLocalReplace(replace *modfile.Replace) bool {
	return replace.New.Version == "" && modfile.IsDirectoryPath(replace.New.Path)
}

// parseGoMod parses go.mod contents already read from goModPath.
//
// Every error names goModPath. A scan covers every module in the workspace at
// once, so an unattributed complaint about "go.mod" leaves the reader to guess
// which of them is at fault.
func parseGoMod(goModPath string, data []byte) (*modfile.File, error) {
	goMod, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, err
	}
	if goMod.Module == nil {
		// modfile tolerates a file with no module line; no Go command does.
		// Caught here it names the file, and the module is never mounted or
		// run. Left to the toolchain it surfaces later, from whichever
		// container happened to run first, as a bare "go: error reading
		// go.mod: missing module declaration".
		return nil, fmt.Errorf("%s: missing module declaration", goModPath)
	}
	return goMod, nil
}

// replacePosition is the file:line a replace directive is written at, so a
// target that resolves to nothing points at the line that declares it.
func replacePosition(goModPath string, replace *modfile.Replace) string {
	if replace.Syntax == nil || replace.Syntax.Start.Line == 0 {
		return goModPath
	}
	return fmt.Sprintf("%s:%d", goModPath, replace.Syntax.Start.Line)
}

// goDirectivesInFile extracts Go comment directives from one Go file. It
// reads the file as go/build does: parsed only up to the end of its imports,
// so a syntax error after them is the compiler's to report.
func goDirectivesInFile(filePath string, data []byte) ([]goDirective, error) {
	if _, err := parser.ParseFile(token.NewFileSet(), filePath, data, parser.ImportsOnly|parser.SkipObjectResolution); err != nil {
		return nil, err
	}
	return directivesIn(filePath, data), nil
}

// directivesIn returns the supported directive comments of a Go file, in
// source order, at the positions a parse gives them.
//
// It finds the comments without a parse, skipping strings, raw strings and
// runes as go/build does when it looks for go:embed lines: a full parse cost
// the scan most of its time. A //line or /*line comment moves the positions
// after it, so a file that may hold one is scanned by go/scanner, which
// applies them.
func directivesIn(filePath string, src []byte) []goDirective {
	if bytes.Contains(src, []byte("//line ")) || bytes.Contains(src, []byte("/*line ")) {
		return scannedDirectives(filePath, src)
	}
	var directives []goDirective
	var lines *token.File
	lineComments(src, func(start, end int) {
		text := src[start:end]
		// The scanner drops a comment's carriage returns.
		if bytes.IndexByte(text, '\r') >= 0 {
			text = bytes.ReplaceAll(text, []byte("\r"), nil)
		}
		if !bytes.HasPrefix(text, []byte("//go:")) {
			return
		}
		if lines == nil {
			lines = token.NewFileSet().AddFile(filePath, -1, len(src))
			lines.SetLinesForContent(src)
		}
		if directive, ok := supportedDirective(filePath, lines.Position(lines.Pos(start)), string(text)); ok {
			directives = append(directives, directive)
		}
	})
	return directives
}

// scannedDirectives is directivesIn for a file that may hold line directives.
func scannedDirectives(filePath string, src []byte) []goDirective {
	fset := token.NewFileSet()
	var s scanner.Scanner
	s.Init(fset.AddFile(filePath, -1, len(src)), src, nil, scanner.ScanComments)
	var directives []goDirective
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return directives
		}
		if tok == token.COMMENT {
			if directive, ok := supportedDirective(filePath, fset.Position(pos), lit); ok {
				directives = append(directives, directive)
			}
		}
	}
}

func supportedDirective(filePath string, position token.Position, comment string) (goDirective, bool) {
	directive := goDirective{
		filePath: filePath,
		position: position.String(),
		comment:  comment,
	}
	ok := directive.isEmbed() || directive.isTestInclude() || directive.isGenerateInclude() || directive.hasName("go:generate:container") || directive.isGenerate()
	return directive, ok
}

// lineComments calls yield with the offsets of each // comment in src, from
// its first slash to the end of its line. Strings, raw strings, runes and
// /* comments are skipped, so a // inside one is not a comment.
func lineComments(src []byte, yield func(start, end int)) {
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '"', '\'':
			quote := src[i]
			for i++; i < len(src) && src[i] != quote && src[i] != '\n'; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '`':
			for i++; i < len(src) && src[i] != '`'; i++ {
			}
		case '/':
			if i+1 == len(src) {
				return
			}
			switch src[i+1] {
			case '/':
				end := bytes.IndexByte(src[i:], '\n')
				if end < 0 {
					end = len(src) - i
				}
				yield(i, i+end)
				i += end
			case '*':
				end := bytes.Index(src[i+2:], []byte("*/"))
				if end < 0 {
					return
				}
				i += 2 + end + 1
			}
		}
	}
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
	if name != "go:embed" && name != "go:test:include" && name != "go:generate:include" && name != "go:generate:container" && name != "go:generate" {
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
