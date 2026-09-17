//go:build never

// The e2e checks use this module as a test command that fails without any Go
// test failing: discovery sees a *_test.go file, but go test finds no
// buildable package and exits non-zero.
package nopackages
