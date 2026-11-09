package main

import (
	"go/build/constraint"
	"strconv"
	"strings"
)

// target is what a build's build constraints are evaluated against, as
// go/build evaluates them: the target's GOOS and GOARCH, cgo, the release
// tags of its Go version, and the build's -tags.
type target struct {
	goos, goarch string
	cgo          bool
	goMinor      int
	tags         map[string]bool
}

// newTarget is the target `go env GOOS GOARCH CGO_ENABLED GOVERSION` prints,
// one per line, with the build's -tags.
func newTarget(goenv string, tags []string) target {
	f := strings.Fields(goenv)
	t := target{tags: map[string]bool{}}
	if len(f) > 0 {
		t.goos = f[0]
	}
	if len(f) > 1 {
		t.goarch = f[1]
	}
	if len(f) > 2 {
		t.cgo = f[2] == "1"
	}
	if len(f) > 3 {
		v := strings.TrimPrefix(f[3], "go")
		if _, minor, ok := strings.Cut(v, "."); ok {
			minor, _, _ = strings.Cut(minor, ".")
			// A prerelease such as "27rc1" counts as its minor.
			end := 0
			for end < len(minor) && minor[end] >= '0' && minor[end] <= '9' {
				end++
			}
			t.goMinor, _ = strconv.Atoi(minor[:end])
		}
	}
	for _, tag := range tags {
		t.tags[tag] = true
	}
	return t
}

// matchTag reports whether a build tag holds for the target, as go/build's
// matchTag does.
func (t target) matchTag(tag string) bool {
	switch {
	case tag == t.goos, tag == t.goarch, tag == "gc", t.tags[tag]:
		return true
	case tag == "cgo":
		return t.cgo
	case tag == "unix":
		return unixOS[t.goos]
	case tag == "linux":
		return t.goos == "android"
	case tag == "solaris":
		return t.goos == "illumos"
	case tag == "darwin":
		return t.goos == "ios"
	case strings.HasPrefix(tag, "go1."):
		n, err := strconv.Atoi(strings.TrimPrefix(tag, "go1."))
		return err == nil && n <= t.goMinor
	}
	return false
}

// matchFile reports whether a Go file builds for the target, by its name's
// GOOS and GOARCH suffixes and its //go:build expression ("-" for none).
func (t target) matchFile(name, expr string) bool {
	if t.goos == "" {
		return true
	}
	if !t.goodOSArchFile(name) {
		return false
	}
	if expr == "" || expr == "-" {
		return true
	}
	x, err := constraint.Parse("//go:build " + expr)
	if err != nil {
		return true
	}
	return x.Eval(t.matchTag)
}

// goodOSArchFile is go/build's: a name_$GOOS, name_$GOARCH or
// name_$GOOS_$GOARCH file, a _test suffix aside, builds only for those.
func (t target) goodOSArchFile(name string) bool {
	name = strings.TrimSuffix(name, ".go")
	i := strings.Index(name, "_")
	if i < 0 {
		return true
	}
	l := strings.Split(name[i:], "_")
	if n := len(l); n > 0 && l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	if n >= 2 && knownOS[l[n-2]] && knownArch[l[n-1]] {
		return t.matchTag(l[n-2]) && t.matchTag(l[n-1])
	}
	if n >= 1 && (knownOS[l[n-1]] || knownArch[l[n-1]]) {
		return t.matchTag(l[n-1])
	}
	return true
}

// From go/build's syslist.go.
var knownOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
	"illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true, "openbsd": true,
	"plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true,
}

var unixOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
	"illumos": true, "ios": true, "linux": true, "netbsd": true, "openbsd": true, "solaris": true,
}

var knownArch = map[string]bool{
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true, "arm64be": true,
	"loong64": true, "mips": true, "mipsle": true, "mips64": true, "mips64le": true, "mips64p32": true,
	"mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

// fileImportsFor is a parsed imports list (importLine's lines): each package's
// imports, by its directory, of the files that build for the target.
func fileImportsFor(t target, list string) map[string][]string {
	seen := map[string]map[string]bool{}
	out := map[string][]string{}
	for _, line := range strings.Split(list, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || !t.matchFile(f[1], f[2]) {
			continue
		}
		dir := f[0]
		if seen[dir] == nil {
			seen[dir] = map[string]bool{}
			out[dir] = []string{}
		}
		for _, ip := range strings.Fields(f[3]) {
			if !seen[dir][ip] {
				seen[dir][ip] = true
				out[dir] = append(out[dir], ip)
			}
		}
	}
	return out
}
