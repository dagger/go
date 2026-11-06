package main

import (
	"context"
	"strings"

	sdk "dagger.io/dagger"
	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
	"github.com/dagger/querybuilder"
)

// declared is dir declared as made for platform (Directory.withPlatform),
// whose contents are the same whichever platform's toolchain made them: an
// engine of any platform that makes it for the same platform finds it, such
// as the arm64 builds an engine image made on amd64 ships. The SDK this
// module builds with has no binding for withPlatform yet, so the query is
// made here, as envOf makes one.
func declared(ctx context.Context, dir *dagger.Directory, platform string) (*dagger.Directory, error) {
	id, err := dir.ID(ctx)
	if err != nil {
		return nil, err
	}
	q := querybuilder.Query().Client(sdk.DefaultGraphQLClient())
	var declaredID core.ID
	if err := q.Select("node").Arg("id", id).InlineFragment("Directory").
		Select("withPlatform").Arg("platform", platform).
		Select("id").
		Bind(&declaredID).
		Execute(ctx); err != nil {
		return nil, err
	}
	return core.Ref[*core.Directory](new(core.Query).WithGraphQLQuery(q), declaredID), nil
}

// machines are the machines an engine runs on, as uname -m names them, with
// their GOARCH.
var machines = []struct{ uname, goarch string }{{"x86_64", "amd64"}, {"aarch64", "arm64"}}

// machineTools is a directory of the tool name as build builds it for each
// machine an engine runs on, under that machine's uname -m, and at name a
// script that runs the one for the exec's machine. A build mounts the same
// directory on every machine, so what it compiles is the same build on
// every machine, whichever machine's tool runs; and it is declared for
// platform, the build's target, so that an engine image of that platform
// ships the tool its engine's machine runs.
func machineTools(ctx context.Context, name, platform string, build func(goarch string) *dagger.File) (*dagger.Directory, error) {
	dir := core.NewDirectory().WithNewFile(name, "#!/bin/sh\nexec \"${0%/*}/$(uname -m)/"+name+"\" \"$@\"\n",
		core.DirectoryWithNewFileOpts{Permissions: 0o755})
	for _, m := range machines {
		dir = dir.WithFile(m.uname+"/"+name, build(m.goarch))
	}
	return declared(ctx, dir, platform)
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
