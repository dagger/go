package main

import "golang.org/x/sync/errgroup"

func main() {
	var g errgroup.Group
	g.Go(func() error { return nil })
	if err := g.Wait(); err != nil {
		panic(err)
	}
}
