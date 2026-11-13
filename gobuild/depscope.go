package main

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// depsModule is the module path of the source dependencyScope makes: no
// module's, so none of the dependencies is in it.
const depsModule = "gobuild.invalid/deps"

// dependencyScope is what a build of only the dependencies of the package in
// pkgDir reads, as Archives with dependencies and without tests builds them:
// the module's go.mod and go.sum, with another module path, and in pkgDir a
// package that imports exactly what the package's own imports reach outside
// the module, a main package if it is one. The dependencies' archives,
// listing and manifest depend on nothing else of the module, so any module
// with the same requirements and the same imports outside it, such as a
// later one with the same generated code, makes the same call. Not ok when
// the module's own packages may reach their dependencies some other way, or
// when the go command would report an error in them that the new package
// doesn't have (outsideImports); when the source holds a go.work or a
// vendor directory; when a dependency requires the module, which its go.sum
// then names, so one may import the module's packages back; or when the
// build may run another toolchain than the base's.
func (m *Gobuild) dependencyScope(ctx context.Context, base *dagger.Container, env map[string]string, source *dagger.Directory, headers *dagger.File, workdir, pkgDir string, flags []string) (*Scoped, bool, error) {
	// What the source's packages import, listed alongside the rest: on a
	// fresh engine its exec waits on gohdr's build.
	platform, err := targetPlatform(ctx, base)
	if err != nil {
		return nil, false, err
	}
	type listing struct {
		out string
		err error
	}
	listed := make(chan listing, 1)
	go func() {
		out, err := core.NewContainer().From(m.ToolImage).
			WithMountedDirectory(hdrDir, hdrTools(m.ToolImage, platform), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true}).
			WithMountedFile("/gobuild/src.hdr", headers, dagger.ContainerWithMountedFileOpts{ReadOnly: true}).
			WithExec([]string{hdrBin, "imports", "/gobuild/src.hdr", "build"}, bare).
			Stdout(ctx)
		listed <- listing{out, err}
	}()
	// C, C++ and other sources the go command refuses without cgo, and a
	// main package's profile, which -pgo=auto applies to its dependencies
	// too, listed alongside the rest.
	type globbed struct {
		paths []string
		err   error
	}
	extra := []string{"**/*.c", "**/*.cc", "**/*.cpp", "**/*.cxx", path.Join(pkgDir, "default.pgo")}
	found := make(chan globbed, len(extra))
	for _, p := range extra {
		go func() {
			paths, err := source.Glob(ctx, p)
			found <- globbed{paths, err}
		}()
	}
	for _, p := range []string{"**/go.work", path.Join(workdir, "vendor/modules.txt")} {
		found, err := source.Glob(ctx, p)
		if err != nil {
			return nil, false, err
		}
		if len(found) > 0 {
			return nil, false, nil
		}
	}
	goMod, err := source.File(path.Join(workdir, "go.mod")).Contents(ctx)
	if err != nil {
		return nil, false, nil
	}
	modPath, goMinor, toolchain := goModInfo(goMod)
	if modPath == "" || toolchain || namesItself(goMod, modPath) {
		// A replace or require of the module's own path may give a
		// dependency that imports it back a version of it to build against.
		return nil, false, nil
	}
	if v := env["GOTOOLCHAIN"]; v != "" && v != "auto" && v != "local" && !strings.HasSuffix(v, "+auto") {
		return nil, false, nil
	}
	envFlags, err := splitGOFLAGS(env["GOFLAGS"])
	if err != nil {
		return nil, false, fmt.Errorf("GOFLAGS: %w", err)
	}
	all := append(envFlags, flags...)
	if mode, _ := lastFlag("mod", flags, envFlags); mode == "mod" || patternFlags(all) {
		// -mod=mod would fetch a checksum the go.sum lacks, which the stand-in
		// must fail on (a dependency importing the module back); a flag for a
		// package pattern may name the module's own packages.
		return nil, false, nil
	}
	t := newTarget(strings.Join([]string{env["GOOS"], env["GOARCH"], env["CGO_ENABLED"], env["GOVERSION"]}, "\n"), nil)
	if goMinor > t.goMinor {
		// The go.mod asks for a newer Go, which the go command switches to.
		return nil, false, nil
	}
	if slices.Contains(buildTags(all), "ignore") {
		return nil, false, nil
	}
	sum, err := source.File(path.Join(workdir, "go.sum")).Contents(ctx)
	if err != nil {
		sum = ""
	}
	if namesModule(sum, modPath) {
		// A dependency requires the module, so one may import its
		// packages back: those resolve to the module's own source.
		return nil, false, nil
	}
	l := <-listed
	if l.err != nil {
		return nil, false, fmt.Errorf("source imports: %w", withStderr(l.err))
	}
	var natives []string
	pgo := false
	for range extra {
		g := <-found
		if g.err != nil {
			return nil, false, g.err
		}
		for _, f := range g.paths {
			if strings.HasSuffix(f, "default.pgo") {
				pgo = true
			} else if !strings.HasSuffix(f, "/") {
				natives = append(natives, path.Dir(f))
			}
		}
	}
	imports, main, reached, ok := outsideImports(parseSourceImports(target{}, l.out), parseSourceFiles(l.out), modPath, workdir, pkgDir)
	if !ok || slices.ContainsFunc(natives, func(d string) bool { return reached[d] }) {
		return nil, false, nil
	}
	if p, _ := lastFlag("pgo", flags, envFlags); main && pgo && p != "off" {
		return nil, false, nil
	}
	var pkg strings.Builder
	if main {
		// A main package's build also links, so the go command adds the
		// runtime and what it links with to its dependencies.
		pkg.WriteString("package main\n")
	} else {
		pkg.WriteString("package deps\n")
	}
	if len(imports) > 0 {
		pkg.WriteString("\nimport (\n")
		for _, ip := range imports {
			pkg.WriteString("\t_ " + strconv.Quote(ip) + "\n")
		}
		pkg.WriteString(")\n")
	}
	if main {
		pkg.WriteString("\nfunc main() {}\n")
	}
	mod := withModulePath(goMod, depsModule)
	deps := core.NewDirectory().
		WithNewFile(path.Join(workdir, "go.mod"), mod).
		WithNewFile(path.Join(pkgDir, "deps.go"), pkg.String())
	if sum != "" {
		deps = deps.WithNewFile(path.Join(workdir, "go.sum"), sum)
	}
	var hdr strings.Builder
	hdr.WriteString("gohdr 1\n")
	for _, f := range [][2]string{{path.Join(workdir, "go.mod"), mod}, {path.Join(pkgDir, "deps.go"), pkg.String()}} {
		fmt.Fprintf(&hdr, "%s\n644\n%d\n%s\n", f[0], len(f[1]), f[1])
	}
	pack := core.NewDirectory().WithNewFile("deps.hdr", hdr.String()).File("deps.hdr")
	return &Scoped{Source: deps, Headers: pack, StandIn: true}, true, nil
}

// sourceFile is a Go file of the source as gohdr imports lists it: its
// name, its //go:build expression or "-", and its imports.
type sourceFile struct {
	name, expr string
	imports    []string
}

// parseSourceFiles is gohdr imports' Go files, by directory.
func parseSourceFiles(out string) map[string][]sourceFile {
	files := map[string][]sourceFile{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Split(line, "\t"); len(f) == 4 {
			files[f[0]] = append(files[f[0]], sourceFile{name: f[1], expr: f[2], imports: strings.Fields(f[3])})
		}
	}
	return files
}

// outsideImports is what the package in pkgDir imports outside its module,
// at workdir, through the module's own packages, sorted, and whether that
// package is a main package. It reads no target: not ok when a file of
// those packages has a build constraint or a file name a target selects,
// so every target builds all of them, as when one of them holds no
// package, uses cgo, embeds, holds a symlink, has a header that doesn't
// parse, declares two package names or a main package it imports, or
// they import each other in a cycle; or when an import is a package of
// another module of the source, or of a directory replacing a module, or
// another module's internal package. A file whose constraint is "ignore"
// alone builds for no target and is left out.
func outsideImports(src sourceImports, files map[string][]sourceFile, modPath, workdir, pkgDir string) (imports []string, main bool, reached map[string]bool, ok bool) {
	if len(src.repls) > 0 {
		return nil, false, nil, false
	}
	// importPath is the import path of the package in dir, in the module.
	importPath := func(dir string) string {
		if dir == workdir {
			return modPath
		}
		return modPath + "/" + strings.TrimPrefix(dir, workdir+"/")
	}
	// own reports whether dir is in the module at workdir: the innermost
	// module of the source that holds it is that one.
	own := func(dir string) bool {
		inner, depth := "", -1
		for _, m := range src.mods {
			d := 0
			if m.dir != "." {
				d = strings.Count(m.dir, "/") + 1
			}
			if (m.dir == "." || dir == m.dir || strings.HasPrefix(dir, m.dir+"/")) && d > depth {
				inner, depth = m.dir, d
			}
		}
		return depth >= 0 && inner == workdir
	}
	imported := map[string]bool{}
	const visiting, done = 1, 2
	state := map[string]int{}
	var visit func(dir string) bool
	visit = func(dir string) bool {
		switch state[dir] {
		case visiting:
			return false // a cycle, which the go command reports
		case done:
			return true
		}
		state[dir] = visiting
		if len(src.names[dir]) != 1 || src.names[dir][0] == "main" && dir != pkgDir {
			return false
		}
		for _, m := range []string{"cgo", "embed", "link", "unparsed"} {
			if slices.Contains(src.marks[dir], m) {
				return false
			}
		}
		built := 0
		for _, f := range files[dir] {
			if f.expr == "ignore" || strings.HasPrefix(f.name, "_") || strings.HasPrefix(f.name, ".") {
				// The go command builds it for no target.
				continue
			}
			if f.expr != "-" || osArchNamed(f.name) {
				return false
			}
			built++
			for _, ip := range f.imports {
				if d, ok := src.dirOf(ip); ok {
					if !own(d) || !internalVisible(importPath(dir), ip) || !visit(d) {
						return false
					}
					continue
				}
				for _, m := range src.mods {
					if ip == m.path || strings.HasPrefix(ip, m.path+"/") {
						// A module of the source names it, but no package
						// of the source is it: the build's own error.
						return false
					}
				}
				if e := "/" + ip + "/"; strings.Contains(e, "/internal/") {
					return false
				}
				imported[ip] = true
			}
		}
		if built == 0 {
			// The go command reports a package with no file to build.
			return false
		}
		state[dir] = done
		return true
	}
	if !own(pkgDir) || !visit(pkgDir) {
		return nil, false, nil, false
	}
	imports = make([]string, 0, len(imported))
	for ip := range imported {
		imports = append(imports, ip)
	}
	slices.Sort(imports)
	reached = map[string]bool{}
	for d := range state {
		reached[d] = true
	}
	return imports, src.names[pkgDir][0] == "main", reached, true
}

// internalVisible reports whether the package at import path importer may
// import ip, as the go command allows an internal package: from within the
// tree rooted at the parent of ip's last "internal" element.
func internalVisible(importer, ip string) bool {
	elems := strings.Split(ip, "/")
	last := -1
	for i, e := range elems {
		if e == "internal" {
			last = i
		}
	}
	if last < 0 {
		return true
	}
	parent := strings.Join(elems[:last], "/")
	return parent == "" || importer == parent || strings.HasPrefix(importer, parent+"/")
}

// patternFlags reports whether go build flags hold a per-package flag
// with a package pattern other than all or std, which may name the
// module's own packages: -gcflags=example.com/m/...=-N.
func patternFlags(flags []string) bool {
	for i, f := range flags {
		if !strings.HasPrefix(f, "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if !hasValue && i+1 < len(flags) {
			value = flags[i+1]
		}
		switch name {
		case "gcflags", "asmflags", "ldflags", "gccgoflags":
			v := strings.TrimSpace(value)
			if v == "" || v[0] == '-' {
				continue
			}
			if pattern, _, ok := strings.Cut(v, "="); ok && pattern != "all" && pattern != "std" {
				return true
			}
		}
	}
	return false
}

// osArchNamed reports whether a Go file's name has a GOOS or GOARCH suffix,
// which builds it for that target alone.
func osArchNamed(name string) bool {
	name = strings.TrimSuffix(name, ".go")
	i := strings.Index(name, "_")
	if i < 0 {
		return false
	}
	l := strings.Split(name[i:], "_")
	if n := len(l); n > 0 && l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	return n >= 1 && (knownOS[l[n-1]] || knownArch[l[n-1]])
}

// namesItself reports whether a go.mod names its own module path anywhere
// but in its module directive, as a replace or a require of it.
func namesItself(goMod, modPath string) bool {
	seen := false
	for _, line := range strings.Split(goMod, "\n") {
		for _, f := range strings.Fields(line) {
			if strings.Trim(f, "\"`") == modPath {
				if seen {
					return true
				}
				seen = true
			}
		}
	}
	return false
}

// namesModule reports whether a go.sum has a line for the module modPath,
// which some dependency then requires.
func namesModule(sum, modPath string) bool {
	for _, line := range strings.Split(sum, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == modPath {
			return true
		}
	}
	return false
}

// goModInfo is a go.mod's module path, the minor version of its go
// directive, and whether it has a toolchain directive.
func goModInfo(goMod string) (modPath string, goMinor int, toolchain bool) {
	for _, line := range strings.Split(goMod, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "module":
			if modPath == "" {
				modPath = strings.Trim(f[1], "\"`")
			}
		case "go":
			if _, rest, ok := strings.Cut(f[1], "."); ok {
				minor, _, _ := strings.Cut(rest, ".")
				goMinor, _ = strconv.Atoi(minor)
			}
		case "toolchain":
			toolchain = true
		}
	}
	return modPath, goMinor, toolchain
}

// withModulePath is a go.mod with its module directive naming modPath.
func withModulePath(goMod, modPath string) string {
	lines := strings.Split(goMod, "\n")
	for i, line := range lines {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "module" {
			lines[i] = "module " + modPath
			break
		}
	}
	return strings.Join(lines, "\n")
}
