// Package quiet is inside the inner module but has its own configuration, so
// the parent project must skip it: under the parent's, staticcheck rejects
// the pattern.
package quiet

import "regexp"

// Pattern never compiles.
func Pattern() *regexp.Regexp {
	return regexp.MustCompile("[")
}
