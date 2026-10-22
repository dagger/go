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
