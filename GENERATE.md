# Generation

The `generate` setting selects directories containing `//go:generate` commands.
Paths are relative to the workspace root—the directory containing `dagger.toml`.
Changing the invocation directory does not change the selection.

```toml
[modules.golang]
source = "dagger.io/go"

[modules.golang.settings]
generate = ["sdk/go/engineconn", "internal/**", "!internal/fixtures/**"]
```

- A literal path selects only that directory. `.` selects only the workspace root.
- `*`, `?`, and character classes match within a path segment.
- `**` matches zero or more path segments. `internal/**` includes `internal` itself.
- A leading `!` excludes matching directories. Exclusions always win.
- An empty list, or a list containing only exclusions, starts with all directories.
- The default is `["**"]`.

Each selected directory runs `go generate .`. Directories without generator
commands are skipped. `//go:generate:include` alone is not a generator command.
Go still controls build tags and the order of files and commands in a package.

Directories within one Go module run in lexical path order and share a container,
so later commands can use earlier output. All containers use the configured
`base`. Separate Go modules run independently; dependencies between their
generators are not inferred. Use an explicit coordinating command when generators
require a different order.

This replaces module-level selection. A module path no longer selects its child
directories. There is no compatibility mode.
