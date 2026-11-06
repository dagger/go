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

// modules are the downloaded modules a build's packages read, each
// extracted on its own from the build's download, keyed by that download's
// content, which the build verified (against go.sum, or the checksum
// database): every build that reads a module version shares it, and a
// dependency bump
// changes only that module's packages and what imports them. A package
// mounts each module it reads whole, so its C and assembly can include any
// file of the module, which the plan never names, and nothing is selected
// file by file. What no build of a module reads, its tests and testdata,
// is left out (testExclude), but for what the build does read.
type modules struct {
	// root is where they are, GOMODCACHE.
	root string
	// all is the module cache the go command made before any download,
	// evaluated, for a file outside any module's directory, or a module's
	// zip downloaded with it.
	all *dagger.Directory
	// zips is each module's zip the build downloaded on its own, by its
	// directory name.
	zips map[string]*dagger.File
	// fetchZip downloads the zip of a module zips lacks, by its directory
	// name; nil when the build downloaded per project.
	fetchZip func(name string) *dagger.File
	// sums holds the checksum of each module the build downloaded, by its
	// directory name, as the go command recorded it in the module cache.
	sums map[string]string
	// exclude holds, by module directory name, the patterns of what the
	// module's extraction leaves out (excludeUnread); a module not in it
	// is extracted whole.
	exclude map[string][]string

	mu   sync.Mutex
	dirs map[string]*dagger.Directory
	// ready holds the modules whose directory in dirs is synced, so it
	// carries its ID.
	ready map[string]bool
}

// dir is the module directory <module>@<version>, as the module cache names
// it, extracted on its own from the build's download of it, which the build
// verified. The zip is read on its own from the evaluated module cache, so
// the directory is keyed by its content, the same in every project,
// toolchain and module cache path that reads that module version; and
// nothing is fetched, nor does the go command run, whose version and
// environment would otherwise decide the install. A Go toolchain, which
// the go command downloads as a module when the build asks for a newer Go,
// comes out runnable: its zip records its commands' modes, which extraction
// keeps.
func (m *modules) dir(name string) *dagger.Directory {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dirs[name]; ok {
		return d
	}
	at := strings.LastIndex(name, "@")
	zip, ok := m.zips[name]
	if !ok && m.fetchZip != nil {
		zip = m.fetchZip(name)
	}
	if zip == nil {
		zip = m.all.File(path.Join("cache/download", name[:at], "@v", name[at+1:]+".zip"))
	}
	// The zip holds the module's files under "<module>@<version>/", the
	// module path not escaped.
	d := zip.Extract(dagger.FileExtractOpts{Exclude: m.exclude[name]}).Directory(unescapeModule(name))
	if m.dirs == nil {
		m.dirs = map[string]*dagger.Directory{}
	}
	m.dirs[name] = d
	return d
}

// testExclude is the patterns of what a module holds that building it
// never reads: its tests, and its testdata directories, which the go
// command ignores. In some modules they are most of the files. They match
// under the module's directory in its zip, mod, the module path not
// escaped, so the module's own path never matches.
func testExclude(mod string) []string {
	mod = escapePattern(mod)
	return []string{mod + "/**/*_test.go", mod + "/**/testdata"}
}

// excludeUnread sets, in m.exclude, what each downloaded module blocks read
// leaves out of its extraction: testExclude, with patterns that put back
// what the build reads from there, such as a dependency's tests that go
// test -c compiles, a file a package embeds from testdata, or a directory
// a package's C reads whole. A module a package reads whole, for its C or
// assembly, which can include any of its files, is extracted whole. A
// module the build reads none of those from, nearly every one, extracts
// the same in every build.
func (m *modules) excludeUnread(blocks map[string]*block) {
	whole := map[string]bool{}
	keep := map[string][]string{}
	for _, b := range blocks {
		for _, f := range b.modFiles {
			name := m.downloaded(f)
			if name == "" {
				continue
			}
			patterns := keep[name]
			mod := escapePattern(unescapeModule(name))
			rel := strings.TrimPrefix(f, name+"/")
			if rel == "**" {
				whole[name] = true
			} else if dir, ok := strings.CutSuffix(rel, "/**"); ok {
				patterns = appendUniq(patterns, "!"+mod+"/"+escapePattern(dir)+"/**")
			} else if testExcluded(rel) {
				patterns = appendUniq(patterns, "!"+mod+"/"+escapePattern(rel))
			}
			keep[name] = patterns
		}
	}
	m.exclude = map[string][]string{}
	for name, patterns := range keep {
		if whole[name] {
			continue
		}
		sort.Strings(patterns)
		m.exclude[name] = append(testExclude(unescapeModule(name)), patterns...)
	}
}

// testExcluded reports whether testExclude leaves out rel, a path in a
// module: a test file, or a file in a testdata directory, or in a
// directory named like a test file.
func testExcluded(rel string) bool {
	for _, elem := range strings.Split(rel, "/") {
		if elem == "testdata" || strings.HasSuffix(elem, "_test.go") {
			return true
		}
	}
	return false
}

// escapePattern escapes what a path pattern would read as a wildcard, or as
// an escape, so the pattern matches p alone.
func escapePattern(p string) string {
	var b strings.Builder
	for _, c := range p {
		if strings.ContainsRune(`*?[]\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
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
		ctr = ctr.WithMountedDirectory(m.root, fileTree(m.all, loose), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	sort.Strings(names)
	for _, name := range names {
		ctr = ctr.WithMountedDirectory(path.Join(m.root, name), m.dir(name), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	return ctr
}

// fetch downloads every module blocks read now, a few at once, so a
// package doesn't wait for its modules when its turn comes. The modules of
// the highest packages (heights) go first, as the packages do. An error
// shows again where the module is used.
func (m *modules) fetch(ctx context.Context, blocks map[string]*block, heights map[string]int64) {
	need := map[string]int64{}
	for id, b := range blocks {
		for _, f := range b.modFiles {
			if name := m.downloaded(f); name != "" {
				need[name] = max(need[name], heights[id])
			}
		}
	}
	names := byHeight(need)
	queue := make(chan string, len(names))
	for _, name := range names {
		queue <- name
	}
	close(queue)
	for range min(16, len(names)) {
		go func() {
			for name := range queue {
				// The synced directory carries its ID, so a package that
				// mounts it doesn't ask for the ID in its slot.
				if d, err := m.dir(name).Sync(ctx); err == nil {
					m.setReady(name, d)
				}
			}
		}()
	}
}

// setReady records d, synced, as the module name's directory.
func (m *modules) setReady(name string, d *dagger.Directory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[name] = d
	if m.ready == nil {
		m.ready = map[string]bool{}
	}
	m.ready[name] = true
}

// byHeight is need's modules, the one needed by the highest package first.
func byHeight(need map[string]int64) []string {
	names := make([]string, 0, len(need))
	for name := range need {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if need[names[i]] != need[names[j]] {
			return need[names[i]] > need[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}

// moduleSums lists the checksum of every module a module cache holds, by its
// directory name, from the go command's own record of each download.
func moduleSums(ctx context.Context, base *dagger.Container, mods *dagger.Directory) (map[string]string, error) {
	out, err := base.
		WithMountedDirectory("/gobuild/mods", mods).
		WithExec([]string{"sh", "-c", "cd /gobuild/mods/cache/download 2>/dev/null || exit 0; " +
			"find . -name '*.ziphash' | sort | while read -r f; do printf '%s %s\\n' \"$f\" \"$(cat \"$f\")\"; done"}, bare).
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
