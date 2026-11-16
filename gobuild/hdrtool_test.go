package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// buildHdrTool builds gohdr, from its source in the go module's library,
// into a new directory, which it returns.
func buildHdrTool(t *testing.T) string {
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "gohdr"), ".")
	build.Dir = filepath.Join("..", "gomod", "helpers", "gohdr")
	build.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("gohdr doesn't build: %v\n%s", err, out)
	}
	return dir
}

// gohdr imports marks the directories whose build may read more than their
// Go files: a //go:embed line, an import of "C", a symlink. A test file's
// mark is listed only with tests.
func TestHdrToolImportsMarks(t *testing.T) {
	dir := buildHdrTool(t)
	type file struct{ rel, mode, data string }
	files := []file{
		{"go.mod", "644", "module example.com/m\n\ngo 1.22\n"},
		{"embeds/e.go", "644", "package embeds\n\nimport \"embed\"\n\n//go:embed static\nvar _ embed.FS\n"},
		{"cgo/c.go", "644", "package cgo\n\n// #include \"../x.h\"\nimport \"C\"\n"},
		{"links/l.go", strconv.FormatUint(uint64(os.ModeSymlink|0o777), 8), "../plain/p.go"},
		{"plain/p.go", "644", "package plain\n\nimport \"fmt\"\n"},
		{"tested/t_test.go", "644", "package tested\n\nimport \"embed\"\n\n//go:embed testdata\nvar _ embed.FS\n"},
	}
	var pack bytes.Buffer
	pack.WriteString("gohdr 1\n")
	for _, f := range files {
		fmt.Fprintf(&pack, "%s\n%s\n%d\n%s\n", f.rel, f.mode, len(f.data), f.data)
	}
	packPath := filepath.Join(dir, "src.hdr")
	if err := os.WriteFile(packPath, pack.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[string]map[string][]string{
		"build": {"embeds": {"embed"}, "cgo": {"cgo"}, "links": {"link", "unparsed"}},
		"tests": {"embeds": {"embed"}, "cgo": {"cgo"}, "links": {"link", "unparsed"}, "tested": {"embed"}},
	} {
		out, err := exec.Command(filepath.Join(dir, "gohdr"), "imports", packPath, mode).Output()
		if err != nil {
			t.Fatalf("imports %s: %v", mode, err)
		}
		src := parseSourceImports(newTarget("linux\namd64\n1\ngo1.27.3\n", nil), string(out))
		if !reflect.DeepEqual(src.marks, want) {
			t.Errorf("imports %s: marks %v, want %v\n%s", mode, src.marks, want, out)
		}
		if want := map[string][]string{"embeds": {"embeds"}, "cgo": {"cgo"}, "plain": {"plain"}}; mode == "build" && !reflect.DeepEqual(src.names, want) {
			t.Errorf("imports %s: package names %v, want %v", mode, src.names, want)
		}
		// The marks are neither Go files' lines nor modules.
		if _, ok := src.all["embed embeds"]; ok {
			t.Errorf("imports %s: a mark read as a package", mode)
		}
		if len(src.mods) != 1 || src.mods[0] != (sourceModule{dir: ".", path: "example.com/m"}) {
			t.Errorf("imports %s: modules %+v", mode, src.mods)
		}
	}
}
