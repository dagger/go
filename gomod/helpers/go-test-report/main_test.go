package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func report(events ...event) io.Reader {
	var b strings.Builder
	e := json.NewEncoder(&b)
	for _, v := range events {
		if err := e.Encode(v); err != nil {
			panic(err)
		}
	}
	return strings.NewReader(b.String())
}

func TestSummarizeAttribution(t *testing.T) {
	selected := []selection{{"example.com/p", []string{"TestOK", "TestBad"}}, {"example.com/q", []string{"TestOther"}}}
	base := []event{
		{Action: "pass", Package: "example.com/p", Test: "TestOK"},
		{Action: "pass", Package: "example.com/q", Test: "TestOther"},
		{Action: "pass", Package: "example.com/q"},
	}
	for _, tc := range []struct {
		name   string
		events []event
		want   map[check]string // Empty means pass; otherwise the exact error.
	}{
		{"named failure", []event{
			{Action: "output", Package: "example.com/p", Test: "TestBad/child", Output: "child assertion"},
			{Action: "fail", Package: "example.com/p", Test: "TestBad"},
			{Action: "fail", Package: "example.com/p"},
		}, map[check]string{{"example.com/p", "TestOK"}: "", {"example.com/p", "TestBad"}: "TestBad failed", {"example.com/q", "TestOther"}: ""}},
		{"undiscovered failure", []event{
			{Action: "fail", Package: "example.com/p", Test: "TestHidden"},
			{Action: "fail", Package: "example.com/p"},
		}, map[check]string{{"example.com/p", "TestOK"}: "package failed outside a selected test check", {"example.com/q", "TestOther"}: ""}},
		{"teardown", []event{
			{Action: "pass", Package: "example.com/p", Test: "TestBad"},
			{Action: "fail", Package: "example.com/p"},
		}, map[check]string{{"example.com/p", "TestOK"}: "package failed outside a selected test check", {"example.com/p", "TestBad"}: "package failed outside a selected test check", {"example.com/q", "TestOther"}: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := summarize(report(append(append([]event{}, base...), tc.events...)...), selected, 1)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range tc.want {
				if value, ok := got[key]; !ok || value != want {
					t.Errorf("%v: got %q, want %q", key, value, want)
				}
			}
		})
	}
}

// A selected test with no events in a package that failed by a named test,
// as -failfast leaves the tests after the first failure, did not run. One that
// started and never finished has no result either.
func TestAbsentAndInterruptedTests(t *testing.T) {
	selected := []selection{{"example.com/p", []string{"TestBad", "TestAfterFailure", "TestInterrupted", "TestOK"}}}
	got, err := summarize(report(
		event{Action: "pass", Package: "example.com/p", Test: "TestOK"},
		event{Action: "fail", Package: "example.com/p", Test: "TestBad"},
		event{Action: "run", Package: "example.com/p", Test: "TestInterrupted"},
		event{Action: "fail", Package: "example.com/p"},
	), selected, 1)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"TestBad":          "TestBad failed",
		"TestAfterFailure": "TestAfterFailure did not run: go test failed example.com/p without running it",
		"TestInterrupted":  "no successful result for TestInterrupted (go test exit 1)",
		"TestOK":           "",
	} {
		if message, ok := got[check{"example.com/p", name}]; !ok || message != want {
			t.Errorf("%s: got %q, want %q", name, message, want)
		}
	}
}

// A passing package does not pass a selected test it never ran, such as one in
// a file a build constraint excludes.
func TestSelectedTestThatNeverRan(t *testing.T) {
	selected := []selection{{"example.com/p", []string{"TestRan", "TestExcluded"}}}
	got, err := summarize(report(
		event{Action: "pass", Package: "example.com/p", Test: "TestRan"},
		event{Action: "pass", Package: "example.com/p"},
	), selected, 0)
	if err != nil {
		t.Fatal(err)
	}
	if message := got[check{"example.com/p", "TestRan"}]; message != "" {
		t.Errorf("TestRan: %q, want a pass", message)
	}
	if message := got[check{"example.com/p", "TestExcluded"}]; message != "TestExcluded did not run: go test passed example.com/p without it" {
		t.Errorf("TestExcluded: %q, want it reported as not run", message)
	}
}

// With -failfast, go test starts no further package once one fails, and a
// package it never started has no events at all. Its selected tests did not run.
func TestPackageNeverStarted(t *testing.T) {
	selected := []selection{{"example.com/p", []string{"TestBad"}}, {"example.com/q", []string{"TestLater"}}}
	got, err := summarize(report(
		event{Action: "fail", Package: "example.com/p", Test: "TestBad"},
		event{Action: "fail", Package: "example.com/p"},
	), selected, 1)
	if err != nil {
		t.Fatal(err)
	}
	if message := got[check{"example.com/q", "TestLater"}]; message != "TestLater did not run: go test stopped before starting example.com/q, after a failure" {
		t.Errorf("TestLater: %q, want it reported as not run", message)
	}
}

func TestMalformedReportAndCommandFailure(t *testing.T) {
	selected := []selection{{"example.com/p", []string{"TestOK"}}}
	if _, err := summarize(strings.NewReader("not JSON"), selected, 1); err == nil {
		t.Fatal("accepted malformed JSON")
	}
	got, err := summarize(report(event{Action: "pass", Package: "example.com/p", Test: "TestOK"}), selected, 1)
	if err != nil || got[check{"example.com/p", "TestOK"}] != "shared test command failed (go test exit 1)" {
		t.Fatalf("lost command failure: %v %v", got, err)
	}
}
