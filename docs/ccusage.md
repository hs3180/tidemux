# Read usage logs with ccusage

TideMux records each completed provider request in its only runtime log format:
Claude-compatible JSONL with complete TideMux diagnostics. No TideMux configuration
changes are needed. Logs are written to `logs/` beside the configured ledger:

```text
logs/projects/tidemux/<session-group>.jsonl
```

For an independently installed [official ccusage](https://github.com/ccusage/ccusage),
set `CLAUDE_CONFIG_DIR` to that `logs/` directory:

```sh
CLAUDE_CONFIG_DIR="/path/to/tidemux/logs" ccusage claude daily
CLAUDE_CONFIG_DIR="/path/to/tidemux/logs" ccusage claude session
```

TideMux does not ship, install or invoke ccusage. The commands above read the
logs using ccusage 20.0.26. Avoid combining these records with client logs of the
same requests, which would count their usage twice.

Each record has `timestamp`, `requestId`, optional `sessionId`, and
`message.id`, `message.model`, plus `message.usage` when known. The same record
also includes level, event, protocol, provider, outcome, error classification,
HTTP status, latency, queue time and persistence fields. Stderr and the session
file receive the exact same encoded JSON line; there is no usage-only format.
Timestamps are UTC with three
millisecond digits. Input tokens exclude cache reads/writes, which use Claude's
`cache_read_input_tokens` and `cache_creation_input_tokens` fields. Missing input
or output counts omit `message.usage`, so ccusage skips unknown usage. Unknown
cache breakdowns are omitted; ccusage treats missing cache counts as zero.
Counts use the terminal request's provider usage or existing local estimate.

Session groups are HMAC hashes keyed by the gateway credential and separated by
client protocol. They remain stable across restarts with the same credential;
rotating that credential changes the grouping. Anonymous requests use
`ungrouped.jsonl`. Raw session IDs, credentials, prompts, responses and tool
content are absent. Directories are private (0700) and files are private (0600).

Logs cover recorded requests from this version onward. Restarting does not
reconstruct logs from historical billing records or replay failed log writes.
Log-write failures emit a bounded `usage_log_write_failure` diagnostic and leave
request handling and budget settlement unchanged. Manage log retention with
your normal local log policy.

ccusage uses its own model prices. Its cost report does not represent TideMux's
configured prices, original currencies or supplier charges. Use `tidemux billing`
for accounting. Elasticsearch collectors can use the same records and indexed
token fields; see the [runtime log schema and collection configuration](runtime-logging.md).
