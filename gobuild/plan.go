package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// roots are where a plan's files live: the workspace source at its mount
// path, module downloads at GOMODCACHE, and the build's working directory.
type roots struct {
	Src string `json:"src"`
	Mod string `json:"mod"`
	Cwd string `json:"cwd"`
}

// block is one package's commands from a go build -n plan.
type block struct {
	id         string // bNNN in the plan
	importPath string
	name       string // stable directory name under /work
	lines      []string
	deps       []string // other block ids
	srcFiles   []string // relative to roots.Src; may hold patterns ("<dir>/**")
	modFiles   []string // relative to roots.Mod; may hold "<dir>/**" patterns
	// srcNonGo marks a package in the source with C or assembly, which
	// also reads every file in the source that isn't Go.
	srcNonGo bool
	// isLink marks the main package's block (b001), which compiles and links.
	isLink bool
	// linkAt is 1 plus the index in lines of the command that runs the
	// linker, or 0: heredoc data that names the linker is no command.
	linkAt int
	// out is the main package's output under its work directory, from the
	// plan's last move of it, e.g. exe/a.out or exe/a.out.so.
	out string
	// cgo marks a package that runs the cgo tool and a C compiler; dir is
	// the package's directory, where the cgo tool runs.
	cgo bool
	dir string
	// isProfile marks the step that preprocesses a PGO profile; profile, on
	// a package's block, names that step when the package's compile reads it.
	isProfile bool
	profile   string
}

var (
	reMkdir       = regexp.MustCompile(`^mkdir -p \$WORK/(b\d+)/`)
	reWorkRef     = regexp.MustCompile(`\$WORK/(b\d+)`)
	reRelFile     = regexp.MustCompile(`(^|\s)\./([^\s"';]+)`)
	reDotSlash    = regexp.MustCompile(`(^|\s)\./`)
	reBuildID     = regexp.MustCompile(`\s-buildid[ =]\S+`)
	reBuildIDStep = regexp.MustCompile(`^(go tool buildid|\S+/buildid) -w `)
	reConc        = regexp.MustCompile(`\s-c=\d+`)
	// The Go compiler, run directly or through a -toolexec program.
	reCompile = regexp.MustCompile(`(/pkg/tool/[^/\s]+/compile) `)
	// The Go linker, run directly or through a -toolexec program. Only a
	// command runs it: parsePlan keeps heredoc data, which may name it, out.
	reLink       = regexp.MustCompile(`/pkg/tool/[^/\s]+/link `)
	reDepArchive = regexp.MustCompile(`\$WORK/(b\d+)/_pkg_\.a`)
	// cat >TARGET << 'EOF' [# internal]
	reHeredoc = regexp.MustCompile(`^cat >(\S+) << 'EOF'( # internal)?$`)
	// mkdir -p DIR [&& REST]
	reMkdirLine = regexp.MustCompile(`^mkdir -p (\S+)(?: && (.*))?$`)
	// The link output's move to the go build -o path: exe/a.out, or
	// exe/a.out.so for a plugin, exe/a.out.exe for Windows...
	reLinkOut = regexp.MustCompile(`^mv \$WORK/(b\d+)/(exe/\S+) `)
	// A test link's output, which no move follows: $WORK/bNNN/<pkg>.test.
	reTestLinkOut = regexp.MustCompile(`/link -o \$WORK/(b\d+)/([^/\s]+) `)
	// A link's import config, the first of its steps.
	reLinkCfg = regexp.MustCompile(`^cat >\$WORK/(b\d+)/importcfg\.link << 'EOF'`)
	// The go command's build ID for a link, before any of the call's own
	// -ldflags, and the link's import config.
	reLinkBuildID = regexp.MustCompile(` -buildid=\S+`)
	reImportcfg   = regexp.MustCompile(` -importcfg (\S+)`)
	// A C compile's output and source, which end its command.
	reCOut = regexp.MustCompile(` -o \S+ -c \S+$`)
	reEnv  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// planOut is where the plan's go build writes the binary. The plan's own
// mkdir and mv for it are dropped: the binary is read from the link's exec.
const planOut = "/gobuild/out"

// mainID is the block id of a binary's main package, compiled and linked in
// one exec. A test binary's main package is the one that links, b001 or,
// with -coverpkg, another.
const mainID = "b001"

// linkID is the id of the block that links: the main package's.
func linkID(blocks map[string]*block) string {
	for id, b := range blocks {
		if b.isLink {
			return id
		}
	}
	return mainID
}

func stableName(importPath string) string {
	sum := sha256.Sum256([]byte(importPath))
	return "p" + hex.EncodeToString(sum[:6])
}

// parsePlan splits a go build -n plan into per-package blocks and records
// each block's input files and dependencies.
func parsePlan(plan string, r roots) (map[string]*block, error) {
	return parseBlocks(plan, r, false)
}

// parseArchivePlan is parsePlan for a plan of many packages whose archives
// are what's wanted: a main package's block is its compile, without the
// link that follows it, and no block links.
func parseArchivePlan(plan string, r roots) (map[string]*block, error) {
	return parseBlocks(plan, r, true)
}

func parseBlocks(plan string, r roots, archives bool) (map[string]*block, error) {
	blocks := map[string]*block{}
	var cur *block
	cwd := r.Cwd
	inEmbedcfg := false
	inHeredoc := false
	var embedcfg strings.Builder
	// A package's header ("#", "# importpath", "#") comes before the mkdir
	// of its work directory in some Go releases (1.24) and after it in
	// others, so a header waits for a block without one.
	pending := ""
	// The block whose link is being left out, from its import config to the
	// next block.
	linking := ""
	lines := strings.Split(plan, "\n")
	for i := 0; i < len(lines); i++ {
		// go -n prints its work directory as "$WORK", which is not an
		// absolute path, so a work file it hands the cgo tool from a source
		// directory, such as the cover tool's output, comes out as
		// "./$WORK/...". It runs it as the absolute path it is.
		line := strings.ReplaceAll(lines[i], "./$WORK/", "$WORK/")
		if line == "#" && i+2 < len(lines) && strings.HasPrefix(lines[i+1], "# ") && lines[i+2] == "#" {
			if h := strings.TrimPrefix(lines[i+1], "# "); cur != nil && cur.importPath == "" {
				cur.importPath = h
			} else {
				pending = h
			}
			// Not kept: it names the package, which script writes itself, and
			// kept in place it would land in the previous block's script.
			i += 2
			continue
		}
		// A heredoc's lines are data, not commands, up to its EOF, which
		// may end its last line.
		content := inHeredoc
		if inHeredoc {
			inHeredoc = !strings.HasSuffix(line, "EOF")
		} else if reHeredoc.MatchString(line) {
			inHeredoc = true
		}
		if linking != "" {
			if m := reMkdir.FindStringSubmatch(line); m == nil || m[1] == linking {
				continue
			}
			linking = ""
		}
		if m := reLinkCfg.FindStringSubmatch(line); archives && m != nil && !content {
			linking = m[1]
			continue
		}
		// A test binary's main package has its work directory made before
		// any other's, and its steps (with -cover, a cover run before its
		// header) come long after: a line working in a block that has no
		// header yet is that block's, and a header waiting names it.
		if m := reWorkRef.FindStringSubmatch(line); m != nil && !content {
			if b, ok := blocks[m[1]]; ok && b.importPath == "" && b != cur {
				cur = b
				if pending != "" {
					b.importPath, pending = pending, ""
				}
			}
		}
		if m := reMkdir.FindStringSubmatch(line); m != nil {
			b, ok := blocks[m[1]]
			if !ok {
				b = &block{id: m[1], importPath: pending}
				blocks[m[1]] = b
				pending = ""
			}
			cur = b
			// The go command prints "cd" only when the directory changes, so
			// carry the current directory into each block explicitly.
			cur.lines = append(cur.lines, "mkdir -p "+shQuote(cwd)+" && cd "+shQuote(cwd))
		}
		if cur == nil {
			continue
		}
		if m := reLinkOut.FindStringSubmatch(line); m != nil {
			if b, ok := blocks[m[1]]; ok {
				b.out = m[2]
			}
		}
		switch {
		// The go command's build ID steps, printed as "go tool buildid" or
		// by the tool's path: nothing here carries an ID to fill in.
		case reBuildIDStep.MatchString(line),
			strings.HasPrefix(line, "mkdir -p "+planOut),
			strings.HasPrefix(line, "mv $WORK/"),
			strings.HasPrefix(line, "rm -rf $WORK/"),
			// A C compiler's flag probes: go build runs each once and keeps
			// the answer, so replaying them per C file only costs time.
			strings.HasSuffix(line, " || true") && strings.Contains(line, " -o /dev/null"):
			continue
		}
		if strings.HasPrefix(line, "cd ") {
			if w := shellWords(line); len(w) == 2 {
				cwd = w[1]
				if cwd == "." {
					cwd = r.Cwd
				}
			}
		} else if strings.HasPrefix(cwd, "$WORK/") && strings.Contains(line, "/cover ") {
			// The go command prints a path under the directory a command
			// runs in as "./", and runs it whole. The cover tool writes
			// the path it is given, a test main's _testmain.go, into the
			// line directives of what it generates, which -trimpath then
			// maps: it gets the whole path, as from the go command.
			line = reDotSlash.ReplaceAllString(line, "${1}"+strings.ReplaceAll(strings.TrimSuffix(cwd, "/"), "$", "$$")+"/")
		}
		// An embedcfg heredoc lists the files a package embeds, whatever
		// their extension, as JSON that may span lines; the go command
		// writes it without a trailing newline, so it can end in "}EOF".
		if strings.Contains(line, "/embedcfg << 'EOF'") {
			inEmbedcfg = true
			embedcfg.Reset()
			cur.lines = append(cur.lines, line)
			continue
		}
		if inEmbedcfg {
			if line == "EOF" || strings.HasSuffix(line, "}EOF") {
				inEmbedcfg = false
				embedcfg.WriteString(strings.TrimSuffix(line, "EOF"))
				if err := addEmbedded(cur, r, embedcfg.String()); err != nil {
					return nil, fmt.Errorf("%s: embedcfg: %w", cur.id, err)
				}
				if line != "EOF" {
					cur.lines = append(cur.lines, strings.TrimSuffix(line, "EOF"))
				}
				cur.lines = append(cur.lines, "EOF")
				continue
			}
			embedcfg.WriteString(line + "\n")
			cur.lines = append(cur.lines, line)
			continue
		}
		// The cgo tool runs in the package's directory, which its C
		// compiles read headers and sources from by bare names.
		if strings.Contains(line, "/cgo ") && strings.Contains(line, " -objdir ") {
			cur.cgo, cur.dir = true, cwd
			addDir(cur, r, cwd)
		}
		if strings.Contains(line, "/preprofile ") {
			cur.isProfile = true
		}
		cur.lines = append(cur.lines, line)
		if !content && reLink.MatchString(line) {
			cur.linkAt = len(cur.lines)
		}
		// A directory change names where the next commands run, not an
		// input.
		if !strings.HasPrefix(line, "cd ") {
			addInputs(cur, r, cwd, line)
		}
	}
	for id, b := range blocks {
		switch {
		case b.isProfile:
			// A PGO profile's preprocessing is the one step without a
			// package header.
			b.importPath = "(pgo profile)"
		case b.importPath == "":
			return nil, fmt.Errorf("plan step %s has no package header", id)
		}
	}
	if !archives {
		if err := markLink(blocks); err != nil {
			return nil, err
		}
	}
	for id, b := range blocks {
		// Assembly and cgo's C compiles can #include files from anywhere in
		// their module, which the plan never names: a package in the module
		// cache that has either reads its whole module.
		withC := b.cgo
		for _, f := range b.modFiles {
			withC = withC || path.Ext(f) == ".s"
		}
		if withC {
			for _, f := range b.modFiles {
				if root := moduleRoot(f); root != "" {
					b.modFiles = appendUniq(b.modFiles, root+"/**")
				}
			}
		}
		// In the source, they include files by paths relative to their
		// directory ("../native/defs.inc"), of any name, including ones
		// the caller adds with includeExtraFiles: a package there that has
		// either reads every file in the source but the Go ones, which
		// leaves its key alone when Go code elsewhere changes.
		for _, f := range b.srcFiles {
			b.srcNonGo = b.srcNonGo || b.cgo || path.Ext(f) == ".s"
		}
		b.name = stableName(b.importPath)
		seen := map[string]bool{}
		for _, l := range b.lines {
			for _, m := range reWorkRef.FindAllStringSubmatch(l, -1) {
				if m[1] == id || seen[m[1]] {
					continue
				}
				seen[m[1]] = true
				// The profile isn't a dependency: every compile reads it,
				// and its output is mounted into every exec instead, so a
				// package's dependencies are the packages it imports.
				if blocks[m[1]].isProfile {
					b.profile = m[1]
					continue
				}
				b.deps = append(b.deps, m[1])
			}
		}
		sort.Strings(b.deps)
		sort.Strings(b.srcFiles)
		sort.Strings(b.modFiles)
	}
	return blocks, nil
}

// moduleRoot is the module directory a path in the module cache is in,
// "<module>@<version>", or "" outside one.
func moduleRoot(f string) string {
	i := strings.Index(f, "@")
	if i < 0 {
		return ""
	}
	j := strings.Index(f[i:], "/")
	if j < 0 {
		return ""
	}
	return f[:i+j]
}

// errUnsupported marks what the per-package build doesn't do yet.
var errUnsupported = errors.New("unsupported for a per-package build")

// markLink marks the main package's block, which ends with the link.
func markLink(blocks map[string]*block) error {
	var links []string
	for id, b := range blocks {
		if b.linkAt > 0 {
			links = append(links, id)
		}
	}
	if len(links) > 1 {
		sort.Strings(links)
		return fmt.Errorf("%w: several links in the plan: %s", errUnsupported, strings.Join(links, ", "))
	}
	id := mainID
	if len(links) == 1 {
		id = links[0]
	}
	main, ok := blocks[id]
	if !ok {
		return fmt.Errorf("%w: no main package in the plan", errUnsupported)
	}
	main.isLink = true
	if main.linkLine() == "" {
		return fmt.Errorf("%w: no link step for %s", errUnsupported, main.importPath)
	}
	// A test link writes the test binary where it stays; go test copies it
	// to -o itself.
	if m := reTestLinkOut.FindStringSubmatch(main.linkLine()); main.out == "" && m != nil && m[1] == id {
		main.out = m[2]
	}
	if main.out == "" {
		return fmt.Errorf("%w: no link output for %s", errUnsupported, main.importPath)
	}
	return nil
}

// addInputs records the files and directories under the source and module
// roots that a plan line names: arguments, flag values after "=", and -I and
// -L directories, each relative to cwd when it starts with "./" or "../". A
// path in a -trimpath or prefix-map rewrite, or after -o, isn't an input.
func addInputs(b *block, r roots, cwd, line string) {
	words := shellWords(line)
	for i, w := range words {
		prev := ""
		if i > 0 {
			prev = words[i-1]
		}
		if prev == "-o" || prev == "-trimpath" || isRewriteFlag(w) {
			continue
		}
		p, dir := w, false
		switch {
		case prev == "-I" || prev == "-L" || prev == "-isystem" || prev == "-iquote":
			dir = true
		case len(w) > 2 && (strings.HasPrefix(w, "-I") || strings.HasPrefix(w, "-L")):
			p, dir = w[2:], true
		case strings.HasPrefix(w, "-"):
			_, v, ok := strings.Cut(w, "=")
			if !ok {
				continue
			}
			p = v
		}
		if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") {
			p = path.Join(cwd, p)
		}
		if !path.IsAbs(p) {
			continue
		}
		if dir {
			addDir(b, r, path.Clean(p))
		} else {
			addFile(b, r, path.Clean(p))
		}
	}
}

// isRewriteFlag reports a flag whose value rewrites paths rather than naming
// files: -trimpath for Go's tools, the prefix maps for a C compiler.
func isRewriteFlag(w string) bool {
	return strings.HasPrefix(w, "-trimpath=") || strings.Contains(w, "-prefix-map=")
}

// addEmbedded records the files an embedcfg names, from its JSON.
func addEmbedded(b *block, r roots, cfg string) error {
	var c struct{ Files map[string]string }
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		return err
	}
	for _, f := range c.Files {
		addFile(b, r, f)
	}
	return nil
}

// addDir records a directory under a root as an input, everything in it.
func addDir(b *block, r roots, p string) {
	if p == r.Src {
		b.srcFiles = appendUniq(b.srcFiles, "**")
	} else if rel, ok := strings.CutPrefix(p, r.Src+"/"); ok {
		b.srcFiles = appendUniq(b.srcFiles, rel+"/**")
	} else if rel, ok := strings.CutPrefix(p, r.Mod+"/"); ok {
		b.modFiles = appendUniq(b.modFiles, rel+"/**")
	}
}

// shellWords splits a plan line into the words a shell would: whitespace
// separates them, single quotes keep everything, and double quotes keep
// everything but a backslash escape.
func shellWords(line string) []string {
	var words []string
	var w strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, w.String())
				w.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			for i++; i < len(line) && line[i] != '\''; i++ {
				w.WriteByte(line[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				w.WriteByte(line[i])
			}
		case c == '\\' && i+1 < len(line):
			inWord = true
			i++
			w.WriteByte(line[i])
		default:
			inWord = true
			w.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, w.String())
	}
	return words
}

// addFile records an input file of the block, relative to its root.
func addFile(b *block, r roots, p string) {
	if rel, ok := strings.CutPrefix(p, r.Src+"/"); ok {
		b.srcFiles = appendUniq(b.srcFiles, rel)
	} else if rel, ok := strings.CutPrefix(p, r.Mod+"/"); ok {
		b.modFiles = appendUniq(b.modFiles, rel)
	}
}

func appendUniq(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// The -c a compile gets: a fixed value rather than the plan's, which is the
// planning machine's CPU count (and absent on one CPU), so a package's exec
// is the same on every engine; the compiler's output doesn't depend on it,
// and it uses 1 where its flags don't allow more. A package built in its
// own exec mostly compiles alone, as the big packages at the end of a build
// do, so it gets what go build passes on a 16-CPU machine. The standard
// library and dependencies build many at once, so each gets 4: in dagger's
// build, 16 there slowed them more than it sped their packages. A heavy one,
// with at least heavyWeight of Go source of its own, such as the runtime or
// a generated API client, gets 16 too: it is long, so its own compile holds
// up its dependents, and few compile at once.
const (
	soloCompileC = "16"
	depCompileC  = "4"
	heavyWeight  = 1 << 20
)

// compileC is the -c of a package's compiles: outside is whether it is the
// standard library's or a dependency's, weight its weight in the plan.
func compileC(outside bool, weight int64) string {
	if outside && weight < heavyWeight {
		return depCompileC
	}
	return soloCompileC
}

// script renders a block as one shell script for its exec. A dependency's
// archive is read from /work/lib. Archives get no Go build ID: the plan's are
// temporary, and leaving them out lets early cutoff compare archives by
// their bytes. The link gets one made from what it reads (gohdr linkid).
func (b *block) script(blocks map[string]*block, conc string) string {
	dirs := []string{"/work/" + b.name}
	var body strings.Builder
	var heredoc []string
	heredocTarget := ""
	needed := cdNeeded(b.lines)
	for i, l := range b.lines {
		// A cgo package's C compiles name its files by bare names in its
		// directory, so it keeps every change the plan prints to it; any
		// other change stays only when needed, as elsewhere, such as the
		// one carried in from the previous step or one back to the build's
		// directory, so it doesn't tie the step to the project.
		if isCd(l) && !needed[i] && (!b.cgo || !cdTo(l, b.dir)) {
			continue
		}
		link := b.isLink && i == b.linkAt-1
		if !link {
			l = reBuildID.ReplaceAllString(l, "")
		}
		// The go command leaves -c out when it would be 1, so the value is
		// put in one place on every compile rather than replaced.
		if reCompile.MatchString(l) {
			l = reConc.ReplaceAllString(l, "")
			l = reCompile.ReplaceAllString(l, "${1} -c="+conc+" ")
		}
		// The go command packs archives in-process; "go tool pack" would
		// build the pack tool from source in every fresh container.
		l = strings.Replace(l, "go tool pack ", toolsDir+"/pack ", 1)
		l = reDepArchive.ReplaceAllStringFunc(l, func(s string) string {
			id := reDepArchive.FindStringSubmatch(s)[1]
			name := blocks[id].name
			if id == b.id {
				return "/work/" + name + "/" + name + ".a"
			}
			return "/work/lib/" + name + ".a"
		})
		l = reWorkRef.ReplaceAllStringFunc(l, func(s string) string {
			return "/work/" + blocks[strings.TrimPrefix(s, "$WORK/")].name
		})
		// What's left is $WORK itself, as in cgo's "cd $WORK".
		l = strings.ReplaceAll(l, "$WORK", "/work")
		// The link's build ID, from which the linker also makes the
		// binary's GNU build ID, is made in the exec from what the link
		// reads, as go build makes it: the plan's is made from headers, so
		// it would stay the same across edits inside function bodies. A
		// -buildid in the call's -ldflags comes later and still wins.
		if loc := reLinkBuildID.FindStringIndex(l); link && loc != nil {
			if m := reImportcfg.FindStringSubmatch(l); m != nil {
				sum := sha256.Sum256([]byte(l[:loc[0]] + l[loc[1]:]))
				body.WriteString("id=$(/gobuild/gohdr linkid " + m[1] + " " + hex.EncodeToString(sum[:8]) + ")\n")
				l = l[:loc[0]] + " -buildid=$id" + l[loc[1]:]
			}
		}
		if heredocTarget != "" {
			if l == "EOF" {
				body.WriteString("printf '%s\\n'")
				for _, h := range heredoc {
					body.WriteString(" " + shQuote(h))
				}
				body.WriteString(" > " + heredocTarget + "\n")
				heredoc, heredocTarget = nil, ""
				continue
			}
			heredoc = append(heredoc, l)
			continue
		}
		if m := reHeredoc.FindStringSubmatch(l); m != nil {
			heredocTarget = m[1]
			continue
		}
		if m := reMkdirLine.FindStringSubmatch(l); m != nil {
			dirs = append(dirs, m[1])
			if m[2] != "" {
				body.WriteString(m[2] + "\n")
			}
			continue
		}
		body.WriteString(l + "\n")
	}
	return "set -e\n# " + b.importPath + "\nmkdir -p " + strings.Join(dirs, " ") + "\n" + body.String()
}

// isCd reports whether a plan line changes directory: the go command's own
// "cd", or the "mkdir -p DIR && cd DIR" parsePlan starts each block with.
func isCd(l string) bool {
	return strings.HasPrefix(l, "cd ") || (strings.HasPrefix(l, "mkdir -p ") && strings.Contains(l, " && cd "))
}

// cdTo reports whether a plan line is the go command's own change to dir.
func cdTo(l, dir string) bool {
	w := shellWords(l)
	return len(w) == 2 && w[0] == "cd" && w[1] == dir
}

// cdNeeded reports, by line index, which directory changes matter: the
// plan's own changes into the work directory, where the go command runs
// tools on bare names (a plugin's or c-shared library's link writes
// "-o a.out.so" there), and which every project's exec has at the same
// /work path; and those a later line uses through a relative path before
// the next change. The others are left out, so a package's exec doesn't
// carry the directory the build happened to be in (the project's mount
// path, or the previous package's directory) and two projects can share it.
func cdNeeded(lines []string) map[int]bool {
	needed := map[int]bool{}
	last := -1
	for i, l := range lines {
		switch {
		case l == "cd $WORK" || strings.HasPrefix(l, "cd $WORK/"):
			needed[i] = true
			last = -1
		case isCd(l):
			last = i
		case last >= 0 && usesDir(l):
			needed[last] = true
		}
	}
	return needed
}

// usesDir reports whether a plan line names a path relative to the
// directory it runs in: "./x" or "../x", or a relative -o output.
func usesDir(l string) bool {
	if reRelFile.MatchString(l) {
		return true
	}
	words := shellWords(l)
	for i, w := range words {
		if strings.HasPrefix(w, "../") {
			return true
		}
		if i > 0 && words[i-1] == "-o" && !path.IsAbs(w) && !strings.HasPrefix(w, "$WORK") {
			return true
		}
	}
	return false
}

// ccInfo is what the plan exec learns for the C compiler flags go build -n
// leaves out (gohdr cc).
type ccInfo struct {
	// probes holds the answer to each probe, by compiler and flag.
	probes map[[2]string]bool
	// modRoots maps a cgo package's directory to its module's root.
	modRoots map[string]string
	// goos is the target's GOOS.
	goos string
}

func parseCC(out string) ccInfo {
	cc := ccInfo{probes: map[[2]string]bool{}, modRoots: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		switch {
		case len(f) == 4 && f[0] == "probe":
			cc.probes[[2]string{f[1], f[2]}] = f[3] == "yes"
		case len(f) == 3 && f[0] == "modroot":
			cc.modRoots[f[1]] = f[2]
		case len(f) == 2 && f[0] == "goos":
			cc.goos = f[1]
		}
	}
	return cc
}

// addCFlags puts back into a cgo package's C compiler commands the flags the
// go command adds when a probe of the compiler says they work, which go
// build -n answers no without asking. As in Go 1.24 to 1.26: around
// -fmessage-length=0, those of every compiler command (compilerCmd), which
// map the package's work directory to /tmp/go-build in debug information
// and leave the command out of it; and before a C compile's output, those
// of a compile (ccompile), which map the package's sources and seed the
// compiler.
func (b *block) addCFlags(cc ccInfo) {
	if !b.cgo {
		return
	}
	var compile []string
	for _, l := range b.lines {
		if reCompile.MatchString(l) {
			compile = shellWords(l)
			break
		}
	}
	from, to := b.sourceMap(cc, compile)
	seed := ""
	for i, w := range compile {
		if w == "-buildid" && i+1 < len(compile) {
			seed, _, _ = strings.Cut(compile[i+1], "/")
		}
	}
	const msgLen = " -fmessage-length=0"
	for i, l := range b.lines {
		at := strings.Index(l, msgLen)
		if at < 0 {
			continue
		}
		words := shellWords(l[:at])
		k := 0
		for k < len(words) && reEnv.MatchString(words[k]) {
			k++
		}
		if k == len(words) {
			continue
		}
		works := func(flag string) bool { return cc.probes[[2]string{words[k], flag}] }
		var before, after, end []string
		for _, f := range []string{"-fno-caret-diagnostics", "-Qunused-arguments", "-Wl,--no-gc-sections"} {
			if works(f) {
				before = append(before, f)
			}
		}
		mapFlag := ""
		if works("-fdebug-prefix-map=a=b") {
			mapFlag = "-fdebug-prefix-map"
			if works("-ffile-prefix-map=a=b") {
				mapFlag = "-ffile-prefix-map"
			}
			after = append(after, mapFlag+"=$WORK/"+b.id+"=/tmp/go-build")
		}
		if works("-gno-record-gcc-switches") {
			after = append(after, "-gno-record-gcc-switches")
		}
		l = l[:at] + flagList(before) + msgLen + flagList(after) + l[at+len(msgLen):]
		if loc := reCOut.FindStringIndex(l); loc != nil {
			if mapFlag != "" && from != "" {
				end = append(end, quoteArg(mapFlag+"="+from+"="+to))
			}
			if seed != "" && works("-frandom-seed=1") {
				end = append(end, "-frandom-seed="+seed)
			}
			l = l[:loc[0]] + flagList(end) + l[loc[0]:]
		}
		b.lines[i] = l
	}
}

// sourceMap is the rewrite of a cgo package's sources in its C compiles'
// debug information, from the package's Go compile: GOROOT for the
// standard library, and with -trimpath, the module's root (its vendor
// directory when vendored) to its path and version, both as the Go
// compile's rewrite of the package's directory has them. Each goes under
// /_, or \\_\_ for a Windows target.
func (b *block) sourceMap(cc ccInfo, compile []string) (from, to string) {
	under := "/_/"
	if cc.goos == "windows" {
		under = `\\_\_/`
	}
	if slices.Contains(compile, "-std") {
		if root, ok := strings.CutSuffix(b.dir, "/src/"+b.importPath); ok {
			return root, under + "GOROOT"
		}
		return "", ""
	}
	// -trimpath rewrites the package's directory in the Go compile.
	rewrite := ""
	for i, w := range compile {
		if w == "-trimpath" && i+1 < len(compile) {
			for _, r := range strings.Split(compile[i+1], ";") {
				if f, t, ok := strings.Cut(r, "=>"); ok && f == b.dir {
					rewrite = t
				}
			}
		}
	}
	switch {
	case rewrite == "":
		return "", ""
	case strings.HasSuffix(b.dir, "/vendor/"+b.importPath):
		return strings.TrimSuffix(b.dir, "/"+b.importPath), under + "vendor"
	case strings.Contains(rewrite, "@"):
		// path@version, then the package's directory in the module.
		mod, sub := rewrite, ""
		if i := strings.Index(rewrite, "@"); strings.Contains(rewrite[i:], "/") {
			j := i + strings.Index(rewrite[i:], "/")
			mod, sub = rewrite[:j], rewrite[j:]
		}
		if root, ok := strings.CutSuffix(b.dir, sub); ok {
			return root, under + mod
		}
	default:
		// The import path: the module's path (from whichever go.mod the
		// build reads, -modfile's included), then the package's directory
		// under the module's root.
		root, ok := cc.modRoots[b.dir]
		if !ok {
			break
		}
		sub, ok := strings.CutPrefix(b.dir, root)
		if !ok {
			break
		}
		if mod, ok := strings.CutSuffix(rewrite, sub); ok && mod != "" {
			return root, under + mod
		}
	}
	return "", ""
}

func flagList(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	return " " + strings.Join(flags, " ")
}

// quoteArg quotes a word for the shell when it needs it.
func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]#~") {
		return shQuote(s)
	}
	return s
}

// linkLine is the plan's link command, or "" for a block that doesn't link.
func (b *block) linkLine() string {
	if !b.isLink || b.linkAt == 0 {
		return ""
	}
	return b.lines[b.linkAt-1]
}

// cgoOutputs are the Go files cgo writes that a cgo package's compile reads,
// by their names in its work directory, in the compile's order.
func (b *block) cgoOutputs() []string {
	var out []string
	for _, l := range b.lines {
		if !reCompile.MatchString(l) {
			continue
		}
		for _, w := range shellWords(l) {
			if f, ok := strings.CutPrefix(w, "$WORK/"+b.id+"/"); ok && strings.HasSuffix(f, ".go") {
				out = append(out, f)
			}
		}
	}
	return out
}

func shJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shQuote(a)
	}
	return strings.Join(quoted, " ")
}

// cgoHeader is the C header cgo writes for a package's exported functions.
const cgoHeader = "_cgo_install.h"

// exportsHeader reports a c-archive or c-shared build whose cgo step is
// asked for the header of its exported functions, which go build installs
// next to the library. cgo writes it only for a package that exports one.
func (b *block) exportsHeader() bool {
	link := b.linkLine()
	if !strings.Contains(link, "-buildmode=c-archive") && !strings.Contains(link, "-buildmode=c-shared") {
		return false
	}
	for _, l := range b.lines {
		if strings.Contains(l, "-exportheader=$WORK/"+b.id+"/"+cgoHeader) {
			return true
		}
	}
	return false
}

// output is the file a block's exec produces, under /work/<name>.
func (b *block) output() string {
	switch {
	case b.isLink:
		return b.out
	case b.isProfile:
		return "pgo.preprofile"
	}
	return b.name + ".a"
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// rePosition is a go command message about a position in a Go file:
// "file.go:line:col: message".
var rePosition = regexp.MustCompile(`^(.+\.go):(\d+):(\d+)(:.*)$`)

// sourcePosition is the position in src of line and col in its header, as
// the scan's header pack holds it (goHeader, in go-includes): the source up
// to the end of its imports, then each go:embed line after them, trimmed,
// one per line, then a line that ends the header. It reports false for a
// line past the header's go:embed lines.
func sourcePosition(src []byte, line, col int) (int, int, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || f == nil {
		// A file that does not parse is in the pack whole.
		return line, col, true
	}
	end := f.Name.End()
	for _, d := range f.Decls {
		if d.End() > end {
			end = d.End()
		}
	}
	off := min(fset.Position(end).Offset, len(src))
	shared := bytes.Count(src[:off], []byte("\n")) + 1
	if line <= shared {
		return line, col, true
	}
	k := line - shared
	for i, l := range strings.Split(string(src[off:]), "\n") {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "//go:embed") {
			continue
		}
		if k--; k == 0 {
			return shared + i, col + strings.Index(l, t), true
		}
	}
	return line, col, false
}
