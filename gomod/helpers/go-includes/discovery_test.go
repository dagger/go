package main

import (
	"encoding/json"
	"fmt"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A header with go:embed lines goes on past them, so go/build's scan of its
// imports stops at a token before the end of the file: up to Go 1.25, it only
// looks for go:embed lines then, and without them go build -n leaves the
// package's embed configuration out. github.com/vito/booklit's errhtml, a
// package its main package imports, has the first shape here.
func TestHeaderEndsAfterEmbeds(t *testing.T) {
	for name, src := range map[string]string{
		"single": "package errhtml\n\nimport \"embed\"\n\n//go:embed *.tmpl *.css\nvar Assets embed.FS\n",
		"block":  "package x\n\nimport (\n\t\"embed\"\n\t\"fmt\"\n)\n\n//go:embed *.tmpl\nvar files embed.FS\n\nfunc F() { fmt.Println(files) }\n",
		"blank":  "package x\n\nimport _ \"embed\"\n\nvar (\n\t//go:embed a.tmpl\n\ta string\n\t//go:embed b.css\n\tb string\n)",
	} {
		t.Run(name, func(t *testing.T) {
			header := goHeader(name+".go", []byte(src))
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "", header, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("header does not parse: %v\n%s", err, header)
			}
			var end token.Pos
			for _, d := range f.Decls {
				end = max(end, d.End())
			}
			rest := header[fset.Position(end).Offset:]
			var s scanner.Scanner
			s.Init(token.NewFileSet().AddFile("", -1, len(rest)), rest, nil, scanner.ScanComments)
			stops := false
			for {
				_, tok, lit := s.Scan()
				if tok == token.EOF {
					break
				}
				if tok != token.COMMENT && !(tok == token.SEMICOLON && lit == "\n") {
					stops = true
					break
				}
			}
			if !stops {
				t.Errorf("the header ends in comments after its imports, where go/build's import scan reaches the end of the file:\n%s", header)
			}

			// The patterns go/build finds, which go build -n embeds: the
			// same in the header as in the source.
			pkg, err := build.ImportDir(writeTree(t, map[string]string{"x.go": string(header)}), 0)
			if err != nil {
				t.Fatal(err)
			}
			want, err := build.ImportDir(writeTree(t, map[string]string{"x.go": src}), 0)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pkg.EmbedPatterns, want.EmbedPatterns) || len(want.EmbedPatterns) == 0 {
				t.Errorf("go/build finds embed patterns %q in the header, %q in the source", pkg.EmbedPatterns, want.EmbedPatterns)
			}
		})
	}
	// A header without go:embed lines is as before.
	if src := "package x\n\nimport \"fmt\"\n\nfunc F() { fmt.Println() }\n"; string(goHeader("x.go", []byte(src))) != "package x\n\nimport \"fmt\"\n" {
		t.Errorf("header without embeds: %q", goHeader("x.go", []byte(src)))
	}
}

// A test may be named in any script: the go module's test search matched
// such names with ripgrep's Unicode \w, and discovery must still find them.
func TestFileKindsFindsUnicodeTestNames(t *testing.T) {
	root := writeTree(t, map[string]string{
		"x_test.go": "package x\n\nimport \"testing\"\n\nfunc Test中文(t *testing.T) {}\n",
		"y_test.go": "package x\n\nimport \"testing\"\n\nfunc Fuzzé(ф *testing.F) {}\n",
	})
	for _, name := range []string{"x_test.go", "y_test.go"} {
		src, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if k, _ := fileKinds(name, src, true); k != "t" {
			t.Errorf("%s: kinds %q; want a test", name, k)
		}
	}
}

func TestScanIndexesTestNames(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod": "module example.com/root\n\ngo 1.26.1\n",
		"a_test.go": `package root
import "testing"
func TestZ(t *testing.T) {}
func Fuzzé(ф *testing.F) {}
func Example() {
	// Output:
}
func TestMain(m *testing.M) {}
func Testify(t *testing.T) {}
func TestWrong(n int) {}
`,
		"z_test.go":                        "//go:build !never\n\npackage root\nimport \"testing\"\nfunc Test中文(t *testing.T) {}\nfunc TestZ(t *testing.T) {}\n",
		"sub/a_test.go":                    "package sub\nimport \"testing\"\nfunc TestZ(t *testing.T) {}\n",
		"sub/main.go":                      "package main\nfunc main() {}\n",
		"testdata/a_test.go":               "package ignored\nimport \"testing\"\nfunc TestIgnored(t *testing.T) {}\n",
		"vendor/example.com/lib/a_test.go": "package lib\nimport \"testing\"\nfunc TestVendored(t *testing.T) {}\n",
		"_ignored/a_test.go":               "package ignored\nimport \"testing\"\nfunc TestIgnored(t *testing.T) {}\n",
		"nested/go.mod":                    "module example.com/nested\n\ngo 1.26.1\n",
		"nested/a_test.go":                 "package nested\nimport \"testing\"\nfunc TestNested(t *testing.T) {}\n",
	})
	output := t.TempDir()
	if err := runAll([]string{"--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	catalog := readDiscoveryCatalog(t, output)
	// testdata, _ignored and vendored directories hold no packages.
	for module, want := range map[string][]string{".": {".", "sub"}, "nested": {"nested"}} {
		var dirs []string
		for _, pkg := range catalog.Packages[module] {
			dirs = append(dirs, pkg.Directory)
		}
		if !slices.Equal(dirs, want) {
			t.Errorf("module %q packages = %q; want %q", module, dirs, want)
		}
	}
	for _, tc := range []struct{ pkg, module, names string }{
		{".", ".", "TestZ\nFuzzé\nExample\nTest中文\nTestZ"},
		{"sub", ".", "TestZ"},
		{"nested", "nested", "TestNested"},
	} {
		pkg := catalogPackage(t, catalog, tc.module, tc.pkg)
		if strings.Join(pkg.Tests, "\n") != tc.names {
			t.Errorf("%s: got %#v; want module %q names %q", tc.pkg, pkg, tc.module, tc.names)
		}
	}
}

func TestDiscoveryIndexKeepsPackageKeysDistinct(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":               "module example.com/index\n\ngo 1.26.1\n",
		"main.go":              "package main\nfunc main() {}\n",
		"tests/main.go":        "package main\nfunc main() {}\n",
		"tests/module/main.go": "package main\nfunc main() {}\n",
		"module/tests/main.go": "package main\nfunc main() {}\n",
	})
	output := t.TempDir()
	if err := runAll([]string{"--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	catalog := readDiscoveryCatalog(t, output)
	for _, key := range []string{".", "tests", "tests/module", "module/tests"} {
		pkg := catalogPackage(t, catalog, ".", key)
		if pkg.Tests == nil || len(pkg.Tests) != 0 {
			t.Errorf("%s: got %#v; want an empty test array", key, pkg)
		}
	}
	paths := make([]string, len(catalog.Packages["."]))
	for i, pkg := range catalog.Packages["."] {
		paths[i] = pkg.Directory
	}
	if !slices.IsSorted(paths) {
		t.Errorf("package records are not in byte order: %q", paths)
	}
}

// Discovery lists only the tests go test builds and runs: a test file must
// build for the platform, with the configured cgo, Go version and GOFLAGS
// tags, and an example needs an output comment go test recognizes.
func TestDiscoveryListsOnlyTestsGoTestBuilds(t *testing.T) {
	test := func(name string) string { return "func " + name + "(t *testing.T) {}\n" }
	otherOS := "windows"
	if runtime.GOOS == otherOS {
		otherOS = "linux"
	}
	examples := "func ExampleShown() {\n\t// Output: shown\n}\n" +
		"func ExampleBlock() {\n\t/* Output: block */\n}\n" +
		"func ExampleHidden() {\n}\n" +
		"func ExampleTrailing() {\n\t// Output: trailing\n\n\t// A later comment means go test doesn't run it.\n}\n"
	root := writeTree(t, map[string]string{
		"go.mod":                     "module example.com/c\n\ngo 1.22\n",
		"a_test.go":                  "package c\nimport \"testing\"\n" + test("TestBuilt") + examples,
		"never_test.go":              "//go:build never\n\npackage c\nimport \"testing\"\n" + test("TestNever"),
		"tagged_test.go":             "//go:build integration\n\npackage c\nimport \"testing\"\n" + test("TestTagged"),
		"os_" + otherOS + "_test.go": "package c\nimport \"testing\"\n" + test("TestOtherOS"),
		"cgo_test.go":                "//go:build cgo\n\npackage c\nimport \"testing\"\n" + test("TestCgo"),
		"newer_test.go":              "//go:build go1.23\n\npackage c\nimport \"testing\"\n" + test("TestNewerGo"),
		"module_go_test.go":          "//go:build go1.22\n\npackage c\nimport \"testing\"\n" + test("TestModuleGo"),
		"excluded/excluded_test.go":  "//go:build never\n\npackage excluded\nimport \"testing\"\n" + test("TestExcluded"),
	})
	built := []string{"TestBuilt", "ExampleShown", "ExampleBlock"}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{nil, append(built, "TestModuleGo")},
		{[]string{"--goflags", "-count=1\t'-tags=other integration'"}, append(built, "TestModuleGo", "TestTagged")},
		{[]string{"--go-version", "go1.26.1", "--cgo"}, append(built, "TestCgo", "TestModuleGo", "TestNewerGo")},
	} {
		output := t.TempDir()
		if err := runAll(append([]string{"--root", root, "--output-dir", output}, tc.args...)); err != nil {
			t.Fatal(err)
		}
		catalog := readDiscoveryCatalog(t, output)
		if packages := catalog.Packages["."]; len(packages) != 1 || !slices.Equal(packages[0].Tests, tc.want) {
			t.Errorf("%q: packages %#v; want only . with tests %q", tc.args, packages, tc.want)
		}
	}
}

// GOFLAGS is split as cmd/go splits it, and its last -tags is parsed as
// cmd/go's -tags flag parses it.
func TestTestConfigReadsGOFLAGSTags(t *testing.T) {
	for goflags, want := range map[string][]string{
		"":                           nil,
		"-count=1":                   nil,
		"-tags=a,b":                  {"a", "b"},
		"--tags=a,,b":                {"a", "b"},
		"-count=1\t-tags=a\n-race":   {"a", "race"},
		"-tags=a -tags=b":            {"b"},
		`"-tags=a b" -count=1`:       {"a", "b"},
		`'-tags=a' '-ldflags=-s -w'`: {"a"},
		"-tags=":                     nil,
		"-race -tags=a":              {"a", "race"},
		"--race=true -asan=false":    {"race"},
		"-msan -race -msan=0":        {"race"},
	} {
		config, err := newTestConfig(goflags, "", false)
		if err != nil || !slices.Equal(config.tags, want) {
			t.Errorf("GOFLAGS %q: tags %q, %v; want %q", goflags, config.tags, err, want)
		}
	}
	if _, err := newTestConfig("'-tags=a", "", false); err == nil {
		t.Error("accepted an unterminated quote")
	}
}

// A module's own directory is its first package in workspace-path order, even
// beside a directory whose relative name sorts before ".".
func TestDiscoveryIndexOrdersNestedModuleByWorkspacePath(t *testing.T) {
	root := writeTree(t, map[string]string{
		"m/go.mod":              "module example.com/m\n\ngo 1.26.1\n",
		"m/m_test.go":           "package m\nimport \"testing\"\nfunc TestRoot(t *testing.T) {}\n",
		"m/-tools/t_test.go":    "package tools\nimport \"testing\"\nfunc TestTools(t *testing.T) {}\n",
		"m/tools/tools_test.go": "package tools\nimport \"testing\"\nfunc TestMore(t *testing.T) {}\n",
	})
	output := t.TempDir()
	if err := runAll([]string{"--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, pkg := range readDiscoveryCatalog(t, output).Packages["m"] {
		paths = append(paths, pkg.Directory)
	}
	if want := []string{"m", "m/-tools", "m/tools"}; !slices.Equal(paths, want) {
		t.Errorf("package records = %q; want %q", paths, want)
	}
}

type completeDiscoveryCatalog struct {
	Roots    []string
	Modules  map[string][]string
	Packages map[string][]discoveryPackage
}

func catalogPackage(t *testing.T, catalog completeDiscoveryCatalog, module, directory string) discoveryPackage {
	t.Helper()
	for _, pkg := range catalog.Packages[module] {
		if pkg.Directory == directory {
			return pkg
		}
	}
	t.Fatalf("package %q not found in module %q", directory, module)
	return discoveryPackage{}
}

func readDiscoveryCatalog(t *testing.T, output string) completeDiscoveryCatalog {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(output, discoveryCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	var catalog discoveryCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	complete := completeDiscoveryCatalog{
		Roots: catalog.Roots, Modules: map[string][]string{}, Packages: map[string][]discoveryPackage{},
	}
	for root, ordinal := range catalog.Modules {
		data, err := os.ReadFile(filepath.Join(output, discoveryModulesDir, strconv.Itoa(ordinal)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var module discoveryModule
		if err := json.Unmarshal(data, &module); err != nil {
			t.Fatal(err)
		}
		complete.Modules[root] = module.Lines
		complete.Packages[root] = module.Packages
	}
	return complete
}

func TestDiscoveryModuleFilesKeepPathAndTestDataSeparate(t *testing.T) {
	longRoot := strings.Repeat("a", 120) + "/" + strings.Repeat("b", 120)
	roots := []string{".", "0.json", "index.json", "parts/0.json", longRoot}
	files := map[string]string{}
	for i, root := range roots {
		files[filepath.Join(root, "go.mod")] = fmt.Sprintf("module example.com/module%d\n\ngo 1.26.1\n", i)
		files[filepath.Join(root, "main_test.go")] = fmt.Sprintf("package example\nimport \"testing\"\nfunc TestCatalogueMarker%d(t *testing.T) {}\n", i)
	}
	root := writeTree(t, files)
	output := t.TempDir()
	if err := runAll([]string{"--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, discoveryCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "TestCatalogueMarker") {
		t.Fatal("reading the small module index would also read test data")
	}
	catalog := readDiscoveryCatalog(t, output)
	if len(catalog.Roots) != len(roots) {
		t.Fatalf("module roots = %q; want %q", catalog.Roots, roots)
	}
	for i, module := range roots {
		pkg := catalogPackage(t, catalog, module, module)
		if len(pkg.Tests) != 1 || pkg.Tests[0] != fmt.Sprintf("TestCatalogueMarker%d", i) {
			t.Errorf("module %q read another module's data: %#v", module, pkg)
		}
	}
}

func TestGoIgnoresVendoredPackages(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"vendor/example.com/lib/a_test.go", true},
		{"pkg/vendor/lib/a_test.go", true},
		{"vendor/a_test.go", false},
		{"pkg/vendor/a_test.go", false},
		{"vendored/a_test.go", false},
	} {
		if got := goIgnores(tc.path); got != tc.want {
			t.Errorf("goIgnores(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Generated files can hold lines longer than any scanner buffer.
func TestFileKindsReadsLongLines(t *testing.T) {
	root := writeTree(t, map[string]string{
		"gen.go": "package main\n\nvar x = \"" + strings.Repeat("a", 17<<20) + "\"\n\nfunc main() {}\n",
	})
	src, err := os.ReadFile(filepath.Join(root, "gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := fileKinds("gen.go", src, false); k != "m" {
		t.Errorf("kinds %q; want a main package", k)
	}
}

// A Go file that can't be read fails its own module's scan, as before, and
// leaves the header pack and discovery of the other modules whole.
func TestUnreadableFileStaysInItsModule(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/go.mod":  "module example.com/a\n\ngo 1.25\n",
		"a/main.go": "package main\n\nfunc main() {}\n",
		"b/go.mod":  "module example.com/b\n\ngo 1.25\n",
		"b/main.go": "package main\n\nfunc main() {}\n",
	})
	if err := os.Symlink(filepath.Join(root, "missing.go"), filepath.Join(root, "b", "broken.go")); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	if err := runAll([]string{"--root", root, "--output-dir", output}); err != nil {
		t.Fatalf("one unreadable file failed the whole scan: %v", err)
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	catalog := readDiscoveryCatalog(t, output)
	for _, root := range []string{"a", "b"} {
		if want := "package\t" + root + "\t.\tm"; !slices.Contains(catalog.Modules[root], want) {
			t.Errorf("discovery lost %q: %q", want, catalog.Modules[root])
		}
	}
	if pack := read(headerPackFile); !strings.Contains(pack, "a/main.go\n") || !strings.Contains(pack, "b/main.go\n") {
		t.Errorf("header pack lost a readable file")
	}
	if got := read("a.err"); got != "" {
		t.Errorf("a readable module recorded a reason: %q", got)
	}
	if got := read("b.err"); !strings.Contains(got, "broken.go") {
		t.Errorf("the unreadable file's module reason: %q", got)
	}
}

// The header pack holds what a build plan reads of a //go:embed's files that
// the plan's view of the source leaves out: test files, as headers, and the
// files in a directory named like a Go file, whole.
func TestHeaderPackHoldsEmbeddable(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":                     "module example.com/x\n\ngo 1.25\n",
		"x.go":                       "package x\n",
		"x_test.go":                  "package x\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { t.Log(\"body\") }\n",
		"tmpl/_dagger.gen.go/a.tmpl": "{{ .A }}\n",
	})
	if err := os.Symlink("a.tmpl", filepath.Join(root, "tmpl", "_dagger.gen.go", "link.tmpl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("x_test.go", filepath.Join(root, "link_test.go")); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	if err := runAll([]string{"--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, headerPackFile))
	if err != nil {
		t.Fatal(err)
	}
	pack := string(data)
	if !strings.Contains(pack, "x_test.go\n") || strings.Contains(pack, "body") {
		t.Errorf("the test file is not in the pack as its header:\n%s", pack)
	}
	if !strings.Contains(pack, "tmpl/_dagger.gen.go/a.tmpl\n") || !strings.Contains(pack, "{{ .A }}") {
		t.Errorf("the file in a directory named like a Go file is not in the pack whole:\n%s", pack)
	}
	if want := fmt.Sprintf("tmpl/_dagger.gen.go/link.tmpl\n%o\n6\na.tmpl\n", uint32(fs.ModeSymlink|0o777)); !strings.Contains(pack, want) {
		t.Errorf("the symlink is not in the pack as one:\n%s", pack)
	}
	if want := fmt.Sprintf("link_test.go\n%o\n9\nx_test.go\n", uint32(fs.ModeSymlink|0o777)); !strings.Contains(pack, want) {
		t.Errorf("the test file symlink is not in the pack as one:\n%s", pack)
	}
}
