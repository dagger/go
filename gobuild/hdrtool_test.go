package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// buildHdrTool builds gohdr into a new directory, which it returns.
func buildHdrTool(t *testing.T) string {
	dir := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module gohdr\n\ngo 1.26\n", "main.go": hdrToolSrc} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-o", "gohdr", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("gohdr doesn't build: %v\n%s", err, out)
	}
	return dir
}

// gohdr unpack lists the pack's Go files in directories named like Go files,
// at any depth, and nothing else.
func TestHdrToolUnpackList(t *testing.T) {
	dir := buildHdrTool(t)
	var pack bytes.Buffer
	pack.WriteString("gohdr 1\n")
	for _, f := range []string{"go.mod", "hdr.go/extra.h", "hdr.go/go.mod", "hdr.go/sub.go/deep.go", "hdr.go/sub.go/more.h", "hdr.go/unused.go", "lib.go/a b.go", "lib/x.go", "main.go"} {
		fmt.Fprintf(&pack, "%s\n644\n%d\n%s\n", f, len(f), f)
	}
	packPath := filepath.Join(dir, "src.hdr")
	if err := os.WriteFile(packPath, pack.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "list.txt")
	if out, err := exec.Command(filepath.Join(dir, "gohdr"), "unpack", packPath, filepath.Join(dir, "src"), list).CombinedOutput(); err != nil {
		t.Fatalf("unpack: %v\n%s", err, out)
	}
	data, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "hdr.go/sub.go/deep.go\nhdr.go/unused.go\nlib.go/a b.go\n"; got != want {
		t.Errorf("listed:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "src/hdr.go/extra.h")); err != nil {
		t.Errorf("the pack wasn't unpacked: %v", err)
	}
}

func TestLiteralPatterns(t *testing.T) {
	got := literalPatterns([]string{"a.go/x.go", "b.go/[x]*?^.go", "!c.go/d.go", `e.go/f\g.go`})
	want := []string{"a.go/x.go", `b.go/\[x\]\*\?\^.go`, `\!c.go/d.go`, `e.go/f\\g.go`}
	if !slices.Equal(got, want) {
		t.Errorf("literalPatterns: %q", got)
	}
}
