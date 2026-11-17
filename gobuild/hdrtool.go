package main

// hdrDir is where an exec that runs gohdr mounts it (Gobuild's gohdr), and
// hdrBin is the command that runs it.
const (
	hdrDir = "/gobuild/gohdr.d"
	hdrBin = hdrDir + "/gohdr"
)
