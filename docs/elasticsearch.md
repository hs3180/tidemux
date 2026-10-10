# Collect runtime logs in Elasticsearch

TideMux writes JSON Lines locally and does not ship logs. Choose runtime stderr
for all events, or `logs/projects/tidemux/*.jsonl` for terminal requests. Keep
ccusage pointed only at `projects/`; mixing client logs of the same requests can
duplicate usage. Restrict collector read access and Elasticsearch permissions.

Decode each line under `tidemux` so the Claude `message` object does not collide
with ECS's top-level text field. A minimal Filebeat configuration is:

```yaml
filebeat.inputs:
  - type: filestream
    id: tidemux-runtime
    paths: ["/path/to/runtime.jsonl"]
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
        - from: tidemux.event_id
          to: "@metadata._id"
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

Store `TIDEMUX_ES_API_KEY` in the collector's keystore and run `filebeat test
config` before starting it. Local native file identity reads short session files
without waiting for a fingerprint-size threshold. Keep registry state through
restarts; handle rotation according to the
[official filestream guidance](https://www.elastic.co/docs/reference/beats/filebeat/filebeat-input-filestream).

The event identity fields are added in 0.3.3; older schema-2 records without an
`event_id` need a separate ingestion policy. Copying `event_id` preserves it in `_source` as well as using it for `_id`.
Replaying the same line into the same index overwrites its document. Daily
indices must use event time, including during replay. IDs do not deduplicate
across different indices or independent client log records. Choose one source
for each request; stderr and session files contain identical event copies.
`requestId` correlates multiple events and audit rows; it is not a document key.
See [Filebeat deduplication](https://www.elastic.co/docs/reference/beats/filebeat/filebeat-deduplication).

For Logstash, decode with `json { source => "message" target => "tidemux" }`,
parse `tidemux.timestamp` with the date filter, and set
`document_id => "%{[tidemux][event_id]}"` on the Elasticsearch output for valid
records. Route malformed JSON or missing IDs to a separate failure output;
never use an unresolved placeholder as a shared document ID. Monitor parse,
timestamp and mapping failures. Use a dead-letter queue for mapping failures.
Deployment, TLS, buffering, retries and index lifecycle follow the
[official output reference](https://www.elastic.co/docs/reference/logstash/plugins/plugins-outputs-elasticsearch).

Install your index template before ingesting into a fresh prefix. Use these
field types; disable numeric coercion to catch malformed values:

| Field under `tidemux` | Mapping |
| --- | --- |
| `timestamp` (and top-level `@timestamp`) | `date` |
| `event_id`, `requestId`, `sessionId`, `event`, `outcome`, `error_code` | `keyword` |
| `service.name`, `service.version`, `service.instance.id` | `keyword` |
| `message.id`, `message.model`, `provider_ref`, `protocol`, `provider_protocol` | `keyword` |
| `schema_version`, `http_status`, `latency_ms`, `queue_time_ms` | `long` |
| `message.usage.input_tokens`, `message.usage.output_tokens` | `long` |
| `message.usage.cache_read_input_tokens`, `message.usage.cache_creation_input_tokens` | `long` |
| `upstream_attempted`, `record_persisted` | `boolean` |

For example, filter one model and sum its known output tokens:

```json
{"query":{"term":{"tidemux.message.model":"custom-model"}},"size":0,"aggs":{"output_tokens":{"sum":{"field":"tidemux.message.usage.output_tokens"}}}}
```

Missing required token counts omit `message.usage`; they mean unknown, not zero.
Use an `exists` query to measure usage coverage separately from token sums.
Input plus cache-read plus cache-creation reconstructs total input. A stream can
have HTTP 200 and `outcome: "error"`. Logs summarize operations; SQLite remains
authoritative for billing. See the [runtime schema and privacy](runtime-logging.md)
and [external ccusage](ccusage.md). Collector credentials and retention are
managed outside TideMux; no deployment directory is included in the package.
