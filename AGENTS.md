When changing the module, always make sure the tests are up to date

To run tests: `dagger check`

This builds a dev engine from dagger/dagger#14178 and runs the module checks in it.
The engine commit is set in `.dagger/modules/engine-e2e/main.dang` and its
`dagger-module.toml`. Keep both values equal.

To format: `dang fmt -w go.dang .dagger/modules/go-dev/main.dang` (needs dang v2.x; v0.1.0 cannot parse `.{{ }}` selections)
