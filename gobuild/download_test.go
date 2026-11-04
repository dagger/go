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
	src := parseSourceImports("mod . example.com/root\n" +
		"mod tools example.com/root/tools\n" +
		"pkg . example.com/root/lib fmt\n" +
		"pkg lib github.com/dep/x\n" +
		"pkg tools/cmd example.com/root/tools/inner\n" +
		"pkg tools/inner golang.org/x/sys/unix\n")
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
