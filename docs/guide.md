# User guide

This guide owns setup, commands, configuration examples, observable interfaces
and troubleshooting. The [core design](design.md) owns runtime semantics,
invariants and policy bounds; link to its contracts instead of restating them.
CLI help owns option syntax, and [CONTRIBUTING](https://github.com/hs3180/tidemux/blob/main/CONTRIBUTING.md)
owns validation and releases.

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
curl -fsSL https://raw.githubusercontent.com/hs3180/tidemux/v0.3.5/scripts/install.sh -o /tmp/tidemux-install.sh
less /tmp/tidemux-install.sh
TIDEMUX_VERSION=0.3.5 sh /tmp/tidemux-install.sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer verifies the archive checksum and version, then installs only the
binary. `TIDEMUX_INSTALL_DIR` selects another directory. Use Homebrew to manage
an existing Homebrew installation. Stop the gateway before uninstalling;
standalone users remove only their installed `tidemux` executable.
Configuration, Keychain entries, ledger, logs, statements and reports are
retained for reinstallation. Source builds are in [CONTRIBUTING](https://github.com/hs3180/tidemux/blob/main/CONTRIBUTING.md#development).

### Agent-assisted setup

Use the same workflow with any coding agent. For example:

> Install or update TideMux from the latest stable release. Configure my chosen
> provider URL, protocol and model scope using the TideMux CLI. Let me enter
> credentials in Terminal, preserve other settings, and verify the local gateway.

1. Check `uname -s`, `uname -m`, `sw_vers -productVersion` and, when installed,
   `tidemux version`. Check the [latest stable release](https://github.com/hs3180/tidemux/releases/latest)
   and use its version-matched README, guide and installer. Pin standalone
   downloads to that release tag; use Homebrew for an existing Homebrew install.
2. Inspect `tidemux provider list` and `tidemux provider show REF` before editing.
   Reuse an existing matching provider with `provider update REF`; use
   `provider add` for a new one. Apply only the requested provider/model scope.
   Rerunning the workflow must preserve unrelated providers and settings.
3. Hand hidden credential prompts to the user in Terminal. Never ask for keys in
   chat or put them in arguments, transcripts or generated files. Use the CLI's
   Keychain workflow. Ask the user to choose any unspecified provider/model and
   before replacing configuration or enabling a non-loopback listener.
4. Run `tidemux doctor`, inspect the provider/model scope and run
   `tidemux gateway check` after the local gateway starts. Use `--config PATH`
   consistently for another profile. Report actionable errors without secrets,
   then summarize the installed version, endpoint, selected model and next step.

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
For process-wide changes that need a restart, see the
[configuration contract](design.md#configuration-and-requests).

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

When upgrading from 0.3.3 or earlier, remove `routing.billing_exhaustion_failover` from an old
configuration, including a value of `false`. The field is no longer accepted.
Recovery uses a new client request; see the [single-dispatch contract](design.md#single-dispatch).

| Model | Selection |
| --- | --- |
| `REF/MODEL_ID` | Explicit provider; only the first reference prefix is removed. |
| `MODEL_ID` | Unique matching scope, or an enabled shared-model strategy. |
| `auto` | The configured ordered provider/model chain; `REF/auto` is invalid. |

```sh
tidemux routing set --shared-model-strategy random
tidemux auto-chain set --entries REF_A/MODEL_A,REF_B/MODEL_B
tidemux provider update REF --max-active-sessions 5
```

Use `random` or `price_priority` for shared selection, or
`--shared-model-strategy off` to restore explicit ambiguity checks. For a stable
conversation, supply `X-TideMux-Session-ID` (or Anthropic `metadata.user_id`).
The [route-selection contract](design.md#route-selection) defines eligibility,
affinity and comparable pricing; the [switching contract](design.md#cooldown-switching)
defines subsequent recovery choices.

Inspect `/v1/models`, `/models` or a model-detail response for each qualified
model's known `context_length` and `max_output_tokens`. Startup warnings
`model_capabilities_unknown`, `model_capabilities_invalid` and
`model_capabilities_conflict` identify metadata to investigate. The
[catalog contract](design.md#model-capability-catalog) defines refresh, override
precedence, unknown values and launcher propagation. Verify limits with the
provider before configuring an override; use separate provider scopes for models
that need different overrides.

Set a verified provider-wide context override with
`tidemux provider update REF --context-tokens 1000000` (one million tokens).
`tidemux provider show REF` displays the configured override. Set it to `0` to
clear the override without changing the configured output limit or other
provider settings. Confirm the saved configuration is applied; effective limits
follow the [catalog contract](design.md#model-capability-catalog).

Clients own recovery requests; see the [dispatch contract](design.md#single-dispatch).
Configure provider error mappings with `provider error-map --help`.

Default streaming requests can continue while the upstream makes progress;
stalled upstream I/O remains bounded. Set `limits.upstream_timeout_seconds` in
the configuration when an absolute request deadline is required. See the
[upstream-timeout contract](design.md#upstream-timeouts) for defaults and
cancellation semantics.

Use `provider_switch_threshold` to control when a later request can choose
another provider. The [switching contract](design.md#cooldown-switching) owns
comparison boundaries, precedence, affinity, unknown waits and switchback.
Set or inspect the routing-level threshold:

```sh
tidemux routing set --provider-switch-threshold 60
tidemux routing show
tidemux routing set --provider-switch-threshold default
```

To keep a stable client model name while changing an upstream model, configure
an alias on the intended provider:

```sh
tidemux provider update ark-agent-plan --model-aliases '{"kimi-k3":"kimi-k3.1-pro"}'
```

Clients can keep requesting `kimi-k3` (or `ark-agent-plan/kimi-k3`). Keep that
client name in `supported_models` when using an allowlist, and configure verified
prices under the upstream ID `kimi-k3.1-pro`. Inspect the applied revision before
sending traffic. The flag replaces that provider's complete alias map; pass `{}`
to clear it. The JSON configuration field is `providers.<ref>.model_aliases`.
See the [route selection contract](design.md#route-selection) for validation,
response names, accounting, shared health and reload behavior.

For provider/model overrides, add these optional fields to the intended provider
object. Model override keys are client model names (upstream IDs when no alias is
configured). Values below are examples,
not defaults:

```json
{
  "provider_switch_threshold": 120,
  "model_switch_thresholds": {"glm-max": 30}
}
```

Save provider edits atomically and confirm the revision is applied. To select
the earlier immediate-recovery policy, use
`tidemux routing set --provider-switch-threshold 0`; the
[switching contract](design.md#cooldown-switching) defines zero and omitted values.

The `routing_decision` metadata in terminal JSONL/SQLite exposes `from_provider`,
`to_provider`, `model`, `action`, `reason`, `threshold_ms`, optional `remaining_ms`
and `cooldown_until_ms`. `wait_unknown` marks a reported wait as only a known
lower bound; optional `to_model` identifies an allowed auto-chain model change.
Storage/accounting and timing rules are defined by the
[decision-record contract](design.md#routing-decision-records). Back up the
complete SQLite database before upgrading, including its decision metadata.

Configure upstream request concurrency with
`tidemux gateway configure --max-in-flight 0`, then restart the gateway. The
[capacity contract](design.md#session-capacity) distinguishes execution
concurrency from logical sessions and defines the default and restart behavior.

Only providers limit logical sessions. Inspect a cap with `provider show REF`
or authenticated `GET /tidemux/session-status`, whose `providers` object contains
each reference's `limit`, `current` and `rejected` counts. A full provider returns
HTTP 429 / `active_session_limit` with `scope: "provider"`, `provider_ref` and
`limit` before dispatch. See the [session contract](design.md#session-capacity)
for identity, lifetime and reload behavior.

Remove the obsolete top-level `max_active_sessions` field before upgrading,
including a value of `0`; validation rejects it with removal guidance. Configure
each intended cap using `provider update REF --max-active-sessions N` instead.
There is no gateway-wide session option, prompt or status counter.
Listener mode defaults to authenticated loopback; `0.0.0.0` exposes plain HTTP on
the LAN and needs network protection. Use `gateway configure --help` for global settings.

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

Local cooldowns return HTTP 429 with the protocol's `rate_limit_error`. Inspect
`error.retry` for `strategy`, optional `after_seconds`, `wait_source`,
`reselect_provider` and qualified alternatives. `eligible_after_seconds` reports
known route eligibility separately; `provider_retry_at` is the upstream error's
provider deadline. These may differ from the client's suggested wait; the
[retry-advice contract](design.md#retry-advice) defines the bounds and unknowns.
For a listed qualified alternative, select its `PROVIDER/MODEL` explicitly if
appropriate; each new request still undergoes admission.

`x-should-retry` reports the mapped retry signal. When supplied, `Retry-After`
and `retry-after-ms` expose matching second/millisecond cadence. A client retains
its own retry budget and may ignore these signals; see the
[HTTP/SSE recovery rules](design.md#retry-advice).

For a verified vendor quota code, configure the intended provider explicitly:

```sh
tidemux provider error-map add REF --code CODE --status 429 --category quota_exhausted
```

Use the [failure-recovery contract](design.md#provider-failure-recovery) to
interpret classification, provider pauses and quota recovery. Verify actual
subscription/reset information with the provider rather than reading a local
probe deadline as a subscription reset.

Client retry budgets remain client settings. In loopback acceptance, Claude Code
2.1.263/2.1.283 and the official OpenAI/Anthropic SDKs consume the terminal signal;
Kilo 7.8.1 and Hermes 0.21.2 can issue further new requests under their own retry,
transport or auxiliary-call policies. The provider gate rejects blocked routes
locally, and shared routing can select an eligible alternative on a new request.
Bound the client budget with `CLAUDE_CODE_MAX_RETRIES`,
`KILO_SESSION_RETRY_LIMIT`, or Hermes `agent.api_max_retries`; a value of 2 was
used in the installed-client comparisons. These limits do not cap every client's
capability checks or independently admitted auxiliary calls. Inspect request IDs
and upstream-attempt counts when diagnosing repeated client messages.
Hermes 0.21.2 can also exhaust this budget when concurrent calls receive the
1311 HTTP 403 response, then exit despite an eligible shared-route alternative
and `x-should-retry: true`. Automatic recovery was intermittent in loopback
comparisons. Start a new request or explicitly choose an eligible provider after
checking the reported subscription entitlement; increasing a retry budget does
not guarantee recovery.

For a Z.ai / BigModel deployment, first confirm that `REF` is that provider,
then install the four explicit mappings below (use your usual `--config` path):

```sh
tidemux provider error-map add REF --code 1308 --status 429 --category quota_exhausted
tidemux provider error-map add REF --code 1310 --status 429 --category quota_exhausted
tidemux provider error-map add REF --code 1311 --status 429 --category provider_access_denied
tidemux provider error-map add REF --code 1313 --status 429 --category fair_use_throttled
tidemux provider error-map list REF
```

These commands are an explicit deployment choice; they are not installed for
other providers, guessed from domains or applied globally to numeric codes.
Save a config backup and confirm the gateway's applied reload revision. Existing
credentials and model/capability settings do not change.

| Z.ai code | Meaning and client semantics | Recovery |
| --- | --- | --- |
| 1308 | Usage-window quota; terminal `quota_exhausted`, HTTP 429 | Wait for the provider's actual reset or use an eligible alternative. |
| 1310 | Weekly/monthly quota; terminal `quota_exhausted`, HTTP 429 | Check the subscription quota/reset in the account center. |
| 1311 | Subscription entitlement; terminal `provider_access_denied`, HTTP 403 with `permission_error` | Enable the required model entitlement or choose a permitted model/provider. |
| 1313 | Terminal `fair_use_throttled`, HTTP 429 with `rate_limit_error` | Check account activity and request manual removal in the account center. If unexpected load is not yours, investigate possible key exposure and rotate the provider key. |

The [official error reference](https://docs.bigmodel.cn/cn/faq/api-code) describes
1311 as access to the named model. Mapping it to `provider_access_denied`
intentionally pauses **all models and keys of this provider**, on the operator's
assumption that they share the subscription. It does not assert that the vendor
revoked every key/model. Do not use that provider-wide mapping for independent
entitlements; restrict the configured model scope instead.

The [provider-recovery contract](design.md#provider-failure-recovery) defines
provider-wide pauses, probe timing and the fixed conditional fair-use guidance;
the [retry-advice contract](design.md#retry-advice) defines what clients receive.
Use the account-center actions above to resolve restrictions. A local cooldown
or a code alone does not establish compromise or predict an unlock time. For
upstream text handling, use [provider error messages](#provider-error-messages).

### Provider error messages

Read `error.message` and the diagnostic identifiers `error.code`, `gateway_code`
and `provider_code` when investigating an upstream failure. Their classification,
forwarding defaults and redaction rules are defined by the
[failure-message contract](design.md#failure-metadata-and-messages).

To exclude upstream free text, set `"forward_error_messages": false` in the
selected `providers.REF` JSON object; legacy single-provider configuration places
this field at the root. Save atomically and confirm the reload applied. Use this
setting before upgrading from 0.3.4 if the provider needs the previous text-free
policy, and keep a configuration/ledger backup. Review the [privacy contract](../PRIVACY.md)
before sharing diagnostic metadata.

`actual_tokens` and `limit_tokens`, when supplied, describe the rejected request;
use the [token-fact rules](design.md#failure-metadata-and-messages) when comparing
them with model discovery or a configured context override.

### Audit failures

Use the response or terminal SSE request ID to inspect the corresponding
SQLite/JSONL record. For `audit_failed_do_not_retry_blindly` with
`error.retry.strategy=manual_check`, check accounting before resubmitting: the
provider may already have completed the request. If later requests report
`audit_unavailable`, reconcile the failed request, repair storage, then restart
the gateway. Model discovery remains available. The
[audit guard contract](design.md#audit-failure-guard) defines its lifetime and
stream-completion ordering.

[SDK HTTP retries and mid-stream errors have different handling](https://platform.claude.com/docs/en/api/errors),
so a client may need an explicit new request after resolving the failure.

### Status fields

Availability JSON includes failure `scope`, optional `retry_after_seconds` and
up to 16 alternatives for an observed model. Omitted wait means no exact time
is known; a zero wait means a real recovery probe can be attempted now. The CLI
shows the wait, reason and model-health alternatives without sending inference.

In authenticated key status, `requests` counts adapter calls and `http_attempts`
counts initiated HTTP calls. Success, failure, cancellation, timeout and in-flight
counts are separate; use SQLite billing for accounting. See the
[counter contract](design.md#key-counters) for counting and reset rules.

`consecutive_rate_limits` stays visible with diagnostics disabled. Inspect it
alongside `rate_limit_retry_at`, `provider_retry_at`, `last_rate_limit_at` and
`recovery_in_progress` when investigating a rate limit. These describe local
health, not subscription quota.
The [rate-limit contract](design.md#rate-limit-recovery) owns backoff, silence and
streak rules; [server-wait continuity](design.md#server-wait-continuity) owns
cross-reload/rotation behavior and recovery claims.

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

Runtime stderr and terminal-request files use the same
[schema-2 runtime records](design.md#runtime-identity-and-usage-records).
Session files live at `logs/projects/tidemux/*.jsonl` beside the ledger.
Each event has `timestamp`, `event`, `service` (name/version/process instance)
and a unique persisted `event_id`. Requests also have `requestId`, outcome,
model (`message.model`) and known usage. Full bodies, credentials and raw sessions
are omitted; provider error reasons follow the configured policy; [privacy](../PRIVACY.md) covers retained metadata.

For a failed call, compare the response's `request-id`, `x-request-id` or
`X-TideMux-Request-ID` (or terminal SSE `request_id`) with `requestId` in the log.
`X-TideMux-Version` identifies the running binary version. `failure` records upstream HTTP/embedded error
status, source/phase, a safe provider code or its redaction state, provider request
ID when valid, category and retryability, plus the redacted reason when enabled
and explicit token facts. Match a terminal `key_label` against the authenticated key status;
`backend_id` is a gateway connection label. Their
[identity and lifetime contract](design.md#key-and-connection-labels) explains
what those labels can establish. An HTTP 200 stream can end with an error;
use `outcome` and the terminal error. `unknown` means the cause was not established,
not a confirmed quota, policy or context-limit failure. Preserve the provider
request ID when asking that provider to investigate; review metadata before sharing.
The optional `failure.response_retry_headers` object exposes `x_should_retry`,
`retry_after_seconds` and `retry_after_ms`; use the
[response snapshot contract](design.md#response-retry-snapshot) to interpret
absence and consistency across outputs.

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
references. These examples are deployment guidance; TideMux CI does not certify
specific collector/Elasticsearch version combinations. TideMux does not ship
logs itself.

### Optional full-content collection

For full request/reply search in Elasticsearch or external archiving, add an
explicit `content_output` block to your configuration, then restart `serve`:

```json
"content_output": {
  "enabled": true,
  "directory": "/absolute/private/tidemux-content",
  "file_bytes": 8388608,
  "directory_bytes": 268435456,
  "queue_events": 256
}
```

The parent directory must exist; TideMux creates the dedicated child as 0700,
or requires an existing private directory. Omit this block to keep the default
off, including upgrades. Once configured, `enabled` reloads for new requests;
admitted requests finish under their original setting. Other settings require a
restart. Body output stays separate from stderr, ccusage and SQLite. It may
contain sensitive business data; read [privacy](../PRIVACY.md) before enabling.
Check the authenticated `GET /tidemux/content-output-status` for `enabled` and
`written_events`, `dropped_events`, `write_failures`, `queued_events` in `counters`.
`content_output_gap` warns of loss. On overflow or disk failure, inference
continues and content may be incomplete. Remove only closed files whose external
delivery has been confirmed to free directory capacity. There is no automatic
retention deletion, upload retry or historical backfill.

The [content contract](design.md#optional-full-content-output) defines byte-exact
client views, metadata, resource limits, file handoff and reconstruction checks.
Use `data` for lossless reconstruction and `text` for chunk-level search. Query
the final `requestId` to collect both input/output chunks, deduplicate event IDs,
sort by `seq`, decode base64, and verify offsets/counts/SHA-256 against
`content_end`. Do not call a missing/false end event, broken sequence or failed
hash a complete response. Visible reasoning/tools and inline media are included;
URLs do not imply downloaded media. An upstream view is not recorded.

Use a **separate, explicitly configured collector and destination** for bodies.
Do not add this path to an existing summary pipeline accidentally. For Filebeat,
adapt the preceding example: use the following input and replace the processor
field paths with `tidemux_content.timestamp` and `tidemux_content.event_id`, then
choose a separate index such as `tidemux-content-%{+yyyy.MM.dd}`:

```yaml
filebeat.inputs:
  - type: filestream
    id: tidemux-content
    paths: ["/absolute/private/tidemux-content/content-*.active",
            "/absolute/private/tidemux-content/content-*.jsonl"]
    file_identity.native: ~
    message_max_bytes: 131072
    parsers:
      - ndjson: {target: tidemux_content, add_error_key: true}
```

Keep its registry across restarts and keep rename tracking enabled. Give the
collector at least a 128 KiB line budget. Preserve/report parse failures,
including an interrupted file's partial final line. Before ingestion, install an
index template with these mappings under `tidemux_content`; map the other
metadata fields explicitly as in the summary guide:

```json
{
  "text": {"type": "text"},
  "data": {"type": "binary"},
  "requestId": {"type": "keyword"},
  "event_id": {"type": "keyword"},
  "seq": {"type": "long"},
  "offset": {"type": "long"},
  "complete": {"type": "boolean"}
}
```

Keep base64 bytes in `_source`; avoid dynamic `text.keyword` indexing and never
expand arbitrary message/tool JSON into mapping fields. Use `match` on
`tidemux_content.text` for full-text chunk search and `term` on
`tidemux_content.requestId` for reconstruction. Select an analyzer appropriate to
your language. A phrase crossing a chunk boundary requires external
reconstruction. Elasticsearch's [text mapping](https://www.elastic.co/docs/reference/elasticsearch/mapping-reference/text)
and the [filestream reference](https://www.elastic.co/docs/reference/beats/filebeat/filebeat-input-filestream)
cover collector-specific settings; validate your own versions and destination.

For OSS file backup, an external job can upload **closed** `.jsonl` files with
the official [ossutil cp](https://help.aliyun.com/zh/oss/developer-reference/cp-upload-file),
using its own private credential configuration:

```sh
for file in /absolute/private/tidemux-content/content-*.jsonl; do
  [ -f "$file" ] || continue
  ossutil cp "$file" "oss://YOUR-BUCKET/YOUR-PREFIX/$(basename "$file")" || exit 1
done
```

Track confirmed objects and verify remote checksums/read-back externally before
deleting local closed files. Preserve filenames and avoid remote deletion on
local rotation. This example deliberately performs no local cleanup.

For real-time forwarding to your own HTTP receiver, an independent Vector
configuration can use a persistent data directory and bounded disk buffer:

```toml
data_dir = "/absolute/private/vector-state"
[sources.content]
type = "file"
include = ["/absolute/private/tidemux-content/content-*.active", "/absolute/private/tidemux-content/content-*.jsonl"]
read_from = "beginning"
max_line_bytes = 131072
[sinks.archive]
type = "http"
inputs = ["content"]
uri = "https://archive.example/ingest"
encoding.codec = "json"
buffer.type = "disk"
buffer.max_size = 268435488
buffer.when_full = "block"
```

This forwards each original JSONL line in the Vector event's `message`; the
receiver decodes that outer envelope before consuming the content contract.
Configure TLS, authentication and acceptance/read-back checks for your receiver
in the collector, and follow the [file source](https://vector.dev/docs/reference/configuration/sources/file/)
and [HTTP sink](https://vector.dev/docs/reference/configuration/sinks/http/) references.
Persistent cursors and bounded buffers do not prove exactly-once or remote
durability. OSS/S3 authentication compatibility and remote storage services are
operator concerns; TideMux holds no storage credentials or ES/OSS SDK.

## Compatibility

Matching client/upstream protocols pass through natively. Conversion supports
representable text, tools/results, usage and streaming. Provider-hosted Anthropic
tools, thinking and context-management controls require an Anthropic upstream;
unsupported request features fail before dispatch. Native blocks/history remain
provider-owned. TideMux carries tool calls and never executes client tools.
Responses API, audio, embeddings and IDE integrations are outside the supported
interface. Validation and optional client investigations belong to
[CONTRIBUTING](https://github.com/hs3180/tidemux/blob/main/CONTRIBUTING.md#releases).

GLM's native server search can return provider-specific names, assistant
`tool_result` blocks with string content, and a reported search counter of zero.
Native mode preserves these provider blocks and usage. Include the tool
definitions on every request that needs them. [#157](https://github.com/hs3180/tidemux/issues/157)
tracks the remaining provider contract/continuation limitations; the provider's
[search MCP documentation](https://docs.bigmodel.cn/cn/coding-plan/mcp/search-mcp-server)
describes its client tool workflow.

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
| `upstream_timeout` | Check `error.failure.phase` and the effective upstream protocol. Defaults are 60 seconds for non-streaming and ten minutes for streaming; a fast model-list query does not establish inference health. |

Partial output is not a completed turn. Follow the
[single-dispatch recovery contract](design.md#single-dispatch). After correcting
a malformed response, use the client's new-conversation mode without
`--continue`/`--resume`, then resume only a successful history. Share timestamps,
request IDs and safe codes, after checking the metadata for private information.
