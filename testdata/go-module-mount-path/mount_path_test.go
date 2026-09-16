package mountpath

import (
	"os"
	"testing"
)

// The e2e check runs this module with mountPath "/src/hugo", mirroring a
// repository whose tests expect the repository name in their working
// directory.
const wantMountPath = "/src/hugo"

func TestWorkingDirectoryUnderMountPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := wantMountPath + "/testdata/go-module-mount-path"; wd != want {
		t.Fatalf("working directory = %q, want %q", wd, want)
	}
	// Nested Dagger clients find the workspace boundary by walking up to .git,
	// so the marker has to move with the mount.
	if _, err := os.Stat(wantMountPath + "/.git/HEAD"); err != nil {
		t.Fatalf("workspace .git marker is missing under the mount path: %v", err)
	}
}
