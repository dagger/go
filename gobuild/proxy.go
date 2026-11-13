package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// ModuleProxy downloads every module version a go.sum names, as a build
// downloads it, and returns them in one directory laid out as a Go module
// proxy, for GOPROXY=file://<dir>: a version with a zip checksum as its
// .zip, .mod and .info, one with only a go.mod checksum as its .mod and
// .info. A go command reads them with no module cache of anyone's, and asks
// the next proxy in GOPROXY for what isn't there. It answers only for exact
// versions: the go command's version lists, which a download leaves, list
// only the versions the go.sum names, so a query such as @latest would find
// one of those instead of asking the next proxy, and are left out.
//
// Each version is a download of its own (downloadModule), keyed by its
// path, version and checksums, which every call for that version in the
// same container shares. Unlike a build's, it reads no module cache the
// image comes with, so it doesn't share a build's download. The directory
// is keyed by the go.sum and those downloads. Any download that fails, and any .zip or .mod that doesn't
// match the go.sum, fails the call, so a directory that exists holds every
// version the go.sum names, checked.
func (m *Gobuild) ModuleProxy(
	ctx context.Context,
	// The go.sum whose module versions are downloaded.
	goSum *dagger.File,
	// The image a public module downloads in, as for Binary.
	// +optional
	image string,
	// The custom container a public module downloads in, as for Binary.
	// +optional
	from *dagger.Container,
	// The container module downloads run with, as for Binary.
	// +optional
	downloads *dagger.Container,
) (*dagger.Directory, error) {
	var err error
	if image != "" {
		from = core.NewContainer().From(image)
	}
	if from == nil {
		return nil, errors.New("module proxy: no image or container to download in")
	}
	caller := from
	if downloads != nil {
		// A private module downloads in the caller's container, with its
		// credentials, but without its Go caches' volumes, as in a build.
		if caller, err = withoutGoCaches(ctx, downloads); err != nil {
			return nil, err
		}
	}
	public, err := withEnvOnly(ctx, caller, from, func(name string) bool { return slices.Contains(downloadEnv, name) })
	if err != nil {
		return nil, err
	}
	privatePatterns, err := firstEnv(ctx, caller, "GONOPROXY", "GOPRIVATE")
	if err != nil {
		return nil, err
	}
	platform, err := targetPlatform(ctx, public)
	if err != nil {
		return nil, err
	}
	sums, err := goSum.Contents(ctx)
	if err != nil {
		return nil, err
	}
	// No module cache is read first: the downloads come from GOPROXY alone.
	// The download's own module cache, as the one to read first, is one
	// downloadModule skips.
	d := &downloader{public: public, private: caller, privatePatterns: privatePatterns, gomodcache: modCacheDir,
		sums: parseGoSums([]string{sums}), hdr: hdrTools(m.ToolImage, platform)}
	var entries []sumEntry
	for _, e := range d.sums {
		if e.zip != "" || e.mod != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return core.NewDirectory(), nil
	}
	dirs := make([]*dagger.Directory, len(entries))
	errs := make([]error, len(entries))
	// As many downloads at once as a build's discovery runs: each is an
	// exec of its own, with its own lookups and connections.
	eachBounded(len(entries), downloadConcurrency, func(i int) {
		e := entries[i]
		var dir *dagger.Directory
		if e.zip != "" {
			dir = d.download(e)
		} else {
			dir = d.downloadMeta(e)
		}
		if dirs[i], errs[i] = dir.Sync(ctx); errs[i] != nil {
			errs[i] = fmt.Errorf("%s@%s: %w", e.path, e.version, withStderr(errs[i]))
		}
	})
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("module proxy: %w", err)
	}
	// One exec copies every download's proxy files into one directory,
	// without the zips' hashes and the version lists, and hashes each .mod
	// there, which is checked against the go.sum below.
	ctr := from
	for i, dir := range dirs {
		ctr = ctr.WithMountedDirectory("/gobuild/in/"+strconv.Itoa(i), dir.Directory("cache/download"), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	ctr = ctr.WithExec([]string{"sh", "-c", `set -e
mkdir -p /gobuild/proxy
for d in /gobuild/in/*; do cp -R "$d/." /gobuild/proxy/; done
cd /gobuild/proxy
find . \( -name '*.ziphash' -o -name list \) -delete
find . -name '*.mod' -exec sha256sum {} + > /gobuild/modsums`}, bare)
	modsums, err := ctr.File("/gobuild/modsums").Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("module proxy: %w", withStderr(err))
	}
	if err := checkGoModSums(entries, modsums); err != nil {
		return nil, fmt.Errorf("module proxy: %w", err)
	}
	return ctr.Directory("/gobuild/proxy"), nil
}

// withoutGoCaches is ctr without the volumes of its Go caches, the module
// cache and the build cache, as a build drops them (newBuild).
func withoutGoCaches(ctx context.Context, ctr *dagger.Container) (*dagger.Container, error) {
	caches := map[string]bool{"/go/pkg/mod": true, "/root/.cache/go-build": true}
	for _, name := range []string{"GOMODCACHE", "GOCACHE"} {
		v, err := ctr.EnvVariable(ctx, name)
		if err != nil {
			return nil, err
		}
		if v != "" {
			caches[v] = true
		}
	}
	mounts, err := ctr.Mounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mounts {
		if caches[m] {
			ctr = ctr.WithoutMount(m)
		}
	}
	return ctr, nil
}

// checkGoModSums checks every entry's go.mod in a module proxy against its
// go.sum checksum, from sha256sum's lines for the proxy's .mod files, as
// paths from its root: the go.mod's h1 hash is the SHA-256 of the line
// "<its SHA-256 in hex>  go.mod\n" (golang.org/x/mod/sumdb/dirhash.Hash1).
func checkGoModSums(entries []sumEntry, modsums string) error {
	have := map[string]string{}
	for _, line := range strings.Split(modsums, "\n") {
		sum, file, ok := strings.Cut(line, "  ")
		if !ok {
			continue
		}
		h := sha256.Sum256([]byte(sum + "  go.mod\n"))
		have[strings.TrimPrefix(file, "./")] = "h1:" + base64.StdEncoding.EncodeToString(h[:])
	}
	var errs []error
	for _, e := range entries {
		if e.mod == "" {
			continue
		}
		file := escapeModule(e.path) + "/@v/" + escapeModule(e.version) + ".mod"
		switch got := have[file]; got {
		case e.mod:
		case "":
			errs = append(errs, fmt.Errorf("%s@%s: no go.mod downloaded", e.path, e.version))
		default:
			errs = append(errs, fmt.Errorf("%s@%s: go.mod checksum mismatch: go.sum has %s, downloaded %s", e.path, e.version, e.mod, got))
		}
	}
	return errors.Join(errs...)
}

// downloadMeta is downloadModule for a module version a go.sum names by
// its go.mod's checksum alone: its go.mod and info, without its zip, which
// no build reads. ModuleProxy checks the go.mod against the go.sum.
func (d *downloader) downloadMeta(e sumEntry) *dagger.Directory {
	ctr := d.public
	if d.privatePatterns != "" && matchPrefixPatterns(d.privatePatterns, e.path) {
		ctr = d.private
	}
	q := e.path + "@" + e.version
	meta := strings.TrimSuffix(zipPath(e), ".zip")
	script := "set -e\nmkdir -p /gobuild/empty\n" +
		"export GOMODCACHE=" + modCacheDir + " GOFLAGS=-modcacherw GOTOOLCHAIN=local GOWORK=off GOSUMDB=off\n" +
		"cd /gobuild/empty\ngo list -m -json " + shQuote(q) + " >/gobuild/meta.json 2>/gobuild/meta.log || { cat /gobuild/meta.json /gobuild/meta.log >&2; exit 1; }\n" +
		"[ -f " + shQuote(modCacheDir+"/"+meta+".mod") + " ] || { echo " + shQuote(q+": no go.mod downloaded") + " >&2; exit 1; }\n" +
		// Only the go.mod and info: not the go command's other records.
		"find " + modCacheDir + " -type f ! -path " + shQuote(modCacheDir+"/"+meta+".mod") + " ! -path " + shQuote(modCacheDir+"/"+meta+".info") + " -delete\n" +
		"find " + modCacheDir + " -mindepth 1 -type d -empty -delete" +
		// The go.mod's checksum is in the key, as downloadModule's is.
		"\n# go.mod " + e.mod
	return ctr.
		WithExec([]string{"sh", "-c", script}, bare).
		Directory(modCacheDir)
}

// eachBounded calls f for 0 to n-1, at most limit at once, and returns when
// every call has.
func eachBounded(n, limit int, f func(i int)) {
	gate := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		gate <- struct{}{}
		go func() {
			defer func() { <-gate; wg.Done() }()
			f(i)
		}()
	}
	wg.Wait()
}
