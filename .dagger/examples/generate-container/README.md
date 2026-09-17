# Container directive example

This Git repository is ready to use. Dagger uses the engine and CLI from
dagger/dagger#14178 at `c624e1f8`.

Run the generator:

```sh
dagger generate
cat generated.txt
git status --short
```

`generate.go` selects `generate-env` from this workspace. The container has Go,
the Dagger CLI, and a dev engine service. The generator uses that service to run
a container. The result contains `dev-engine-connected`.

Edit `internal/generate/main.go` with `nano`. Change the message, run
`dagger generate` again, and read `generated.txt`.

To see the default behavior, remove the `//go:generate:container` line from
`generate.go`. This generator will fail because the default base has no dev
engine connection. Restore the file with `git restore generate.go`.

The workspace module is in `.dagger/example/main.dang`. The Go module under test
is in `.dagger/go`. These are local copies that you can edit.
