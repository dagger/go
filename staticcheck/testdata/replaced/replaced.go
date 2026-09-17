// Package replaced imports a local module with embedded data.
package replaced

import "example.com/clean"

func Message() string {
	return clean.Message
}
