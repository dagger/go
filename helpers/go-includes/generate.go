package main

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// generateDirectoriesFor scans command lines as go generate does, rather than
// mistaking go:generate:include annotations for executable directives.
func (index *localIndex) generateDirectoriesFor(moduleRoot string) ([]string, error) {
	seen := map[string]bool{}
	for _, file := range index.goFilesByModule[moduleRoot] {
		// Go ignores these files, even when selecting their package explicitly.
		name := path.Base(file)
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(file)))
		if err != nil {
			return nil, err
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "//go:generate ") || strings.HasPrefix(line, "//go:generate\t") {
				seen[path.Dir(file)] = true
				break
			}
		}
	}
	var dirs []string
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}
