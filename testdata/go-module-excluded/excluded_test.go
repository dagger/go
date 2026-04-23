package gomoduleexcluded

import "testing"

func TestExcludedModuleIsNotRun(t *testing.T) {
	t.Fatal("this module should be excluded by the e2e check")
}
