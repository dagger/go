# Container directive example

This example uses Dagger v1.0.0-beta.15.

Run the generator:

```sh
dagger generate
cat generated.txt
git status --short
```

`generate.go` selects `dag://example/generate-env`. This container has Go,
a mounted configuration file, and an HTTP service. The generator checks the
configuration and writes the service response to `generated.txt`.

Edit `internal/generate/main.go` with `nano`, then run `dagger generate` again.

Remove the `//go:generate:container` line to try the default container. The
generator will fail because it needs the selected container's configuration
and service. Restore the file with `git restore generate.go`.

The workspace module is in `.dagger/example/main.dang`. The Go module under
test is in `.dagger/go`. These are local copies that you can edit.
