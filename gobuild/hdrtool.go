package main

import (
	"sync"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// hdrTool builds gohdr, the helper's in-container tool. "gohdr unpack" writes
// out a header pack, the go module's scan of each Go file up to the end of its
// imports, for the plan, and lists the pack's Go files in directories named
// like Go files. "gohdr weigh" weighs the plan's steps by the Go
// source they compile. "gohdr cc" answers the plan's C compiler probes.
// "gohdr linkid" makes a link's build ID. "gohdr manifest" describes a lint
// target's archives for golangci-lint's export driver.
//
// It is one object for the whole module, so the SDK asks the engine for its
// ID once, not once for every exec that mounts it.
var hdrTool = sync.OnceValue(func() *dagger.File {
	// A standalone tool, so it is built with a pinned toolchain, statically,
	// and runs in whatever image the module builds with.
	return core.NewContainer().From(toolImage).
		WithEnvVariable("CGO_ENABLED", "0").
		WithNewFile("/gohdr-src/go.mod", "module gohdr\n\ngo 1.26\n").
		WithNewFile("/gohdr-src/main.go", hdrToolSrc).
		WithWorkdir("/gohdr-src").
		WithExec([]string{"go", "build", "-o", "/gohdr", "."}, bare).File("/gohdr")
})

const hdrToolSrc = `package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// unpack writes out the pack at src under dst, and writes to list, one per
// line, the pack's Go files in a directory named like a Go file, at any
// depth. With dirs, a file listing directories of the pack a line each, it
// writes only the Go files the plan reads (planFiles).
func unpack(src, dst, list string, dirs ...string) error {
	files, listed, err := readPack(src, dst)
	if err != nil {
		return err
	}
	if files, err = planFiles(files, dirs); err != nil {
		return err
	}
	if err := writeFiles(files); err != nil {
		return err
	}
	return os.WriteFile(list, []byte(listed), 0o644)
}

// unpackAll unpacks every .gohdr pack in dir under dst, all in one process
// and at once, with dirs as unpack takes them, of every pack.
func unpackAll(dir, dst string, dirs ...string) error {
	packs, err := filepath.Glob(filepath.Join(dir, "*.gohdr"))
	if err != nil {
		return err
	}
	var files []packFile
	for _, p := range packs {
		f, _, err := readPack(p, dst)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		files = append(files, f...)
	}
	if files, err = planFiles(files, dirs); err != nil {
		return err
	}
	return writeFiles(files)
}

// planFiles is files without the Go files a plan doesn't read, with dirs
// one file, the list of the directories it reads Go files in: those of the
// packages it may load. Under a directory there with a //go:embed line,
// which may embed any file below it, every file stays, as does any file
// that isn't Go, which a C include may name. Without dirs, every file stays.
func planFiles(files []packFile, dirs []string) ([]packFile, error) {
	if len(dirs) == 0 {
		return files, nil
	}
	list, err := os.ReadFile(dirs[0])
	if err != nil {
		return nil, err
	}
	read := map[string]bool{}
	for _, d := range strings.Split(string(list), "\n") {
		if d != "" {
			read[d] = true
		}
	}
	embeds := map[string]bool{}
	for _, f := range files {
		if dir := filepath.Dir(f.rel); read[dir] && strings.HasSuffix(f.rel, ".go") &&
			bytes.Contains(f.data, []byte("//go:embed")) {
			embeds[dir] = true
		}
	}
	kept := files[:0]
	for _, f := range files {
		if keepFile(f.rel, read, embeds) {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// keepFile reports whether planFiles keeps the file at rel: one that isn't
// Go, a Go file in a directory the plan reads, or one below a directory that
// embeds.
func keepFile(rel string, read, embeds map[string]bool) bool {
	if !strings.HasSuffix(rel, ".go") {
		return true
	}
	dir := filepath.Dir(rel)
	if read[dir] {
		return true
	}
	for d := dir; d != "." && d != "/"; {
		d = filepath.Dir(d)
		if embeds[d] {
			return true
		}
	}
	return false
}

// packFile is a file of a pack: its path in the pack and under where it's
// unpacked.
type packFile struct {
	rel, path string
	mode      fs.FileMode
	data      []byte
}

// readPack reads the pack at src, its files at their paths under dst, and
// lists, one per line, its Go files in a directory named like a Go file.
func readPack(src, dst string) ([]packFile, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, "", err
	}
	defer in.Close()
	var files []packFile
	var listed strings.Builder
	r := bufio.NewReader(in)
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		return strings.TrimSuffix(s, "\n"), err
	}
	if magic, err := line(); err != nil || magic != "gohdr 1" {
		return nil, "", fmt.Errorf("not a gohdr pack")
	}
	for {
		rel, err := line()
		if err == io.EOF && rel == "" {
			return files, listed.String(), nil
		}
		if err != nil {
			return nil, "", err
		}
		modeStr, err := line()
		if err != nil {
			return nil, "", err
		}
		sizeStr, err := line()
		if err != nil {
			return nil, "", err
		}
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return nil, "", err
		}
		size, err := strconv.Atoi(sizeStr)
		if err != nil {
			return nil, "", err
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, "", err
		}
		if dir := filepath.Dir(rel); strings.HasSuffix(rel, ".go") &&
			(strings.HasSuffix(dir, ".go") || strings.Contains(dir, ".go/")) {
			listed.WriteString(rel + "\n")
		}
		files = append(files, packFile{rel, filepath.Join(dst, rel), fs.FileMode(mode), data[:size]})
	}
}

// writeFiles writes files, several at once: a plan unpacks thousands of
// small files, each a wait on the file system more than work for the CPU,
// which one at a time took most of the plan's time.
func writeFiles(files []packFile) error {
	var dirs sync.Map
	jobs := make(chan packFile)
	var once sync.Once
	var first error
	var wg sync.WaitGroup
	for range min(32, 2*runtime.NumCPU()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if err := writeFile(f, &dirs); err != nil {
					once.Do(func() { first = err })
				}
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	return first
}

// writeFile writes f, making its directory, once, if no other write did.
func writeFile(f packFile, dirs *sync.Map) error {
	dir := filepath.Dir(f.path)
	if _, ok := dirs.Load(dir); !ok {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		dirs.Store(dir, true)
	}
	if f.mode&fs.ModeSymlink != 0 {
		return os.Symlink(string(f.data), f.path)
	}
	return os.WriteFile(f.path, f.data, f.mode)
}

// sizes prints a line "<size> <path>" for each Go file under dir, the path
// relative to dir: the real size of the source the plan sees only the
// headers of.
func sizes(dir string) error {
	w := bufio.NewWriter(os.Stdout)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%d %s\n", info.Size(), filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	return w.Flush()
}

// weigh prints a line "<step> <weight>" for each step (bNNN) of a go build
// -n plan, as weighPlan weighs them.
func weigh(planPath string) error {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	start, err := os.Getwd()
	if err != nil {
		return err
	}
	steps, weights := weighPlan(string(data), start)
	w := bufio.NewWriter(os.Stdout)
	for _, step := range steps {
		fmt.Fprintf(w, "%s %d\n", step, weights[step])
	}
	return w.Flush()
}

// valueFlags are the tool flags in a plan whose value is the next word: an
// import path, such as -p's, or a path, which may end in ".go" without being
// a source file of the step.
var valueFlags = map[string]bool{
	"-p": true, "-o": true, "-D": true, "-I": true, "-trimpath": true,
	"-importcfg": true, "-embedcfg": true, "-buildid": true, "-lang": true,
	"-goversion": true, "-asmhdr": true, "-symabis": true, "-pgoprofile": true,
	"-coveragecfg": true, "-objdir": true, "-importpath": true, "-srcdir": true,
}

// weighPlan weighs each step (bNNN) of a go build -n plan: the bytes of Go
// source files its lines name, plus a fixed cost for starting a compile, a
// stand-in for its compile time. The weight is the step's own: neither a
// flag's value nor a comment names a source file, even when it ends in ".go". A relative file is
// in the directory the plan last changed to, from start, as the go command
// prints "cd" only when the directory changes.
func weighPlan(plan, start string) ([]string, map[string]int64) {
	dir := start
	weights := map[string]int64{}
	var steps []string
	step := ""
	for _, line := range strings.Split(plan, "\n") {
		if rest, ok := strings.CutPrefix(line, "mkdir -p $WORK/b"); ok {
			if n, _, ok := strings.Cut(rest, "/"); ok && n != "" && strings.Trim(n, "0123456789") == "" {
				step = "b" + n
				if _, ok := weights[step]; !ok {
					weights[step] = 8 << 10
					steps = append(steps, step)
				}
			}
		}
		if d, ok := strings.CutPrefix(line, "cd "); ok {
			dir = d
			if d == "." {
				dir = start
			}
			continue
		}
		// A comment, such as the package header "# example.com/dep.go",
		// names no file.
		if step == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if i > 0 && valueFlags[fields[i-1]] {
				continue
			}
			f = strings.Trim(f, "\"'")
			if !strings.HasSuffix(f, ".go") {
				continue
			}
			if !filepath.IsAbs(f) {
				f = filepath.Join(dir, f)
			}
			if fi, err := os.Stat(f); err == nil {
				weights[step] += fi.Size()
			}
		}
	}
	return steps, weights
}

// probedFlags are the C compiler flags the go command adds only when a probe
// of the compiler says they work.
var probedFlags = map[string]bool{
	"-fno-caret-diagnostics":   true,
	"-Qunused-arguments":       true,
	"-Wl,--no-gc-sections":     true,
	"-fdebug-prefix-map=a=b":   true,
	"-ffile-prefix-map=a=b":    true,
	"-gno-record-gcc-switches": true,
	"-frandom-seed=1":          true,
}

// cc runs the C compiler probes a plan prints, which go build -n answers
// "unsupported" without running, and prints "probe <compiler> <flag>
// yes|no" for each. For each directory a cgo step runs in it prints
// "modroot <dir> <root>", the directory of the go.mod above it, as go finds
// a module's root (-modfile names another file but not another root):
// -trimpath maps a package's C sources by it. And it prints "goos <GOOS>",
// the target, whose C source mappings differ on Windows.
func cc(planPath string) error {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	start, err := os.Getwd()
	if err != nil {
		return err
	}
	type probe struct {
		compiler, flag, cmd string
		ok                  bool
	}
	var probes []*probe
	seen := map[string]bool{}
	add := func(compiler, flag, cmd string) {
		if !seen[compiler+" "+flag] {
			seen[compiler+" "+flag] = true
			probes = append(probes, &probe{compiler: compiler, flag: flag, cmd: cmd})
		}
	}
	var dirs []string
	seenDir := map[string]bool{}
	dir := start
	for _, line := range strings.Split(string(data), "\n") {
		if d, ok := strings.CutPrefix(line, "cd "); ok {
			dir = d
			if d == "." {
				dir = start
			}
			continue
		}
		if strings.Contains(line, "/cgo ") && strings.Contains(line, " -objdir ") && !seenDir[dir] {
			seenDir[dir] = true
			dirs = append(dirs, dir)
		}
		cmd, ok := strings.CutSuffix(line, " || true")
		if !ok || !strings.Contains(cmd, " -o /dev/null") {
			continue
		}
		f := strings.Fields(cmd)
		compiler := strings.Trim(f[0], "'\"")
		for _, w := range f[1:] {
			if probedFlags[w] {
				add(compiler, w, cmd)
				// Probed only once this one works, so -n never prints it.
				if w == "-fdebug-prefix-map=a=b" {
					add(compiler, "-ffile-prefix-map=a=b", strings.Replace(cmd, w, "-ffile-prefix-map=a=b", 1))
				}
				break
			}
		}
	}
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tmp, err := os.MkdirTemp("", "probe")
			if err != nil {
				return
			}
			defer os.RemoveAll(tmp)
			c := exec.Command("sh", "-c", p.cmd)
			c.Dir = tmp
			c.Env = append(os.Environ(), "LC_ALL=C")
			out, _ := c.CombinedOutput()
			// The go command's own test of the output.
			p.ok = true
			for _, no := range []string{"unrecognized", "unknown", "unrecognised", "is not supported", "not recognized", "unsupported"} {
				if bytes.Contains(out, []byte(no)) {
					p.ok = false
				}
			}
		}()
	}
	wg.Wait()
	w := bufio.NewWriter(os.Stdout)
	for _, p := range probes {
		answer := "no"
		if p.ok {
			answer = "yes"
		}
		fmt.Fprintf(w, "probe\t%s\t%s\t%s\n", p.compiler, p.flag, answer)
	}
	for _, d := range dirs {
		for r := d; filepath.IsAbs(r); r = filepath.Dir(r) {
			if fi, err := os.Stat(filepath.Join(r, "go.mod")); err == nil && !fi.IsDir() {
				fmt.Fprintf(w, "modroot\t%s\t%s\n", d, r)
				break
			}
			if r == "/" {
				break
			}
		}
	}
	if len(dirs) > 0 {
		out, err := exec.Command("go", "env", "GOOS").Output()
		if err != nil {
			return fmt.Errorf("go env GOOS: %w", err)
		}
		fmt.Fprintf(w, "goos\t%s\n", strings.TrimSpace(string(out)))
	}
	return w.Flush()
}

// linkID prints the Go build ID for a link: a hash of the link's command
// (salt), its import config and every archive the config names, so that,
// like the go command's, it changes with whatever the binary is made from.
// The linker makes the binary's GNU build ID from it.
func linkID(cfg, salt string) error {
	data, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	var files []string
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "packagefile "); ok {
			if _, f, ok := strings.Cut(rest, "="); ok {
				files = append(files, f)
			}
		}
	}
	sums := make([][]byte, len(files))
	errs := make([]error, len(files))
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			in, err := os.Open(f)
			if err != nil {
				errs[i] = err
				return
			}
			defer in.Close()
			h := sha256.New()
			_, errs[i] = io.Copy(h, in)
			sums[i] = h.Sum(nil)
		}()
	}
	wg.Wait()
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s", salt, data)
	for i := range files {
		if errs[i] != nil {
			return errs[i]
		}
		h.Write(sums[i])
	}
	// As the go command writes a hash in a build ID.
	id := base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:15])
	fmt.Println(id + "/" + id + "/" + id + "/" + id)
	return nil
}

// listPackage is a package as "go list -json" prints it.
type listPackage struct {
	ImportPath, Dir, ForTest string
	Module                  *listModule
	GoFiles, CgoFiles       []string
	// What go/packages calls OtherFiles.
	CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles []string
	SwigFiles, SwigCXXFiles, SysoFiles              []string
	EmbedFiles                                      []string
}

// listModule is a package's module as "go list -json" prints it.
type listModule struct {
	Path, Version string
	Replace       *listModule
}

// version is the module version a package's files come from, "path@version",
// or "" for one whose files no version fixes: the main module's, or a local
// replacement's.
func (m *listModule) version() string {
	if m != nil && m.Replace != nil {
		m = m.Replace
	}
	if m == nil || m.Version == "" {
		return ""
	}
	return m.Path + "@" + m.Version
}

// within reports whether path is dir or below it: a package's files from the
// module cache, not a copy, such as a vendor directory's.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return dir != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func readList(listPath string) ([]listPackage, error) {
	f, err := os.Open(listPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pkgs []listPackage
	dec := json.NewDecoder(f)
	for {
		var p listPackage
		if err := dec.Decode(&p); err == io.EOF {
			return pkgs, nil
		} else if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
}

// archived reports whether a package "go list -deps -test" lists gets an
// archive: a package with Go files to compile, test variants and external
// test packages included, but not a test main, whose _testmain.go no plan
// holds. unsafe has none: the compiler knows it.
func archived(p listPackage) bool {
	return !strings.HasSuffix(p.ImportPath, ".test") && p.ImportPath != "unsafe" && len(p.GoFiles)+len(p.CgoFiles) > 0
}

// cgoRoot is where the archives' exec mounts each cgo package's work
// directory; a generated file's path under it, less the root, is its path
// in the archives directory too.
const cgoRoot = "/gobuild/"

// manifest prints the manifest golangci-lint's export driver reads for a
// directory of archives: the settings they were compiled with, from "go env
// -json" (env) and the build's tags, and for each package a line of entries
// names, "<id>\t<archive>[\t<cgo output>...]", the archive's path in the
// directory and what it was compiled from, from the "go list" listing. A cgo
// package's entry names the Go files cgo wrote for its compile, in the
// compile's order, under cgoRoot; the manifest gives their paths in the
// directory, where the archives' exec copies them, the package's cgo files,
// which the driver leaves out of what it compiles, and the module version
// its files come from, which decides whether the driver can use that
// output.
func manifest(listPath, envPath, entriesPath, tags string) error {
	pkgs, err := readList(listPath)
	if err != nil {
		return err
	}
	byPath := map[string]listPackage{}
	for _, p := range pkgs {
		if archived(p) {
			byPath[p.ImportPath] = p
		}
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		return err
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("%s: %w", envPath, err)
	}
	data, err = os.ReadFile(entriesPath)
	if err != nil {
		return err
	}
	var lines [][]string
	for _, l := range strings.Split(string(data), "\n") {
		if f := strings.Split(l, "\t"); len(f) >= 2 {
			lines = append(lines, f)
		}
	}
	type entry struct{ archive, sources string }
	entries := make([]entry, len(lines))
	errs := make([]error, len(lines))
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for i, f := range lines {
		p, ok := byPath[f[0]]
		if !ok {
			return fmt.Errorf("%s: not a package go list lists for the archives", f[0])
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			entries[i].archive = f[1]
			entries[i].sources, errs[i] = sourcesDigest(p, f[2:])
		}()
	}
	wg.Wait()
	packages := map[string]any{}
	for i, f := range lines {
		if errs[i] != nil {
			return fmt.Errorf("%s: %w", f[0], errs[i])
		}
		e := map[string]any{"archive": entries[i].archive, "sources": entries[i].sources}
		if p := byPath[f[0]]; len(p.CgoFiles) > 0 {
			var output []string
			for _, g := range f[2:] {
				output = append(output, strings.TrimPrefix(g, cgoRoot))
			}
			e["cgoFiles"], e["cgoOutput"] = p.CgoFiles, output
			if v := p.Module.version(); v != "" && within(env["GOMODCACHE"], p.Dir) {
				e["module"] = v
			}
		}
		packages[f[0]] = e
	}
	buildTags := strings.FieldsFunc(tags, func(r rune) bool { return r == ',' })
	sort.Strings(buildTags)
	if buildTags == nil {
		buildTags = []string{}
	}
	m := map[string]any{
		"version":    2,
		"goVersion":  env["GOVERSION"],
		"goos":       env["GOOS"],
		"goarch":     env["GOARCH"],
		"cgoEnabled": env["CGO_ENABLED"] == "1",
		"buildTags":  buildTags,
		"packages":   packages,
	}
	// What cgo's output depends on besides the sources: the go command's C
	// settings, every one but the go command's own in env, and the first
	// line the C compiler prints for --version, so that the driver uses
	// the archives' cgo output only with the same.
	if env["CGO_ENABLED"] == "1" {
		cgo := map[string]string{}
		for k, v := range env {
			if k != "GOVERSION" && k != "GOOS" && k != "GOARCH" && k != "CGO_ENABLED" && k != "GOMODCACHE" {
				cgo[k] = v
			}
		}
		if cc := strings.Fields(env["CC"]); len(cc) > 0 {
			out, _ := exec.Command(cc[0], append(cc[1:], "--version")...).Output()
			first, _, _ := strings.Cut(string(out), "\n")
			cgo["ccVersion"] = strings.TrimSpace(first)
		}
		m["cgo"] = cgo
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(out, '\n'))
	return err
}

// sourcesDigest is the export driver's digest of what a package's archive is
// compiled from: sha256 over the sorted lines "<name> <sha256 of content>\n"
// of its Go files, its other source files and its embedded files, each named
// by its path relative to the package directory. For a cgo package, whose
// compile reads cgo's output (generated) rather than its cgo files, a line
// "cgo\n", its plain Go files, its cgo files, and cgo's output, each named
// "cgo-output-<i>" by its place in the compile and hashed without its
// //line directives, which name the directory cgo ran in.
func sourcesDigest(p listPackage, generated []string) (string, error) {
	var lines []string
	add := func(name, file string, gen bool) error {
		h, err := fileSHA256(file, gen)
		if err != nil {
			return err
		}
		lines = append(lines, name+" "+h+"\n")
		return nil
	}
	if len(p.CgoFiles) > 0 {
		if len(generated) == 0 {
			return "", fmt.Errorf("a cgo package without cgo's output")
		}
		lines = append(lines, "cgo\n")
		for i, g := range generated {
			if err := add(fmt.Sprintf("cgo-output-%d", i), g, true); err != nil {
				return "", err
			}
		}
	}
	for _, group := range [][]string{p.GoFiles, p.CgoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.FFiles, p.SFiles,
		p.SwigFiles, p.SwigCXXFiles, p.SysoFiles, p.EmbedFiles} {
		for _, f := range group {
			if err := add(f, filepath.Join(p.Dir, f), false); err != nil {
				return "", err
			}
		}
	}
	sort.Strings(lines)
	sum := sha256.New()
	for _, l := range lines {
		io.WriteString(sum, l)
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil)), nil
}

// fileSHA256 hashes a file's content, for a generated Go file without the
// comments, as the Go scanner finds them, that start with "//line ".
func fileSHA256(file string, generated bool) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if !generated {
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	fset := token.NewFileSet()
	tf := fset.AddFile(file, -1, len(data))
	var s scanner.Scanner
	s.Init(tf, data, nil, scanner.ScanComments)
	last := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && strings.HasPrefix(lit, "//line ") {
			off := tf.Offset(pos)
			h.Write(data[last:off])
			last = off + len(lit)
		}
	}
	h.Write(data[last:])
	return hex.EncodeToString(h.Sum(nil)), nil
}

// packmod writes to out a pack of what a plan reads of a downloaded module:
// the files at rels under root, the module cache, each directory walked. A
// Go file is its header (goHeader), as the plan reads of the source; a test
// file and a testdata directory, which no build reads, are left out; any
// other file is whole, as a //go:embed or a C include may name it.
func packmod(root, out string, rels []string) error {
	var files []string
	for _, rel := range rels {
		err := filepath.WalkDir(filepath.Join(root, rel), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			r, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(r))
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Strings(files)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "gohdr 1")
	// Each Go file's imports and build constraint: what discover follows.
	var imports []string
	for _, rel := range files {
		p := filepath.Join(root, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		var data []byte
		mode := fi.Mode() & (fs.ModePerm | fs.ModeSymlink)
		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			data = []byte(target)
		} else {
			if data, err = os.ReadFile(p); err != nil {
				return err
			}
			if strings.HasSuffix(rel, ".go") {
				data = goHeader(rel, data)
				imports = append(imports, importLine(rel, data))
			}
		}
		fmt.Fprintf(w, "%s\n%o\n%d\n", rel, uint32(mode), len(data))
		w.Write(data)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.WriteFile(out+".imports", []byte(strings.Join(imports, "")), 0o644)
}

// importLine is a Go file's line in an imports list: its directory, its
// name, its //go:build expression or "-", and its imports, separated by
// tabs, the imports by spaces.
func importLine(rel string, src []byte) string {
	expr := "-"
	for _, l := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "package ") {
			break
		}
		if strings.HasPrefix(t, "//go:build ") {
			expr = strings.TrimSpace(strings.TrimPrefix(t, "//go:build "))
		}
	}
	return filepath.ToSlash(filepath.Dir(rel)) + "\t" + filepath.Base(rel) + "\t" + expr + "\t" +
		strings.Join(fileImports(rel, src), " ") + "\n"
}

// importsOfPack prints what a pack's Go files import, for following the
// imports of a build from its main package: a line per Go file (importLine),
// but tests, or with tests, its tests too; and a line "mod <dir> <module
// path>" per go.mod, from its module directive. Testdata directories are
// left out.
func importsOfPack(pack string, tests bool) error {
	in, err := os.Open(pack)
	if err != nil {
		return err
	}
	defer in.Close()
	r := bufio.NewReader(in)
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		return strings.TrimSuffix(s, "\n"), err
	}
	if magic, err := line(); err != nil || magic != "gohdr 1" {
		return fmt.Errorf("not a gohdr pack")
	}
	var files []string
	var mods []string
	for {
		rel, err := line()
		if err == io.EOF && rel == "" {
			break
		}
		if err != nil {
			return err
		}
		if _, err := line(); err != nil {
			return err
		}
		sizeStr, err := line()
		if err != nil {
			return err
		}
		size, err := strconv.Atoi(sizeStr)
		if err != nil {
			return err
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}
		if strings.Contains("/"+rel, "/testdata/") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if filepath.Base(rel) == "go.mod" {
			for _, l := range strings.Split(string(data[:size]), "\n") {
				if f := strings.Fields(l); len(f) >= 2 && f[0] == "module" {
					mods = append(mods, "mod "+dir+" "+strings.Trim(f[1], "\""))
					break
				}
			}
			continue
		}
		if !strings.HasSuffix(rel, ".go") || (!tests && strings.HasSuffix(rel, "_test.go")) {
			continue
		}
		files = append(files, importLine(rel, data[:size]))
	}
	sort.Strings(mods)
	var b strings.Builder
	for _, m := range mods {
		b.WriteString(m + "\n")
	}
	for _, f := range files {
		b.WriteString(f)
	}
	_, err = os.Stdout.WriteString(b.String())
	return err
}

// fileImports is a Go file's import paths but "C"; nothing for a file that
// does not parse.
func fileImports(name string, src []byte) []string {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly|parser.SkipObjectResolution)
	if err != nil || f == nil {
		return nil
	}
	var out []string
	for _, im := range f.Imports {
		if ip, err := strconv.Unquote(im.Path.Value); err == nil && ip != "C" {
			out = append(out, ip)
		}
	}
	return out
}

// headerEnd follows a header's go:embed lines, as the go module's scan writes
// them: go/build up to Go 1.25 looks for go:embed lines only when its scan of
// the imports stops at a token before the end of the file.
const headerEnd = "var _ struct{}\n"

// goHeader is a Go file up to the end of its imports, plus its go:embed
// lines, then headerEnd when there are any; a file that does not parse is
// kept whole. It is the go module's scan's header (gomod's go-includes).
func goHeader(name string, src []byte) []byte {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || f == nil {
		return src
	}
	end := f.Name.End()
	for _, d := range f.Decls {
		if gen, ok := d.(*ast.GenDecl); !ok || gen.Tok != token.IMPORT {
			break
		}
		if d.End() > end {
			end = d.End()
		}
	}
	off := min(fset.Position(end).Offset, len(src))
	var out bytes.Buffer
	out.Write(src[:off])
	out.WriteByte('\n')
	embeds := false
	for _, line := range strings.Split(string(src[off:]), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "//go:embed") {
			out.WriteString(t)
			out.WriteByte('\n')
			embeds = true
		}
	}
	if embeds {
		out.WriteString(headerEnd)
	}
	return out.Bytes()
}

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 3 && args[0] == "imports":
		err = importsOfPack(args[1], args[2] == "tests")
	case len(args) >= 3 && args[0] == "packmod":
		err = packmod(args[1], args[2], args[3:])
	case (len(args) == 3 || len(args) == 4) && args[0] == "unpackall":
		err = unpackAll(args[1], args[2], args[3:]...)
	case (len(args) == 4 || len(args) == 5) && args[0] == "unpack":
		err = unpack(args[1], args[2], args[3], args[4:]...)
	case len(args) == 2 && args[0] == "weigh":
		err = weigh(args[1])
	case len(args) == 2 && args[0] == "sizes":
		err = sizes(args[1])
	case len(args) == 2 && args[0] == "cc":
		err = cc(args[1])
	case len(args) == 3 && args[0] == "linkid":
		err = linkID(args[1], args[2])
	case len(args) == 5 && args[0] == "manifest":
		err = manifest(args[1], args[2], args[3], args[4])
	default:
		err = fmt.Errorf("usage: gohdr unpack <pack> <dir> <list>, gohdr weigh <plan>, gohdr sizes <dir>, gohdr cc <plan>, gohdr linkid <importcfg> <salt>, or gohdr manifest <list> <env> <entries> <tags>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gohdr:", err)
		os.Exit(1)
	}
}
`
