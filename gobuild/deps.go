package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	w := &walker{base: base, platform: platform, hdr: hdr, tools: tools, mods: mods, roots: roots{Mod: modRoot},
		heights: nodeHeights(nodes)}
	results, err := w.walk(ctx, nodes, nil)
	if err != nil {
		return nil, err
	}
	out := &DependencyBuilds{}
	for _, n := range pkgs {
		r := results[n.ID]
		out.Archives = append(out.Archives, r.file)
		out.Dirs = append(out.Dirs, r.dir)
	}
	return out, nil
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

// dependencies builds the walk's packages in set through the dependency
// call and returns their results, by id, for the walk's other packages to
// import.
func (b *build) dependencies(ctx context.Context, w *walker, nodes map[string]*node, set map[string]bool, weights map[string]int64) (map[string]*result, error) {
	pkgs, ids := dependencyNodes(nodes, set, weights)
	if len(pkgs) == 0 {
		return nil, nil
	}
	var names []string
	for _, n := range pkgs {
		for _, f := range n.ModFiles {
			names = appendUniq(names, w.mods.downloaded(f))
		}
	}
	sort.Strings(names)
	dirs, err := w.mods.synced(ctx, names)
	if err != nil {
		return nil, err
	}
	js, err := json.Marshal(pkgs)
	if err != nil {
		return nil, err
	}
	call := dagger.Connect().Gobuild(b.toolImage).Dependencies(w.base, w.hdr, w.tools, w.platform, b.roots.Mod, names, dirs, string(js))
	archives, err := call.Archives(ctx)
	if err != nil {
		return nil, execFailure(err)
	}
	outs, err := call.Dirs(ctx)
	if err != nil {
		return nil, err
	}
	if len(archives) != len(ids) || len(outs) != len(ids) {
		return nil, fmt.Errorf("the dependency call built %d archives and %d directories for %d packages", len(archives), len(outs), len(ids))
	}
	// The call returns no object code, so it is the same call whether the
	// build links or not: a build's archives and its binary make one call.
	// A build that links reads each package's object code from its
	// directory here, keyed by its content, while its own packages compile.
	links := false
	for _, n := range nodes {
		links = links || n.Link
	}
	done := map[string]*result{}
	for i, id := range ids {
		r := &result{file: &archives[i], dir: &outs[i], done: make(chan struct{}), linkDone: make(chan struct{})}
		close(r.done)
		switch {
		case !links:
			close(r.linkDone)
		case pkgs[i].LinkOutput == pkgs[i].Output:
			r.link = r.file
			close(r.linkDone)
		default:
			go func() {
				defer close(r.linkDone)
				r.link, r.linkErr = r.dir.File(pkgs[i].LinkOutput).Sync(ctx, core.FileSyncOpts{KeyByContent: true})
			}()
		}
		done[id] = r
	}
	return done, nil
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
