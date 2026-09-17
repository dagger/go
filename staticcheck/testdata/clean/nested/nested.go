// Package nested is discovered independently of its parent.
package nested

import _ "embed"

const Answer = 42

//go:embed assets/nested.txt
var Message string
