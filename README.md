# TideMux

Connect Claude Code, Kilo CLI and Hermes Agent to your own LLM API.
TideMux is a local macOS gateway serving OpenAI Chat Completions and Anthropic
Messages. It forwards streams and tools, manages routing and capacity, and
keeps a private usage ledger with explicit cost estimates.

Provider/model state and recovery: [availability guide](docs/availability.md).

## Install

Requires macOS 15+ on Apple Silicon. Install each client CLI separately.

```sh
brew install hs3180/tap/tidemux
```

If Homebrew requests formula trust, run
`brew trust --formula hs3180/tap/tidemux` and retry. Homebrew installs the
published stable release. See [installation](docs/install.md) for a pinned
standalone installer, source builds and data-preserving uninstall.

## Quick start

Add your provider's API root. Enter its key through the hidden terminal prompt;
credentials are stored in macOS Keychain. Supply an upstream model ID to restrict
scope, or omit `--model` to allow all models from that provider.

```sh
tidemux provider add https://api.example.com/v1 --model MODEL_ID
tidemux provider list
tidemux doctor
tidemux serve
```

Keep `serve` running. In another terminal, choose a client and use the reference
printed by `provider list`:

```sh
tidemux claude --model REF/MODEL_ID
tidemux kilo --model REF/MODEL_ID -- run 'Explain this project'
tidemux hermes --model REF/MODEL_ID -- -q 'Explain this project'
```

A bare `MODEL_ID` selects a provider when exactly one configured scope matches.
Use `REF/MODEL_ID` to select explicitly when scopes overlap. The client's API
protocol does not choose the provider; TideMux converts when the selected
upstream uses the other supported protocol. Launchers supply the local gateway
credential and keep isolated persistent client profiles. See
[client setup and recovery](docs/clients.md) and the
[provider/gateway CLI](docs/provider-cli.md).

## Routing and diagnostics

Provider and routing edits are validated and applied by the running gateway.
Invalid edits leave the last valid view active; admitted requests keep their
configuration, credentials, prices and accounting snapshot.

Configure `model: auto` with `tidemux auto-chain set --entries
REF_A/MODEL_A,REF_B/MODEL_B`. Shared bare-model routing can opt into `random` or
`price_priority`; billing-exhaustion failover has a separate opt-in. Global and
provider session ceilings coexist with request-concurrency limits.

```sh
tidemux gateway check
tidemux gateway availability
tidemux gateway availability --json
tidemux doctor --diagnostics
```

[Availability](docs/availability.md) explains provider/model failure scope,
bounded request-driven recovery, safe key labels, counters and cooldowns.
Diagnostics contain no credentials or message bodies. A status query sends no
upstream requests. Responses already delivered through SSE are never replayed.

## Usage and logs

`tidemux billing` summarizes the current calendar month in local time. Add
`--details`, `--json` or `--download billing.csv`; explicit prices determine
estimates, and missing usage/cost stays unknown. Normalized statement CSVs are
reconciled automatically. Reports support private HTML, macOS notifications and
configured plain-text webhooks. See [accounting](docs/accounting.md).

Runtime stderr and terminal-request session files use one
[Claude-compatible JSONL format](docs/runtime-logging.md). Each event carries
service version, process identity and a persisted unique `event_id`. Choose one
collection source; use the event ID for replay deduplication. See
[official ccusage integration](docs/ccusage.md) and
[Elasticsearch collection](docs/elasticsearch.md).

## Compatibility

Native Anthropic routes preserve provider-hosted tool and content blocks.
Cross-protocol conversion is limited to features with a representation in the
target API. Responses API, audio, embeddings and IDE extensions are outside the
supported interface. [Protocol support](docs/protocols.md) and
[client compatibility](docs/client-compatibility.md) describe these boundaries.
[Agent-led setup](docs/agent-install.md) covers secure installation and client
configuration; Codex can assist installation but cannot directly use this
Chat Completions gateway as its Responses provider.

Read [core design](docs/design.md), [privacy](PRIVACY.md),
[security reporting](SECURITY.md), [changelog](CHANGELOG.md) and
[license](LICENSE). Development and validation methods live in
[CONTRIBUTING](https://github.com/hs3180/tidemux/blob/main/CONTRIBUTING.md) and
[release procedure](docs/releasing.md). Execution evidence stays in issue/PR
attachments or a private workspace outside the source and release archive.
