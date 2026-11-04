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
