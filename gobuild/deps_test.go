package main

import (
	"reflect"
	"testing"
)

// layerPaths is the import paths of each layer of pkgs, in order.
func layerPaths(pkgs []*node, layers [][]int) [][]string {
	out := make([][]string, len(layers))
	for i, l := range layers {
		for _, k := range l {
			out[i] = append(out[i], pkgs[k].ImportPath)
		}
	}
	return out
}

func TestDependencyLayers(t *testing.T) {
	// Named by stable name, importing by stable name, as the dependency call
	// takes them; the order is the call's, by stable name.
	pkgs := []*node{
		{Name: "a", ImportPath: "errors"},
		{Name: "b", ImportPath: "context", Deps: []string{"a"}},
		{Name: "c", ImportPath: "os/exec", Deps: []string{"a"}},
		{Name: "d", ImportPath: "example.com/lib", Deps: []string{"b"}},
		{Name: "e", ImportPath: "example.com/lib/sub", Deps: []string{"d", "a"}},
		{Name: "f", ImportPath: "example.org/extra", Deps: []string{"e", "c"}},
	}
	for _, tc := range []struct {
		name   string
		shared []string
		want   [][]string
		n      int
	}{
		{
			name: "no shared imports: one call",
			want: [][]string{{"errors", "context", "os/exec", "example.com/lib", "example.com/lib/sub", "example.org/extra"}},
		},
		{
			name:   "shared imports reach the standard library and a module",
			shared: []string{"example.com/lib/sub"},
			want:   [][]string{{"errors", "context"}, {"example.com/lib", "example.com/lib/sub"}, {"os/exec", "example.org/extra"}},
			n:      2,
		},
		{
			name:   "shared imports reach only the standard library",
			shared: []string{"context"},
			want:   [][]string{{"errors", "context"}, {"os/exec", "example.com/lib", "example.com/lib/sub", "example.org/extra"}},
			n:      1,
		},
		{
			name:   "shared imports reach everything: nothing left for a last call",
			shared: []string{"example.org/extra"},
			want:   [][]string{{"errors", "context", "os/exec"}, {"example.com/lib", "example.com/lib/sub", "example.org/extra"}},
			n:      2,
		},
		{
			name:   "shared imports the build doesn't import",
			shared: []string{"example.net/absent"},
			want:   [][]string{{"errors", "context", "os/exec", "example.com/lib", "example.com/lib/sub", "example.org/extra"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layers, n := dependencyLayers(pkgs, tc.shared)
			if got := layerPaths(pkgs, layers); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("layers %v, want %v", got, tc.want)
			}
			if n != tc.n {
				t.Errorf("%d shared layers, want %d", n, tc.n)
			}
			// Each layer imports only its own packages and earlier layers'.
			seen := map[string]bool{}
			for _, l := range layers {
				in := map[string]bool{}
				for _, k := range l {
					in[pkgs[k].Name] = true
				}
				for _, k := range l {
					for _, d := range pkgs[k].Deps {
						if !in[d] && !seen[d] {
							t.Errorf("%s imports %s, built in a later call", pkgs[k].ImportPath, d)
						}
					}
				}
				for name := range in {
					seen[name] = true
				}
			}
		})
	}
}
