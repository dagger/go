package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
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
// from and how it verifies. Nothing else of a caller's environment changes
// what a download makes, so nothing else is in its key.
var downloadEnv = []string{
	"GOPROXY", "GONOPROXY", "GOPRIVATE", "GONOSUMDB", "GONOSUMCHECK", "GOSUMDB",
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
}

// discover downloads the modules a build of the source may read, as the go
// command's loader finds them, and returns a directory of their plan packs
// and their zips by module cache directory name. It starts from the modules
// that provide the source's imports, seed, and as each download finishes,
// downloads those that provide the module's own imports, until no new one
// turns up: what the build's packages import, whatever their build
// constraints select, but no test's. Only modules whose zip a go.sum names
// are candidates; a module the plan reads that discover missed, such as one
// a replace renames, the plan fetches itself, and the build downloads its
// zip on its own (lazyZip).
func (d *downloader) discover(ctx context.Context, seed []string) (*dagger.Directory, map[string]*dagger.File, error) {
	byPath := map[string][]sumEntry{}
	for _, e := range d.sums {
		if e.zip != "" {
			byPath[e.path] = append(byPath[e.path], e)
		}
	}
	var (
		mu      sync.Mutex
		started = map[string]bool{}
		got     = map[string]*dagger.Directory{}
		entry   = map[string]sumEntry{}
		errs    []error
		wg      sync.WaitGroup
		// Downloads wait on the network more than the CPU.
		gate = make(chan struct{}, 32)
	)
	var follow func(imports []string)
	follow = func(imports []string) {
		for _, ip := range imports {
			for _, e := range providers(byPath, ip) {
				name := moduleDirName(e)
				mu.Lock()
				if started[name] {
					mu.Unlock()
					continue
				}
				started[name] = true
				entry[name] = e
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					gate <- struct{}{}
					dir, err := d.download(e).Sync(ctx)
					var imports string
					if err == nil {
						imports, err = dir.File(packName(e) + ".imports").Contents(ctx)
					}
					<-gate
					mu.Lock()
					if err != nil {
						errs = append(errs, fmt.Errorf("%s@%s: %w", e.path, e.version, err))
						mu.Unlock()
						return
					}
					got[name] = dir
					mu.Unlock()
					follow(strings.Fields(imports))
				}()
			}
		}
	}
	follow(seed)
	wg.Wait()
	if len(errs) > 0 {
		return nil, nil, errs[0]
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	packs := make([]*dagger.File, len(names))
	zips := map[string]*dagger.File{}
	for i, name := range names {
		packs[i] = got[name].File(packName(entry[name]))
		zips[name] = got[name].File(zipPath(entry[name]))
	}
	if len(packs) == 0 {
		return nil, zips, nil
	}
	// One directory of every pack, made in one step: merged one by one,
	// hundreds of them would stack a layer each, past what a mount takes.
	dir, err := core.NewDirectory().WithFiles(".", packs).Sync(ctx)
	return dir, zips, err
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
func downloadModule(ctr *dagger.Container, gomodcache string, e sumEntry) *dagger.Directory {
	q := e.path + "@" + e.version
	meta := strings.TrimSuffix(zipPath(e), ".zip")
	script := "set -e\nmkdir -p " + modCacheDir + " /gobuild/empty\n" +
		"c=" + shQuote(gomodcache) + "\n" +
		"[ ! -d \"$c/cache/download\" ] || [ \"$c\" -ef " + modCacheDir + " ] || export GOPROXY=\"file://$c/cache/download,${GOPROXY:-https://proxy.golang.org,direct}\"\n" +
		"export GOMODCACHE=" + modCacheDir + " GOFLAGS=-modcacherw GOTOOLCHAIN=local GOWORK=off\n" +
		"cd /gobuild/empty\ngo mod download -json " + shQuote(q) + " >/dev/null\n" +
		"[ \"$(cat " + shQuote(modCacheDir+"/"+meta+".ziphash") + ")\" = " + shQuote(e.zip) + " ] || { echo " +
		shQuote(q+": checksum mismatch: go.sum has "+e.zip) + " >&2; exit 1; }\n" +
		"/gobuild/gohdr packmod " + modCacheDir + " " + shQuote(modCacheDir+"/"+packName(e)) + " " +
		shQuote(moduleDirName(e)) + " " + shQuote(meta+".mod") + " " + shQuote(meta+".info") + " " + shQuote(meta+".ziphash") + "\n" +
		// Not part of the module version: its extraction, which a build
		// makes from the zip on its own; the go command's locks; and its
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

// envOf is ctr's environment, as names and values in its order.
func envOf(ctx context.Context, ctr *dagger.Container) ([]string, []string, error) {
	vars, err := ctr.EnvVariables(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, len(vars))
	values := make([]string, len(vars))
	errs := make([]error, len(vars))
	var wg sync.WaitGroup
	for i, v := range vars {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if names[i], errs[i] = v.Name(ctx); errs[i] == nil {
				values[i], errs[i] = v.Value(ctx)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	return names, values, nil
}
