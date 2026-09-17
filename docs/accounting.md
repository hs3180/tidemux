# Request accounting and reconciliation

TideMux records local request outcomes, estimates cost from provider-reported
usage and automatically reconciles normalized supplier statements while the
gateway runs. It does not fetch provider invoices or charge users.

## What is recorded

The SQLite database keeps the following accounting records:

| Table | Purpose |
| --- | --- |
| `request_audit` | One terminal record per admitted-to-processing request, including requests canceled while queued |
| `audit_events` | Ordered events such as queue waiting and upstream rate limiting, committed with the request |
| `local_diagnostics` | Rejections before upstream processing, such as invalid authentication, unsupported endpoints or invalid parameters |
| `token_comparisons` | Optional local tokenizer counts keyed to an audited request; never stores request text |
| `balance_snapshots` | Explicit account-balance observations for account-level variance diagnostics |
| `supplier_statement_lines` | Imported supplier statement rows, kept separate from local estimates |
| `reconciliation_requests`, `reconciliation_statements` | Automatically maintained matches, coverage and comparison amounts |
| `statement_imports`, `statement_import_sources` | Persistent file fingerprints and accepted source paths |
| `statement_sync` | Latest background check result and last successful check |

Successful model discovery is served locally and does not create a billing attempt.
A tool workflow or client retry can create several request records. TideMux does
not automatically retry or group those records into a conversation invoice.
Queued cancellation is recorded without an upstream call; request counts therefore
must not be interpreted as a count of provider-billed requests.

Each audit contains a generated request ID, timestamp, protocol, upstream label,
model, terminal status (`ok`, `error`, `canceled`), error code, total latency,
queue duration, nullable token/cache usage and estimated cost. Full detail,
including pricing snapshots and cache counts, lives in `record_json`; frequently
queried fields are also columns. Message bodies and credentials are not recorded.

Request and event rows commit in one SQLite transaction. Terminal writes have a
separate five-second context so client cancellation does not abort them. If the
write fails, the request reports `audit_failed_do_not_retry_blindly`; the upstream
may already have processed the request. During streaming, the normal terminal
frame is withheld until auditing succeeds. Earlier streamed content cannot be
retracted, and a terminal audit is not proof the client received the final byte.
An abrupt process crash before terminal persistence can leave an attempt unrecorded.

## Usage and cost

Usage comes from the upstream response, not local tokenization. OpenAI cached
input is a subset of prompt tokens. Anthropic cache read/write counts are added
to its ordinary input count to produce total input. Streaming usage is handled
according to each protocol's events; cumulative counters are not blindly summed.

With sufficient usage and configured prices, the estimate is:

```text
ordinary_input = total_input - cache_read - cache_write
cost = (ordinary_input × input_rate
      + cache_read × cache_read_rate
      + cache_write × cache_write_rate
      + output × output_rate) / 1,000,000
```

Rates are explicitly configured per model, with currency, source and version.
There is no default currency or region-specific pricing behavior.
The gateway's configuration supplies the upstream context. Successful requests
retain a copy of the configured price with their record, so later configuration changes
do not rewrite history. There is no automatic price discovery, time-of-day rate
switching, currency conversion or provider discount calculation.

Missing usage, missing required rates or ambiguous cache breakdowns produce
`null`, not zero. Current failed/canceled attempts do not retain partially
observed streaming usage or a cost estimate; they may still incur provider costs.
Cost calculations use floating-point numbers and SQLite REAL, suitable for
estimation, not exact financial settlement.

## Inspect requests

```sh
# Request details for the current calendar month:
tidemux billing --details

# Recent local rejections, kept separate from upstream attempts:
tidemux doctor --diagnostics

# Structured request details from the local ledger:
tidemux billing --details --json
```

Request details include every audit in the selected period, newest first, and
follow the same period as billing statistics and downloads.
`doctor --diagnostics` shows the latest 100 local rejections in a readable table;
add `--json` for a structured array. Local rejections are not billed attempts.
Preserve unknown-cost counts and group by currency rather than silently treating
unknown amounts as free or adding different currencies.

Validation compares each response's request ID and usage with its audit record,
then independently recomputes cost from the recorded rates. Tests cover unknown
usage, failures, cancellation, queueing and atomic writes. Real DeepSeek acceptance
also compared response/cache usage and independently calculated Decimal estimates
with recorded results. Those runs verified selected configured prices; they did
not implement automatic peak/off-peak pricing or reconcile supplier invoices.

## Automatic reconciliation

Reconciliation runs with the gateway; there is no command to start, import or
retry it. Local estimates are recorded with each completed request. On gateway
startup and every 60 seconds thereafter, TideMux checks the `statements`
directory beside the ledger for normalized supplier CSV files and imports new
files automatically. The gateway creates this directory when needed. Files that
cannot be read or validated are retried on later checks without interrupting
request serving.

Obtain the statement from your provider and normalize it to this CSV format:

```text
period_start,period_end,currency,amount,request_id,model
2026-09-14T00:00:00Z,2026-09-15T00:00:00Z,USD,0.0134,tidemux-request-id,deepseek-flash
```

The first four columns are mandatory. Times must be RFC3339 (or positive Unix
milliseconds), `amount` must be a non-negative finite number, and the end must
follow the start. `request_id` and `model` are optional. A row without a known
TideMux `request_id` is explicitly reported as unmatched: TideMux never guesses
that a statement charge belongs to a local request.

Treat statement files as immutable. Write each complete file outside the watched
directory, then move it into that directory with a `.csv` extension. TideMux
deduplicates identical file contents, including across gateway restarts or file
renames. Changes to an already imported file path are rejected. Keep exports
non-overlapping: content deduplication does not identify the same charge repeated
in different exports. A file must be at most 32 MiB and contain at most 100,000
data rows. Each background import also has a two-second time budget to keep
request auditing responsive; files within the size limits can still exceed this
budget. Timed-out files are retried on later checks. Split slow or large exports
into independent, non-overlapping smaller files. Invalid or timed-out files are
rejected as a whole.

To change the directory or interval, add this optional object to the local
TideMux configuration:

```json
"reconciliation": {
  "statement_dir": "/absolute/path/to/statements",
  "poll_interval_seconds": 60
}
```

The polling interval accepts 1 through 86400 seconds; `0` selects the default.
Missing fields use their defaults. TideMux reads provider exports supplied
locally; automatic provider invoice retrieval is not implemented.

## Billing statistics and download

Billing is a single-user local view. The command reads
`~/Library/Application Support/TideMux/config.json` and queries the existing
database at its `ledger_path`, preserving the configured location and history.
The command uses this location automatically. It works while the gateway is
stopped, does not trigger reconciliation and requires an existing initialized
ledger. It does not access Keychain or contact the provider.

```sh
# Readable current-month statistics and statement synchronization status:
tidemux billing

# The same statistics as structured JSON:
tidemux billing --json

# Request audit details for the same period:
tidemux billing --details

# An inclusive start and exclusive end:
tidemux billing --from 2026-09-14T00:00:00Z --to 2026-09-15T00:00:00Z

# Download the stored billing details as a private CSV file:
tidemux billing --from 2026-09-14T00:00:00Z --to 2026-09-15T00:00:00Z \
  --download /absolute/path/to/billing-2026-09-14.csv
```

Without `--from` or `--to`, every billing view uses the current calendar month
in the computer's local timezone: from midnight on its first day, inclusive, to
midnight on the first day of the next month, exclusive. The output displays the
exact RFC3339 bounds, including timezone offsets. This default applies to the
summary, `--details`, `--json` and `--download`.

To select a period, use RFC3339 times after the Unix epoch, with at most
millisecond precision. Finer nonzero fractions are rejected because stored
timestamps use milliseconds. When you supply either `--from` or `--to`, the
omitted boundary is open; supplying only `--from` does not add an implicit
month-end limit. The same period rules apply to every billing
view. Combine `--details --json` for structured request details. The download path
must be new: existing files are never overwritten, and a failed export removes
its partial output.

The summary shows statistics by currency and the latest statement synchronization
status. With `--json`, the result includes `period`, `usage`, `reconciliation`
and `statement_sync`. The period's `from` and `to` are RFC3339 strings, or `null`
for an open boundary. `--details --json` adds a `requests` array, including when
no records match. Statistics include local estimated cost, supplier statement
amounts, matched and unmatched lines, requests with unknown local cost and
requests with no matching statement. Coverage is `no_statement`, `partial` or `complete`;
`difference` is `null` unless the selected records have complete comparable
coverage. Supplier periods that only partly fit the selected range are excluded
from the supplier total and counted separately. Different currencies are never
combined. Synchronization status reports the last attempt, last successful check,
files and lines imported, and failures, so stale or unavailable statements remain
visible. When the gateway is stopped these values stay at the last recorded check.

The CSV is a download of locally stored billing details, not an official provider
invoice. Local requests and supplier statement lines have separate row types;
an estimate is never substituted for a supplier charge. Blank amounts or token
counts mean unknown or unavailable, while `0` means a recorded zero. Without a
supplier statement, the download contains local request records only. A period
with no records produces column headers and no data rows.
After a successful download, the CLI confirms the destination and selected
period. Add `--json` to receive a structured receipt with `period` and `download`
instead; the CSV contents are still written to the requested file.

The reconciliation schema also has append-only records for a local tokenizer
measurement and account-balance snapshots. The tokenizer record contains only
counts, tool/version and request ID; provider API usage remains the request
settlement truth. Balance net changes are account-level diagnostics only:
top-ups, grants, expiry and other API clients may alter a balance. They are never
booked as TideMux spending. Automatic local tokenization and provider balance
retrieval are not implemented; these diagnostics are present only when
measurements have been recorded.

## Legacy data

The `ledger` command has been removed. Update scripts to use
`tidemux billing --details --json` for request records and
`tidemux doctor --diagnostics --json` for local rejections. Billing request
records follow the selected period; use explicit `--from` and `--to` bounds
when the current calendar month is not the intended range.

`ledger_requests`, `ledger_events` and their old `Summarize` view remain for
historical compatibility. The active gateway writes `request_audit`/`audit_events`,
and the CLI reads those newer records. Do not use the legacy fixed-currency summary as an
aggregate of current traffic. Historical currency-specific database identifiers
are preserved to avoid reinterpreting old amounts or breaking compatibility. Existing data is retained during upgrades.
