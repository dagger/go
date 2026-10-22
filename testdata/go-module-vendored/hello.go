// Package vendored greets through a vendored dependency.
package vendored

import "example.com/greet"

// Greeting is the greeting for the vendored module.
func Greeting() string { return greet.Hello("vendor") }
