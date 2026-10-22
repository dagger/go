package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"dagger/gobuild/internal/dagger"
)

// toolsDir is where package execs that need it find the pack tool, which
// the go command runs in-process.
const toolsDir = "/gobuild/tools"

// node is one exec: a block rendered for the walk that runs it.
type node struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	ImportPath string   `json:"importPath"`
	Deps       []string `json:"deps"`
	SrcFiles   []string `json:"srcFiles,omitempty"`
	ModFiles   []string `json:"modFiles,omitempty"`
	Tools      bool     `json:"tools,omitempty"`
	// Gohdr is set when the script runs the helper's tool, as the link
	// does for its build ID.
	Gohdr  bool   `json:"gohdr,omitempty"`
	Output string `json:"output"`
	Script string `json:"script"`
	// Profile is set when the package's compile reads the PGO profile.
	Profile bool `json:"profile,omitempty"`
	// Link is set for the main package, which links.
	Link bool `json:"link,omitempty"`
}

// toNode renders a block for a walk. A package reads its dependencies'
// archives from /work/lib, which holds those and nothing else.
func toNode(b *block, blocks map[string]*block, conc string) *node {
	// Dependencies in the order of their stable names, not of the plan's
	// step numbers, which an edit elsewhere in the plan can renumber.
	deps := append([]string(nil), b.deps...)
	sort.Slice(deps, func(i, j int) bool { return blocks[deps[i]].name < blocks[deps[j]].name })
	n := &node{ID: b.id, Name: b.name, ImportPath: b.importPath, Deps: deps,
		SrcFiles: b.srcFiles, ModFiles: b.modFiles, Output: b.output(),
		Script: b.script(blocks, conc), Profile: b.profile != "", Link: b.isLink}
	n.Tools = strings.Contains(n.Script, toolsDir+"/")
	n.Gohdr = strings.Contains(n.Script, "/gobuild/gohdr ")
	return n
}

// walker runs a set of nodes, one exec each, each after its dependencies.
type walker struct {
	base  *dagger.Container
	tools *dagger.Directory
	src   *dagger.Directory
	// mods is the module cache, evaluated: see fileTree.
	mods  *dagger.Directory
	roots roots
	// heights orders the execs waiting for a slot, as heightsOf describes.
	heights map[string]int64
	// profile is the preprocessed PGO profile, mounted for packages whose
	// compile reads it; nil without one.
	profile *profileOutput
	// testmain is a test binary's _testmain.go, which its main package
	// compiles; nil for a binary.
	testmain *dagger.File
}

// profileOutput is the PGO profile step's output and where scripts read it.
type profileOutput struct {
	dir  *dagger.Directory
	path string
}

// packageConcurrency is how many package execs a walk runs at once.
const packageConcurrency = 16

type result struct {
	file *dagger.File
	// dir is the package's evaluated work directory, which holds file.
	dir  *dagger.Directory
	err  error // set before done closes; dependents read it
	done chan struct{}
}

func (w *walker) walk(ctx context.Context, nodes map[string]*node) (map[string]*result, error) {
	results := map[string]*result{}
	for id := range nodes {
		results[id] = &result{done: make(chan struct{})}
	}
	slots := newGate(packageConcurrency)
	var wg sync.WaitGroup
	for id, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := results[id]
			defer close(r.done)
			ctr := w.base
			var depFiles []*dagger.File
			for _, d := range n.Deps {
				dr, ok := results[d]
				if !ok {
					continue
				}
				<-dr.done
				if dr.err != nil {
					r.err = fmt.Errorf("%s: %w", nodes[d].ImportPath, dr.err)
					return
				}
				depFiles = append(depFiles, dr.file)
			}
			if len(depFiles) > 0 {
				ctr = ctr.WithMountedDirectory("/work/lib", assemble(depFiles))
			}
			if n.Tools {
				ctr = ctr.WithMountedDirectory(toolsDir, w.tools, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if n.Gohdr {
				ctr = ctr.WithMountedFile("/gobuild/gohdr", hdrTool())
			}
			if n.Profile {
				ctr = ctr.WithMountedDirectory(w.profile.path, w.profile.dir, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if n.Link && w.testmain != nil {
				ctr = ctr.WithFile("/work/"+n.Name+"/_testmain.go", w.testmain)
			}
			if len(n.SrcFiles) > 0 {
				srcDir, at := selected(w.src, n.SrcFiles)
				ctr = ctr.WithMountedDirectory(path.Join(w.roots.Src, at), srcDir)
			}
			if len(n.ModFiles) > 0 {
				ctr = ctr.WithMountedDirectory(w.roots.Mod, fileTree(w.mods, n.ModFiles))
			}
			// A script past the kernel's argument-size limit (the link's
			// import config at a large project's scale) goes in as a file.
			var ran *dagger.Container
			if len(n.Script) > 64<<10 {
				ran = ctr.WithNewFile("/gobuild/run.sh", n.Script).WithExec([]string{"sh", "/gobuild/run.sh"}, noNest)
			} else {
				ran = ctr.WithExec([]string{"sh", "-c", n.Script}, noNest)
			}
			slots.acquire(w.heights[id])
			defer slots.release()
			// Evaluate the output directory first, then read the file from
			// it: only a file read from an evaluated directory is keyed by its
			// content, so a package whose archive came out unchanged leaves
			// its dependents' execs cached.
			out, err := ran.Directory("/work/" + n.Name).Sync(ctx)
			if err != nil {
				r.err = err
				return
			}
			r.dir = out
			r.file, r.err = out.File(n.Output).Sync(ctx)
		}()
	}
	wg.Wait()
	var errs []string
	for _, n := range nodes {
		if r := results[n.ID]; r.err != nil {
			errs = append(errs, r.err.Error())
		}
	}
	if len(errs) > 0 {
		// Report each failure once: the package that failed, not every
		// dependent that waited on it.
		sort.Strings(errs)
		return nil, fmt.Errorf("%s", shortest(errs))
	}
	return results, nil
}

// shortest is the failure at the bottom of a chain: a dependent's error
// wraps its dependency's, so the shortest message is the original.
func shortest(errs []string) string {
	s := errs[0]
	for _, e := range errs[1:] {
		if len(e) < len(s) {
			s = e
		}
	}
	return s
}

// assemble returns a directory holding files, one withFiles. That needs
// Dagger v1.0.0-beta.17 or later: before it, withFiles chained one snapshot
// per file, and past roughly 400 the overlay mount's options grew too long
// for the kernel.
func assemble(files []*dagger.File) *dagger.Directory {
	return dag.Directory().WithFiles(".", files)
}

// selected is the part of dir that files name, and where under the root to
// mount it: their deepest common directory, filtered to them. A package's
// files share its own directory as a rule, so selecting costs per package,
// not per tree. A pattern such as "**/*.h" selects from the whole tree.
func selected(dir *dagger.Directory, files []string) (*dagger.Directory, string) {
	at := commonDir(files)
	if at == "" {
		return dir.Filter(dagger.DirectoryFilterOpts{Include: files}), ""
	}
	rel := make([]string, len(files))
	for i, f := range files {
		rel[i] = strings.TrimPrefix(f, at+"/")
	}
	return dir.Directory(at).Filter(dagger.DirectoryFilterOpts{Include: rel}), at
}

// fileTree is a directory holding files from dir, the module cache, at the
// same paths. Each file is read on its own from the evaluated cache, so it
// is keyed by its content, and so is the tree. A selection made from the
// cache directory itself would be keyed by the cache's recipe, which a bump
// of any dependency changes, re-running every dependency package.
func fileTree(dir *dagger.Directory, files []string) *dagger.Directory {
	byDir := map[string][]*dagger.File{}
	for _, f := range files {
		byDir[path.Dir(f)] = append(byDir[path.Dir(f)], dir.File(f))
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	tree := dag.Directory()
	for _, d := range dirs {
		tree = tree.WithFiles(d, byDir[d])
	}
	return tree
}

// expandHeaders replaces each module cache pattern with the files it
// matches, but Go files, so every module file a package reads is named and
// fileTree reads files alone: "<module>/**" for cgo or assembly that may
// include any file in its module, "<dir>/**" for a directory a C compile
// reads.
func expandHeaders(ctx context.Context, mods *dagger.Directory, blocks map[string]*block) error {
	found := map[string][]string{}
	for _, b := range blocks {
		for _, f := range b.modFiles {
			if strings.Contains(f, "*") {
				found[f] = nil
			}
		}
	}
	patterns := make([]string, 0, len(found))
	for pattern := range found {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	matched := make([][]string, len(patterns))
	errs := make([]error, len(patterns))
	var wg sync.WaitGroup
	for i, pattern := range patterns {
		wg.Go(func() {
			dir, glob := splitPattern(pattern)
			hs, err := mods.Directory(dir).Glob(ctx, glob)
			if err != nil {
				errs[i] = fmt.Errorf("module cache %s: %w", dir, err)
				return
			}
			for _, h := range hs {
				// A glob names directories too, with a trailing slash. Go
				// files are named one by one where they're read, so a
				// pattern brings in what C and assembly include.
				if !strings.HasSuffix(h, "/") && path.Ext(h) != ".go" {
					matched[i] = append(matched[i], dir+"/"+h)
				}
			}
		})
	}
	wg.Wait()
	for i, pattern := range patterns {
		if errs[i] != nil {
			return errs[i]
		}
		found[pattern] = matched[i]
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

func commonDir(files []string) string {
	var common []string
	for i, f := range files {
		parts := strings.Split(f, "/")
		parts = parts[:len(parts)-1]
		for j, p := range parts {
			if strings.ContainsAny(p, "*?[") {
				parts = parts[:j]
				break
			}
		}
		if i == 0 {
			common = parts
			continue
		}
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, "/")
}

// heightsOf is each package's priority for an exec slot: its weight plus
// the heaviest chain of packages that import it, so the walk starts first
// what a long chain waits for, as go build's own scheduler does.
func heightsOf(blocks map[string]*block, weights map[string]int64) map[string]int64 {
	dependents := map[string][]string{}
	for id, b := range blocks {
		for _, d := range b.deps {
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
		heights[id] = weights[id] + above
		return heights[id]
	}
	for id := range blocks {
		heightOf(id)
	}
	return heights
}

// gate hands out n slots, each freed slot to the highest waiter.
type gate struct {
	mu      sync.Mutex
	free    int
	waiting []gateWaiter
}

type gateWaiter struct {
	height int64
	ready  chan struct{}
}

func newGate(n int) *gate { return &gate{free: n} }

func (g *gate) acquire(height int64) {
	g.mu.Lock()
	if g.free > 0 {
		g.free--
		g.mu.Unlock()
		return
	}
	ready := make(chan struct{})
	g.waiting = append(g.waiting, gateWaiter{height, ready})
	g.mu.Unlock()
	<-ready
}

func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.waiting) == 0 {
		g.free++
		return
	}
	best := 0
	for i, w := range g.waiting {
		if w.height > g.waiting[best].height {
			best = i
		}
	}
	w := g.waiting[best]
	g.waiting = append(g.waiting[:best], g.waiting[best+1:]...)
	close(w.ready)
}

// closedSet is every block that has property and whose dependencies all
// have it too.
func closedSet(blocks map[string]*block, property func(*block) bool) map[string]bool {
	memo := map[string]bool{}
	var in func(id string) bool
	in = func(id string) bool {
		if v, ok := memo[id]; ok {
			return v
		}
		b := blocks[id]
		v := property(b)
		for _, d := range b.deps {
			if !in(d) {
				v = false
			}
		}
		memo[id] = v
		return v
	}
	set := map[string]bool{}
	for id := range blocks {
		if in(id) {
			set[id] = true
		}
	}
	return set
}

// outsideSet is the packages that read nothing from the source, down to
// their dependencies: the standard library and dependencies.
func outsideSet(blocks map[string]*block) map[string]bool {
	// The PGO profile's preprocessing runs on its own, before anything that
	// reads it, even when the profile is outside the source.
	return closedSet(blocks, func(b *block) bool { return len(b.srcFiles) == 0 && !b.isLink && !b.isProfile })
}
