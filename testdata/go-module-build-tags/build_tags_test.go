package buildtags

import "testing"

// The e2e check runs this module with goflags "-tags=extended". Without the
// tag, default.go is compiled instead of extended.go and this test fails.
func TestBuiltWithExtendedTag(t *testing.T) {
	if !hasExtended {
		t.Fatal("built without the extended build tag; GOFLAGS was not applied")
	}
}
