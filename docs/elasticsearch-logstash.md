# Advanced Elasticsearch collection with Logstash

This optional reference in `examples/elasticsearch/` collects existing TideMux
JSON Lines with a fully mapped Logstash pipeline. For the shorter Filebeat
setup, see [Elasticsearch collection](elasticsearch.md). The reference was
tested with **Logstash 8.19.5**, its bundled Elasticsearch output plugin
**11.22.13**, and **Elasticsearch 8.19.5**, using the official Elastic images.
This is the tested version pair, not a claim that it is the latest patch. Review
Elastic's supported versions and security updates before your own deployment.
TideMux has no Elasticsearch client or credentials and needs none of #100–#104.
SQLite remains the accounting source of truth.

## Capture the correct input

`serve` writes JSON events to **stderr** and human-readable startup output to
**stdout**. Keep them separate:

```sh
tidemux serve --config /path/to/config.json \
  >> /path/to/tidemux-stdout.log 2>> /path/to/tidemux-runtime.jsonl
```

For a launchd service, set `StandardErrorPath` to the JSON input and
`StandardOutPath` to a separate human-output file. Existing installs may have a
combined or legacy file; point the collector at the actual stderr path from
your plist, not an assumed filename. Non-JSON lines are retained in the
parse-failure index and a local diagnostic file. Old non-JSON lines can contain
sensitive data: protect those diagnostic files and indices too.

The collector account needs read access to the input/CA/config and write access
to its data, sincedb, persistent queue, DLQ and diagnostics directories. Use
private directories and restrict file access to the gateway/collector accounts.
Never add provider keys, request bodies or session IDs to the logging schema.

## Configure and start

Copy `logstash.yml`, `tidemux.conf` and `index-template.json` from
`examples/elasticsearch/`. Set these operator-owned values; the paths below are
placeholders:

```sh
export TIDEMUX_LOG_PATH=/path/to/tidemux-runtime.jsonl
export TIDEMUX_SINCEDB_PATH=/path/to/private-collector/sincedb
export TIDEMUX_LOGSTASH_DATA=/path/to/private-collector/data
export TIDEMUX_DLQ_PATH=/path/to/private-collector/dlq
export TIDEMUX_PARSE_FAILURE_PATH=/path/to/private-collector/parse-failures.jsonl
export TIDEMUX_MAPPING_FAILURE_PATH=/path/to/private-collector/mapping-failures.jsonl
export TIDEMUX_ES_INDEX_PREFIX=tidemux
export TIDEMUX_ES_URL=https://elasticsearch.example:9200
export TIDEMUX_ES_CA=/path/to/elasticsearch-ca.pem
```

Provide `TIDEMUX_ES_API_KEY` through the Logstash keystore or your service's
secret manager, in Logstash's `id:api_key` form. Do not put a real key in the
repository, shell history or a shared environment file. TLS certificate
verification stays enabled. The
[Elastic security guide](https://www.elastic.co/guide/en/logstash/8.19/ls-security.html)
explains CA and credential setup.

Install the composable template **before the first event** with an administrative
identity. If changing the prefix, also change its `index_patterns` from
`tidemux-*` to your exact prefix plus `-*`; use a dedicated template name.
For example, using an operator-protected curl config containing TLS/auth settings:

```sh
curl --config /path/to/admin-curl.conf -X PUT \
  "$TIDEMUX_ES_URL/_index_template/tidemux" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/elasticsearch/index-template.json
```

The pipeline disables automatic template management, ILM and data streams. Its
writer needs cluster `monitor` and index `write` plus `create_index` on the
chosen prefix only (including the parse-failure indices). If indices are
pre-created, index creation can be removed from the writer's responsibilities.
Template installation is a separate administrative operation requiring
`manage_index_templates`. Queries use a separate identity with `read` and
`view_index_metadata`; do not give the collector cluster administration or
access to accounting data.

Use a fresh writable `path.data` for each Logstash process. Copy the standard
Logstash `log4j2.properties` into your settings directory to control collector
logs; configuration diagnostics otherwise use its console defaults. Check and
start with Logstash 8.19.5:

```sh
/path/to/logstash/bin/logstash --path.settings /path/to/settings \
  -f /path/to/tidemux.conf --config.test_and_exit
/path/to/logstash/bin/logstash --path.settings /path/to/settings \
  -f /path/to/tidemux.conf
```

The `file` input starts at the beginning only when no sincedb position exists.
Keep sincedb and queue state across normal restarts. The default main pipeline
ID is `main`, which the DLQ reader uses. Output goes to
`PREFIX-yyyy.MM.dd`, using the event timestamp in UTC, or to
`PREFIX-parse-failures-yyyy.MM.dd`, using collector receipt time for an
unparseable event. Index lifecycle, retention, rotation, outage handling and
collector deployment remain operator responsibilities. Persistent queue and
sincedb do not constitute an end-to-end exactly-once guarantee.

## Namespace, types and failures

| Native JSON | Indexed mapping |
| --- | --- |
| `event` | `tidemux.event` keyword, copied to ECS `event.action` keyword |
| `time` | `tidemux.time` date_nanos; converted to `@timestamp` date |
| `level` | `tidemux.level` keyword, copied to `log.level` |
| `request_id`, outcome, protocol, provider/model/error identifiers | keywords under `tidemux.*` |
| `schema_version`, `http_status`, `latency_ms`, `queue_time_ms`, counts | long integers, numeric-string coercion disabled |
| `upstream_attempted`, `record_persisted` | boolean |
| `msg` | `tidemux.msg`, retained without text indexing |

The JSON filter decodes into `tidemux`, preventing the native string `event`
from colliding with ECS's object. Logstash's date filter supplies millisecond
precision for `@timestamp`; the original nanosecond timestamp remains in
`tidemux.time`. Unknown fields remain in `_source` but are not dynamically
indexed. The template is for schema version 1. Review it when the schema changes.

The collector uses auto-generated document IDs. **Do not use `request_id` as
`document_id`**: one request can produce multiple distinct events. It is a
correlation key, not an event ID. Replayed input can create duplicates; no
application event IDs or cross-retry root correlation are introduced here.

Non-JSON, missing required schema fields and invalid timestamps receive visible
failure tags and `collector.error: parse_failure`. Their original `message`
is retained, forwarded to the parse-failure index and written to
`TIDEMUX_PARSE_FAILURE_PATH`. Malformed decoded fields are removed only from
that diagnostic envelope; the raw input remains intact.

A bulk mapping rejection (400/404) goes to the enabled DLQ, retaining the event
and reason. Collector logs and DLQ statistics must be monitored; a successful
bulk HTTP 200 alone does not prove every document indexed. This follows
[Elastic's output error policy](https://www.elastic.co/guide/en/logstash/8.19/plugins-outputs-elasticsearch.html#plugins-outputs-elasticsearch-retry-policy).
Read failures through the supplied diagnostic-only pipeline:

```sh
# Keep the main process's data directory; set a DIFFERENT one for this reader.
TIDEMUX_LOGSTASH_DATA=/path/to/private-collector/dlq-reader \
  /path/to/logstash/bin/logstash --path.settings /path/to/reader-settings \
  -f /path/to/read-dlq.conf
```

Copy `read-dlq.yml` as `logstash.yml` into a separate reader settings directory;
it disables a DLQ writer so the reader can inspect the running main pipeline.
The reader writes `collector.error: mapping_failure` and `collector.reason`
to `TIDEMUX_MAPPING_FAILURE_PATH`. It does not rewrite/re-index failed events
or consume committed offsets; repeating it may duplicate diagnostics. Repair
and replay are explicit operator actions. Monitor disk space, queue/DLQ sizes
and file permissions, and follow
[Elastic's DLQ operations](https://www.elastic.co/guide/en/logstash/8.19/dead-letter-queues.html)
for retention. Do not delete live queue files.

## Query and reproduce the smoke check

With a read-only curl identity, search an existing request ID and terminal
outcome. HTTP 200 can still mean a failed stream:

```sh
curl --config /path/to/reader-curl.conf \
  "$TIDEMUX_ES_URL/$TIDEMUX_ES_INDEX_PREFIX-*/_search" \
  -H 'Content-Type: application/json' --data-binary @- <<'JSON'
{"query":{"bool":{"filter":[
  {"term":{"tidemux.request_id":"REQUEST_ID"}},
  {"term":{"event.action":"request_terminal"}},
  {"term":{"tidemux.outcome":"error"}},
  {"term":{"tidemux.http_status":200}}
]}}}
JSON
```

For the reproducible package smoke, start a **new isolated** Elasticsearch
8.19.5 fixture on a loopback-only port, with no production credentials or
indices. Docker must have the official Logstash 8.19.5 image. The script accepts
only an explicitly supplied loopback HTTP fixture and a fresh `tidemux-smoke-*`
namespace. It never discovers or cleans production indices:

```sh
docker run -d --name tidemux-es-smoke \
  -p 127.0.0.1:19231:9200 -e discovery.type=single-node \
  -e xpack.security.enabled=false -e 'ES_JAVA_OPTS=-Xms512m -Xmx512m' \
  docker.elastic.co/elasticsearch/elasticsearch:8.19.5
python3 scripts/test_elasticsearch_package.py \
  --binary /path/to/extracted/tidemux \
  --es-url http://127.0.0.1:19231 \
  --index-prefix tidemux-smoke-UNIQUE-LOWERCASE-ID \
  --evidence /path/to/new-private-evidence-directory
```

Use a lowercase unique prefix. The container connects to the host fixture through
Docker Desktop's `host.docker.internal`; this gate targets macOS arm64 packaged
binaries. The script checks the unmodified TLS/API-key reference configuration
syntactically using synthetic placeholders/public CA material; it then removes
only those transport settings for the security-disabled loopback fixture. It
checks ES's actual version, installs the scoped template, captures real packaged
stderr, runs Logstash, queries typed fields and reads a deliberately rejected
mapping from the DLQ. It stops its collector containers and leaves its isolated
indices and evidence available for inspection. Stop your fixture afterwards.

Verified 2026-10-03: five real runtime events indexed (success, local rejection,
HTTP 200 stream failure, start and shutdown); two synthetic events sharing one
request ID remained distinct; one legacy line remained visible in the parse
index; one invalid numeric field was retained/read from the DLQ. Eight documents
were searchable. No private fixture keys, prompts or session IDs appeared in
runtime logs. This smoke establishes basic ingestion, mapping and diagnostics;
TLS credential enforcement, long outages, rotation/restart delivery guarantees
and production deployment are outside this minimal gate.
