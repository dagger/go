package main

import (
	"reflect"
	"testing"
)

func TestSuiteFlags(t *testing.T) {
	flags, err := splitGOFLAGS(`-race -count=1 -timeout=3m -p=2 '-run=TestOne|TestTwo' '-ldflags=-s -w' -short`)
	if err != nil {
		t.Fatal(err)
	}
	args, p, err := suiteFlags(flags)
	want := []string{"-test.count=1", "-test.timeout=3m", "-test.run=TestOne|TestTwo", "-test.short=true"}
	if err != nil || p != 2 || !reflect.DeepEqual(args, want) {
		t.Fatalf("%q, %d, %v", args, p, err)
	}
	for _, flag := range []string{"-coverprofile=out", "-fuzz=FuzzX", "-exec=wrapper", "-failfast", "-p=0"} {
		if _, _, err := suiteFlags([]string{flag}); err == nil {
			t.Fatalf("silently accepted %s", flag)
		}
	}
}
