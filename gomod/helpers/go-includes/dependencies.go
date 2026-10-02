package main

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
)

// dependenciesFor returns bindings in name order, encoded as name TAB address.
func (index *localIndex) dependenciesFor(moduleRoot, operation string) (map[string][]string, error) {
	directives, err := index.directives(moduleRoot)
	if err != nil {
		return nil, err
	}
	bindings := map[string]map[string]string{}
	positions := map[string]map[string]string{}
	for _, directive := range directives {
		if !directive.hasName("go:" + operation + ":dependency") {
			continue
		}
		args, err := directive.args()
		if err != nil {
			return nil, err
		}
		name, _, _ := directive.line()
		if len(args) != 2 || args[0] == "" || strings.IndexFunc(args[0], unicode.IsSpace) >= 0 ||
			!(strings.HasPrefix(args[1], "dag://") || strings.HasPrefix(args[1], "dag+service://")) ||
			strings.IndexFunc(args[1], unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("%s: //%s requires a binding name and a dag service link", directive.position, name)
		}
		dir := path.Dir(directive.filePath)
		if bindings[dir] == nil {
			bindings[dir] = map[string]string{}
			positions[dir] = map[string]string{}
		}
		if previous, ok := bindings[dir][args[0]]; ok && previous != args[1] {
			return nil, fmt.Errorf("%s: conflicting service dependency %q in directory %s: %q (at %s) and %q", directive.position, args[0], dir, previous, positions[dir][args[0]], args[1])
		}
		bindings[dir][args[0]] = args[1]
		positions[dir][args[0]] = directive.position
	}
	result := map[string][]string{}
	for dir, byName := range bindings {
		for name, address := range byName {
			result[dir] = append(result[dir], name+"\t"+address)
		}
		sort.Strings(result[dir])
	}
	return result, nil
}
