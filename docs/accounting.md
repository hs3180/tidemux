# Request accounting and reconciliation

TideMux records local request outcomes and estimates cost from provider-reported
usage. It does not fetch provider invoices or charge users.

## What is recorded

The SQLite database has three active tables:

| Table | Purpose |
| --- | --- |
| `request_audit` | One terminal record per admitted-to-processing request, including requests canceled while queued |
| `audit_events` | Ordered events such as queue waiting and upstream rate limiting, committed with the request |
| `local_diagnostics` | Rejections before upstream processing, such as invalid authentication, unsupported endpoints or invalid parameters |

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

Actual invoice reconciliation would additionally require provider request IDs
and billing exports, accounting periods, rounding/discount rules, and a workflow
for resolving unmatched or unknown attempts. These are not implemented in 0.1.0.

## Legacy data

`ledger_requests`, `ledger_events` and their old `Summarize` view remain for
historical compatibility. The active gateway writes `request_audit`/`audit_events`,
and the CLI reads those newer records. Do not use the legacy CNY summary as an
aggregate of current traffic. Existing data is retained during upgrades.
