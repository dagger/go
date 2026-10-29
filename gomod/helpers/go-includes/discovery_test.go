package main

import (
	"fmt"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
		if k, _ := fileKinds(src, true); k != "t" {
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
func Example() {}
func TestMain(m *testing.M) {}
func Testify(t *testing.T) {}
func TestWrong(n int) {}
`,
		"z_test.go":          "//go:build never\n\npackage root\nimport \"testing\"\nfunc Test中文(t *testing.T) {}\nfunc TestZ(t *testing.T) {}\n",
		"sub/a_test.go":      "package sub\nimport \"testing\"\nfunc TestZ(t *testing.T) {}\n",
		"sub/main.go":        "package main\nfunc main() {}\n",
		"testdata/a_test.go": "package ignored\nimport \"testing\"\nfunc TestIgnored(t *testing.T) {}\n",
		"_ignored/a_test.go": "package ignored\nimport \"testing\"\nfunc TestIgnored(t *testing.T) {}\n",
		"nested/go.mod":      "module example.com/nested\n\ngo 1.26.1\n",
		"nested/a_test.go":   "package nested\nimport \"testing\"\nfunc TestNested(t *testing.T) {}\n",
	})
	output := t.TempDir()
	if err := runAll([]string{"--all", "--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, testNamesFile))
	if err != nil {
		t.Fatal(err)
	}
	want := ".\tTestZ\n.\tFuzzé\n.\tExample\nsub\tTestZ\n.\tTest中文\n.\tTestZ\nnested\tTestNested\n"
	if string(data) != want {
		t.Fatalf("test index lost file/source order or module boundaries:\n got: %q\nwant: %q", data, want)
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
	if k, _ := fileKinds(src, false); k != "m" {
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
	if err := runAll([]string{"--all", "--root", root, "--output-dir", output}); err != nil {
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
	discovery := read(discoveryFile)
	for _, want := range []string{"package\ta\t.\tm\n", "package\tb\t.\tm\n"} {
		if !strings.Contains(discovery, want) {
			t.Errorf("discovery lost %q:\n%s", want, discovery)
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
	if err := runAll([]string{"--all", "--root", root, "--output-dir", output}); err != nil {
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
