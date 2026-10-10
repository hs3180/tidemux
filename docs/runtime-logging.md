# Runtime JSON logs

`tidemux serve` has one runtime log format: Claude-compatible JSON Lines, with
TideMux diagnostic fields in the same record. Every event is one JSON object
per line. Human-facing command output, including the listening
address, remains on stdout. The runtime logger uses Go's standard-library
`log/slog`; it does not send data over the network.

## Event schema

Every event has `timestamp`, `level`, `msg`, `schema_version` (currently `2`), and
`event`, `service` and `event_id`. Service and event identity are additive fields
in schema 2; existing request, usage and SQLite accounting meanings are unchanged.
The event name and fields are stable within a schema version. Unknown
or unsafe identifiers are omitted as empty strings rather than copied into the
log.

`timestamp` is UTC with exactly three millisecond digits. A terminal request
uses its audit timestamp; local rejections use their diagnostic timestamp, and
other events use their emission time. There are no legacy `time`, `request_id`
or top-level `model` aliases. There is no format selector or compatibility mode.

| Field | Meaning |
| --- | --- |
| `service.name` | Always `tidemux`. |
| `service.version` | The same application version printed by `tidemux version`. Library callers without build metadata use `unknown`; no revision is inferred from the environment. |
| `service.instance.id` | A random 128-bit lowercase hexadecimal namespace, shared by all loggers in one process run; a restart creates a new namespace. |
| `event_id` | Process namespace plus `-` plus a 16-digit hexadecimal monotonic sequence (49 characters). Every distinct emitted event has its own ID, including events without a request ID. Persisted copies and replay retain it; use this field as the collector document/replay key. |
| `requestId` | TideMux request ID, also returned in `X-TideMux-Request-ID`; use it to join `request_terminal` with `request_audit.id` or `local_rejection` with `local_diagnostics.id`. |
| `sessionId` | Optional pseudonymous client session group; never the raw session ID. |
| `type` | `assistant` for terminal provider requests. Lifecycle and diagnostic events do not fabricate assistant usage. |
| `message.id` | The terminal request ID, also present on local rejections. |
| `protocol` | Client-facing protocol (`openai` or `anthropic`). |
| `provider_protocol` | Configured upstream wire protocol. It is `unknown` when a local rejection occurs before a provider is selected. |
| `endpoint` | Normalized route category: `chat_completions`, `messages`, `models`, or `unsupported`; never a URL. |
| `provider_ref` | Selected TideMux provider reference, when known. |
| `message.model` | Normalized model ID, when known. |
| `message.usage.input_tokens` | Known input tokens excluding cache reads and cache creation. |
| `message.usage.output_tokens` | Known output tokens. |
| `message.usage.cache_read_input_tokens` | Known cache-read tokens, when supplied. |
| `message.usage.cache_creation_input_tokens` | Known cache-creation tokens, when supplied. |
| `outcome` | `success`, `error`, `canceled`, or `rejected`. It describes the terminal request outcome independently of HTTP status. |
| `error_code` | Bounded TideMux error code. Raw upstream error messages and response bodies are not recorded. |
| `startup_stage` | Bounded initialization stage on a failed `gateway_start`; see the stage/code pairs below. |
| `http_status` | Status written to the client. A stream that starts with HTTP 200 and later emits an error has `http_status: 200` and `outcome: "error"`. |
| `latency_ms` | Elapsed time for the terminal adapter call, or time to a local rejection. |
| `queue_time_ms` | Time spent waiting for the shared upstream concurrency gate; zero for local rejections. |
| `upstream_attempted` | Whether the adapter entered an upstream attempt path. This does not prove the provider received the request. |
| `record_persisted` | Whether the corresponding audit or diagnostic row was written locally. |

Example terminal event:

```json
{"timestamp":"2026-09-29T12:00:00.000Z","level":"INFO","msg":"request summary","schema_version":2,"service":{"name":"tidemux","version":"0.3.3","instance":{"id":"abcdef0123456789abcdef0123456789"}},"event_id":"abcdef0123456789abcdef0123456789-0000000000000001","event":"request_terminal","requestId":"0123456789abcdef0123456789abcdef","protocol":"anthropic","provider_protocol":"openai","endpoint":"messages","provider_ref":"glm","outcome":"success","error_code":"","http_status":200,"latency_ms":815,"queue_time_ms":4,"upstream_attempted":true,"record_persisted":true,"type":"assistant","message":{"id":"0123456789abcdef0123456789abcdef","model":"glm-5.3","usage":{"input_tokens":120,"output_tokens":24,"cache_read_input_tokens":32,"cache_creation_input_tokens":8}}}
```

Other events cover `gateway_start` (with `outcome: "error"`, a stable
`error_code` and `startup_stage` if startup fails), `gateway_shutdown`,
`unexpected_server_error`, `http_server_error`, `provider_unavailable`,
`statement_sync_failure`, `request_audit_write_failure`,
`budget_settlement_failure`, and `usage_log_write_failure`. The latter is
rate-limited to one warning per minute while writing usage logs fails; it
contains no paths or raw filesystem errors.
Background statement-sync events contain counts
only; they never contain file paths, names, contents, or raw errors. The HTTP
server adapter likewise discards its raw message because it may contain
untrusted request data.

The central envelope covers early startup, lifecycle, request/rejection, conversion
warnings, recovered panic, audit/budget, HTTP-server and background events. It
reserves root `service` and `event_id` attributes; caller groups cannot replace
them. Identity generation uses only local cryptographic randomness and a
process-wide atomic counter, with constant memory and no event history or network
access. It does not use machine/user identity, paths, credentials or sessions.
If the counter is exhausted, the logger refuses subsequent events instead of
reusing IDs.

## Startup failures and help

The JSON logger is initialized as soon as the `serve` command is recognized,
before parsing flags or reading configuration and Keychain credentials. Every
failed startup after identity initialization emits exactly one terminal `gateway_start` with `outcome: "error"`
and exits with status **1**. Other bounded diagnostic events may precede this
failure, but no successful start is emitted. Main does not print a duplicate
plain-text error to stderr. The human message on stdout contains only the
stage/code and a reference to this guide; raw errors, private paths, flag
values, configuration and credentials are not copied into either message.

| `startup_stage` | `error_code` | Recovery |
| --- | --- | --- |
| `arguments` | `invalid_arguments` | Run `tidemux serve --help`; remove unsupported flags/positional arguments and supply a nonempty `--config` value. |
| `config` | `config_load_failed` | Check that the selected config exists, is readable and uses the current JSON schema and valid settings. |
| `credentials` | `credential_resolution_failed` | Check the configured Keychain references, unlock/access permissions and distinct, nonempty gateway/provider secrets. |
| `providers` | `provider_initialization_failed` | Configure an upstream with `provider add`; if protocol detection fails, explicitly set its supported protocol. |
| `ledger` | `ledger_initialization_failed` | Check the existing ledger directory, permissions and integrity; stop another gateway owning the same ledger. Never delete a live `.gateway.lock` sidecar. |
| `listener` | `listener_bind_failed` | Check that the configured listener can bind and its port is available. |
| `gateway` | `gateway_initialization_failed` | Bounded fallback for an unclassified initialization error; preserve local state and check the supported configuration and installation. |

If runtime identity cannot initialize, serve exits **1** before reading config or
credentials, with a fixed `logging (runtime_identity_failed)` recovery message on
stdout and no JSON event or plain-text stderr fallback. A colliding placeholder
ID is never used; check OS randomness and restart the process.

These classifications identify the failing subsystem, not every underlying OS
or provider cause. Diagnostics intentionally omit the raw error. Other CLI
commands retain their human-facing output and error behavior.

`tidemux serve -h` and `tidemux serve --help` print help to stdout, leave stderr
empty and exit **0**, without loading configuration or credentials. Normal
startup still emits `gateway_start`; graceful shutdown emits `gateway_shutdown`.
Runtime failures after startup remain separate server/shutdown events, not a
second startup failure. Developer verification methods are documented in the
[release procedure](releasing.md#runtime-checks).

## External collection and privacy

An operator-managed collector can read TideMux's stderr stream, parse each line
as JSON, and forward the resulting documents to an Elasticsearch data stream
or index. Configure parsing and mappings for the fields above, and use
`requestId` to correlate events. It is not a unique event ID and must not be
used alone for deduplication or as an Elasticsearch document ID. Use the persisted
`event_id` as the document key and `service.instance.id` to group one process run.
A root request ID spanning provider failover is not supplied by this schema. The [Elasticsearch collection guide](elasticsearch.md) shows the minimal
Filebeat settings, Logstash document key, field types and model/usage queries.
Decode records under `tidemux` to avoid a conflict between Claude's `message`
object and ECS's text field. Map `tidemux.timestamp` to `@timestamp`; index model,
request, session and event IDs as keywords, usage and latency as longs, outcomes
as keywords, and persistence as a boolean. Input plus cache read plus cache
creation reconstructs total input. Missing usage stays absent rather than zero.
Keep collector credentials, TLS, buffering, retries, lifecycle and retention in
the collector deployment. TideMux has no Elasticsearch client or shipping
credential and does not distribute collector deployment files.

Terminal request summaries include the [ccusage-compatible usage fields](ccusage.md):
`timestamp`, `requestId`, optional pseudonymous `sessionId`, `type: "assistant"`
and `message` with model and known token/cache counts. Unknown required token
counts omit `message.usage`. Local rejections carry request/model metadata but
no `type: "assistant"` or usage. Other diagnostics likewise have no usage.

Each terminal request is encoded once. Stderr and
`logs/projects/tidemux/<session-group>.jsonl` receive the exact same complete
record, including service metadata, event identity, provider, outcome, HTTP status
and timing fields. Elasticsearch
may collect either the runtime stderr stream (including lifecycle/rejection
events) or these session files (terminal requests). Do not collect both copies
into the same index. ccusage reads only `projects/`, so a stderr capture stored
outside that directory cannot duplicate its usage. Configure local native file
identity in Filebeat when collecting session files so short, one-request files
do not wait for a fingerprint-size threshold.

Request summaries omit credentials, authorization headers, raw session IDs,
request and response bodies, raw upstream errors, full URLs, query strings and
pricing snapshots. Provider/model IDs, session groups and token counts can
reveal configuration and usage patterns. Restrict
access to collected logs and set retention to match the operator's privacy
requirements. The local SQLite ledger remains the source of truth for
accounting; the JSON event stream is an operational summary.
