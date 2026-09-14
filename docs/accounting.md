# Request accounting and reconciliation

TideMux records local request outcomes and estimates cost from provider-reported
usage. It does not fetch provider invoices or charge users.

## What is recorded

The SQLite database has the following active tables:

| Table | Purpose |
| --- | --- |
| `request_audit` | One terminal record per admitted-to-processing request, including requests canceled while queued |
| `audit_events` | Ordered events such as queue waiting and upstream rate limiting, committed with the request |
| `local_diagnostics` | Rejections before upstream processing, such as invalid authentication, unsupported endpoints or invalid parameters |
| `token_comparisons` | Optional local tokenizer counts keyed to an audited request; never stores request text |
| `balance_snapshots` | Explicit account-balance observations for account-level variance diagnostics |
| `supplier_statement_lines` | Imported supplier statement rows, kept separate from local estimates |

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
The gateway's profile supplies the upstream context. Successful requests retain
a copy of the configured price with their record, so later configuration changes
do not rewrite history. There is no automatic price discovery, time-of-day rate
switching, currency conversion or provider discount calculation.

Missing usage, missing required rates or ambiguous cache breakdowns produce
`null`, not zero. Current failed/canceled attempts do not retain partially
observed streaming usage or a cost estimate; they may still incur provider costs.
Cost calculations use floating-point numbers and SQLite REAL, suitable for
estimation, not exact financial settlement.

## Inspect and reconcile

```sh
tidemux ledger
tidemux ledger --diagnostics
# For another profile:
tidemux ledger --config /absolute/path/to/profile.json
```

The CLI returns the latest 100 records as JSON. Use read-only SQLite queries for
larger periods; preserve unknown-cost counts and group by currency rather than
silently treating nulls as free or adding different currencies.

Validation compares each response's request ID and usage with its audit record,
then independently recomputes cost from the recorded rates. Tests cover unknown
usage, failures, cancellation, queueing and atomic writes. Real DeepSeek acceptance
also compared response/cache usage and independently calculated Decimal estimates
with recorded results. Those runs verified selected configured prices; they did
not implement automatic peak/off-peak pricing or reconcile supplier invoices.

## 0.1.1 reconciliation model

`tidemux reconcile import --config /absolute/path/to/profile.json --file statement.csv`
imports a normalized supplier statement. The first supported CSV format is:

```text
period_start,period_end,currency,amount,request_id,model
2026-09-14T00:00:00Z,2026-09-15T00:00:00Z,USD,0.0134,tidemux-request-id,deepseek-flash
```

The first four columns are mandatory. Times must be RFC3339 (or positive Unix
milliseconds), `amount` must be a non-negative finite number, and the end must
follow the start. `request_id` and `model` are optional. A row without a known
TideMux `request_id` is explicitly reported as unmatched: TideMux never guesses
that a statement charge belongs to a local request. Re-importing a file imports
new lines, so users should retain the source file and avoid accidental repeats.

`tidemux reconcile report --config /absolute/path/to/profile.json --from
2026-09-14T00:00:00Z --to 2026-09-15T00:00:00Z` returns one row per currency
with local estimated cost, imported statement amount, statement-minus-estimate
difference, known/unknown statement links, requests with unknown local cost and
tokenizer comparison counts. It does not combine currencies or call an estimate
an actual supplier charge when no statement has been imported.

The reconciliation schema also has append-only records for a local tokenizer
measurement and account-balance snapshots. The tokenizer record contains only
counts, tool/version and request ID; provider API usage remains the request
settlement truth. Balance net changes are account-level diagnostics only:
top-ups, grants, expiry and other API clients may alter a balance. They are never
booked as TideMux spending.

## Legacy data

`ledger_requests`, `ledger_events` and their old `Summarize` view remain for
historical compatibility. The active gateway writes `request_audit`/`audit_events`,
and the CLI reads those newer records. Do not use the legacy fixed-currency summary as an
aggregate of current traffic. Historical currency-specific database identifiers
are preserved to avoid reinterpreting old amounts or breaking compatibility. Existing data is retained during upgrades.
