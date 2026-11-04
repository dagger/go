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

// downloads downloads, once each, the module versions in sums whose zips a
// build may read, and returns a directory of their plan packs and their zips
// by module cache directory name. public runs a public module's download:
// the build's toolchain with downloadEnv alone. private runs one GOPRIVATE or
// GONOPROXY names, in the caller's own container, with its credentials, so
// its download is shared only with builds that have them.
//
// A go.sum also records the go.mod of every version the module graph ever
// looked at; with graph pruning, the plan loads few of them, and fetches
// those itself.
func downloads(ctx context.Context, public, private *dagger.Container, gomodcache string, sums []sumEntry) (*dagger.Directory, map[string]*dagger.File, error) {
	privatePatterns, err := firstEnv(ctx, private, "GOPRIVATE", "GONOPROXY")
	if err != nil {
		return nil, nil, err
	}
	var zips []sumEntry
	for _, e := range sums {
		if e.zip != "" {
			zips = append(zips, e)
		}
	}
	got := make([]*dagger.Directory, len(zips))
	errs := make([]error, len(zips))
	var wg sync.WaitGroup
	// Downloads wait on the network more than the CPU.
	gate := make(chan struct{}, 32)
	for i, e := range zips {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			ctr := public
			if privatePatterns != "" && matchPrefixPatterns(privatePatterns, e.path) {
				ctr = private
			}
			got[i], errs[i] = downloadModule(ctx, ctr, gomodcache, e)
		}()
	}
	wg.Wait()
	packs := make([]*dagger.File, len(zips))
	files := map[string]*dagger.File{}
	for i, e := range zips {
		if errs[i] != nil {
			return nil, nil, fmt.Errorf("%s@%s: %w", e.path, e.version, errs[i])
		}
		name := moduleDirName(e)
		packs[i] = got[i].File(packName(e))
		files[name] = got[i].File(zipPath(e))
	}
	if len(packs) == 0 {
		return nil, files, nil
	}
	// One directory of every pack, made in one step: merged one by one,
	// hundreds of them would stack a layer each, past what a mount takes.
	dir, err := core.NewDirectory().WithFiles(".", packs).Sync(ctx)
	return dir, files, err
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
func downloadModule(ctx context.Context, ctr *dagger.Container, gomodcache string, e sumEntry) (*dagger.Directory, error) {
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
		"find " + modCacheDir + " -mindepth 1 -maxdepth 1 ! -name cache ! -name " + shQuote(packName(e)) + " -exec rm -rf {} +\n" +
		"find " + modCacheDir + " -name '*.lock' -delete\nrm -rf " + modCacheDir + "/cache/download/sumdb " + modCacheDir + "/cache/vcs" +
		// The go.mod's checksum is in the key too, as a comment: the plan
		// verifies the go.mod against it, so a download is the same input
		// only for the same pair of checksums.
		"\n# go.mod " + e.mod
	return ctr.
		WithMountedFile("/gobuild/gohdr", hdrTool(), dagger.ContainerWithMountedFileOpts{ReadOnly: true}).
		WithExec([]string{"sh", "-c", script}, bare).
		Directory(modCacheDir).Sync(ctx)
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
