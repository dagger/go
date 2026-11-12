package main

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// Scoped is what a build reads of a source and of its header pack (Scope).
type Scoped struct {
	// The source's files the build reads.
	Source *dagger.Directory
	// The header pack of those files.
	Headers *dagger.File
}

// Scope is what a build of packages reads of source and headers: the files
// of the source directories its plan reads (planDirs), with every module's
// go.mod and go.sum, and the header pack the plan reads (planHeaders). Given
// them instead of the whole source and pack, a Binary, TestBinary,
// TestBinaries or Archives call is the same call after an edit of any other
// file, which Dagger answers from its cache: both are keyed by their
// content.
//
// A directory keeps its Go files, its .syso files and its default.pgo, or
// with a //go:embed line, everything below it; without tests, but where an
// embed may name them, not its test files, which the build doesn't compile.
// The whole source and pack are kept for a build of every package, which
// reads them whole, and when the build may read a file of the source this
// can't tell it reads: when one of those directories holds C, C++ or assembly,
// uses cgo or holds a symlink; when flags or the base's GOFLAGS or
// GOWORK name a file (fileFlags); when a package is named other than by a
// path below the build's directory; when the source holds a version control
// system's metadata, which the go command stamps a binary from, for a
// build without tests or with -buildvcs=true; in vendor mode; and when the
// go command switches to another toolchain.
func (m *Gobuild) Scope(
	ctx context.Context,
	// The Go container the build runs in, as for Binary.
	base *dagger.Container,
	// The image base was made from, as for Binary.
	// +optional
	image string,
	// The custom container base was made from, as for Binary.
	// +optional
	from *dagger.Container,
	// The source tree, as the build is given it.
	source *dagger.Directory,
	// The header pack of source's Go files, as the build is given it.
	headers *dagger.File,
	// Where source is mounted.
	mountPath string,
	// The directory the build runs in, relative to mountPath.
	// +default="."
	workdir string,
	// The build's package arguments: Binary's or TestBinary's package,
	// TestBinary's set, TestBinaries' packages, or Archives' packages.
	// Unset, or with a "..." pattern, every package of the module, for which
	// the whole source and pack are kept.
	// +optional
	packages []string,
	// Whether the build reads the packages' test files.
	// +optional
	tests bool,
	// The build's flags.
	// +optional
	flags []string,
	// The container module downloads run with, as for Binary.
	// +optional
	downloads *dagger.Container,
) (*Scoped, error) {
	whole := &Scoped{Source: source, Headers: headers}
	// A build of every package reads the module's whole source: the scope
	// would cost its own work and save nothing.
	pkgDirs, ok := packageDirs(workdir, packages)
	if !ok || pkgDirs == nil {
		return whole, nil
	}
	goflags, err := base.EnvVariable(ctx, "GOFLAGS")
	if err != nil {
		return nil, err
	}
	envFlags, err := splitGOFLAGS(goflags)
	if err != nil {
		return nil, fmt.Errorf("GOFLAGS: %w", err)
	}
	gowork, err := base.EnvVariable(ctx, "GOWORK")
	if err != nil {
		return nil, err
	}
	if fileFlags(append(envFlags, flags...)) || (gowork != "" && gowork != "off" && gowork != "auto") {
		return whole, nil
	}
	cwd := path.Join(mountPath, workdir)
	// C and assembly files, as the go module's source holds them
	// (goSourceInclude), and a version control system's metadata, which
	// the go command reads to stamp a binary, listed alongside the setup.
	type globbed struct {
		pattern string
		paths   []string
		err     error
	}
	natives := []string{"**/*.[chsS]", "**/*.[ch][ch]", "**/*.[ch]pp", "**/*.[ch]xx"}
	vcs := []string{"**/.git", "**/.hg", "**/.svn", "**/.bzr", "**/.fslckout", "**/_FOSSIL_"}
	// Directories named like a test file, which leaving test files out
	// mustn't take.
	const testNamed = "**/*_test.go"
	patterns := append(append(natives, vcs...), testNamed)
	listed := make(chan globbed, len(patterns))
	for _, p := range patterns {
		go func() {
			paths, err := source.Glob(ctx, p)
			listed <- globbed{p, paths, err}
		}()
	}
	b, err := setup(ctx, m.ToolImage, base, image, from, downloads, source, headers, pkgDirs, tests, mountPath, cwd, flags)
	if err != nil {
		return nil, err
	}
	var nativeFiles, testDirs []string
	versioned := false
	for range patterns {
		g := <-listed
		if g.err != nil {
			return nil, g.err
		}
		switch {
		case slices.Contains(vcs, g.pattern):
			versioned = versioned || len(g.paths) > 0
		case g.pattern == testNamed:
			for _, p := range g.paths {
				if strings.HasSuffix(p, "/") {
					testDirs = append(testDirs, strings.TrimSuffix(p, "/"))
				}
			}
		default:
			nativeFiles = append(nativeFiles, g.paths...)
		}
	}
	// The go command stamps a binary from version control metadata in go
	// build, not in go test unless -buildvcs=true asks it to, and a build
	// that links nothing, such as archives', has nothing to stamp.
	stamps := !tests || buildVCS(append(envFlags, flags...))
	if b.planSrc == nil || versioned && stamps {
		return whole, nil
	}
	dirs := strings.Split(*b.planSrc, "\n")
	include, exclude, ok := scopeInclude(dirs, b.imports.marks, nativeFiles, testDirs, tests)
	if !ok {
		return whole, nil
	}
	pack, _, err := b.planHeaders(ctx, headers, tests)
	if err != nil {
		return nil, err
	}
	if pack, err = pack.Sync(ctx, core.FileSyncOpts{KeyByContent: true}); err != nil {
		return nil, err
	}
	return &Scoped{Source: source.Filter(dagger.DirectoryFilterOpts{Include: include, Exclude: exclude}), Headers: pack}, nil
}

// packageDirs is the directories, relative to the source, of the packages
// a build in workdir names: none for every package below workdir, when one
// is a "..." pattern below it; not ok when one is no path relative to
// workdir, such as an import path, or reaches above workdir.
func packageDirs(workdir string, packages []string) (dirs []string, ok bool) {
	every := false
	for _, p := range packages {
		if p != "." && !strings.HasPrefix(p, "./") {
			return nil, false
		}
		if c := path.Clean(p); c == ".." || strings.HasPrefix(c, "../") {
			return nil, false
		}
		if strings.Contains(p, "...") {
			every = true
			continue
		}
		dirs = append(dirs, path.Join(workdir, p))
	}
	if every {
		return nil, true
	}
	return dirs, true
}

// scopeInclude is the include and exclude patterns of what a build reads of
// the source directories dirs (Scope), with marks each directory's marks
// (sourceImports) and natives the source's C, C++ and assembly files, as
// Directory.glob lists them; not ok when the build may read a file of the
// source outside them. Without tests, a directory leaves out its test
// files, which a build without tests doesn't compile, unless it or one
// above it embeds, which may name any file, or it holds one of testDirs,
// the source's directories named like a test file, which the pattern would
// leave out too.
func scopeInclude(dirs []string, marks map[string][]string, natives, testDirs []string, tests bool) (include, exclude []string, ok bool) {
	read := map[string]bool{}
	for _, d := range dirs {
		if d != "" {
			read[d] = true
		}
	}
	for _, f := range natives {
		// A directory named like one is none.
		if !strings.HasSuffix(f, "/") && read[path.Dir(f)] {
			return nil, nil, false
		}
	}
	embeds := func(d string) bool { return read[d] && slices.Contains(marks[d], "embed") }
	testNamed := map[string]bool{}
	for _, d := range testDirs {
		testNamed[path.Dir(d)] = true
	}
	include = []string{"**/go.mod", "**/go.sum", "go.work", "go.work.sum", "**/vendor/modules.txt"}
	for _, d := range slices.Sorted(maps.Keys(read)) {
		if slices.Contains(marks[d], "cgo") || slices.Contains(marks[d], "link") {
			return nil, nil, false
		}
		prefix := d + "/"
		if d == "." {
			prefix = ""
		}
		if embeds(d) {
			include = append(include, prefix+"**")
			continue
		}
		include = append(include, prefix+"*.go", prefix+"*.syso", prefix+"default.pgo")
		embedded := false
		for a := d; a != "." && !embedded; {
			a = path.Dir(a)
			embedded = embeds(a)
		}
		if !tests && !embedded && !testNamed[d] {
			exclude = append(exclude, prefix+"*_test.go")
		}
	}
	return include, exclude, true
}

// fileFlags reports whether go build flags name a file the build reads,
// which may be in the source: -C, -modfile, -overlay, -pgo with a profile,
// -toolexec, or a path in -gcflags, -asmflags or -ldflags, other than a -X
// value.
func fileFlags(flags []string) bool {
	for i, f := range flags {
		if !strings.HasPrefix(f, "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if !hasValue && i+1 < len(flags) {
			value = flags[i+1]
		}
		switch name {
		case "C", "overlay", "toolexec":
			return true
		case "modfile":
			if value != "" {
				return true
			}
		case "pgo":
			if value != "" && value != "auto" && value != "off" {
				return true
			}
		case "gcflags", "asmflags", "ldflags":
			// A package pattern before the flags, as in all=-N, is no path.
			if v := strings.TrimSpace(value); v != "" && v[0] != '-' {
				_, value, _ = strings.Cut(v, "=")
			}
			// Split as the go command splits them (cmd/internal/quoted).
			args, err := splitGOFLAGS(value)
			if err != nil {
				return true
			}
			for j := 0; j < len(args); j++ {
				if args[j] == "-X" {
					j++
					continue
				}
				if strings.HasPrefix(args[j], "-X=") {
					continue
				}
				if pathLike(args[j]) {
					return true
				}
			}
		}
	}
	return false
}

// buildVCS reports whether go build flags set -buildvcs to true, as the go
// command parses it (strconv.ParseBool, "auto" aside): the last -buildvcs
// wins, and one without a value is true.
func buildVCS(flags []string) bool {
	on := false
	for _, f := range flags {
		name, value, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if strings.HasPrefix(f, "-") && name == "buildvcs" {
			b, err := strconv.ParseBool(value)
			on = !hasValue || err == nil && b
		}
	}
	return on
}

// pathLike reports whether a tool flag argument holds a path: a part of it,
// split at "=" and ",", that starts with "/", "./" or "../".
func pathLike(arg string) bool {
	for _, p := range strings.FieldsFunc(arg, func(r rune) bool { return r == '=' || r == ',' }) {
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") {
			return true
		}
	}
	return false
}
