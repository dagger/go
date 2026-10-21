package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// headerPackFile names the scan's header pack: every non-test Go file of the
// workspace's modules up to the end of its imports, plus its go:embed lines,
// and every go.mod whole, in gohdr's pack format. That is what a "go build -n"
// plan reads of Go source, so a plan keyed on the pack survives edits inside
// function bodies. The go module's per-package build plans from it, which
// saves it an exec of its own on every build.
const headerPackFile = "_headers_.gohdr"

func (index *localIndex) writeHeaderPack(path string) error {
	var files []string
	for _, root := range index.moduleRoots {
		if root == "." {
			files = append(files, "go.mod")
		} else {
			files = append(files, root+"/go.mod")
		}
		for _, f := range index.goFilesByModule[root] {
			if !strings.HasSuffix(f, "_test.go") {
				files = append(files, f)
			}
		}
	}
	sort.Strings(files)
	datas := make([][]byte, len(files))
	errs := make([]error, len(files))
	forEach(len(files), func(i int) {
		datas[i], errs[i] = os.ReadFile(filepath.Join(index.root, files[i]))
		if errs[i] == nil && strings.HasSuffix(files[i], ".go") {
			datas[i] = goHeader(files[i], datas[i])
		}
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
		fmt.Fprintf(w, "%s\n%o\n%d\n", rel, 0o644, len(data))
		w.Write(data)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return out.Close()
}

// goHeader returns a Go file up to the end of its import declarations, plus
// its go:embed lines. A file that does not parse is kept whole.
func goHeader(name string, src []byte) []byte {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || f == nil {
		return src
	}
	end := f.Name.End()
	for _, d := range f.Decls {
		if d.End() > end {
			end = d.End()
		}
	}
	off := min(fset.Position(end).Offset, len(src))
	var out bytes.Buffer
	out.Write(src[:off])
	out.WriteByte('\n')
	for _, line := range strings.Split(string(src[off:]), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "//go:embed") {
			out.WriteString(t)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}
