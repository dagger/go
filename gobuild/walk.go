package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// toolsDir is where package execs that need it find the pack tool, which
// the go command runs in-process.
const toolsDir = "/gobuild/tools"

// scriptPath is where a package's exec reads its script.
const scriptPath = "/gobuild/build.sh"

// node is one exec: a block rendered for the walk that runs it.
type node struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	ImportPath string   `json:"importPath"`
	Deps       []string `json:"deps"`
	// CompileDeps, for the main package that links, are the dependencies
	// its own compile imports, whose export data it mounts; the link mounts
	// every dependency's object code.
	CompileDeps []string `json:"compileDeps,omitempty"`
	SrcFiles    []string `json:"srcFiles,omitempty"`
	ModFiles    []string `json:"modFiles,omitempty"`
	Tools       bool     `json:"tools,omitempty"`
	// SrcNonGo is set when the package also reads every file in the
	// source that isn't Go (block.srcNonGo).
	SrcNonGo bool `json:"srcNonGo,omitempty"`
	// Gohdr is set when the script runs the helper's tool, as the link
	// does for its build ID.
	Gohdr  bool   `json:"gohdr,omitempty"`
	Output string `json:"output"`
	// LinkOutput is the archive of the package's object code, which a link
	// reads: Output, unless the package splits (block.splits).
	LinkOutput string `json:"linkOutput"`
	Script     string `json:"script"`
	// Profile is set when the package's compile reads the PGO profile.
	Profile bool `json:"profile,omitempty"`
	// Link is set for the main package, which links.
	Link bool `json:"link,omitempty"`
	// Cgo is set when the package runs the cgo tool and the C compiler,
	// whose output depends on the machine's C toolchain: it isn't declared
	// for its target (Directory.withPlatform).
	Cgo bool `json:"cgo,omitempty"`
	// Weight, in the dependency call, is the package's weight (heightsOf),
	// which orders its execs there.
	Weight int64 `json:"weight,omitempty"`
}

// toNode renders a block for a walk. A package reads its dependencies'
// archives from /work/lib, which holds those and nothing else, and a link
// reads their object code from /work/linklib.
func toNode(b *block, blocks map[string]*block, conc string) *node {
	// Dependencies in the order of their stable names, not of the plan's
	// step numbers, which an edit elsewhere in the plan can renumber.
	deps := append([]string(nil), b.deps...)
	sort.Slice(deps, func(i, j int) bool { return blocks[deps[i]].name < blocks[deps[j]].name })
	n := &node{ID: b.id, Name: b.name, ImportPath: b.importPath, Deps: deps,
		SrcFiles: b.srcFiles, SrcNonGo: b.srcNonGo, ModFiles: b.modFiles, Output: b.output(), LinkOutput: b.linkOutput(),
		Script: b.script(blocks, conc), Profile: b.profile != "", Link: b.isLink, Cgo: b.cgo}
	if b.isLink {
		n.CompileDeps = b.compileDeps()
	}
	n.Tools = strings.Contains(n.Script, toolsDir+"/")
	n.Gohdr = strings.Contains(n.Script, hdrBin+" ")
	return n
}

// toNodes renders every block but a PGO profile's for a walk. Rendering
// the scripts is most of gobuild's own time before the walk starts, ~0.15 s
// for dagger's CLI one after another, and each package's is its own: they're
// rendered at once.
func toNodes(blocks map[string]*block, outside map[string]bool, weights map[string]int64) map[string]*node {
	nodes := map[string]*node{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for id, bl := range blocks {
		if bl.isProfile {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := toNode(bl, blocks, compileC(outside[id], weights[id]))
			mu.Lock()
			nodes[id] = n
			mu.Unlock()
		}()
	}
	wg.Wait()
	return nodes
}

// walker runs a set of nodes, one exec each, each after its dependencies.
type walker struct {
	base *dagger.Container
	// platform is the build's target, as a Platform, which each package's
	// output is declared for.
	platform string
	// hdr is gohdr, for every machine an engine runs on (hdrTools).
	hdr *dagger.Directory
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
	// file is the package's archive its importers' compiles read, and link
	// the one a link reads: the same file unless the package splits, when
	// it is read after done closes, by the time linkDone closes, and only
	// in a walk that links, so that importers don't wait for it.
	file     *dagger.File
	link     *dagger.File
	linkErr  error
	linkDone chan struct{}
	// dir is the package's work directory, which holds file.
	dir *dagger.Directory
	// importPath names the package in its dependents' errors: a package
	// the dependency call built is no node of the walk that reads it.
	importPath string
	err        error // set before done closes; dependents read it
	done       chan struct{}
}

// walk runs nodes and returns their results, with done's: the results of
// packages already built, such as the dependency call's, which nodes import.
func (w *walker) walk(ctx context.Context, nodes map[string]*node, done map[string]*result) (map[string]*result, error) {
	results := map[string]*result{}
	for id, r := range done {
		results[id] = r
	}
	links := false
	for id, n := range nodes {
		results[id] = &result{done: make(chan struct{}), linkDone: make(chan struct{}), importPath: n.ImportPath}
		links = links || n.Link
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
			// The source a package reads doesn't depend on its imports, so it
			// is synced while they build rather than in the package's slot.
			// The synced directory carries its ID, so the package's exec
			// doesn't ask for it either. An error shows again where it's
			// mounted.
			var srcDir *dagger.Directory
			var at string
			if len(n.SrcFiles) > 0 {
				srcDir, at = selected(w.src, n.SrcFiles)
				if n.SrcNonGo {
					srcDir = w.nonGo.WithDirectory(path.Join(".", at), srcDir)
					at = ""
				}
				if d, err := srcDir.Sync(ctx); err == nil {
					srcDir = d
				}
			}
			asyncLink := false
			defer func() {
				if !asyncLink {
					close(r.linkDone)
				}
			}()
			ctr := w.base
			var depFiles, linkFiles []*dagger.File
			for _, d := range n.Deps {
				dr, ok := results[d]
				if !ok {
					continue
				}
				<-dr.done
				if dr.err != nil {
					r.err = fmt.Errorf("%s: %w", dr.importPath, dr.err)
					return
				}
				if !n.Link {
					depFiles = append(depFiles, dr.file)
					continue
				}
				<-dr.linkDone
				if dr.linkErr != nil {
					r.err = fmt.Errorf("%s: %w", dr.importPath, dr.linkErr)
					return
				}
				linkFiles = append(linkFiles, dr.link)
			}
			// The link's own compile reads only its imports' export data.
			for _, d := range n.CompileDeps {
				if dr, ok := results[d]; ok {
					depFiles = append(depFiles, dr.file)
				}
			}
			// A package's exec writes only its own directory. Its inputs are
			// mounted read-only: a writable mount is one of the exec's
			// outputs, a copy made for it to write and kept after.
			if len(depFiles) > 0 {
				ctr = ctr.WithMountedFiles("/work/lib", depFiles)
			}
			if n.Link && len(linkFiles) > 0 {
				ctr = ctr.WithMountedFiles("/work/linklib", linkFiles)
			}
			if n.Tools {
				ctr = ctr.WithMountedDirectory(toolsDir, w.tools, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if n.Gohdr {
				ctr = ctr.WithMountedDirectory(hdrDir, w.hdr, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
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
			// The script goes in as a file the step's container writes, and
			// the exec names the package instead: its arguments and options
			// are its title wherever the build is shown, such as over a
			// compile error, and a script runs to tens of KB with the link's
			// import config. Written in the step's own query, the file costs
			// no request of its own, as a file object passed in would, for
			// its ID.
			ran := ctr.
				WithNewFile(scriptPath, n.Script).
				WithExec([]string{"sh", scriptPath, n.ImportPath}, pkgExec)
			if !first[id] {
				slots.acquire(w.heights[id])
			}
			defer slots.release()
			out := ran.Directory("/work/" + n.Name)
			// What a package compiles is the same whichever platform's
			// toolchain compiles it, unless it runs the C compiler.
			if !n.Cgo {
				out = out.WithPlatform(core.Platform(w.platform))
			}
			if n.Link {
				// No exec reads the link's output, so it is read from the
				// unevaluated directory: keyed by its content, it would cost
				// a hash of the whole binary on every link.
				r.dir = out
				r.file, r.err = r.dir.File(n.Output).Sync(ctx)
				r.err = execFailure(r.err)
				return
			}
			// The request that runs the exec reads the archive, keyed by its
			// content, so a package whose archive came out unchanged leaves
			// its dependents' execs cached.
			r.dir = out
			r.file, r.err = out.File(n.Output).Sync(ctx, core.FileSyncOpts{KeyByContent: true})
			r.err = execFailure(r.err)
			switch {
			case r.err != nil || !links:
			case n.LinkOutput == n.Output:
				r.link = r.file
			default:
				asyncLink = true
				go func() {
					defer close(r.linkDone)
					r.link, r.linkErr = out.File(n.LinkOutput).Sync(ctx, core.FileSyncOpts{KeyByContent: true})
				}()
			}
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
	tree := core.NewDirectory()
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

// execFailure is a package exec's failure with what the exec printed: the
// compiler's or the linker's own messages, which they write to stdout, and
// the script's, on stderr. The SDK's error says only the exit code, and the
// exec's output would otherwise reach a user only in the exec's span.
func execFailure(err error) error {
	var execErr *dagger.ExecError
	if !errors.As(err, &execErr) {
		return err
	}
	var out []string
	for _, o := range []string{execErr.Stdout, execErr.Stderr} {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	if len(out) == 0 {
		return err
	}
	return fmt.Errorf("%s: %s", strings.Join(out, "\n"), err.Error())
}
