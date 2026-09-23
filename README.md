# go

[Dagger](https://dagger.io) modules for Go tooling, written in the `.dang`
module language.

Go has one toolchain from one supplier, so `go` is one module and it lives at
the root of this repository, where it always has. The linters do not come from
that supplier and do not share its release dates, so they are their own modules
— over one shared library, in this repository.

```
github.com/dagger/go
├── go.dang           the main module, at the root: test, generate
├── gomod/            the shared library. No checks.
├── golangci-lint/    lint
├── staticcheck/      lint
└── .dagger/          development checks for the root module
```

## Install

`go` is the root module, so its address is the repository:

```sh
dagger install github.com/dagger/go              # test, generate
dagger install github.com/dagger/go/golangci-lint
dagger install github.com/dagger/go/staticcheck
```

`gomod` is a library, not a tool. You depend on it when you write a module; you
do not install it to get checks.

## Scoping

Every module discovers **every** Go module in the workspace, so a repository
with test fixtures or a vendored copy of some source will have those checked
too. Say which module roots you mean in `dagger.toml`:

```toml
[modules.golangci-lint.settings]
lint = ["**", "!docs"]
```

A bare pattern selects, a `"!"`-prefixed pattern excludes, and an exclude wins
whatever the order. `docs` means `docs` and every module below it; `**` and `*`
mean all of them; `["!**"]` means none. Other glob shapes are not interpreted.
`go` spells its two as `test` and `generate` rather than `lint`.

## The modules

### `go`

| Function              | Description                                                   |
| --------------------- | ------------------------------------------------------------- |
| `modules`             | Modules discovered from the workspace, as a collection.       |
| `module`              | The module containing a workspace path.                       |

On a module: `test`, `generate`, `test-directories`, `skip-test`,
`skip-generate`, `has-generate-directives`, `generate-directories`, `base`,
`include`, `include-base`, `include-discovered`, `source`, `test-data`.

#### Settings

| Setting               | Meaning                                                       |
| --------------------- | ------------------------------------------------------------- |
| `version`             | Go version for every module, instead of each one's `go.mod`.  |
| `base`                | Base image for Go containers.                                 |
| `includeExtraFiles`   | Extra workspace files to mount, as include patterns.          |
| `test`                | Module roots to test (see [Scoping](#scoping)).               |
| `generate`            | Directories to run `go generate` in (see below).              |
| `goflags`             | `GOFLAGS` in every Go container.                              |
| `mountPath`           | Where the workspace is mounted in Go containers.              |

`base` must carry a Go toolchain and any C/C++ dependencies the modules need.
otelgotest is installed into it unless it already has one. It supplies its own
toolchain, so `version` is ignored alongside it and recorded in `warnings`.

`goflags` is passed as-is, e.g. `-tags=extended,withdeploy`. Flags that take a
value must use the `-flag=value` form, as Go requires inside `GOFLAGS`.

`mountPath` defaults to `/src/<workspace name>`, where the name is the last
segment of the workspace address — usually the repository name — so tests that
expect it in their working directory keep passing. It must be absolute.

#### Modules and batches

`modules` finds the Go modules at or below the workspace cwd, plus the
enclosing module when the cwd is inside one. `include`/`exclude` filter the
roots it finds. `module` returns the module containing a path; pass
`findUp: false` when the path is already a module root.

`modules` is a collection keyed by module root, so it adds a `go-module`
dimension. Each module's tests are a collection too, keyed by test function
name, which adds `go-test`. Checks and generators select on both:

```console
$ dagger list go-tests --go-module=sdk/go
$ dagger check go/modules/tests/run --go-module=sdk/go --go-module=cmd/tool
$ dagger check go/modules/tests/run --go-module=sdk/go --go-test=TestConnect
$ dagger generate go/modules/generate --go-module=sdk/go
```

Tests run once per selected module, through the `GoTests` batch `run`. With
every test selected it runs `go test ./...`, examples included; with some
filtered out it runs `go test -run` over the selected names.

Because discovery starts at the cwd, `dagger -W ./sdk/go check` scopes every
tool to that module without a dimension flag.

Test names come from `go test -list`, so reading them builds the module's
tests. A module outside the `test` selection, or one whose sources do not
build, reports no tests instead of failing discovery.

`test` on a module, and the `modules` batch `test`, are plain functions rather
than checks, so that `dagger check` does not run the same tests twice. The
batch `test` runs every selected module even after one fails, then lists each
failing module by path.

#### Generate

`generate` patterns are relative to the workspace root, whatever the cwd, and
use Dagger glob syntax: `*`, `?` and character classes match within a path
segment, and `**` matches zero or more segments.

- A literal path selects only that directory, even if it is a module root.
  `.` selects the workspace root only.
- `internal/**` selects `internal` and everything below it.
- A `!` prefix excludes, and exclusions always win.
- An empty list, or one with only exclusions, starts from every directory.
- The default `["**"]` reaches directories in nested modules.

For example: `["sdk/go/engineconn", "internal/**", "!internal/fixtures/**"]`.

`go generate .` runs in each selected directory that has a generator command;
`go:generate:include` alone is not one. Within a module, directories run in
lexical path order. Modules run independently and cross-module dependencies are
not inferred; use an explicit coordinating command when generators need a
different order.

A directory can pick the container its generators run in with
`//go:generate:container`, resolved with the caller's `Workspace.resolve` — a
workspace container by name, such as `generate-env`, or an image reference such
as `docker.io/library/golang:1.26.1-alpine`. Conflicting values in one
directory are errors. Consecutive directories naming the same one share a
container; when it changes, workspace files carry over so later commands still
see earlier output. Without the directive, the directory uses the module's own
base container.

A module's own `generate` fails on a module the scan could not read, naming the
file, while the batch `generate` skips it (see below).

Tests run with a nested Dagger engine, so a suite that drives Dagger works.

### `golangci-lint`

| Function     | Description                                   |
| ------------ | --------------------------------------------- |
| `modules`    | Modules discovered from the workspace, as a collection. |
| `module`     | The module containing a workspace path.       |
| `version`    | The bundled golangci-lint version.            |

On a module: `lint` (a `@check`). The collection's batch `lint` runs once over
the selected modules in place of each module's own, as for `go`.

`version` is the linter release, pinned to 2.11.4 by digest, and `goVersion` is
the Go toolchain — chosen independently, because the binary is copied out of
its image onto that toolchain. A C/C++ toolchain is present, since cgo
dependencies need one during typecheck.

### `staticcheck`

| Function     | Description                                   |
| ------------ | --------------------------------------------- |
| `modules`    | Modules discovered from the workspace, as a collection. |
| `module`     | The module containing a workspace path.       |

On a module: `lint` (a `@check`). The collection's batch `lint` runs once over
the selected modules in place of each module's own, as for `go`.

`version` is the Staticcheck release, built once with `go install` in a pinned
Go container, and `goVersion` is the Go toolchain. Test files are analyzed;
tests are not executed.

### `gomod`

The shared library. It has no checks of its own and never will — two modules
with a check for the same tool would run that tool twice.

| Function            | Description                                                 |
| ------------------- | ----------------------------------------------------------- |
| `modules`           | Modules discovered from the workspace's go.mod files.       |
| `module`            | The module containing a workspace path.                     |
| `at`                | A module root taken as given, with no workspace consulted.  |
| `setting-patterns`  | Selection patterns, repaired for older beta engines.        |
| `go-container`      | A Go toolchain container at a version, with the shared caches. |
| `tool-builder`      | The pinned container tool binaries are built in.             |
| `with-tool`         | Install a tool binary, unless the container has one.        |
| `with-warnings`     | Announce ignored settings as container steps.               |
| `default-go-version` | The version used for a module that declares no `go` directive. |
| `scanned-module-roots` | The directories the scan accepts as Go modules.          |
| `scan`              | The raw workspace scan, as a directory.                     |
| `with-go-caches`    | A container with the shared Go download and build caches.   |

On a module:

| Function                | Description                                                   |
| ----------------------- | ------------------------------------------------------------- |
| `selected`              | Whether selection patterns choose this module root.           |
| `include-discovered`    | Patterns found by scanning directives and local replaces.     |
| `include` / `source`    | What this module mounts, as patterns and as a directory.      |
| `test-directories`      | Directories holding Go test files.                            |
| `generate-directories`  | Directories holding a `go:generate` command.                  |
| `scan-error`            | Why this module could not be scanned, or null.                |
| `include-base`          | The built-in Go source patterns, module-scoped.               |
| `go-version`            | The Go version this module's `go` directive asks for.         |
| `subpath` / `test-data` | Path and testdata mechanics.                                  |

## Choices worth knowing

**The tool types stay duplicated; the mechanics do not.** Each module returns
its own `GoModule`, because that type is what its callers see and each tool has
its own fields and verbs to hang on it. Dang also cannot yet name a
dependency's non-root type. So each `GoModule` holds a path and forwards
discovery, selection and source policy to `gomod`.

**One source scan for the whole workspace.** `gomod` builds the
`helpers/go-includes` scanner in a pinned Go image that depends on nothing
about the caller or the module root, so three tools with three different base
images still produce one identical container. The engine builds it once, runs
it once, and each module reads its own slice of the output. Before, each of the
three modules carried its own copy of that program and ran its own scan.

**The Go toolchain comes from the module, not from the tool.** With nothing
set, each module is checked with the toolchain its own `go.mod` asks for. Each
module runs in its own container anyway, so there is no need to find one
version that suits the whole workspace.

| | |
| --- | --- |
| `go 1.26.1` in a module | `golang:1.26-alpine` — the minor series, always its newest patch |
| `goVersion: "1.26.1"` | `golang:1.26.1-alpine` — set explicitly, used as written |
| no `go` directive | `gomod`'s `default-go-version` |

The series rather than the exact patch, because a `go` directive is a minimum
and a `go.mod` bumped to a patch with no image yet would otherwise stop the
module being checked at all. The root reports `null` for `version`/`goVersion`,
since the answer varies per module; ask a module what it resolved to.

**A tool is never built in the module's container.** golangci-lint's binary is
copied out of its pinned image; Staticcheck and otelgotest are built once in a
pinned Go container. A module on an older Go would otherwise fail to build the
tool, and the error would be about the tool rather than the module.

A tool is installed only when the container does not already have it, so a
`base` carrying its own build — an instrumented one, say — keeps it. `version`
therefore says *what to install if needed* and composes with `base`.

`base` brings its own toolchain, so `version`/`goVersion` is ignored alongside
it rather than refused — no setting needs editing to say what the base already
says. Each ignored setting is listed in `warnings` and announced as a step in
the container, since Dang has no diagnostics channel of its own.

**A directory with a `go.mod` is not automatically a module.** Discovery
reports a module root only when its `go.mod` parses and carries a module line,
and when the module holds Go files that Go itself would look at — not only ones
under `testdata/` or a `.`/`_` directory. Everything else is left out, because
handing it to a tool yields a complaint about the tool rather than about the
directory: golangci-lint errors on a module with no packages, and no Go command
accepts a `go.mod` with no module line. `module` and `at` still reach one, so a
caller that spells out a path gets that path, and an unparseable `go.mod` still
says what is wrong with it. `gomod`'s `scanned-module-roots` is the list.

The line is "no Go files", not "no packages after build constraints". A module
whose files are all constrained out is still a module, and the toolchain's
message about it names the module already.

**A module the scan cannot read fails alone.** One scan serves every tool in
the workspace, so a module with an unreadable `go.mod` or a Go file that will
not parse records the reason against itself and the pass carries on. Anything
that needs that module's sources raises the reason, which names the file;
everything else is unaffected. The batch `test` reports it beside the modules
that passed, and the batch `generate` — which has to ask every selected module
whether it holds generators — gets an answer rather than an error. Failing the
whole scan instead would let one bad file stop every check in the repository,
including for modules you had excluded.

**Discovery errors name the file.** `go: error reading go.mod: missing module
declaration`, from whichever container happened to run first, is not something
you can act on. The scan catches the same fault and says
`vendored/go.mod: missing module declaration`, and an unresolvable local
`replace` points at the line that declares it.

**Scan flags mean what they say.** A bare scan follows `go:embed` and local
`replace` directives. `--test` also follows `go:test:include`, and `--generate`
also follows `go:generate:include` and the modules a `go:generate go -C`
reaches. A lint run asks for neither, because golangci-lint and Staticcheck
type check the code rather than run it.

**Selection is one syntax, asserted in one place.** `gomod`'s suite owns the
selection matrix; each tool's suite asserts only that its own settings reach
it.

**Fixtures are shared where they are the same.** `testdata/` and `fixtures/` at
the root serve every module. A tool keeps its own only when the fixture is
specific to it — `golangci-lint/testdata/go-module-lint-fail` does not compile,
so it cannot live where `go`'s batch `test` would find it.

## Known problem: only one failure is reported

A batch of three failing modules reports one failure, not three. The lint
modules aggregate results into a directory and sync it, and the first error
stops every later report; you repair one module, run again, and find the next.
`go`'s batch `test` does not have this shape — it collects each module's failure
and names them all.

The obvious repair for the lint modules — collecting exit codes rather than
merging outputs — does report all three, but it loses the parallel run. Dang
has no structured concurrency primitive of its own, so this should be repaired
once, in Dang, rather than twice here.

## Development

One command runs all four suites:

```sh
dagger check
```

That builds a dev engine from dagger/dagger and runs them inside it, because
`//go:generate:container` needs `Workspace.resolve`, which no released engine
has yet. The engine commit is pinned in `.dagger/modules/engine-e2e/`, and the
suites it runs are listed in that module's `workspace.toml`.

The three suites that do not need the dev engine also run against a released
one directly, which is far quicker while iterating:

```sh
dagger check -m gomod/.dagger/modules/e2e         # the shared library
dagger check -m golangci-lint/.dagger/modules/e2e
dagger check -m staticcheck/.dagger/modules/e2e

dagger check -m gomod/.dagger/modules/e2e scan-check   # or one check by name
```

`dagger shell playground` opens the dev engine with an example repository at
`/example`; run `dagger generate` there to try the container directive.

`testdata/` and `fixtures/` hold the modules the suites run against. Several of
them fail on purpose — a failing test, a lint diagnostic, a module with no
packages — and the suites assert those failures, so repairing a fixture would
be a hole in the coverage rather than a repair.

## Relationship to the standalone modules

`github.com/dagger/golangci-lint` and `github.com/dagger/staticcheck` came
first and still work. The modules here are the same tools rebuilt on the shared
library, so they gain what it gives every module: one notion of a module root,
one selection syntax, one source scan shared across every tool in the
workspace.

Two differences are worth knowing before you switch:

- `skipLint` no longer takes a workspace. It never read one; selection is a
  question about a path.
- A bare scan no longer implies test inputs. The standalone modules were
  embed-only in practice, so this changes nothing about what they mounted, but
  the flag now says so.
