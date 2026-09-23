# Release acceptance — 0.1.1

## Candidate status

The 0.1.1 release combines the merged budget, reconciliation and daily-report
features. It includes both OpenAI and Anthropic client-boundary streaming,
provider translation, tools, CLI launchers, configurable
limits, model discovery, local diagnostics, usage reports and macOS scheduled
notifications. SMTP and webhook delivery are intentionally deferred.

The final acceptance run covers source tests, packaged arm64 artifacts, installer
behavior, client handoff, onboarding PTY flows, report generation/delivery and
dependency notices. Claude Code, Kilo CLI and Hermes passed installed file
read/edit/test workflows and continuation in a new client process. See [client
acceptance](client-compatibility.md) for tested versions and protocol boundaries.
VS Code extension compatibility is out of scope.

Package checksums/SBOM, Homebrew install/rollback, configuration/data compatibility,
and installed CLI usage/price reconciliation are release gates. Final documentation
changes do not alter the tested runtime. BUILD.txt identifies the exact source
commit and build metadata for every artifact.

## Automated release gates

The final candidate is checked with:

- `gofmt -l cmd internal` returning no files;
- `go test ./...`, `CGO_ENABLED=0 go test ./...`, `go vet ./...` and
  `go test -race ./...`;
- `python3 scripts/test_install.py`, `python3 scripts/test_client_commands.py`,
  `python3 scripts/test_provider_add_pty.py` and `python3 scripts/licenses.py`;
- `python3 scripts/release.py`, checksum verification and packaged-binary
  version/installation smoke checks.

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
- Guided provider add: PTY covers automatic unlock, the daily report notification
  time prompt, no unnecessary prompt, wrong-password/cancellation stop, hidden API
  key input and no plaintext config; rollback is tested. Real isolated Keychain
  writes/read-back preserve existing items.
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
