package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// DependencyBuilds is what the dependency call (Dependencies) built, in the
// order of its packages.
type DependencyBuilds struct {
	// Each package's archive, which its importers' compiles read.
	Archives []*dagger.File
	// Each package's work directory, which holds its archive and, when the
	// package splits, the object code a link reads.
	Dirs []*dagger.Directory
}

// Dependencies builds packages of a build that read none of its source and
// import nothing that does, one exec per package as a build does: the
// standard library and the build's dependencies. A build calls it through
// the engine with exactly what those packages read, so the engine caches the
// call: every build that compiles the same packages the same way, whatever
// its own source, finds it in one request, instead of one or two for each
// package, and an edit of the build's own source leaves it as it is.
func (m *Gobuild) Dependencies(
	ctx context.Context,
	// The container each package's exec runs in.
	base *dagger.Container,
	// gohdr, for every machine an engine runs on (hdrTools).
	hdr *dagger.Directory,
	// Go's pack, for every machine an engine runs on.
	tools *dagger.Directory,
	// The platform the packages are compiled for, which their builds are
	// declared for.
	platform string,
	// GOMODCACHE, where each package's exec mounts the modules it reads.
	modRoot string,
	// The modules the packages read, by directory name in the module cache,
	// and each one's directory, in the same order.
	moduleNames []string,
	moduleDirs []*dagger.Directory,
	// The packages, as a JSON list of the walk's nodes, each named and
	// importing by its stable name (dependencyNodes).
	packages string,
	// The packages they import that an earlier dependency call built
	// (sharedImports), by stable name, and each one's archive, in the same
	// order.
	// +optional
	importedNames []string,
	// +optional
	imported []*dagger.File,
) (*DependencyBuilds, error) {
	var pkgs []*node
	if err := json.Unmarshal([]byte(packages), &pkgs); err != nil {
		return nil, fmt.Errorf("dependency packages: %w", err)
	}
	if len(moduleNames) != len(moduleDirs) {
		return nil, fmt.Errorf("%d module names for %d module directories", len(moduleNames), len(moduleDirs))
	}
	mods := &modules{root: modRoot, sums: map[string]string{}, dirs: map[string]*dagger.Directory{}}
	for i, name := range moduleNames {
		// Verified by the caller's build, which downloaded it.
		mods.sums[name] = "verified"
		mods.dirs[name] = moduleDirs[i]
	}
	nodes := map[string]*node{}
	for _, n := range pkgs {
		nodes[n.ID] = n
	}
	if len(importedNames) != len(imported) {
		return nil, fmt.Errorf("%d imported package names for %d archives", len(importedNames), len(imported))
	}
	done := map[string]*result{}
	for i, name := range importedNames {
		r := &result{file: imported[i], done: make(chan struct{}), linkDone: make(chan struct{}), importPath: name}
		close(r.done)
		close(r.linkDone)
		done[name] = r
	}
	w := &walker{base: base, platform: platform, hdr: hdr, tools: tools, mods: mods, roots: roots{Mod: modRoot},
		heights: nodeHeights(nodes)}
	results, err := w.walk(ctx, nodes, done)
	if err != nil {
		return nil, err
	}
	out := &DependencyBuilds{}
	for _, n := range pkgs {
		r := results[n.ID]
		out.Archives = append(out.Archives, r.file)
		out.Dirs = append(out.Dirs, r.dir)
	}
	if err := fetchIDs(ctx, out.Dirs); err != nil {
		return nil, err
	}
	return out, nil
}

// LinkObjects is the object code a link reads of each package in dirs, the
// package directories the dependency call returns: in each, the file named
// in files, in the same order, keyed by its content. A build that links
// calls it through the engine, so a later build of the same dependencies
// finds all of their object code in one request, instead of one for each
// package.
func (m *Gobuild) LinkObjects(
	ctx context.Context,
	// The package directories, as the dependency call returns them.
	dirs []*dagger.Directory,
	// The object code's file in each directory.
	files []string,
) ([]*dagger.File, error) {
	if len(dirs) != len(files) {
		return nil, fmt.Errorf("%d object code files for %d directories", len(files), len(dirs))
	}
	out := make([]*dagger.File, len(dirs))
	errs := make([]error, len(dirs))
	var wg sync.WaitGroup
	for i, d := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = d.File(files[i]).Sync(ctx, core.FileSyncOpts{KeyByContent: true})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchIDs asks for the IDs of dirs, several at a time. Returning an object
// sends its ID, which the SDK asks for one object after another; once asked
// for, an ID is remembered, so the return then asks for none. Each package's
// directory is unevaluated, so asking costs no evaluation of the package.
func fetchIDs(ctx context.Context, dirs []*dagger.Directory) error {
	errs := make([]error, len(dirs))
	gate := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i, d := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			_, errs[i] = d.ID(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// dependencySet is the blocks the dependency call builds: those that read
// nothing from the source (outsideSet), nor any file of the module cache
// outside a downloaded module's directory, whose key is the cache's recipe,
// nor the PGO profile, which comes from the source; down to their
// dependencies.
func dependencySet(blocks map[string]*block, mods *modules) map[string]bool {
	return closedSet(blocks, func(b *block) bool {
		if len(b.srcFiles) > 0 || b.isLink || b.isProfile || b.profile != "" {
			return false
		}
		for _, f := range b.modFiles {
			if mods.downloaded(f) == "" {
				return false
			}
		}
		return true
	})
}

// dependencyNodes is the nodes of set as the dependency call takes them, in
// the order of their stable names: each named by its stable name and
// importing by theirs, with its weight, so the call's arguments are the same
// for every build of the same packages, however the rest of its plan
// numbers its steps. ids is their ids in the same order.
func dependencyNodes(nodes map[string]*node, set map[string]bool, weights map[string]int64) (pkgs []*node, ids []string) {
	for id := range set {
		if _, ok := nodes[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return nodes[ids[i]].Name < nodes[ids[j]].Name })
	for _, id := range ids {
		n := *nodes[id]
		n.ID = n.Name
		n.Deps = make([]string, len(nodes[id].Deps))
		for i, d := range nodes[id].Deps {
			n.Deps[i] = nodes[d].Name
		}
		n.Weight = weights[id]
		pkgs = append(pkgs, &n)
	}
	return pkgs, ids
}

// dependencies builds the walk's packages in set through dependency calls,
// one, or with shared imports one per layer (dependencyLayers), and returns
// their results, by id, for the walk's other packages to import.
func (b *build) dependencies(ctx context.Context, w *walker, nodes map[string]*node, set map[string]bool, weights map[string]int64) (map[string]*result, error) {
	pkgs, ids := dependencyNodes(nodes, set, weights)
	if len(pkgs) == 0 {
		return nil, nil
	}
	archives := make([]dagger.File, len(pkgs))
	outs := make([]dagger.Directory, len(pkgs))
	at := map[string]int{}
	for i, n := range pkgs {
		at[n.Name] = i
	}
	layers, shared := dependencyLayers(pkgs, b.shared)
	for li, layer := range layers {
		call, err := b.dependencyCall(ctx, w, pkgs, layer, archives, at)
		if err != nil {
			return nil, err
		}
		la, err := call.Archives(ctx)
		if err != nil {
			return nil, execFailure(err)
		}
		lo, err := call.Dirs(ctx)
		if err != nil {
			return nil, err
		}
		if len(la) != len(layer) || len(lo) != len(layer) {
			return nil, fmt.Errorf("the dependency call built %d archives and %d directories for %d packages", len(la), len(lo), len(layer))
		}
		for k, i := range layer {
			archives[i], outs[i] = la[k], lo[k]
		}
		if li < shared {
			b.sharedCalls = append(b.sharedCalls, call)
		}
	}
	// The call returns no object code, so it is the same call whether the
	// build links or not: a build's archives and its binary make one call.
	// A build that links reads the object code of the packages that split
	// through the object code call, while its own packages compile.
	links := false
	for _, n := range nodes {
		links = links || n.Link
	}
	done := map[string]*result{}
	var split []*result
	var splitDirs []*dagger.Directory
	var splitFiles []string
	for i, id := range ids {
		r := &result{file: &archives[i], dir: &outs[i], done: make(chan struct{}), linkDone: make(chan struct{}),
			importPath: pkgs[i].ImportPath}
		close(r.done)
		switch {
		case !links:
			close(r.linkDone)
		case pkgs[i].LinkOutput == pkgs[i].Output:
			r.link = r.file
			close(r.linkDone)
		default:
			split = append(split, r)
			splitDirs = append(splitDirs, r.dir)
			splitFiles = append(splitFiles, pkgs[i].LinkOutput)
		}
		done[id] = r
	}
	if len(split) > 0 {
		go func() {
			files, err := dagger.Connect().Gobuild(b.toolImage).LinkObjects(ctx, splitDirs, splitFiles)
			if err == nil && len(files) != len(split) {
				err = fmt.Errorf("the object code call read %d files for %d packages", len(files), len(split))
			}
			for i, r := range split {
				if err != nil {
					r.linkErr = err
				} else {
					r.link = &files[i]
				}
				close(r.linkDone)
			}
		}()
	}
	return done, nil
}

// dependencyCall is the dependency call for the packages of pkgs at layer,
// given the archives earlier calls built of what they import.
func (b *build) dependencyCall(ctx context.Context, w *walker, pkgs []*node, layer []int, archives []dagger.File, at map[string]int) (*dagger.GobuildDependencyBuilds, error) {
	in := map[string]bool{}
	for _, i := range layer {
		in[pkgs[i].Name] = true
	}
	var lpkgs []*node
	var names, importedNames []string
	for _, i := range layer {
		n := pkgs[i]
		lpkgs = append(lpkgs, n)
		for _, d := range n.Deps {
			if !in[d] {
				importedNames = appendUniq(importedNames, d)
			}
		}
		for _, f := range n.ModFiles {
			names = appendUniq(names, w.mods.downloaded(f))
		}
	}
	sort.Strings(importedNames)
	imported := make([]*dagger.File, len(importedNames))
	for k, d := range importedNames {
		imported[k] = &archives[at[d]]
	}
	sort.Strings(names)
	dirs, err := w.mods.synced(ctx, names)
	if err != nil {
		return nil, err
	}
	js, err := json.Marshal(lpkgs)
	if err != nil {
		return nil, err
	}
	return dagger.Connect().Gobuild(b.toolImage).Dependencies(w.base, w.hdr, w.tools, w.platform, b.roots.Mod, names, dirs, string(js),
		dagger.GobuildDependenciesOpts{ImportedNames: importedNames, Imported: imported}), nil
}

// dependencyLayers is the dependency calls a build makes for pkgs, as
// indexes into pkgs, in the order they run: with shared import paths, first
// the standard library's packages they reach, then their other packages,
// then the rest; each layer imports only its own packages and earlier
// layers'. The first n layers are the shared ones. Without shared imports
// that reach any of pkgs, one call for all of them.
//
// What shared imports reach is the same in every build that keeps their
// modules' versions, so each shared layer is the same call there, whatever
// else the build imports. The standard library's packages depend on no
// module version, so a build that changes one still shares the first.
func dependencyLayers(pkgs []*node, shared []string) (layers [][]int, n int) {
	byName := map[string]*node{}
	byPath := map[string]*node{}
	for _, p := range pkgs {
		byName[p.Name] = p
		byPath[p.ImportPath] = p
	}
	reached := map[string]bool{}
	var reach func(p *node)
	reach = func(p *node) {
		if reached[p.Name] {
			return
		}
		reached[p.Name] = true
		for _, d := range p.Deps {
			if dp, ok := byName[d]; ok {
				reach(dp)
			}
		}
	}
	for _, imp := range shared {
		if p, ok := byPath[imp]; ok {
			reach(p)
		}
	}
	var std, other, rest []int
	for i, p := range pkgs {
		switch {
		case !reached[p.Name]:
			rest = append(rest, i)
		case standard(p.ImportPath):
			std = append(std, i)
		default:
			other = append(other, i)
		}
	}
	for _, l := range [][]int{std, other} {
		if len(l) > 0 {
			layers = append(layers, l)
		}
	}
	n = len(layers)
	if len(rest) > 0 {
		layers = append(layers, rest)
	}
	return layers, n
}

// standard reports whether importPath names a package of the standard
// library: its first element has no dot, as the go command decides.
func standard(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// nodeHeights is heightsOf for a walk's nodes alone: each one's weight plus
// the heaviest chain of nodes that import it.
func nodeHeights(nodes map[string]*node) map[string]int64 {
	dependents := map[string][]string{}
	for id, n := range nodes {
		for _, d := range n.Deps {
			dependents[d] = append(dependents[d], id)
		}
	}
	heights := map[string]int64{}
	var heightOf func(id string) int64
	heightOf = func(id string) int64 {
		if v, ok := heights[id]; ok {
			return v
		}
		var above int64
		for _, up := range dependents[id] {
			above = max(above, heightOf(up))
		}
		heights[id] = nodes[id].Weight + above
		return heights[id]
	}
	for id := range nodes {
		heightOf(id)
	}
	return heights
}

// synced is the directories of the named modules, each synced, so it
// carries its ID, in the same order: the dependency call's arguments. A
// module fetch already synced isn't asked for again.
func (m *modules) synced(ctx context.Context, names []string) ([]*dagger.Directory, error) {
	dirs := make([]*dagger.Directory, len(names))
	errs := make([]error, len(names))
	gate := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.mu.Lock()
			d, ok := m.dirs[name], m.ready[name]
			m.mu.Unlock()
			if ok {
				dirs[i] = d
				return
			}
			gate <- struct{}{}
			defer func() { <-gate }()
			d, err := m.dir(name).Sync(ctx)
			if err != nil {
				errs[i] = fmt.Errorf("module %s: %w", name, err)
				return
			}
			m.setReady(name, d)
			dirs[i] = d
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return dirs, nil
}
