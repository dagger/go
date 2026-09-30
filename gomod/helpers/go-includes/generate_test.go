package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGenerationDirectoryDiscovery(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"ignored/_generate.go":  "package ignored\n//go:generate echo no\n",
		"hidden/.generate.go":   "package hidden\n//go:generate echo no\n",
		"go.mod":                "module example.com/root\n",
		"generate.go":           "package root\n//go:generate echo root\n",
		"a/generate.go":         "package a\n//go:generate echo a\n",
		"a/another.go":          "package a\n//go:generate echo another\n",
		"z/generate.go":         "package z\n//go:generate\techo z\n",
		"include/generate.go":   "package include\n//go:generate:include asset\n",
		"container/generate.go": "package container\n//go:generate:container generate-env\n",
		"comment/generate.go":   "package comment\n// go:generate echo no\n",
		"nested/go.mod":         "module example.com/nested\n",
		"nested/generate.go":    "package nested\n//go:generate echo nested\n",
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

func TestGenerationContainers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  map[string]string
		err   string
	}{
		{
			name: "directory scope and unchanged values",
			files: map[string]string{
				"generate.go":        "//go:generate:container generate-env\n",
				"another.go":         "//go:generate:container generate-env\n",
				"child/generate.go":  "//go:generate go version\n",
				"image/generate.go":  "//go:generate:container docker.io/library/golang:1.26.1-alpine\n",
				"wired/generate.go":  "//go:generate:container dag://tools/generate-env\n",
				"quoted/generate.go": "//go:generate:container \"dag://tools/generate-env\"\n",
				"nested/go.mod":      "module example.com/nested\n",
				"nested/generate.go": "//go:generate:container nested-env\n",
			},
			want: map[string]string{".": "generate-env", "image": "docker.io/library/golang:1.26.1-alpine", "wired": "dag://tools/generate-env", "quoted": "dag://tools/generate-env"},
		},
		{name: "default", files: map[string]string{"generate.go": "//go:generate go version\n"}, want: map[string]string{}},
		{name: "same file conflict", files: map[string]string{"generate.go": "//go:generate:container one\n//go:generate:container two\n"}, err: "conflicting //go:generate:container values in directory .: \"one\""},
		{name: "cross file conflict", files: map[string]string{"a.go": "//go:generate:container one\n", "b.go": "//go:generate:container two\n"}, err: "conflicting //go:generate:container values in directory .: \"one\""},
		{name: "missing", files: map[string]string{"generate.go": "//go:generate:container\n"}, err: "requires one non-empty value"},
		{name: "empty", files: map[string]string{"generate.go": "//go:generate:container \"\"\n"}, err: "requires one non-empty value"},
		{name: "multiple", files: map[string]string{"generate.go": "//go:generate:container one two\n"}, err: "requires one non-empty value"},
		{name: "invalid quote", files: map[string]string{"generate.go": "//go:generate:container \"one\n"}, err: "invalid quoted string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/root\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for name, contents := range tc.files {
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(name, ".go") {
					contents = "package fixture\n" + contents
				}
				if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			index, err := indexLocal(root)
			if err != nil {
				t.Fatal(err)
			}
			got, err := index.generateContainersFor(".")
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("got %v, want error containing %q", err, tc.err)
				}
				if !strings.Contains(err.Error(), ".go:") {
					t.Fatalf("error has no source position: %v", err)
				}
				// A bad directive is one module's problem: the scan records
				// it against that module and carries on, so every other
				// module in the workspace is still scanned.
				output := t.TempDir()
				if err := runAll([]string{"--all", "--generate", "--root", root, "--output-dir", output}); err != nil {
					t.Fatalf("one module's bad directive failed the whole scan: %v", err)
				}
				recorded, err := os.ReadFile(filepath.Join(output, "_root_.err"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(recorded), tc.err) {
					t.Fatalf("recorded %q, want it to contain %q", recorded, tc.err)
				}
				if !strings.Contains(string(recorded), ".go:") {
					t.Fatalf("recorded reason has no source position: %q", recorded)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestTestContainers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  map[string]string
		err   string
	}{
		{
			name: "directory scope and unchanged values",
			files: map[string]string{
				"main_test.go":          "//go:test:container test-env\n",
				"another.go":            "//go:test:container test-env\n",
				"child/child_test.go":   "",
				"image/image_test.go":   "//go:test:container docker.io/library/golang:1.26.1-alpine\n",
				"wired/wired_test.go":   "//go:test:container dag://tools/test-env\n",
				"quoted/quoted_test.go": "//go:test:container \"dag://tools/test-env\"\n",
				"nested/go.mod":         "module example.com/nested\n",
				"nested/nested_test.go": "//go:test:container nested-env\n",
			},
			want: map[string]string{
				".":      "test-env",
				"image":  "docker.io/library/golang:1.26.1-alpine",
				"wired":  "dag://tools/test-env",
				"quoted": "dag://tools/test-env",
			},
		},
		{name: "same file conflict", files: map[string]string{"main_test.go": "//go:test:container one\n//go:test:container two\n"}, err: "conflicting //go:test:container values in directory ."},
		{name: "cross file conflict", files: map[string]string{"a_test.go": "//go:test:container one\n", "b_test.go": "//go:test:container two\n"}, err: "conflicting //go:test:container values in directory ."},
		{name: "missing", files: map[string]string{"main_test.go": "//go:test:container\n"}, err: "//go:test:container requires one non-empty value"},
		{name: "empty", files: map[string]string{"main_test.go": "//go:test:container \"\"\n"}, err: "//go:test:container requires one non-empty value"},
		{name: "multiple", files: map[string]string{"main_test.go": "//go:test:container one two\n"}, err: "//go:test:container requires one non-empty value"},
		{name: "invalid quote", files: map[string]string{"main_test.go": "//go:test:container \"one\n"}, err: "invalid quoted string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/root\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for name, contents := range tc.files {
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(name, ".go") {
					contents = "package fixture\n" + contents
				}
				if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			index, err := indexLocal(root)
			if err != nil {
				t.Fatal(err)
			}
			got, err := index.testContainersFor(".")
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("got %v; want error containing %q", err, tc.err)
				}
				output := t.TempDir()
				if err := runAll([]string{"--all", "--test", "--root", root, "--output-dir", output}); err != nil {
					t.Fatalf("one module's bad directive failed the whole scan: %v", err)
				}
				recorded, err := os.ReadFile(filepath.Join(output, "_root_.err"))
				if err != nil || !strings.Contains(string(recorded), tc.err) {
					t.Fatalf("recorded %q, %v; want error containing %q", recorded, err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tc.want)
			}
			output := t.TempDir()
			if err := runAll([]string{"--all", "--test", "--root", root, "--output-dir", output}); err != nil {
				t.Fatal(err)
			}
			for dir, want := range tc.want {
				name := dir
				if name == "." {
					name = "_root_"
				}
				data, err := os.ReadFile(filepath.Join(output, name+".testcontainer"))
				if err != nil || string(data) != want {
					t.Errorf("%s: got %q, %v; want %q", dir, data, err, want)
				}
			}
		})
	}
}
