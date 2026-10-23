package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
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

// gohdr scope lists the plain packages of a "go list -deps -test" listing
// that compile, and gohdr manifest describes their archives as the export
// driver reads them: a package's digest covers its Go, other and embedded
// files by name and content, and a cgo package's covers cgo's output, by its
// place in the compile, without its //line directives, instead of its cgo
// files.
func TestHdrToolManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("tool/go.mod", "module gohdr\n\ngo 1.26\n")
	write("tool/main.go", hdrToolSrc)
	build := exec.Command("go", "build", "-o", "gohdr", ".")
	build.Dir = filepath.Join(dir, "tool")
	build.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("gohdr doesn't build: %v\n%s", err, out)
	}
	files := map[string]string{
		"p/a.go":       "package p\n",
		"p/c.go":       "package p\n\nimport \"C\"\n",
		"p/x.c":        "int x;\n",
		"p/data/e.txt": "embedded\n",
		"q/q.go":       "package q\n",
	}
	for name, data := range files {
		write("src/"+name, data)
	}
	gen := map[string]string{
		"_cgo_gotypes.go": "//line /somewhere/c.go:1:1\npackage p\n",
		"c.cgo1.go":       "package p\n//line /somewhere/c.go:3:1\nvar _ = 1 // line, not a directive\n",
	}
	for name, data := range gen {
		write("gen/"+name, data)
	}
	src := filepath.Join(dir, "src")
	var list bytes.Buffer
	enc := json.NewEncoder(&list)
	for _, p := range []map[string]any{
		{"ImportPath": "example.com/p", "Dir": src + "/p", "GoFiles": []string{"a.go"}, "CgoFiles": []string{"c.go"}, "CFiles": []string{"x.c"}, "EmbedFiles": []string{"data/e.txt"}},
		{"ImportPath": "example.com/p [example.com/p.test]", "ForTest": "example.com/p", "Dir": src + "/p", "GoFiles": []string{"a.go"}},
		{"ImportPath": "example.com/p_test [example.com/p.test]", "ForTest": "example.com/p", "Dir": src + "/p", "GoFiles": []string{"x_test.go"}},
		{"ImportPath": "example.com/p.test", "Dir": src + "/p", "GoFiles": []string{"_testmain.go"}},
		{"ImportPath": "unsafe", "Dir": "/goroot/src/unsafe", "GoFiles": []string{"unsafe.go"}},
		{"ImportPath": "example.com/testsonly", "Dir": src + "/t"},
		{"ImportPath": "example.com/q", "Dir": src + "/q", "GoFiles": []string{"q.go"}},
	} {
		if err := enc.Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	listPath := write("list.json", list.String())
	envPath := write("env.json", `{"GOVERSION": "go1.26.1", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1"}`)
	entriesPath := write("entries.txt", "example.com/p archives/p.a "+filepath.Join(dir, "gen/_cgo_gotypes.go")+" "+filepath.Join(dir, "gen/c.cgo1.go")+"\nexample.com/q archives/q.a\n")
	gohdr := filepath.Join(dir, "tool/gohdr")
	out, err := exec.Command(gohdr, "scope", listPath).CombinedOutput()
	if err != nil || string(out) != "example.com/p\nexample.com/q\n" {
		t.Errorf("scope: %q, %v", out, err)
	}
	out, err = exec.Command(gohdr, "manifest", listPath, envPath, entriesPath, "b,a").Output()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("manifest: %v\n%s", err, out)
	}
	digest := func(lines ...string) string {
		sort.Strings(lines)
		return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(strings.Join(lines, ""))))
	}
	line := func(name, data string) string { return fmt.Sprintf("%s %x\n", name, sha256.Sum256([]byte(data))) }
	want := map[string]any{
		"version": 1.0, "goVersion": "go1.26.1", "goos": "linux", "goarch": "amd64", "cgoEnabled": true,
		"buildTags": []any{"a", "b"},
		"packages": map[string]any{
			"example.com/p": map[string]any{"archive": "archives/p.a", "sources": digest(
				"cgo\n",
				line("cgo-output-0", "\npackage p\n"),
				line("cgo-output-1", "package p\n\nvar _ = 1 // line, not a directive\n"),
				line("a.go", files["p/a.go"]),
				line("x.c", files["p/x.c"]),
				line("data/e.txt", files["p/data/e.txt"]),
			)},
			"example.com/q": map[string]any{"archive": "archives/q.a", "sources": digest(line("q.go", files["q/q.go"]))},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifest:\n%s\nwant:\n%v", out, want)
	}
}

// A dependency's weight is its own: a file of the project named like its
// import path doesn't change it.
func TestHdrToolWeighOwnSource(t *testing.T) {
	gohdr := filepath.Join(buildHdrTool(t), "gohdr")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "plan.txt")
	// As go build -n prints it: each package's step after a header naming
	// the package, here the dependency's after its importer's.
	plan := "#\n# example.com/top\n#\n\nmkdir -p $WORK/b001/\ncd " + dir + "\n/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p example.com/top -pack ./a.go\n" +
		"#\n# example.com/dep.go\n#\n\nmkdir -p $WORK/b002/\n/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b002/_pkg_.a -trimpath \"$WORK/b002=>\" -p example.com/dep.go -lang=go1.26 -complete -pack ./a.go\n"
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	weigh := func() string {
		cmd := exec.Command(gohdr, "weigh", planPath)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("weigh: %v: %s", err, out)
		}
		return string(out)
	}
	without := weigh()
	if err := os.MkdirAll(filepath.Join(dir, "example.com"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "example.com/dep.go"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	with := weigh()
	want := fmt.Sprintf("b001 %d\nb002 %d\n", 8<<10+100, 8<<10+100)
	if without != want || with != want {
		t.Errorf("weights %q and %q, want %q for both", without, with, want)
	}
}
