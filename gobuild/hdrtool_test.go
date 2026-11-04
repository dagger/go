package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

// With a list of directories, gohdr unpack writes only the Go files there
// and below a listed directory that embeds, and every file that isn't Go.
func TestHdrToolUnpackPlanDirs(t *testing.T) {
	dir := buildHdrTool(t)
	files := map[string]string{
		"go.mod":              "module m\n",
		"main.go":             "package main\n\nimport \"m/lib\"\n",
		"lib/lib.go":          "package lib\n",
		"lib/lib.s":           "TEXT x(SB)\n",
		"other/other.go":      "package other\n",
		"other/data.txt":      "data\n",
		"tmpl/tmpl.go":        "package tmpl\n\nimport _ \"embed\"\n\n//go:embed skel\nvar skel string\n",
		"tmpl/skel/x.go":      "package skel\n",
		"tmpl/skel/sub/y.go":  "package sub\n",
		"unread/deep/deep.go": "package deep\n",
	}
	var pack bytes.Buffer
	pack.WriteString("gohdr 1\n")
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		fmt.Fprintf(&pack, "%s\n644\n%d\n%s\n", f, len(files[f]), files[f])
	}
	packPath := filepath.Join(dir, "src.hdr")
	if err := os.WriteFile(packPath, pack.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs := filepath.Join(dir, "dirs")
	if err := os.WriteFile(dirs, []byte(".\nlib\ntmpl"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "src")
	if b, err := exec.Command(filepath.Join(dir, "gohdr"), "unpack", packPath, out, os.DevNull, dirs).CombinedOutput(); err != nil {
		t.Fatalf("unpack: %v\n%s", err, b)
	}
	var got []string
	filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(out, p)
			got = append(got, rel)
		}
		return err
	})
	want := []string{"go.mod", "lib/lib.go", "lib/lib.s", "main.go", "other/data.txt", "tmpl/skel/sub/y.go", "tmpl/skel/x.go", "tmpl/tmpl.go"}
	if !slices.Equal(got, want) {
		t.Errorf("unpacked %q, want %q", got, want)
	}
}

// gohdr packmod packs a downloaded module as a plan reads it: Go files as
// their headers, C and assembly as their leading comments, the module's
// metadata whole, and any other file empty.
func TestHdrToolPackmod(t *testing.T) {
	dir := buildHdrTool(t)
	root := filepath.Join(dir, "mc")
	files := map[string]string{
		"cache/download/example.com/m/@v/v1.0.0.mod": "module example.com/m\n",
		"example.com/m@v1.0.0/go.mod":                "module example.com/m\n",
		"example.com/m@v1.0.0/m.go":                  "package m\n\nimport \"fmt\"\n\nfunc F() { fmt.Println() }\n",
		"example.com/m@v1.0.0/m_amd64.s":             "//go:build !purego\n\n#include \"textflag.h\"\nTEXT x(SB)\n",
		"example.com/m@v1.0.0/unused.c":              "/* Copyright notice\n   continued */\n//go:build ignore\n\nint unused;\n",
		"example.com/m@v1.0.0/api.json":              "{\"big\": true}\n",
		"example.com/m@v1.0.0/m_test.go":             "package m\n",
	}
	for name, data := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pack := filepath.Join(dir, "m.gohdr")
	if out, err := exec.Command(filepath.Join(dir, "gohdr"), "packmod", root, pack, "example.com/m@v1.0.0", "cache/download/example.com/m/@v/v1.0.0.mod").CombinedOutput(); err != nil {
		t.Fatalf("packmod: %v\n%s", err, out)
	}
	out := filepath.Join(dir, "out")
	if b, err := exec.Command(filepath.Join(dir, "gohdr"), "unpack", pack, out, os.DevNull).CombinedOutput(); err != nil {
		t.Fatalf("unpack: %v\n%s", err, b)
	}
	for name, want := range map[string]string{
		"cache/download/example.com/m/@v/v1.0.0.mod": "module example.com/m\n",
		"example.com/m@v1.0.0/go.mod":                "module example.com/m\n",
		"example.com/m@v1.0.0/m.go":                  "package m\n\nimport \"fmt\"\n",
		"example.com/m@v1.0.0/m_amd64.s":             "//go:build !purego\n\n",
		"example.com/m@v1.0.0/unused.c":              "/* Copyright notice\n   continued */\n//go:build ignore\n\n",
		"example.com/m@v1.0.0/api.json":              "",
	} {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "example.com/m@v1.0.0/m_test.go")); err == nil {
		t.Errorf("the test file was packed")
	}
}

// gohdr imports lists each go.mod's replacements of a module by a directory,
// by the directory's path in the pack, and no other replacement.
func TestHdrToolImportsReplace(t *testing.T) {
	dir := buildHdrTool(t)
	files := map[string]string{
		"app/go.mod": "module example.com/app\n\ngo 1.22\n\n" +
			"replace example.com/original => ../fork // a fork\n\n" +
			"replace (\n\texample.com/sub v1.0.0 => ./sub\n\texample.com/versioned => example.com/other v1.2.3\n)\n",
		"app/main.go": "package main\n",
	}
	var pack bytes.Buffer
	pack.WriteString("gohdr 1\n")
	for _, f := range []string{"app/go.mod", "app/main.go"} {
		fmt.Fprintf(&pack, "%s\n644\n%d\n%s\n", f, len(files[f]), files[f])
	}
	packPath := filepath.Join(dir, "src.hdr")
	if err := os.WriteFile(packPath, pack.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(dir, "gohdr"), "imports", packPath, "build").Output()
	if err != nil {
		t.Fatalf("imports: %v", err)
	}
	for _, want := range []string{"mod app example.com/app\n", "repl fork example.com/original\n", "repl app/sub example.com/sub\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("imports output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "versioned") {
		t.Errorf("imports output lists a module replacement:\n%s", out)
	}
}

func TestLiteralPatterns(t *testing.T) {
	got := literalPatterns([]string{"a.go/x.go", "b.go/[x]*?^.go", "!c.go/d.go", `e.go/f\g.go`})
	want := []string{"a.go/x.go", `b.go/\[x\]\*\?\^.go`, `\!c.go/d.go`, `e.go/f\\g.go`}
	if !slices.Equal(got, want) {
		t.Errorf("literalPatterns: %q", got)
	}
}

// gohdr manifest describes the archives of a "go list -deps -test" listing's
// packages, test variants included, as the export driver reads them, by
// their IDs: a package's digest covers its Go, other and embedded files by
// name and content, and a cgo package's covers its cgo files and cgo's
// output, by its place in the compile, without its //line directives, whose
// paths and the cgo files it names too.
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
		"p/a_test.go":  "package p\n\nfunc f() {}\n",
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
		{"ImportPath": "example.com/p", "Module": map[string]any{"Path": "example.com/p", "Version": "v1.0.0", "Replace": map[string]any{"Path": "example.com/fork", "Version": "v1.0.1"}}, "Dir": src + "/p", "GoFiles": []string{"a.go"}, "CgoFiles": []string{"c.go"}, "CFiles": []string{"x.c"}, "EmbedFiles": []string{"data/e.txt"}},
		{"ImportPath": "example.com/p [example.com/p.test]", "ForTest": "example.com/p", "Dir": src + "/p", "GoFiles": []string{"a.go", "a_test.go"}},
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
	envPath := write("env.json", `{"GOVERSION": "go1.26.1", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1", "CGO_CFLAGS": "-O2", "GOMODCACHE": "`+src+`"}`)
	entriesPath := write("entries.txt", "example.com/p\tarchives/p.a\t"+filepath.Join(dir, "gen/_cgo_gotypes.go")+"\t"+filepath.Join(dir, "gen/c.cgo1.go")+"\n"+
		"example.com/p [example.com/p.test]\tarchives/pt.a\nexample.com/q\tarchives/q.a\n")
	gohdr := filepath.Join(dir, "tool/gohdr")
	out, err := exec.Command(gohdr, "manifest", listPath, envPath, entriesPath, "b,a").Output()
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
		"version": 2.0, "goVersion": "go1.26.1", "goos": "linux", "goarch": "amd64", "cgoEnabled": true,
		"buildTags": []any{"a", "b"},
		"cgo":       map[string]any{"CGO_CFLAGS": "-O2"},
		"packages": map[string]any{
			"example.com/p": map[string]any{"archive": "archives/p.a", "sources": digest(
				"cgo\n",
				line("cgo-output-0", "\npackage p\n"),
				line("cgo-output-1", "package p\n\nvar _ = 1 // line, not a directive\n"),
				line("a.go", files["p/a.go"]),
				line("c.go", files["p/c.go"]),
				line("x.c", files["p/x.c"]),
				line("data/e.txt", files["p/data/e.txt"]),
			),
				"cgoFiles":  []any{"c.go"},
				"module":    "example.com/fork@v1.0.1",
				"cgoOutput": []any{filepath.Join(dir, "gen/_cgo_gotypes.go"), filepath.Join(dir, "gen/c.cgo1.go")},
			},
			"example.com/p [example.com/p.test]": map[string]any{"archive": "archives/pt.a", "sources": digest(
				line("a.go", files["p/a.go"]),
				line("a_test.go", files["p/a_test.go"]),
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

// gohdr sizes lists each Go file's size, by its path under the directory.
func TestHdrToolSizes(t *testing.T) {
	gohdr := filepath.Join(buildHdrTool(t), "gohdr")
	dir := t.TempDir()
	for name, size := range map[string]int{"a.go": 10, "p/b.go": 2000, "p/c.txt": 5} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(gohdr, "sizes", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("sizes: %v: %s", err, out)
	}
	if want := "10 a.go\n2000 p/b.go\n"; string(out) != want {
		t.Errorf("sizes = %q, want %q", out, want)
	}
}
