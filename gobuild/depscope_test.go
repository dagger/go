package main

import (
	"reflect"
	"testing"
)

func TestOutsideImports(t *testing.T) {
	run := func(list string) ([]string, bool, bool) {
		imports, main, _, ok := outsideImports(parseSourceImports(target{}, list), parseSourceFiles(list), "example.com/m", ".", ".")
		return imports, main, ok
	}
	base := "mod . example.com/m\nmod tools example.com/m/tools\n" +
		"name\t.\tmain\nname\tlib\tlib\nname\tother\tother\nname\ttools\ttools\n" +
		".\tmain.go\t-\texample.com/m/lib fmt\n" +
		".\tgen.go\tignore\tgolang.org/x/tools/cmd/stringer\n" +
		"lib\tlib.go\t-\tgithub.com/a/b os\n" +
		"other\tother.go\t-\tgithub.com/unused\n" +
		"tools\ttools.go\t-\tgithub.com/t\n"
	// Files the go command ignores, by name or constraint, and an internal
	// package below the importer, which it may import.
	base += ".\t_scratch.go\t-\tnet/http\n.\t.hidden.go\t-\tnet/http\n" +
		"name\tinternal/y\ty\ninternal/y\ty.go\t-\terrors\nlib\tl2.go\t-\texample.com/m/internal/y\n"
	got, main, ok := run(base)
	if want := []string{"errors", "fmt", "github.com/a/b", "os"}; !ok || !main || !reflect.DeepEqual(got, want) {
		t.Fatalf("outsideImports = %q, main %v, %v; want %q, a main package", got, main, ok, want)
	}
	lib := "mod . example.com/m\nname\t.\tm\n.\tm.go\t-\tstrings\n"
	if got, main, ok := run(lib); !ok || main || !reflect.DeepEqual(got, []string{"strings"}) {
		t.Errorf("outsideImports of a library = %q, main %v, %v", got, main, ok)
	}
	for name, l := range map[string]string{
		"a package of another module of the source":  base + ".\tx.go\t-\texample.com/m/tools\n",
		"a module's package the source doesn't hold": base + ".\tx.go\t-\texample.com/m/missing\n",
		"a build constraint":                         base + "lib\tl.go\tlinux\tnet\n",
		"a GOOS file name":                           base + "lib\tl_windows.go\t-\tnet\n",
		"another module's internal package":          base + "lib\tl.go\t-\texample.com/x/internal/y\n",
		"cgo":                                        base + "cgo lib\n",
		"an embed":                                   base + "embed lib\n",
		"a header that doesn't parse":                base + "unparsed lib\n",
		"two package names":                          base + "name\tlib\tother\n",
		"an imported main package":                   base + "name\tother\tmain\nlib\tl.go\t-\texample.com/m/other\n",
		"an import cycle":                            base + "lib\tl.go\t-\texample.com/m\n",
		"a directory replacing a module":             base + "repl fork example.com/x\n",
		"a legacy // +build line":                    base + "lib\tl.go\t+build\tnet\n",
		"a package whose files are all ignored":      base + "name\tskip\tskip\nskip\ts.go\tignore\tnet\nlib\tl.go\t-\texample.com/m/skip\n",
		"an internal package it may not import":      base + "name\tother/internal/x\tx\nother/internal/x\tx.go\t-\tos\nlib\tl.go\t-\texample.com/m/other/internal/x\n",
		"no package":                                 "mod . example.com/m\n",
	} {
		if got, _, ok := run(l); ok {
			t.Errorf("%s: outsideImports = %q, want not ok", name, got)
		}
	}
	// A package of a module nested in the build's isn't the build's.
	nested := "mod . example.com/m\nmod nested example.com/m/nested\nname\tnested\tnested\nnested\tn.go\t-\tos\n"
	if got, _, _, ok := outsideImports(parseSourceImports(target{}, nested), parseSourceFiles(nested), "example.com/m", ".", "nested"); ok {
		t.Errorf("outsideImports of a nested module's package = %q, want not ok", got)
	}
	// A module below the workspace root, as the build's directory.
	sub := "mod . example.com/root\nmod sub example.com/sub\nname\tsub\ts\nname\tsub/x\tx\nsub\ts.go\t-\texample.com/sub/x\nsub/x\tx.go\t-\tstrings\n"
	if got, _, _, ok := outsideImports(parseSourceImports(target{}, sub), parseSourceFiles(sub), "example.com/sub", "sub", "sub"); !ok || !reflect.DeepEqual(got, []string{"strings"}) {
		t.Errorf("outsideImports in sub = %q, %v; want [strings]", got, ok)
	}
}

func TestNamesItself(t *testing.T) {
	for mod, want := range map[string]bool{
		"module example.com/a\n\ngo 1.22\n\nrequire example.com/b v1.0.0\n":                                          false,
		"module example.com/a\n\nrequire example.com/b v1.0.0\n\nreplace example.com/a => example.com/fork v1.0.0\n": true,
		"module \"example.com/a\"\n\nrequire (\n\texample.com/a v0.1.0\n)\n":                                         true,
		"module example.com/a\n\nrequire example.com/a/sub v1.0.0\n":                                                 false,
	} {
		if got := namesItself(mod, "example.com/a"); got != want {
			t.Errorf("namesItself(%q) = %v, want %v", mod, got, want)
		}
	}
}

func TestNamesModule(t *testing.T) {
	sum := "example.com/a v1.0.0 h1:x=\nexample.com/m v0.1.0/go.mod h1:y=\n"
	if !namesModule(sum, "example.com/m") || namesModule(sum, "example.com/m/sub") || namesModule("", "example.com/m") {
		t.Errorf("namesModule is wrong for %q", sum)
	}
}

func TestGoModInfo(t *testing.T) {
	mod := "// comment\nmodule \"example.com/m\"\n\ngo 1.24.2\n\nrequire example.com/x v1.0.0\n"
	if p, minor, tc := goModInfo(mod); p != "example.com/m" || minor != 24 || tc {
		t.Errorf("goModInfo = %q, %d, %v", p, minor, tc)
	}
	if _, _, tc := goModInfo(mod + "toolchain go1.27.3\n"); !tc {
		t.Errorf("goModInfo missed the toolchain directive")
	}
	if got, want := withModulePath(mod, depsModule), "// comment\nmodule gobuild.invalid/deps\n\ngo 1.24.2\n\nrequire example.com/x v1.0.0\n"; got != want {
		t.Errorf("withModulePath = %q, want %q", got, want)
	}
}

func TestInternalVisible(t *testing.T) {
	for _, c := range []struct {
		importer, ip string
		want         bool
	}{
		{"example.com/m", "example.com/m/internal/x", true},
		{"example.com/m/a", "example.com/m/b/internal/c", false},
		{"example.com/m/b/d", "example.com/m/b/internal/c", true},
		{"example.com/m", "example.com/m/a/internal/b/internal/c", false},
		{"example.com/m/a/internal/b/z", "example.com/m/a/internal/b/internal/c", true},
		{"example.com/m", "example.com/m/lib", true},
	} {
		if got := internalVisible(c.importer, c.ip); got != c.want {
			t.Errorf("internalVisible(%q, %q) = %v, want %v", c.importer, c.ip, got, c.want)
		}
	}
}

func TestPatternFlags(t *testing.T) {
	for _, c := range []struct {
		flags []string
		want  bool
	}{
		{nil, false},
		{[]string{"-gcflags=-N -l", "-ldflags=-s -w"}, false},
		{[]string{"-gcflags=all=-N -l", "-asmflags=std=-x"}, false},
		{[]string{"-ldflags=example.com/app=-linkmode=external"}, true},
		{[]string{"-gcflags", "example.com/m/...=-m"}, true},
	} {
		if got := patternFlags(c.flags); got != c.want {
			t.Errorf("patternFlags(%q) = %v, want %v", c.flags, got, c.want)
		}
	}
}
