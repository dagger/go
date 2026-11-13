package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
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

// gohdr packmod packs a downloaded module from its zip as a plan reads it,
// as the go command would extract it: Go files as their headers, C and
// assembly as their leading comments, the module's metadata whole, any other
// file empty, and no test file; a testdata directory's files as any other,
// since a build may import a package there by its path.
// gohdr subset writes, as a pack, exactly what gohdr unpack writes with the
// plan's directories, and the whole pack's list of Go files in directories
// named like Go files. A plan mounts it in place of the whole pack, so it
// must change when a file the plan reads changes, and only then.
func TestHdrToolSubset(t *testing.T) {
	dir := buildHdrTool(t)
	gohdr := filepath.Join(dir, "gohdr")
	type entry struct{ mode, data string }
	base := map[string]entry{
		"go.mod":              {"644", "module m\n"},
		"main.go":             {"644", "package main\n\nimport \"m/lib\"\n"},
		"lib/lib.go":          {"755", "package lib\n"},
		"lib/lib.s":           {"644", "TEXT x(SB)\n"},
		"lib/link.go":         {strconv.FormatUint(uint64(os.ModeSymlink|0o777), 8), "lib.go"},
		"other/other.go":      {"644", "package other\n"},
		"other/data.txt":      {"644", "data\n"},
		"tmpl/tmpl.go":        {"644", "package tmpl\n\nimport _ \"embed\"\n\n//go:embed skel\nvar skel string\n"},
		"tmpl/skel/x.go":      {"644", "package skel\n"},
		"tmpl/skel/shared.go": {"644", "package tmpl\n"},
		"tmpl/shared.go":      {strconv.FormatUint(uint64(os.ModeSymlink|0o777), 8), "skel/shared.go"},
		"gen.go/inner.go":     {"644", "package gen\n"},
		"unread/deep/deep.go": {"644", "package deep\n"},
	}
	writePack := func(name string, files map[string]entry) string {
		var pack bytes.Buffer
		pack.WriteString("gohdr 1\n")
		names := make([]string, 0, len(files))
		for f := range files {
			names = append(names, f)
		}
		sort.Strings(names)
		for _, f := range names {
			fmt.Fprintf(&pack, "%s\n%s\n%d\n%s\n", f, files[f].mode, len(files[f].data), files[f].data)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pack.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(gohdr, args...).CombinedOutput(); err != nil {
			t.Fatalf("gohdr %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// tree is every file and symlink under root: its mode and its contents
	// or target.
	tree := func(root string) map[string]string {
		t.Helper()
		got := map[string]string{}
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			content := ""
			if info.Mode()&os.ModeSymlink != 0 {
				content, err = os.Readlink(p)
			} else {
				var data []byte
				data, err = os.ReadFile(p)
				content = string(data)
			}
			got[rel] = info.Mode().String() + " " + content
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	dirs := filepath.Join(dir, "dirs")
	if err := os.WriteFile(dirs, []byte(".\nlib\ntmpl"), 0o644); err != nil {
		t.Fatal(err)
	}
	pack := writePack("src.hdr", base)

	// What a plan unpacks from the subset is what it unpacked from the
	// whole pack with its directories, and the list is the whole pack's.
	run("unpack", pack, filepath.Join(dir, "direct"), filepath.Join(dir, "direct.txt"), dirs)
	run("subset", pack, filepath.Join(dir, "plan.hdr"), filepath.Join(dir, "subset.txt"), dirs)
	run("unpack", filepath.Join(dir, "plan.hdr"), filepath.Join(dir, "staged"), os.DevNull)
	if got, want := tree(filepath.Join(dir, "staged")), tree(filepath.Join(dir, "direct")); !reflect.DeepEqual(got, want) {
		t.Errorf("unpacked from the subset:\n%v\nwant:\n%v", got, want)
	}
	if got := read(filepath.Join(dir, "staged/tmpl/skel/x.go")); got != "" {
		t.Errorf("an embedded Go file the plan doesn't read was unpacked with %q, want it empty", got)
	}
	if got, want := read(filepath.Join(dir, "staged/tmpl/shared.go")), base["tmpl/skel/shared.go"].data; got != want {
		t.Errorf("a Go file read through a symlink was unpacked with %q, want %q", got, want)
	}
	if got, want := read(filepath.Join(dir, "subset.txt")), read(filepath.Join(dir, "direct.txt")); got != want || want != "gen.go/inner.go\n" {
		t.Errorf("subset listed %q, unpack %q, want %q", got, want, "gen.go/inner.go\n")
	}

	// Without tests, a test file of a directory the plan reads is left out,
	// but where an embed may name it, and a change to it leaves the subset
	// as it was.
	tested := maps.Clone(base)
	tested["lib/lib_test.go"] = entry{"644", "package lib\n\nimport \"testing\"\n"}
	tested["tmpl/tmpl_test.go"] = entry{"644", "package tmpl\n"}
	run("subset", writePack("tested.hdr", tested), filepath.Join(dir, "notests.hdr"), os.DevNull, dirs, "notests")
	run("unpack", filepath.Join(dir, "notests.hdr"), filepath.Join(dir, "notests"), os.DevNull)
	got := tree(filepath.Join(dir, "notests"))
	if _, ok := got["lib/lib_test.go"]; ok {
		t.Errorf("without tests, the subset kept lib/lib_test.go")
	}
	if _, ok := got["tmpl/tmpl_test.go"]; !ok {
		t.Errorf("without tests, the subset left out tmpl/tmpl_test.go, which tmpl's embed may name")
	}
	withoutTests := read(filepath.Join(dir, "notests.hdr"))
	// A test file's own //go:embed is no build's: it doesn't keep the test
	// files of its directory.
	tested["lib/embed_test.go"] = entry{"644", "package lib\n\nimport _ \"embed\"\n\n//go:embed lib.s\nvar _ string\n"}
	run("subset", writePack("tested.hdr", tested), filepath.Join(dir, "notests-embed.hdr"), os.DevNull, dirs, "notests")
	if read(filepath.Join(dir, "notests-embed.hdr")) != withoutTests {
		t.Errorf("without tests, a test file's //go:embed kept its directory's test files")
	}
	delete(tested, "lib/embed_test.go")
	tested["lib/lib_test.go"] = entry{"644", "package lib\n\nimport \"os\"\n"}
	run("subset", writePack("tested.hdr", tested), filepath.Join(dir, "notests.hdr"), os.DevNull, dirs, "notests")
	if read(filepath.Join(dir, "notests.hdr")) != withoutTests {
		t.Errorf("without tests, a change to a test file changed the subset")
	}

	// Without directories, the subset is the whole pack.
	run("subset", pack, filepath.Join(dir, "all.hdr"), os.DevNull)
	if got, want := read(filepath.Join(dir, "all.hdr")), read(pack); got != want {
		t.Errorf("subset without directories:\n%s\nwant the pack:\n%s", got, want)
	}

	// With an absolute symlink in the pack, whose target depends on where
	// the source is mounted, every embedded Go file stays whole.
	abs := maps.Clone(base)
	abs["lib/abs.go"] = entry{strconv.FormatUint(uint64(os.ModeSymlink|0o777), 8), "/ws/tmpl/skel/x.go"}
	run("subset", writePack("abs.hdr", abs), filepath.Join(dir, "abs-plan.hdr"), os.DevNull, dirs)
	run("unpack", filepath.Join(dir, "abs-plan.hdr"), filepath.Join(dir, "abs"), os.DevNull)
	if got, want := read(filepath.Join(dir, "abs/tmpl/skel/x.go")), base["tmpl/skel/x.go"].data; got != want {
		t.Errorf("with an absolute symlink, an embedded Go file was unpacked with %q, want %q", got, want)
	}

	// The subset changes with a header the plan reads, an embedded file
	// included, and not with one it doesn't.
	planned := read(filepath.Join(dir, "plan.hdr"))
	for _, c := range []struct {
		file    string
		changes bool
	}{
		{"unread/deep/deep.go", false},
		{"other/other.go", false},
		{"lib/lib.go", true},
		// An embedded Go file is read by its name alone.
		{"tmpl/skel/x.go", false},
		{"tmpl/skel/new.go", true},
		// One a package the plan reads links to is read whole.
		{"tmpl/skel/shared.go", true},
		{"other/data.txt", true},
		{"go.mod", true},
	} {
		files := maps.Clone(base)
		e, ok := files[c.file]
		if !ok {
			e.mode = "644"
		}
		e.data += "import _ \"fmt\"\n"
		files[c.file] = e
		changed := writePack("changed.hdr", files)
		run("subset", changed, filepath.Join(dir, "changed-plan.hdr"), os.DevNull, dirs)
		if got := read(filepath.Join(dir, "changed-plan.hdr")) != planned; got != c.changes {
			t.Errorf("a change to %s changed the subset: %v, want %v", c.file, got, c.changes)
		}
	}
}

func TestHdrToolPackmod(t *testing.T) {
	dir := buildHdrTool(t)
	root := filepath.Join(dir, "mc")
	meta := "cache/download/example.com/!m/@v/v1.0.0.mod"
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(meta)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, meta), []byte("module example.com/M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The zip names the module path as it is; the module cache escapes it.
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for _, f := range []struct{ name, data string }{
		{"example.com/M@v1.0.0/go.mod", "module example.com/M\n"},
		{"example.com/M@v1.0.0/m.go", "package m\n\nimport \"fmt\"\n\nfunc F() { fmt.Println() }\n"},
		{"example.com/M@v1.0.0/m_amd64.s", "//go:build !purego\n\n#include \"textflag.h\"\nTEXT x(SB)\n"},
		{"example.com/M@v1.0.0/unused.c", "/* Copyright notice\n   continued */\n//go:build ignore\n\nint unused;\n"},
		{"example.com/M@v1.0.0/closed.c", "//go:build ignore\n\n/* Explanation\n*/ int unused;\n"},
		{"example.com/M@v1.0.0/api.json", "{\"big\": true}\n"},
		{"example.com/M@v1.0.0/m_test.go", "package m\n"},
		{"example.com/M@v1.0.0/sub/testdata/fixture.go", "package fixture\n\nimport \"os\"\n\nvar _ = os.Args\n"},
		{"example.com/M@v1.0.0/sub/testdata/fixture_test.go", "package fixture\n"},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipRel := "cache/download/example.com/!m/@v/v1.0.0.zip"
	if err := os.WriteFile(filepath.Join(root, zipRel), zipped.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(dir, "m.gohdr")
	if out, err := exec.Command(filepath.Join(dir, "gohdr"), "packmod", root, pack, zipRel, "example.com/!m@v1.0.0", meta).CombinedOutput(); err != nil {
		t.Fatalf("packmod: %v\n%s", err, out)
	}
	out := filepath.Join(dir, "out")
	if b, err := exec.Command(filepath.Join(dir, "gohdr"), "unpack", pack, out, os.DevNull).CombinedOutput(); err != nil {
		t.Fatalf("unpack: %v\n%s", err, b)
	}
	for name, want := range map[string]string{
		meta:                                            "module example.com/M\n",
		"example.com/!m@v1.0.0/go.mod":                  "module example.com/M\n",
		"example.com/!m@v1.0.0/m.go":                    "package m\n\nimport \"fmt\"\n",
		"example.com/!m@v1.0.0/m_amd64.s":               "//go:build !purego\n\n",
		"example.com/!m@v1.0.0/unused.c":                "/* Copyright notice\n   continued */\n//go:build ignore\n\n",
		"example.com/!m@v1.0.0/closed.c":                "//go:build ignore\n\n/* Explanation\n*/ ",
		"example.com/!m@v1.0.0/api.json":                "",
		"example.com/!m@v1.0.0/sub/testdata/fixture.go": "package fixture\n\nimport \"os\"\n",
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
	for _, name := range []string{"example.com/!m@v1.0.0/m_test.go", "example.com/!m@v1.0.0/sub/testdata/fixture_test.go"} {
		if _, err := os.Stat(filepath.Join(out, name)); err == nil {
			t.Errorf("%s was packed", name)
		}
	}
}

// gohdr packmod refuses a zip the go command would refuse to extract: one
// with a path that leaves the module's directory, two paths that differ
// only in case, a file outside the module's directory, or a go.mod outside
// the module's root or named in upper case. A plan unpacks
// the pack, so such a path would be written there.
func TestHdrToolPackmodRefuses(t *testing.T) {
	dir := buildHdrTool(t)
	for name, files := range map[string][]string{
		"traversal": {"example.com/m@v1.0.0/go.mod", "example.com/m@v1.0.0/../../../src/go.sum"},
		"collision": {"example.com/m@v1.0.0/go.mod", "example.com/m@v1.0.0/a.go", "example.com/m@v1.0.0/A.go"},
		"outside":   {"example.com/m@v1.0.0/go.mod", "example.com/other@v1.0.0/x.go"},
		"nested":    {"example.com/m@v1.0.0/go.mod", "example.com/m@v1.0.0/m.go", "example.com/m@v1.0.0/nested/go.mod"},
		"upper":     {"example.com/m@v1.0.0/GO.MOD", "example.com/m@v1.0.0/m.go"},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(dir, name)
			zipRel := "cache/download/example.com/m/@v/v1.0.0.zip"
			if err := os.MkdirAll(filepath.Join(root, filepath.Dir(zipRel)), 0o755); err != nil {
				t.Fatal(err)
			}
			var zipped bytes.Buffer
			zw := zip.NewWriter(&zipped)
			for _, f := range files {
				w, err := zw.Create(f)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte("package m\n")); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, zipRel), zipped.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			pack := filepath.Join(root, "m.gohdr")
			out, err := exec.Command(filepath.Join(dir, "gohdr"), "packmod", root, pack, zipRel, "example.com/m@v1.0.0").CombinedOutput()
			if err == nil {
				t.Fatalf("packmod packed it:\n%s", out)
			}
			if _, err := os.Stat(pack); err == nil {
				t.Errorf("packmod wrote a pack")
			}
		})
	}
}

// gohdr imports lists each go.mod's replacements of a module by a directory,
// by the directory's path in the pack, and no other replacement.
func TestHdrToolImportsReplace(t *testing.T) {
	dir := buildHdrTool(t)
	files := map[string]string{
		"go.work": "go 1.22\n\nuse ./app\n\nreplace example.com/worked => ./workfork\n",
		"app/go.mod": "module example.com/app\n\ngo 1.22\n\n" +
			"replace example.com/original => ../fork // a fork\n\n" +
			"replace (\n\texample.com/sub v1.0.0 => ./sub\n\texample.com/versioned => example.com/other v1.2.3\n)\n",
		"app/main.go": "package main\n",
	}
	var pack bytes.Buffer
	pack.WriteString("gohdr 1\n")
	for _, f := range []string{"app/go.mod", "app/main.go", "go.work"} {
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
	for _, want := range []string{"mod app example.com/app\n", "repl fork example.com/original\n", "repl app/sub example.com/sub\n", "repl workfork example.com/worked\n"} {
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

// gohdr imports lists a file constrained by a legacy // +build line alone
// with the expression "+build", and one with a //go:build line by that.
func TestHdrToolImportsLegacyBuild(t *testing.T) {
	dir := buildHdrTool(t)
	files := map[string]string{
		"legacy.go":  "// +build linux\n\npackage m\n\nimport \"os\"\n",
		"both.go":    "//go:build linux\n// +build linux\n\npackage m\n",
		"plain.go":   "package m\n",
		"nospace.go": "//+build windows\n\npackage m\n\nimport \"syscall\"\n",
		"tab.go":     "//go:build\tlinux\n\npackage m\n",
	}
	var pack bytes.Buffer
	pack.WriteString("gohdr 1\n")
	for _, f := range []string{"both.go", "legacy.go", "nospace.go", "plain.go", "tab.go"} {
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
	for _, want := range []string{".\tlegacy.go\t+build\tos\n", ".\tboth.go\tlinux\t\n", ".\tplain.go\t-\t\n", ".\tnospace.go\t+build\tsyscall\n", ".\ttab.go\tlinux\t\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("imports output lacks %q:\n%s", want, out)
		}
	}
}
