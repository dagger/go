package main

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

func TestGoDirectiveIncludesByMode(t *testing.T) {
	data := []byte(`package includes

import "embed"

//go:test:include local.txt "../shared file.txt" ` + "`raw path.txt`" + `
//go:generate:include generator.txt
//go:embed assets/*.tmpl all:hidden
//go:generate go -C .. run ./cmd/codegen
var embedded embed.FS
`)

	gotTestIncludes, gotTestModules, err := scanGoFileDirectives("pkg/includes_test.go", data, true, false)
	if err != nil {
		t.Fatal(err)
	}
	wantTestIncludes := []string{
		"pkg/local.txt",
		"shared file.txt",
		"pkg/raw path.txt",
		"pkg/assets/*.tmpl",
		"pkg/hidden",
	}
	if !reflect.DeepEqual(gotTestIncludes, wantTestIncludes) {
		t.Fatalf("test includes mismatch:\n got: %#v\nwant: %#v", gotTestIncludes, wantTestIncludes)
	}
	if len(gotTestModules) != 0 {
		t.Fatalf("test modules got %#v, want none", gotTestModules)
	}

	gotGenerateIncludes, gotGenerateModules, err := scanGoFileDirectives("pkg/includes_test.go", data, false, true)
	if err != nil {
		t.Fatal(err)
	}
	wantGenerateIncludes := []string{
		"pkg/generator.txt",
		"pkg/assets/*.tmpl",
		"pkg/hidden",
	}
	wantGenerateModules := []string{"."}
	if !reflect.DeepEqual(gotGenerateIncludes, wantGenerateIncludes) {
		t.Fatalf("generate includes mismatch:\n got: %#v\nwant: %#v", gotGenerateIncludes, wantGenerateIncludes)
	}
	if !reflect.DeepEqual(gotGenerateModules, wantGenerateModules) {
		t.Fatalf("generate modules mismatch:\n got: %#v\nwant: %#v", gotGenerateModules, wantGenerateModules)
	}

	gotCombinedIncludes, gotCombinedModules, err := scanGoFileDirectives("pkg/includes_test.go", data, true, true)
	if err != nil {
		t.Fatal(err)
	}
	wantCombinedIncludes := []string{
		"pkg/local.txt",
		"shared file.txt",
		"pkg/raw path.txt",
		"pkg/generator.txt",
		"pkg/assets/*.tmpl",
		"pkg/hidden",
	}
	wantCombinedModules := []string{"."}
	if !reflect.DeepEqual(gotCombinedIncludes, wantCombinedIncludes) {
		t.Fatalf("combined includes mismatch:\n got: %#v\nwant: %#v", gotCombinedIncludes, wantCombinedIncludes)
	}
	if !reflect.DeepEqual(gotCombinedModules, wantCombinedModules) {
		t.Fatalf("combined modules mismatch:\n got: %#v\nwant: %#v", gotCombinedModules, wantCombinedModules)
	}
}

func TestIncludeHelpers(t *testing.T) {
	ws := &workspace{moduleSet: map[string]bool{
		".":       true,
		"pkg/mod": true,
	}}
	if got, ok := ws.containingModuleDir("pkg/mod/subdir"); !ok || got != "pkg/mod" {
		t.Fatalf("workspace.containingModuleDir got %q, %v", got, ok)
	}

}

func TestIsLocalReplace(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		version string
		want    bool
	}{
		{name: "current directory", path: ".", want: true},
		{name: "parent directory", path: "..", want: true},
		{name: "current subdirectory", path: "./local", want: true},
		{name: "parent subdirectory", path: "../local", want: true},
		{name: "absolute directory", path: "/local", want: true},
		{name: "module path", path: "example.com/local", want: false},
		{name: "versioned module path", path: "example.com/local", version: "v1.0.0", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			replace := &modfile.Replace{
				New: module.Version{
					Path:    test.path,
					Version: test.version,
				},
			}
			if got := isLocalReplace(replace); got != test.want {
				t.Fatalf("isLocalReplace() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestIncludeBasePreservesNestedModuleBoundaries(t *testing.T) {
	got := (targetModule{moduleRoot: "pkg"}).includeBase()
	want := []string{
		"pkg/**/*.go",
		"pkg/**/*.c",
		"pkg/**/*.cc",
		"pkg/**/*.cpp",
		"pkg/**/*.cxx",
		"pkg/**/*.h",
		"pkg/**/*.hh",
		"pkg/**/*.hpp",
		"pkg/**/*.hxx",
		"pkg/**/*.s",
		"pkg/**/*.S",
		"pkg/**/*.syso",
		"pkg/go.mod",
		"pkg/**/go.mod",
		"pkg/go.sum",
		"pkg/**/go.sum",
		"pkg/go.work",
		"pkg/go.work.sum",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includeBase mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestTestDirectoriesFromFiles(t *testing.T) {
	got := testDirectoriesFromFiles([]string{
		"root_test.go",
		"pkg/b/b_test.go",
		"pkg/a/another_test.go",
		"pkg/a/a_test.go",
	})
	want := []string{
		".",
		"pkg/a",
		"pkg/b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("testDirectoriesFromFiles mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLocalIndexTestDirectories(t *testing.T) {
	index := &localIndex{goFilesByModule: map[string][]string{
		"api": {
			"api/auth/auth.go",
			"api/auth/auth_test.go",
			"api/auth/more_test.go",
			"api/db/db_test.go",
		},
	}}
	want := []string{"api/auth", "api/db"}
	if got := index.testDirectoriesFor("api"); !reflect.DeepEqual(got, want) {
		t.Fatalf("testDirectoriesFor mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestInvalidQuotedDirectiveArg(t *testing.T) {
	_, err := (goDirective{
		position: "test.go:1:1",
		comment:  `//go:test:include "unterminated`,
	}).includePatterns()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRelativeCLIPathRejected(t *testing.T) {
	_, _, _, err := newTargetModuleFromArgs(t.Context(), []string{"--output", "/tmp/out", "relative/module"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "workspace path must be absolute: relative/module") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNonDirectiveCommentsAreIgnored(t *testing.T) {
	tests := []string{
		"// go:embed assets",
		"// go:generate go -C . run ./cmd/codegen",
		"//go:test:included assets",
		"//go:generate:included assets",
		"//go:generate:include assets",
		"// workspace:include assets",
		"/* go:test:include assets */",
	}
	for _, test := range tests {
		directive := goDirective{comment: test}
		var got []string
		if directive.isEmbed() || directive.isTestInclude() {
			var err error
			got, err = directive.includePatterns()
			if err != nil {
				t.Fatalf("%q: %v", test, err)
			}
		}
		_, isGenerateGoDashC, err := directive.generateGoDashC()
		if err != nil {
			t.Fatalf("%q: %v", test, err)
		}
		if len(got) != 0 || isGenerateGoDashC {
			t.Fatalf("%q: got includes %#v", test, got)
		}
	}
}

func scanGoFileDirectives(filePath string, data []byte, test, generate bool) ([]string, []string, error) {
	directives, err := goDirectivesInFile(filePath, data)
	if err != nil {
		return nil, nil, err
	}
	var includes []string
	var modules []string

	for _, directive := range directives {
		switch {
		case directive.isEmbed():
		case generate && directive.isGenerateInclude():
		case test && directive.isTestInclude():
		default:
			if !generate {
				continue
			}
			workdir, ok, err := directive.generateGoDashC()
			if err != nil {
				return nil, nil, err
			}
			if ok {
				modules = append(modules, path.Join(directive.dir(), workdir))
			}
			continue
		}
		patterns, err := directive.includePatterns()
		if err != nil {
			return nil, nil, err
		}
		includes = append(includes, patterns...)
	}
	return includes, modules, nil
}

// writeTree writes a map of relative paths to contents under a new temp root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, contents := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A go.mod with no module line is not a module to any Go command, and the
// toolchain's own complaint about it names no file. The scan has to.
func TestGoModMissingModuleDeclarationNamesFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":               "module example.com/root\n\ngo 1.25\n",
		"root.go":              "package root\n",
		"vendored/go.mod":      "// no module line here\n\ngo 1.25\n",
		"vendored/vendored.go": "package vendored\n",
	})
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = index.includesFor("vendored", false, false)
	if err == nil {
		t.Fatal("a go.mod with no module declaration was accepted")
	}
	if got := err.Error(); got != "vendored/go.mod: missing module declaration" {
		t.Fatalf("error did not name the file: %q", got)
	}
}

// An empty go.mod is the same fault, and reaches the same message.
func TestEmptyGoModNamesFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/go.mod": "",
		"a/a.go":   "package a\n",
	})
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.includesFor("a", false, false); err == nil ||
		err.Error() != "a/go.mod: missing module declaration" {
		t.Fatalf("empty go.mod: got %v", err)
	}
}

// A replace target that resolves to no module names the go.mod and the line
// that declares it, not just the target path.
func TestUnresolvableReplaceNamesDeclaringLine(t *testing.T) {
	// No module above app: containingModuleDir walks up, so an ancestor would
	// absorb the bad target instead of failing.
	root := writeTree(t, map[string]string{
		"app/go.mod": "module example.com/app\n\n" +
			"go 1.25\n\n" +
			"require example.com/lib v0.0.0\n\n" +
			"replace example.com/lib => ../nowhere\n",
		"app/app.go": "package app\n",
	})
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = index.includesFor("app", false, false)
	if err == nil {
		t.Fatal("an unresolvable local replace was accepted")
	}
	want := "app/go.mod:7: no Go module found for local replace target: ../nowhere"
	if got := err.Error(); got != want {
		t.Fatalf("replace error:\n got: %q\nwant: %q", got, want)
	}
}

// One module the scan cannot read must not cost every other module its slice
// of the scan: the whole workspace shares one pass.
func TestScanRecordsFailurePerModuleAndCarriesOn(t *testing.T) {
	root := writeTree(t, map[string]string{
		"broken/go.mod":      "// no module line\n\ngo 1.25\n",
		"broken/b.go":        "package broken\n",
		"good/go.mod":        "module example.com/good\n\ngo 1.25\n",
		"good/g.go":          "package good\n",
		"good/g_test.go":     "package good\n",
		"unparseable/go.mod": "module example.com/unparseable\n\ngo 1.25\n",
		"unparseable/u.go":   "package unparseable\n\nfunc {{{\n",
	})
	output := t.TempDir()
	if err := runAll([]string{"--all", "--root", root, "--output-dir", output}); err != nil {
		t.Fatalf("one bad module failed the whole scan: %v", err)
	}

	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if got := strings.TrimSpace(read("broken.err")); got != "broken/go.mod: missing module declaration" {
		t.Errorf("broken module reason: %q", got)
	}
	if got := strings.TrimSpace(read("unparseable.err")); !strings.HasPrefix(got, "unparseable/u.go:") {
		t.Errorf("unparseable module reason did not name the file: %q", got)
	}
	if got := read("good.err"); got != "" {
		t.Errorf("a readable module recorded a reason: %q", got)
	}
	if got := read("good.inc"); !strings.Contains(got, "good/**/*.go") {
		t.Errorf("a readable module lost its includes: %q", got)
	}
	if got := strings.TrimSpace(read("good.testdirs")); got != "good" {
		t.Errorf("a readable module lost its test directories: %q", got)
	}
	// A module with no slice still gets its files, so a reader never has to
	// tell "absent" from "empty".
	if got := read("broken.inc"); got != "" {
		t.Errorf("broken module wrote includes: %q", got)
	}
}

// A directory holding a go.mod is not automatically a Go module a tool can
// work on. Handing one that is not to golangci-lint produces a complaint about
// golangci-lint, so the scan leaves it out of the module list.
func TestScanListsOnlyRealGoModules(t *testing.T) {
	root := writeTree(t, map[string]string{
		"real/go.mod": "module example.com/real\n\ngo 1.25\n",
		"real/r.go":   "package real\n",

		// A module whose only Go files are ones Go itself skips.
		"real/testdata/fixture/go.mod": "module example.com/fixture\n\ngo 1.25\n",
		"real/testdata/fixture/f.go":   "package fixture\n",

		"no-files/go.mod":    "module example.com/nofiles\n\ngo 1.25\n",
		"no-files/README.md": "no Go here\n",

		"ignored-files/go.mod":           "module example.com/ignored\n\ngo 1.25\n",
		"ignored-files/testdata/skip.go": "package skip\n",
		"ignored-files/_scratch/skip.go": "package skip\n",
		"ignored-files/.hidden/skip.go":  "package skip\n",

		"no-module-line/go.mod": "// nothing here\n\ngo 1.25\n",
		"no-module-line/n.go":   "package nomoduleline\n",

		"unparseable-mod/go.mod": "this is not a go.mod\n",
		"unparseable-mod/u.go":   "package unparseable\n",
	})
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"real", "real/testdata/fixture"}
	if got := index.goModules(); !reflect.DeepEqual(got, want) {
		t.Errorf("go modules:\n got: %q\nwant: %q", got, want)
	}

	// A module below a testdata directory is still a module: Go's ignore rules
	// apply below the module root, not above it.
	if reason := index.moduleSkipReason("real/testdata/fixture"); reason != "" {
		t.Errorf("a module under testdata was skipped: %s", reason)
	}
	for _, skipped := range []string{"no-files", "ignored-files"} {
		if reason := index.moduleSkipReason(skipped); reason != skipped+"/go.mod: module holds no Go files" {
			t.Errorf("%s: reason %q", skipped, reason)
		}
	}
	if reason := index.moduleSkipReason("no-module-line"); reason != "no-module-line/go.mod: missing module declaration" {
		t.Errorf("no-module-line: reason %q", reason)
	}
	if reason := index.moduleSkipReason("unparseable-mod"); !strings.HasPrefix(reason, "unparseable-mod/go.mod:") {
		t.Errorf("unparseable-mod: reason did not name the file: %q", reason)
	}

	// The list is written out for the module layer to filter discovery by.
	output := t.TempDir()
	if err := runAll([]string{"--all", "--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "_modules_"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, want) {
		t.Errorf("_modules_:\n got: %q\nwant: %q", got, want)
	}
}

// The Go version a module's commands run under comes from its own go directive,
// truncated to the minor series: a directive is a minimum, and the series tag
// is always its newest published patch.
func TestModuleGoVersion(t *testing.T) {
	root := writeTree(t, map[string]string{
		"minor/go.mod": "module example.com/minor\n\ngo 1.25\n",
		"minor/m.go":   "package minor\n",
		"patch/go.mod": "module example.com/patch\n\ngo 1.26.1\n",
		"patch/p.go":   "package patch\n",
		"toolchain/go.mod": "module example.com/toolchain\n\n" +
			"go 1.26.0\n\ntoolchain go1.26.1\n",
		"toolchain/t.go": "package toolchain\n",
		"none/go.mod":    "module example.com/none\n",
		"none/n.go":      "package none\n",
	})
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	for module, want := range map[string]string{
		"minor":     "1.25",
		"patch":     "1.26",
		"toolchain": "1.26",
		"none":      "",
	} {
		if got := index.goVersion(module); got != want {
			t.Errorf("%s: go version %q, want %q", module, got, want)
		}
	}

	output := t.TempDir()
	if err := runAll([]string{"--all", "--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "patch.goversion"))
	if err != nil || string(data) != "1.26" {
		t.Fatalf("patch.goversion: %q, %v", data, err)
	}
	// A module that declares none records nothing, so the caller supplies its
	// own fallback rather than being handed a guess.
	if data, err := os.ReadFile(filepath.Join(output, "none.goversion")); err != nil || string(data) != "" {
		t.Fatalf("none.goversion: %q, %v", data, err)
	}
}
