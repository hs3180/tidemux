# Core design

TideMux runs as a local gateway with a process-wide SQLite ledger. Client
authentication, provider selection, upstream attempts and accounting are
separate stages. The operational surface is documented in the
[provider CLI](provider-cli.md), [availability](availability.md),
[protocols](protocols.md) and [accounting](accounting.md) manuals.

## Configuration publication

Configuration files contain Keychain references, not credentials. Setup writes
validated files atomically. The watcher resolves credentials and prepares a new
provider/routing view before publication; failed validation retains the last
valid view. Configuration application status distinguishes saved intent from
the running view.

Every admitted request uses one immutable view. It keeps the selected endpoint,
credentials, protocol, model scope, prices, budget policy and accounting
snapshot while queued or in flight. Compatible connections can reuse clients,
catalogs and key pools. Endpoint, protocol, API-version or credential changes
assign a new connection generation and isolate prompt-cache state. Deleted and
re-added providers get a new generation.

Publication advances a routing epoch. Semantic session bindings can survive a
compatible view when their provider/model is still eligible. Old requests may
finish their own audit and reservation settlement; they cannot overwrite
current routing preferences or availability observations. An active recovery
probe keeps its claim across a compatible reload until the old request ends;
its old result releases the claim without confirming new-epoch recovery.

## Routing and availability

Explicit `REF/MODEL` selects that profile and strips the first reference prefix.
A bare model requires an unambiguous configured scope unless shared selection
opts into random or price priority. `model: auto` uses an ordered instance
chain. Client protocol does not choose the provider; adapters convert only
when necessary and supported.

Healthy session bindings take priority within their routing rules. A confirmed
failure excludes the affected target from later eligible selections without
replaying the failed request. Model-not-found and temporary unavailability are
model-scoped; insufficient balance and proven connection establishment failure
are provider-scoped. Authentication and rate limits retain key scope.

Selection and dispatch share the same bounded availability state. Dispatch
rechecks after the concurrency queue, so a failure observed while waiting can
block a stale selection before HTTP. Cooling targets become eligible for one
request-driven probe after their retry time. Backoff/jitter, scope, generation
and quantity/idle limits are defined in [availability](availability.md). Model
discovery and expired cooldowns do not confirm inference recovery. There is no
background model traffic.

## Sessions, concurrency and attempts

The global request gate bounds simultaneous upstream work. Gateway and provider
logical-session ceilings independently limit active conversations, retaining
idle bindings for their configured TTL. Admission and local capacity/budget
rejections occur without an upstream attempt. A request queued under an older
view retains that view, subject to the final health check.

Within one provider, the key pool orders eligible credentials and rechecks
cooldowns before later candidates. 429 retries stay on the same key for at most
three total attempts. Eligible authentication failures and safe connection
failures can switch keys. Once downstream bytes may have been written, no key
or provider replay is permitted.

Optional per-key diagnostics observe each initiated HTTP `Do` and its complete
response/stream outcome. Same-key retries are separate attempts; cooled skips
and pre-dispatch failures are excluded. Opaque labels are assigned independently
of secrets. Counters and a bounded recent failover ring are process-local,
generation-bound observations. Disabling telemetry preserves routing and
cooldowns. The authenticated status query never sends upstream requests.

## Accounting and logs

A logical adapter call produces one authoritative audit even if it retries or
switches keys. Cross-provider routing creates separate adapter calls and audits;
operational key counts do not invent a gateway root request. Budget reservations
and settlement remain attached to the admitted provider/policy snapshot. A
settlement failure blocks further budgeted work until resolved.

Known usage is recorded with its price/currency snapshot. Missing usage or
cost remains unknown. Supplier CSV reconciliation compares recorded estimates
with statements; it does not infer invoice truth from HTTP success.

The central logger adds schema version, service/version, a random process
instance and unique event ID to every runtime event. Each terminal request is
encoded once and copied unchanged to stderr and its session JSONL file. Known
usage uses the Claude-compatible assistant envelope; local rejection and other
diagnostics omit assistant usage, preventing ccusage double counting. Persisted
event IDs support collector replay deduplication, while `requestId` correlates
events and is not an event key.

SQLite remains the accounting source of truth. JSONL and status queries are
bounded operational summaries without credentials, bodies, raw session IDs or
provider error text. Collector access and retention are operator choices; see
[runtime logs](runtime-logging.md), [ccusage](ccusage.md) and
[Elasticsearch](elasticsearch.md).
