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
Listener, ledger, request-concurrency and session-idle-timeout changes require a gateway restart.

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
Each client retry is a new request; the gateway dispatches at most once for it.

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

Shared selection is off by default. `random` retains a healthy binding when
the caller supplies a stable `X-TideMux-Session-ID` (or Anthropic
`metadata.user_id`); without one, selection is per request. `price_priority`
chooses the lowest sum of the three token rates and requires comparable prices
in one currency. It has no session affinity. Shared-model launchers preserve
the bare ID and omit any one candidate's context/output-limit declarations.
Use `--shared-model-strategy off` to restore explicit ambiguity checks.

`/v1/models`, `/models` and model-detail responses expose each qualified model's
known `context_length` and `max_output_tokens`. Named providers discover these
from their model catalog. The cache lasts for the connection: endpoint, protocol,
API-version or credential changes fetch it again; restart to refresh an unchanged
upstream catalog. This performs no generation requests. Kilo/Hermes launchers
use the running gateway's per-model limits; shared bare models keep limits unset.

If the catalog omits limits, or supplies invalid/conflicting values, those values
remain unknown and startup logs warn with `model_capabilities_unknown` or
`model_capabilities_invalid`. Nonzero `providers.REF.model_capabilities` settings
(`context_tokens`, `max_output_tokens`) override discovered fields; a disagreement
produces `model_capabilities_conflict`. Verify values with the provider before
configuring them. The override applies to the entire provider, so use a dedicated
scope for models with the same limits. Model names and overflow error text are
not used to guess a window. A missing field still means unknown.

Set a verified provider-wide context override with
`tidemux provider update REF --context-tokens 1000000` (one million tokens).
`tidemux provider show REF` displays the configured override. Set it to `0` to
clear the override and use catalog discovery again; missing catalog metadata
still means unknown. This preserves the configured output limit and other
provider settings. The running gateway applies the saved configuration through
its normal reload process.

Auto-chain failures affect later eligible requests; the dispatched request is
not replayed. After dispatch, TideMux does not wait for a retry, retry a 429,
switch keys, fail over to another provider, or change the requested model. It
returns the mapped result and bounded gateway retry advice to the caller; the
client owns retry timing and sends a new request. Configure error mappings with
`provider error-map --help`.

A stable random/auto session keeps its provider during a short cooldown. On the
next request, TideMux compares the **remaining** wait with
`routing.provider_switch_threshold` (default **60 seconds**). A known remaining
wait strictly greater than the threshold switches to a healthy compatible
alternative; equality or a shorter wait retains the binding and returns a local
429 with a bounded wait, without generating upstream. Price priority applies the
same comparison to its lowest-price compatible provider. It still has no session
affinity: after recovery, later price-priority requests follow normal price order.
Fresh random/auto conversations keep normal eligible-route selection.

Set or inspect the global threshold:

```sh
tidemux routing set --provider-switch-threshold 60
tidemux routing show
tidemux routing set --provider-switch-threshold default
```

Values are integer seconds in `0..86400`. Zero explicitly selects the previous
immediate eligible-route recovery policy. Global, provider and exact upstream
model overrides are hot reloadable; precedence is model, then provider, then
global, then 60 seconds. Add these optional fields to an existing provider object:

```json
{
  "provider_switch_threshold": 120,
  "model_switch_thresholds": {"glm-max": 30}
}
```

Confirm the saved revision is applied. Already admitted requests keep their own
threshold/configuration snapshot. The comparison includes the maximum independent
provider, model and key gate; it never shortens a full server pause to the client
retry cadence. An active probe has unknown completion time. Unless a known lower
bound already exceeds the threshold, the default retains that binding with
bounded backoff rather than inventing a countdown. Without a healthy compatible
alternative, the binding stays in place. Explicit `PROVIDER/MODEL` never switches.
Alternatives still pass request-feature, capacity and budget admission checks.

After a long-wait switch, healthy random/auto bindings stay on their new provider;
there is no active migration back. A fresh conversation can probe an earlier
eligible route, and successful recovery restores it for normal new selection.
Auto chains keep their configured permitted model order; only bare shared models
require the identical model across providers. A partial reply is never replayed.
For the older immediate recovery behavior during migration, configure threshold
zero explicitly; omitting the field now enables the 60-second policy.

The `routing_decision` metadata in terminal JSONL/SQLite identifies the actual
source/destination provider, model, action, reason, threshold, known remaining
wait and cooldown deadline. Millisecond remainders round upward; `wait_unknown`
means any recorded wait is only a known lower bound. An optional `to_model`
identifies an allowed auto-chain model change. Local waits have one correlated
`local_diagnostics` row and optional `local_routing_decisions` details, committed
together; these are not generation/cost records. Historical rows and the old
diagnostic table's write shape remain readable/usable. Back up the complete
SQLite database before upgrading, including this additive metadata table.

The `routing.billing_exhaustion_failover` field has been removed. Delete it from
existing configuration files before upgrading; configuration validation rejects
unknown fields.

Global upstream request concurrency is unlimited in new configurations.
Use `tidemux gateway configure --max-in-flight 0` to disable the limit in an
existing configuration, then restart the gateway. Existing positive values
still cap simultaneous upstream requests across all providers; this setting
does not hot reload.

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
Listener mode defaults
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

Local cooldowns return HTTP 429 with the protocol's `rate_limit_error`.
A valid upstream `Retry-After` on a rate-limit or temporary server failure
pauses the whole provider, across keys and models, until its real deadline.
This pause is independent of client retry timing and can exceed 24 hours.
When another compatible route is eligible, the client receives a two-second
wait and new-request routing advice. Otherwise, known waits are capped at 30
seconds for the client; a retry only rechecks routing and never dispatches to a
still-paused provider. An active probe has no known finish time and recommends
bounded exponential backoff with jitter. Retry advice is limited to three attempts.

A new shared-model or `auto` request can select another healthy provider.
An explicit `PROVIDER/MODEL` keeps that choice; the error may list qualified
alternatives that require changing the model. Alternatives describe model,
protocol and health; features, session capacity and budgets are checked again
when a request is sent. `error.retry` supplies `strategy`, optional
`after_seconds`, `wait_source`, `reselect_provider` and these alternatives.
`eligible_after_seconds` separately reports known route eligibility and
`provider_retry_at` retains the provider deadline on the upstream error.
For example, A can stay paused for one hour while a new request after two
seconds selects B. A long upstream header is not forwarded to freeze the agent.
Clients choose their own retry policy.

Recoverable HTTP errors include `x-should-retry: true`. `retry-after-ms`, when
present, matches the bounded `Retry-After` in milliseconds. Known terminal
errors use `x-should-retry: false` without a wait; a header does not override a
client's retry budget or guarantee that its agent loop will retry.
For confirmed provider quota exhaustion, shared/auto requests can recover through
an eligible alternative; explicit requests and routes all blocked by quota send
the terminal retry signal. Quota errors retain HTTP 429 and their full server pause.
Map a verified vendor quota code for that provider with
`tidemux provider error-map add REF --code CODE --status 429 --category quota_exhausted`.
The standard `insufficient_quota` code is recognized directly; numeric vendor
codes are not inferred globally. Without a server reset time, quota health uses
a five-minute initial local recovery wait, not a claimed subscription reset.

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

All four configured failures pause the provider immediately. The failed request
returns its error after one generation dispatch. A shared/auto client may retry
as a new request to an eligible compatible alternative; an explicit provider and
routes all blocked by these restrictions send `x-should-retry: false`, with no
retry wait. Local provider-gate rejections use HTTP 429 and retain a distinct
cooldown code; fair-use rejections keep the generated manual-recovery guidance.

Without a valid upstream `Retry-After`, the initial request-driven recovery wait
is five minutes with jitter; repeated failed probes back off to 30 minutes plus
jitter. After the wait, one real request may probe the provider; success restores
eligibility. These are local probe bounds, **not predicted quota reset or fair-use
unlock times**. No timer or background inference declares the account healthy.
A valid upstream wait is preserved in full. Reset dates in free text do not become
trusted deadlines. Code 1313 alone does not establish key compromise, and its
recovery time remains unknown. Its fixed conditional guidance is included even
when provider message forwarding is disabled or a forwarded reason is present.
Set `forward_error_messages: false` for strict exclusion of upstream free text.

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

Availability JSON includes failure `scope`, optional `retry_after_seconds` and
up to 16 alternatives for an observed model. Omitted wait means no exact time
is known; a zero wait means a real recovery probe can be attempted now. The CLI
shows the wait, reason and model-health alternatives without sending inference.

Key labels are opaque. `requests` counts adapter calls using a key;
`http_attempts` counts actual HTTP attempts. Each downstream generation request
initiates at most one such attempt.
Success, failure, cancellation, timeout and in-flight counts are separate.
Skipped keys and local rejections add no attempts. These counters are not billing.
Compatible reloads preserve them; credential/connection changes or restart reset
the counters. A server pause survives policy reload and credential rotation on
the same provider endpoint/protocol, even when its response arrives after the
reload. It also protects requests already queued under an older configuration.
Changing the endpoint/protocol, removing and re-adding the provider, or restarting
the process resets it. Recovery advice uses the current available routes.
Toggling diagnostics clears counters/history while preserving routing
and cooldowns. See [recovery bounds](design.md#routing-and-recovery).

`consecutive_rate_limits` on each key and pool is operational health state and
remains visible with diagnostics disabled. The pool also reports
`rate_limit_retry_at`, `provider_retry_at`, `last_rate_limit_at` and
`recovery_in_progress` when relevant. Without a server-supplied provider pause,
one limited key leaves other ready keys usable. If all keys are cooling, later
requests wait for the earliest eligible key; only one request tests recovery.
Without a valid upstream wait, consecutive 429s back off from 30 seconds to
30 minutes with jitter. The 11th failure silences the affected key for 24 hours;
if the provider streak exceeds ten with no ready keys, the provider is silenced
for 24 hours. Success resets the applicable streaks. This is local failure
recovery, not a measurement of remaining subscription quota.

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

## Compatibility

Matching client/upstream protocols pass through natively. Conversion supports
representable text, tools/results, usage and streaming. Provider-hosted Anthropic
tools, thinking and context-management controls require an Anthropic upstream;
unsupported request features fail before dispatch. Native blocks/history remain
provider-owned. TideMux carries tool calls and never executes client tools.
Responses API, audio, embeddings and IDE integrations are outside the supported
interface. Full interactive/dsh/Codex workflows are outside release acceptance.

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

Partial output is not a completed turn. TideMux never replays output that may
have reached the client; client retries create new requests. After correcting
a malformed response, use the client's new-conversation mode without
`--continue`/`--resume`, then resume only a successful history. Share timestamps,
request IDs and safe codes, after checking the metadata for private information.
