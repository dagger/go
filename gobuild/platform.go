package main

import (
	"context"
	"strings"

	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// machines are the machines an engine runs on, as uname -m names them, with
// their GOARCH.
var machines = []struct{ uname, goarch string }{{"x86_64", "amd64"}, {"aarch64", "arm64"}}

// machineTools is a directory of the tool name as build builds it for each
// machine an engine runs on, under that machine's uname -m, and at name a
// script that runs the one for the exec's machine. A build mounts the same
// directory on every machine, so what it compiles is the same build on
// every machine, whichever machine's tool runs; and it is declared for
// platform, the build's target (Directory.withPlatform), so that an engine
// image of that platform ships the tool its engine's machine runs.
func machineTools(name, platform string, build func(goarch string) *dagger.File) *dagger.Directory {
	dir := core.NewDirectory().WithNewFile(name, "#!/bin/sh\nexec \"${0%/*}/$(uname -m)/"+name+"\" \"$@\"\n",
		core.DirectoryWithNewFileOpts{Permissions: 0o755})
	for _, m := range machines {
		dir = dir.WithFile(m.uname+"/"+name, build(m.goarch))
	}
	return dir.WithPlatform(core.Platform(platform))
}

// targetPlatform is the GOOS/GOARCH ctr builds for, as a Platform: its
// GOOS and GOARCH, or the engine's platform's.
func targetPlatform(ctx context.Context, ctr *dagger.Container) (string, error) {
	names, values, err := envOf(ctx, ctr)
	if err != nil {
		return "", err
	}
	var goos, goarch string
	for i, n := range names {
		switch n {
		case "GOOS":
			goos = values[i]
		case "GOARCH":
			goarch = values[i]
		}
	}
	if goos == "" || goarch == "" {
		engine, err := core.DefaultPlatform(ctx)
		if err != nil {
			return "", err
		}
		f := strings.Split(string(engine), "/")
		if goos == "" {
			goos = f[0]
		}
		if goarch == "" && len(f) > 1 {
			goarch = f[1]
		}
	}
	return goos + "/" + goarch, nil
}

// platform is the target's GOOS/GOARCH, as a Platform.
func (t target) platform() string {
	return t.goos + "/" + t.goarch
}
