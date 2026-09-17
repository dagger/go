// Package configured inherits its Staticcheck config from a parent directory.
package configured

import "regexp"

func Allowed() *regexp.Regexp {
	return regexp.MustCompile("[")
}
