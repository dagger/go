package main

import (
	"path"
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

	gotLintIncludes, gotLintModules, err := scanGoFileDirectives("pkg/includes_test.go", data, false, false)
	if err != nil {
		t.Fatal(err)
	}
	wantLintIncludes := []string{
		"pkg/assets/*.tmpl",
		"pkg/hidden",
	}
	if !reflect.DeepEqual(gotLintIncludes, wantLintIncludes) {
		t.Fatalf("lint includes mismatch:\n got: %#v\nwant: %#v", gotLintIncludes, wantLintIncludes)
	}
	if len(gotLintModules) != 0 {
		t.Fatalf("lint modules got %#v, want none", gotLintModules)
	}

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
