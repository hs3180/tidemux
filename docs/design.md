# Core design

The [user guide](guide.md) owns setup and operations. CLI help owns flags;
[CONTRIBUTING](https://github.com/hs3180/tidemux/blob/main/CONTRIBUTING.md) owns validation and releases.

```text
client -> gateway (view, authentication, route, admission)
       -> adapter (HTTP, conversion, stream) -> SQLite audit + JSONL summary
```

`cmd/tidemux` owns setup, launchers and startup. `internal/gateway` owns immutable
views, routing, availability, capacity and reservations. `internal/adapter` owns
upstream calls and settlement; ledger/observability/limiter own persistence,
canonical events and shared admission.

## Configuration and requests

Setup atomically saves validated configuration with Keychain references. The
watcher resolves credentials and prepares a complete provider/routing view;
failed validation keeps the previous view. Saved intent and applied state differ.
Admitted requests retain their endpoint, credentials, protocol, prices and budget
snapshot while queued or in flight.

Compatible policy reloads reuse connections, key pools and eligible bindings.
Endpoint/protocol/API-version/key changes and removal/re-addition create a new
connection generation. Publication advances an epoch; old requests may finish
settlement but cannot overwrite current health or routing. A compatible active
probe keeps its claim until the old request ends; its old result only releases
ownership. Local prompt-prefix estimates use the admitted generation, are bounded
to 256 entries/16 MiB with five-minute idle expiry, and reset on restart.

## Routing and recovery

Explicit routes, ordered auto chains and shared random/price selection use the
same eligibility state and recheck it after the concurrency queue. Healthy
bindings survive when their provider/model stays eligible. Random/auto affinity
requires a stable caller/protocol/session identity; random also includes the
bare model. Bindings retain hashes, expire after 24 idle hours and reset on
restart. Price priority has no affinity. Capacity is an admission check.

Model-not-found and temporary unavailability affect a provider/model (30-second
initial cooldown). Insufficient balance affects a provider (five minutes);
proven pre-header connection failure also affects a provider (30 seconds).
Authentication/429 retain key scope. Local rejection, cancellation and
unclassified errors do not create upstream failure evidence.

After cooldown, at most one real request probes a target. Failed probes double
backoff to 30 minutes plus 0–20% jitter; longer upstream waits win, bounded to
24 hours. Success restores eligibility for later requests. There is no background
inference or timer declaring health. Observations are capped at 4,096 targets,
24-hour idle retention and 128-byte ASCII model labels; active probes are retained.

Gateway/provider session ceilings count logical conversations separately from
request concurrency. Within a provider, 429 retries remain on one key for at most
three total attempts; eligible auth and proven pre-write failures can try another.
No key/provider replay follows downstream output. Safe mapped billing failover
is opt-in and bounded; dispatched auto/shared-session calls retain their route.

## Accounting and observability

Each adapter call has one terminal audit; cross-provider attempts have separate
calls. Queued cancellation is recorded without upstream work. Request/events
commit together; final SSE completion waits for audit persistence. Budget pending
attempts survive crashes as unknown charges, fail closed when unresolved and
settle against the admitted provider/policy. Operational counts add no usage rows.
Prices/currencies stay with history; missing usage/cost is unknown, and local
estimates or supplied statement CSVs are not automatically fetched invoices.

Optional key counters observe initiated HTTP calls and complete outcomes; labels
are assigned independently of secrets. One state slot exists per configured key,
and each provider retains at most 32 failover decisions for one hour. Generation
or telemetry-epoch changes fence old completions; disabling counters preserves
operational routing/cooldowns. Status queries are authenticated and read-only.

The central logger supplies schema 2, service version, a random process identity
and unique event ID to all runtime events. Identity failure stops startup without
a colliding fallback. Terminal events are encoded once and copied unchanged to
stderr/session files. Known usage uses the Claude-compatible assistant envelope;
other events add no assistant usage. `event_id` deduplicates copies/replay;
`requestId` correlates events. SQLite is authoritative for accounting. Logs and
queries omit credentials, bodies, raw session IDs and provider errors.
