package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// generateDirectoriesFor scans command lines as go generate does, rather than
// mistaking go:generate:include annotations for executable directives.
func (index *localIndex) generateDirectoriesFor(moduleRoot string, patterns []string) ([]string, error) {
	seen := map[string]bool{}
	for _, file := range index.goFilesByModule[moduleRoot] {
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
		selected, err := directorySelected(dir, patterns)
		if err != nil {
			return nil, err
		}
		if selected {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func directorySelected(dir string, patterns []string) (bool, error) {
	hasInclude, included, excluded := false, false, false
	for _, pattern := range patterns {
		exclude := strings.HasPrefix(pattern, "!")
		matched, err := matchDirectory(strings.TrimPrefix(pattern, "!"), dir)
		if err != nil {
			return false, err
		}
		if exclude {
			excluded = excluded || matched
		} else {
			hasInclude = true
			included = included || matched
		}
	}
	return (!hasInclude || included) && !excluded, nil
}

// matchDirectory extends path.Match with ** as a complete path segment.
// Literal paths match exactly, including module roots; they never imply recursion.
func matchDirectory(pattern, dir string) (bool, error) {
	pattern = path.Clean(strings.TrimPrefix(pattern, "/"))
	if pattern == ".." || strings.HasPrefix(pattern, "../") {
		return false, fmt.Errorf("generate pattern escapes workspace: %q", pattern)
	}
	parts := strings.Split(pattern, "/")
	for _, part := range parts {
		if _, err := path.Match(part, ""); err != nil {
			return false, fmt.Errorf("invalid generate pattern %q: %w", pattern, err)
		}
	}
	var match func([]string, []string) bool
	match = func(pattern, name []string) bool {
		if len(pattern) == 0 {
			return len(name) == 0
		}
		if pattern[0] == "**" {
			return match(pattern[1:], name) || (len(name) > 0 && match(pattern, name[1:]))
		}
		if len(name) == 0 {
			return false
		}
		ok, _ := path.Match(pattern[0], name[0])
		return ok && match(pattern[1:], name[1:])
	}
	if pattern == "." {
		parts = nil
	}
	name := strings.Split(dir, "/")
	if dir == "." {
		name = nil
	}
	return match(parts, name), nil
}
