package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A link whose dependency's object code couldn't be read fails with that
// dependency's error, named by its import path, also when the dependency
// call built it and it is no node of the walk.
func TestWalkLinkDependencyError(t *testing.T) {
	dep := &result{done: make(chan struct{}), linkDone: make(chan struct{}),
		importPath: "example.com/dep", linkErr: errors.New("object code unreadable")}
	close(dep.done)
	close(dep.linkDone)
	nodes := map[string]*node{"b2": {ID: "b2", Name: "b2", ImportPath: "example.com/m", Deps: []string{"b1"}, Link: true}}
	w := &walker{heights: map[string]int64{}}
	_, err := w.walk(context.Background(), nodes, map[string]*result{"b1": dep})
	if err == nil || !strings.Contains(err.Error(), "example.com/dep: object code unreadable") {
		t.Fatalf("walk: %v, want the dependency's error named by its import path", err)
	}
}
