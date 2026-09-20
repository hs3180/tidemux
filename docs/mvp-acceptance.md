# Local release acceptance — 0.1.0

## Local repair status

The local 0.1.0 release includes dual-protocol streaming, tools, CLI launchers,
configurable limits, model discovery and local diagnostics. Claude Code, Kilo CLI
and Hermes passed installed file read/edit/test workflows and continuation in a
new client process. See [client acceptance](client-compatibility.md) for tested
versions and protocol boundaries. VS Code extension compatibility is out of scope.

Package checksums/SBOM, Homebrew install/rollback, configuration/data compatibility,
and installed CLI usage/price reconciliation passed. Final documentation changes
do not alter the tested runtime. Assets retain version 0.1.0 and use a commit/build
suffix and hash; BUILD.txt identifies the exact source. The original text-only
build remains archived as a historical baseline.

## Original installed baseline: locally verified

- OpenAI and Anthropic text protocols: custom endpoint prefixes, models,
  protocol-specific auth/version, compatible responses and usage normalization.
- Loopback, independent local credentials, strict JSON/config validation,
  rejection of unsupported fields, bounded request/response sizes, no redirects.
- Explicit concurrency; already-canceled and queued-canceled requests do not
  start upstream work; successful queue events are observable.
- Atomic terminal audit/event writes; transport/read/HTTP/parse failures and
  cancellation are not recorded as successful. Audit failure is surfaced safely.
- Provider usage and explicit model pricing are tested without assuming a
  provider's current prices; when usage is absent, configured pricing enables a
  clearly marked local content estimate, while requests without a usable price
  remain unknown.
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
model. That original text-only build changed only the version marker and release
documentation relative to rc.4. The repaired CLI release has separate installed
workflow evidence described above.

## Public distribution

Release assets are published through [GitHub Releases](https://github.com/hs3180/tidemux/releases),
with the formula in [hs3180/homebrew-tap](https://github.com/hs3180/homebrew-tap).
Local source builds, archive extraction and local-tap installation have been
verified on macOS 15.7.4 arm64. Download-path verification is recorded separately;
a clean-machine installation is not claimed by these local tests.
See the [release procedure](releasing.md).
