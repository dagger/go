package main

import "dagger/gobuild/internal/dagger"

// hdrTool builds gohdr, the helper's in-container tool. "gohdr unpack" writes
// out a header pack, the go module's scan of each Go file up to the end of its
// imports, for the plan, and lists the pack's Go files in directories named
// like Go files. "gohdr weigh" weighs the plan's steps by the Go
// source they compile. "gohdr cc" answers the plan's C compiler probes.
// "gohdr linkid" makes a link's build ID. "gohdr modunzip" extracts a
// downloaded module, checking its checksum. "gohdr scope" lists the
// packages a lint target's archives are compiled for, and "gohdr manifest"
// describes those archives for golangci-lint's export driver.
func hdrTool() *dagger.File {
	// A standalone tool, so it is built with a pinned toolchain, statically,
	// and runs in whatever image the module builds with.
	return dag.Container().From(toolImage).
		WithEnvVariable("CGO_ENABLED", "0").
		WithNewFile("/gohdr-src/go.mod", "module gohdr\n\ngo 1.26\n").
		WithNewFile("/gohdr-src/main.go", hdrToolSrc).
		WithWorkdir("/gohdr-src").
		WithExec([]string{"go", "build", "-o", "/gohdr", "."}, bare).File("/gohdr")
}

const hdrToolSrc = `package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// depth.
func unpack(src, dst, list string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	var listed strings.Builder
	defer func() {
		if werr := os.WriteFile(list, []byte(listed.String()), 0o644); werr != nil && err == nil {
			err = werr
		}
	}()
	r := bufio.NewReader(in)
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		return strings.TrimSuffix(s, "\n"), err
	}
	if magic, err := line(); err != nil || magic != "gohdr 1" {
		return fmt.Errorf("not a gohdr pack")
	}
	for {
		rel, err := line()
		if err == io.EOF && rel == "" {
			return nil
		}
		if err != nil {
			return err
		}
		modeStr, err := line()
		if err != nil {
			return err
		}
		sizeStr, err := line()
		if err != nil {
			return err
		}
		mode, err := strconv.ParseUint(modeStr, 8, 32)
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
		if dir := filepath.Dir(rel); strings.HasSuffix(rel, ".go") &&
			(strings.HasSuffix(dir, ".go") || strings.Contains(dir, ".go/")) {
			listed.WriteString(rel + "\n")
		}
		p := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if fs.FileMode(mode)&fs.ModeSymlink != 0 {
			if err := os.Symlink(string(data[:size]), p); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(p, data[:size], fs.FileMode(mode)); err != nil {
			return err
		}
	}
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

// modUnzip extracts a module zip, as the go command's download writes it,
// into dir, the module's directory in the module cache, as the go command
// extracts it: every file under the zip's "<module>@<version>/" prefix,
// read-only. It first checks the zip's checksum, the go command's "h1:"
// hash of the files it holds, against sum.
func modUnzip(zipPath, prefix, dir, sum string) error {
	z, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer z.Close()
	// The checksum covers every entry, directories too, as the go command's
	// does (golang.org/x/mod/sumdb/dirhash.HashZip); only files are written.
	var names []string
	files := map[string]*zip.File{}
	for _, f := range z.File {
		rel, ok := strings.CutPrefix(f.Name, prefix+"/")
		if !ok || (rel != "" && !filepath.IsLocal(strings.TrimSuffix(rel, "/"))) {
			return fmt.Errorf("%s: unexpected file %q", zipPath, f.Name)
		}
		names = append(names, f.Name)
		files[f.Name] = f
	}
	sort.Strings(names)
	summary := sha256.New()
	for _, name := range names {
		r, err := files[name].Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, r)
		r.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(summary, "%x  %s\n", h.Sum(nil), name)
	}
	if got := "h1:" + base64.StdEncoding.EncodeToString(summary.Sum(nil)); got != sum {
		return fmt.Errorf("%s: checksum %s, not %s as the build verified", prefix, got, sum)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			continue
		}
		dst := filepath.Join(dir, strings.TrimPrefix(name, prefix+"/"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		r, err := files[name].Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
		if err != nil {
			r.Close()
			return err
		}
		_, err = io.Copy(w, r)
		r.Close()
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	// Read-only, as the go command leaves them, deepest first.
	var dirs []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return err
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

// listPackage is a package as "go list -json" prints it.
type listPackage struct {
	ImportPath, Dir, ForTest string
	GoFiles, CgoFiles       []string
	// What go/packages calls OtherFiles.
	CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles []string
	SwigFiles, SwigCXXFiles, SysoFiles              []string
	EmbedFiles                                      []string
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
// archive: a plain package with Go files to compile, not a test variant, an
// external test package or a test main. unsafe has none: the compiler knows
// it.
func archived(p listPackage) bool {
	return p.ForTest == "" && !strings.Contains(p.ImportPath, " [") && !strings.HasSuffix(p.ImportPath, ".test") &&
		p.ImportPath != "unsafe" && len(p.GoFiles)+len(p.CgoFiles) > 0
}

// scope prints the import path of each package of a "go list -deps -test
// -json" listing that gets an archive, one per line.
func scope(listPath string) error {
	pkgs, err := readList(listPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(os.Stdout)
	for _, p := range pkgs {
		if archived(p) {
			fmt.Fprintln(w, p.ImportPath)
		}
	}
	return w.Flush()
}

// manifest prints the manifest golangci-lint's export driver reads for a
// directory of archives: the settings they were compiled with, from "go env
// -json" (env) and the build's tags, and for each package a line of entries
// names, "<import path> <archive> [<cgo output>...]", the archive's path in
// the directory and what it was compiled from, from the "go list" listing.
// A cgo package's entry names the Go files cgo wrote for its compile, in
// the compile's order.
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
	var env struct{ GOVERSION, GOOS, GOARCH, CGO_ENABLED string }
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("%s: %w", envPath, err)
	}
	data, err = os.ReadFile(entriesPath)
	if err != nil {
		return err
	}
	var lines [][]string
	for _, l := range strings.Split(string(data), "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
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
		packages[f[0]] = map[string]string{"archive": entries[i].archive, "sources": entries[i].sources}
	}
	buildTags := strings.FieldsFunc(tags, func(r rune) bool { return r == ',' })
	sort.Strings(buildTags)
	if buildTags == nil {
		buildTags = []string{}
	}
	out, err := json.MarshalIndent(map[string]any{
		"version":    1,
		"goVersion":  env.GOVERSION,
		"goos":       env.GOOS,
		"goarch":     env.GOARCH,
		"cgoEnabled": env.CGO_ENABLED == "1",
		"buildTags":  buildTags,
		"packages":   packages,
	}, "", "  ")
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
// "cgo\n", its plain Go files, and cgo's output, each named
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
	for _, group := range [][]string{p.GoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.FFiles, p.SFiles,
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

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 4 && args[0] == "unpack":
		err = unpack(args[1], args[2], args[3])
	case len(args) == 2 && args[0] == "weigh":
		err = weigh(args[1])
	case len(args) == 2 && args[0] == "cc":
		err = cc(args[1])
	case len(args) == 3 && args[0] == "linkid":
		err = linkID(args[1], args[2])
	case len(args) == 5 && args[0] == "modunzip":
		err = modUnzip(args[1], args[2], args[3], args[4])
	case len(args) == 2 && args[0] == "scope":
		err = scope(args[1])
	case len(args) == 5 && args[0] == "manifest":
		err = manifest(args[1], args[2], args[3], args[4])
	default:
		err = fmt.Errorf("usage: gohdr unpack <pack> <dir> <list>, gohdr weigh <plan>, gohdr cc <plan>, gohdr linkid <importcfg> <salt>, gohdr modunzip <zip> <module>@<version> <dir> <checksum>, gohdr scope <list>, or gohdr manifest <list> <env> <entries> <tags>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gohdr:", err)
		os.Exit(1)
	}
}
`
