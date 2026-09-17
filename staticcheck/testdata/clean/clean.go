// Package clean is a valid Go module with embedded assets.
package clean

import _ "embed"

//go:embed assets/message.txt
var Message string
