When changing the module, always make sure the tests are up to date

To run tests: `dagger check`

To format: `dang fmt -w go.dang .dagger/modules/go-dev/main.dang` (needs dang v2.x; v0.1.0 cannot parse `.{{ }}` selections)
