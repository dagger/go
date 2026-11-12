package main

import (
	"reflect"
	"testing"
)

func TestScopeInclude(t *testing.T) {
	marks := map[string][]string{"web": {"embed"}, "unread": {"cgo", "link"}}
	got, exclude, ok := scopeInclude([]string{"cmd/app", ".", "web", "web/sub", ""}, marks, []string{"unread/x.c", "other/sub/a.s", "dir.c/"}, nil, true)
	want := []string{"**/go.mod", "**/go.sum", "go.work", "go.work.sum", "**/vendor/modules.txt",
		"*.go", "*.syso", "default.pgo",
		"cmd/app/*.go", "cmd/app/*.syso", "cmd/app/default.pgo",
		"web/**",
		"web/sub/*.go", "web/sub/*.syso", "web/sub/default.pgo"}
	if !ok || !reflect.DeepEqual(got, want) || exclude != nil {
		t.Fatalf("scopeInclude = %q, %q, %v; want %q", got, exclude, ok, want)
	}
	// Without tests, test files are left out but where an embed may name
	// them: in or below a directory that embeds.
	got, exclude, ok = scopeInclude([]string{"cmd/app", ".", "web", "web/sub", ""}, marks, nil, nil, false)
	if want := []string{"*_test.go", "cmd/app/*_test.go"}; !ok || !reflect.DeepEqual(exclude, want) {
		t.Fatalf("scopeInclude without tests: exclude %q, %v; want %q", exclude, ok, want)
	}
	// A directory named like a test file, such as a package's or another
	// workspace module's, is kept: the directory holding it leaves its test
	// files in.
	_, exclude, ok = scopeInclude([]string{".", "cmd/app", "cmd/app/lib_test.go/inner"}, nil, nil, []string{"cmd/app/lib_test.go", "other_test.go"}, false)
	if want := []string{"cmd/app/lib_test.go/inner/*_test.go"}; !ok || !reflect.DeepEqual(exclude, want) {
		t.Fatalf("scopeInclude with directories named like a test file: exclude %q, %v; want %q", exclude, ok, want)
	}
	for name, c := range map[string]struct {
		marks   map[string][]string
		natives []string
	}{
		"C in a directory read": {nil, []string{"cmd/app/x.c"}},
		"assembly at the root":  {nil, []string{"asm_amd64.s"}},
		"cgo":                   {map[string][]string{"cmd/app": {"cgo"}}, nil},
		"a symlink":             {map[string][]string{".": {"link"}}, nil},
	} {
		if got, _, ok := scopeInclude([]string{".", "cmd/app", "web/x.h"}, c.marks, c.natives, nil, true); ok {
			t.Errorf("%s: scopeInclude = %q, want the whole source", name, got)
		}
	}
}

func TestFileFlags(t *testing.T) {
	for _, c := range []struct {
		flags []string
		want  bool
	}{
		{nil, false},
		{[]string{"-race", "-trimpath", "-tags=a,b", "-mod=mod", "-buildvcs=false"}, false},
		{[]string{"-ldflags=-s -w -X main.version=/opt/v -X=main.dir=./x"}, false},
		{[]string{"-ldflags", "-X github.com/a/b.V=1"}, false},
		{[]string{"-gcflags=all=-N -l", "-gcflags=github.com/a/...=-m"}, false},
		{[]string{"-modfile="}, false},
		{[]string{"-pgo=auto"}, false},
		{[]string{"-pgo=off"}, false},
		{[]string{"-modfile=go.test.mod"}, true},
		{[]string{"-modfile", "go.test.mod"}, true},
		{[]string{"-pgo", "cpu.pprof"}, true},
		{[]string{"-overlay=overlay.json"}, true},
		{[]string{"-toolexec=wrap"}, true},
		{[]string{"-C", "sub"}, true},
		{[]string{"-asmflags=-I ./include"}, true},
		{[]string{"-gcflags=all=-importcfg=/src/cfg"}, true},
		{[]string{"-ldflags=-extldflags=-Wl,--version-script=./v.map"}, true},
		{[]string{"-gcflags=-pgoprofile '/src/profiles/default.pgo'"}, true},
		{[]string{"-ldflags=-X 'main.v=/a b'"}, false},
	} {
		if got := fileFlags(c.flags); got != c.want {
			t.Errorf("fileFlags(%q) = %v, want %v", c.flags, got, c.want)
		}
	}
}

func TestPackageDirs(t *testing.T) {
	for _, c := range []struct {
		workdir  string
		packages []string
		dirs     []string
		ok       bool
	}{
		{".", []string{"./cmd/app"}, []string{"cmd/app"}, true},
		{"sub", []string{".", "./a"}, []string{"sub", "sub/a"}, true},
		{".", []string{"./a", "./..."}, nil, true},
		{".", nil, nil, true},
		// An import path names no directory this can tell.
		{".", []string{"./a", "example.com/m/b"}, nil, false},
		{".", []string{"all"}, nil, false},
		{"sub", []string{"../other"}, nil, false},
		{"sub", []string{"./a/../../other"}, nil, false},
	} {
		dirs, ok := packageDirs(c.workdir, c.packages)
		if ok != c.ok || !reflect.DeepEqual(dirs, c.dirs) {
			t.Errorf("packageDirs(%q, %q) = %q, %v; want %q, %v", c.workdir, c.packages, dirs, ok, c.dirs, c.ok)
		}
	}
}
