package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"

	"dagger/gobuild/internal/dagger"
)

// modules are the downloaded modules a build's packages read, each its own
// download, keyed by the module, its version and the checksum the build's
// own download verified (against go.sum, or the checksum database): every
// build that reads a module version shares it, and a dependency bump
// changes only that module's packages and what imports them. A package
// mounts each module it reads whole, so its C and assembly can include any
// file of the module, which the plan never names, and nothing is selected
// file by file.
type modules struct {
	base *dagger.Container
	// root is where they are, GOMODCACHE.
	root string
	// all is the module cache the build downloaded, evaluated, for a file
	// outside any module's directory.
	all *dagger.Directory
	// sums holds the checksum of each module the build downloaded, by its
	// directory name, as the go command recorded it in the module cache.
	sums map[string]string

	mu   sync.Mutex
	dirs map[string]*dagger.Directory
}

// dir is the module directory <module>@<version>, as the module cache names
// it, downloaded on its own and checked against the build's checksum for it,
// which keys it.
func (m *modules) dir(name string) *dagger.Directory {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dirs[name]; ok {
		return d
	}
	at := strings.LastIndex(name, "@")
	ziphash := path.Join(m.root, "cache/download", name[:at], "@v", name[at+1:]+".ziphash")
	// Outside any module and workspace, with no GOFLAGS (such as -mod or
	// -modfile) that apply to the build's own module, and with the
	// checksum the build verified rather than the checksum database, so a
	// base whose module cache is filled offline downloads nothing. A Go
	// toolchain, which the go command downloads as a module when the build
	// asks for a newer Go, it verifies against the database whatever the
	// environment says, so its download keeps the base's setting; and its
	// commands are made executable, as the go command does before it first
	// runs a toolchain it downloaded.
	toolchain := strings.HasPrefix(name, "golang.org/toolchain@")
	script := "set -e\ngo mod download " + shQuote(unescapeModule(name)) +
		"\nsum=$(cat " + shQuote(ziphash) + ")" +
		"\n[ \"$sum\" = " + shQuote(m.sums[name]) + " ] || { echo " + shQuote(unescapeModule(name)+": checksum ") + "\"$sum\"" +
		shQuote(", not "+m.sums[name]+" as the build verified") + " >&2; exit 1; }"
	if toolchain {
		dir := shQuote(path.Join(m.root, name))
		script += "\nfor d in " + dir + "/bin " + dir + "/pkg/tool; do [ ! -d \"$d\" ] || find \"$d\" -type f -exec chmod a+x {} +; done"
	}
	ctr := m.base.
		WithEnvVariable("GOFLAGS", "").
		WithEnvVariable("GOWORK", "off")
	if !toolchain {
		ctr = ctr.WithEnvVariable("GOSUMDB", "off")
	}
	d := ctr.
		WithWorkdir("/").
		WithExec([]string{"sh", "-c", script}, noNest).
		Directory(path.Join(m.root, name))
	if m.dirs == nil {
		m.dirs = map[string]*dagger.Directory{}
	}
	m.dirs[name] = d
	return d
}

// downloaded is the downloaded module a file, relative to the module cache,
// is in, or "": a module the build's download recorded a checksum for. A
// directory named like one that the download didn't make, such as a local
// replacement placed in the module cache, isn't one.
func (m *modules) downloaded(f string) string {
	if name := moduleRoot(f); m.sums[name] != "" {
		return name
	}
	return ""
}

// mount mounts in ctr the downloaded modules files, relative to the module
// cache, are in, each whole, read-only, and any other files on their own.
func (m *modules) mount(ctr *dagger.Container, files []string) *dagger.Container {
	var names, loose []string
	for _, f := range files {
		if name := m.downloaded(f); name != "" {
			names = appendUniq(names, name)
		} else {
			loose = append(loose, f)
		}
	}
	if len(loose) > 0 {
		ctr = ctr.WithMountedDirectory(m.root, fileTree(m.all, loose))
	}
	sort.Strings(names)
	for _, name := range names {
		ctr = ctr.WithMountedDirectory(path.Join(m.root, name), m.dir(name), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	return ctr
}

// fetch downloads every module blocks read now, a few at once, so a
// package doesn't wait for its modules when its turn comes. An error shows
// again where the module is used.
func (m *modules) fetch(ctx context.Context, blocks map[string]*block) {
	var names []string
	for _, b := range blocks {
		for _, f := range b.modFiles {
			if name := m.downloaded(f); name != "" {
				names = appendUniq(names, name)
			}
		}
	}
	sem := make(chan struct{}, 16)
	for _, name := range names {
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			m.dir(name).Sync(ctx)
		}()
	}
}

// moduleSums lists the checksum of every module a module cache holds, by its
// directory name, from the go command's own record of each download.
func moduleSums(ctx context.Context, base *dagger.Container, mods *dagger.Directory) (map[string]string, error) {
	out, err := base.
		WithMountedDirectory("/gobuild/mods", mods).
		WithExec([]string{"sh", "-c", "cd /gobuild/mods/cache/download 2>/dev/null || exit 0; " +
			"find . -name '*.ziphash' | sort | while read -r f; do printf '%s %s\\n' \"$f\" \"$(cat \"$f\")\"; done"}, noNest).
		Stdout(ctx)
	if err != nil {
		return nil, err
	}
	return parseSums(out), nil
}

// parseSums parses moduleSums' listing: "./<module>/@v/<version>.ziphash
// <checksum>" lines, the module and version escaped as in the module cache.
func parseSums(out string) map[string]string {
	sums := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f, sum, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		f = strings.TrimSuffix(strings.TrimPrefix(f, "./"), ".ziphash")
		mod, ver, ok := strings.Cut(f, "/@v/")
		if !ok || sum == "" {
			continue
		}
		sums[mod+"@"+ver] = sum
	}
	return sums
}

// unescapeModule is a module cache directory name as the go command takes a
// module and version: the cache writes an upper-case letter as "!" and the
// letter in lower case.
func unescapeModule(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '!' && i+1 < len(name) {
			i++
			b.WriteRune(unicode.ToUpper(rune(name[i])))
			continue
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

// expandLoose replaces each pattern of module cache files outside any
// downloaded module, such as a directory a C compile reads by -I, with the
// files it matches, but Go files, so fileTree reads files alone. A
// downloaded module's own files need none: it is mounted whole.
func (m *modules) expandLoose(ctx context.Context, blocks map[string]*block) error {
	mods := m.all
	found := map[string][]string{}
	for _, b := range blocks {
		for _, f := range b.modFiles {
			if strings.Contains(f, "*") && m.downloaded(f) == "" {
				found[f] = nil
			}
		}
	}
	patterns := make([]string, 0, len(found))
	for pattern := range found {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, pattern := range patterns {
		dir, glob := splitPattern(pattern)
		hs, err := mods.Directory(dir).Glob(ctx, glob)
		if err != nil {
			return fmt.Errorf("module cache %s: %w", dir, err)
		}
		for _, h := range hs {
			// A glob names directories too, with a trailing slash.
			if !strings.HasSuffix(h, "/") && path.Ext(h) != ".go" {
				found[pattern] = append(found[pattern], dir+"/"+h)
			}
		}
	}
	for _, b := range blocks {
		var files []string
		for _, f := range b.modFiles {
			if hs, ok := found[f]; ok {
				for _, h := range hs {
					files = appendUniq(files, h)
				}
			} else {
				files = appendUniq(files, f)
			}
		}
		sort.Strings(files)
		b.modFiles = files
	}
	return nil
}

// splitPattern splits a path pattern at its first segment holding a "*":
// the directory before it, and the glob from it on.
func splitPattern(pattern string) (string, string) {
	segments := strings.Split(pattern, "/")
	for i, s := range segments {
		if strings.Contains(s, "*") {
			return strings.Join(segments[:i], "/"), strings.Join(segments[i:], "/")
		}
	}
	return path.Dir(pattern), path.Base(pattern)
}
