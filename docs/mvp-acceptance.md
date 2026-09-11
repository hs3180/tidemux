# Candidate acceptance — 0.1.0-rc.2

## Locally verified

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
- Built CLI process tests for both protocols, temporary Keychain double,
  HTTP request, ledger usage/cost query and clean shutdown.
- `CGO_ENABLED=0 go test ./...`, `go vet ./...`, and `go test -race ./...` pass on
  Go 1.27.1 / macOS 15.7.4 arm64.

## Still required for public 0.1.0

- Real Keychain entries and one authorized live call for each protocol, with
  usage and price arithmetic reconciled and sanitized evidence retained.
- Authenticated GitHub repository and release publication; verified private
  security/community report channel.
- The generated artifact's Homebrew formula installed and exercised on a clean
  macOS machine with a real configured endpoint. Local extraction/mock testing
  does not establish this result.

The source candidate is deliberately not relabeled 0.1.0 until these conditions
are met. [Release procedure](releasing.md) specifies the remaining steps.
