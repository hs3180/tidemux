# TideMux

A macOS local gateway for user-configured **OpenAI-compatible or
Anthropic-compatible APIs**. Explicit concurrency, a SQLite audit ledger,
Keychain credentials, and safe errors in one Go binary.

**Local version: 0.1.0.** The repair is installed and verified with real
DeepSeek `deepseek-flash` workflows in Claude Code, Kilo CLI and Hermes.
Package checksums, SBOM, Homebrew install/rollback and CLI usage/price
reconciliation passed. Release acceptance covers the three CLIs; VS Code
extension compatibility is outside the 0.1.0 scope. GitHub publication
remains deferred. Other compatible services remain untested.

## Featured clients

TideMux provides CLI examples for **Claude Code, Kilo CLI, and Hermes Agent**,
covering terminal coding and agent automation.

| Client | TideMux connection | Client documentation |
| --- | --- | --- |
| Claude Code | Anthropic-compatible Messages gateway | [LLM gateway configuration](https://code.claude.com/docs/en/llm-gateway) |
| Kilo CLI | OpenAI-compatible custom provider | [AI providers](https://kilo.ai/docs/ai-providers) |
| Hermes Agent | OpenAI-compatible custom endpoint | [AI providers](https://hermes-agent.nousresearch.com/docs/integrations/providers/) |

**Integration status:** all three CLI launchers passed Keychain handoff,
model discovery, streaming, file reading/editing, test execution and persistent
conversation checks through the installed release. See [client setup](docs/clients.md) and
[compatibility](docs/client-compatibility.md). Each TideMux process uses one
upstream protocol; it does not convert between OpenAI and Anthropic formats.

## Try locally

With Go 1.27+ from this source root:

```sh
CGO_ENABLED=0 go build -o tidemux ./cmd/tidemux
./tidemux version
```

For DeepSeek Flash, run `./tidemux configure --preset deepseek`, paste your API
key at the hidden prompt, then run `./tidemux serve`. No manual Keychain work is
needed. If locked, the system asks for your keychain password in the same
terminal, then setup continues. See the [configuration guide](docs/configure.md).

With the gateway running, use `./tidemux connect kilo` or
`./tidemux connect hermes`. Claude uses `./tidemux connect claude` with a
separate Anthropic profile. See
[client setup](docs/clients.md) for prerequisites and complete commands.

Follow the [demo](docs/demo.md) for configuration and your first
request. Choose [OpenAI](examples/openai.json) or
[Anthropic](examples/anthropic.json), supplying your API root and model.
The built arm64 candidate is tested on macOS 15.7.4; other platforms are unverified.

## What it does

- Chat Completions or Messages, including streaming and tool messages, with a configurable API root.
- Loopback listener, independent local authentication, macOS Keychain references.
- Explicit in-flight concurrency cap and cancelable waiting; no automatic retries.
- Atomic terminal request/event records, including failures and cancellation.
- Nullable usage/cost, explicit per-model pricing snapshots and currency.
- `configure` (hidden key input and automatic Keychain setup), `serve`, `doctor`,
  `ledger` (upstream records or local diagnostics), `connect` (CLI client
  launcher), and `version` commands.

See the [exact protocol subset](docs/protocols.md). Multi-upstream routing,
fallback, protocol conversion, UI and automatic tuning are out of scope. No measured cost saving or latency benefit is claimed.

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
