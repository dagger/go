package main

import (
	"dagger.io/dagger/core"
	"dagger/gobuild/internal/dagger"
)

// golangciLintCommit is the dagger/golangci-lint commit whose manifest
// module writes the archives' manifest, in the format its export driver
// reads.
const golangciLintCommit = "7d9cbaf83a85addefb99346297301eeb321d98c6"

// manifestSource is the manifest module beside golangci-lint's export
// driver, at golangciLintCommit, which needs only the standard library.
func manifestSource() *dagger.Directory {
	return core.Git("https://github.com/dagger/golangci-lint").
		Commit(golangciLintCommit).
		Tree().
		Directory("helpers/export-driver/manifest")
}

// writeManifestDir is where an exec that runs write-manifest mounts it
// (manifestTools), and writeManifestBin is the command that runs it.
const (
	writeManifestDir = "/gobuild/write-manifest.d"
	writeManifestBin = writeManifestDir + "/write-manifest"
)

// manifestTools builds golangci-lint's write-manifest, which writes the
// manifest of a directory of archives for the export driver, from
// manifestSource, for every machine an engine runs on (machineTools),
// declared for platform. Like gohdr, it is built in image, gomod's pinned
// toolchain, statically, and runs in whatever image the module builds with.
func manifestTools(image, platform string) *dagger.Directory {
	return toolsFor("write-manifest", image, platform, func(goarch string) *dagger.File {
		return core.NewContainer().From(image).
			WithEnvVariable("CGO_ENABLED", "0").
			WithEnvVariable("GOOS", "linux").
			WithEnvVariable("GOARCH", goarch).
			WithMountedDirectory("/src", manifestSource()).
			WithWorkdir("/src").
			WithExec([]string{"go", "build", "-o", "/write-manifest", "./cmd/write-manifest"}, bare).
			File("/write-manifest")
	})
}
