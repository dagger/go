package main

// writeManifestDir is where an exec that runs write-manifest mounts it (the
// writeManifest argument of Archives), and writeManifestBin is the command
// that runs it.
const (
	writeManifestDir = "/gobuild/write-manifest.d"
	writeManifestBin = writeManifestDir + "/write-manifest"
)
