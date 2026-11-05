package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	sdk "dagger.io/dagger"
	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
	"github.com/dagger/querybuilder"
)

// A build's module cache is assembled from one download per module version,
// not downloaded whole per project: every go.sum in the source names the
// module versions the build may read, each with its checksum, and each is
// downloaded on its own, keyed by its path, version and checksums. Every
// project, and every caller, that reads a module version then shares its
// download, as builds sharing one module cache volume share it, but as
// results: verified, persisted and pruned like any other, and the same on
// every engine. The plan reads the assembled cache with go.sum, so the go
// command verifies every file it reads against it, as it always does.

// sumEntry is one module version a go.sum names: the checksum of its zip,
// when the go.sum has one, and of its go.mod.
type sumEntry struct {
	path, version string
	zip, mod      string
}

// parseGoSums reads go.sum files' contents into their entries, by module
// version, in a stable order. A line it doesn't understand is left out: the
// go command, which verifies against the go.sum itself, reports it.
func parseGoSums(contents []string) []sumEntry {
	byKey := map[string]*sumEntry{}
	for _, c := range contents {
		for _, line := range strings.Split(c, "\n") {
			f := strings.Fields(line)
			if len(f) != 3 || !strings.HasPrefix(f[2], "h1:") {
				continue
			}
			version, isMod := strings.CutSuffix(f[1], "/go.mod")
			key := f[0] + "@" + version
			e := byKey[key]
			if e == nil {
				e = &sumEntry{path: f[0], version: version}
				byKey[key] = e
			}
			if isMod {
				e.mod = f[2]
			} else {
				e.zip = f[2]
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]sumEntry, len(keys))
	for i, k := range keys {
		out[i] = *byKey[k]
	}
	return out
}

// escapeModule is a module path or version as the module cache names it: an
// upper-case letter as "!" and the letter in lower case.
func escapeModule(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// matchPrefixPatterns reports whether a glob pattern in the comma-separated
// list, as GOPRIVATE and GONOPROXY take them, matches a prefix of the
// module path, element by element, as the go command matches them.
func matchPrefixPatterns(patterns, target string) bool {
	for _, p := range strings.Split(patterns, ",") {
		p = strings.TrimSuffix(strings.TrimSpace(p), "/")
		if p == "" {
			continue
		}
		n := strings.Count(p, "/")
		prefix := target
		for i := 0; i < len(target); i++ {
			if target[i] == '/' {
				if n == 0 {
					prefix = target[:i]
					break
				}
				n--
			}
		}
		if n > 0 {
			continue
		}
		if ok, _ := path.Match(p, prefix); ok {
			return true
		}
	}
	return false
}

// downloadEnv is the environment a module download reads: where it fetches
// from. It verifies against go.sum alone, not the checksum database, so
// nothing else of a caller's environment changes what a download makes,
// and nothing else is in its key.
var downloadEnv = []string{
	"GOPROXY", "GONOPROXY", "GOPRIVATE",
	"GOINSECURE", "GOVCS", "GOAUTH", "GIT_SSH_COMMAND", "GIT_TERMINAL_PROMPT",
}

// downloader downloads, once each, the module versions in sums whose zips a
// build reads. public runs a public module's download: the build's toolchain
// with downloadEnv alone. private runs one GOPRIVATE or GONOPROXY names, in
// the caller's own container, with its credentials, so its download is
// shared only with builds that have them.
type downloader struct {
	public, private *dagger.Container
	privatePatterns string
	gomodcache      string
	sums            []sumEntry
	// target is what discover evaluates build constraints against.
	target target
}

// discover downloads the modules a build of the source may read, as the go
// command's loader finds them, and returns a directory of their plan packs,
// their zips by module cache directory name, and their downloads in name
// order. It follows packages: from
// the package in the source's directory pkgDir, or with none, every package
// of the source, through the source's own packages, each imported package's
// module is downloaded,
// and its pack lists the package's own imports, which it follows in turn,
// until no new package turns up: what the build's packages import, whatever
// their build constraints select, but not the rest of their modules, nor
// any test's. Only modules whose zip a go.sum names are candidates; a module
// the plan reads that discover missed, such as one a replace renames, the
// plan fetches itself, and the build downloads its zip on its own (lazyZip).
// With them come the directories whose Go files the plan reads (planDirs).
func (d *downloader) discover(ctx context.Context, src sourceImports, pkgDir string) (*dagger.Directory, map[string]*dagger.File, []*dagger.Directory, planDirs, error) {
	byPath := map[string][]sumEntry{}
	for _, e := range d.sums {
		if e.zip != "" {
			byPath[e.path] = append(byPath[e.path], e)
		}
	}
	type module struct {
		e    sumEntry
		dir  *dagger.Directory
		done chan struct{}
		err  error
		// imports is each of its packages' imports, by import path.
		imports map[string][]string
		pkgs    modulePackages
	}
	var (
		mu       sync.Mutex
		modules  = map[string]*module{}
		seenPkgs = map[string]bool{}
		seenDirs = map[string]bool{}
		wg       sync.WaitGroup
		// Downloads wait on the network more than the CPU.
		gate = make(chan struct{}, 32)
	)
	start := func(e sumEntry) *module {
		name := moduleDirName(e)
		if m, ok := modules[name]; ok {
			return m
		}
		m := &module{e: e, done: make(chan struct{})}
		modules[name] = m
		go func() {
			defer close(m.done)
			gate <- struct{}{}
			defer func() { <-gate }()
			dir, err := d.download(e).Sync(ctx)
			var list string
			if err == nil {
				list, err = dir.File(packName(e) + ".imports").Contents(ctx)
			}
			if err != nil {
				m.err = fmt.Errorf("%s@%s: %w", e.path, e.version, withStderr(err))
				return
			}
			m.dir = dir
			m.imports = map[string][]string{}
			m.pkgs = modulePackages{all: map[string][]string{}, dirs: map[string]string{}}
			prefix := moduleDirName(e)
			importPath := func(dir string) string {
				// Only the module path and version are escaped in the
				// module cache; its directories are as the module has them.
				if rel := strings.TrimPrefix(strings.TrimPrefix(dir, prefix), "/"); rel != "" {
					return e.path + "/" + rel
				}
				return e.path
			}
			for dir, imports := range fileImportsFor(d.target, list) {
				m.imports[importPath(dir)] = imports
			}
			for dir, imports := range fileImportsFor(target{}, list) {
				m.pkgs.all[importPath(dir)] = imports
				m.pkgs.dirs[importPath(dir)] = dir
			}
		}()
		return m
	}
	var follow func(ips []string)
	var followSource func(dir string)
	followSource = func(dir string) {
		mu.Lock()
		if seenDirs[dir] {
			mu.Unlock()
			return
		}
		seenDirs[dir] = true
		mu.Unlock()
		follow(src.pkgs[dir])
	}
	follow = func(ips []string) {
		for _, ip := range ips {
			if dir, ok := src.dirOf(ip); ok {
				followSource(dir)
				continue
			}
			mu.Lock()
			if seenPkgs[ip] {
				mu.Unlock()
				continue
			}
			seenPkgs[ip] = true
			var ms []*module
			for _, e := range providers(byPath, ip) {
				ms = append(ms, start(e))
			}
			mu.Unlock()
			for _, m := range ms {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-m.done
					if m.err == nil {
						follow(m.imports[ip])
					}
				}()
			}
		}
	}
	if _, ok := src.pkgs[path.Clean(pkgDir)]; pkgDir != "" && ok {
		followSource(path.Clean(pkgDir))
	} else {
		for dir := range src.pkgs {
			if !inTestdata(dir) {
				followSource(dir)
			}
		}
	}
	wg.Wait()
	names := make([]string, 0, len(modules))
	for name, m := range modules {
		if m.err != nil {
			return nil, nil, nil, planDirs{}, m.err
		}
		names = append(names, name)
	}
	pkgs := map[string]modulePackages{}
	for name, m := range modules {
		pkgs[name] = m.pkgs
	}
	dirs := planDirsOf(src, pkgDir, byPath, pkgs)
	sort.Strings(names)
	packs := make([]*dagger.File, len(names))
	zips := map[string]*dagger.File{}
	downloads := make([]*dagger.Directory, len(names))
	for i, name := range names {
		m := modules[name]
		packs[i] = m.dir.File(packName(m.e))
		zips[name] = m.dir.File(zipPath(m.e))
		downloads[i] = m.dir
	}
	if len(packs) == 0 {
		return nil, zips, downloads, dirs, nil
	}
	// One directory of every pack, made in one step: merged one by one,
	// hundreds of them would stack a layer each, past what a mount takes.
	dir, err := core.NewDirectory().WithFiles(".", packs).Sync(ctx)
	return dir, zips, downloads, dirs, err
}

// inTestdata reports whether a source directory is in a testdata directory,
// whose packages a build of every package leaves out, as "./..." does.
func inTestdata(dir string) bool {
	return dir == "testdata" || strings.HasPrefix(dir, "testdata/") || strings.Contains(dir, "/testdata/") || strings.HasSuffix(dir, "/testdata")
}

// planDirs is the directories whose Go files a plan reads, of the source and
// of the module cache, each relative to it: a plan unpacks the headers of the
// Go files there alone, of thousands in a large source and its modules.
type planDirs struct {
	src, mods []string
}

// modulePackages is a downloaded module's packages: each one's imports,
// whatever its files' build constraints, and its directory in the module
// cache, by import path.
type modulePackages struct {
	all  map[string][]string
	dirs map[string]string
}

// planDirsOf is the directories a plan reads: of the packages imports reach
// from the package in pkgDir, or every package of the source, through the
// source's packages and the downloaded modules', pkgs by module cache
// directory name, followed whatever their files' build constraints, so
// never fewer than the go command loads for the build's target.
func planDirsOf(src sourceImports, pkgDir string, byPath map[string][]sumEntry, pkgs map[string]modulePackages) planDirs {
	var dirs planDirs
	seenSrc, seenPkg := map[string]bool{}, map[string]bool{}
	var queue []string
	addSource := func(dir string) {
		if !seenSrc[dir] {
			seenSrc[dir] = true
			dirs.src = append(dirs.src, dir)
			queue = append(queue, src.all[dir]...)
		}
	}
	if _, ok := src.all[path.Clean(pkgDir)]; pkgDir != "" && ok {
		addSource(path.Clean(pkgDir))
	} else {
		for dir := range src.all {
			if !inTestdata(dir) {
				addSource(dir)
			}
		}
	}
	for len(queue) > 0 {
		ip := queue[0]
		queue = queue[1:]
		if seenPkg[ip] {
			continue
		}
		seenPkg[ip] = true
		if srcDirs := src.dirsOf(ip); len(srcDirs) > 0 {
			for _, dir := range srcDirs {
				addSource(dir)
			}
			continue
		}
		for _, e := range providers(byPath, ip) {
			if dir, ok := pkgs[moduleDirName(e)].dirs[ip]; ok {
				dirs.mods = append(dirs.mods, dir)
				queue = append(queue, pkgs[moduleDirName(e)].all[ip]...)
			}
		}
	}
	sort.Strings(dirs.src)
	sort.Strings(dirs.mods)
	return dirs
}

// sourceImports is what the source's packages import, from "gohdr imports":
// its modules' paths by their directories, and each package's imports by its
// directory, both relative to the source.
type sourceImports struct {
	mods []sourceModule
	// repls is each directory of the source that a go.mod replaces a module
	// with, by the module's path.
	repls []sourceModule
	pkgs  map[string][]string
	// all is each package's imports whatever its files' build constraints.
	all map[string][]string
}

type sourceModule struct{ dir, path string }

// parseSourceImports parses "gohdr imports": its module lines, and its Go
// files' lines (importLine), of the files that build for t, and of all.
func parseSourceImports(t target, out string) sourceImports {
	src := sourceImports{pkgs: fileImportsFor(t, out), all: fileImportsFor(target{}, out)}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "mod" {
			src.mods = append(src.mods, sourceModule{dir: f[1], path: f[2]})
		} else if len(f) == 3 && f[0] == "repl" {
			src.repls = append(src.repls, sourceModule{dir: f[1], path: f[2]})
		}
	}
	// The longest module path first, so a nested module wins.
	sort.Slice(src.mods, func(i, j int) bool { return len(src.mods[i].path) > len(src.mods[j].path) })
	return src
}

// dirOf is the source directory of the package with import path ip, when a
// module of the source provides it.
func (src sourceImports) dirOf(ip string) (string, bool) {
	for _, m := range src.mods {
		if ip != m.path && !strings.HasPrefix(ip, m.path+"/") {
			continue
		}
		dir := path.Join(m.dir, strings.TrimPrefix(strings.TrimPrefix(ip, m.path), "/"))
		_, ok := src.pkgs[dir]
		return dir, ok
	}
	return "", false
}

// dirsOf is every source directory that may hold the package with import
// path ip, by any module of the source whose path is a prefix of it, or any
// directory a go.mod replaces such a module with, whatever module path its
// own go.mod declares.
func (src sourceImports) dirsOf(ip string) []string {
	var dirs []string
	for _, m := range slices.Concat(src.mods, src.repls) {
		if ip != m.path && !strings.HasPrefix(ip, m.path+"/") {
			continue
		}
		dir := path.Join(m.dir, strings.TrimPrefix(strings.TrimPrefix(ip, m.path), "/"))
		if _, ok := src.all[dir]; ok {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// withStderr is err with what a failed exec wrote to stderr, or with none
// there, to stdout, where "go mod download -json" reports why a download
// failed: what the exec's error leaves out.
func withStderr(err error) error {
	var execErr *dagger.ExecError
	if !errors.As(err, &execErr) {
		return err
	}
	out := strings.TrimSpace(execErr.Stderr)
	if out == "" {
		out = strings.TrimSpace(execErr.Stdout)
	}
	if out == "" {
		return err
	}
	if len(out) > 2048 {
		out = out[:2048] + "..."
	}
	return fmt.Errorf("%w: %s", err, out)
}

// lazyZip is the zip of the module version with the module cache directory
// name, downloaded on its own; nil for one no go.sum names a zip for.
func (d *downloader) lazyZip(name string) *dagger.File {
	for _, e := range d.sums {
		if e.zip != "" && moduleDirName(e) == name {
			return d.download(e).File(zipPath(e))
		}
	}
	return nil
}

// providers is the module versions that may provide the package with import
// path ip: those of the longest module path that is ip or a prefix of it by
// path elements. A standard library package, whose first element has no
// dot, has none.
func providers(byPath map[string][]sumEntry, ip string) []sumEntry {
	first, _, _ := strings.Cut(ip, "/")
	if !strings.Contains(first, ".") {
		return nil
	}
	for p := ip; ; {
		if es, ok := byPath[p]; ok {
			return es
		}
		i := strings.LastIndex(p, "/")
		if i < 0 {
			return nil
		}
		p = p[:i]
	}
}

// download is the download of one module version (downloadModule), in the
// container its path calls for.
func (d *downloader) download(e sumEntry) *dagger.Directory {
	ctr := d.public
	if d.privatePatterns != "" && matchPrefixPatterns(d.privatePatterns, e.path) {
		ctr = d.private
	}
	return downloadModule(ctr, d.gomodcache, e)
}

// moduleDirName is a module version's directory name in the module cache.
func moduleDirName(e sumEntry) string {
	return escapeModule(e.path) + "@" + escapeModule(e.version)
}

// zipPath is where a module version's zip is in the module cache.
func zipPath(e sumEntry) string {
	return "cache/download/" + escapeModule(e.path) + "/@v/" + escapeModule(e.version) + ".zip"
}

// packName is the file name of a module version's plan pack: its directory
// name, which no other module version has, with no "/".
func packName(e sumEntry) string {
	return strings.ReplaceAll(moduleDirName(e), "/", "%") + ".gohdr"
}

// downloadModule downloads one module version into an empty module cache and
// returns it: its zip, checked against the go.sum checksum, and its go.mod
// and info, and beside them its plan pack, what a plan reads of the module
// (gohdr packmod): its Go files' headers, its other files whole, and its
// go.mod, info and checksum, so a plan that unpacks it sees the module
// downloaded and extracted. A module cache the toolchain image comes with,
// filled for offline builds, is read first. Its key is the module version
// and its checksums, so every project that reads it shares it.
//
// The go.sum checksum is what verifies it, as go build verifies a module
// go.sum names, without the checksum database; so the download doesn't
// ask the database either, which would take most of a small module's
// download. Nor is the zip extracted, which takes most of a large
// one's, and which the build makes from the zip on its own: the module's
// directory is there before the go command runs, so it takes the module as
// extracted once it has the zip and its checksum, and gohdr packs the
// module from the zip.
func downloadModule(ctr *dagger.Container, gomodcache string, e sumEntry) *dagger.Directory {
	q := e.path + "@" + e.version
	meta := strings.TrimSuffix(zipPath(e), ".zip")
	script := "set -e\nmkdir -p " + shQuote(modCacheDir+"/"+moduleDirName(e)) + " /gobuild/empty\n" +
		"c=" + shQuote(gomodcache) + "\n" +
		"[ ! -d \"$c/cache/download\" ] || [ \"$c\" -ef " + modCacheDir + " ] || export GOPROXY=\"file://$c/cache/download,${GOPROXY:-https://proxy.golang.org,direct}\"\n" +
		"export GOMODCACHE=" + modCacheDir + " GOFLAGS=-modcacherw GOTOOLCHAIN=local GOWORK=off GOSUMDB=off\n" +
		"cd /gobuild/empty\ngo mod download -json " + shQuote(q) + " >/gobuild/download.json || { cat /gobuild/download.json >&2; exit 1; }\n" +
		"[ \"$(cat " + shQuote(modCacheDir+"/"+meta+".ziphash") + ")\" = " + shQuote(e.zip) + " ] || { echo " +
		shQuote(q+": checksum mismatch: go.sum has "+e.zip) + " >&2; exit 1; }\n" +
		"/gobuild/gohdr packmod " + modCacheDir + " " + shQuote(modCacheDir+"/"+packName(e)) + " " +
		shQuote(zipPath(e)) + " " + shQuote(moduleDirName(e)) + " " + shQuote(meta+".mod") + " " + shQuote(meta+".info") + " " + shQuote(meta+".ziphash") + "\n" +
		// Not part of the module version: its directory, which a build
		// extracts from the zip on its own; the go command's locks; and its
		// record of the queries that found it.
		"find " + modCacheDir + " -mindepth 1 -maxdepth 1 ! -name cache ! -name " + shQuote(packName(e)) + " ! -name " + shQuote(packName(e)+".imports") + " -exec rm -rf {} +\n" +
		"find " + modCacheDir + " -name '*.lock' -delete\nrm -rf " + modCacheDir + "/cache/download/sumdb " + modCacheDir + "/cache/vcs" +
		// The go.mod's checksum is in the key too, as a comment: the plan
		// verifies the go.mod against it, so a download is the same input
		// only for the same pair of checksums.
		"\n# go.mod " + e.mod
	return ctr.
		WithMountedFile("/gobuild/gohdr", hdrTool(), dagger.ContainerWithMountedFileOpts{ReadOnly: true}).
		WithExec([]string{"sh", "-c", script}, bare).
		Directory(modCacheDir)
}

// firstEnv is the first of ctr's named environment variables that is set.
func firstEnv(ctx context.Context, ctr *dagger.Container, names ...string) (string, error) {
	for _, n := range names {
		v, err := ctr.EnvVariable(ctx, n)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	return "", nil
}

// withEnvOnly is from with only the named variables of ctr's environment, and
// from's own.
func withEnvOnly(ctx context.Context, ctr, from *dagger.Container, keep func(name string) bool) (*dagger.Container, error) {
	names, values, err := envOf(ctx, ctr)
	if err != nil {
		return nil, err
	}
	out := from
	for i, n := range names {
		if keep(n) {
			out = out.WithEnvVariable(n, values[i])
		}
	}
	return out, nil
}

// envOf is ctr's environment, as names and values in its order, read in one
// request. The generated API lists a container's variables by ID only, and
// reading each one's name and value through it takes two requests more per
// variable.
func envOf(ctx context.Context, ctr *dagger.Container) ([]string, []string, error) {
	id, err := ctr.ID(ctx)
	if err != nil {
		return nil, nil, err
	}
	var vars []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := querybuilder.Query().Client(sdk.DefaultGraphQLClient()).
		Select("node").Arg("id", id).InlineFragment("Container").
		Select("envVariables").SelectMultiple("name", "value").
		Bind(&vars).
		Execute(ctx); err != nil {
		return nil, nil, err
	}
	names := make([]string, len(vars))
	values := make([]string, len(vars))
	for i, v := range vars {
		names[i], values[i] = v.Name, v.Value
	}
	return names, values, nil
}
