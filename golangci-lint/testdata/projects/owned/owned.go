// Package owned is claimed by its own configuration, so the parent project
// must leave it alone: under the parent's, staticcheck rejects the pattern.
package owned

import "regexp"

// Pattern never compiles.
func Pattern() *regexp.Regexp {
	return regexp.MustCompile("[")
}
