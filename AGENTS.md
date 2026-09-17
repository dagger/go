# Repository conventions

One repository, one module per directory, with `go` at the root:

- `go.dang` — the main module. Test and generate.
- `gomod/` — the shared library: module discovery, selection, source policy,
  and the `helpers/go-includes` scanner. It never gets `@check` functions.
- `golangci-lint/`, `staticcheck/` — tool modules, each with its own checks.
- `testdata/`, `fixtures/` — fixture modules shared by every suite. Several
  fail on purpose. A tool keeps its own fixture only when it cannot live here.
  Two are not modules at all — `testdata/go-module-empty` holds no Go files and
  `fixtures/go-module-unreadable` has a `go.mod` with no module line — and the
  suites assert that discovery leaves them out. Do not add selection exclusions
  for them: an exclusion would hide a regression in that filtering.

Each module is checked with the Go toolchain its own `go.mod` asks for, unless
`version`/`goVersion` says otherwise — or `base`, which supplies its own and so
makes those settings ignored, recorded in `warnings`, rather than refused. Tool
binaries come from `gomod`'s pinned `tool-builder`, never built in the module's
own container, and `gomod.withTool` installs one only when the container does
not already carry it.

Mechanics belong in `gomod`. Each tool keeps its own `GoModule` type, holding a
path and forwarding to the library, because Dang cannot name a dependency's
non-root type and because the type is that tool's public API.

When changing a module, always make sure the tests are up to date.

To run tests: `dagger check` runs all four suites, because each is installed in
`.dagger/modules/engine-e2e/workspace.toml`. This builds a dev engine from
dagger/dagger#14178 and runs the
module checks in it. The engine commit is set in
`.dagger/modules/engine-e2e/main.dang` and its `dagger-module.toml`. Keep both
values equal.

One suite, or one check, at a time:

```sh
dagger check -m .dagger/modules/go-dev            # the root go module
dagger check -m gomod/.dagger/modules/e2e         # the shared library
dagger check -m golangci-lint/.dagger/modules/e2e
dagger check -m staticcheck/.dagger/modules/e2e

dagger check gomod-checks:scan-check
```

A new suite is installed under a `<tool>-checks` key: the key is what prefixes
its checks, and `e2e` would kebab-case into `e-2-e`.

To try the container directive: `dagger shell playground`
The shell starts in `/example`. Run `dagger generate` there.

To format: `dang fmt -w go.dang gomod/main.dang golangci-lint/main.dang
staticcheck/main.dang .dagger/modules/go-dev/main.dang
gomod/.dagger/modules/e2e/main.dang
golangci-lint/.dagger/modules/e2e/main.dang
staticcheck/.dagger/modules/e2e/main.dang` (needs dang v2.x; v0.1.0 cannot
parse `.{{ }}` selections)
