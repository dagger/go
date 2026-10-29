package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// headerPackFile names the scan's header pack: every Go file of the
// workspace's modules up to the end of its imports, plus its go:embed lines,
// and every go.mod whole, in gohdr's pack format. Test files are in it too:
// a build never compiles them, but a //go:embed may name one. So are the
// other files in a directory named like a Go file, whole: the plan's view of
// the source leaves out everything named *.go, directories included, and a
// //go:embed may name what such a directory holds. That is what a
// "go build -n" plan reads of Go source, so a plan keyed on the pack survives
// edits inside function bodies. The go module's per-package build plans from
// it, which saves it an exec of its own on every build.
const headerPackFile = "_headers_.gohdr"

func (index *localIndex) writeHeaderPack(path string) error {
	var files []string
	for _, root := range index.moduleRoots {
		if root == "." {
			files = append(files, "go.mod")
		} else {
			files = append(files, root+"/go.mod")
		}
		files = append(files, index.goFilesByModule[root]...)
		files = append(files, index.goDirFilesByModule[root]...)
	}
	sort.Strings(files)
	goFiles := index.goFileFacts()
	datas := make([][]byte, len(files))
	modes := make([]fs.FileMode, len(files))
	errs := make([]error, len(files))
	forEach(len(files), func(i int) {
		if facts, ok := goFiles[files[i]]; ok {
			datas[i], modes[i], errs[i] = facts.header, facts.headerMode, facts.headerErr
			return
		}
		full := filepath.Join(index.root, files[i])
		modes[i] = 0o644
		// A symlink this pack holds only for a //go:embed or a file in a
		// directory named like a Go file, stays one, its target as its
		// data, so the embed fails, or leaves it out, as go decides.
		if filepath.Base(files[i]) != "go.mod" {
			if fi, err := os.Lstat(full); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				target, err := os.Readlink(full)
				datas[i], modes[i], errs[i] = []byte(target), fs.ModeSymlink|0o777, err
				return
			}
		}
		datas[i], errs[i] = os.ReadFile(full)
	})
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	fmt.Fprintln(w, "gohdr 1")
	for i, rel := range files {
		// A file that can't be read is left out: the module's own scan
		// records why, in its .err, and other modules are unaffected.
		if errs[i] != nil {
			continue
		}
		data := datas[i]
		fmt.Fprintf(w, "%s\n%o\n%d\n", rel, uint32(modes[i]), len(data))
		w.Write(data)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return out.Close()
}

// headerEnd follows a header's go:embed lines. Up to Go 1.25, go/build (and
// the go command's copy of it) looks for go:embed lines only when its scan of
// the imports stops at a token before the end of the file: skipping the
// comments after the imports to the end, it finds none, and go build -n
// leaves the package's -embedcfg out, so its compile fails with "build system
// did not supply embed configuration". Go 1.26 scans the whole file either
// way. Nothing compiles a header, so what the token is doesn't matter.
const headerEnd = "var _ struct{}\n"

// goHeader returns a Go file up to the end of its import declarations, plus
// its go:embed lines, then headerEnd when there are any. A file that does not
// parse is kept whole.
func goHeader(name string, src []byte) []byte {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || f == nil {
		return src
	}
	return headerOf(fset, f, src)
}

// headerOf is goHeader for a file already parsed, in full or imports only:
// only the declarations an imports-only parse holds, the leading imports,
// mark the end of the header.
func headerOf(fset *token.FileSet, f *ast.File, src []byte) []byte {
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
