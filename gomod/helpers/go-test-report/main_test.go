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
		want   map[check]string // Empty means pass; otherwise the error must contain this text.
	}{
		{"named failure with child diagnostics", []event{
			{Action: "output", Package: "example.com/p", Test: "TestBad/child", Output: "child assertion"},
			{Action: "fail", Package: "example.com/p", Test: "TestBad"},
			{Action: "fail", Package: "example.com/p"},
		}, map[check]string{{"example.com/p", "TestOK"}: "", {"example.com/p", "TestBad"}: "child assertion", {"example.com/q", "TestOther"}: ""}},
		{"undiscovered failure", []event{
			{Action: "fail", Package: "example.com/p", Test: "TestHidden"},
			{Action: "fail", Package: "example.com/p"},
		}, map[check]string{{"example.com/p", "TestOK"}: "outside a selected test check", {"example.com/q", "TestOther"}: ""}},
		{"teardown", []event{
			{Action: "pass", Package: "example.com/p", Test: "TestBad"},
			{Action: "fail", Package: "example.com/p"},
		}, map[check]string{{"example.com/p", "TestOK"}: "outside a selected test check", {"example.com/p", "TestBad"}: "outside a selected test check", {"example.com/q", "TestOther"}: ""}},
		{"package without checks", []event{{Action: "fail", Package: "example.com/unchecked"}},
			map[check]string{{"example.com/p", "TestOK"}: "without selected test checks", {"example.com/q", "TestOther"}: "without selected test checks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := summarize(report(append(append([]event{}, base...), tc.events...)...), selected, 1, strings.NewReader(""))
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range tc.want {
				value, ok := got[key]
				if !ok || (want == "" && value != "") || (want != "" && !strings.Contains(value, want)) {
					t.Errorf("%v: got %q, want %q", key, value, want)
				}
			}
		})
	}
}

func TestLargeReportAndBoundedDiagnostics(t *testing.T) {
	// Over 128 MiB in total, with bounded reader and per-check memory.
	chunk, err := json.Marshal(event{Action: "output", Package: "example.com/p", Test: "TestBig/child", Output: strings.Repeat("x", 4096)})
	if err != nil {
		t.Fatal(err)
	}
	chunk = append(chunk, '\n')
	for _, action := range []string{"pass", "fail"} {
		t.Run(action, func(t *testing.T) {
			input := io.MultiReader(
				&repeatedReader{chunk: chunk, remaining: int64(len(chunk)) * (33 << 10)},
				report(
					event{Action: "output", Package: "example.com/p", Test: "TestBig/child", Output: "final assertion"},
					event{Action: action, Package: "example.com/p", Test: "TestBig"},
					event{Action: action, Package: "example.com/p"},
				),
			)
			exitCode := 0
			if action == "fail" {
				exitCode = 1
			}
			got, err := summarize(input, []selection{{"example.com/p", []string{"TestBig"}}}, exitCode, strings.NewReader(strings.Repeat("e", 4*diagnosticLimit)))
			if err != nil {
				t.Fatal(err)
			}
			message := got[check{"example.com/p", "TestBig"}]
			if action == "pass" {
				if message != "" {
					t.Fatalf("large passing report failed: %s", message)
				}
			} else if !strings.Contains(message, "final assertion") || !strings.Contains(message, "earlier output omitted") || len(message) > 3*diagnosticLimit {
				t.Fatalf("diagnostics were lost or unbounded: length %d", len(message))
			}
		})
	}
}

type repeatedReader struct {
	chunk     []byte
	offset    int
	remaining int64
}

func (r *repeatedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), len(r.chunk)-r.offset, int(r.remaining))
	copy(p, r.chunk[r.offset:r.offset+n])
	r.offset = (r.offset + n) % len(r.chunk)
	r.remaining -= int64(n)
	return n, nil
}

func TestMalformedReportAndCommandFailure(t *testing.T) {
	selected := []selection{{"example.com/p", []string{"TestOK"}}}
	if _, err := summarize(strings.NewReader("not JSON"), selected, 1, strings.NewReader("")); err == nil {
		t.Fatal("accepted malformed JSON")
	}
	got, err := summarize(report(event{Action: "pass", Package: "example.com/p", Test: "TestOK"}), selected, 1, strings.NewReader("wrapper failed"))
	if err != nil || !strings.Contains(got[check{"example.com/p", "TestOK"}], "wrapper failed") {
		t.Fatalf("lost command failure: %v %v", got, err)
	}
}
