// go-test-report streams a go test -json report once and writes a bounded
// failure message (or an empty passing result) for each selected test check.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const diagnosticLimit = 32 << 10

type selection struct {
	Pkg   string   `json:"pkg"`
	Names []string `json:"names"`
}

type event struct {
	Action, Package, ImportPath, Test, Output string
}

type check struct{ pkg, name string }

// tail keeps diagnostic memory bounded even when a passing test logs gigabytes.
type tail struct {
	data      []byte
	truncated bool
}

func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= diagnosticLimit {
		t.truncated = t.truncated || len(t.data) > 0 || n > diagnosticLimit
		t.data = append(t.data[:0], p[n-diagnosticLimit:]...)
	} else {
		if extra := len(t.data) + n - diagnosticLimit; extra > 0 {
			t.data = append(t.data[:0], t.data[extra:]...)
			t.truncated = true
		}
		t.data = append(t.data, p...)
	}
	return n, nil
}

func (t *tail) add(s string) { _, _ = t.Write([]byte(s)) }

func (t *tail) String() string {
	if t.truncated {
		return "[earlier output omitted; showing diagnostic tail]\n" + string(t.data)
	}
	return string(t.data)
}

type testResult struct {
	observed, failed, finished bool
	log                        tail
}

type packageResult struct {
	started, passed, failed, namedFailure, uncheckedTestFailure bool
	tests                                                       map[string]*testResult
	log                                                         tail
}

func summarize(report io.Reader, selections []selection, exitCode int, stderr io.Reader) (map[check]string, error) {
	packages := make(map[string]*packageResult)
	for _, s := range selections {
		if s.Pkg == "" {
			return nil, fmt.Errorf("invalid selected package %q", s.Pkg)
		}
		if _, exists := packages[s.Pkg]; exists {
			return nil, fmt.Errorf("duplicate selected package %q", s.Pkg)
		}
		p := &packageResult{tests: make(map[string]*testResult)}
		for _, name := range s.Names {
			if name == "" || strings.Contains(name, "/") {
				return nil, fmt.Errorf("invalid selected test %q", name)
			}
			if _, exists := p.tests[name]; exists {
				return nil, fmt.Errorf("duplicate selected test %q in %s", name, s.Pkg)
			}
			p.tests[name] = &testResult{}
		}
		packages[s.Pkg] = p
	}
	anyFailure := false
	decoder := json.NewDecoder(report)
	for {
		var e event
		if err := decoder.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode test report: %w", err)
		}
		pkg := e.Package
		if pkg == "" {
			pkg = e.ImportPath
		}
		p := packages[pkg]
		if p == nil {
			p = &packageResult{}
			packages[pkg] = p
		}
		p.started = true
		p.log.add(e.Output)
		if e.Action == "fail" {
			anyFailure = true
		}
		if e.Test == "" {
			p.failed = p.failed || e.Action == "fail"
			p.passed = p.passed || e.Action == "pass" || e.Action == "skip"
			continue
		}
		top, _, _ := strings.Cut(e.Test, "/")
		t := p.tests[top]
		if e.Action == "fail" {
			p.namedFailure = true
			p.uncheckedTestFailure = p.uncheckedTestFailure || t == nil
		}
		if t != nil {
			t.observed = true
			t.log.add(e.Output)
			if e.Test == top {
				t.failed = t.failed || e.Action == "fail"
				t.finished = t.finished || e.Action == "pass" || e.Action == "skip"
			}
		}
	}
	var stderrTail tail
	if _, err := io.Copy(&stderrTail, stderr); err != nil {
		return nil, fmt.Errorf("read test stderr: %w", err)
	}
	globalFailure := ""
	if exitCode != 0 && !anyFailure {
		globalFailure = fmt.Sprintf("shared test command failed (go test exit %d)\n%s", exitCode, stderrTail.String())
	}
	results := make(map[check]string)
	for _, s := range selections {
		p := packages[s.Pkg]
		for _, name := range s.Names {
			t := p.tests[name]
			failure := globalFailure
			if failure == "" {
				switch {
				case t.failed:
					failure = name + " failed\n" + t.log.String()
				case p.uncheckedTestFailure || (p.failed && !p.namedFailure):
					failure = fmt.Sprintf("package failed outside a selected test check (go test exit %d)\n%s%s", exitCode, p.log.String(), stderrTail.String())
				case t.finished:
				case !t.observed && p.passed:
					// go test left the test out: a build constraint excluded the file
					// declaring it, or TestMain never called m.Run.
					failure = fmt.Sprintf("%s did not run: go test passed %s without it\n%s", name, s.Pkg, p.log.String())
				case !t.observed && p.failed && p.namedFailure:
					// -failfast stopped the package at a represented failure before
					// this test started, or the test binary left it out.
					failure = fmt.Sprintf("%s did not run: go test failed %s without running it\n%s", name, s.Pkg, p.log.String())
				case !p.started && anyFailure:
					// With -failfast, go test starts no further package once one
					// fails, and a package it never starts has no events at all.
					failure = fmt.Sprintf("%s did not run: go test stopped before starting %s, after a failure\n", name, s.Pkg)
				default:
					failure = fmt.Sprintf("no successful result for %s (go test exit %d)\n%s%s", name, exitCode, p.log.String(), stderrTail.String())
				}
			}
			// Headers and combined package/stderr logs are bounded too, so every
			// result stays comfortably below File.contents' limit.
			if len(failure) > 3*diagnosticLimit {
				failure = failure[:3*diagnosticLimit] + "\n[remaining diagnostics omitted]\n"
			}
			results[check{s.Pkg, name}] = failure
		}
	}
	return results, nil
}

func run() error {
	reportPath := flag.String("report", "", "go test JSON report")
	selectionPath := flag.String("selection", "", "selected package/test names")
	stderrPath := flag.String("stderr", "", "go test stderr")
	exitCodePath := flag.String("exit-code-file", "", "file holding go test's exit code")
	output := flag.String("output", "", "result directory")
	flag.Parse()
	data, err := os.ReadFile(*selectionPath)
	if err != nil {
		return err
	}
	var selections []selection
	if err := json.Unmarshal(data, &selections); err != nil {
		return err
	}
	report, err := os.Open(*reportPath)
	if err != nil {
		return err
	}
	defer report.Close()
	stderr, err := os.Open(*stderrPath)
	if err != nil {
		return err
	}
	defer stderr.Close()
	code, err := os.ReadFile(*exitCodePath)
	if err != nil {
		return err
	}
	exitCode, err := strconv.Atoi(strings.TrimSpace(string(code)))
	if err != nil {
		return fmt.Errorf("read exit code: %w", err)
	}
	results, err := summarize(report, selections, exitCode, stderr)
	if err != nil {
		return err
	}
	// Match the byte-order ranks used by GoTestResults.assertPassed. Identifiers
	// never enter filesystem paths, including long names and Unicode names.
	sort.Slice(selections, func(i, j int) bool { return selections[i].Pkg < selections[j].Pkg })
	for packageIndex, s := range selections {
		sort.Strings(s.Names)
		for testIndex, name := range s.Names {
			file := filepath.Join(*output, fmt.Sprint(packageIndex), fmt.Sprint(testIndex))
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(file, []byte(results[check{s.Pkg, name}]), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
