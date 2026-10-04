# Elasticsearch collection

TideMux writes one JSON object per line to `stderr`; human-facing CLI output
stays on `stdout`. An operator-managed collector reads that existing stream and
owns Elasticsearch connectivity, credentials, buffering and retries. TideMux
does not include an Elasticsearch client, endpoint setting or shipping
credential. The current event schema is sufficient; basic collection does not
depend on the deferred #100–#104 fields. SQLite remains the accounting source
of truth.

## Minimal path: Filebeat

This configuration uses Filebeat 8.19.x syntax and sends events to one
dedicated index. Check [Elastic's compatibility matrix](https://www.elastic.co/support/matrix/)
for the Filebeat and Elasticsearch versions you deploy. It relies on
Elasticsearch dynamic mappings for this basic setup; add your own template and
lifecycle policy if you need strict mappings, rollover or retention.

Capture only the runtime `stderr` file. Keep `stdout` separate:

```sh
tidemux serve --config /path/to/config.json \
  >> /var/log/tidemux/console.log 2>> /var/log/tidemux/runtime.jsonl
```

For a service manager, point Filebeat at the actual stderr path. The Filebeat
account needs read access to that file and write access to its own data
directory.

In Elasticsearch Dev Tools, create a small ingest pipeline once. It maps the
native TideMux `time` into Elasticsearch's `@timestamp` while retaining the
original value under `tidemux.time`. The setup identity needs permission to
manage ingest pipelines:

```http
PUT _ingest/pipeline/tidemux-runtime-time
{
  "description": "Map TideMux event time to @timestamp",
  "processors": [
    {
      "date": {
        "field": "tidemux.time",
        "formats": ["ISO8601"],
        "target_field": "@timestamp",
        "ignore_failure": true
      }
    }
  ]
}
```

Use this `filebeat.yml` as the starting point; replace the log path, endpoint
and CA path:

```yaml
filebeat.inputs:
  - type: filestream
    id: tidemux-runtime-jsonl
    paths:
      - /var/log/tidemux/runtime.jsonl
    parsers:
      - ndjson:
          target: tidemux
          add_error_key: true

output.elasticsearch:
  hosts: ["https://elasticsearch.example:9200"]
  index: tidemux-runtime
  pipeline: tidemux-runtime-time
  api_key: "${TIDEMUX_ES_API_KEY}"
  ssl.certificate_authorities:
    - /etc/filebeat/elasticsearch-ca.pem

setup.ilm.enabled: false
setup.template.enabled: false

queue.disk:
  max_size: 2GB
```

The `ndjson` parser puts TideMux's native fields under `tidemux`, so its string
field `tidemux.event` cannot conflict with ECS's object-valued `event`. The
pipeline preserves `tidemux.time` and writes the parsed value to `@timestamp`;
if a line has no valid time, `ignore_failure` leaves Filebeat's collection
timestamp in place. Non-JSON lines carry a Filebeat parse error. For a dedicated
parse-failure index and more detailed mapping diagnostics, use the optional
Logstash reference below.

Create a Filebeat keystore and add the API key as `id:api_key`; use the same
service account and Filebeat data/config paths that will run the service:

```sh
filebeat keystore create
filebeat keystore add TIDEMUX_ES_API_KEY
filebeat test config -c /etc/filebeat/filebeat.yml
filebeat -e -c /etc/filebeat/filebeat.yml
```

Filebeat prompts for the key. Do not put a real key in this file or in shell
history. Use an HTTPS endpoint and a trusted CA. If the server uses a private
CA, configure its certificate path as above; leave certificate verification
enabled. Give the writer identity only the permissions needed to create and
write the `tidemux-runtime` index.

The disk queue is a finite buffer, not an exactly-once guarantee. Keep
Filebeat's data directory across restarts, provide enough disk space, and
retain rotated TideMux logs until Filebeat has read them. A full queue or a
replayed file can still lead to delayed or duplicate documents. `request_id`
correlates related events; it is not a unique event ID and must not be used as
an Elasticsearch document ID. Configure index rollover and retention for a
long-running deployment.

If Fluent Bit or Vector is already your standard collector, either can read the
same stderr JSON Lines file; TideMux needs no collector-specific integration.
See the [Filebeat NDJSON parser](https://www.elastic.co/guide/en/beats/filebeat/8.19/filebeat-input-filestream.html),
[Elasticsearch output and TLS settings](https://www.elastic.co/guide/en/beats/filebeat/8.19/elasticsearch-output.html),
[Filebeat keystore](https://www.elastic.co/guide/en/beats/filebeat/8.19/keystore.html),
[disk queue](https://www.elastic.co/guide/en/beats/filebeat/8.19/configuring-internal-queue.html),
and [Elasticsearch date processor](https://www.elastic.co/guide/en/elasticsearch/reference/8.19/date-processor.html)
references for version-specific details.

## Optional advanced path

The repository also keeps a verified, more detailed [Logstash and Elasticsearch
reference](elasticsearch-logstash.md) for typed mappings, parse-failure
isolation, DLQ diagnostics and the packaged ingestion smoke procedure. It is
optional; basic collection does not require it. Fluent Bit or Vector
configurations are not included here.
