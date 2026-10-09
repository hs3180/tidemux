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
| `startup_stage` | Bounded initialization stage on a failed `gateway_start`; see the stage/code pairs below. |
| `http_status` | Status written to the client. A stream that starts with HTTP 200 and later emits an error has `http_status: 200` and `outcome: "error"`. |
| `latency_ms` | Elapsed time for the terminal adapter call, or time to a local rejection. |
| `queue_time_ms` | Time spent waiting for the shared upstream concurrency gate; zero for local rejections. |
| `upstream_attempted` | Whether the adapter entered an upstream attempt path. This does not prove the provider received the request. |
| `record_persisted` | Whether the corresponding audit or diagnostic row was written locally. |

Example terminal event:

```json
{"time":"2026-09-29T12:00:00Z","level":"INFO","msg":"request summary","schema_version":1,"event":"request_terminal","request_id":"0123456789abcdef0123456789abcdef","protocol":"anthropic","provider_protocol":"openai","endpoint":"messages","provider_ref":"glm","model":"glm-5.3","outcome":"success","error_code":"","http_status":200,"latency_ms":815,"queue_time_ms":4,"upstream_attempted":true,"record_persisted":true}
```

Other events cover `gateway_start` (with `outcome: "error"`, a stable
`error_code` and `startup_stage` if startup fails), `gateway_shutdown`,
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

## Startup failures and help

The JSON logger is initialized as soon as the `serve` command is recognized,
before parsing flags or reading configuration and Keychain credentials. Every
failed startup emits exactly one terminal `gateway_start` with `outcome: "error"`
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

These classifications identify the failing subsystem, not every underlying OS
or provider cause. Diagnostics intentionally omit the raw error. Other CLI
commands retain their human-facing output and error behavior.

`tidemux serve -h` and `tidemux serve --help` print help to stdout, leave stderr
empty and exit **0**, without loading configuration or credentials. Normal
startup still emits `gateway_start`; graceful shutdown emits `gateway_shutdown`.
Runtime failures after startup remain separate server/shutdown events, not a
second startup failure. Run the candidate gate with:

```sh
python3 scripts/test_startup_logs_package.py --binary /path/to/extracted/tidemux
```

The gate uses only synthetic configuration, fake Keychain credentials and
loopback listeners. The optional `--evidence DIR` saves a result and redacted
startup events for isolated collection verification. It never contacts
production services or requires the user's real secrets.

## External collection and privacy

An operator-managed collector can read TideMux's stderr stream, parse each line
as JSON, and forward the resulting documents to an Elasticsearch data stream
or index. Configure parsing and mappings for the fields above, and use
`request_id` to correlate events. It is not a unique event ID and must not be
used alone for deduplication or as an Elasticsearch document ID. The short
Filebeat example in the [README](../README.md#forward-runtime-logs-to-elasticsearch)
shows the basic setup. Map
`tidemux.startup_stage` as a keyword in collector templates if filtering
by startup phase; the additional field preserves schema version 1 and existing
request/lifecycle mappings. Existing indices can retain the field in `_source`
without indexing it until their mappings are updated by the operator. Keep
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
