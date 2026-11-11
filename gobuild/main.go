// A private helper of the go module: it builds a main package with one exec
// per package, so Dagger caches each package's compile separately and an
// edit re-runs only what it reaches.
//
// It replays the go command's own plan, "go build -n", made from a header
// pack of the source, so an edit inside function bodies doesn't re-plan.
// Every package, the standard library and dependencies included, is built
// in an exec of its own that reads only the archives of what it imports, so
// builds that compile the same package the same way share it, and a package
// whose archive comes out unchanged leaves its dependents cached.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

type Gobuild struct {
	// +private
	ToolImage string
}

func New(
	// The image gohdr, the build's own tool, is built in, which every plan and
	// download mounts. It is the image of the Go version gomod's tools build
	// with (Gomod.goImage of its scannerVersion), pinned by digest there, so a
	// build pulls one Go image, which an engine image can ship, and gohdr is
	// the same input in every build.
	toolImage string,
) *Gobuild {
	return &Gobuild{ToolImage: toolImage}
}

// bare runs an exec as its command alone: none of these execs call Dagger,
// and starting a nested session for each would cost every package; and
// none needs Dagger's init process, whose start every package would pay:
// a script, or the go command, waits for what it starts, and a cancelled
// exec is stopped whole.
var bare = dagger.ContainerWithExecOpts{DisableDaggerInDagger: true, NoInit: true}

// pkgExec is bare for a package's own exec, its compile and link, which also
// asks for transparent huge pages: the compiler's heap grows into fresh
// memory, and on a host set to "always" huge pages save it most of its page
// faults. Elsewhere it changes nothing.
var pkgExec = dagger.ContainerWithExecOpts{DisableDaggerInDagger: true, NoInit: true, TransparentHugePages: true}

// Binary builds the main package pkg as "go build" would and returns a
// directory holding the binary, named output, and for a c-archive or
// c-shared build the C header go build writes next to it. The binary's Go
// build ID is a hash of what its link reads, not the one go build computes.
func (m *Gobuild) Binary(
	ctx context.Context,
	// The Go container to build in: its image supplies the toolchain, and
	// its environment (GOOS, GOARCH, GOFLAGS, GOPROXY...) applies. Per
	// package, its mounts, such as Go cache volumes, are left out.
	base *dagger.Container,
	// The image base was made from. Set, package execs run in that image
	// with base's environment alone, so their keys don't carry how base was
	// made (a workdir at the project's mount path, its cache mounts), and
	// projects with the same image and settings share them.
	// +optional
	image string,
	// The custom container base was made from, as image is for an image:
	// package execs run in it with base's environment alone.
	// +optional
	from *dagger.Container,
	// The source tree.
	source *dagger.Directory,
	// The header pack of source's Go files and go.mod files, from the go
	// module's scan: what the plan is made from, with source's other files.
	headers *dagger.File,
	// Where source is mounted. Paths in the binary's debug information
	// name it, unless flags hold -trimpath.
	mountPath string,
	// The directory to run go build in, relative to mountPath.
	// +default="."
	workdir string,
	// The go build package argument, e.g. "." or "./cmd/foo".
	pkg string,
	// The binary's file name.
	output string,
	// Flags for go build, before the package.
	// +optional
	flags []string,
	// The container module downloads run with: its environment, such as
	// GOPROXY and GOPRIVATE, its credentials and its mounts, such as git
	// configuration or an SSH agent. Unset, base's. No package exec reads it,
	// so it keys downloads alone.
	// +optional
	downloads *dagger.Container,
	// Import paths that many builds import, such as what generated code
	// imports. The packages they reach outside the module are compiled in
	// dependency calls of their own: the standard library's, then the rest.
	// Each is the same call in every build that keeps those packages' module
	// versions, whatever else the build imports, so a cache that has one, such
	// as an engine image's, serves it whole.
	// +optional
	sharedImports []string,
) (*dagger.Directory, error) {
	blocks, linked, err := run(ctx, m.ToolImage, base, image, from, downloads, source, headers, mountPath, workdir, pkg, flags, false, sharedImports)
	if err != nil {
		return nil, err
	}
	out := core.NewDirectory().WithFile(output, linked.file)
	if blocks[linkID(blocks)].exportsHeader() {
		// cgo writes the header only for a package that exports something,
		// and go build installs it only if it was written.
		names, err := linked.dir.Entries(ctx)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, cgoHeader) {
			out = out.WithFile(strings.TrimSuffix(output, path.Ext(output))+".h", linked.dir.File(cgoHeader))
		}
	}
	return out, nil
}

// TestBinary builds the test binary of package pkg as "go test -c" would,
// one exec per package as Binary builds a binary: the package compiled with
// its test files, its external test package, the packages it is imported
// through recompiled against it, and the test main the go command writes,
// linked. Unlike go test -c, it does not run go vet, which is left to the
// lint checks. The binary's Go build ID is a hash of what its link reads,
// not the one go test computes.
func (m *Gobuild) TestBinary(
	ctx context.Context,
	// The Go container to build in, as for Binary.
	base *dagger.Container,
	// The image base was made from, as for Binary.
	// +optional
	image string,
	// The custom container base was made from, as for Binary.
	// +optional
	from *dagger.Container,
	// The source tree, with the package's test files.
	source *dagger.Directory,
	// The header pack of source's Go files, test files included, and go.mod
	// files, from the go module's scan.
	headers *dagger.File,
	// Where source is mounted.
	mountPath string,
	// The directory to run go test in, relative to mountPath.
	// +default="."
	workdir string,
	// The go test package argument, e.g. "." or "./pkg/foo".
	pkg string,
	// Flags for go test -c, before the package, e.g. -race or -ldflags.
	// +optional
	flags []string,
	// The container module downloads run with, as for Binary.
	// +optional
	downloads *dagger.Container,
	// Import paths whose packages are compiled in dependency calls of their
	// own, as for Binary.
	// +optional
	sharedImports []string,
	// The go test package arguments of the test binaries planned with this
	// one, pkg's among them, such as its collection's. The plan, its setup,
	// the test mains and the dependency call are those of TestBinaries of the
	// set, which every TestBinary of the set shares, and this one compiles
	// only what its binary links, so it is ready when its own link is. With
	// a flag, in flags or the base's GOFLAGS, whose packages are those on go
	// test's command line (commandLineFlags), it is planned alone, so that it
	// is go test -c's of pkg alone.
	// +optional
	set []string,
) (*dagger.File, error) {
	if len(set) > 0 {
		goflags, err := base.EnvVariable(ctx, "GOFLAGS")
		if err != nil {
			return nil, err
		}
		envFlags, err := splitGOFLAGS(goflags)
		if err != nil {
			return nil, fmt.Errorf("GOFLAGS: %w", err)
		}
		if !commandLineFlags(append(envFlags, flags...)) {
			return setMember(ctx, m.ToolImage, base, image, from, downloads, source, headers, mountPath, workdir, pkg, set, flags, sharedImports)
		}
	}
	_, linked, err := run(ctx, m.ToolImage, base, image, from, downloads, source, headers, mountPath, workdir, pkg, flags, true, sharedImports)
	if err != nil {
		return nil, err
	}
	return linked.file, nil
}

// TestBinaries builds the test binaries of packages pkgs as go test -c
// builds them together, each as TestBinary builds one, from one plan of them
// all: the set's setup, plan, test mains and dependency call are made once,
// and a package more than one binary compiles compiles once. As on go test's
// command line, a build flag without a package pattern, such as
// -gcflags=-N, applies to every one of pkgs, including where one is
// another's dependency. It returns a directory with each binary in its
// package's directory, relative to workdir, named as go test -c names it,
// e.g. "a/a.test". A package with no test files for the target, whose files
// build constraints leave out, has none, as go test builds it none.
func (m *Gobuild) TestBinaries(
	ctx context.Context,
	// The Go container to build in, as for Binary.
	base *dagger.Container,
	// The image base was made from, as for Binary.
	// +optional
	image string,
	// The custom container base was made from, as for Binary.
	// +optional
	from *dagger.Container,
	// The source tree, with the packages' test files.
	source *dagger.Directory,
	// The header pack of source's Go files, test files included, and go.mod
	// files, from the go module's scan.
	headers *dagger.File,
	// Where source is mounted.
	mountPath string,
	// The directory to run go test in, relative to mountPath.
	// +default="."
	workdir string,
	// The go test package arguments, one package each, e.g. ["./a", "./b"].
	pkgs []string,
	// Flags for go test -c, before the packages, e.g. -race or -ldflags.
	// +optional
	flags []string,
	// The container module downloads run with, as for Binary.
	// +optional
	downloads *dagger.Container,
	// Import paths whose packages are compiled in dependency calls of their
	// own, as for Binary.
	// +optional
	sharedImports []string,
) (*dagger.Directory, error) {
	return runTests(ctx, m.ToolImage, base, image, from, downloads, source, headers, mountPath, workdir, pkgs, flags, sharedImports)
}

// setMember builds pkg's test binary from the plan of set's, as TestBinary
// with a set does.
func setMember(ctx context.Context, toolImage string, base *dagger.Container, image string, from, downloads *dagger.Container, source *dagger.Directory, headers *dagger.File, mountPath, workdir, pkg string, set, flags, shared []string) (*dagger.File, error) {
	t, err := planTestSet(ctx, toolImage, base, image, from, downloads, source, headers, mountPath, workdir, set, flags, shared)
	if err != nil {
		return nil, err
	}
	dir := path.Join(mountPath, workdir, pkg)
	if t != nil {
		for i, d := range t.dirs {
			if d != dir {
				continue
			}
			results, err := t.walk(ctx, closure(t.blocks, t.links[i]))
			if err != nil {
				return nil, err
			}
			return results[t.links[i]].file, nil
		}
	}
	return nil, fmt.Errorf("go test package %s: no test files for the target among its set's", pkg)
}

// runTests builds pkgs' test binaries from one plan, as TestBinaries does.
func runTests(ctx context.Context, toolImage string, base *dagger.Container, image string, from, downloads *dagger.Container, source *dagger.Directory, headers *dagger.File, mountPath, workdir string, pkgs, flags, shared []string) (*dagger.Directory, error) {
	out := core.NewDirectory()
	t, err := planTestSet(ctx, toolImage, base, image, from, downloads, source, headers, mountPath, workdir, pkgs, flags, shared)
	if err != nil || t == nil {
		return out, err
	}
	results, err := t.walk(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i, id := range t.links {
		rel := "."
		if t.dirs[i] != t.cwd {
			rel = strings.TrimPrefix(t.dirs[i], t.cwd+"/")
		}
		out = out.WithFile(path.Join(rel, t.blocks[id].out), results[id].file)
	}
	return out, nil
}

// testsPlan is one plan of several packages' test binaries, which a set of
// them builds from (runTests), or one of them (TestBinary with its set).
type testsPlan struct {
	b       *build
	p       *planned
	blocks  map[string]*block
	headers *dagger.File
	flags   []string
	cwd     string
	// The packages with test files for the target, by import path, in go
	// list's order; each one's directory and the source directory of its
	// test files; and the id of the block that links its test binary.
	ips, dirs, srcDirs, links []string
}

// planTestSet plans pkgs' test binaries as runTests builds them: the same
// setup, plan and listing for every caller with the same pkgs, flags and
// shared imports, so the engine runs each of their steps once. It is nil
// when none of pkgs has test files for the target.
func planTestSet(ctx context.Context, toolImage string, base *dagger.Container, image string, from, downloads *dagger.Container, source *dagger.Directory, headers *dagger.File, mountPath, workdir string, pkgs, flags, shared []string) (*testsPlan, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}
	cwd := path.Join(mountPath, workdir)
	// Vet is go test's, as for one test binary (run).
	flags = append([]string{"-vet=off"}, flags...)
	dirs := make([]string, len(pkgs))
	for i, pkg := range pkgs {
		dirs[i] = path.Join(workdir, pkg)
	}
	b, err := newBuild(ctx, toolImage, base, image, from, downloads, source, headers, dirs, true, mountPath, cwd, flags)
	if err != nil {
		return nil, err
	}
	b.shared = shared
	type sizesResult struct {
		sizes map[string]int64
		err   error
	}
	sized := make(chan sizesResult, 1)
	go func() {
		sizes, err := b.sourceSizes(ctx)
		sized <- sizesResult{sizes, err}
	}()
	// Only the packages with test files for the target are planned, by
	// import path, in go list's order: go test builds the others no test
	// binary. The plan's headers name packages by import path alone, and a
	// plan of several test binaries compiles a package once for each test it
	// is recompiled for: the listing names each variant, as for archives.
	buildFlags := flags[1:]
	tested := append(append([]string{"go", "list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}"}, buildFlags...), pkgs...)
	list := append([]string{"go", "list", "-deps", "-test", "-json=ImportPath,Name,Module,DepOnly,Dir,ForTest"}, buildFlags...)
	test := append(append([]string{"go", "test", "-c", "-n"}, flags...), "-o", "/dev/null")
	p, err := b.planWith(ctx, headers, "tested=$("+shJoin(tested)+") && printf '%s\\n' $tested > /gobuild/tested.txt && "+
		"if [ -n \"$tested\" ]; then "+shJoin(list)+" $tested > /gobuild/list.json && "+shJoin(test)+" $tested; else : > /gobuild/list.json; fi", nil)
	if err != nil {
		return nil, err
	}
	testedList, err := p.ran.File("/gobuild/tested.txt").Contents(ctx)
	if err != nil {
		return nil, err
	}
	ips := strings.Fields(testedList)
	if len(ips) == 0 {
		return nil, nil
	}
	blocks, err := parseTestsPlan(p.plan, b.roots)
	if err != nil {
		return nil, err
	}
	listing, err := p.ran.File("/gobuild/list.json").Contents(ctx)
	if err != nil {
		return nil, err
	}
	ids, mains, _, err := listedIDs(listing)
	if err != nil {
		return nil, err
	}
	if err := identifyVariants(blocks, ids, mains, true); err != nil {
		return nil, err
	}
	dirs, links, err := testLinks(blocks, listing, ips)
	if err != nil {
		return nil, err
	}
	sr := <-sized
	if sr.err != nil {
		return nil, fmt.Errorf("source sizes: %w", sr.err)
	}
	weighSource(blocks, p.weights, sr.sizes)
	srcDirs := make([]string, len(dirs))
	for i, dir := range dirs {
		srcDirs[i] = strings.TrimPrefix(strings.TrimPrefix(dir, b.roots.Src), "/")
	}
	return &testsPlan{b: b, p: p, blocks: blocks, headers: headers, flags: flags, cwd: cwd,
		ips: ips, dirs: dirs, srcDirs: srcDirs, links: links}, nil
}

// walk builds the plan's packages, or with keep, only those it keeps, and
// returns their results by block id. The dependency call and the test mains
// are the whole plan's either way, so every walk of the plan shares them.
func (t *testsPlan) walk(ctx context.Context, keep map[string]bool) (map[string]*result, error) {
	return t.b.walk(ctx, t.blocks, t.p.weights, t.p.cc, t.p.goDirs, keep, func(w *walker) error {
		w.testmain = t.b.testmains(ctx, t.headers, t.flags, t.ips, t.srcDirs, t.links)
		return nil
	})
}

// closure is the block with id and every block it depends on.
func closure(blocks map[string]*block, id string) map[string]bool {
	seen := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, d := range blocks[id].deps {
			visit(d)
		}
	}
	visit(id)
	return seen
}

// testLinks is, for each package of ips, by import path, its directory,
// from go list's listing, and the id of the block that links its test
// binary: its test main's.
func testLinks(blocks map[string]*block, listing string, ips []string) (dirs, links []string, err error) {
	dirOf := map[string]string{}
	dec := json.NewDecoder(strings.NewReader(listing))
	for {
		var p struct{ ImportPath, Dir, ForTest string }
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, fmt.Errorf("go list: %w", err)
		}
		if p.ForTest == "" && !strings.HasSuffix(p.ImportPath, ".test") {
			dirOf[p.ImportPath] = p.Dir
		}
	}
	linkOf := map[string]string{}
	for id, bl := range blocks {
		if bl.isLink {
			linkOf[strings.TrimSuffix(bl.importPath, ".test")] = id
		}
	}
	for _, ip := range ips {
		dir, ok := dirOf[ip]
		if !ok {
			return nil, nil, fmt.Errorf("go test package %s: not in go list's listing", ip)
		}
		id, ok := linkOf[ip]
		if !ok {
			return nil, nil, fmt.Errorf("go test package %s: no test binary in the plan", ip)
		}
		dirs, links = append(dirs, dir), append(links, id)
	}
	return dirs, links, nil
}

// Archives compiles, one exec per package as Binary builds, every package
// that "go list -export -deps -test ./..." compiles in the module in workdir:
// its packages, main packages included, their test variants and external test
// packages, the packages recompiled against those, and every package they
// import. It returns a directory for golangci-lint's export driver: under
// archives/, each package's export data, the archive "go tool compile -pack
// -linkobj" writes without the package's object code; under cgo/, the Go files cgo wrote for each cgo package's compile;
// and manifest.json, which describes them: the Go version, target and build
// tags they were compiled for, and for each package, by its ID in go list's
// listing, its archive, its cgo files and their output, and a digest of what
// it was compiled from. With all of them, the driver compiles nothing and
// runs no cgo. With dependencies, only the packages of other modules and the
// standard library: a directory keyed by their content alone, which an edit
// of the module's own packages leaves as it is.
func (m *Gobuild) Archives(
	ctx context.Context,
	// The Go container to build in, as for Binary.
	base *dagger.Container,
	// The image base was made from, as for Binary.
	// +optional
	image string,
	// The custom container base was made from, as for Binary.
	// +optional
	from *dagger.Container,
	// The source tree, with its test files, which name packages the tests
	// import.
	source *dagger.Directory,
	// The header pack of source's Go files, test files included, and go.mod
	// files, from the go module's scan.
	headers *dagger.File,
	// Where source is mounted.
	mountPath string,
	// The module's directory, relative to mountPath.
	// +default="."
	workdir string,
	// Flags for go build, as for Binary: the build tags among them are the
	// manifest's.
	// +optional
	flags []string,
	// Only the packages outside the module.
	// +optional
	dependencies bool,
	// The container module downloads run with, as for Binary.
	// +optional
	downloads *dagger.Container,
	// The packages whose imports are archived, as go list patterns from
	// workdir. Unset, every package of the module.
	// +optional
	packages []string,
	// With the packages' test variants and external test packages, and what
	// they import, as a lint reads them.
	// +default=true
	tests bool,
	// Import paths whose packages are compiled in dependency calls of their
	// own, as for Binary.
	// +optional
	sharedImports []string,
) (*dagger.Directory, error) {
	w, err := archiveWalk(ctx, m.ToolImage, base, image, from, source, headers, mountPath, workdir, flags, dependencies, downloads, packages, tests, sharedImports)
	if err != nil {
		return nil, err
	}
	b, blocks, results, order, p, envFlags := w.build, w.blocks, w.results, w.order, w.plan, w.envFlags
	// The digests are of the source whole, and of what cgo wrote for a cgo
	// package's compile, read from the package's exec.
	var modFiles []string
	for _, bl := range blocks {
		modFiles = append(modFiles, bl.modFiles...)
	}
	tool := w.manifestTool
	ctr := b.modules.mount(b.base.
		WithMountedDirectory(writeManifestDir, tool), modFiles).
		WithMountedDirectory(b.roots.Src, b.src)
	out := core.NewDirectory()
	var entries strings.Builder
	var archives []*dagger.File
	for _, bl := range order {
		r := results[bl.id]
		archives = append(archives, r.file)
		entries.WriteString(bl.importPath + "\tarchives/" + bl.output())
		if bl.cgo {
			at := "cgo/" + bl.name
			ctr = ctr.WithMountedDirectory("/gobuild/"+at, r.dir)
			var generated []*dagger.File
			for _, f := range bl.cgoOutputs() {
				entries.WriteString("\t" + at + "/" + f)
				generated = append(generated, r.dir.File(f))
			}
			out = out.WithFiles("cgo/"+bl.name, generated)
		}
		entries.WriteString("\n")
	}
	// The settings the manifest records are the go command's where the plan
	// ran it, as the plan left it: the toolchain it switched to, with the
	// module cache it set up for it. The plan's exec records them.
	env := p.ran.File("/gobuild/env.json")
	// The manifest is read from its exec's evaluated output, so it is keyed
	// by its content: the exec reads the whole listing, which any edit
	// changes.
	manifestDir, err := ctr.
		WithMountedFile("/gobuild/list.json", p.ran.File("/gobuild/list.json")).
		WithMountedFile("/gobuild/env.json", env).
		WithNewFile("/gobuild/entries.txt", entries.String()).
		WithExec([]string{"sh", "-c", "mkdir -p /gobuild/manifest && " + writeManifestBin + " /gobuild/list.json /gobuild/env.json /gobuild/entries.txt " +
			shQuote(strings.Join(buildTags(append(envFlags, flags...)), ",")) + " /gobuild > /gobuild/manifest/manifest.json"}, bare).
		Directory("/gobuild/manifest").Sync(ctx)
	if err != nil {
		return nil, err
	}
	manifest, err := manifestDir.File("manifest.json").Sync(ctx)
	if err != nil {
		return nil, err
	}
	list, err := p.ran.File("/gobuild/list.json").Contents(ctx)
	if err != nil {
		return nil, err
	}
	listing, err := dependencyListing(list)
	if err != nil {
		return nil, err
	}
	return out.WithFiles("archives", archives).WithFile("manifest.json", manifest).WithNewFile(listingFile, listing), nil
}

// archiveWalked is the walk Archives makes: the build, its plan and the
// flags GOFLAGS adds, the blocks it planned, each walked package's result,
// and the walked packages Archives returns, in import path order.
type archiveWalked struct {
	build    *build
	plan     *planned
	envFlags []string
	blocks   map[string]*block
	results  map[string]*result
	order    []*block
	// manifestTool is write-manifest, for the build's target, which the
	// plan's exec ran to record the go command's settings.
	manifestTool *dagger.Directory
}

// archiveWalk plans and walks what Archives compiles, with Archives' arguments.
func archiveWalk(ctx context.Context, toolImage string, base *dagger.Container, image string, from *dagger.Container, source *dagger.Directory, headers *dagger.File, mountPath, workdir string, flags []string, dependencies bool, downloads *dagger.Container, packages []string, tests bool, shared []string) (*archiveWalked, error) {
	cwd := path.Join(mountPath, workdir)
	// Discovery follows the imports of the one package archived, as a
	// binary's build follows its package's; of every package otherwise.
	var pkgDirs []string
	if len(packages) == 1 && !strings.Contains(packages[0], "...") {
		pkgDirs = []string{path.Join(workdir, packages[0])}
	}
	b, err := newBuild(ctx, toolImage, base, image, from, downloads, source, headers, pkgDirs, tests, mountPath, cwd, flags)
	if err != nil {
		return nil, err
	}
	b.shared = shared
	goflags, err := b.base.EnvVariable(ctx, "GOFLAGS")
	if err != nil {
		return nil, err
	}
	envFlags, err := splitGOFLAGS(goflags)
	if err != nil {
		return nil, fmt.Errorf("GOFLAGS: %w", err)
	}
	// go list -export prints the plan of what it compiles, as go build -n
	// does, and its listing of the packages.
	listCmd := []string{"go", "list", "-export", "-deps", "-n", "-json=" + listFields}
	if tests {
		listCmd = append(listCmd, "-test")
	}
	if len(packages) == 0 {
		packages = []string{"./..."}
	}
	goCmd := shJoin(append(append(listCmd, flags...), packages...)) + " > /gobuild/list.json"
	tool := manifestTools(toolImage, b.target.platform())
	p, err := b.planWith(ctx, headers, goCmd, tool)
	if err != nil {
		return nil, err
	}
	blocks, err := parseArchivePlan(p.plan, b.roots)
	if err != nil {
		return nil, err
	}
	list, err := p.ran.File("/gobuild/list.json").Contents(ctx)
	if err != nil {
		return nil, err
	}
	ids, mains, own, err := listedIDs(list)
	if err != nil {
		return nil, err
	}
	if err := identifyVariants(blocks, ids, mains, false); err != nil {
		return nil, err
	}
	walked := blocks
	if dependencies {
		walked = dependencyBlocks(blocks, own)
	}
	results, err := b.walk(ctx, walked, p.weights, p.cc, p.goDirs, nil, nil)
	if err != nil {
		return nil, err
	}
	var order []*block
	for _, bl := range walked {
		if !bl.isProfile && !(dependencies && own[bl.importPath]) {
			order = append(order, bl)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].importPath < order[j].importPath })
	return &archiveWalked{build: b, plan: p, envFlags: envFlags, blocks: blocks, results: results, order: order, manifestTool: tool}, nil
}

// ArchiveCache is what a build reads from the cache that every build of its
// packages with the same toolchain, flags and go.sum shares.
type ArchiveCache struct {
	// Each package's build: the directory the package's exec wrote, with its
	// archives, as every build of the package reads it from the cache.
	Builds []*dagger.Directory
	// Each module version the build downloaded (go mod download), with its
	// plan pack and its zip, in the order of its module cache directory name.
	Downloads []*dagger.Directory
	// gohdr, the tool the build's execs run, for every machine an engine
	// runs on (machineTools).
	Tool *dagger.Directory
	// golangci-lint's write-manifest, which writes the archives' manifest,
	// for every machine an engine runs on (machineTools), and the source it
	// is built from, which a fresh engine would otherwise fetch to find the
	// build.
	ManifestTool   *dagger.Directory
	ManifestSource *dagger.Directory
	// Go's own tools the build's execs run, built by its toolchain for every
	// machine an engine runs on: pack, which packages with assembly run.
	Tools *dagger.Directory
	// The shared dependency calls the build made (sharedImports), each
	// call's result by its ID in this session: a cache that is to have the
	// calls whole, not only each package's build, takes these.
	SharedDependencies []string
}

// ArchiveBuilds compiles what Archives compiles, with its arguments, and
// returns what it read from the cache instead of the export driver's
// directory: each package's build, each module download, gohdr,
// write-manifest and its source, and Go's pack. A cache that is to have exactly these results, such as the cache an
// engine image pre-warms, is filled from them.
func (m *Gobuild) ArchiveBuilds(
	ctx context.Context,
	// The Go container to build in, as for Binary.
	base *dagger.Container,
	// The image base was made from, as for Binary.
	// +optional
	image string,
	// The custom container base was made from, as for Binary.
	// +optional
	from *dagger.Container,
	// The source tree, as for Archives.
	source *dagger.Directory,
	// The header pack of source's Go files, as for Archives.
	headers *dagger.File,
	// Where source is mounted.
	mountPath string,
	// The module's directory, relative to mountPath.
	// +default="."
	workdir string,
	// Flags for go build, as for Binary.
	// +optional
	flags []string,
	// Only the packages outside the module.
	// +optional
	dependencies bool,
	// The container module downloads run with, as for Binary.
	// +optional
	downloads *dagger.Container,
	// The packages whose imports are compiled, as for Archives.
	// +optional
	packages []string,
	// With the packages' test variants, as for Archives.
	// +default=true
	tests bool,
	// Import paths whose packages are compiled in dependency calls of their
	// own, as for Binary.
	// +optional
	sharedImports []string,
) (*ArchiveCache, error) {
	w, err := archiveWalk(ctx, m.ToolImage, base, image, from, source, headers, mountPath, workdir, flags, dependencies, downloads, packages, tests, sharedImports)
	if err != nil {
		return nil, err
	}
	dirs := make([]*dagger.Directory, 0, len(w.order))
	for _, bl := range w.order {
		dirs = append(dirs, w.results[bl.id].dir)
	}
	shared := make([]string, 0, len(w.build.sharedCalls))
	for _, call := range w.build.sharedCalls {
		id, err := call.ID(ctx)
		if err != nil {
			return nil, err
		}
		shared = append(shared, string(id))
	}
	return &ArchiveCache{Builds: dirs, Downloads: w.build.downloads, Tool: w.build.hdr, ManifestTool: manifestTools(w.build.toolImage, w.build.target.platform()), ManifestSource: manifestSource(), Tools: w.build.tools, SharedDependencies: shared}, nil
}

// listFields are the fields of go list's output Archives reads: the
// packages and their names, what write-manifest reads, the manifest
// module's ListFields: their modules and the files their digests cover, and
// their imports, for the listing (listingFile).
const listFields = "ImportPath,Name,Dir,ForTest,Module,GoFiles,CgoFiles,CFiles,CXXFiles,MFiles,HFiles,FFiles,SFiles,SwigFiles,SwigCXXFiles,SysoFiles,EmbedFiles,Imports,ImportMap"

// listingFile is the listing Archives writes beside the manifest, the
// manifest module's ListingFile: the packages outside the module, as its
// plan listed them, with the manifest module's ListingFields. With it,
// golangci-lint's export driver answers a caller that type-checks the
// module from source, such as code generation, with no go command and no
// module cache.
const listingFile = "listing.json"

// listingPackage is a package as the listing (listingFile) has it.
type listingPackage struct {
	ImportPath string
	Name       string
	ForTest    string            `json:",omitempty"`
	Imports    []string          `json:",omitempty"`
	ImportMap  map[string]string `json:",omitempty"`
}

// dependencyListing is the listing (listingFile) of the packages of a
// "go list -json" listing outside the main module: by their content
// alone, so an edit of the module's own packages leaves it as it is.
func dependencyListing(list string) (string, error) {
	var out strings.Builder
	enc := json.NewEncoder(&out)
	dec := json.NewDecoder(strings.NewReader(list))
	for {
		var listed struct {
			listingPackage
			Module *struct{ Main bool }
		}
		if err := dec.Decode(&listed); err == io.EOF {
			return out.String(), nil
		} else if err != nil {
			return "", fmt.Errorf("go list: %w", err)
		}
		if listed.Module != nil && listed.Module.Main {
			continue
		}
		if err := enc.Encode(listed.listingPackage); err != nil {
			return "", err
		}
	}
}

// listedIDs is the set of package IDs in a "go list -json" listing, and the
// set of its main packages, by import path, and of the packages of the main
// module, own.
func listedIDs(list string) (ids, mains, own map[string]bool, err error) {
	ids, mains, own = map[string]bool{}, map[string]bool{}, map[string]bool{}
	dec := json.NewDecoder(strings.NewReader(list))
	for {
		var p struct {
			ImportPath, Name string
			Module           *struct{ Main bool }
		}
		if err := dec.Decode(&p); err == io.EOF {
			return ids, mains, own, nil
		} else if err != nil {
			return nil, nil, nil, fmt.Errorf("go list: %w", err)
		}
		ids[p.ImportPath] = true
		if p.Name == "main" && !strings.Contains(p.ImportPath, " [") {
			mains[p.ImportPath] = true
		}
		own[p.ImportPath] = p.Module != nil && p.Module.Main
	}
}

// dependencyBlocks is the blocks of a plan that compile none of the module's
// own packages (own, by import path), nor anything that imports them, such
// as a test main: the packages of other modules and the standard library,
// which no other module's package can import the module's own into.
func dependencyBlocks(blocks map[string]*block, own map[string]bool) map[string]*block {
	excluded := map[string]bool{}
	for id, bl := range blocks {
		if own[bl.importPath] || strings.HasSuffix(bl.importPath, ".test") {
			excluded[id] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for id, bl := range blocks {
			if excluded[id] {
				continue
			}
			for _, d := range bl.deps {
				if excluded[d] {
					excluded[id], changed = true, true
					break
				}
			}
		}
	}
	kept := map[string]*block{}
	for id, bl := range blocks {
		if !excluded[id] {
			kept[id] = bl
		}
	}
	return kept
}

// buildTags is the -tags value in go build flags, the last one's, as a list.
func buildTags(flags []string) []string {
	var tags []string
	for i := 0; i < len(flags); i++ {
		name, value, ok := strings.Cut(strings.TrimLeft(flags[i], "-"), "=")
		if name != "tags" || !strings.HasPrefix(flags[i], "-") {
			continue
		}
		if !ok && i+1 < len(flags) {
			i++
			value = flags[i]
		}
		tags = strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' })
	}
	return tags
}

// run builds pkg's binary, or with test its test binary, and returns the
// plan's blocks and the link's result.
func run(ctx context.Context, toolImage string, base *dagger.Container, image string, from, downloads *dagger.Container, source *dagger.Directory, headers *dagger.File, mountPath, workdir, pkg string, flags []string, test bool, shared []string) (map[string]*block, *result, error) {
	cwd := path.Join(mountPath, workdir)
	if test {
		// Vet is go test's, not the binary's, and is left to the lint
		// checks: the per-package build doesn't replay it.
		flags = append([]string{"-vet=off"}, flags...)
	}
	b, err := newBuild(ctx, toolImage, base, image, from, downloads, source, headers, []string{path.Join(workdir, pkg)}, test, mountPath, cwd, flags)
	if err != nil {
		return nil, nil, err
	}
	b.shared = shared
	// The source's real sizes are listed alongside the plan, which sees only
	// the files' headers.
	type sizesResult struct {
		sizes map[string]int64
		err   error
	}
	sized := make(chan sizesResult, 1)
	go func() {
		sizes, err := b.sourceSizes(ctx)
		sized <- sizesResult{sizes, err}
	}()
	plan, weights, cc, goDirs, err := b.plan(ctx, headers, flags, pkg, test)
	if err != nil {
		return nil, nil, err
	}
	blocks, err := parsePlan(plan, b.roots)
	if err != nil {
		return nil, nil, err
	}
	sr := <-sized
	if sr.err != nil {
		return nil, nil, fmt.Errorf("source sizes: %w", sr.err)
	}
	weighSource(blocks, weights, sr.sizes)
	results, err := b.walk(ctx, blocks, weights, cc, goDirs, nil, func(w *walker) error {
		if test {
			// Only the link reads it: the packages build meanwhile.
			dir := strings.TrimPrefix(strings.TrimPrefix(path.Join(b.roots.Cwd, pkg), b.roots.Src), "/")
			w.testmain = b.testmains(ctx, headers, flags, []string{pkg}, []string{dir}, []string{linkID(blocks)})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return blocks, results[linkID(blocks)], nil
}

// walk runs a plan's blocks, one exec per package, and returns their results
// by block id. goDirs is the plan's list of the source's Go files in
// directories named like Go files. prepare, when set, readies the walker once
// the module downloads have started.
func (b *build) walk(ctx context.Context, blocks map[string]*block, weights map[string]int64, cc ccInfo, goDirs *dagger.File, keep map[string]bool, prepare func(*walker) error) (map[string]*result, error) {
	// A package with C or assembly in the source reads its files that
	// aren't Go (nonGoSource), which the Go files in directories named like
	// Go files are left out of by name.
	var nonGo *dagger.Directory
	for _, bl := range blocks {
		if bl.srcNonGo {
			list, err := goDirs.Contents(ctx)
			if err != nil {
				return nil, fmt.Errorf("Go files in directories named like Go files: %w", err)
			}
			var goDirGo []string
			for _, f := range strings.Split(list, "\n") {
				if f != "" {
					goDirGo = append(goDirGo, f)
				}
			}
			nonGo = nonGoSource(b.src, goDirGo)
			break
		}
	}
	// A walk that keeps some blocks reads only their sources.
	synced := blocks
	if keep != nil {
		synced = map[string]*block{}
		for id, bl := range blocks {
			if keep[id] {
				synced[id] = bl
			}
		}
	}
	sources := syncSources(ctx, b.src, nonGo, synced)
	for _, bl := range blocks {
		bl.addCFlags(cc)
	}
	mods := &modules{root: b.roots.Mod, all: b.mods, zips: b.zips, fetchZip: b.fetch, sums: b.sums}
	b.modules = mods
	if err := mods.expandLoose(ctx, blocks); err != nil {
		return nil, err
	}
	mods.excludeUnread(blocks)
	heights := heightsOf(blocks, weights)
	mods.fetch(ctx, blocks, heights)
	w := &walker{base: b.base, platform: b.target.platform(), hdr: b.hdr, tools: b.tools, mods: mods, roots: b.roots, sources: sources}
	if prepare != nil {
		if err := prepare(w); err != nil {
			return nil, err
		}
	}
	// A PGO profile is preprocessed first: every compile reads it.
	var err error
	for _, bl := range blocks {
		if bl.isProfile {
			if w.profile, err = b.profileStep(ctx, bl, blocks, mods); err != nil {
				return nil, fmt.Errorf("pgo profile: %w", err)
			}
		}
	}
	// Every package is its own exec, the standard library and dependencies
	// as much as the workspace's, each reading only the archives of the
	// packages it imports: its key is what it compiles, so every build that
	// compiles the same package the same way shares it.
	outside := outsideSet(blocks)
	nodes := toNodes(blocks, outside, weights)
	w.heights = heights
	done, err := b.dependencies(ctx, w, nodes, dependencySet(blocks, mods), weights)
	if err != nil {
		return nil, err
	}
	for id := range nodes {
		if done[id] != nil || (keep != nil && !keep[id]) {
			delete(nodes, id)
		}
	}
	return w.walk(ctx, nodes, done)
}

// profileStep runs the plan's PGO profile preprocessing, from the profile in
// the source, and returns its output for the execs whose compiles read it.
func (b *build) profileStep(ctx context.Context, bl *block, blocks map[string]*block, mods *modules) (*profileOutput, error) {
	ctr := b.base
	if len(bl.srcFiles) > 0 {
		srcDir, at := selected(b.src, bl.srcFiles)
		ctr = ctr.WithMountedDirectory(path.Join(b.roots.Src, at), srcDir, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	ctr = mods.mount(ctr, bl.modFiles)
	out := "/work/" + bl.name
	dir, err := ctr.WithExec([]string{"sh", "-c", bl.script(blocks, soloCompileC)}, bare).
		Directory(out).Sync(ctx)
	if err != nil {
		return nil, err
	}
	return &profileOutput{dir: dir, path: out}, nil
}

// modCacheDir is where the build's module download is made, mounted at
// GOMODCACHE wherever it is read.
const modCacheDir = "/gobuild/modcache"

// build is what a build's execs share.
type build struct {
	// base is the caller's container without its mounts, so no cache volume
	// shadows the module sources mounted per package.
	base *dagger.Container
	src  *dagger.Directory
	// mods is the module cache the go command made before any download,
	// such as a toolchain it switched to, or all of it, downloaded per
	// project, when it switched.
	mods *dagger.Directory
	// packs is the plan packs of the build's downloaded modules (downloads),
	// which a plan unpacks into its module cache; nil without any.
	packs *dagger.Directory
	// zips is each downloaded module's zip, by its directory name in the
	// module cache.
	zips map[string]*dagger.File
	// downloads is each downloaded module's download (downloadModule), in
	// the order of its directory name in the module cache.
	downloads []*dagger.Directory
	// sums is the checksum of each downloaded module, by its directory name
	// in the module cache.
	sums map[string]string
	// planSrc and planMods list, a line each, the directories of the source
	// and of the module cache whose Go files a plan unpacks (planDirs); nil
	// for every one.
	planSrc, planMods *string
	// modules is the walk's modules, for a step after it that reads them.
	modules *modules
	// fetch downloads, on its own, the zip of a module the build reads that
	// discover didn't find, by its directory name; nil when the build didn't
	// download per module.
	fetch func(name string) *dagger.File
	tools *dagger.Directory
	// hdr is gohdr, for every machine an engine runs on (hdrTools).
	hdr   *dagger.Directory
	roots roots
	// toolImage is the image the tools are built in, which the build's own
	// steps, those that run gohdr alone, run in too.
	toolImage string
	// target is what the build compiles for.
	target target
	// shared is the import paths whose packages the dependency call compiles
	// in calls of their own (sharedImports), and sharedCalls those calls,
	// once made.
	shared      []string
	sharedCalls []*dagger.GobuildDependencyBuilds
}

// newBuild prepares a build of the source; pkgDirs are the directories of
// the packages it builds, relative to the source, or none for every package
// of the module, and tests whether it builds tests: they decide which modules the
// build downloads.
func newBuild(ctx context.Context, toolImage string, ctr *dagger.Container, image string, from, downloads *dagger.Container, source *dagger.Directory, headers *dagger.File, pkgDirs []string, tests bool, mountPath, cwd string, flags []string) (*build, error) {
	// The plan's view of the source is a filtered copy, as slow on a large
	// source as the rest of this setup, which it doesn't depend on: it's
	// made alongside it. An error shows again where the plan mounts it.
	go planSource(source).Sync(ctx)
	var err error
	if image != "" {
		from = core.NewContainer().From(image)
	}
	// The caller's own container downloads a private module, with its
	// credentials; a public one downloads in the toolchain alone, with the
	// caller's download settings. The caller is downloads when it's set.
	caller := ctr
	if downloads != nil {
		caller = downloads
	}
	public := caller
	if from != nil {
		if public, err = withEnvOnly(ctx, caller, from, func(name string) bool { return slices.Contains(downloadEnv, name) }); err != nil {
			return nil, err
		}
		if ctr, err = canonical(ctx, ctr, from); err != nil {
			return nil, err
		}
	}
	// What the source's packages import, which the build follows from the
	// package it builds to the modules it downloads. It needs nothing of the
	// setup below, so it's listed alongside it.
	importsMode := "build"
	if tests {
		importsMode = "tests"
	}
	// The tools are for the build's target, as the builds that run them.
	platform, err := targetPlatform(ctx, ctr)
	if err != nil {
		return nil, err
	}
	hdr := hdrTools(toolImage, platform)
	type listing struct {
		out string
		err error
	}
	imports := make(chan listing, 1)
	go func() {
		out, err := public.
			WithMountedDirectory(hdrDir, hdr, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true}).
			WithMountedFile("/gobuild/src.hdr", headers, dagger.ContainerWithMountedFileOpts{ReadOnly: true}).
			WithExec([]string{hdrBin, "imports", "/gobuild/src.hdr", importsMode}, bare).
			Stdout(ctx)
		imports <- listing{out, err}
	}()
	// The Go caches' volumes are dropped: they would shadow the module
	// sources mounted per package, and live on one engine. A custom base's
	// other mounts, such as a toolchain or headers, stay.
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
	callerMounts, err := caller.Mounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range callerMounts {
		if caches[m] {
			caller = caller.WithoutMount(m)
		}
	}
	gomodcache, err := ctr.EnvVariable(ctx, "GOMODCACHE")
	if err != nil {
		return nil, err
	}
	if gomodcache == "" {
		out, err := ctr.WithExec([]string{"go", "env", "GOMODCACHE"}, bare).Stdout(ctx)
		if err != nil {
			return nil, withStderr(err)
		}
		gomodcache = strings.TrimSpace(out)
	}
	// Synced once, so each package exec refers to it instead of repeating
	// how it was made.
	base, err := ctr.Sync(ctx)
	if err != nil {
		return nil, err
	}
	goflags, err := base.EnvVariable(ctx, "GOFLAGS")
	if err != nil {
		return nil, err
	}
	envFlags, err := splitGOFLAGS(goflags)
	if err != nil {
		return nil, fmt.Errorf("GOFLAGS: %w", err)
	}
	if err := checkFlags(append(append([]string(nil), flags...), envFlags...)); err != nil {
		return nil, err
	}
	b := &build{base: base, src: source, roots: roots{Src: mountPath, Mod: gomodcache, Cwd: cwd}, toolImage: toolImage, hdr: hdr}
	// The module cache is made from the build's go.sums: the main module's,
	// those of a go.work's modules and its go.work.sum, or the alternate
	// go.mod a -modfile flag names, with its go.sum. The go command lists
	// them, from every go.mod and go.sum in the source, and does what it
	// does before any download, such as switching to the toolchain the
	// go.mod asks for, which it downloads as a module.
	include := []string{"**/go.mod", "**/go.sum", "go.work", "go.work.sum", "**/vendor/modules.txt"}
	download := []string{"go", "mod", "download"}
	list := []string{"go", "list", "-m", "-f", "{{.GoMod}}"}
	// The go.mod the build reads its go directive from: the alternate one,
	// or with none, the module's own, which the script asks go for.
	goMod := ""
	if mf, ok := lastFlag("modfile", flags, envFlags); ok && mf == "" {
		// An empty -modfile in the call means the module's own go.mod,
		// whatever GOFLAGS, which the download runs with, names.
		download = append(download, "-modfile=")
		list = append(list, "-modfile=")
	} else if ok {
		if !path.IsAbs(mf) {
			mf = path.Join(cwd, mf)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(mf, mountPath), "/")
		include = append(include, rel, strings.TrimSuffix(rel, ".mod")+".sum")
		download = append(download, "-modfile="+mf)
		list = append(list, "-modfile="+mf)
		goMod = mf
	}
	// The cache directory is made first: a module with no dependencies
	// downloads nothing, and nothing else would create it.
	quoted := make([]string, len(download))
	for i, a := range download {
		quoted[i] = shQuote(a)
	}
	listed := make([]string, len(list))
	for i, a := range list {
		listed[i] = shQuote(a)
	}
	// In vendor mode the go command reads dependencies from the vendor
	// directory and never the module cache, and a vendored dependency need
	// not be downloadable at all. The mode is go's own choice: -mod names
	// it, or with no -mod, a vendor directory at the main module's root, or
	// the go.work's, selects it from go 1.14, or 1.22 for a go.work, by the
	// go directive of the go.mod the build reads, a -modfile one included. A
	// vendor directory without modules.txt is an error go reports itself.
	// Without one in the source, go is not asked.
	mode, _ := lastFlag("mod", flags, envFlags)
	// The download goes to one path whatever GOMODCACHE the base has, as a
	// file read from the module cache is keyed by its path in it too: then
	// a module's download is the same input in every project. A module
	// cache the base comes with, filled for offline builds, is its start.
	seed := ""
	if path.Clean(gomodcache) != modCacheDir {
		seed = "\n[ ! -d " + shQuote(gomodcache) + " ] || [ " + shQuote(gomodcache) + " -ef " + modCacheDir + " ] || cp -a " + shQuote(gomodcache) + "/. " + modCacheDir + "/"
	}
	script := "set -e\nmkdir -p " + modCacheDir + seed +
		"\nexport GOMODCACHE=" + modCacheDir + "\nmode=" + shQuote(mode) + "\nsrc=" + shQuote(mountPath) + "\ngomod=" + shQuote(goMod) + `
if [ -z "$mode" ] && [ -n "$(find "$src" -path '*/vendor/modules.txt' | head -n 1)" ]; then
	work=$(go env GOWORK)
	if [ -n "$work" ] && [ "$work" != off ]; then
		file=$work edit=work min=22
	else
		file=$(go env GOMOD) edit=mod min=14
	fi
	root=${file%/*}
	[ "$edit" = mod ] && [ -n "$gomod" ] && file=$gomod
	if [ -f "$root/vendor/modules.txt" ]; then
		minor=$(go "$edit" edit -json "$file" | sed -n 's/^	"Go": "1\.\([0-9]*\).*/\1/p')
		[ "${minor:-0}" -ge "$min" ] && mode=vendor
	fi
fi
printf '%s' "$mode" > /gobuild/mode
: > /gobuild/sums
go env GOOS GOARCH CGO_ENABLED GOVERSION > /gobuild/goenv
[ "$mode" = vendor ] || {
	gomods=$(` + strings.Join(listed, " ") + `)
	if [ "$(go env GOVERSION)" != "$(GOTOOLCHAIN=local go env GOVERSION)" ]; then
		# The go.mod asked for a newer Go, which the go command switched to:
		# that toolchain downloads, in the project, as it always has.
		` + strings.Join(quoted, " ") + `
		touch /gobuild/switched
	else
		work=$(go env GOWORK)
		for f in $gomods; do [ ! -f "${f%.mod}.sum" ] || cat "${f%.mod}.sum" >> /gobuild/sums; done
		if [ -n "$work" ] && [ "$work" != off ] && [ -f "$work.sum" ]; then cat "$work.sum" >> /gobuild/sums; fi
	fi
}`
	first, err := base.
		WithMountedDirectory(mountPath, source.Filter(dagger.DirectoryFilterOpts{Include: include})).
		WithWorkdir(cwd).
		WithExec([]string{"sh", "-c", script}, bare).
		Sync(ctx)
	if err != nil {
		return nil, fmt.Errorf("go mod download: %w", withStderr(err))
	}
	sumFile, err := first.File("/gobuild/sums").Contents(ctx)
	if err != nil {
		return nil, err
	}
	start := first.Directory(modCacheDir)
	// What the go command downloaded before any module, such as a
	// toolchain, is recorded in the module cache it starts from.
	type sumsListing struct {
		sums map[string]string
		err  error
	}
	startSums := make(chan sumsListing, 1)
	go func() {
		sums, err := moduleSums(ctx, base, start)
		startSums <- sumsListing{sums, err}
	}()
	entries := parseGoSums([]string{sumFile})
	b.mods = start
	goenv, err := first.File("/gobuild/goenv").Contents(ctx)
	if err != nil {
		return nil, err
	}
	tags := buildTags(append(append([]string(nil), envFlags...), flags...))
	b.target = newTarget(goenv, tags)
	switched, err := first.Directory("/gobuild").Glob(ctx, "switched")
	if err != nil {
		return nil, err
	}
	if len(switched) > 0 {
		entries = nil
	} else {
		d := &downloader{public: public, private: caller, gomodcache: gomodcache, sums: entries, target: b.target, hdr: b.hdr}
		// The modules the go command fetches directly rather than through a
		// proxy: GONOPROXY's, or GOPRIVATE's when it's unset or empty.
		if d.privatePatterns, err = firstEnv(ctx, caller, "GONOPROXY", "GOPRIVATE"); err != nil {
			return nil, err
		}
		listed := <-imports
		if listed.err != nil {
			return nil, fmt.Errorf("source imports: %w", withStderr(listed.err))
		}
		src := parseSourceImports(d.target, listed.out)
		src.root = strings.TrimPrefix(strings.TrimPrefix(cwd, mountPath), "/")
		found, err := d.discoverOnce(ctx, toolImage, goenv, tags, src, pkgDirs)
		if err != nil {
			return nil, fmt.Errorf("go mod download: %w", err)
		}
		b.packs, b.zips, b.downloads = found.packs, found.zips, found.downloads
		dirs := found.dirs
		mods := strings.Join(dirs.mods, "\n")
		b.planMods = &mods
		// In vendor mode a package outside the source's modules is in its
		// vendor directory, which discover doesn't follow: the plan unpacks
		// every source directory.
		if mode, err := first.File("/gobuild/mode").Contents(ctx); err != nil {
			return nil, err
		} else if mode != "vendor" {
			src := strings.Join(dirs.src, "\n")
			b.planSrc = &src
		}
		b.fetch = d.lazyZip
	}
	listedSums := <-startSums
	if listedSums.err != nil {
		return nil, fmt.Errorf("start module cache checksums: %w", withStderr(listedSums.err))
	}
	b.sums = listedSums.sums
	for _, e := range entries {
		if e.zip != "" {
			b.sums[escapeModule(e.path)+"@"+escapeModule(e.version)] = e.zip
		}
	}
	// Built by the build's own toolchain, for every machine an engine runs
	// on.
	b.tools = machineTools("pack", b.target.platform(), func(goarch string) *dagger.File {
		return base.
			WithEnvVariable("GOOS", "linux").
			WithEnvVariable("GOARCH", goarch).
			WithEnvVariable("GOFLAGS", "").
			WithEnvVariable("GOCACHE", "/gobuild/cache").
			WithExec([]string{"go", "build", "-o", "/gobuild/pack", "cmd/pack"}, bare).
			File("/gobuild/pack")
	})
	// Go 1.25 and later run "go tool pack" for each package with assembly,
	// the runtime among them, so a build on them needs pack and its runtime
	// waits for it there: it's built alongside the plan instead. Earlier Go
	// runs the toolchain's own pack. An error shows again where it's used.
	if newTarget(goenv, nil).goMinor >= 25 {
		go b.tools.Sync(ctx)
	}
	return b, nil
}

// sourceSizes is the size of every Go file in the source, by its path in
// it. It is read in an exec of its own over the source as it is: a filtered
// copy of a large source costs more than the listing, which reruns when the
// source changes anyway and costs one exec then.
func (b *build) sourceSizes(ctx context.Context) (map[string]int64, error) {
	out, err := core.NewContainer().From(b.toolImage).
		WithMountedDirectory(hdrDir, b.hdr, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true}).
		WithMountedDirectory("/gobuild/src", b.src, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true}).
		WithExec([]string{hdrBin, "sizes", "/gobuild/src"}, bare).
		Stdout(ctx)
	if err != nil {
		return nil, err
	}
	return parseSizes(out)
}

// parseSizes parses "gohdr sizes" output: "<size> <path>" lines.
func parseSizes(out string) (map[string]int64, error) {
	sizes := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		n, file, ok := strings.Cut(line, " ")
		size, err := strconv.ParseInt(n, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("source sizes: %q", line)
		}
		sizes[file] = size
	}
	return sizes, nil
}

// weighSource weighs each package compiled from the source by its Go files'
// real sizes. The plan weighs what it sees of them, their headers, which is
// a fraction of a large package, so its chain would rank below the ones
// that wait on it far less. The scheduler reads weights only through
// heights; no exec's key depends on them.
func weighSource(blocks map[string]*block, weights map[string]int64, sizes map[string]int64) {
	for id, bl := range blocks {
		if len(bl.srcFiles) == 0 {
			continue
		}
		w := int64(8 << 10)
		for _, f := range bl.srcFiles {
			w += sizes[f]
		}
		weights[id] = max(weights[id], w)
	}
}

// canonical is from with ctr's environment and nothing else of ctr's.
func canonical(ctx context.Context, ctr *dagger.Container, from *dagger.Container) (*dagger.Container, error) {
	names, values, err := envOf(ctx, ctr)
	if err != nil {
		return nil, err
	}
	out := from
	for i, name := range names {
		out = out.WithEnvVariable(name, values[i])
	}
	return out, nil
}

// plan is go build's plan for pkg, made from headers, the header pack of the
// source's Go files, and the source's other files whole. Both are keyed by
// content, so an edit inside function bodies leaves the plan exec a cache hit.
// With it come the plan's step weights, by step id, from "gohdr weigh",
// which order the execs waiting for a slot (for the source's packages they
// weigh headers); the answers to its C compiler probes, from "gohdr cc"; and
// the list of the source's Go files in directories named like Go files.
func (b *build) plan(ctx context.Context, headers *dagger.File, flags []string, pkg string, test bool) (string, map[string]int64, ccInfo, *dagger.File, error) {
	cmd := []string{"go", "build", "-n"}
	if test {
		cmd = []string{"go", "test", "-c", "-n"}
	}
	cmd = append(cmd, flags...)
	cmd = append(cmd, "-o", planOut+"/out", pkg)
	p, err := b.planWith(ctx, headers, shJoin(cmd), nil)
	if err != nil {
		return "", nil, ccInfo{}, nil, err
	}
	return p.plan, p.weights, p.cc, p.goDirs, nil
}

// planSource is what a plan reads of the source whole: its files that
// aren't Go, which an edit inside a Go file leaves as they were.
func planSource(src *dagger.Directory) *dagger.Directory {
	return src.Filter(dagger.DirectoryFilterOpts{Exclude: []string{"**/*.go"}})
}

// planned is a plan with what plan reads with it, and the exec that made it.
type planned struct {
	ran     *dagger.Container
	plan    string
	weights map[string]int64
	cc      ccInfo
	goDirs  *dagger.File
}

// withModules mounts the module cache a plan reads: the go command's own,
// and, when the build downloaded modules, their plan packs, which
// unpackHeaders unpacks into it, with the list of the directories it
// unpacks.
func (b *build) withModules(ctr *dagger.Container) *dagger.Container {
	ctr = ctr.WithMountedDirectory(b.roots.Mod, b.mods)
	if b.packs != nil {
		ctr = ctr.WithMountedDirectory("/gobuild/modpacks", b.packs, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	if b.planMods != nil {
		ctr = ctr.WithNewFile("/gobuild/plandirs/mods", *b.planMods)
	}
	return ctr
}

// unpackHeaders is the script that unpacks, for a plan, the build's plan
// packs into the module cache, only in the directories the plan reads when
// they're known, and the pack of the source's headers the plan reads
// (planHeaders), mounted at /gobuild/src.hdr, into the source.
func (b *build) unpackHeaders() string {
	script := ""
	if b.packs != nil {
		script = hdrBin + " unpackall /gobuild/modpacks " + shQuote(b.roots.Mod)
		if b.planMods != nil {
			script += " /gobuild/plandirs/mods"
		}
		script += " && "
	}
	// The list is planHeaders', made from the whole pack.
	return script + hdrBin + " unpack /gobuild/src.hdr " + shQuote(b.roots.Src) + " /dev/null"
}

// planHeaders is what a plan reads of the source's Go files: a pack of the
// headers that "gohdr unpack" writes from the source's header pack with the
// directories the plan reads, when they're known (planDirs); and the whole
// pack's list of its Go files in directories named like Go files, the plan's
// goDirs.
//
// They are made by an exec of their own, and each is read from its evaluated
// output, so it is keyed by its content: the plan follows only the headers it
// reads. With the whole pack, the plan was keyed by every Go file's imports
// in the workspace: a change to any of them, another module's included, ran
// the plan again, and its go command, for the same plan.
func (b *build) planHeaders(ctx context.Context, headers *dagger.File) (*dagger.File, *dagger.File, error) {
	ctr := core.NewContainer().From(b.toolImage).
		WithMountedDirectory(hdrDir, b.hdr, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true}).
		WithMountedFile("/gobuild/src.hdr", headers, dagger.ContainerWithMountedFileOpts{ReadOnly: true})
	cmd := []string{hdrBin, "subset", "/gobuild/src.hdr", "/gobuild/hdr/plan.hdr", "/gobuild/hdr/godirs.txt"}
	if b.planSrc != nil {
		ctr = ctr.WithNewFile("/gobuild/plandirs/src", *b.planSrc)
		cmd = append(cmd, "/gobuild/plandirs/src")
	}
	out, err := ctr.WithExec([]string{"sh", "-c", "mkdir -p /gobuild/hdr && " + shJoin(cmd)}, bare).Directory("/gobuild/hdr").Sync(ctx)
	if err != nil {
		return nil, nil, err
	}
	pack, err := out.File("plan.hdr").Sync(ctx)
	if err != nil {
		return nil, nil, err
	}
	return pack, out.File("godirs.txt"), nil
}

// planWith makes a plan as plan does, with goCmd the go command that prints
// it. With manifestTool, write-manifest, the same exec records the go
// command's settings as the plan left them, for an archives manifest, in
// /gobuild/env.json: an exec of their own would cost another exec start.
func (b *build) planWith(ctx context.Context, headers *dagger.File, goCmd string, manifestTool *dagger.Directory) (*planned, error) {
	planHdr, goDirs, err := b.planHeaders(ctx, headers)
	if err != nil {
		return nil, err
	}
	base := b.base.WithMountedDirectory(hdrDir, b.hdr)
	env := ""
	if manifestTool != nil {
		base = base.WithMountedDirectory(writeManifestDir, manifestTool)
		env = " && " + writeManifestBin + " env > /gobuild/env.json"
	}
	ran := b.withModules(base).
		WithMountedDirectory(b.roots.Src, planSource(b.src)).
		WithMountedFile("/gobuild/src.hdr", planHdr).
		// An empty build cache, so the plan holds every step.
		WithEnvVariable("GOCACHE", "/gobuild/plancache").
		// A failed plan prints go build's error, such as a //go:embed
		// pattern it rejects, rather than leaving it in the plan file: the
		// end of go build's output, where it writes the error, and not the
		// plan before it, which a large build makes hundreds of KB long.
		WithExec([]string{"sh", "-c", b.unpackHeaders() +
			" && cd " + shQuote(b.roots.Cwd) + " && { " + goCmd + " 2> /gobuild/plan.txt" +
			" || { status=$?; tail -n 40 /gobuild/plan.txt >&2; exit $status; }; }" +
			" && " + hdrBin + " weigh /gobuild/plan.txt > /gobuild/weights.txt" +
			" && " + hdrBin + " cc /gobuild/plan.txt > /gobuild/cc.txt" + env}, bare)
	plan, err := ran.File("/gobuild/plan.txt").Contents(ctx)
	if err != nil {
		return nil, b.planError(ctx, err)
	}
	out, err := ran.File("/gobuild/weights.txt").Contents(ctx)
	if err != nil {
		return nil, err
	}
	ccOut, err := ran.File("/gobuild/cc.txt").Contents(ctx)
	if err != nil {
		return nil, err
	}
	weights := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		id, n, _ := strings.Cut(line, " ")
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("plan weights: %q: %w", line, err)
		}
		weights[id] = v
	}
	return &planned{ran: ran, plan: plan, weights: weights, cc: parseCC(ccOut), goDirs: goDirs}, nil
}

// testmains starts the step that writes the _testmain.go of each of pkgs'
// test binaries, whose test files are in the source's directories dirs, and
// which the blocks links link, in order, and returns what
// waits for it: each one's file, by the id of the block that links it. A
// plan never holds them, so go test runs as far as writing them: before the
// go command runs any tool, which -toolexec=false then stops. Each is in
// the work directory of the block that links it, as numbered in the plan,
// which is the same go test command. It reads the plan's view of the source
// with pkgs' own test files whole, as the test names and examples come from
// them, so it runs again when they change.
func (b *build) testmains(ctx context.Context, headers *dagger.File, flags, pkgs, dirs, links []string) func(id string) (*dagger.File, error) {
	written := sync.OnceValues(func() (*dagger.Directory, error) {
		var include []string
		for _, dir := range dirs {
			include = append(include, path.Join(dir, "*_test.go"))
		}
		tests := b.src.Filter(dagger.DirectoryFilterOpts{Include: include})
		cmd := []string{"go", "test", "-c", "-work", "-toolexec=false"}
		cmd = append(cmd, flags...)
		cmd = append(cmd, "-o", "/dev/null")
		cmd = append(cmd, pkgs...)
		planHdr, _, err := b.planHeaders(ctx, headers)
		if err != nil {
			return nil, err
		}
		return b.withModules(b.base.
			WithMountedDirectory(hdrDir, b.hdr)).
			WithMountedDirectory(b.roots.Src, planSource(b.src)).
			WithMountedFile("/gobuild/src.hdr", planHdr).
			WithMountedDirectory("/gobuild/tests", tests).
			WithEnvVariable("GOCACHE", "/gobuild/plancache").
			WithExec([]string{"sh", "-c", b.unpackHeaders() +
				" && cp -R /gobuild/tests/. " + shQuote(b.roots.Src) + "/" +
				" && cd " + shQuote(b.roots.Cwd) + " && { " + shJoin(cmd) + " 2> /gobuild/testmain.err; " +
				`work=$(sed -n 's/^WORK=//p' /gobuild/testmain.err); ` +
				`for id in ` + strings.Join(links, " ") + `; do ` +
				`[ -f "$work/$id/_testmain.go" ] || { cat /gobuild/testmain.err >&2; exit 1; }; ` +
				`mkdir -p /gobuild/testmains/$id && cp "$work/$id/_testmain.go" /gobuild/testmains/$id/; done; }`}, bare).
			Directory("/gobuild/testmains").Sync(ctx)
	})
	go written()
	return func(id string) (*dagger.File, error) {
		dir, err := written()
		if err != nil {
			return nil, err
		}
		return dir.File(id + "/_testmain.go"), nil
	}
}

// planError is a failed plan's error with go build's own message first, then
// the exec's, which holds its exit code. The plan read Go files as their
// headers, so a position go build gives in one, such as a //go:embed
// pattern's, is moved to the line the source has.
func (b *build) planError(ctx context.Context, err error) error {
	var execErr *dagger.ExecError
	if !errors.As(err, &execErr) || strings.TrimSpace(execErr.Stderr) == "" {
		return err
	}
	lines := strings.Split(strings.TrimSpace(execErr.Stderr), "\n")
	for i, l := range lines {
		m := rePosition.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		p := m[1]
		if !path.IsAbs(p) {
			p = path.Join(b.roots.Cwd, p)
		}
		rel, ok := strings.CutPrefix(p, b.roots.Src+"/")
		if !ok {
			continue
		}
		src, err := b.src.File(rel).Contents(ctx)
		if err != nil {
			continue
		}
		line, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		if line, col, ok := sourcePosition([]byte(src), line, col); ok {
			lines[i] = m[1] + ":" + strconv.Itoa(line) + ":" + strconv.Itoa(col) + m[4]
		}
	}
	// Not wrapped: a module's error that wraps an ExecError reaches the
	// caller as the exec's error alone, "exit code: N", without the message.
	return fmt.Errorf("%s: %s", strings.Join(lines, "\n"), err.Error())
}

// checkFlags rejects the go build flags, from the call and GOFLAGS, that
// can't make a binary here: -n prints the plan instead; -linkshared links
// against a shared standard library that go build rebuilds in its own
// container, so the binary can't run anywhere else; -buildmode=shared makes
// shared libraries, not a binary. gccgo builds don't come here: the go module
// builds them whole.
func checkFlags(flags []string) error {
	for i, f := range flags {
		name, value, ok := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if !ok && i+1 < len(flags) {
			value = flags[i+1]
		}
		switch {
		case name == "n":
			return fmt.Errorf("go build %s prints the plan instead of building a binary", f)
		case name == "linkshared":
			return fmt.Errorf("go build %s rebuilds the shared standard library in its own container, so the binary can't run anywhere else", f)
		case name == "buildmode" && value == "shared":
			return fmt.Errorf("go build %s builds shared libraries, not a binary", f)
		case name == "compiler" && value == "gccgo", name == "gccgoflags":
			return fmt.Errorf("%w: %s: a gccgo build is one whole go build, not a per-package one", errUnsupported, f)
		}
	}
	return nil
}

// lastFlag is the value of the go flag name, e.g. the alternate go.mod
// -modfile names, and whether it is set: the last flag wins, the call's over
// GOFLAGS'. An empty -modfile means the module's own go.mod.
func lastFlag(name string, flags, goflags []string) (string, bool) {
	last := func(flags []string) (string, bool) {
		v, set := "", false
		for i := 0; i < len(flags); i++ {
			f := flags[i]
			if !strings.HasPrefix(f, "-") {
				continue
			}
			n, value, ok := strings.Cut(strings.TrimLeft(f, "-"), "=")
			if n != name {
				continue
			}
			// A value in the next argument is not a flag of its own.
			if !ok && i+1 < len(flags) {
				i++
				value = flags[i]
			}
			v, set = value, true
		}
		return v, set
	}
	if v, ok := last(flags); ok {
		return v, true
	}
	return last(goflags)
}

// commandLineFlags reports whether go build flags apply to the packages
// named on the go command line, so that what they reach depends on which:
// -coverpkg, and -gcflags, -asmflags or -gccgoflags without a package
// pattern, which the go command reads from the value with its spaces
// trimmed (cmd/go/internal/load.PerPackageFlag): an empty value too, which
// clears the flags of the command line's packages alone.
func commandLineFlags(flags []string) bool {
	for i, f := range flags {
		name, value, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if !strings.HasPrefix(f, "-") {
			continue
		}
		if !hasValue && i+1 < len(flags) {
			value = flags[i+1]
		}
		switch name {
		case "coverpkg":
			return true
		case "gcflags", "asmflags", "gccgoflags":
			if v := strings.TrimSpace(value); v == "" || v[0] == '-' || !strings.Contains(v, "=") {
				return true
			}
		}
	}
	return false
}

// splitGOFLAGS splits GOFLAGS as the go command does (cmd/internal/quoted):
// spaces, tabs and newlines separate flags, and a flag that starts with a
// quote runs to the same quote, with nothing unescaped.
func splitGOFLAGS(s string) ([]string, error) {
	var flags []string
	for {
		s = strings.TrimLeft(s, " \t\n\r")
		if s == "" {
			return flags, nil
		}
		if q := s[0]; q == '"' || q == '\'' {
			i := strings.IndexByte(s[1:], q)
			if i < 0 {
				return nil, fmt.Errorf("unterminated %c string", q)
			}
			flags = append(flags, s[1:1+i])
			s = s[2+i:]
			continue
		}
		i := strings.IndexAny(s, " \t\n\r")
		if i < 0 {
			i = len(s)
		}
		flags = append(flags, s[:i])
		s = s[i:]
	}
}
