package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
