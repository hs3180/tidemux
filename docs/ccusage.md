# ccusage usage reports

Usage export is **off by default**. Enabling it adds private local accounting
metadata and keyed session groups to the ledger; the operational stderr JSONL
schema remains unchanged. Gateway requests never start or wait for ccusage.

```sh
tidemux usage configure --enable --config /path/to/config.json
# Restart the gateway to apply this startup setting.
tidemux usage status --config /path/to/config.json --json
```

Configuration is `usage_log: {"enabled": true}`. Optional `directory` must be an
absolute, dedicated directory. Defaults: `usage/` beside the ledger, polling
every one second, 16 MiB per rotated file, eight retained files. CLI settings
are `--directory`, `--poll-seconds`, `--max-bytes`, `--max-files`; configuration
fields use underscores. Directory mode is 0700, files 0600. Existing public,
symlink or unrelated/client-owned output is refused. A single record can exceed
a configured small rotation size, but is capped at 64 KiB. Export reads at most
128 audit/statement identities per scan and stops audit pages after 1 MiB of
source JSON (one oversized audit may cross that bound). It uses a separate
read-only ledger connection, never recovering live pending budget reservations.

Install the pinned maintained **actual ccusage** build using the
[source adapter instructions](../integrations/ccusage/README.md), then:

```sh
ccusage tidemux session --path /absolute/path/to/usage --json
ccusage tidemux aggregate --path /absolute/path/to/usage --json
```

The upstream npm build has no TideMux adapter. Supported fork version is
`20.0.26+tidemux.1`, pinned ccusage commit
`e12b7dd9c14494808057df1897d07edc999081eb`; source, patch, dependency lockfile,
licenses and local installer are shipped here. Its dedicated command reports
TideMux records; client/default `--all` behavior is unchanged. Adding a client's
own report for the same routed requests would double count them.

Records have `schema_version: 1`, `source: "tidemux"`, stable `source_id`,
`request_id`, and `revision`. They contain audit timestamp, provider, model,
protocol, outcome, available token counts, nullable currency and estimated
cost, token/cost/price provenance and a separate matched supplier amount.
There are no prompts, responses, tool payloads, credentials, raw session IDs
or raw upstream errors. Each upstream attempt corresponds to its committed
audit: opt-in billing-exhaustion failover across providers can produce multiple
attempts, including failures. An auto-chain failure changes preference for new
sessions and does not replay the already dispatched request.
Same-provider credential retries retain the existing single-audit behavior.

`usage_source` distinguishes `provider`, `local_estimate`, `unknown`.
`cost_source` distinguishes provider-token-derived estimates, local content
estimates, historical estimates and unknown cost. `price_source` and
`price_version` refer to the committed price snapshot; private configured source
URLs are not copied. **Provider-reported tokens multiplied by prices are
estimates, not actual supplier charges.** Matched statement totals are separate
and never added to or substituted for the estimate. Currency partitions all
reports, including unknown currency. Complete sums remain null whenever any
request is unknown; known subtotals and known/unknown counts show coverage.

Client `X-TideMux-Session-ID` or supported Anthropic metadata session identity is
HMAC-SHA256 grouped by client protocol, gateway caller credential and session.
A private 32-byte key at `<ledger>.usage-key` persists the groups across restart
and output rotation. This key and ledger backups require the same protections
as accounting data. Groups reveal activity patterns despite hiding raw IDs.
Rotating the gateway caller credential intentionally separates groups. Missing
client IDs remain `session_group: null`; runtime-generated anonymous identities
and pre-feature history are never presented as real client sessions.

The exporter scans only committed audits. It syncs complete JSONL records before
atomically replacing its private checkpoint. Crash-after-commit retries later;
crash-after-output/before-checkpoint replays the same record. The consumer
deduplicates `(source_id, request_id)` and chooses the highest immutable supplier
statement revision. Later matched statements emit a replacement snapshot, not
an additional request or an additive copy of the old charge. Unmatched or
wrong-currency statements cannot become request charges. A stored audit ID
anchor detects rowid renumbering (for example VACUUM) and replays safely.
An incomplete output tail is repaired; consumers flag and ignore such a tail.
SQLite and JSONL have no shared atomic transaction, so raw JSONL can contain
duplicates. Use the supported consumer for totals.

Output retention bounds disk use, so reports describe retained records. Restore
older history from the authoritative ledger explicitly:

```sh
tidemux usage export --backfill --config /path/to/config.json
```

This read-only command can run with the gateway but yields if its exporter holds
the output lock. Backfill uses the same retention limits; increase configured
retention before exporting a full history. Restoring/replacing an entire ledger
and identity key should use a fresh dedicated output directory, preserving old
reports separately rather than mixing different accounting histories.

Export permission, key, disk, malformed input and checkpoint failures are
isolated from normal requests/SSE and budget/audit transactions. The gateway
logs a safe `usage_export_failure` classification and writes queryable status to
`<ledger>.usage-status.json`; the status preserves last success. An unreadable or
unpersistable identity key disables the optional export/grouping capability;
it never falls back to raw IDs or temporary pseudonyms. If that diagnostic
sidecar itself cannot be written, consult the safe runtime warning. Correct the
path/permissions and restart for identity recovery. Consumer failures affect
only reports. A shared disk failure that also breaks SQLite still follows the
existing audit/budget error contract.

```sh
tidemux usage configure --disable --config /path/to/config.json
# Restart. Existing ledger metadata, usage files and private key are preserved.
```

Disabling does not erase accounting history. Remove usage artifacts only through
an explicit data-retention decision after stopping the exporter. Restoring the
pre-feature binary ignores optional ledger JSON fields; remove `usage_log` from
configuration before downgrade if the old validator does not support it.
