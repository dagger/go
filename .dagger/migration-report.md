# Migration Report

## Root module requires explicit loading

The root module is still valid, but it must be loaded explicitly.

- **This works**: `dagger -m . call --help`
- **This no longer works**: `dagger call --help`

ACTION: If your scripts rely on implicit loading of the root module, change them to use explicit loading.
