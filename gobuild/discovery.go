package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// Discovery is what the discovery call (Discover) found: the modules a
// build downloads and what its plan reads of them.
type Discovery struct {
	// Each downloaded module's directory name in the module cache, in name
	// order.
	Names []string
	// Each one's download (downloadModule), in the same order.
	Downloads []*dagger.Directory
	// Their plan packs, in one directory.
	Packs *dagger.Directory
	// The directories of the module cache whose Go files the plan reads.
	PlanMods []string
	// The import paths discover followed, by the target's build constraints
	// and by none: the caller checks that its source provides none of them.
	Followed     []string
	PlanFollowed []string
}

// Discover downloads, as discover does, the modules a build may read, from
// the import paths of packages outside the build's source that the source's
// packages import (sourceImports.outside), and returns what it found. A
// build calls it through the engine with exactly what discover reads of the
// build, none of its own source, so the engine caches the call: every build
// whose source imports the same packages from outside it, with the same
// go.sums, finds it in one request, instead of two for each module, and an
// edit of the build's own source that changes none of those imports leaves
// it as it is.
func (m *Gobuild) Discover(
	ctx context.Context,
	// The container a public module downloads in.
	public *dagger.Container,
	// The container a private module downloads in, with the caller's
	// credentials.
	// +optional
	private *dagger.Container,
	// The modules private downloads, as GONOPROXY or GOPRIVATE names them.
	// +optional
	privatePatterns string,
	// gohdr, for every machine an engine runs on (hdrTools).
	hdr *dagger.Directory,
	// The caller's GOMODCACHE, a module cache its image may come with.
	gomodcache string,
	// The module versions the build may read, as a go.sum lists them (goSum).
	sums string,
	// The build's target, as `go env GOOS GOARCH CGO_ENABLED GOVERSION`
	// prints it, and its build tags.
	goenv string,
	// +optional
	tags []string,
	// The packages to follow, by the target's build constraints and by none,
	// as sourceImports.outside lists them.
	imports []string,
	planImports []string,
	// The paths of the build's modules that sums may also provide a package
	// of (sourceImports.ownModules), whose packages the call neither
	// downloads nor follows, and reports as followed.
	own []string,
) (*Discovery, error) {
	d := &downloader{public: public, private: private, privatePatterns: privatePatterns, gomodcache: gomodcache,
		sums: parseGoSums([]string{sums}), target: newTarget(goenv, tags), hdr: hdr, own: own}
	// They are followed as from a package of a source that imports them
	// and provides none of them.
	found, err := d.discover(ctx, sourceImports{pkgs: map[string][]string{".": imports}, all: map[string][]string{".": planImports}}, ".")
	if err != nil {
		return nil, err
	}
	out := &Discovery{Names: found.names, Downloads: found.downloads, Packs: found.packs, PlanMods: found.dirs.mods,
		Followed: found.followed, PlanFollowed: found.planFollowed}
	if out.Packs == nil {
		out.Packs = core.NewDirectory()
	}
	return out, nil
}

// goSum is the entries with a zip, as a go.sum lists them: what Discover
// reads of a build's go.sums, the same whatever order and repeats they had.
func goSum(entries []sumEntry) string {
	var b strings.Builder
	for _, e := range entries {
		if e.zip == "" {
			continue
		}
		b.WriteString(e.path + " " + e.version + " " + e.zip + "\n")
		if e.mod != "" {
			b.WriteString(e.path + " " + e.version + "/go.mod " + e.mod + "\n")
		}
	}
	return b.String()
}

// discoverOnce is discover's result for the source's package in pkgDir, or
// every package of the source, found by the discovery call: once per set of
// packages the source imports from outside it, for every build and every
// call of one. goenv and tags are the target's. When a module's package
// imports one of the source's, which discover follows into the source, the
// call couldn't follow it, and discover runs here instead. So it does when a
// dependency's package imports one of the source's modules that go.sum also
// names, which the call doesn't download.
func (d *downloader) discoverOnce(ctx context.Context, toolImage, goenv string, tags []string, src sourceImports, pkgDir string) (*discovered, error) {
	imports, plan, srcDirs := src.outside(pkgDir)
	own := src.ownModules(d.sums)
	opts := dagger.GobuildDiscoverOpts{Tags: tags}
	if d.privatePatterns != "" {
		opts.Private, opts.PrivatePatterns = d.private, d.privatePatterns
	}
	call := dagger.Connect().Gobuild(toolImage).Discover(d.public, d.hdr, d.gomodcache, goSum(d.sums), goenv, imports, plan, own, opts)
	followed, err := call.Followed(ctx)
	if err != nil {
		return nil, err
	}
	planFollowed, err := call.PlanFollowed(ctx)
	if err != nil {
		return nil, err
	}
	if src.providesAny(followed, planFollowed) || slices.ContainsFunc(followed, func(ip string) bool { return inModules(ip, own) }) {
		return d.discover(ctx, src, pkgDir)
	}
	names, err := call.Names(ctx)
	if err != nil {
		return nil, err
	}
	downloads, err := call.Downloads(ctx)
	if err != nil {
		return nil, err
	}
	mods, err := call.PlanMods(ctx)
	if err != nil {
		return nil, err
	}
	if len(downloads) != len(names) {
		return nil, fmt.Errorf("the discovery call downloaded %d modules for %d names", len(downloads), len(names))
	}
	out := &discovered{names: names, zips: map[string]*dagger.File{}, dirs: planDirs{src: srcDirs, mods: mods},
		followed: followed, planFollowed: planFollowed}
	if len(names) == 0 {
		return out, nil
	}
	entries := map[string]sumEntry{}
	for _, e := range d.sums {
		entries[moduleDirName(e)] = e
	}
	for i, name := range names {
		out.downloads = append(out.downloads, &downloads[i])
		out.zips[name] = downloads[i].File(zipPath(entries[name]))
	}
	if out.packs, err = call.Packs().Sync(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// providesAny reports whether the source provides a package the discovery
// call followed as a module's: by followed, an import path discover follows
// into the source, or by planFollowed, one planDirsOf does.
func (src sourceImports) providesAny(followed, planFollowed []string) bool {
	for _, ip := range followed {
		if _, ok := src.dirOf(ip); ok {
			return true
		}
	}
	for _, ip := range planFollowed {
		if len(src.dirsOf(ip)) > 0 {
			return true
		}
	}
	return false
}
