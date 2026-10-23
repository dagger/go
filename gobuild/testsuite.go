package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"dagger/gobuild/internal/dagger"
)

type testSuite struct {
	Packages []testPackage
	Args     []string
	Parallel int
}
type testPackage struct{ Package, Binary, Dir, Error string }

// TestSuite builds selected packages with the same cached compile steps as
// TestBinary. Its directory holds binaries and an otelgotest manifest. Run it
// with the source and testdata mounted at mountPath. Build
// failures remain per-package results, so unrelated tests can still run.
// As with TestBinary, vet is left to lint checks.
func (m *Gobuild) TestSuite(
	ctx context.Context,
	base *dagger.Container,
	source *dagger.Directory,
	headers *dagger.File,
	mountPath string,
	workdir string,
	modulePath string,
	packages []string,
	// +optional
	image string,
	// A test selection, expressed as go test's -run regular expression.
	// +optional
	runPattern string,
) (*dagger.Directory, error) {
	goflags, err := base.EnvVariable(ctx, "GOFLAGS")
	if err != nil {
		return nil, err
	}
	flags, err := splitGOFLAGS(goflags)
	if err != nil {
		return nil, err
	}
	args, parallel, err := suiteFlags(flags)
	if err != nil {
		return nil, err
	}
	if runPattern != "" {
		args = append(args, "-test.run="+runPattern)
	}
	suite := testSuite{Args: args, Parallel: parallel}
	out := dag.Directory()
	packages = slices.Clone(packages)
	slices.Sort(packages)
	for _, pkg := range slices.Compact(packages) {
		rel := strings.TrimPrefix(pkg, "./")
		name := modulePath
		if rel != "." {
			name += "/" + rel
		}
		binary := "bin/" + stableName(name) + ".test"
		entry := testPackage{Package: name, Binary: binary, Dir: path.Join(mountPath, workdir, rel)}
		file, buildErr := m.TestBinary(ctx, base, image, source, headers, mountPath, workdir, pkg, nil)
		if buildErr == nil {
			file, buildErr = file.Sync(ctx)
		}
		if buildErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			entry.Error = buildErr.Error()
		} else {
			out = out.WithFile(binary, file)
		}
		suite.Packages = append(suite.Packages, entry)
	}
	manifest, err := json.Marshal(suite)
	if err != nil {
		return nil, err
	}
	return out.WithNewFile("manifest.json", string(manifest)), nil
}

// Build flags stay on the build container. Only testing flags are passed to
// binaries. GOFLAGS uses -flag=value, so quoting is handled before this step.
func suiteFlags(flags []string) ([]string, int, error) {
	var args []string
	parallel := 0
	for _, flag := range flags {
		name, value, hasValue := strings.Cut(strings.TrimLeft(flag, "-"), "=")
		name = strings.TrimPrefix(name, "test.")
		switch name {
		case "run", "skip", "count", "timeout", "parallel", "short", "shuffle", "cpu", "bench", "benchtime", "benchmem", "fullpath":
			if !hasValue {
				value = "true"
			}
			args = append(args, "-test."+name+"="+value)
		case "p":
			var err error
			parallel, err = strconv.Atoi(value)
			if err != nil || parallel < 1 {
				return nil, 0, fmt.Errorf("invalid -p value %q", value)
			}
		case "v", "json": // The runner always requests test2json framing.
		case "exec", "fuzz", "fuzztime", "fuzzminimizetime", "coverprofile", "outputdir", "cpuprofile", "memprofile", "memprofilerate", "blockprofile", "blockprofilerate", "mutexprofile", "mutexprofilefraction", "trace", "list", "failfast":
			return nil, 0, fmt.Errorf("prebuilt test batches do not support -%s; use GoModule.test for this option", name)
		}
	}
	return args, parallel, nil
}
