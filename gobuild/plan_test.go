package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

var testRoots = roots{Src: "/src", Mod: "/go/pkg/mod", Cwd: "/src"}

// mainLink is a main package's compile and link, as go build -n prints them,
// with extra lines for the package's compile step.
func mainLink(compile ...string) string {
	return strings.Join(append(append([]string{
		"mkdir -p $WORK/b001/",
		"",
		"#",
		"# example.com/m",
		"#",
		"",
	}, compile...),
		"cat >$WORK/b001/importcfg.link << 'EOF' # internal",
		"packagefile example.com/m=$WORK/b001/_pkg_.a",
		"EOF",
		"mkdir -p $WORK/b001/exe/",
		"cd .",
		"/usr/local/go/pkg/tool/linux_amd64/link -o $WORK/b001/exe/a.out -importcfg $WORK/b001/importcfg.link -buildmode=exe $WORK/b001/_pkg_.a",
		"mv $WORK/b001/exe/a.out /gobuild/out/out",
	), "\n")
}

func TestShellWords(t *testing.T) {
	got := shellWords(`a 'b c' "d \"e\"" f\ g "-ldflags=\"-O2\" \"-g\""`)
	want := []string{"a", "b c", `d "e"`, "f g", `-ldflags="-O2" "-g"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shellWords: %q, want %q", got, want)
	}
}

// A cgo package's steps run as the plan has them: the C compiler's flag
// probes are dropped, its directory changes kept, and the package's
// directory, which C compiles read by bare names, mounted whole.
func TestParsePlanCgo(t *testing.T) {
	plan := mainLink(
		"cd /src/cgoapp",
		"TERM='dumb' CGO_LDFLAGS='' /usr/local/go/pkg/tool/linux_amd64/cgo -objdir $WORK/b001/ -importpath example.com/m -- -I $WORK/b001/ -O2 -g ./main.go",
		"cd $WORK",
		"gcc -fno-caret-diagnostics -O2 -g -c -x c - -o /dev/null || true",
		"cd /src/cgoapp",
		"TERM='dumb' gcc -I . -fPIC -m64 -pthread -I $WORK/b001/ -O2 -g -o $WORK/b001/_x001.o -c add.c",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -trimpath \"$WORK/b001=>\" -p main -pack $WORK/b001/_cgo_gotypes.go $WORK/b001/main.cgo1.go",
		"go tool pack r $WORK/b001/_pkg_.a $WORK/b001/_x001.o # internal",
	)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	b := blocks["b001"]
	if !b.cgo {
		t.Error("cgo step not marked")
	}
	if !slices.Contains(b.srcFiles, "cgoapp/**") || !slices.Contains(b.srcFiles, "cgoapp/main.go") {
		t.Errorf("srcFiles: %q", b.srcFiles)
	}
	if b.out != "exe/a.out" {
		t.Errorf("out: %q", b.out)
	}
	script := b.script(blocks, soloCompileC)
	if strings.Contains(script, "/dev/null || true") {
		t.Error("a flag probe was kept")
	}
	if !strings.Contains(script, "cd /src/cgoapp\nTERM='dumb' gcc") {
		t.Errorf("the change to the package's directory before the C compile was dropped:\n%s", script)
	}
	if strings.Contains(script, "$WORK") {
		t.Errorf("$WORK left in the script:\n%s", script)
	}
}

// A downloaded module's package with cgo or assembly can #include any file
// of its module, by a relative path or through -I, which the plan never
// names, so it reads the whole module; one without either reads only what
// the plan names.
func TestParsePlanModuleIncludes(t *testing.T) {
	plan := strings.Join([]string{
		"mkdir -p $WORK/b002/",
		"",
		"#",
		"# example.com/dep/asm",
		"#",
		"",
		"cd /go/pkg/mod/example.com/dep@v1.0.0/asm",
		"/usr/local/go/pkg/tool/linux_amd64/asm -p example.com/dep/asm -trimpath \"$WORK/b002=>\" -I $WORK/b002/ -I /usr/local/go/pkg/include -o $WORK/b002/value_amd64.o ./value_amd64.s",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b002/_pkg_.a -trimpath \"$WORK/b002=>\" -p example.com/dep/asm -pack ./value.go",
		"mkdir -p $WORK/b003/",
		"",
		"#",
		"# example.com/dep/cgo",
		"#",
		"",
		"cd /go/pkg/mod/example.com/dep@v1.0.0/cgo",
		"TERM='dumb' CGO_LDFLAGS='' /usr/local/go/pkg/tool/linux_amd64/cgo -objdir $WORK/b003/ -importpath example.com/dep/cgo -- -I $WORK/b003/ -O2 -g ./value.go",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b003/_pkg_.a -trimpath \"$WORK/b003=>\" -p example.com/dep/cgo -pack $WORK/b003/_cgo_gotypes.go $WORK/b003/value.cgo1.go",
		"mkdir -p $WORK/b004/",
		"",
		"#",
		"# example.com/other/pure",
		"#",
		"",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b004/_pkg_.a -trimpath \"$WORK/b004=>\" -p example.com/other/pure -pack /go/pkg/mod/example.com/other@v1.0.0/pure/pure.go",
	}, "\n") + "\n" + mainLink(
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -importcfg $WORK/b001/importcfg -pack ./main.go",
	)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b002", "b003"} {
		if b := blocks[id]; !slices.Contains(b.modFiles, "example.com/dep@v1.0.0/**") {
			t.Errorf("%s (%s) modFiles: %q", id, b.importPath, b.modFiles)
		}
	}
	if b := blocks["b004"]; !slices.Equal(b.modFiles, []string{"example.com/other@v1.0.0/pure/pure.go"}) {
		t.Errorf("b004 modFiles: %q", b.modFiles)
	}
}

// An embedded file is named in embedcfg JSON, spaces and all.
func TestParsePlanEmbedcfg(t *testing.T) {
	plan := mainLink(
		"cat >$WORK/b001/embedcfg << 'EOF' # internal",
		`{"Patterns":{"assets/*":["assets/hello world.txt"]},"Files":{"assets/hello world.txt":"/src/assets/hello world.txt"}}EOF`,
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -embedcfg $WORK/b001/embedcfg -pack ./main.go",
	)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(blocks["b001"].srcFiles, "assets/hello world.txt") {
		t.Errorf("srcFiles: %q", blocks["b001"].srcFiles)
	}
}

// A PGO profile's preprocessing has no package header, and every compile
// reads its output: it isn't a dependency, so the standard library and
// dependencies that read it stay outside the source.
func TestParsePlanProfile(t *testing.T) {
	plan := strings.Join([]string{
		"mkdir -p $WORK/b002/",
		"/usr/local/go/pkg/tool/linux_amd64/preprofile -o $WORK/b002/pgo.preprofile -i /src/default.pgo",
		"mkdir -p $WORK/b003/",
		"",
		"#",
		"# fmt",
		"#",
		"",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b003/_pkg_.a -p fmt -std -pgoprofile=$WORK/b002/pgo.preprofile -pack /usr/local/go/src/fmt/print.go",
	}, "\n") + "\n" + mainLink(
		"cat >$WORK/b001/importcfg << 'EOF' # internal",
		"packagefile fmt=$WORK/b003/_pkg_.a",
		"EOF",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pgoprofile=$WORK/b002/pgo.preprofile -importcfg $WORK/b001/importcfg -pack ./main.go",
	)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	p := blocks["b002"]
	if !p.isProfile || !slices.Contains(p.srcFiles, "default.pgo") || p.output() != "pgo.preprofile" {
		t.Errorf("profile step: %+v", p)
	}
	for _, id := range []string{"b001", "b003"} {
		b := blocks[id]
		if b.profile != "b002" || slices.Contains(b.deps, "b002") {
			t.Errorf("%s: profile %q, deps %q", id, b.profile, b.deps)
		}
	}
	if !outsideSet(blocks)["b003"] {
		t.Error("fmt left the outside set")
	}
}

// -trimpath's rewrites name the source root, but the source root isn't an
// input of the compile: mounting it would make every package read the whole
// tree.
func TestParsePlanTrimpathIsNotAnInput(t *testing.T) {
	plan := mainLink(
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -trimpath \"$WORK/b001=>;/src=>example.com/m\" -p main -pack ./main.go",
	)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks["b001"].srcFiles; !reflect.DeepEqual(got, []string{"main.go"}) {
		t.Errorf("srcFiles: %q", got)
	}
}

// A directory change names where the next commands run: a standard library
// package's step that changes to the source doesn't read it.
func TestParsePlanCdIsNotAnInput(t *testing.T) {
	plan := strings.Join([]string{
		"mkdir -p $WORK/b002/",
		"",
		"#",
		"# fmt",
		"#",
		"",
		"cd /src/sub",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b002/_pkg_.a -p fmt -std -pack /usr/local/go/src/fmt/print.go",
	}, "\n") + "\n" + mainLink("/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pack ./main.go")
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks["b002"].srcFiles; len(got) != 0 {
		t.Errorf("fmt's srcFiles: %q", got)
	}
	if !outsideSet(blocks)["b002"] {
		t.Error("fmt left the outside set")
	}
}

// A cgo step keeps the directory changes the plan prints, but not the one
// carried in from the step before, which would tie it to the project.
func TestCgoScriptDropsCarriedDirectory(t *testing.T) {
	plan := mainLink(
		"cd /usr/local/go/src/runtime/cgo",
		"TERM='dumb' /usr/local/go/pkg/tool/linux_amd64/cgo -objdir $WORK/b001/ -importpath runtime/cgo -- -I $WORK/b001/ ./cgo.go",
		"TERM='dumb' gcc -I . -o $WORK/b001/_x001.o -c gcc_linux.c",
	)
	blocks, err := parsePlan(plan, roots{Src: "/proj", Mod: "/go/pkg/mod", Cwd: "/proj/sub"})
	if err != nil {
		t.Fatal(err)
	}
	script := blocks["b001"].script(blocks, soloCompileC)
	if strings.Contains(script, "/proj/sub") {
		t.Errorf("the project's directory is in the script:\n%s", script)
	}
	if !strings.Contains(script, "cd /usr/local/go/src/runtime/cgo\n") {
		t.Errorf("the plan's own directory change was dropped:\n%s", script)
	}
}

// A quoted directory is decoded for inputs and quoted again in scripts.
func TestParsePlanQuotedDirectory(t *testing.T) {
	plan := mainLink(
		"cd '/src/my dir'",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pack ./main.go",
	)
	blocks, err := parsePlan(plan, roots{Src: "/src", Mod: "/go/pkg/mod", Cwd: "/src/my dir"})
	if err != nil {
		t.Fatal(err)
	}
	b := blocks["b001"]
	if !slices.Contains(b.srcFiles, "my dir/main.go") {
		t.Errorf("srcFiles: %q", b.srcFiles)
	}
	if script := b.script(blocks, soloCompileC); !strings.Contains(script, "cd '/src/my dir'") {
		t.Errorf("the directory isn't quoted:\n%s", script)
	}
}

func TestSplitPattern(t *testing.T) {
	for in, want := range map[string][2]string{
		"m@v1/**/*.h":  {"m@v1", "**/*.h"},
		"m@v1/inc/**":  {"m@v1/inc", "**"},
		"m@v1/x/y.h":   {"m@v1/x", "y.h"},
		"m@v1/a/*/b.h": {"m@v1/a", "*/b.h"},
	} {
		dir, glob := splitPattern(in)
		if dir != want[0] || glob != want[1] {
			t.Errorf("splitPattern(%q) = %q, %q; want %q, %q", in, dir, glob, want[0], want[1])
		}
	}
}

// As with any repeated go flag, the last -modfile or -mod counts, and the
// call's flags count over GOFLAGS.
func TestLastFlag(t *testing.T) {
	for _, c := range []struct {
		flags, goflags []string
		want           string
		set            bool
	}{
		{[]string{"-modfile=a.mod", "-modfile=b.mod"}, nil, "b.mod", true},
		{[]string{"-modfile", "a.mod"}, []string{"-modfile=g.mod"}, "a.mod", true},
		{nil, []string{"-modfile=g.mod", "-modfile=h.mod"}, "h.mod", true},
		// An empty one in the call means the module's own go.mod.
		{[]string{"-modfile="}, []string{"-modfile=g.mod"}, "", true},
		{[]string{"-trimpath"}, nil, "", false},
	} {
		if got, set := lastFlag("modfile", c.flags, c.goflags); got != c.want || set != c.set {
			t.Errorf("lastFlag(modfile, %q, %q) = %q, %v; want %q, %v", c.flags, c.goflags, got, set, c.want, c.set)
		}
	}
	// -mod is not -modfile.
	for _, c := range []struct {
		flags, goflags []string
		want           string
		set            bool
	}{
		{[]string{"-modfile=a.mod"}, []string{"-mod=vendor"}, "vendor", true},
		{[]string{"-mod", "mod"}, []string{"-mod=vendor"}, "mod", true},
		{[]string{"-mod", "vendor", "-trimpath"}, nil, "vendor", true},
		{[]string{"-modfile", "mod"}, nil, "", false},
		{nil, []string{"-modfile=g.mod"}, "", false},
	} {
		if got, set := lastFlag("mod", c.flags, c.goflags); got != c.want || set != c.set {
			t.Errorf("lastFlag(mod, %q, %q) = %q, %v; want %q, %v", c.flags, c.goflags, got, set, c.want, c.set)
		}
	}
}

// GOFLAGS splits as the go command splits it, not as a shell would.
func TestSplitGOFLAGS(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"-tags=foo\n-modfile=alt.mod", []string{"-tags=foo", "-modfile=alt.mod"}},
		{" '-ldflags=-X a=b'\t-trimpath ", []string{"-ldflags=-X a=b", "-trimpath"}},
		{`-tags=a\b -x"y"`, []string{`-tags=a\b`, `-x"y"`}},
		{`"-gcflags=all=-N -l"`, []string{"-gcflags=all=-N -l"}},
		{"", nil},
	} {
		got, err := splitGOFLAGS(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitGOFLAGS(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := splitGOFLAGS("'-tags=a"); err == nil {
		t.Error("an unterminated quote was accepted")
	}
}

// go build installs the header of a c-shared or c-archive library's
// exported functions next to it.
func TestExportsHeader(t *testing.T) {
	cshared := strings.Replace(mainLink(
		"TERM='dumb' /usr/local/go/pkg/tool/linux_amd64/cgo -objdir $WORK/b001/ -importpath example.com/m -exportheader=$WORK/b001/_cgo_install.h -- ./main.go",
	), "-buildmode=exe", "-buildmode=c-shared", 1)
	blocks, err := parsePlan(cshared, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if !blocks["b001"].exportsHeader() {
		t.Error("a c-shared build's header was not found")
	}
	exe, err := parsePlan(mainLink("/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pack ./main.go"), testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if exe["b001"].exportsHeader() {
		t.Error("an executable has a header")
	}
}

// A plugin or c-shared library links in its exe directory with a relative
// -o, whether or not the main package uses cgo.
func TestScriptKeepsLinkDirectory(t *testing.T) {
	plan := strings.NewReplacer(
		"cd .\n", "cd $WORK/b001/exe/\n",
		"-o $WORK/b001/exe/a.out ", "-o a.out.so ",
		"-buildmode=exe", "-buildmode=c-shared",
		"mv $WORK/b001/exe/a.out ", "mv $WORK/b001/exe/a.out.so ",
	).Replace(mainLink("/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pack ./main.go"))
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	b := blocks["b001"]
	if b.cgo {
		t.Fatal("no cgo step here")
	}
	script := b.script(blocks, soloCompileC)
	if !strings.Contains(script, "cd /work/"+b.name+"/exe/\n") || !strings.Contains(script, "link -o a.out.so ") {
		t.Errorf("the link's directory change was dropped:\n%s", script)
	}
}

// Go leaves -c out when it would be 1, as on a one-CPU engine: the script is
// the same either way.
func TestScriptCompileConcurrency(t *testing.T) {
	script := func(compile string) string {
		blocks, err := parsePlan(mainLink(compile), testRoots)
		if err != nil {
			t.Fatal(err)
		}
		return blocks["b001"].script(blocks, soloCompileC)
	}
	with := script("/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -c=4 -nolocalimports -pack ./main.go")
	without := script("/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -nolocalimports -pack ./main.go")
	if with != without || strings.Count(with, " -c=") != 1 || !strings.Contains(with, "compile -c="+soloCompileC+" -o ") {
		t.Errorf("with -c:\n%s\nwithout:\n%s", with, without)
	}
	wrapped := script("/opt/wrap /usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pack ./main.go")
	if !strings.Contains(wrapped, "/opt/wrap /usr/local/go/pkg/tool/linux_amd64/compile -c="+soloCompileC+" -o ") {
		t.Errorf("a -toolexec compile:\n%s", wrapped)
	}
}

// The link's build ID is made in its exec from what it reads; a -buildid
// in the call's -ldflags still comes last.
func TestScriptLinkBuildID(t *testing.T) {
	plan := strings.Replace(mainLink("/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -buildid PlanC/PlanC -pack ./main.go"),
		"-buildmode=exe ", "-buildmode=exe -buildid=PlanX/PlanY/PlanY/PlanX -X main.v=1 ", 1)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	b := blocks["b001"]
	script := b.script(blocks, soloCompileC)
	if !strings.Contains(script, "id=$(/gobuild/gohdr linkid /work/"+b.name+"/importcfg.link ") ||
		!strings.Contains(script, " -buildid=$id -X main.v=1 ") || strings.Contains(script, "Plan") {
		t.Errorf("link build ID:\n%s", script)
	}
	user, err := parsePlan(strings.Replace(plan, "-X main.v=1", "-buildid=", 1), testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if s := user["b001"].script(user, soloCompileC); !strings.Contains(s, " -buildid=$id -buildid= ") {
		t.Errorf("the call's own -buildid was dropped:\n%s", s)
	}
}

// The C compiler flags the go command adds after probing the compiler come
// back, in the go command's places.
func TestAddCFlags(t *testing.T) {
	probes := []string{
		"probe\tgcc\t-fno-caret-diagnostics\tno",
		"probe\tgcc\t-Qunused-arguments\tno",
		"probe\tgcc\t-Wl,--no-gc-sections\tyes",
		"probe\tgcc\t-fdebug-prefix-map=a=b\tyes",
		"probe\tgcc\t-ffile-prefix-map=a=b\tyes",
		"probe\tgcc\t-gno-record-gcc-switches\tyes",
		"probe\tgcc\t-frandom-seed=1\tyes",
	}
	cc := parseCC(strings.Join(slices.Concat(probes, []string{"modroot\t/src/cgoapp\t/src", "goos\tlinux"}), "\n"))
	windows := parseCC(strings.Join(slices.Concat(probes, []string{"goos\twindows"}), "\n"))
	block := func(cc ccInfo, dir, importPath, compileFlags string) *block {
		blocks, err := parsePlan(strings.Replace(mainLink(
			"cd "+dir,
			"TERM='dumb' /usr/local/go/pkg/tool/linux_amd64/cgo -objdir $WORK/b001/ -importpath "+importPath+" -- -I $WORK/b001/ -O2 -g ./x.go",
			"TERM='dumb' gcc -I . -fPIC -m64 -pthread -fmessage-length=0 -I $WORK/b001/ -O2 -g -o $WORK/b001/_x001.o -c x.c",
			"TERM='dumb' gcc -I . -fPIC -m64 -pthread -fmessage-length=0 -o $WORK/b001/_cgo_.o $WORK/b001/_x001.o -O2 -g",
			"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a "+compileFlags+" -p "+importPath+" -buildid Seed/Seed -pack $WORK/b001/x.cgo1.go",
		), "# example.com/m\n", "# "+importPath+"\n", 1), testRoots)
		if err != nil {
			t.Fatal(err)
		}
		b := blocks["b001"]
		b.addCFlags(cc)
		return b
	}
	const compiler = "TERM='dumb' gcc -I . -fPIC -m64 -pthread -Wl,--no-gc-sections -fmessage-length=0 -ffile-prefix-map=$WORK/b001=/tmp/go-build -gno-record-gcc-switches"
	for _, c := range []struct {
		name                            string
		cc                              ccInfo
		dir, importPath, flags, mapping string
	}{
		{"std", cc, "/usr/local/go/src/runtime/cgo", "runtime/cgo", `-trimpath "$WORK/b001=>" -std`, " -ffile-prefix-map=/usr/local/go=/_/GOROOT"},
		{"windows std", windows, "/usr/local/go/src/runtime/cgo", "runtime/cgo", `-trimpath "$WORK/b001=>" -std`, ` '-ffile-prefix-map=/usr/local/go=\\_\_/GOROOT'`},
		// -modfile=alt.mod naming another module path: the import path has it.
		{"alternate module file", cc, "/src/cgoapp", "example.com/alt/cgoapp", `-trimpath "/src/cgoapp=>example.com/alt/cgoapp;$WORK/b001=>"`, " -ffile-prefix-map=/src=/_/example.com/alt"},
		{"no -trimpath", cc, "/src/cgoapp", "example.com/m/cgoapp", `-trimpath "$WORK/b001=>"`, ""},
		{"main module", cc, "/src/cgoapp", "example.com/m/cgoapp", `-trimpath "/src/cgoapp=>example.com/m/cgoapp;$WORK/b001=>"`, " -ffile-prefix-map=/src=/_/example.com/m"},
		{"module cache", cc, "/go/pkg/mod/example.com/x@v1.2.0/sub", "example.com/x/sub", `-trimpath "/go/pkg/mod/example.com/x@v1.2.0/sub=>example.com/x@v1.2.0/sub;$WORK/b001=>"`, " -ffile-prefix-map=/go/pkg/mod/example.com/x@v1.2.0=/_/example.com/x@v1.2.0"},
		{"vendored", cc, "/src/vendor/example.com/x/sub", "example.com/x/sub", `-trimpath "/src/vendor/example.com/x/sub=>example.com/x@v1.2.0/sub;$WORK/b001=>"`, " -ffile-prefix-map=/src/vendor=/_/vendor"},
	} {
		b := block(c.cc, c.dir, c.importPath, c.flags)
		want := compiler + " -I $WORK/b001/ -O2 -g" + c.mapping + " -frandom-seed=Seed -o $WORK/b001/_x001.o -c x.c"
		if !slices.Contains(b.lines, want) {
			t.Errorf("%s: no C compile\n%s\nin\n%s", c.name, want, strings.Join(b.lines, "\n"))
		}
		if link := compiler + " -o $WORK/b001/_cgo_.o $WORK/b001/_x001.o -O2 -g"; !slices.Contains(b.lines, link) {
			t.Errorf("%s: no C link\n%s\nin\n%s", c.name, link, strings.Join(b.lines, "\n"))
		}
	}
}

// A position in a header is moved to the source's line and column: the
// lines up to the end of the imports are the source's own, and each go:embed
// line after them is the next one in the source, trimmed.
func TestSourcePosition(t *testing.T) {
	src := []byte("package x\n\nimport \"embed\"\n\n// Files.\n//go:embed a.txt\nvar a string\n\n\t//go:embed b\nvar b embed.FS\n")
	for _, c := range []struct{ line, col, wantLine, wantCol int }{
		{3, 8, 3, 8},
		{4, 12, 6, 12},
		{5, 12, 9, 13},
	} {
		line, col, ok := sourcePosition(src, c.line, c.col)
		if !ok || line != c.wantLine || col != c.wantCol {
			t.Errorf("sourcePosition(%d:%d) = %d:%d, %v; want %d:%d", c.line, c.col, line, col, ok, c.wantLine, c.wantCol)
		}
	}
	if _, _, ok := sourcePosition(src, 6, 1); ok {
		t.Errorf("a line past the header's was placed")
	}
}

// A go test -c plan: the test main's work directory is made first, its
// header comes after the packages it imports, its compile reads the
// _testmain.go the go command writes, and its link's output stays in its
// work directory, with no move.
func TestParsePlanTest(t *testing.T) {
	plan := strings.Join([]string{
		"mkdir -p $WORK/b001/",
		"mkdir -p $WORK/b002/",
		"",
		"#",
		"# example.com/x",
		"#",
		"",
		"cat >$WORK/b002/importcfg << 'EOF' # internal",
		"# import config",
		"EOF",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b002/_pkg_.a -trimpath \"$WORK/b002=>\" -p example.com/x -importcfg $WORK/b002/importcfg -pack ./x.go ./x_test.go",
		"",
		"#",
		"# example.com/x.test",
		"#",
		"",
		"cat >$WORK/b001/importcfg << 'EOF' # internal",
		"# import config",
		"packagefile example.com/x=$WORK/b002/_pkg_.a",
		"EOF",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -trimpath \"$WORK/b001=>\" -p main -importcfg $WORK/b001/importcfg -pack $WORK/b001/_testmain.go",
		"cat >$WORK/b001/importcfg.link << 'EOF' # internal",
		"packagefile example.com/x.test=$WORK/b001/_pkg_.a",
		"packagefile example.com/x=$WORK/b002/_pkg_.a",
		"EOF",
		"cd .",
		"/usr/local/go/pkg/tool/linux_amd64/link -o $WORK/b001/x.test -importcfg $WORK/b001/importcfg.link -buildmode=exe -buildid=a/b/c/d $WORK/b001/_pkg_.a",
	}, "\n")
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	main, x := blocks[mainID], blocks["b002"]
	if main.importPath != "example.com/x.test" || x.importPath != "example.com/x" {
		t.Fatalf("headers: %q, %q", main.importPath, x.importPath)
	}
	if !main.isLink || main.out != "x.test" {
		t.Errorf("test link: isLink %v, out %q", main.isLink, main.out)
	}
	if !reflect.DeepEqual(main.deps, []string{"b002"}) || len(x.deps) != 0 {
		t.Errorf("deps: main %v, x %v", main.deps, x.deps)
	}
	for _, l := range x.lines {
		if strings.Contains(l, "$WORK/b001") {
			t.Errorf("a test main line landed in the package's block: %q", l)
		}
	}
	if !reflect.DeepEqual(x.srcFiles, []string{"x.go", "x_test.go"}) {
		t.Errorf("package inputs: %v", x.srcFiles)
	}
}

// With -cover, a test main's cover run comes after the other packages' steps
// and before its header, and with -coverpkg its block is not b001: its steps
// are its own wherever they come, and the block that links is the main one.
func TestParsePlanTestCover(t *testing.T) {
	plan := strings.Join([]string{
		"mkdir -p $WORK/b002/",
		"mkdir -p $WORK/b003/",
		"cd $WORK/b003/",
		"/usr/local/go/pkg/tool/linux_amd64/cover -pkgcfg ./pkgcfg.txt -mode set -var goCover_x -outfilelist ./coveroutfiles.txt /src/x.go",
		"",
		"#",
		"# example.com/x",
		"#",
		"",
		"cat >$WORK/b003/importcfg << 'EOF' # internal",
		"# import config",
		"EOF",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b003/_pkg_.a -trimpath \"$WORK/b003=>\" -p example.com/x -importcfg $WORK/b003/importcfg -pack $WORK/b003/x.cover.go ./x_test.go",
		"echo '{\"OutConfig\":\"$WORK/b002/coveragecfg\"}' > $WORK/b002/pkgcfg.txt # internal",
		"cd $WORK/b002/",
		"/usr/local/go/pkg/tool/linux_amd64/cover -pkgcfg ./pkgcfg.txt -mode testmain -var goCover_m -outfilelist ./coveroutfiles.txt ./_testmain.go",
		"",
		"#",
		"# example.com/x.test",
		"#",
		"",
		"cat >$WORK/b002/importcfg << 'EOF' # internal",
		"# import config",
		"packagefile example.com/x=$WORK/b003/_pkg_.a",
		"EOF",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b002/_pkg_.a -trimpath \"$WORK/b002=>\" -p main -importcfg $WORK/b002/importcfg -pack $WORK/b002/_testmain.cover.go",
		"cat >$WORK/b002/importcfg.link << 'EOF' # internal",
		"packagefile example.com/x.test=$WORK/b002/_pkg_.a",
		"packagefile example.com/x=$WORK/b003/_pkg_.a",
		"EOF",
		"cd .",
		"/usr/local/go/pkg/tool/linux_amd64/link -o $WORK/b002/x.test -importcfg $WORK/b002/importcfg.link -buildmode=exe -buildid=a/b/c/d $WORK/b002/_pkg_.a",
	}, "\n")
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	main, x := blocks["b002"], blocks["b003"]
	if linkID(blocks) != "b002" || !main.isLink || main.out != "x.test" {
		t.Fatalf("link: %s, isLink %v, out %q", linkID(blocks), main.isLink, main.out)
	}
	if main.importPath != "example.com/x.test" || x.importPath != "example.com/x" {
		t.Fatalf("headers: %q, %q", main.importPath, x.importPath)
	}
	if !reflect.DeepEqual(main.deps, []string{"b003"}) || len(x.deps) != 0 {
		t.Errorf("deps: main %v, x %v", main.deps, x.deps)
	}
	for _, l := range x.lines {
		if strings.Contains(l, "$WORK/b002") {
			t.Errorf("a test main line landed in the package's block: %q", l)
		}
	}
	// The cover run gets _testmain.go's whole path, as from the go command,
	// which wrote "./" for the directory it runs in.
	if !slices.ContainsFunc(main.lines, func(l string) bool {
		return strings.Contains(l, "/cover ") && strings.HasSuffix(l, " $WORK/b002/_testmain.go") && strings.Contains(l, " -pkgcfg $WORK/b002/pkgcfg.txt ")
	}) {
		t.Errorf("the test main's cover run kept its ./ paths: %q", main.lines)
	}
}

// A heredoc line that names the linker, such as an embedded file's in an
// embedcfg, is not a link: the main package is the one that runs the linker.
func TestParsePlanLinkIsTheLinker(t *testing.T) {
	plan := strings.Join([]string{
		"mkdir -p $WORK/b002/",
		"",
		"#",
		"# example.com/m/assets",
		"#",
		"",
		"cat >$WORK/b002/embedcfg << 'EOF' # internal",
		"{",
		"\t\"Patterns\": {",
		"\t\t\"pkg/tool/linux_amd64/link data.txt\": [",
		"\t\t\t\"pkg/tool/linux_amd64/link data.txt\"",
		"\t\t]",
		"\t},",
		"\t\"Files\": {",
		"\t\t\"pkg/tool/linux_amd64/link data.txt\": \"/src/assets/pkg/tool/linux_amd64/link data.txt\"",
		"\t}",
		"}EOF",
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b002/_pkg_.a -trimpath \"$WORK/b002=>\" -p example.com/m/assets -embedcfg $WORK/b002/embedcfg -pack ./assets/assets.go",
		mainLink("/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pack ./main.go"),
	}, "\n")
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if linkID(blocks) != mainID || blocks["b002"].isLink {
		t.Errorf("link: %s, assets isLink %v", linkID(blocks), blocks["b002"].isLink)
	}
}

// A work file go hands the cgo tool from a source directory, printed as
// "./$WORK/...", is the work file: an input of no source file, run at the
// work path.
func TestParsePlanCgoCoverWorkFile(t *testing.T) {
	plan := mainLink(
		"cd /src",
		"TERM='dumb' CGO_LDFLAGS='' /usr/local/go/pkg/tool/linux_amd64/cgo -objdir $WORK/b001/ -importpath example.com/m -- -I $WORK/b001/ -O2 -g ./$WORK/b001/m.cover.go",
	)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	b := blocks[mainID]
	if !slices.ContainsFunc(b.lines, func(l string) bool { return strings.HasSuffix(l, " -g $WORK/b001/m.cover.go") }) {
		t.Errorf("the cgo run kept ./$WORK: %q", b.lines)
	}
	for _, f := range b.srcFiles {
		if strings.Contains(f, "WORK") || strings.Contains(f, "cover.go") {
			t.Errorf("a work file is a source input: %q", b.srcFiles)
		}
	}
}

// A profile outside the source, such as one a -pgo flag names in the base
// image, is preprocessed on its own like any other, not as a package.
func TestProfileOutsideTheSource(t *testing.T) {
	plan := strings.Join([]string{
		"mkdir -p $WORK/b002/",
		"/usr/local/go/pkg/tool/linux_amd64/preprofile -o $WORK/b002/pgo.preprofile -i /profile.pprof",
	}, "\n") + "\n" + mainLink(
		"/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b001/_pkg_.a -p main -pgoprofile=$WORK/b002/pgo.preprofile -pack ./main.go",
	)
	blocks, err := parsePlan(plan, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	if p := blocks["b002"]; !p.isProfile || len(p.srcFiles) != 0 {
		t.Fatalf("profile step: %+v", p)
	}
	if outsideSet(blocks)["b002"] {
		t.Error("the profile step is in the outside set, which builds packages")
	}
}
