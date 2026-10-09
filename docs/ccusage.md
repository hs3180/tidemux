# ccusage-compatible usage logs

TideMux can write private Claude-compatible JSONL for an independently installed
[official ccusage](https://github.com/ccusage/ccusage). Usage logging is **off by
default**. Enable it in the existing gateway configuration, then restart:

```json
"usage_log": {
  "enabled": true,
  "directory": "/absolute/path/to/tidemux-usage"
}
```

`directory` is optional and defaults to `usage/` beside the ledger. Other optional
settings are `poll_seconds` (default 1), `max_bytes` (default 16777216), and
`max_files` (default 8, across all sessions). The output directory must be
dedicated to this ledger. Directories are 0700 and files are 0600; public files,
symlinks, unrelated files and client-owned project directories are refused.
Gateway requests never invoke or wait for a reporting tool. Operational stderr
JSONL keeps its existing schema and excludes token/session accounting fields.

## Read the logs externally

The layout is `projects/tidemux/<session-group>/usage-<sequence>.jsonl` beneath
the export root. Each line has `timestamp`, `requestId`, an optional `sessionId`,
and `message.id`, `message.model`, `message.usage`. Timestamps use UTC with exactly
three millisecond digits. Cache fields use Claude's
`cache_creation_input_tokens` and `cache_read_input_tokens` names. Ledger input
includes cached input; the exported `input_tokens` subtracts known cache counts
so ccusage's total does not count them twice. Unknown cache counts are omitted
from `message.usage` and remain null in the `tidemux` accounting metadata.

Use only the TideMux export root as `CLAUDE_CONFIG_DIR`. For ccusage 20.0.26,
the focused Claude commands avoid reading other agents' logs:

```sh
CLAUDE_CONFIG_DIR=/absolute/path/to/tidemux-usage npx ccusage@latest claude daily --json --offline
CLAUDE_CONFIG_DIR=/absolute/path/to/tidemux-usage npx ccusage@latest claude session --json --offline
```

ccusage is an external reader; TideMux does not ship, install or configure it.
Do not combine this export with client logs covering the same routed requests,
which can double count their usage. The `daily` and `session` reports describe
retained records with known input and output counts. Records missing either
required count omit `message.usage` and are skipped by ccusage, while their
nullable audit values remain in `tidemux`. Absent cache breakdowns default to
zero in ccusage; the nullable metadata identifies that missing coverage.

ccusage calculates model-based costs using its own pricing. Those costs do not
represent TideMux's configured prices, original currencies or supplier charges;
models without ccusage pricing cannot yield a reliable cost report. TideMux
does not populate `costUSD`. Use `tidemux billing` and the ledger for accounting.
The `tidemux` object separately preserves estimated cost, currency, usage/cost/
price provenance and matched supplier statement snapshots. Multiplying
provider-reported tokens by a price is still an estimate.

## Identity, replay and retention

Client session identities are HMAC-SHA256 grouped by protocol, gateway caller
credential and session. A private key at `<ledger>.usage-key` keeps groups stable
across restarts and rotation. Raw session IDs and caller credentials are absent;
grouped activity still reveals usage patterns. Missing client IDs and historical
audits use the explicit `ungrouped` directory and omit `sessionId`.

The exporter uses a separate read-only connection and scans committed audits in
bounded pages. Complete JSONL records are synced before the atomic checkpoint.
Replay after a crash, checkpoint loss or rowid renumbering can duplicate lines;
stable source-scoped `message.id` and `requestId` let ccusage deduplicate tokens
across files. Later matched supplier statements emit new metadata snapshots
with unchanged token identities. ccusage does not interpret statement revisions
or choose the latest supplier charge; billing remains authoritative. No prompts,
responses, tool content, private price-source URLs or raw upstream errors are
exported. Existing per-attempt audit semantics are unchanged.

Rotation keeps a session's directory stable and limits total retained files
across all groups. Empty session directories are removed with their last pruned
file. A record can exceed a small configured rotation size but is capped at
64 KiB. Each scan reads at most 128 audit/statement identities and about 1 MiB
of audit JSON (one oversized audit can cross that bound).

First enablement or a fresh dedicated output directory automatically exports
committed history. Retention limits still apply; increase retention before
requesting a longer reporting window. To rebuild, stop the gateway and configure
a fresh empty directory, keeping old reports separately. Pre-release logs from
the former custom source format also require a fresh directory. Never reuse
client log directories or mix restored ledgers and identity keys with old output.

## Status and failures

Read `<ledger>.usage-status.json` for `enabled`, `last_success`, `error_code`,
`audit_rowid` and `statement_id`. Within a gateway run, failures preserve the last success and
emit a safe `usage_export_failure` warning on stderr. Key, output and checkpoint
failures remain isolated from request/SSE handling and budget/audit transactions.
An unavailable identity key disables export/grouping; fix it and restart. A disk
failure that also breaks SQLite follows the existing audit/budget error contract.

Set `usage_log.enabled` to false and restart to disable export. Existing ledger
metadata, private key and output are preserved. For downgrade to a pre-feature
binary, remove `usage_log` if its validator does not recognize that setting.
