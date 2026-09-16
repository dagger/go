When changing the module, always make sure the tests are up to date

To run tests: `dagger check -m .dagger/modules/e2e`

To format: `dang fmt -w go.dang .dagger/modules/e2e/main.dang` (needs dang v2.x; v0.1.0 cannot parse `.{{ }}` selections)
