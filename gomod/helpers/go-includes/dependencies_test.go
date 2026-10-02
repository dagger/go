package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestServiceDependencies(t *testing.T) {
	for _, operation := range []string{"generate", "test"} {
		t.Run(operation, func(t *testing.T) {
			for _, tc := range []struct{ name, directives, err string }{
				{"multiple and duplicate", ":dependency backend dag://tools/backend\n//go:" + operation + ":dependency db \"dag+service://tools/db\"\n//go:" + operation + ":dependency db dag+service://tools/db", ""},
				{"missing link", ":dependency db", "requires a binding name and a dag service link"},
				{"extra argument", ":dependency db dag://tools/db extra", "requires a binding name and a dag service link"},
				{"empty name", ":dependency \"\" dag://tools/db", "requires a binding name and a dag service link"},
				{"invalid name", ":dependency \"db name\" dag://tools/db", "requires a binding name and a dag service link"},
				{"image link", ":dependency db docker.io/postgres:17", "requires a binding name and a dag service link"},
				{"bad quote", ":dependency db \"dag://tools/db", "invalid quoted string"},
				{"cross file conflict", ":dependency db dag://tools/db", "conflicting service dependency"},
				{"conflict", ":dependency db dag://tools/db\n//go:" + operation + ":dependency db dag://tools/other", "conflicting service dependency"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					root := t.TempDir()
					files := map[string]string{
						"go.mod":              "module example.com/root\n",
						"main_test.go":        "package fixture\n//go:generate echo root\n//go:" + operation + tc.directives + "\n",
						"child/child_test.go": "package child\n//go:generate echo child\n",
						"nested/go.mod":       "module example.com/nested\n",
						"nested/main_test.go": "package nested\n//go:" + operation + ":dependency isolated dag://tools/nested\n",
					}
					if tc.name == "cross file conflict" {
						files["another.go"] = "package fixture\n//go:" + operation + ":dependency db dag://tools/other\n"
					}
					// Check that equal declarations in different files are accepted.
					if tc.err == "" {
						files["another.go"] = "package fixture\n//go:" + operation + ":dependency db dag+service://tools/db\n"
					}
					for name, data := range files {
						target := filepath.Join(root, name)
						if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(target, []byte(data), 0644); err != nil {
							t.Fatal(err)
						}
					}
					index, err := indexLocal(root)
					if err != nil {
						t.Fatal(err)
					}
					got, err := index.dependenciesFor(".", operation)
					output := t.TempDir()
					if scanErr := runAll([]string{"--all", "--" + operation, "--root", root, "--output-dir", output}); scanErr != nil {
						t.Fatal(scanErr)
					}
					if tc.err != "" {
						if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), ".go:") {
							t.Fatalf("got %v, want positioned error containing %q", err, tc.err)
						}
						recorded, readErr := os.ReadFile(filepath.Join(output, "_root_.err"))
						if readErr != nil || !strings.Contains(string(recorded), tc.err) {
							t.Fatalf("recorded %q, %v", recorded, readErr)
						}
						return
					}
					want := map[string][]string{".": {"backend\tdag://tools/backend", "db\tdag+service://tools/db"}}
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("got %v, %v; want %v", got, err, want)
					}
					for dir, expected := range map[string]string{"_root_": strings.Join(want["."], "\n") + "\n", "child": ""} {
						data, err := os.ReadFile(filepath.Join(output, dir+"."+operation+"dependencies"))
						if err != nil || string(data) != expected {
							t.Fatalf("%s: got %q, %v; want %q", dir, data, err, expected)
						}
					}
				})
			}
		})
	}
}
