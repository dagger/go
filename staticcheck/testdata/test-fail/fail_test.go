package testfail

import (
	"regexp"
	"testing"
)

func TestBroken(t *testing.T) {
	t.Log(regexp.MustCompile("["))
}
