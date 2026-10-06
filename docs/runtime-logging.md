# Runtime JSON logs

`tidemux serve` writes machine-readable runtime events to stderr as JSON Lines:
one JSON object per line. Human-facing command output, including the listening
address, remains on stdout. The runtime logger uses Go's standard-library
`log/slog`; it does not send data over the network.

## Event schema

Every event has `time`, `level`, `msg`, `schema_version` (currently `1`), and
`event`. The event name and fields are stable within a schema version. Unknown
or unsafe identifiers are omitted as empty strings rather than copied into the
log.

| Field | Meaning |
| --- | --- |
| `request_id` | TideMux request ID, also returned in `X-TideMux-Request-ID`; use it to join `request_terminal` with `request_audit.id` or `local_rejection` with `local_diagnostics.id`. |
| `protocol` | Client-facing protocol (`openai` or `anthropic`). |
| `provider_protocol` | Configured upstream wire protocol. It is `unknown` when a local rejection occurs before a provider is selected. |
| `endpoint` | Normalized route category: `chat_completions`, `messages`, `models`, or `unsupported`; never a URL. |
| `provider_ref` | Selected TideMux provider reference, when known. |
| `model` | Normalized model ID, when known. |
| `outcome` | `success`, `error`, `canceled`, or `rejected`. It describes the terminal request outcome independently of HTTP status. |
| `error_code` | Bounded TideMux error code. Raw upstream error messages and response bodies are not recorded. |
| `http_status` | Status written to the client. A stream that starts with HTTP 200 and later emits an error has `http_status: 200` and `outcome: "error"`. |
| `latency_ms` | Elapsed time for the terminal adapter call, or time to a local rejection. |
| `queue_time_ms` | Time spent waiting for the shared upstream concurrency gate; zero for local rejections. |
| `upstream_attempted` | Whether the adapter entered an upstream attempt path. This does not prove the provider received the request. |
| `record_persisted` | Whether the corresponding audit or diagnostic row was written locally. |

Example terminal event:

```json
{"time":"2026-09-29T12:00:00Z","level":"INFO","msg":"request summary","schema_version":1,"event":"request_terminal","request_id":"0123456789abcdef0123456789abcdef","protocol":"anthropic","provider_protocol":"openai","endpoint":"messages","provider_ref":"glm","model":"glm-5.3","outcome":"success","error_code":"","http_status":200,"latency_ms":815,"queue_time_ms":4,"upstream_attempted":true,"record_persisted":true}
```

Other events cover `gateway_start` (with `outcome: "error"` and a stable
`error_code` if startup fails), `gateway_shutdown`,
`unexpected_server_error`, `http_server_error`, `provider_unavailable`,
`statement_sync_failure`, `request_audit_write_failure`, and
`budget_settlement_failure`, and optional `usage_export_failure`. The latter
contains only a bounded `failure_code` and is rate-limited to one warning per
minute while export fails; it contains no accounting values, session groups,
paths or raw errors. The existing request-summary fields remain unchanged.
Background statement-sync events contain counts
only; they never contain file paths, names, contents, or raw errors. The HTTP
server adapter likewise discards its raw message because it may contain
untrusted request data.

## External collection and privacy

An operator-managed collector can read TideMux's stderr stream, parse each line
as JSON, and forward the resulting documents to an Elasticsearch data stream
or index. Configure parsing and mappings for the fields above, and use
`request_id` to correlate events. It is not a unique event ID and must not be
used alone for deduplication or as an Elasticsearch document ID. See the
[minimal Logstash/Elasticsearch reference](elasticsearch.md) for configuration,
mappings, failure diagnostics and packaged ingestion verification. Keep
collector credentials, TLS, buffering, retries, index lifecycle, access
control, and retention in the collector and Elasticsearch deployment. TideMux
does not include an Elasticsearch client, endpoint setting, or shipping
credential.

Request summaries intentionally omit credentials, authorization headers,
session IDs, request and response bodies, raw upstream error bodies, full URLs
and query strings, token counts, and pricing snapshots. Provider references
and model IDs can still reveal configuration and usage patterns. Restrict
access to collected logs and set retention to match the operator's privacy
requirements. The local SQLite ledger remains the source of truth for
accounting; the JSON event stream is an operational summary.
