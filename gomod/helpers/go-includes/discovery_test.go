package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A test may be named in any script: the go module's test search matched
// such names with ripgrep's Unicode \w, and discovery must still find them.
func TestFileKindsFindsUnicodeTestNames(t *testing.T) {
	root := writeTree(t, map[string]string{
		"x_test.go": "package x\n\nimport \"testing\"\n\nfunc Test中文(t *testing.T) {}\n",
		"y_test.go": "package x\n\nimport \"testing\"\n\nfunc Fuzzé(ф *testing.F) {}\n",
	})
	for _, name := range []string{"x_test.go", "y_test.go"} {
		k, err := fileKinds(filepath.Join(root, name), true)
		if err != nil || k != "t" {
			t.Errorf("%s: kinds %q, err %v; want a test", name, k, err)
		}
	}
}

// Generated files can hold lines longer than any scanner buffer.
func TestFileKindsReadsLongLines(t *testing.T) {
	root := writeTree(t, map[string]string{
		"gen.go": "package main\n\nvar x = \"" + strings.Repeat("a", 17<<20) + "\"\n\nfunc main() {}\n",
	})
	k, err := fileKinds(filepath.Join(root, "gen.go"), false)
	if err != nil || k != "m" {
		t.Errorf("kinds %q, err %v; want a main package", k, err)
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
