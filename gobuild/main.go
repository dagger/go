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
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"dagger/gobuild/internal/dagger"
)

type Gobuild struct{}

// toolImage builds gohdr, which runs in whatever image the build uses.
const toolImage = "golang:1.26-alpine"

// bare runs an exec as its command alone: none of these execs call Dagger,
// and starting a nested session for each would cost every package; and
// none needs Dagger's init process, whose start every package would pay:
// a script, or the go command, waits for what it starts, and a cancelled
// exec is stopped whole.
var bare = dagger.ContainerWithExecOpts{DisableDaggerInDagger: true, NoInit: true}

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
) (*dagger.Directory, error) {
	blocks, linked, err := run(ctx, base, image, from, source, headers, mountPath, workdir, pkg, flags, false)
	if err != nil {
		return nil, err
	}
	out := dag.Directory().WithFile(output, linked.file)
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
) (*dagger.File, error) {
	_, linked, err := run(ctx, base, image, from, source, headers, mountPath, workdir, pkg, flags, true)
	if err != nil {
		return nil, err
	}
	return linked.file, nil
}

// Archives compiles, one exec per package as Binary builds, every package of
// the module in workdir and every package it and its tests import, and
// returns a directory of their archives, the full output of "go tool compile
// -pack", under archives/, with manifest.json, which describes them for
// golangci-lint's export driver: the Go version, target and build tags they
// were compiled for, and for each package, by import path, its archive and a
// digest of what it was compiled from. The packages are those "go list -deps
// -test ./..." lists but test variants, external test packages and test
// mains: main packages too, which the driver loads plainly as well.
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
) (*dagger.Directory, error) {
	cwd := path.Join(mountPath, workdir)
	b, err := newBuild(ctx, base, image, from, source, mountPath, cwd, flags)
	if err != nil {
		return nil, err
	}
	goflags, err := b.base.EnvVariable(ctx, "GOFLAGS")
	if err != nil {
		return nil, err
	}
	envFlags, err := splitGOFLAGS(goflags)
	if err != nil {
		return nil, fmt.Errorf("GOFLAGS: %w", err)
	}
	// go list names the packages, from the plan's view of the source, which
	// has every import; go build -n compiles them, a main package without
	// its link. With none, go build would build ".", so it doesn't run, and
	// the plan is empty.
	list := append(append([]string{"go", "list", "-deps", "-test", "-json=" + listFields}, flags...), "./...")
	pre := shJoin(list) + " > /gobuild/list.json" +
		" && /gobuild/gohdr scope /gobuild/list.json > /gobuild/pkgs.txt" +
		" && go env -json GOVERSION GOOS GOARCH CGO_ENABLED > /gobuild/env.json && "
	goCmd := "{ [ ! -s /gobuild/pkgs.txt ] || " + shJoin(append([]string{"go", "build", "-n"}, flags...)) + " $(cat /gobuild/pkgs.txt); }"
	p, err := b.planWith(ctx, headers, pre, goCmd)
	if err != nil {
		return nil, err
	}
	blocks, err := parseArchivePlan(p.plan, b.roots)
	if err != nil {
		return nil, err
	}
	results, err := b.walk(ctx, blocks, p.weights, p.cc, p.goDirs, nil)
	if err != nil {
		return nil, err
	}
	pkgs, err := p.ran.File("/gobuild/pkgs.txt").Contents(ctx)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*block{}
	for _, bl := range blocks {
		if !bl.isProfile {
			byPath[bl.importPath] = bl
		}
	}
	// The digests are of the source whole, and of what cgo wrote for a cgo
	// package's compile, read from the package's exec.
	ctr := b.base.
		WithMountedFile("/gobuild/gohdr", hdrTool()).
		WithMountedDirectory(b.roots.Mod, b.mods).
		WithMountedDirectory(b.roots.Src, b.src)
	var entries strings.Builder
	var archives []*dagger.File
	for _, ip := range strings.Fields(pkgs) {
		bl, ok := byPath[ip]
		if !ok {
			return nil, fmt.Errorf("%s: no compile for it in the plan", ip)
		}
		r := results[bl.id]
		archives = append(archives, r.file)
		entries.WriteString(ip + " archives/" + bl.output())
		if bl.cgo {
			at := "/gobuild/cgo/" + bl.name
			ctr = ctr.WithMountedDirectory(at, r.dir)
			for _, f := range bl.cgoOutputs() {
				entries.WriteString(" " + at + "/" + f)
			}
		}
		entries.WriteString("\n")
	}
	manifest := ctr.
		WithMountedFile("/gobuild/list.json", p.ran.File("/gobuild/list.json")).
		WithMountedFile("/gobuild/env.json", p.ran.File("/gobuild/env.json")).
		WithNewFile("/gobuild/entries.txt", entries.String()).
		WithExec([]string{"sh", "-c", "/gobuild/gohdr manifest /gobuild/list.json /gobuild/env.json /gobuild/entries.txt " +
			shQuote(strings.Join(buildTags(append(envFlags, flags...)), ",")) + " > /gobuild/manifest.json"}, bare).
		File("/gobuild/manifest.json")
	return dag.Directory().WithFiles("archives", archives).WithFile("manifest.json", manifest), nil
}

// listFields are the fields of go list's output Archives reads: the
// packages, and the files their digests cover.
const listFields = "ImportPath,Dir,ForTest,GoFiles,CgoFiles,CFiles,CXXFiles,MFiles,HFiles,FFiles,SFiles,SwigFiles,SwigCXXFiles,SysoFiles,EmbedFiles"

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
func run(ctx context.Context, base *dagger.Container, image string, from *dagger.Container, source *dagger.Directory, headers *dagger.File, mountPath, workdir, pkg string, flags []string, test bool) (map[string]*block, *result, error) {
	cwd := path.Join(mountPath, workdir)
	if test {
		// Vet is go test's, not the binary's, and is left to the lint
		// checks: the per-package build doesn't replay it.
		flags = append([]string{"-vet=off"}, flags...)
	}
	b, err := newBuild(ctx, base, image, from, source, mountPath, cwd, flags)
	if err != nil {
		return nil, nil, err
	}
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
	results, err := b.walk(ctx, blocks, weights, cc, goDirs, func(w *walker) (err error) {
		if test {
			if w.testmain, err = b.testmain(ctx, headers, flags, pkg); err != nil {
				return fmt.Errorf("test main: %w", err)
			}
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
func (b *build) walk(ctx context.Context, blocks map[string]*block, weights map[string]int64, cc ccInfo, goDirs *dagger.File, prepare func(*walker) error) (map[string]*result, error) {
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
	for _, bl := range blocks {
		bl.addCFlags(cc)
	}
	sums, err := moduleSums(ctx, b.base, b.mods)
	if err != nil {
		return nil, fmt.Errorf("module checksums: %w", err)
	}
	mods := &modules{root: b.roots.Mod, all: b.mods, sums: sums}
	if err := mods.expandLoose(ctx, blocks); err != nil {
		return nil, err
	}
	heights := heightsOf(blocks, weights)
	mods.fetch(ctx, blocks, heights)
	// Go 1.25 and later run "go tool pack" for packages with assembly, the
	// runtime among the first: build the tool now, not when the runtime
	// waits for it. An error shows again where it's used.
	if needsTools(blocks) {
		go b.tools.Sync(ctx)
	}
	w := &walker{base: b.base, tools: b.tools, src: b.src, mods: mods, roots: b.roots, nonGo: nonGo}
	if prepare != nil {
		if err := prepare(w); err != nil {
			return nil, err
		}
	}
	// A PGO profile is preprocessed first: every compile reads it.
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
	nodes := map[string]*node{}
	for id, bl := range blocks {
		if bl.isProfile {
			continue
		}
		nodes[id] = toNode(bl, blocks, compileC(outside[id], weights[id]))
	}
	w.heights = heights
	return w.walk(ctx, nodes)
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
	base  *dagger.Container
	src   *dagger.Directory
	mods  *dagger.Directory
	tools *dagger.Directory
	roots roots
}

func newBuild(ctx context.Context, ctr *dagger.Container, image string, from *dagger.Container, source *dagger.Directory, mountPath, cwd string, flags []string) (*build, error) {
	var err error
	if image != "" {
		from = dag.Container().From(image)
	}
	if from != nil {
		if ctr, err = canonical(ctx, ctr, from); err != nil {
			return nil, err
		}
	}
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
	gomodcache, err := ctr.EnvVariable(ctx, "GOMODCACHE")
	if err != nil {
		return nil, err
	}
	if gomodcache == "" {
		out, err := ctr.WithExec([]string{"go", "env", "GOMODCACHE"}, bare).Stdout(ctx)
		if err != nil {
			return nil, err
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
	b := &build{base: base, src: source, roots: roots{Src: mountPath, Mod: gomodcache, Cwd: cwd}}
	// The module cache, downloaded from every go.mod and go.sum in the
	// source: the root's, and those a local replace or go.work points at,
	// or the alternate go.mod a -modfile flag names, with its go.sum.
	include := []string{"**/go.mod", "**/go.sum", "go.work", "go.work.sum", "**/vendor/modules.txt"}
	download := []string{"go", "mod", "download"}
	// The go.mod the build reads its go directive from: the alternate one,
	// or with none, the module's own, which the script asks go for.
	goMod := ""
	if mf, ok := lastFlag("modfile", flags, envFlags); ok && mf == "" {
		// An empty -modfile in the call means the module's own go.mod,
		// whatever GOFLAGS, which the download runs with, names.
		download = append(download, "-modfile=")
	} else if ok {
		if !path.IsAbs(mf) {
			mf = path.Join(cwd, mf)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(mf, mountPath), "/")
		include = append(include, rel, strings.TrimSuffix(rel, ".mod")+".sum")
		download = append(download, "-modfile="+mf)
		goMod = mf
	}
	// The cache directory is made first: a module with no dependencies
	// downloads nothing, and nothing else would create it.
	quoted := make([]string, len(download))
	for i, a := range download {
		quoted[i] = shQuote(a)
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
	start := ""
	if path.Clean(gomodcache) != modCacheDir {
		start = "\n[ ! -d " + shQuote(gomodcache) + " ] || [ " + shQuote(gomodcache) + " -ef " + modCacheDir + " ] || cp -a " + shQuote(gomodcache) + "/. " + modCacheDir + "/"
	}
	script := "set -e\nmkdir -p " + modCacheDir + start +
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
[ "$mode" = vendor ] || ` + strings.Join(quoted, " ")
	b.mods, err = base.
		WithMountedDirectory(mountPath, source.Filter(dagger.DirectoryFilterOpts{Include: include})).
		WithWorkdir(cwd).
		WithExec([]string{"sh", "-c", script}, bare).
		Directory(modCacheDir).Sync(ctx)
	if err != nil {
		return nil, fmt.Errorf("go mod download: %w", err)
	}
	// Built for the machine the execs run on, by the build's own toolchain.
	b.tools = base.
		WithoutEnvVariable("GOOS").
		WithoutEnvVariable("GOARCH").
		WithEnvVariable("GOFLAGS", "").
		WithEnvVariable("GOCACHE", "/gobuild/cache").
		WithExec([]string{"go", "build", "-o", toolsDir + "/", "cmd/pack"}, bare).
		Directory(toolsDir)
	return b, nil
}

// sourceSizes is the size of every Go file in the source, by its path in
// it. It is read in an exec of its own, keyed by the source's Go files and
// nothing else, so it reruns when a file changes and costs one exec then.
func (b *build) sourceSizes(ctx context.Context) (map[string]int64, error) {
	out, err := dag.Container().From(toolImage).
		WithMountedFile("/gobuild/gohdr", hdrTool(), dagger.ContainerWithMountedFileOpts{ReadOnly: true}).
		WithMountedDirectory("/gobuild/src", b.src.Filter(dagger.DirectoryFilterOpts{Include: []string{"**/*.go"}}), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true}).
		WithExec([]string{"/gobuild/gohdr", "sizes", "/gobuild/src"}, bare).
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
	vars, err := ctr.EnvVariables(ctx)
	if err != nil {
		return nil, err
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
	out := from
	for i := range vars {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = out.WithEnvVariable(names[i], values[i])
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
	p, err := b.planWith(ctx, headers, "", shJoin(cmd))
	if err != nil {
		return "", nil, ccInfo{}, nil, err
	}
	return p.plan, p.weights, p.cc, p.goDirs, nil
}

// planned is a plan with what plan reads with it, and the exec that made it.
type planned struct {
	ran     *dagger.Container
	plan    string
	weights map[string]int64
	cc      ccInfo
	goDirs  *dagger.File
}

// planWith makes a plan as plan does, with goCmd the go command that prints
// it, run after pre, which may be empty or a command ending in "&&".
func (b *build) planWith(ctx context.Context, headers *dagger.File, pre, goCmd string) (*planned, error) {
	ran := b.base.
		WithMountedFile("/gobuild/gohdr", hdrTool()).
		WithMountedDirectory(b.roots.Mod, b.mods).
		WithMountedDirectory(b.roots.Src, b.src.Filter(dagger.DirectoryFilterOpts{Exclude: []string{"**/*.go"}})).
		WithMountedFile("/gobuild/src.hdr", headers).
		// An empty build cache, so the plan holds every step.
		WithEnvVariable("GOCACHE", "/gobuild/plancache").
		// A failed plan prints go build's error, such as a //go:embed
		// pattern it rejects, rather than leaving it in the plan file.
		WithExec([]string{"sh", "-c", "/gobuild/gohdr unpack /gobuild/src.hdr " + shQuote(b.roots.Src) + " /gobuild/godirs.txt" +
			" && cd " + shQuote(b.roots.Cwd) + " && " + pre + "{ " + goCmd + " 2> /gobuild/plan.txt" +
			" || { status=$?; cat /gobuild/plan.txt >&2; exit $status; }; }" +
			" && /gobuild/gohdr weigh /gobuild/plan.txt > /gobuild/weights.txt" +
			" && /gobuild/gohdr cc /gobuild/plan.txt > /gobuild/cc.txt"}, bare)
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
	return &planned{ran: ran, plan: plan, weights: weights, cc: parseCC(ccOut), goDirs: ran.File("/gobuild/godirs.txt")}, nil
}

// testmain is the _testmain.go the go command writes for pkg's test binary.
// A plan never holds it, so go test runs as far as writing it: before the
// go command runs any tool, which -toolexec=false then stops. It reads the
// plan's view of the source with pkg's own test files whole, as the test
// names and examples come from them, so it runs again when they change.
func (b *build) testmain(ctx context.Context, headers *dagger.File, flags []string, pkg string) (*dagger.File, error) {
	dir := strings.TrimPrefix(strings.TrimPrefix(path.Join(b.roots.Cwd, pkg), b.roots.Src), "/")
	tests := b.src.Filter(dagger.DirectoryFilterOpts{Include: []string{path.Join(dir, "*_test.go")}})
	cmd := []string{"go", "test", "-c", "-work", "-toolexec=false"}
	cmd = append(cmd, flags...)
	cmd = append(cmd, "-o", "/dev/null", pkg)
	quoted := make([]string, len(cmd))
	for i, c := range cmd {
		quoted[i] = shQuote(c)
	}
	return b.base.
		WithMountedFile("/gobuild/gohdr", hdrTool()).
		WithMountedDirectory(b.roots.Mod, b.mods).
		WithMountedDirectory(b.roots.Src, b.src.Filter(dagger.DirectoryFilterOpts{Exclude: []string{"**/*.go"}})).
		WithMountedFile("/gobuild/src.hdr", headers).
		WithMountedDirectory("/gobuild/tests", tests).
		WithEnvVariable("GOCACHE", "/gobuild/plancache").
		WithExec([]string{"sh", "-c", "/gobuild/gohdr unpack /gobuild/src.hdr " + shQuote(b.roots.Src) + " /gobuild/godirs.txt" +
			" && cp -R /gobuild/tests/. " + shQuote(b.roots.Src) + "/" +
			" && cd " + shQuote(b.roots.Cwd) + " && { " + strings.Join(quoted, " ") + " 2> /gobuild/testmain.err; " +
			`work=$(sed -n 's/^WORK=//p' /gobuild/testmain.err); ` +
			`set -- "$work"/b*/_testmain.go; [ -f "$1" ] || { cat /gobuild/testmain.err >&2; exit 1; }; ` +
			`cp "$1" /gobuild/_testmain.go; }`}, bare).
		File("/gobuild/_testmain.go").Sync(ctx)
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

func needsTools(blocks map[string]*block) bool {
	for _, b := range blocks {
		for _, l := range b.lines {
			if strings.Contains(l, "go tool pack ") {
				return true
			}
		}
	}
	return false
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
