package main

import (
	"reflect"
	"testing"
)

func TestParseGoSums(t *testing.T) {
	got := parseGoSums([]string{
		"github.com/A/b v1.0.0 h1:zip=\n" +
			"github.com/A/b v1.0.0/go.mod h1:mod=\n" +
			"example.com/only v0.1.0/go.mod h1:onlymod=\n" +
			"not a line\n" +
			"example.com/bad v1 nothash\n",
		// A second go.sum repeats an entry and adds one.
		"github.com/A/b v1.0.0 h1:zip=\n" +
			"golang.org/x/sys v0.42.0 h1:sys=\n",
	})
	want := []sumEntry{
		{path: "example.com/only", version: "v0.1.0", mod: "h1:onlymod="},
		{path: "github.com/A/b", version: "v1.0.0", zip: "h1:zip=", mod: "h1:mod="},
		{path: "golang.org/x/sys", version: "v0.42.0", zip: "h1:sys="},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseGoSums:\n got %+v\nwant %+v", got, want)
	}
}

func TestEscapeModule(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/Azure/go-autorest": "github.com/!azure/go-autorest",
		"golang.org/x/sys":             "golang.org/x/sys",
		"v1.0.0-RC1":                   "v1.0.0-!r!c1",
	} {
		if got := escapeModule(in); got != want {
			t.Errorf("escapeModule(%q) = %q, want %q", in, got, want)
		}
		if got := unescapeModule(escapeModule(in)); got != in {
			t.Errorf("unescapeModule(escapeModule(%q)) = %q", in, got)
		}
	}
}

func TestMatchPrefixPatterns(t *testing.T) {
	for _, c := range []struct {
		patterns, path string
		want           bool
	}{
		{"example.com", "example.com/private/repo", true},
		{"example.com/private", "example.com/private/repo", true},
		{"example.com/private", "example.com/public/repo", false},
		{"*.corp.example.com", "git.corp.example.com/team/repo", true},
		{"*.corp.example.com", "corp.example.com/repo", false},
		{"github.com/org/*", "github.com/org/repo/sub", true},
		{"github.com/org/*", "github.com/other/repo", false},
		{" a.com , example.com/x/ ", "example.com/x/y", true},
		{"", "example.com", false},
		{"example.com/a/b/c", "example.com/a", false},
	} {
		if got := matchPrefixPatterns(c.patterns, c.path); got != c.want {
			t.Errorf("matchPrefixPatterns(%q, %q) = %v, want %v", c.patterns, c.path, got, c.want)
		}
	}
}

func TestProviders(t *testing.T) {
	byPath := map[string][]sumEntry{
		"github.com/a/b":           {{path: "github.com/a/b", version: "v1.0.0", zip: "h1:x"}},
		"github.com/a/b/v2":        {{path: "github.com/a/b/v2", version: "v2.1.0", zip: "h1:y"}},
		"golang.org/x/sys":         {{path: "golang.org/x/sys", version: "v0.42.0", zip: "h1:z"}},
		"cloud.google.com/go":      {{path: "cloud.google.com/go", version: "v0.1.0", zip: "h1:c"}},
		"cloud.google.com/go/auth": {{path: "cloud.google.com/go/auth", version: "v0.2.0", zip: "h1:d"}},
	}
	for ip, want := range map[string]string{
		"github.com/a/b":                 "github.com/a/b",
		"github.com/a/b/pkg/sub":         "github.com/a/b",
		"github.com/a/b/v2/pkg":          "github.com/a/b/v2",
		"github.com/a/bc":                "",
		"golang.org/x/sys/unix":          "golang.org/x/sys",
		"cloud.google.com/go/auth/oauth": "cloud.google.com/go/auth",
		"cloud.google.com/go/storage":    "cloud.google.com/go",
		"fmt":                            "",
		"dagger/gobuild/internal/dagger": "",
	} {
		got := ""
		if es := providers(byPath, ip); len(es) > 0 {
			got = es[0].path
		}
		if got != want {
			t.Errorf("providers(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestSourceImports(t *testing.T) {
	src := parseSourceImports(newTarget("linux\narm64\n0\ngo1.27.3\n", nil), "mod . example.com/root\n"+
		"mod tools example.com/root/tools\n"+
		".\tmain.go\t-\texample.com/root/lib fmt\n"+
		"lib\tlib.go\t-\tgithub.com/dep/x\n"+
		"tools/cmd\tmain.go\t-\texample.com/root/tools/inner\n"+
		"tools/inner\tinner.go\t-\tgolang.org/x/sys/unix\n")
	for ip, want := range map[string]string{
		"example.com/root":             ".",
		"example.com/root/lib":         "lib",
		"example.com/root/tools/cmd":   "tools/cmd",
		"example.com/root/tools/inner": "tools/inner",
		"example.com/root/missing":     "",
		"github.com/dep/x":             "",
	} {
		dir, ok := src.dirOf(ip)
		if !ok {
			dir = ""
		}
		if dir != want {
			t.Errorf("dirOf(%q) = %q, want %q", ip, dir, want)
		}
	}
}

func TestTargetMatchFile(t *testing.T) {
	tg := newTarget("linux\narm64\n0\ngo1.27.3\n", []string{"extended"})
	for _, c := range []struct {
		name, expr string
		want       bool
	}{
		{"a.go", "-", true},
		{"a_linux.go", "-", true},
		{"a_windows.go", "-", false},
		{"a_arm64.go", "-", true},
		{"a_amd64.go", "-", false},
		{"a_linux_arm64.go", "-", true},
		{"a_darwin_arm64.go", "-", false},
		{"a_linux_test.go", "-", true},
		{"tools.go", "tools", false},
		{"x.go", "ignore", false},
		{"x.go", "linux && !cgo", true},
		{"x.go", "cgo", false},
		{"x.go", "unix", true},
		{"x.go", "go1.27", true},
		{"x.go", "go1.28", false},
		{"x.go", "extended", true},
		{"x.go", "windows || darwin", false},
	} {
		if got := tg.matchFile(c.name, c.expr); got != c.want {
			t.Errorf("matchFile(%q, %q) = %v, want %v", c.name, c.expr, got, c.want)
		}
	}
}

// planDirsOf follows imports from the package built, whatever their files'
// build constraints, through the source and the downloaded modules' packages,
// and no further: not into another package of a module, nor a module not
// downloaded.
func TestPlanDirsOf(t *testing.T) {
	src := parseSourceImports(newTarget("linux\namd64\n0\ngo1.27.3\n", nil), "mod . example.com/root\n"+
		"mod sub example.com/root/sub\n"+
		"cmd\tmain.go\t-\texample.com/root/lib fmt\n"+
		"lib\tlib_windows.go\t-\tgithub.com/dep/x/win\n"+
		"lib\tlib.go\t-\texample.com/root/sub/inner\n"+
		"sub/inner\tinner.go\t-\tgithub.com/dep/x\n"+
		"unread\tunread.go\t-\tgithub.com/dep/y\n")
	byPath := map[string][]sumEntry{
		"github.com/dep/x": {{path: "github.com/dep/x", version: "v1.0.0", zip: "h1:x"}},
		"github.com/dep/y": {{path: "github.com/dep/y", version: "v1.0.0", zip: "h1:y"}},
		"github.com/dep/z": {{path: "github.com/dep/z", version: "v1.0.0", zip: "h1:z"}},
	}
	pkgs := map[string]modulePackages{
		"github.com/dep/x@v1.0.0": {
			all: map[string][]string{
				"github.com/dep/x":       {"github.com/dep/z/zz"},
				"github.com/dep/x/win":   {"golang.org/x/sys/windows"},
				"github.com/dep/x/other": nil,
			},
			dirs: map[string]string{
				"github.com/dep/x":       "github.com/dep/x@v1.0.0",
				"github.com/dep/x/win":   "github.com/dep/x@v1.0.0/win",
				"github.com/dep/x/other": "github.com/dep/x@v1.0.0/other",
			},
		},
	}
	got, _ := planDirsOf(src, "cmd", byPath, pkgs)
	want := planDirs{
		src:  []string{"cmd", "lib", "sub/inner"},
		mods: []string{"github.com/dep/x@v1.0.0", "github.com/dep/x@v1.0.0/win"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planDirsOf:\n got %+v\nwant %+v", got, want)
	}
}

// A package in a testdata directory is followed when the build names it, and
// left out of a build of every package, as "./..." leaves it out.
func TestPlanDirsOfTestdata(t *testing.T) {
	src := parseSourceImports(newTarget("linux\namd64\n0\ngo1.27.3\n", nil), "mod . example.com/root\n"+
		"mod testdata/fixture example.com/fixture\n"+
		"lib\tlib.go\t-\t\n"+
		"testdata/fixture/cmd\tmain.go\t-\texample.com/fixture/inner\n"+
		"testdata/fixture/inner\tinner.go\t-\t\n")
	if got, want := planSrcDirs(src, "testdata/fixture/cmd"), []string{"testdata/fixture/cmd", "testdata/fixture/inner"}; !reflect.DeepEqual(got, want) {
		t.Errorf("planDirsOf(testdata/fixture/cmd).src = %q, want %q", got, want)
	}
	if got, want := planSrcDirs(src, ""), []string{"lib"}; !reflect.DeepEqual(got, want) {
		t.Errorf("planDirsOf(every package).src = %q, want %q", got, want)
	}
}

// A package in a directory a go.mod replaces a module with is followed by the
// replaced module's import paths, whatever module path its own go.mod
// declares, as the go command accepts.
func TestPlanDirsOfReplace(t *testing.T) {
	src := parseSourceImports(newTarget("linux\namd64\n0\ngo1.27.3\n", nil), "mod . example.com/app\n"+
		"mod fork example.com/fork\n"+
		"repl fork example.com/original\n"+
		".\tmain.go\t-\texample.com/original/pkg\n"+
		"fork/pkg\tpkg.go\t-\t\n"+
		"fork/other\tother.go\t-\t\n")
	if got, want := planSrcDirs(src, "."), []string{".", "fork/pkg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("planDirsOf(.).src = %q, want %q", got, want)
	}
}

// planSrcDirs is the source directories planDirsOf reaches from pkgDir.
func planSrcDirs(src sourceImports, pkgDir string) []string {
	dirs, _ := planDirsOf(src, pkgDir, nil, nil)
	return dirs.src
}

// outside follows the source's packages from the package built, through the
// source alone, to the import paths outside it that a module may provide:
// by the target's build constraints, and by none, with the directories a
// plan reads. A package of another module that imports nothing stays out,
// as does one no package built imports.
func TestOutside(t *testing.T) {
	src := parseSourceImports(newTarget("linux\namd64\n0\ngo1.27.3\n", nil), "mod . example.com/root\n"+
		"mod fork example.com/fork\n"+
		"repl fork example.com/original\n"+
		".\tmain.go\t-\texample.com/root/lib fmt github.com/dep/x\n"+
		"lib\tlib.go\t-\tgithub.com/dep/y example.com/original/pkg\n"+
		"lib\tlib_windows.go\t-\tgithub.com/dep/win\n"+
		"fork/pkg\tpkg.go\t-\tgithub.com/dep/fork\n"+
		"unread\tunread.go\t-\tgithub.com/dep/unread\n")
	imports, plan, dirs := src.outside(".")
	// The replaced module's package is a module's to discover, which finds
	// no module for it, and the source's to a plan.
	if want := []string{"example.com/original/pkg", "github.com/dep/x", "github.com/dep/y"}; !reflect.DeepEqual(imports, want) {
		t.Errorf("imports = %q, want %q", imports, want)
	}
	if want := []string{"github.com/dep/fork", "github.com/dep/win", "github.com/dep/x", "github.com/dep/y"}; !reflect.DeepEqual(plan, want) {
		t.Errorf("plan = %q, want %q", plan, want)
	}
	if want := []string{".", "fork/pkg", "lib"}; !reflect.DeepEqual(dirs, want) {
		t.Errorf("dirs = %q, want %q", dirs, want)
	}
	if imports, plan, _ := src.outside(""); !slicesContain(imports, "github.com/dep/unread") || !slicesContain(plan, "github.com/dep/unread") {
		t.Errorf("outside(every package) = %q, %q, want github.com/dep/unread in both", imports, plan)
	}
}

func slicesContain(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// The discovery call follows the modules' packages from what the source
// imports from outside it, as from a source of one package that imports
// it: a plan reads the same module directories as when planDirsOf follows
// the source itself, and what it followed is none of the source's.
func TestPlanDirsOfOutside(t *testing.T) {
	src := parseSourceImports(newTarget("linux\namd64\n0\ngo1.27.3\n", nil), "mod . example.com/root\n"+
		"cmd\tmain.go\t-\texample.com/root/lib\n"+
		"lib\tlib.go\t-\tgithub.com/dep/x\n")
	byPath := map[string][]sumEntry{
		"github.com/dep/x": {{path: "github.com/dep/x", version: "v1.0.0", zip: "h1:x"}},
		"github.com/dep/z": {{path: "github.com/dep/z", version: "v1.0.0", zip: "h1:z"}},
	}
	pkgs := map[string]modulePackages{
		"github.com/dep/x@v1.0.0": {
			all:  map[string][]string{"github.com/dep/x": {"github.com/dep/z/zz", "strings"}},
			dirs: map[string]string{"github.com/dep/x": "github.com/dep/x@v1.0.0"},
		},
		"github.com/dep/z@v1.0.0": {
			all:  map[string][]string{"github.com/dep/z/zz": nil},
			dirs: map[string]string{"github.com/dep/z/zz": "github.com/dep/z@v1.0.0/zz"},
		},
	}
	want, _ := planDirsOf(src, "cmd", byPath, pkgs)
	imports, plan, _ := src.outside("cmd")
	called, followed := planDirsOf(sourceImports{pkgs: map[string][]string{".": imports}, all: map[string][]string{".": plan}}, ".", byPath, pkgs)
	if !reflect.DeepEqual(called.mods, want.mods) {
		t.Errorf("mods through the call = %q, want %q", called.mods, want.mods)
	}
	if want := []string{"github.com/dep/x", "github.com/dep/z/zz", "strings"}; !reflect.DeepEqual(followed, want) {
		t.Errorf("followed = %q, want %q", followed, want)
	}
	if src.providesAny(imports, followed) {
		t.Errorf("providesAny(%q, %q) = true, want false", imports, followed)
	}
}

// A module's package that imports one of the source's sends discover into
// the source, which the discovery call can't follow: the build discovers
// on its own then.
func TestProvidesAny(t *testing.T) {
	src := parseSourceImports(newTarget("linux\namd64\n0\ngo1.27.3\n", nil), "mod . example.com/root\n"+
		"mod fork example.com/fork\n"+
		"repl fork example.com/original\n"+
		".\tmain.go\t-\tgithub.com/dep/x\n"+
		"lib\tlib.go\t-\t\n"+
		"win\twin_windows.go\t-\t\n"+
		"fork/pkg\tpkg.go\t-\t\n")
	for _, c := range []struct {
		followed, plan []string
		want           bool
	}{
		{[]string{"github.com/dep/x", "fmt"}, []string{"github.com/dep/x"}, false},
		{[]string{"github.com/dep/x", "example.com/root/lib"}, nil, true},
		{nil, []string{"example.com/root/lib"}, true},
		// Only a plan follows a package no file of which builds for the
		// target, or a replacement directory's.
		{[]string{"example.com/root/win", "example.com/original/pkg"}, nil, false},
		{nil, []string{"example.com/root/win"}, true},
		{nil, []string{"example.com/original/pkg"}, true},
	} {
		if got := src.providesAny(c.followed, c.plan); got != c.want {
			t.Errorf("providesAny(%q, %q) = %v, want %v", c.followed, c.plan, got, c.want)
		}
	}
}

func TestGoSum(t *testing.T) {
	entries := parseGoSums([]string{
		"github.com/A/b v1.0.0 h1:zip=\ngithub.com/A/b v1.0.0/go.mod h1:mod=\nexample.com/only v0.1.0/go.mod h1:onlymod=\n",
		"golang.org/x/sys v0.42.0 h1:sys=\ngithub.com/A/b v1.0.0 h1:zip=\n",
	})
	want := "github.com/A/b v1.0.0 h1:zip=\ngithub.com/A/b v1.0.0/go.mod h1:mod=\ngolang.org/x/sys v0.42.0 h1:sys=\n"
	if got := goSum(entries); got != want {
		t.Errorf("goSum = %q, want %q", got, want)
	}
	if got := goSum(parseGoSums([]string{want})); got != want {
		t.Errorf("goSum of its own output = %q, want %q", got, want)
	}
}
