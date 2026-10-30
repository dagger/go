package main

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// generateDirectoriesFor scans command lines as go generate does, rather than
// mistaking go:generate:include annotations for executable directives.
func (index *localIndex) generateDirectoriesFor(moduleRoot string) ([]string, error) {
	seen := map[string]bool{}
	facts := index.goFileFacts()
	for _, file := range index.goFilesByModule[moduleRoot] {
		// Go ignores these files, even when selecting their package explicitly.
		name := path.Base(file)
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		if err := facts[file].readErr; err != nil {
			return nil, err
		}
		if facts[file].generates {
			seen[path.Dir(file)] = true
		}
	}
	var dirs []string
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// generateContainersFor uses the same parsed comments and directory scope as
// go:generate:include. Values are passed unchanged to Workspace.resolve.
func (index *localIndex) generateContainersFor(moduleRoot string) (map[string]string, error) {
	directives, err := index.directives(moduleRoot)
	if err != nil {
		return nil, err
	}
	containers := map[string]string{}
	positions := map[string]string{}
	for _, directive := range directives {
		if !directive.hasName("go:generate:container") {
			continue
		}
		args, err := directive.args()
		if err != nil {
			return nil, err
		}
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return nil, fmt.Errorf("%s: //go:generate:container requires one non-empty value", directive.position)
		}
		dir := path.Dir(directive.filePath)
		if previous, ok := containers[dir]; ok && previous != args[0] {
			return nil, fmt.Errorf("%s: conflicting //go:generate:container values in directory %s: %q (at %s) and %q", directive.position, dir, previous, positions[dir], args[0])
		}
		containers[dir] = args[0]
		positions[dir] = directive.position
	}
	return containers, nil
}
