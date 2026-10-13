# User guide

Start with the [README](../README.md#first-request). Use `tidemux --help` and
`tidemux <command> --help` for all options and defaults. Pass `--config PATH`
to manage another profile; the gateway and its clients must use the same file.

[Installation](#installation) · [Providers](#providers) · [Clients](#clients) ·
[Routing](#routing-and-limits) · [Health](#health) · [Billing](#billing-and-budgets) ·
[Logs](#logs-and-collection) · [Troubleshooting](#troubleshooting)

## Installation

Homebrew commands are in the [README](../README.md#install).
For a standalone installation, download and inspect the version-pinned script:

```sh
curl -fsSL https://raw.githubusercontent.com/hs3180/tidemux/v0.3.3/scripts/install.sh -o /tmp/tidemux-install.sh
less /tmp/tidemux-install.sh
TIDEMUX_VERSION=0.3.3 sh /tmp/tidemux-install.sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer verifies the archive checksum and version, then installs only the
binary. `TIDEMUX_INSTALL_DIR` selects another directory. Use Homebrew to manage
an existing Homebrew installation. Stop the gateway before uninstalling;
standalone users remove only their installed `tidemux` executable.
Configuration, Keychain entries, ledger, logs, statements and reports are
retained for reinstallation. Source builds are in [CONTRIBUTING](https://github.com/hs3180/tidemux/blob/main/CONTRIBUTING.md#development).

## Providers

Each provider reference groups one API root, protocol, model scope and key pool.
`provider add` prompts securely for a key and probes `/models` to detect the
protocol. If detection is ambiguous, specify `--protocol openai` or
`--protocol anthropic`. Discovery never sends a completion.

```sh
tidemux provider add https://api.example.com/v1 --model MODEL_ID
tidemux provider show REF
tidemux provider update REF
tidemux provider models REF --only MODEL_A,MODEL_B
tidemux provider key add REF
tidemux provider key list REF
tidemux provider validate REF
```

Omit `--model` to allow all models, or use `provider models REF --all` to clear
the allowlist. Keep upstream keys in the hidden prompt, including when an agent
helps with setup. Configuration and client profiles contain references, not
provider secrets. `provider remove` and `provider key remove` ask for confirmation.

A running gateway applies valid provider/routing edits automatically; invalid
edits leave the last valid view active. Wait for **applied**, rather than only
**saved**, before using a change. Admitted requests retain their original view.
Listener, ledger and global-limit changes require a gateway restart.

## Clients

Launch commands are in the [README](../README.md#first-request). They read the
local gateway credential, check its authenticated model endpoint and keep
persistent profiles in `client-state/` beside the selected configuration.
They preserve the clients' original profiles and never pass upstream keys.
Use `--executable PATH` before `--` when a client is outside `PATH`; arguments
after `--` belong to that client. Hermes's generated configuration is managed
by TideMux and regenerated on launch.

For manual setup, select Anthropic Messages for Claude, or Chat Completions for
Kilo/Hermes, and use the gateway's address, local key and chosen model.
Hermes requires a custom `chat_completions` provider. Responses-only clients
cannot use this gateway as their model provider. Coding agents can still follow
this guide to install and configure TideMux.

## Routing and limits

| Model | Selection |
| --- | --- |
| `REF/MODEL_ID` | Explicit provider; only the first reference prefix is removed. |
| `MODEL_ID` | Unique matching scope, or an enabled shared-model strategy. |
| `auto` | The configured ordered provider/model chain; `REF/auto` is invalid. |

```sh
tidemux routing set --shared-model-strategy random
tidemux auto-chain set --entries REF_A/MODEL_A,REF_B/MODEL_B
tidemux provider update REF --max-active-sessions 5
tidemux gateway configure --max-active-sessions 20
```

Shared selection is off by default. `random` retains a healthy binding when
the caller supplies a stable `X-TideMux-Session-ID` (or Anthropic
`metadata.user_id`); without one, selection is per request. `price_priority`
chooses the lowest sum of the three token rates and requires comparable prices
in one currency. It has no session affinity. Shared-model launchers preserve
the bare ID and omit any one candidate's context/output-limit declarations.
Use `--shared-model-strategy off` to restore explicit ambiguity checks.

Auto-chain failures affect later eligible requests; the dispatched request is
not replayed. After dispatch, TideMux does not wait for a retry, retry a 429,
switch keys, fail over to another provider, or change the requested model. It
returns the mapped result and a valid upstream `Retry-After` to the caller; the
client owns retry timing and sends a new request. The legacy
`billing_exhaustion_failover` setting is ignored. Configure error mappings with
`provider error-map --help`.

Global upstream request concurrency is unlimited in new configurations.
Use `tidemux gateway configure --max-in-flight 0` to disable the limit in an
existing configuration, then restart the gateway. Existing positive values
still cap simultaneous upstream requests across all providers; this setting
does not hot reload.

Session ceilings count conversations independently of request concurrency.
A full session ceiling returns `active_session_limit` before dispatch.
Reducing a provider cap does not cancel admitted work. Listener mode defaults
to authenticated loopback; `0.0.0.0` exposes plain HTTP on the LAN and needs
network protection. Use `gateway configure --help` for global settings.

## Health

```sh
tidemux gateway availability
tidemux gateway availability --json
tidemux gateway configure --health-diagnostics=false
```

The authenticated query sends no upstream requests. `unknown` has no retained
inference evidence; `available` records success; `cooling` remembers a failure;
`probing` has one real request testing recovery. `retry_at` is eligibility to
try again. Without traffic, recovery stays unconfirmed.

Key labels are opaque. `requests` counts adapter calls using a key;
`http_attempts` counts actual HTTP attempts. Each downstream generation request
initiates at most one such attempt.
Success, failure, cancellation, timeout and in-flight counts are separate.
Skipped keys and local rejections add no attempts. These counters are not billing.
Compatible reloads preserve them; credential/connection changes or restart reset
the pool. Toggling diagnostics clears counters/history while preserving routing
and cooldowns. See [recovery bounds](design.md#routing-and-recovery).

## Billing and budgets

```sh
tidemux billing
tidemux billing --details --json
tidemux billing --download /path/to/new-billing.csv
tidemux provider pricing set REF MODEL_ID --input-cache-hit 0.10 --input-cache-miss 1.00 --output 2.00 --currency USD
tidemux provider budget REF --budget-5h 5 --budget-weekly 80 --budget-currency USD --budget-mode hard
```

The rates above are examples in currency units per million tokens. Billing
uses the current calendar month in local time; use RFC3339 `--from`/`--to`
bounds for another period. Downloads must use a new path. Costs are estimates
from reported usage and a price snapshot, or a marked local content estimate.
Missing usage/cost stays unknown; currencies are never added together.

Budgets are per provider over rolling five-hour/seven-day windows and require
matching prices. `alert` warns, `soft` requires `X-TideMux-Budget-Confirm: 1`,
and `hard` refuses over-budget work before dispatch. Unresolved attempted
charges block later budgeted work. To reset a window, stop the gateway, run
`provider budget reset REF --window 5h` or `7d`, then restart; audit history stays.

The gateway imports normalized CSVs from `statements/` beside the ledger.
Required columns are `period_start,period_end,currency,amount`; optional
`request_id,model` identify matches. Use RFC3339 periods and nonnegative amounts.
Move complete, immutable, non-overlapping files into that directory. Rewriting
an imported file is rejected. TideMux does not fetch invoices or guess missing matches.

For reports, use `report export --days 30`, `report open`, or
`report schedule --time 09:00`. `report webhook --provider lark` securely configures
an optional destination; select it with `--channel webhook`. Webhooks receive a
content-free summary. CLI help covers delivery/retry and other adapters.

## Logs and collection

Runtime stderr and terminal-request files use the same schema-2 JSONL records.
Session files live at `logs/projects/tidemux/*.jsonl` beside the ledger.
Each event has `timestamp`, `event`, `service` (name/version/process instance)
and a unique persisted `event_id`. Requests also have `requestId`, outcome,
model and known usage. Bodies, credentials, raw sessions and provider errors
are omitted; [privacy](../PRIVACY.md) covers retained metadata.

Point an independently installed [ccusage](https://github.com/ccusage/ccusage)
at the `logs/` directory:

```sh
CLAUDE_CONFIG_DIR="/path/to/tidemux/logs" ccusage claude daily
CLAUDE_CONFIG_DIR="/path/to/tidemux/logs" ccusage claude session
```

Unknown required usage is excluded. ccusage uses its own prices;
`tidemux billing` remains the local accounting view. Choose one source for each
request; combining client logs, stderr and session copies duplicates usage.

For Elasticsearch, decode under `tidemux`, use event time for daily indices and
`event_id` for document IDs. A minimal Filebeat input/output is:

```yaml
filebeat.inputs:
  - type: filestream
    id: tidemux-runtime
    paths: ["/path/to/runtime.jsonl"]
    file_identity.native: ~
    parsers:
      - ndjson: {target: tidemux, add_error_key: true}
processors:
  - timestamp:
      field: tidemux.timestamp
      layouts: ["2006-01-02T15:04:05.000Z"]
  - copy_fields:
      fields: [{from: tidemux.event_id, to: "@metadata._id"}]
      fail_on_error: true
      ignore_missing: false
output.elasticsearch:
  hosts: ["https://elasticsearch.example:9200"]
  api_key: "${TIDEMUX_ES_API_KEY}"
  ssl.certificate_authorities: ["/path/to/ca.pem"]
  index: "tidemux-runtime-%{+yyyy.MM.dd}"
setup.ilm.enabled: false
setup.template.enabled: false
```

Keep the collector key in its keystore and preserve registry state. Install a
mapping before ingestion: IDs/models/states/service fields as `keyword`, token
and timing fields as `long` without coercion, timestamps as `date`, flags as
`boolean`. Query `tidemux.message.model` and `tidemux.message.usage.*`; missing
usage is absent, and input plus cache read plus cache creation gives total input.

For Logstash, decode `message` under `tidemux`, parse `tidemux.timestamp` and set
`document_id => "%{[tidemux][event_id]}"` for valid records. Route parse errors
and missing IDs to a separate failure output; enable DLQ for mapping failures.
Same-event replay deduplicates within the same index. Earlier records without
IDs need a separate policy. Deployment and retention follow the official
[Filebeat](https://www.elastic.co/docs/reference/beats/filebeat/filebeat-input-filestream)
and [Logstash](https://www.elastic.co/docs/reference/logstash/plugins/plugins-outputs-elasticsearch)
references. TideMux does not ship logs itself.

## Compatibility

Matching client/upstream protocols pass through natively. Conversion supports
representable text, tools/results, usage and streaming. Provider-hosted Anthropic
tools, thinking and context-management controls require an Anthropic upstream;
unsupported request features fail before dispatch. Native blocks/history remain
provider-owned. TideMux carries tool calls and never executes client tools.
Responses API, audio, embeddings and IDE integrations are outside the supported
interface. Full interactive/dsh/Codex workflows are outside release acceptance.

## Troubleshooting

Run `tidemux doctor`, `tidemux gateway check` and `tidemux doctor --diagnostics`
first. Correlate failures with the JSON event's request ID and safe error code.

| Failure | Next step |
| --- | --- |
| `invalid_arguments` / `config_load_failed` | Check `serve --help`, the selected config and its readable, valid JSON. |
| `credential_resolution_failed` | Unlock Keychain and check the selected provider/gateway references; never paste keys into reports. |
| `provider_initialization_failed` / `provider_unavailable` | Check the API root; use `provider update REF --protocol openai` or `anthropic` when discovery is ambiguous. |
| `ledger_initialization_failed` | Check permissions/integrity and stop another gateway using the ledger. Preserve the database and live lock sidecar. |
| `listener_bind_failed` | Check listener address and port occupancy. |
| `runtime_identity_failed` | Check OS randomness and restart; this early failure intentionally has no JSON fallback. |
| `active_session_limit` | Wait for active work and retained sessions to release, then retry with bounded backoff. A new conversation does not free another slot. |
| Upstream stream/response error | Check the `request_terminal` event's `outcome` and `error_code`; HTTP 200 can still end in failure. |

Partial output is not a completed turn. TideMux never replays output that may
have reached the client; client retries create new requests. After correcting
a malformed response, use the client's new-conversation mode without
`--continue`/`--resume`, then resume only a successful history. Share timestamps,
request IDs and safe codes, after checking the metadata for private information.
