package main

import (
	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// golangciLintCommit is the dagger/golangci-lint commit whose manifest
// module writes the archives' manifest, in the format its export driver
// reads.
const golangciLintCommit = "cec25a2af6e0d16d555da7e256424a76ee713599"

// manifestSource is the manifest module beside golangci-lint's export
// driver, at golangciLintCommit, which needs only the standard library.
func manifestSource() *dagger.Directory {
	return core.Git("https://github.com/dagger/golangci-lint").
		Commit(golangciLintCommit).
		Tree().
		Directory("helpers/export-driver/manifest")
}

// manifestTool builds golangci-lint's write-manifest, which writes the
// manifest of a directory of archives for the export driver, from
// manifestSource. Like gohdr, it is built in image, gomod's pinned
// toolchain, statically, and runs in whatever image the module builds with.
func manifestTool(image string) *dagger.File {
	return core.NewContainer().From(image).
		WithEnvVariable("CGO_ENABLED", "0").
		WithMountedDirectory("/src", manifestSource()).
		WithWorkdir("/src").
		WithExec([]string{"go", "build", "-o", "/write-manifest", "./cmd/write-manifest"}, bare).
		File("/write-manifest")
}
