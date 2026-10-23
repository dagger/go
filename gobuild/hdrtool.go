package main

import "dagger/gobuild/internal/dagger"

// hdrTool builds gohdr, the helper's in-container tool. "gohdr unpack" writes
// out a header pack, the go module's scan of each Go file up to the end of its
// imports, for the plan, and lists the pack's Go files in directories named
// like Go files. "gohdr weigh" weighs the plan's steps by the Go
// source they compile. "gohdr cc" answers the plan's C compiler probes.
// "gohdr linkid" makes a link's build ID. "gohdr modunzip" extracts a
// downloaded module, checking its checksum.
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
	"fmt"
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
// -n plan: the bytes of Go source the step's lines name, plus a fixed cost
// for starting a compile, a stand-in for its compile time. A relative file is
// in the directory the plan last changed to, from the current one, as the go
// command prints "cd" only when the directory changes.
func weigh(planPath string) error {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	start, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := start
	weights := map[string]int64{}
	var steps []string
	step := ""
	for _, line := range strings.Split(string(data), "\n") {
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
		if step == "" {
			continue
		}
		for _, f := range strings.Fields(line) {
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
	w := bufio.NewWriter(os.Stdout)
	for _, step := range steps {
		fmt.Fprintf(w, "%s %d\n", step, weights[step])
	}
	return w.Flush()
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
	default:
		err = fmt.Errorf("usage: gohdr unpack <pack> <dir> <list>, gohdr weigh <plan>, gohdr cc <plan>, gohdr linkid <importcfg> <salt>, or gohdr modunzip <zip> <module>@<version> <dir> <checksum>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gohdr:", err)
		os.Exit(1)
	}
}
`
