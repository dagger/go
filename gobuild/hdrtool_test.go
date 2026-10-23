package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// gohdr's source is a string the helper builds in a container, so the
// helper's own build doesn't compile it: build it here.
func TestHdrToolBuilds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module gohdr\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(hdrToolSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "vet", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gohdr doesn't build: %v\n%s", err, out)
	}
}

// gohdr modunzip extracts a module zip as the go command does, after
// checking it against the go command's checksum of it, and refuses one that
// doesn't match.
func TestHdrToolModUnzip(t *testing.T) {
	dir := buildHdrTool(t)
	files := map[string]string{
		"example.com/M@v1.0.0/notes..txt":  "a name with dots\n",
		"example.com/M@v1.0.0/go.mod":      "module example.com/M\n",
		"example.com/M@v1.0.0/sub/value.h": "#define V 1\n",
	}
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	var names []string
	// A directory entry, as a zip made by hand may hold, counts too.
	files["example.com/M@v1.0.0/sub/"] = ""
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(data))
		names = append(names, name)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(dir, "m.zip")
	if err := os.WriteFile(zipPath, zipped.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// The go command's "h1:" hash (golang.org/x/mod/sumdb/dirhash.Hash1).
	sort.Strings(names)
	summary := sha256.New()
	for _, name := range names {
		fmt.Fprintf(summary, "%x  %s\n", sha256.Sum256([]byte(files[name])), name)
	}
	sum := "h1:" + base64.StdEncoding.EncodeToString(summary.Sum(nil))
	run := func(sum, out string) ([]byte, error) {
		return exec.Command(filepath.Join(dir, "gohdr"), "modunzip", zipPath, "example.com/M@v1.0.0", out, sum).CombinedOutput()
	}
	if out, err := run("h1:wrong", filepath.Join(dir, "bad")); err == nil {
		t.Errorf("a zip that doesn't match its checksum was extracted:\n%s", out)
	}
	mod := filepath.Join(dir, "mod")
	if out, err := run(sum, mod); err != nil {
		t.Fatalf("modunzip: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		filepath.WalkDir(mod, func(p string, d fs.DirEntry, err error) error { os.Chmod(p, 0o755); return nil })
	})
	if data, err := os.ReadFile(filepath.Join(mod, "sub/value.h")); err != nil || string(data) != files["example.com/M@v1.0.0/sub/value.h"] {
		t.Errorf("sub/value.h: %q, %v", data, err)
	}
	if fi, err := os.Stat(filepath.Join(mod, "sub")); err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("sub is writable: %v, %v", fi.Mode(), err)
	}
}

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
