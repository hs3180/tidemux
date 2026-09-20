# Request accounting and reconciliation

TideMux records local request outcomes, estimates cost from provider-reported
usage or, when that is unavailable, from a local content estimate, and
automatically reconciles normalized supplier statements while the gateway runs.
It does not fetch provider invoices or charge users.

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

Provider usage takes precedence. OpenAI cached input is a subset of prompt
tokens. Anthropic cache read/write counts are added to its ordinary input count
to produce total input. Streaming usage is handled according to each protocol's
events; cumulative counters are not blindly summed.

With sufficient usage and configured prices, the estimate is:

```text
input_cache_miss = total_input - cache_read
cost = (input_cache_miss × input_cache_miss_rate
      + cache_read × input_cache_hit_rate
      + output × output_rate) / 1,000,000
```

Cache creation/write tokens are included in `input_cache_miss`; there is no
separate cache-write price.

Rates are stored per model with currency, source and version. The DeepSeek
preset supplies one fixed peak price for supported models; custom `prices`
entries are selected by `configure` and override the preset. There is no
generic price discovery, default currency, or provider discount calculation.
The gateway's configuration supplies the upstream context. Successful requests
retain a copy of the selected price with their record, so later configuration
changes do not rewrite history. TideMux does not perform currency conversion.

If provider usage is missing, ambiguous or incomplete and a matching price is
configured or built in, TideMux uses a model-independent local content estimator. It counts
the normalized request input, uses the response text/content received so far
for output, and applies the configured input-cache-hit rate to the longest
common input prefix for a known session. Once an upstream transport attempt starts,
the full input is counted even when the transport fails; an interrupted stream
contributes only the output received before interruption. These records have
`cost_source: "local_estimated_cache_prefix"` and are estimates, not supplier
billing. A request that was never sent, has no price, or cannot produce a
usable local estimate remains `null`, not zero. Cost calculations use
floating-point numbers and SQLite REAL, suitable for estimation, not exact
financial settlement.

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
not reconcile supplier invoices.

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

## Budgets

An optional `budget` config section applies to one ledger and one currency. It
does not convert currencies. A matching configured `prices` entry for the
configured model is required whenever `budget` is enabled. DeepSeek's peak
preset and custom rates are written by `configure` alongside the provider API
key; `budget` only changes limits. If pricing is absent or uses another
currency, TideMux refuses to start and never sends an upstream request. Budget
windows are rolling five-hour and seven-day periods.

```json
"budget": {
  "currency": "USD",
  "five_hour_limit": 5,
  "weekly_limit": 80,
  "alert_threshold": 0.8,
  "mode": "hard"
}
```

`mode` is `alert`, `soft`, or `hard`. Budget checks use settled charges; there
is no user-configured per-request reserve. Alert allows the request and sets
`X-TideMux-Budget-Warning: 1` after the threshold is reached. Soft mode requires
a deliberate retry with `X-TideMux-Budget-Confirm: 1` once a threshold or limit
is reached. Hard mode rejects before upstream transmission. If usage or pricing
remains unknown, later budget requests are blocked with
`budget_usage_unknown`. A request for a model without a matching configured
price is rejected with `budget_pricing_unconfigured` before upstream
transmission.

Budget admission persists a zero-value pending attempt before the upstream call.
It is not a reserve and does not count toward the amount. If the process exits
before settlement, restart converts the pending attempt to `unknown`, so the
request cannot disappear from budget accounting.

When provider usage is missing, TideMux can make a local content estimate if a
pricing entry is configured. Provider usage always takes precedence. For a
known session, the longest common normalized input prefix is priced as cache-hit
input and new input as cache-miss input. Set `X-TideMux-Session-ID` on compatible
clients, or use Anthropic `metadata.user_id`; without a session identifier all
input is treated as cache-miss. The SSE response itself is output, never cache
hit input. Output is estimated from response content, including only the
portion received before an interrupted stream. The tokenizer is intentionally
model-independent, so this is a transparent approximation rather than an
exact provider token count. These records are marked
`local_estimated_cache_prefix`, not supplier billing. Session prompt history is
in memory; after a restart the next request starts without a local cache prefix.

Pre-release budget tables and fields are not migrated automatically. A profile
using the old budget schema must be replaced with the new configuration before
the budget feature can be used.

## Daily reports and delivery

`tidemux report generate --config /absolute/path/to/profile.json` writes a
content-free report for the current day; `--date YYYY-MM-DD` regenerates a
specific local-date view and `report list` reads persisted history. A report
contains counts, token totals and local estimates. If reconciliation or budget
tables exist, it reads those optional data sources without owning their migrations.
Unavailable extensions are `null`, not zero. Use `--timezone` (default UTC) and,
for a budget remainder, `--daily-budget N --budget-currency USD`; these are
reporting inputs and do not enforce a gateway budget. It never includes request text, response
text, API keys, or SMTP credentials.

Create an offline, self-contained visual report with:

```sh
tidemux report export --config /absolute/path/to/profile.json \
  --days 30 --timezone Asia/Shanghai
```

The HTML file contains summary cards, inline SVG charts for daily requests and
estimated cost, and a daily detail table. It has no network or JavaScript
dependency, is written with private file permissions, and is saved by default
as `reports/latest.html` beside the configured ledger. Run
`tidemux report open` to open that latest report in the default browser. Use
`--output /absolute/path/to/tidemux-report.html` for another destination or
`--open` to open an export immediately. The export reads a date range without
creating persisted daily-report rows.

`tidemux report deliver --id N --channel macos` refreshes the default HTML
report and sends a macOS notification whose click target is that file when
`terminal-notifier` is installed:

```sh
brew install terminal-notifier
```

Without it, TideMux keeps using the built-in AppleScript notification and
includes `tidemux report open` as the short fallback. `report retry` retries a
recorded failed delivery. Optional SMTP uses this config, with the Keychain
item containing `username:password` rather than a plaintext secret in JSON:

```json
"smtp": {
  "host": "smtp.example.com",
  "port": 587,
  "from": "tidemux@example.com",
  "to": "you@example.com",
  "keychain": {"service": "com.example.tidemux.smtp", "account": "default"}
}
```

Run the generate/deliver commands from a user-owned macOS `launchd` job at the
desired daily time. If the machine is offline or asleep, launchd will run it at
its next opportunity; TideMux records only the actual generated report and does
not fabricate a missed balance snapshot.

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
