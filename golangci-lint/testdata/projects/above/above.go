// Package above has no configuration of its own: the one in its parent
// directory disables errcheck, which this file would fail.
package above

import "os"

// Clean removes a file and ignores the result.
func Clean(name string) {
	os.Remove(name)
}
