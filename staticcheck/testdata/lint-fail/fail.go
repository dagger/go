// Package lintfail contains a Staticcheck diagnostic.
package lintfail

import "regexp"

func Broken() *regexp.Regexp {
	return regexp.MustCompile("[")
}
