# TideMux

A macOS local gateway for user-configured **OpenAI-compatible or
Anthropic-compatible APIs**. Explicit concurrency, a SQLite audit ledger,
Keychain credentials, and safe errors in one Go binary.

**Current source candidate: 0.1.0-rc.2.** Both protocol implementations pass local
mock and process tests. Live-provider reconciliation and public GitHub/Homebrew
installation have not been verified; this is not a released 0.1.0.

## Try locally

With Go 1.27+ from this source root:

```sh
CGO_ENABLED=0 go build -o tidemux ./cmd/tidemux
./tidemux version
```

Follow the [demo](docs/demo.md) for Keychain setup, configuration and your first
request. Choose [OpenAI](examples/openai.json) or
[Anthropic](examples/anthropic.json), supplying your API root and model.
The built arm64 candidate is tested on macOS 15.7.4; other platforms are unverified.

## What it does

- Non-streaming text Chat Completions or Messages, with configurable API root.
- Loopback listener, independent local authentication, macOS Keychain references.
- Explicit in-flight concurrency cap and cancelable waiting; no automatic retries.
- Atomic terminal request/event records, including failures and cancellation.
- Nullable usage/cost, explicit per-model pricing snapshots and currency.
- `serve`, `doctor`, `ledger` (recent records), and `version` commands.

See the [exact protocol subset](docs/protocols.md). Multi-upstream routing,
fallback, protocol conversion, streaming, tools, UI and automatic tuning are out
of scope. No measured cost saving or latency benefit is claimed.

## Verify and package

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
go test -race ./...
```

The process test builds a binary, uses a temporary Keychain double, calls both
mock upstream protocols, verifies stored usage/cost and stops the process.
See [acceptance](docs/mvp-acceptance.md) and [release procedure](docs/releasing.md).

## Source and policies

`cmd/tidemux` provides the CLI; `internal/gateway`, `adapter`, `limiter`, and
`ledger` handle protocol boundaries, upstream calls, concurrency and audit storage.

[Apache-2.0](LICENSE) source; [brand reservation](NOTICE);
[third-party notices](THIRD_PARTY_NOTICES.md); [SBOM](SBOM.md);
[privacy](PRIVACY.md); [security](SECURITY.md);
[contribution guide](CONTRIBUTING.md); [conduct](CODE_OF_CONDUCT.md);
[changelog](CHANGELOG.md).
