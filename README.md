# TideMux

A local API gateway for macOS: explicit concurrency control, a SQLite usage
ledger, and structured errors. The 0.1.0 target supports a user-configured
OpenAI-compatible or Anthropic-compatible upstream, with no provider-brand lock-in.

**Source candidate: 0.1.0-rc.1.** No public binary or Homebrew tap has been
released. The current implementation is still DeepSeek-specific. Generic
OpenAI and Anthropic adapters are planned, not implemented; local tests do not
establish support for both protocols.

## 0.1.0 target

One configured upstream per running instance, with a selectable protocol,
base URL, model, and Keychain credential reference. OpenAI Chat Completions
and Anthropic Messages will each support a documented non-streaming text
subset, matching local and upstream protocols. Cross-protocol conversion,
multi-upstream routing, and fallback are outside this release.

“Compatible” means conforming to that supported subset, not every endpoint or
vendor extension. Both protocol paths must pass separate tests and live usage
reconciliation before release.

## Supported today (DeepSeek source baseline)

- Minimal non-streaming `POST /v1/chat/completions` with text messages.
- Loopback listener and an independent local Bearer token.
- macOS Keychain references for credentials; no plaintext credential config.
- Explicit maximum in-flight requests and cancelable waiting.
- Local request / event records and internal aggregate queries.
- `tidemux serve`, `tidemux doctor`, and `tidemux version`.

This candidate has known ledger and pricing limitations; read the
[acceptance status](docs/mvp-acceptance.md) before relying on its records.
It does not yet implement generic OpenAI or Anthropic support. Concurrent
multi-upstream routing, fallback, streaming, tool calls,
a GUI, automatic tuning, and full API/client compatibility remain outside the
release scope. It makes no
verified cost-saving or latency-improvement claim.

## Build and try

Use macOS and a Go toolchain satisfying [go.mod](go.mod). From this source root:

```sh
CGO_ENABLED=0 go build -o tidemux ./cmd/tidemux
./tidemux version
```

Follow the [demo](docs/demo.md) to create the local state directory, store
both credentials in Keychain, and configure `doctor` / `serve`. `doctor`
checks local readiness; it does not test the upstream provider.

## Verify

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
```

Tests use mock upstreams and a temporary Keychain double, not a real API key.

## Source map

`cmd/tidemux` provides the CLI. `internal/gateway`, `adapter`, `limiter`, and
`ledger` implement the HTTP boundary, DeepSeek call, concurrency gate, and
SQLite records respectively.

## Privacy, license, and contribution

Credentials stay in Keychain; requests are sent to the configured upstream.
No telemetry or full prompt/response persistence is implemented. See
[Privacy](PRIVACY.md) and [Security](SECURITY.md).

Source is [Apache-2.0](LICENSE); brand rights are reserved in [NOTICE](NOTICE).
Dependency materials are tracked in [Third-party notices](THIRD_PARTY_NOTICES.md)
and [SBOM](SBOM.md). For changes, see [Contributing](CONTRIBUTING.md),
[Code of conduct](CODE_OF_CONDUCT.md), and [Changelog](CHANGELOG.md).
