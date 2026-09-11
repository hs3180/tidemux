# Candidate acceptance status — 0.1.0-rc.1

This is a source candidate, not a public release. Existing local tests establish
a baseline; they do not establish complete audit coverage or live-provider readiness.

| Area | Evidence / status |
| --- | --- |
| Loopback and authentication | Gateway tests cover non-loopback rejection and Bearer authentication. |
| Credentials | Config tests cover Keychain references, non-serialization of runtime secrets, and rejection of legacy plaintext fields. |
| Minimal chat | DeepSeek HTTP tests verify non-streaming forwarding and completion response. |
| Generic protocols | Pending: configurable OpenAI Chat Completions and Anthropic Messages adapters; current tests do not prove this release requirement. |
| Concurrency | Tests cover admission, waiting, cancellation, and successful queue records; cancellation edge cases remain to be reviewed. |
| Structured failures | Tests cover validation, cancellation, 429, and upstream HTTP failures without returning upstream details. |
| CLI | A fresh binary is built and `doctor` is run with a temporary Keychain double. |
| Ledger correctness | Incomplete: transport/read failures can bypass recording; malformed successful HTTP responses can be recorded as successful before validation. |
| Pricing | Incomplete: gateway does not supply pricing parameters; zero-valued estimates are not reliable real costs. |
| Live request / installation | Pending; no public Release or Homebrew installation evidence. |

## Reproduce the baseline

From the source root on macOS with the Go toolchain required by `go.mod`:

```sh
gofmt -l cmd internal
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build -o tidemux ./cmd/tidemux
./tidemux version
```

Formatting output must be empty. Expected source version: `0.1.0-rc.1`.
Tests use no real provider key. The CLI smoke validates `doctor`, not a full
live `serve` session.

## Required before a public MVP

- User-configurable OpenAI-compatible / Anthropic-compatible upstreams: base
  URL, model, Keychain references, protocol-specific authentication and version
  handling; matching local and upstream protocols without cross-conversion.
- Separate success, error, cancellation, usage, custom-endpoint, and process
  tests for both protocols. No hard-coded provider/model whitelist.
- A documented supported non-streaming text subset and examples for each protocol.

- Correct terminal ledger states for success, upstream failures, malformed
  responses, and cancellation; defined request/event consistency on write failure.
- Explicit, traceable pricing and unknown-cost semantics.
- Protocol, credential, cancellation, and process-level regression checks.
- At least one live request per protocol using macOS Keychain, with upstream /
  model identity, usage, and estimated-cost reconciliation.
- A committed release tag, validated build artifacts, checksums, dependency
  notices / SBOM, a working private security contact, and clean macOS installation.

Update this status only when corresponding evidence exists. The minimal protocol
and absence of streaming, tool calls, fallback, and UI remain intentional limits.
