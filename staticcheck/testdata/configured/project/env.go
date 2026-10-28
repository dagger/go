package configured

import "os"

// Home reaches os, so linting type-checks the toolchain's standard library.
func Home() string {
	return os.Getenv("HOME")
}
