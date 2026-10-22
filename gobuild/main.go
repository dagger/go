// A private helper of the go module: it builds a main package with one exec
// per package, so Dagger caches each package's compile separately and an
// edit re-runs only what it reaches.
//
// It replays the go command's own plan, "go build -n", made from a header
// pack of the source, so an edit inside function bodies doesn't re-plan.
// Packages outside the source (the standard library and dependencies) are
// built in tiers, one exec each, so an unchanged dependency graph is a
// cache hit and a cold one costs a container or two: the heavy tail of
// their heaviest chain, when there is one, builds in a tier of its own.
// Workspace packages are built one exec each, and a package whose archive
// comes out unchanged leaves its dependents cached.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"dagger/gobuild/internal/dagger"
)

type Gobuild struct{}

// toolImage builds gohdr, which runs in whatever image the build uses.
const toolImage = "golang:1.26-alpine"

// noNest runs an exec without Dagger-in-Dagger: none of these execs call
// Dagger, and starting a nested session for each would cost every package.
var noNest = dagger.ContainerWithExecOpts{DisableDaggerInDagger: true}

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
	cwd := path.Join(mountPath, workdir)
	b, err := newBuild(ctx, base, image, source, mountPath, cwd, flags)
	if err != nil {
		return nil, err
	}
	plan, weights, cc, err := b.plan(ctx, headers, flags, pkg)
	if err != nil {
		return nil, err
	}
	blocks, err := parsePlan(plan, b.roots)
	if err != nil {
		return nil, err
	}
	for _, bl := range blocks {
		bl.addCFlags(cc)
	}
	parts, names := heavyTiers(blocks, weights)
	heights := heightsOf(blocks, outsideSet(blocks), weights)
	if err := expandHeaders(ctx, b.mods, blocks); err != nil {
		return nil, err
	}
	// Go 1.25 and later run "go tool pack" for packages with assembly, the
	// runtime among the first: build the tool now, not when the runtime
	// waits for it. An error shows again where it's used.
	if needsTools(blocks) {
		go b.tools.Sync(ctx)
	}
	w := &walker{base: b.base, tools: b.tools, src: source, mods: b.mods, roots: b.roots}
	// A PGO profile is preprocessed first: every compile reads it, in the
	// tiers and in the walk.
	for _, bl := range blocks {
		if bl.isProfile {
			if w.profile, err = b.profileStep(ctx, bl, blocks); err != nil {
				return nil, fmt.Errorf("pgo profile: %w", err)
			}
		}
	}
	// The heavy tier starts once the light tier it reads is done, and each
	// workspace package once the tiers it imports are.
	outside := map[string]string{}
	for k, part := range parts {
		for id := range part {
			outside[id] = names[k]
		}
	}
	w.tiers = map[string]*tierResult{}
	for _, name := range names {
		w.tiers[name] = &tierResult{done: make(chan struct{})}
	}
	for k, part := range parts {
		go func() {
			r := w.tiers[names[k]]
			defer close(r.done)
			other := map[string]string{}
			for id, t := range outside {
				if t != names[k] {
					other[id] = t
				}
			}
			reads := map[string]bool{}
			for id := range part {
				for _, d := range blocks[id].deps {
					if t, ok := other[d]; ok {
						reads[t] = true
					}
				}
			}
			lower := map[string]*dagger.Directory{}
			for t := range reads {
				lt := w.tiers[t]
				<-lt.done
				if lt.err != nil {
					r.err = lt.err
					return
				}
				lower[t] = lt.dir
			}
			r.dir, r.err = b.tier(ctx, blocks, part, other, lower, heights, w.profile)
			if r.err != nil {
				r.err = fmt.Errorf("tier %s: %w", names[k], r.err)
			}
		}()
	}
	nodes := map[string]*node{}
	for id, bl := range blocks {
		if _, ok := outside[id]; !ok && !bl.isProfile {
			nodes[id] = toNode(bl, blocks, outside, soloCompileC)
		}
	}
	results, err := w.walk(ctx, nodes)
	if err != nil {
		return nil, err
	}
	linked := results[mainID]
	out := dag.Directory().WithFile(output, linked.file)
	if blocks[mainID].exportsHeader() {
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

// tier builds a tier's packages in one exec: gohdr runs their scripts in
// dependency order, as many at once as the machine has CPUs, the way go build
// runs its own actions, so the tier costs one container, not one per package.
// Its packages read each other's archives from /work/lib, and those of other
// tiers (other, by package) from lower, mounted at /work/t/<tier>. It returns
// the tier's archives, each named <name>.a. heights orders its packages, as
// heightsOf describes.
func (b *build) tier(ctx context.Context, blocks map[string]*block, part map[string]bool, other map[string]string, lower map[string]*dagger.Directory, heights map[string]int64, profile *profileOutput) (*dagger.Directory, error) {
	type runNode struct {
		Name, Pkg, Output, Script string
		Deps                      []string
		Height                    int64
	}
	spec := struct {
		Lib   string
		Nodes []runNode
	}{Lib: "/work/lib"}
	mods, tools, prof := false, false, false
	for id := range part {
		n := toNode(blocks[id], blocks, other, tierCompileC)
		spec.Nodes = append(spec.Nodes, runNode{Name: n.Name, Pkg: n.ImportPath,
			Output: n.Output, Script: n.Script, Deps: n.DepNames, Height: heights[id]})
		mods = mods || len(n.ModFiles) > 0
		tools = tools || n.Tools
		prof = prof || n.Profile
	}
	// Sorted, so the same tier is the same exec.
	sort.Slice(spec.Nodes, func(i, j int) bool { return spec.Nodes[i].Name < spec.Nodes[j].Name })
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	ctr := b.base.WithMountedFile("/gobuild/gohdr", hdrTool())
	if mods {
		ctr = ctr.WithMountedDirectory(b.roots.Mod, b.mods)
	}
	if tools {
		ctr = ctr.WithMountedDirectory(toolsDir, b.tools, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	if prof {
		ctr = ctr.WithMountedDirectory(profile.path, profile.dir, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	lowerNames := make([]string, 0, len(lower))
	for t := range lower {
		lowerNames = append(lowerNames, t)
	}
	sort.Strings(lowerNames)
	for _, t := range lowerNames {
		ctr = ctr.WithMountedDirectory("/work/t/"+t, lower[t], dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
	}
	return ctr.WithNewFile("/gobuild/tier.json", string(data)).
		WithExec([]string{"/gobuild/gohdr", "run", "/gobuild/tier.json"}, noNest).
		Directory("/work/lib").Sync(ctx)
}

// profileStep runs the plan's PGO profile preprocessing, from the profile in
// the source, and returns its output for the execs whose compiles read it.
func (b *build) profileStep(ctx context.Context, bl *block, blocks map[string]*block) (*profileOutput, error) {
	ctr := b.base
	if len(bl.srcFiles) > 0 {
		srcDir, at := selected(b.src, bl.srcFiles)
		ctr = ctr.WithMountedDirectory(path.Join(b.roots.Src, at), srcDir)
	}
	if len(bl.modFiles) > 0 {
		ctr = ctr.WithMountedDirectory(b.roots.Mod, fileTree(b.mods, bl.modFiles))
	}
	out := "/work/" + bl.name
	dir, err := ctr.WithExec([]string{"sh", "-c", bl.script(blocks, nil, soloCompileC)}, noNest).
		Directory(out).Sync(ctx)
	if err != nil {
		return nil, err
	}
	return &profileOutput{dir: dir, path: out}, nil
}

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

func newBuild(ctx context.Context, ctr *dagger.Container, image string, source *dagger.Directory, mountPath, cwd string, flags []string) (*build, error) {
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
	if image != "" {
		if ctr, err = canonical(ctx, ctr, image); err != nil {
			return nil, err
		}
	}
	gomodcache, err := ctr.EnvVariable(ctx, "GOMODCACHE")
	if err != nil {
		return nil, err
	}
	if gomodcache == "" {
		out, err := ctr.WithExec([]string{"go", "env", "GOMODCACHE"}, noNest).Stdout(ctx)
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
	script := "set -e\nmkdir -p " + shQuote(gomodcache) + "\nmode=" + shQuote(mode) + "\nsrc=" + shQuote(mountPath) + "\ngomod=" + shQuote(goMod) + `
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
		WithExec([]string{"sh", "-c", script}, noNest).
		Directory(gomodcache).Sync(ctx)
	if err != nil {
		return nil, fmt.Errorf("go mod download: %w", err)
	}
	// Built for the machine the execs run on, by the build's own toolchain.
	b.tools = base.
		WithoutEnvVariable("GOOS").
		WithoutEnvVariable("GOARCH").
		WithEnvVariable("GOFLAGS", "").
		WithEnvVariable("GOCACHE", "/gobuild/cache").
		WithExec([]string{"go", "build", "-o", toolsDir + "/", "cmd/pack"}, noNest).
		Directory(toolsDir)
	return b, nil
}

// canonical is image with ctr's environment and nothing else.
func canonical(ctx context.Context, ctr *dagger.Container, image string) (*dagger.Container, error) {
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
	out := dag.Container().From(image)
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
// With it come the plan's step weights, by step id, from "gohdr weigh": for
// the source's packages they weigh headers, so only the others' are used;
// and the answers to its C compiler probes, from "gohdr cc".
func (b *build) plan(ctx context.Context, headers *dagger.File, flags []string, pkg string) (string, map[string]int64, ccInfo, error) {
	cmd := []string{"go", "build", "-n"}
	cmd = append(cmd, flags...)
	cmd = append(cmd, "-o", planOut+"/out", pkg)
	quoted := make([]string, len(cmd))
	for i, c := range cmd {
		quoted[i] = shQuote(c)
	}
	ran := b.base.
		WithMountedFile("/gobuild/gohdr", hdrTool()).
		WithMountedDirectory(b.roots.Mod, b.mods).
		WithMountedDirectory(b.roots.Src, b.src.Filter(dagger.DirectoryFilterOpts{Exclude: []string{"**/*.go"}})).
		WithMountedFile("/gobuild/src.hdr", headers).
		// An empty build cache, so the plan holds every step.
		WithEnvVariable("GOCACHE", "/gobuild/plancache").
		// A failed plan prints go build's error, such as a //go:embed
		// pattern it rejects, rather than leaving it in the plan file.
		WithExec([]string{"sh", "-c", "/gobuild/gohdr unpack /gobuild/src.hdr " + shQuote(b.roots.Src) +
			" && cd " + shQuote(b.roots.Cwd) + " && { " + strings.Join(quoted, " ") + " 2> /gobuild/plan.txt" +
			" || { status=$?; cat /gobuild/plan.txt >&2; exit $status; }; }" +
			" && /gobuild/gohdr weigh /gobuild/plan.txt > /gobuild/weights.txt" +
			" && /gobuild/gohdr cc /gobuild/plan.txt > /gobuild/cc.txt"}, noNest)
	plan, err := ran.File("/gobuild/plan.txt").Contents(ctx)
	if err != nil {
		return "", nil, ccInfo{}, b.planError(ctx, err)
	}
	out, err := ran.File("/gobuild/weights.txt").Contents(ctx)
	if err != nil {
		return "", nil, ccInfo{}, err
	}
	ccOut, err := ran.File("/gobuild/cc.txt").Contents(ctx)
	if err != nil {
		return "", nil, ccInfo{}, err
	}
	weights := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		id, n, _ := strings.Cut(line, " ")
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return "", nil, ccInfo{}, fmt.Errorf("plan weights: %q: %w", line, err)
		}
		weights[id] = v
	}
	return plan, weights, parseCC(ccOut), nil
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
