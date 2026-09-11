# TideMux

A macOS local gateway for user-configured **OpenAI-compatible or
Anthropic-compatible APIs**. Explicit concurrency, a SQLite audit ledger,
Keychain credentials, and safe errors in one Go binary.

**Version: 0.1.0 repair candidate.** Real DeepSeek `deepseek-flash` workflows
pass for Claude Code, Kilo CLI/VS Code and Hermes in the development build.
The previously installed 0.1.0 remains the text-only baseline; repaired-package
installation and remaining IDE fault acceptance are in progress. GitHub
publication is deferred. Other compatible services remain untested.

## Featured clients

TideMux's client examples will focus on **Claude Code, Kilo Code, and Hermes
Agent**, covering terminal coding, IDE workflows, and general agent automation.

| Client | TideMux connection | Client documentation |
| --- | --- | --- |
| Claude Code | Anthropic-compatible Messages gateway | [LLM gateway configuration](https://code.claude.com/docs/en/llm-gateway) |
| Kilo Code | OpenAI-compatible custom provider | [AI providers](https://kilo.ai/docs/ai-providers) |
| Hermes Agent | OpenAI-compatible custom endpoint | [AI providers](https://hermes-agent.nousresearch.com/docs/integrations/providers/) |

**Integration status:** the installed 0.1.0 baseline rejected the three CLI
workflows. The current development source adds streaming, tool calls and a
[secure client launcher](docs/clients.md); real file-edit/test workflows have
passed for the three CLIs with isolated test profiles. Product launchers and the Kilo VS Code read/edit/test and restart workflows
also pass; IDE fault and repaired-package acceptance remain open. See the
[client compatibility report](docs/client-compatibility.md) for tested versions,
observed blockers and the acceptance gate. Today, use the [HTTP demo](docs/demo.md) to try the verified
protocol subset. Each TideMux process uses one upstream protocol; it does not
convert between OpenAI and Anthropic formats.

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

With the gateway running, use `./tidemux connect kilo-ide -- /path/to/project`
for the Kilo VS Code extension, or `connect kilo` / `connect hermes` for their
CLIs. Claude uses `connect claude` with a separate Anthropic profile. See
[client setup](docs/clients.md) for prerequisites and complete commands.

Follow the [demo](docs/demo.md) for configuration and your first
request. Choose [OpenAI](examples/openai.json) or
[Anthropic](examples/anthropic.json), supplying your API root and model.
The built arm64 candidate is tested on macOS 15.7.4; other platforms are unverified.

## What it does

- Chat Completions or Messages, including streaming and tool messages in the
  development source, with a configurable API root.
- Loopback listener, independent local authentication, macOS Keychain references.
- Explicit in-flight concurrency cap and cancelable waiting; no automatic retries.
- Atomic terminal request/event records, including failures and cancellation.
- Nullable usage/cost, explicit per-model pricing snapshots and currency.
- `configure` (hidden key input and automatic Keychain setup), `serve`, `doctor`,
  `ledger` (upstream records or local diagnostics), `connect` (development client
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
