# Security policy

## Reporting

This source candidate has not been publicly released. A verified private
security reporting channel has not yet been configured; establishing and testing
one is required before public release. Do not post credentials, private prompts,
or vulnerability details in public issues.

## Current boundary

- The gateway binds only a loopback IP and requires a local Bearer token.
- Provider and gateway credentials are read from macOS Keychain references.
- Configuration does not serialize runtime credentials. Errors avoid exposing
  upstream response bodies; full prompts and responses are not persisted.
- Requests are sent to the configured upstream. No telemetry or diagnostic
  upload feature is implemented.

The candidate has known correctness gaps documented in
[acceptance status](docs/mvp-acceptance.md). There are no published binaries,
verified Homebrew assets, or promised response-time guarantees yet.

Security reports should cover the CLI, HTTP service, ledger, credentials,
and bundled dependencies. Public release materials must identify affected
versions, the reporting channel, and supported release versions.
