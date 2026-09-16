package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGenerationDirectorySelection(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		dir      string
		want     bool
	}{
		{nil, "a/b", true},
		{[]string{"."}, ".", true},
		{[]string{"."}, "pkg", false},
		{[]string{"sdk/go"}, "sdk/go", true},
		{[]string{"sdk/go"}, "sdk/go/engineconn", false},
		{[]string{"sdk/go/engineconn"}, "sdk/go", false},
		{[]string{"**"}, ".", true},
		{[]string{"*"}, ".", false},
		{[]string{"*"}, "sdk", true},
		{[]string{"*"}, "sdk/go", false},
		{[]string{"sdk/**"}, "sdk", true},
		{[]string{"sdk/**"}, "sdk/go/engineconn", true},
		{[]string{"sdk/**/engineconn"}, "sdk/engineconn", true},
		{[]string{"sdk/**/engineconn"}, "sdk/go/engineconn", true},
		{[]string{"sdk/*/engineconn"}, "sdk/go/engineconn", true},
		{[]string{"pkg/[ab]?"}, "pkg/a1", true},
		{[]string{"!sdk"}, "sdk/go", true},
		{[]string{"!sdk/**"}, "sdk/go", false},
		{[]string{"**", "!sdk/**", "sdk/go"}, "sdk/go", false},
		{[]string{"!**"}, ".", false},
		{[]string{"./sdk/go/"}, "sdk/go", true},
	} {
		got, err := directorySelected(tc.dir, tc.patterns)
		if err != nil || got != tc.want {
			t.Errorf("select %q with %q = %v, %v; want %v", tc.dir, tc.patterns, got, err, tc.want)
		}
	}
	for _, pattern := range []string{"[", "../outside", "!../outside"} {
		if _, err := directorySelected("pkg", []string{pattern}); err == nil {
			t.Errorf("accepted invalid pattern %q", pattern)
		}
	}
}

func TestGenerationDirectoryDiscovery(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"go.mod":              "module example.com/root\n",
		"generate.go":         "package root\n//go:generate echo root\n",
		"a/generate.go":       "package a\n//go:generate echo a\n",
		"a/another.go":        "package a\n//go:generate echo another\n",
		"z/generate.go":       "package z\n//go:generate\techo z\n",
		"include/generate.go": "package include\n//go:generate:include asset\n",
		"comment/generate.go": "package comment\n// go:generate echo no\n",
		"nested/go.mod":       "module example.com/nested\n",
		"nested/generate.go":  "package nested\n//go:generate echo nested\n",
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
	for _, tc := range []struct {
		module         string
		patterns, want []string
	}{
		{".", nil, []string{".", "a", "z"}},
		{".", []string{"."}, []string{"."}},
		{".", []string{"a"}, []string{"a"}},
		{".", []string{"!a"}, []string{".", "z"}},
		{"nested", []string{"**"}, []string{"nested"}},
	} {
		got, err := index.generateDirectoriesFor(tc.module, tc.patterns)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("module %s patterns %q: got %q, %v; want %q", tc.module, tc.patterns, got, err, tc.want)
		}
	}
	output := t.TempDir()
	if err := runAll([]string{"--all", "--generate", "--root", root, "--output-dir", output, "--generate-patterns", `["a"]`}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(output, "_root_.generatedirs"))
	if err != nil || string(got) != "a" {
		t.Fatalf("directory index: %q, %v", got, err)
	}
}
