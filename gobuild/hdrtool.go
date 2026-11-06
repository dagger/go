package main

import (
	"context"
	"sync"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// hdrDir is where an exec that runs gohdr mounts it (hdrTools), and hdrBin
// is the command that runs it.
const (
	hdrDir = "/gobuild/gohdr.d"
	hdrBin = hdrDir + "/gohdr"
)

// hdrTools builds gohdr, the helper's in-container tool. "gohdr unpack" writes
// out a header pack, the go module's scan of each Go file up to the end of its
// imports, for the plan, and lists the pack's Go files in directories named
// like Go files. "gohdr weigh" weighs the plan's steps by the Go
// source they compile. "gohdr cc" answers the plan's C compiler probes.
// "gohdr linkid" makes a link's build ID.
//
// It is gohdr for every machine an engine runs on (machineTools), declared
// for a build's target. A standalone tool, it is built in image, gomod's
// pinned toolchain, statically, and runs in whatever image the module builds
// with. It is one object for each image and target, so the SDK asks the
// engine for its ID once, not once for every exec that mounts it.
func hdrTools(ctx context.Context, image, platform string) (*dagger.Directory, error) {
	return toolsFor(ctx, "gohdr", image, platform, func(goarch string) *dagger.File {
		return core.NewContainer().From(image).
			WithEnvVariable("CGO_ENABLED", "0").
			WithEnvVariable("GOOS", "linux").
			WithEnvVariable("GOARCH", goarch).
			WithNewFile("/gohdr-src/go.mod", "module gohdr\n\ngo 1.26\n").
			WithNewFile("/gohdr-src/main.go", hdrToolSrc).
			WithWorkdir("/gohdr-src").
			WithExec([]string{"go", "build", "-o", "/gohdr", "."}, bare).File("/gohdr")
	})
}

// toolsFor is machineTools' directory of the tool name, built by build in
// image and declared for platform, made once for each name, image and
// platform.
func toolsFor(ctx context.Context, name, image, platform string, build func(goarch string) *dagger.File) (*dagger.Directory, error) {
	key := name + "\x00" + image + "\x00" + platform
	toolsMu.Lock()
	defer toolsMu.Unlock()
	if dir, ok := toolsMade[key]; ok {
		return dir, nil
	}
	dir, err := machineTools(ctx, name, platform, build)
	if err != nil {
		return nil, err
	}
	toolsMade[key] = dir
	return dir, nil
}

var (
	toolsMu   sync.Mutex
	toolsMade = map[string]*dagger.Directory{}
)

const hdrToolSrc = `package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
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

// subset writes to out, as a pack, the files unpack writes from the pack at
// src with dirs (planFiles), and to list the whole pack's Go files in a
// directory named like a Go file, as unpack lists them: what a plan reads of
// the pack, in a file of its own, which changes only when one of them does.
func subset(src, out, list string, dirs ...string) error {
	files, listed, err := readPack(src, "")
	if err != nil {
		return err
	}
	if files, err = planFiles(files, dirs); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "gohdr 1")
	for _, file := range files {
		fmt.Fprintf(w, "%s\n%o\n%d\n", file.rel, uint32(file.mode), len(file.data))
		w.Write(file.data)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.WriteFile(list, []byte(listed), 0o644)
}

// planFiles is files without the Go files a plan doesn't read, with dirs
// one file, the list of the directories it reads Go files in: those of the
// packages it may load. Under a directory there with a //go:embed line,
// which may embed any file below it, every file stays, the Go files emptied,
// as does any file that isn't Go, which a C include may name. Without dirs,
// every file stays.
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
	linked, absolute := linkTargets(files)
	kept := files[:0]
	for _, f := range files {
		if !keepFile(f.rel, read, embeds) {
			continue
		}
		// A Go file in no directory the plan reads is kept only because a
		// //go:embed pattern above it may match it, by its name: the plan
		// reads nothing of it, so it is written empty, and a change to it
		// leaves what the plan reads as it was. One a symlink leads to is
		// kept whole: a package the plan reads may hold the link as a
		// source file. A pack with an absolute symlink, which leads where
		// the source is mounted, keeps every file whole.
		if strings.HasSuffix(f.rel, ".go") && !read[filepath.Dir(f.rel)] && f.mode.IsRegular() && !linked[f.rel] && !absolute {
			f.data = nil
		}
		kept = append(kept, f)
	}
	return kept, nil
}

// linkTargets is the files of a pack that its relative symlinks lead to,
// through chains of them, by their paths in the pack, and whether it holds
// an absolute symlink, whose target depends on where the pack is unpacked.
func linkTargets(files []packFile) (map[string]bool, bool) {
	links := map[string]string{}
	absolute := false
	for _, f := range files {
		if f.mode&fs.ModeSymlink == 0 {
			continue
		}
		if filepath.IsAbs(string(f.data)) {
			absolute = true
			continue
		}
		links[f.rel] = filepath.Join(filepath.Dir(f.rel), string(f.data))
	}
	targets := map[string]bool{}
	for _, t := range links {
		for i := 0; i < 40 && !targets[t]; i++ {
			targets[t] = true
			next, ok := links[t]
			if !ok {
				break
			}
			t = next
		}
	}
	return targets, absolute
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

// packmod writes to out a pack of what a plan reads of a downloaded module:
// its files, read from its zip at zipRel under root, the module cache, as
// the go command extracts them into dir, without extracting them; and the
// files at rels under root, its metadata. A Go file is its header
// (goHeader), as the plan reads of the source; a test file and a testdata
// directory, which no build reads, are left out; any other file is what a
// plan reads of it (planContent).
func packmod(root, out, zipRel, dir string, rels []string) error {
	type file struct {
		rel  string
		mode fs.FileMode
		read func() ([]byte, error)
	}
	var files []file
	z, err := zip.OpenReader(filepath.Join(root, zipRel))
	if err != nil {
		return err
	}
	defer z.Close()
	// Every file is under "<module>@<version>/", the module path not
	// escaped, and the zip is checked as the go command checks one before
	// it extracts it.
	prefix := unescapeModule(dir) + "/"
	if err := checkModuleZip(z.File, prefix); err != nil {
		return fmt.Errorf("%s: %w", zipRel, err)
	}
	for _, zf := range z.File {
		name := strings.TrimPrefix(zf.Name, prefix)
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		elems := strings.Split(name, "/")
		if slices.Contains(elems[:len(elems)-1], "testdata") || strings.HasSuffix(elems[len(elems)-1], "_test.go") {
			continue
		}
		// The go command extracts every file read-only.
		files = append(files, file{rel: dir + "/" + name, mode: 0o444, read: func() ([]byte, error) {
			r, err := zf.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}})
	}
	for _, rel := range rels {
		p := filepath.Join(root, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		files = append(files, file{rel: rel, mode: fi.Mode() & fs.ModePerm, read: func() ([]byte, error) { return os.ReadFile(p) }})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	// Each file is read, decompressed from the zip, on its own: a large
	// module's Go files are hundreds of MB.
	datas := make([][]byte, len(files))
	errs := make([]error, len(files))
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for i, file := range files {
		keep := planContent(file.rel)
		if keep == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, err := file.read()
			switch {
			case err != nil:
				errs[i] = err
			case keep == "header":
				datas[i] = goHeader(file.rel, data)
			case keep == "constraints":
				datas[i] = leadingComments(data)
			default:
				datas[i] = data
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "gohdr 1")
	// Each Go file's imports and build constraint: what discover follows.
	var imports []string
	for i, file := range files {
		data := datas[i]
		if planContent(file.rel) == "header" {
			imports = append(imports, importLine(file.rel, data))
		}
		fmt.Fprintf(w, "%s\n%o\n%d\n", file.rel, uint32(file.mode), len(data))
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

// The limits the go command puts on a module zip's contents.
const (
	maxZipFile = 500 << 20
	maxGoMod   = 16 << 20
	maxLicense = 16 << 20
)

// checkModuleZip reports why files aren't a module zip the go command
// would extract, as it checks one before extracting it (golang.org/x/mod's
// zip.CheckZip): a file outside the module's directory, prefix; a path
// that isn't clean, or isn't a module file path (checkFilePath); a go.mod
// anywhere but the module's root, or not named in lower case; two paths
// that differ only in case, or one that is both a file and a directory;
// or contents larger than the go command's limits.
func checkModuleZip(files []*zip.File, prefix string) error {
	type entry struct {
		path string
		dir  bool
	}
	seen := map[string]entry{}
	var collide func(p string, dir bool) error
	collide = func(p string, dir bool) error {
		fold := foldPath(p)
		if other, ok := seen[fold]; ok {
			switch {
			case other.path != p:
				return fmt.Errorf("case-insensitive file name collision: %q and %q", other.path, p)
			case other.dir != dir:
				return fmt.Errorf("entry %q is both a file and a directory", p)
			case !dir:
				return fmt.Errorf("multiple entries for file %q", p)
			}
		} else {
			seen[fold] = entry{p, dir}
		}
		if parent := path.Dir(p); parent != "." {
			return collide(parent, true)
		}
		return nil
	}
	var size uint64
	for _, zf := range files {
		name, ok := strings.CutPrefix(zf.Name, prefix)
		if !ok {
			return fmt.Errorf("%s: path does not have prefix %q", zf.Name, prefix)
		}
		if name == "" {
			continue
		}
		dir := strings.HasSuffix(name, "/")
		if dir {
			name = name[:len(name)-1]
		}
		if path.Clean(name) != name {
			return fmt.Errorf("%s: file path is not clean", zf.Name)
		}
		if err := checkFilePath(name); err != nil {
			return fmt.Errorf("%s: %w", zf.Name, err)
		}
		if base := path.Base(name); !dir && strings.EqualFold(base, "go.mod") {
			if base != name {
				return fmt.Errorf("%s: go.mod file not in module root directory", zf.Name)
			}
			if name != "go.mod" {
				return fmt.Errorf("%s: go.mod file name must be lower case", zf.Name)
			}
		}
		if !dir {
			sz := zf.UncompressedSize64
			if sz > maxZipFile-size {
				return fmt.Errorf("total uncompressed size of module contents too large (max size is %d bytes)", maxZipFile)
			}
			size += sz
			if name == "go.mod" && sz > maxGoMod {
				return fmt.Errorf("go.mod file too large (max size is %d bytes)", maxGoMod)
			}
			if name == "LICENSE" && sz > maxLicense {
				return fmt.Errorf("LICENSE file too large (max size is %d bytes)", maxLicense)
			}
		}
		if err := collide(name, dir); err != nil {
			return err
		}
	}
	return nil
}

// checkFilePath reports why p isn't a file path a module may hold, as
// golang.org/x/mod's module.CheckFilePath does.
func checkFilePath(p string) error {
	switch {
	case !utf8.ValidString(p):
		return fmt.Errorf("malformed file path %q: invalid UTF-8", p)
	case p == "":
		return fmt.Errorf("malformed file path %q: empty string", p)
	case strings.Contains(p, "//"):
		return fmt.Errorf("malformed file path %q: double slash", p)
	case strings.HasSuffix(p, "/"):
		return fmt.Errorf("malformed file path %q: trailing slash", p)
	}
	for _, elem := range strings.Split(p, "/") {
		if err := checkFileElem(elem); err != nil {
			return fmt.Errorf("malformed file path %q: %w", p, err)
		}
	}
	return nil
}

// badWindowsNames are the path element names Windows reserves, which no
// module file path may have, whatever its extension.
var badWindowsNames = []string{
	"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
}

// checkFileElem reports why elem isn't an element of a module file path:
// empty, all dots, ending in a dot, a character no module file name may
// hold, or a name Windows reserves.
func checkFileElem(elem string) error {
	switch {
	case elem == "":
		return errors.New("empty path element")
	case strings.Count(elem, ".") == len(elem):
		return fmt.Errorf("invalid path element %q", elem)
	case strings.HasSuffix(elem, "."):
		return errors.New("trailing dot in path element")
	}
	for _, r := range elem {
		if !fileNameOK(r) {
			return fmt.Errorf("invalid char %q", r)
		}
	}
	short, _, _ := strings.Cut(elem, ".")
	for _, bad := range badWindowsNames {
		if strings.EqualFold(bad, short) {
			return fmt.Errorf("%q disallowed as path element component on Windows", short)
		}
	}
	return nil
}

// fileNameOK reports whether r may be in a module file name: an ASCII
// letter or digit, the ASCII punctuation no shell or file system treats
// specially, a space, or a non-ASCII letter.
func fileNameOK(r rune) bool {
	if r < utf8.RuneSelf {
		const allowed = "!#$%&()+,-.=@[]^_{}~ "
		return '0' <= r && r <= '9' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || strings.ContainsRune(allowed, r)
	}
	return unicode.IsLetter(r)
}

// foldPath is p case-folded, so two paths that differ only in case fold
// to the same string.
func foldPath(p string) string {
	var b strings.Builder
	for _, r := range p {
		// unicode.SimpleFold cycles through a rune's case equivalents;
		// the smallest one stands for all of them.
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				r = next
				break
			}
			r = next
		}
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unescapeModule is a module path or version as the module cache's
// escaping ("!" and a lower-case letter for an upper-case one) names it.
func unescapeModule(s string) string {
	var b strings.Builder
	upper := false
	for _, r := range s {
		if r == '!' {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// planContent is what a plan reads of a downloaded module's file at rel:
// "header" for a Go file; "constraints", the leading comments where go/build
// finds build constraints, for another file it builds, C or assembly; "whole"
// for the module's metadata and the files the go command reads as such; and
// nothing of any other file, whose existence alone a //go:embed pattern
// matches. A module's other files, such as an API's JSON descriptions, can
// be most of its size.
func planContent(rel string) string {
	switch base := filepath.Base(rel); {
	case strings.HasPrefix(rel, "cache/"), base == "go.mod", base == "go.sum", base == "go.work", base == "go.work.sum", base == "modules.txt":
		return "whole"
	case strings.HasSuffix(base, ".go"):
		return "header"
	}
	switch filepath.Ext(rel) {
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx", ".m", ".s", ".S", ".sx", ".f", ".F", ".for", ".f90", ".swig", ".swigcxx":
		return "constraints"
	}
	return ""
}

// leadingComments is a file's comments before its first text that isn't a
// comment, line or block, which is as far as go/build reads a file that
// isn't Go for its build constraints. A block comment closed on the line
// of that text stays closed.
func leadingComments(src []byte) []byte {
	end, inBlock := 0, false
	for end < len(src) {
		next := bytes.IndexByte(src[end:], '\n')
		if next < 0 {
			next = len(src) - end
		} else {
			next++
		}
		line := src[end : end+next]
		for i := 0; ; {
			for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
				i++
			}
			rest := line[i:]
			switch {
			case len(rest) == 0:
			case inBlock:
				j := bytes.Index(rest, []byte("*/"))
				if j < 0 {
					break
				}
				inBlock, i = false, i+j+2
				continue
			case bytes.HasPrefix(rest, []byte("//")):
			case bytes.HasPrefix(rest, []byte("/*")):
				inBlock, i = true, i+2
				continue
			default:
				return src[:end+i]
			}
			break
		}
		end += next
	}
	return src[:end]
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
// path>" per go.mod, from its module directive, and a line "repl <dir>
// <module path>" per directory replacing a module in a go.mod or go.work.
// Testdata directories are
// listed too: a build may name a package there, though no pattern matches
// one.
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
		dir := filepath.ToSlash(filepath.Dir(rel))
		if base := filepath.Base(rel); base == "go.mod" || base == "go.work" {
			for _, l := range strings.Split(string(data[:size]), "\n") {
				if f := strings.Fields(l); len(f) >= 2 && f[0] == "module" {
					mods = append(mods, "mod "+dir+" "+strings.Trim(f[1], "\""))
					break
				}
			}
			for _, r := range replaceDirs(string(data[:size])) {
				mods = append(mods, "repl "+filepath.ToSlash(filepath.Clean(filepath.Join(dir, r[1])))+" "+r[0])
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

// replaceDirs is a go.mod's replacements of a module by a directory: each
// replaced module path and the directory, relative to the go.mod's.
func replaceDirs(gomod string) [][2]string {
	var out [][2]string
	block := false
	for _, l := range strings.Split(gomod, "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		f := strings.Fields(l)
		switch {
		case len(f) == 0:
			continue
		case block && f[0] == ")":
			block = false
			continue
		case block:
		case f[0] == "replace" && len(f) == 2 && f[1] == "(":
			block = true
			continue
		case f[0] == "replace":
			f = f[1:]
		default:
			continue
		}
		// old [version] => new [version]: a directory has no version.
		arrow := slices.Index(f, "=>")
		if arrow < 1 || arrow != len(f)-2 {
			continue
		}
		if repl := strings.Trim(f[arrow+1], "\""); repl == "." || repl == ".." ||
			strings.HasPrefix(repl, "./") || strings.HasPrefix(repl, "../") {
			out = append(out, [2]string{strings.Trim(f[0], "\""), repl})
		}
	}
	return out
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
	case len(args) >= 5 && args[0] == "packmod":
		err = packmod(args[1], args[2], args[3], args[4], args[5:])
	case (len(args) == 3 || len(args) == 4) && args[0] == "unpackall":
		err = unpackAll(args[1], args[2], args[3:]...)
	case (len(args) == 4 || len(args) == 5) && args[0] == "unpack":
		err = unpack(args[1], args[2], args[3], args[4:]...)
	case (len(args) == 4 || len(args) == 5) && args[0] == "subset":
		err = subset(args[1], args[2], args[3], args[4:]...)
	case len(args) == 2 && args[0] == "weigh":
		err = weigh(args[1])
	case len(args) == 2 && args[0] == "sizes":
		err = sizes(args[1])
	case len(args) == 2 && args[0] == "cc":
		err = cc(args[1])
	case len(args) == 3 && args[0] == "linkid":
		err = linkID(args[1], args[2])
	default:
		err = fmt.Errorf("usage: gohdr unpack <pack> <dir> <list>, gohdr subset <pack> <out> <list>, gohdr weigh <plan>, gohdr sizes <dir>, gohdr cc <plan>, or gohdr linkid <importcfg> <salt>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gohdr:", err)
		os.Exit(1)
	}
}
`
