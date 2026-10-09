# TideMux

**Connect Claude Code, Kilo CLI and Hermes Agent to your own LLM API.**

TideMux is a local macOS gateway for **OpenAI-compatible and Anthropic-compatible APIs**.

- **Streaming and tools** — forward conversations and tool calls to your chosen provider.
- **Simple client setup** — run `tidemux claude`, `tidemux kilo` or `tidemux hermes` with secure local credentials.
- **Concurrency control** — limit active requests and queue the rest.
- **Local usage ledger** — track outcomes, tokens and cost estimates without storing message bodies.
- **Usage logs** — write private [ccusage-compatible JSONL](docs/ccusage.md) by default for an external report reader.
- **Automatic reconciliation** — match locally supplied statement CSVs while the gateway runs; query statistics or download billing details with `tidemux billing`.
- **Daily reports** — generate private HTML reports, schedule macOS notifications, or send concise plain-text webhook summaries to IM platforms.

## Install

Requires **macOS 15+ on Apple Silicon** and [Homebrew](https://brew.sh).

```sh
brew install hs3180/tap/tidemux
```

If Homebrew asks you to trust the formula, run
`brew trust --formula hs3180/tap/tidemux`, then retry the install command.

The published stable release is v0.3.1. This source tree prepares 0.3.2 with
coordinated route reloads, independent provider session limits, opt-in ccusage
reports and client recovery guidance. A local candidate is available only after
building the reviewed source; the Homebrew command installs the published release.

[Build from source or install without Homebrew →](docs/install.md)

Upgrade with `brew upgrade tidemux`; uninstall with `brew uninstall tidemux`.

## Quick start

This walkthrough describes the **0.3.2 candidate CLI**.

Install your preferred client CLI. The example below uses **DeepSeek
`deepseek-flash`**; have your DeepSeek API key ready.

### 1. Add providers

Each provider has one upstream API protocol; TideMux supports both client
protocols and converts when they differ. Add the endpoint forms you want to
use, then select the provider explicitly in each request:

```sh
tidemux provider add https://api.deepseek.com
tidemux provider add https://api.deepseek.com/anthropic/v1
tidemux provider list
```

Each command prompts for its API key without echo. Provider references and
readable labels are generated from the endpoint; no provider name is required.
All models are allowed by default. During guided setup, choose model IDs only
if you want to restrict a provider; the selection can also be supplied as
`--model MODEL[,MODEL...]`. A bare model ID routes when exactly one provider's
configured scope matches it; use `REF/MODEL` to select a provider explicitly
when scopes overlap. `/v1/models` lists qualified IDs. If you add a provider
while the gateway is running, TideMux automatically validates and applies it. The CLI reports pending until the gateway acknowledges the saved configuration. To
configure just one protocol, add only its endpoint.

TideMux 0.3.0 also accepts Anthropic provider-hosted tools such as
`web_search_20250305` on Anthropic upstream routes. Configure an ordered model
chain with `tidemux auto-chain set --entries REF_A/MODEL_A,REF_B/MODEL_B`; then use
`model:auto`. Valid active session bindings take priority over connection reuse.
Removed, out-of-scope or unavailable provider/model pairs lose their bindings,
including when the client is unavailable or all keys are cooling down. Later
requests select again under the routing rules. Safely classified
model-not-found, insufficient-balance and temporarily-unavailable failures also
invalidate the affected route for later requests without immediately selecting
it again. The failed request is not replayed. `REF/auto` is rejected. Shared
bare model IDs remain ambiguous by default.
Those confirmed classifications also invalidate the binding when received
after SSE output has started: the current stream stays on its route, and the
next request, including one with the same session ID, selects again. Cancellation,
unclassified errors and transport errors after output do not themselves
invalidate the binding or advance the preference.
`tidemux routing set --shared-model-strategy random` or
`price_priority` opts into selection, and billing-exhaustion failover is a
separate opt-in that requires an exact `insufficient_balance` provider mapping.
With `random`, a stable `X-TideMux-Session-ID` (or Anthropic `metadata.user_id`
when the header is absent) pins each conversation to its first eligible
provider while that binding remains valid, for 24 hours of idle time. Endpoint,
key, protocol or API-version changes preserve healthy bindings when the same
provider/model remains eligible; later requests use the new configuration.
A new connection generation isolates prompt-cache history and clears old route
failure state. Bindings are kept in memory and reset on restart. Without a
stable ID, selection stays random for each request. Already-started SSE and
requests queued on an older configuration retain their original configuration
and settlement snapshot; an invalid reload keeps the last valid configuration. See the
[routing guide](docs/provider-cli.md#model-fallback-and-shared-model-routing).

### 2. Start the gateway

```sh
tidemux doctor
tidemux serve
```

`doctor` checks local configuration. Keep the gateway terminal open; stop it with **Ctrl-C**.
Next time, just run `tidemux serve` to reuse your configuration.

### 3. Launch your client

In another terminal, from your project directory, run the client you want.
The `--model` value explicitly selects both provider and upstream model:

```sh
# Anthropic API client
tidemux claude --model REF/deepseek-flash

# OpenAI API clients — choose one
tidemux kilo --model REF/deepseek-flash -- run 'Explain this project'
tidemux hermes --model REF/deepseek-flash -- -q 'Explain this project'
```

TideMux supplies the gateway URL, model and local credential to the client
without overwriting its existing settings.

Run `tidemux billing` for a readable summary of the current calendar month in
your computer's local timezone. It reads your existing local ledger from the
default TideMux configuration. Add `--details` to inspect request records or
`--json` for structured output. Cost estimates require a built-in or configured
[model price](docs/provider-cli.md#provider-pricing); unknown amounts stay unknown.
Use the [DeepSeek USD rates](docs/deepseek-pricing.md) for this example.
Reconciliation runs automatically with the gateway. Place normalized supplier
CSVs in the `statements` directory beside the ledger. Use
`tidemux billing --download billing.csv` to save that month's stored billing
details, or select a period with `--from` and `--to`. The summary shows its exact
RFC3339 date bounds. Use `tidemux doctor --diagnostics` to inspect local request
rejections. See [accounting](docs/accounting.md#billing-statistics-and-download)
for periods, synchronization status, statement coverage and the CSV format.
For current client launch commands and profile behavior, see the
[client setup guide](docs/clients.md). Agent-led installation for Claude Code,
Codex, Hermes and dsh is covered in the [agent installation guide](docs/agent-install.md).
The current installed-client matrix, error displays and artifact boundaries are
documented in [client compatibility](docs/client-compatibility.md).

## Compatibility

TideMux exposes OpenAI Chat Completions and Anthropic Messages client APIs
simultaneously. Each request selects a provider with a bare model ID only when
one configured model scope matches; otherwise specify `model: "REF/MODEL"`.
TideMux uses the provider's configured upstream protocol and strips `REF/`
before forwarding an explicitly qualified model. Provider selection is
independent of the client's protocol, so either client API can reach any
provider, with conversion only when required. There is no default provider or
model. An upstream 429 retries on the same key for up to three total attempts, honoring
`Retry-After`; if those attempts fail, TideMux returns a rate-limit error
without trying another key or provider. Eligible 401/403 and transport failures
before request headers are written can still fail over within the selected
provider's key group. TideMux stops retrying once response content may have
reached the client. Gateway authentication and ledger accounting remain shared.
An optional gateway-wide session ceiling coexists with independent provider
session limits. The current installed Claude Code, Kilo and Hermes workflows
were verified with isolated profiles and a local mock provider. The final 0.3.2
archive must pass the same gate; a development result does not certify it.
Other compatible providers can be configured, though they have not all been tested.

Responses API, audio and IDE extensions are outside this release's scope.
Anthropic images, documents, citations and server-tool blocks pass through on
native Anthropic routes but cannot be converted to OpenAI Chat Completions.
Provider-specific features without an equivalent in the selected protocol are
reported with a field-specific error. See the
[current tested-client matrix](docs/client-compatibility.md) for exact versions
and verification scope, its [historical evidence](docs/client-compatibility.md#historical-evidence)
section for the 0.1.1 release, and [protocol support](docs/protocols.md) for the
current routing contract.

## CLI design principles

The CLI is resource-oriented: a top-level noun identifies what is being
managed, and a subcommand states the action. Provider setup and lifecycle belong
under `tidemux provider`; the old top-level `tidemux configure` command is not
retained as a compatibility alias. Gateway-wide settings belong under
`tidemux gateway`. The commands below describe the 0.3.2 candidate CLI.

| Command | Semantics |
| --- | --- |
| `tidemux provider add [ENDPOINT] [--name LABEL] [--protocol PROTOCOL] [--model MODELS]` | Add exactly one provider without replacing others. `--model` is a comma-separated allowlist. With no endpoint, start guided setup and offer the initial daily-notification prompt; with an endpoint, infer protocol from bounded authenticated `/models` schema probes and allow all models unless `--model` is supplied. |
| `tidemux provider list [--json]` | List provider references, endpoint, protocol, key count, model scope and budget status. Never reveal credentials. |
| `tidemux provider show REF` | Show one provider's effective settings, including its budget, but not its API key. |
| `tidemux provider validate REF` | Check a provider's configuration and Keychain credentials locally without an upstream request or displaying key material. |
| `tidemux provider update REF [--endpoint URL] [--protocol PROTOCOL] [--model MODELS|all] [--max-active-sessions N] [--anthropic-version DATE] [--rotate-key]` | With no field options, open a short line-by-line form; otherwise change only the supplied fields. `--model` replaces the allowlist and `--model all` allows every model. From 0.3.2, the provider session cap applies automatically; zero disables it. |
| `tidemux provider key add REF` | Add another hidden-input API key to the selected provider profile. All keys share that profile's endpoint, protocol and model scope. |
| `tidemux provider key list REF` | List redacted key slots only; never reveal credentials or Keychain account details. |
| `tidemux provider key remove REF INDEX [--yes]` | Remove one key from the profile. A provider must retain at least one key; the Keychain item is deleted only when no remaining provider references it. |
| `tidemux provider remove REF [--yes]` | Remove only that provider. Confirm interactively; without a terminal require `--yes`. Delete a Keychain item only when no remaining provider references it. |
| `tidemux provider models REF [--only MODELS] [--all] [--select]` | With no scope option, query and display the provider's models when available. `--only` restricts allowed IDs; `--all` removes that restriction; `--select` uses a simple prompt to enter discovered or manual model IDs. The options are mutually exclusive. This is not a separate top-level `models` command. |
| `tidemux provider pricing list REF` | Show that provider's per-model rates and their source/version. |
| `tidemux provider pricing set REF MODEL --input-cache-hit RATE --input-cache-miss RATE --output RATE [other options]` | Set or replace all required per-million-token rates for one model; incomplete rate sets are rejected. |
| `tidemux provider pricing remove REF MODEL` | Remove that model's explicit rate. This does not change the provider or model scope. |
| `tidemux provider budget [REF] [options]` | Configure rolling spending limits for one provider. If exactly one provider exists, `REF` may be omitted. |
| `tidemux provider budget reset REF --window 5h (or 7d)` | Reset only that provider's selected rolling budget window. Stop the gateway before reset, then restart it; request audit and billing history remain intact. |
| `tidemux auto-chain show`, `set` or `clear` | Inspect, replace or clear the single instance chain used by `model:auto`; preserve valid session bindings and reselect invalid routes on later requests without replaying the failed request. |
| `tidemux routing show` / `tidemux routing set` | Inspect and opt into shared bare-model selection or exact billing-exhaustion cross-provider failover. Both routing settings default off. |
| `tidemux gateway configure [options]` | Set process-wide listener (`loopback` by default or `0.0.0.0`), gateway credential, request-concurrency and active-session settings. `0.0.0.0` requires a gateway API key. It does not add or modify upstream providers. |

Provider identity does not depend on a user-chosen name. TideMux creates and
prints a stable reference derived from the endpoint; `--name` optionally
overrides that reference. Multiple profiles may use the same endpoint (for
example, separate accounts); each `add` creates a distinct reference and never
silently updates or replaces an existing provider. Use `tidemux provider key
add REF` to attach additional credentials to one profile; they share that
profile's endpoint and model scope. TideMux detects protocol from bounded,
authenticated `/models` schema probes; endpoint names do not select a protocol.
If the response is ambiguous or unrecognized, set `--protocol
openai|anthropic` explicitly. API keys are always collected through hidden input, never
command-line arguments or configuration JSON; they are stored in macOS
Keychain. Without a terminal for that prompt, setup fails before changing
configuration. Model discovery uses
the authenticated model-list endpoint only; setup never sends a completion
request to select a model.

On a first guided setup, TideMux also asks whether to schedule a daily local-time
notification for usage reports. Leave it blank to skip. Set or change this later
with `tidemux report schedule --time HH:MM`; disable it with
`tidemux report schedule --disable`. Report text follows the user's preferred
system language; English is the fallback, and English, Simplified Chinese, and
Traditional Chinese are currently supported.

### Plain-text webhook reports

Configure a webhook endpoint with hidden terminal input; its URL is stored in
macOS Keychain, not in the profile or command history:

```sh
tidemux report webhook --provider lark
tidemux report schedule --time 09:00 --channel webhook
```

The supported adapters are `generic`, `telegram`, `discord` and `lark`; all
send a JSON-wrapped plain-text summary (up to 10 KB), never the HTML report or
request and response content. Generic and Telegram use a `text` field, Discord
uses `content`, and Lark uses `msg_type: text` with `content.text`. For Telegram
Bot API, include the bot `sendMessage` endpoint and `chat_id` in the hidden URL.
Run `tidemux report notify` to send the configured channel
immediately, or `tidemux report notify --channel webhook` to select it
explicitly. A webhook schedule can be disabled while retaining its endpoint
with `report schedule --disable`; `report webhook --disable` removes the
endpoint and disables a webhook schedule.

For a saved report, `report deliver --id N --channel webhook` sends it and
`report retry --id N --channel webhook` retries only a recorded failure.
`report deliveries --id N` shows each channel's latest status and attempt count.
Each retry updates the same report/channel ledger entry rather than creating a
second report record.

All model IDs are allowed by default. During guided setup, leave the model
selection blank to allow all, or select IDs to create an allowlist. The same
allowlist can be supplied with `provider add --model MODELS` or changed later
with `provider update --model MODELS` or `provider models REF --only ...`;
`provider update --model all` or `provider models REF --all` removes it.
Provider setup does not select a default model or provider. A bare model ID
routes only if exactly one provider's configured scope can serve it; use
`REF/MODEL` with the reference printed by `provider list` to select explicitly
when scopes overlap. TideMux removes the first `REF/` prefix from an explicit
route before forwarding the model ID. The gateway model catalog lists
qualified IDs.
`supported_models`, when configured, contains upstream model IDs without the
provider prefix. Configuration writes are validated and atomic; credentials
created for a provider addition are rolled back if its config write fails.

Provider-specific values (endpoint, credentials, protocol, model scope, prices
and spending budget) stay with the provider. Budget limits and accrued usage are
isolated per provider, even when providers use the same currency. Gateway-wide
values such as listen mode and the optional overall active-session ceiling apply
to the whole gateway. From 0.3.2, each provider can additionally set its own
logical-session cap with `provider update REF --max-active-sessions N`; see
[provider capacity](docs/provider-cli.md#provider-logical-session-capacity-032).
An automatically recognized built-in rate may be used when no explicit rate
exists; otherwise cost remains unknown rather than guessed. An enabled budget
requires a matching price for each requested model at request time. Report
scheduling remains under `tidemux report`.

Rates are scoped by provider and model. `provider pricing set` requires all
three per-million-token rate flags: `--input-cache-hit`, `--input-cache-miss`,
and `--output`; `--currency` defaults to USD, while `--source` and `--version`
identify the rate reference. Removing a custom rate reveals a matching built-in
rate if one exists; otherwise cost remains unknown. An enabled provider budget
requires a matching price for each requested model; without a budget, unpriced
models remain usable.

Typical resource operations look like this; `<ref>` is the stable reference
shown by `provider list`, not a name the user must invent:

```sh
tidemux provider add https://api.example.com/v1
tidemux provider list
tidemux provider models REF_FROM_LIST --only model-a,model-b
tidemux provider budget REF_FROM_LIST --budget-5h 5 --budget-weekly 80
tidemux gateway configure --listen loopback
tidemux claude --model REF_FROM_LIST/model-a
```

## Forward runtime logs to Elasticsearch

`tidemux serve` uses one Claude-compatible JSON Lines format for runtime logs,
including usage and TideMux diagnostic fields in the same record. It writes
complete terminal request records to session files beside the ledger and sends
the exact same JSON lines to stderr. Keep stderr separate from
human-readable `stdout` and let an external collector ship it; TideMux does not
connect to Elasticsearch:

```sh
tidemux serve --config /path/to/config.json \
  >> /var/log/tidemux/console.log 2>> /var/log/tidemux/runtime.jsonl
```

For example, point Filebeat at that `stderr` file and set its Elasticsearch
output. Replace the paths and endpoint:

```yaml
filebeat.inputs:
  - type: filestream
    id: tidemux-runtime
    paths: ["/var/log/tidemux/runtime.jsonl"]
    file_identity.native: ~
    parsers:
      - ndjson:
          target: tidemux
          add_error_key: true

processors:
  - timestamp:
      field: tidemux.timestamp
      layouts: ["2006-01-02T15:04:05.000Z"]
  - copy_fields:
      fields:
        - from: tidemux.event
          to: event.action
        - from: tidemux.level
          to: log.level

output.elasticsearch:
  hosts: ["https://elasticsearch.example:9200"]
  index: tidemux-runtime
  api_key: "${TIDEMUX_ES_API_KEY}"

setup.ilm.enabled: false
setup.template.enabled: false
```

Once, as the same user that runs Filebeat, create its keystore and store the
API key under the referenced name; then check the configuration:

```sh
filebeat keystore create
filebeat keystore add TIDEMUX_ES_API_KEY
filebeat test config
```

The `tidemux` namespace avoids conflicts between TideMux's `event` and Claude's
`message` object and ECS fields. The timestamp processor uses
`tidemux.timestamp` as `@timestamp`. Grant the collector read access
to the log and only the required write access in Elasticsearch. Use HTTPS with
a trusted CA. Install the [index template](examples/elasticsearch/index-template.json)
before the first event to index model IDs, request/session IDs and the four
usage fields for filtering and numeric aggregations. When moving from schema 1
to schema 2, update field paths and use a new index prefix with this template.
The complete [Filebeat](examples/elasticsearch/filebeat.yml) and
[Logstash](examples/elasticsearch/tidemux.conf) examples use the same fields.
Manage index lifecycle and retention in Elasticsearch. See
[runtime log fields and privacy](docs/runtime-logging.md) and the official
[Filebeat filestream](https://www.elastic.co/guide/en/beats/filebeat/current/filebeat-input-filestream.html)
and [Elasticsearch output](https://www.elastic.co/guide/en/beats/filebeat/current/elasticsearch-output.html)
references for collector details.

To collect the same session files ccusage reads, use
`/path/to/tidemux/logs/projects/tidemux/*.jsonl` as the collector path. Collect
either this path or runtime stderr for requests, not both copies. The stderr
stream additionally contains startup, shutdown and local rejection events.

## Documentation

[Provider and gateway CLI](docs/provider-cli.md) ·
[Client setup](docs/clients.md) ·
[Agent installation and setup](docs/agent-install.md) ·
[Runtime JSON logs](docs/runtime-logging.md) ·
[0.3.1 release guide](docs/release-0.3.1.md) ·
[0.2.2 release plan](docs/release-0.2.2.md) ·
[0.2.1 release history](docs/release-0.2.1.md) ·
[Accounting](docs/accounting.md) ·
[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) ·
[Privacy](PRIVACY.md) · [Security](SECURITY.md)

[Apache-2.0](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md) · [Brand reservation](NOTICE)
