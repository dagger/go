package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGenerationDirectoryDiscovery(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"ignored/_generate.go": "package ignored\n//go:generate echo no\n",
		"hidden/.generate.go":  "package hidden\n//go:generate echo no\n",
		"go.mod":               "module example.com/root\n",
		"generate.go":          "package root\n//go:generate echo root\n",
		"a/generate.go":        "package a\n//go:generate echo a\n",
		"a/another.go":         "package a\n//go:generate echo another\n",
		"z/generate.go":        "package z\n//go:generate\techo z\n",
		"include/generate.go":  "package include\n//go:generate:include asset\n",
		"comment/generate.go":  "package comment\n// go:generate echo no\n",
		"nested/go.mod":        "module example.com/nested\n",
		"nested/generate.go":   "package nested\n//go:generate echo nested\n",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	for module, want := range map[string][]string{
		".":      {".", "a", "z"},
		"nested": {"nested"},
	} {
		got, err := index.generateDirectoriesFor(module)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("module %s: got %q, %v; want %q", module, got, err, want)
		}
	}
	output := t.TempDir()
	if err := runAll([]string{"--all", "--generate", "--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(output, "_root_.generatedirs"))
	if err != nil || string(got) != ".\na\nz" {
		t.Fatalf("directory index: %q, %v", got, err)
	}
}
