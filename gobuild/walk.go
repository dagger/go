package main

import (
	"context"
	"fmt"
	"path"
	"runtime"
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
	// SrcNonGo is set when the package also reads every file in the
	// source that isn't Go (block.srcNonGo).
	SrcNonGo bool `json:"srcNonGo,omitempty"`
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
		SrcFiles: b.srcFiles, SrcNonGo: b.srcNonGo, ModFiles: b.modFiles, Output: b.output(),
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
	// nonGo is the source's files that aren't Go, which a package with C or
	// assembly there reads (srcNonGo); nil when no package does.
	nonGo *dagger.Directory
	// mods is the module cache's modules, each on its own.
	mods  *modules
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

// packageConcurrency is how many package execs a walk runs at once: 12, or
// one and a half times GOMAXPROCS, if that's fewer. GOMAXPROCS counts the
// machine's CPUs and a CPU limit on gobuild's own container, but not one set
// on the engine's container: Go reads only the cgroup it runs in, and that's
// a child of the engine's. Under such a limit the walk can run up to 12.
// In cold builds of dagger's CLI, running more at once made each compile and
// the engine's work per exec slower, by more than the extra concurrency
// gained: the build was fastest at 12 on 16 and 32 cores, as fast at 12 as at
// any count on 8, and on 4 cores as fast at 3 to 8 as at any count but about
// 4 s slower at 12.
func packageConcurrency() int {
	return min(12, max(1, runtime.GOMAXPROCS(0)*3/2))
}

type result struct {
	file *dagger.File
	// dir is the package's work directory, which holds file: evaluated, but
	// for the link's.
	dir  *dagger.Directory
	err  error // set before done closes; dependents read it
	done chan struct{}
}

func (w *walker) walk(ctx context.Context, nodes map[string]*node) (map[string]*result, error) {
	results := map[string]*result{}
	for id := range nodes {
		results[id] = &result{done: make(chan struct{})}
	}
	limit := packageConcurrency()
	first := firstSlots(nodes, w.heights, limit)
	slots := newGate(limit - len(first))
	var wg sync.WaitGroup
	for id, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := results[id]
			defer close(r.done)
			// The source a package reads doesn't depend on its imports, so its
			// ID is fetched while they build rather than in the package's
			// slot. An error shows again where it's mounted.
			var srcDir *dagger.Directory
			var at string
			if len(n.SrcFiles) > 0 {
				srcDir, at = selected(w.src, n.SrcFiles)
				if n.SrcNonGo {
					srcDir = w.nonGo.WithDirectory(path.Join(".", at), srcDir)
					at = ""
				}
				srcDir.ID(ctx)
			}
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
			// A package's exec writes only its own directory. Its inputs are
			// mounted read-only: a writable mount is one of the exec's
			// outputs, a copy made for it to write and kept after.
			if len(depFiles) > 0 {
				ctr = ctr.WithMountedDirectory("/work/lib", assemble(depFiles), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if n.Tools {
				ctr = ctr.WithMountedDirectory(toolsDir, w.tools, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if n.Gohdr {
				ctr = ctr.WithMountedFile("/gobuild/gohdr", hdrTool(), dagger.ContainerWithMountedFileOpts{ReadOnly: true})
			}
			if n.Profile {
				ctr = ctr.WithMountedDirectory(w.profile.path, w.profile.dir, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if n.Link && w.testmain != nil {
				ctr = ctr.WithFile("/work/"+n.Name+"/_testmain.go", w.testmain)
			}
			if srcDir != nil {
				ctr = ctr.WithMountedDirectory(path.Join(w.roots.Src, at), srcDir, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			ctr = w.mods.mount(ctr, n.ModFiles)
			// A script past the kernel's argument-size limit (the link's
			// import config at a large project's scale) goes in as a file.
			var ran *dagger.Container
			if len(n.Script) > 64<<10 {
				ran = ctr.WithNewFile("/gobuild/run.sh", n.Script).WithExec([]string{"sh", "/gobuild/run.sh"}, bare)
			} else {
				ran = ctr.WithExec([]string{"sh", "-c", n.Script}, bare)
			}
			if !first[id] {
				slots.acquire(w.heights[id])
			}
			defer slots.release()
			if n.Link {
				// No exec reads the link's output, so it is read from the
				// unevaluated directory: keyed by its content, it would cost
				// a hash of the whole binary on every link.
				r.dir = ran.Directory("/work/" + n.Name)
				r.file, r.err = r.dir.File(n.Output).Sync(ctx)
				return
			}
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

// firstSlots are the packages that start with a slot: the n highest of
// those that import nothing the walk builds. The gate ranks only the
// packages waiting for a slot, and at the start only these can ask, so
// without this the first n to ask would get them, whatever their height.
func firstSlots(nodes map[string]*node, heights map[string]int64, n int) map[string]bool {
	leaves := map[string]int64{}
	for id, nd := range nodes {
		leaf := true
		for _, d := range nd.Deps {
			if _, ok := nodes[d]; ok {
				leaf = false
				break
			}
		}
		if leaf {
			leaves[id] = heights[id]
		}
	}
	first := map[string]bool{}
	for _, id := range byHeight(leaves) {
		if len(first) == n {
			break
		}
		first[id] = true
	}
	return first
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

// nonGoSource is the files in src that aren't Go. Leaving out *.go leaves
// out a directory named like a Go file whole, so what such directories hold
// comes back, less goDirGo, the Go files in them, left out by their escaped
// paths in the same filter, which keeps the selection keyed by what it
// holds. A path the filter would trim, with space at either end, isn't left
// out: a Go file kept, as one missing from goDirGo would be, changes keys,
// not what builds.
func nonGoSource(src *dagger.Directory, goDirGo []string) *dagger.Directory {
	var exclude []string
	for _, f := range goDirGo {
		if strings.TrimSpace(f) == f {
			exclude = append(exclude, f)
		}
	}
	return src.Filter(dagger.DirectoryFilterOpts{Exclude: []string{"**/*.go"}}).
		WithDirectory(".", src.Filter(dagger.DirectoryFilterOpts{Include: []string{"**/*.go/**"}, Exclude: literalPatterns(exclude)}))
}

// literalPatterns are filter patterns that each match one path as written,
// with the characters a pattern reads specially escaped.
func literalPatterns(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		var b strings.Builder
		for j, c := range p {
			if strings.ContainsRune(`*?[]^\`, c) || (j == 0 && c == '!') {
				b.WriteByte('\\')
			}
			b.WriteRune(c)
		}
		out[i] = b.String()
	}
	return out
}
