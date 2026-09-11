# Local release acceptance — 0.1.0

## Local repair status

The source now includes dual-protocol streaming, tools, client launchers,
configurable limits, model discovery and local diagnostics. Real file read/edit/
test workflows pass for Claude, Kilo CLI, Kilo VS Code and Hermes, including
launcher-based continuation. The IDE launcher also survives a full application
restart. See [client acceptance](client-compatibility.md) for boundaries.

The original installed build and evidence below remain a historical baseline.
The repair separately passed package checksums/SBOM, Homebrew install/rollback,
and installed CLI read/edit/test/continuation plus usage/price reconciliation.
Remaining IDE verification is temporarily deferred by the maintainer, so full
IDE acceptance is not claimed. Repair assets retain version 0.1.0 and use a
separate commit/build suffix and hash.

## Original installed baseline: locally verified

- OpenAI and Anthropic text protocols: custom endpoint prefixes, models,
  protocol-specific auth/version, compatible responses and usage normalization.
- Loopback, independent local credentials, strict JSON/config validation,
  rejection of unsupported fields, bounded request/response sizes, no redirects.
- Explicit concurrency; already-canceled and queued-canceled requests do not
  start upstream work; successful queue events are observable.
- Atomic terminal audit/event writes; transport/read/HTTP/parse failures and
  cancellation are not recorded as successful. Audit failure is surfaced safely.
- Unknown usage and price remain null; cached-token arithmetic and explicit
  model pricing are tested without assuming a provider's current prices.
- Interactive configure: PTY covers automatic unlock, no unnecessary prompt,
  wrong-password/cancellation stop, hidden API key input and no plaintext config;
  rollback is tested. Real isolated Keychain writes/read-back preserve existing items.
- Built CLI process tests for both protocols, temporary Keychain double,
  HTTP request, ledger usage/cost query and clean shutdown.
- `CGO_ENABLED=0 go test ./...`, `go vet ./...`, and `go test -race ./...` pass on
  Go 1.27.1 / macOS 15.7.4 arm64.

## Live-provider verification

On 2026-09-11, the unchanged runtime code from rc.4 completed one authorized
non-thinking request per protocol against DeepSeek `deepseek-flash`. Each returned
8 input tokens and 1 output token; matching audit IDs, normalized usage and
explicit USD price arithmetic reconciled. Real credentials stayed in Keychain;
private evidence contains no full message bodies or keys.

This establishes these two DeepSeek-compatible paths, not every provider or
model. Version 0.1.0 changes only the version marker and release documentation
relative to the live-tested runtime.

## Deferred public distribution

GitHub publication is deferred by the maintainer. A verified private
security/community reporting channel and clean-machine public tap installation
remain prerequisites for public distribution. Local source builds, archive
extraction and local-tap installation are verified on macOS 15.7.4 arm64.
See the [release procedure](releasing.md).
