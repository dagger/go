// Command archivescheck checks a directory of archives for golangci-lint's
// export driver against the go command, in the module in the current
// directory: the manifest lists the plain packages "go list -deps -test
// ./..." lists, each with the digest the driver computes from go list's own
// files, cgo's output included, and the settings go env reports; and each
// archive's export data type-checks the module's packages. It prints what
// differs, or nothing.
//
// The digest is a copy of the driver's (dagger/golangci-lint,
// helpers/export-driver/manifest.go), run on what go list -compiled reports,
// as go/packages reports it to the driver.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
)

type listed struct {
	ImportPath, ForTest, Dir, Name                   string
	Module                                           *struct{ Main bool }
	GoFiles, CgoFiles, CompiledGoFiles, EmbedFiles   []string
	CFiles, CXXFiles, HFiles, SFiles, MFiles, FFiles []string
	SwigFiles, SwigCXXFiles, SysoFiles               []string
}

type manifest struct {
	Version    int      `json:"version"`
	GoVersion  string   `json:"goVersion"`
	GOOS       string   `json:"goos"`
	GOARCH     string   `json:"goarch"`
	CgoEnabled bool     `json:"cgoEnabled"`
	BuildTags  []string `json:"buildTags"`
	Packages   map[string]struct {
		Archive string `json:"archive"`
		Sources string `json:"sources"`
	} `json:"packages"`
}

func main() {
	dir := os.Args[1]
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	must(err)
	var m manifest
	must(json.Unmarshal(data, &m))
	out, err := exec.Command("go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED").Output()
	must(err)
	var env map[string]string
	must(json.Unmarshal(out, &env))
	want := manifest{Version: 1, GoVersion: env["GOVERSION"], GOOS: env["GOOS"], GOARCH: env["GOARCH"], CgoEnabled: env["CGO_ENABLED"] == "1", BuildTags: []string{}}
	if got := (manifest{m.Version, m.GoVersion, m.GOOS, m.GOARCH, m.CgoEnabled, m.BuildTags, nil}); !reflect.DeepEqual(got, want) {
		fail("settings: %+v, want %+v", got, want)
	}
	cmd := exec.Command("go", "list", "-e", "-compiled", "-deps", "-test",
		"-json=ImportPath,ForTest,Dir,Name,Module,GoFiles,CgoFiles,CompiledGoFiles,EmbedFiles,CFiles,CXXFiles,HFiles,SFiles,MFiles,FFiles,SwigFiles,SwigCXXFiles,SysoFiles", "./...")
	cmd.Stderr = os.Stderr
	out, err = cmd.Output()
	must(err)
	var plain []listed
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else {
			must(err)
		}
		if p.ForTest != "" || strings.Contains(p.ImportPath, " [") || strings.HasSuffix(p.ImportPath, ".test") || p.ImportPath == "unsafe" {
			continue
		}
		plain = append(plain, p)
	}
	var names []string
	for _, p := range plain {
		names = append(names, p.ImportPath)
		e, ok := m.Packages[p.ImportPath]
		if !ok {
			fail("%s: not in the manifest", p.ImportPath)
			continue
		}
		abs := func(groups ...[]string) []string {
			var out []string
			for _, g := range groups {
				for _, f := range g {
					if !filepath.IsAbs(f) {
						f = filepath.Join(p.Dir, f)
					}
					out = append(out, f)
				}
			}
			return out
		}
		sum, err := sourcesDigest(p.Dir, abs(p.GoFiles, p.CgoFiles), abs(p.CompiledGoFiles),
			abs(p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.FFiles, p.SFiles, p.SwigFiles, p.SwigCXXFiles, p.SysoFiles), abs(p.EmbedFiles))
		must(err)
		if e.Sources != sum {
			fail("%s: sources %s, the driver's %s", p.ImportPath, e.Sources, sum)
		}
		if fi, err := os.Stat(filepath.Join(dir, e.Archive)); err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			fail("%s: archive %s: %v", p.ImportPath, e.Archive, err)
		}
	}
	for name := range m.Packages {
		if !slices.Contains(names, name) {
			fail("%s: in the manifest, not in go list", name)
		}
	}
	// The module's own packages type-check from source against the
	// archives' export data, as golangci-lint's do through the driver.
	imp := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		e, ok := m.Packages[path]
		if !ok {
			return nil, fmt.Errorf("%s: no archive", path)
		}
		return os.Open(filepath.Join(dir, e.Archive))
	})
	for _, p := range plain {
		if p.Module == nil || !p.Module.Main {
			continue
		}
		fset := token.NewFileSet()
		var files []*ast.File
		for _, f := range p.CompiledGoFiles {
			if !filepath.IsAbs(f) {
				f = filepath.Join(p.Dir, f)
			}
			af, err := parser.ParseFile(fset, f, nil, 0)
			must(err)
			files = append(files, af)
		}
		conf := types.Config{Importer: imp}
		if _, err := conf.Check(p.ImportPath, fset, files, nil); err != nil {
			fail("%s: doesn't type-check against the archives: %v", p.ImportPath, err)
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		fmt.Println(p)
	}
	fmt.Printf("checked %d packages\n", len(plain))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func sourcesDigest(dir string, goFiles, compiled, other, embed []string) (string, error) {
	var lines []string
	add := func(name, path string, generated bool) error {
		h, err := fileSHA256(path, generated)
		if err != nil {
			return err
		}
		lines = append(lines, name+" "+h+"\n")
		return nil
	}
	rel := func(path string) string {
		if r, err := filepath.Rel(dir, path); err == nil {
			return filepath.ToSlash(r)
		}
		return path
	}
	usesCgo := len(compiled) != len(goFiles)
	for _, c := range compiled {
		usesCgo = usesCgo || !slices.Contains(goFiles, c)
	}
	if usesCgo {
		lines = append(lines, "cgo\n")
		generated := 0
		for _, c := range compiled {
			var err error
			if slices.Contains(goFiles, c) {
				err = add(rel(c), c, false)
			} else {
				err = add(fmt.Sprintf("cgo-output-%d", generated), c, true)
				generated++
			}
			if err != nil {
				return "", err
			}
		}
	} else {
		for _, g := range goFiles {
			if err := add(rel(g), g, false); err != nil {
				return "", err
			}
		}
	}
	for _, group := range [][]string{other, embed} {
		for _, o := range group {
			if err := add(rel(o), o, false); err != nil {
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

func fileSHA256(path string, generated bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if !generated {
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	file := token.NewFileSet().AddFile(path, -1, len(data))
	var s scanner.Scanner
	s.Init(file, data, nil, scanner.ScanComments)
	last := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && strings.HasPrefix(lit, "//line ") {
			off := file.Offset(pos)
			h.Write(data[last:off])
			last = off + len(lit)
		}
	}
	h.Write(data[last:])
	return hex.EncodeToString(h.Sum(nil)), nil
}
