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

Server-supplied provider waits have a separate identity: a continuously
configured provider name with the same resolved protocol and endpoint shares
its deadline, observation version and recovery claim across credential changes
and admitted views. A late Retry-After still pauses current and queued requests
after policy reload or key rotation, including during view preparation. Changing
the endpoint/protocol, removal/re-addition or process restart starts new server
wait state. This does not relax epoch/generation fences for other health results.

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
Authentication retains key scope. A 429 without a valid server wait cools its
key and leaves healthy peers eligible. A valid upstream `Retry-After` on a
retryable failure pauses the entire provider, across all keys and models.
Once all keys are cooling, the provider also becomes ineligible.
Local rejection, cancellation, terminal client errors and conversion failures
do not create upstream failure evidence. Upstream 5xx, interrupted streams and
upstream timeouts affect only the provider/model. An explicit unclassified SSE
error also temporarily avoids that provider/model for 30 seconds plus jitter;
its diagnostic category and retryability stay unknown, and repeated unknown
errors do not lengthen this provisional wait. Other models remain eligible.

After cooldown, at most one real request probes a target. Failed probes double
backoff to 30 minutes plus 0–20% jitter. Explicit server deadlines are retained
separately and are not shortened to the 24-hour local backoff bound.
Success restores eligibility for later requests. There is no background
inference or timer declaring health. Observations are capped at 4,096 targets,
24-hour idle retention and 128-byte ASCII model labels; active probes are retained.

Rate limits without a valid upstream wait start at 30 seconds and double to
30 minutes plus 0–20% jitter. A valid upstream `Retry-After` supplies an absolute
provider-wide pause, independent of key backoff and client retry cadence. An
older in-flight success cannot erase that pause; a shorter concurrent wait
cannot reduce it. The 11th consecutive key rate limit silences that key for 24 hours.
The provider streak counts actual rate-limited attempts across keys; if it
exceeds ten while every key is cooling, all keys enter a 24-hour silence.
A successful request resets its key streak and, when no newer provider failure
has occurred, the provider streak. Older successes and old routing epochs cannot
erase newer failures. Operational streaks do not depend on optional telemetry.
Only one real request probes a rate-limited provider after silence, even if its
idle availability observation has expired; cancellation releases that claim.
The dispatch check also claims recovery if a wait appeared and expired while
the request was queued; sharing a server wait also shares this single probe.

Gateway/provider session ceilings count logical conversations separately from
request concurrency. The optional global request gate is disabled when
`max_in_flight` is zero or omitted; new configurations default to zero. Positive
values share a single gate across providers and queue until a slot is available
or the caller cancels. Changing this process-wide setting requires a restart.
Session admission, budgets, cancellation and terminal accounting still apply
with the request gate disabled.

TideMux dispatches at most one upstream generation request for each downstream
generation request. After dispatch, it does not wait for a retry, retry a 429,
switch keys, or fail over to another provider. It returns the mapped upstream
result with bounded gateway retry advice; the downstream client owns
retry timing and submits each retry as a new request. Key and route health can
influence selection for later requests, but never replay the current request.
The next request on a failed shared-model or auto session selects an eligible
alternative under the same model/capability and admission policies. A healthy
binding remains stable. An explicitly qualified provider stays explicit; without
an eligible alternative, cooldown is reported rather than bypassed.

Local temporary cooldowns use HTTP 429 and native rate-limit error types.
Route eligibility is the maximum independent gate for each route, then the
minimum across compatible routes. This is reported separately from client wait.
An eligible alternative yields a two-second client wait; otherwise known client
waits are capped at 30 seconds. The full server deadline remains in provider
state and is rechecked after queueing, before any HTTP attempt. An active probe
makes its completion time unknown and never supplies a synthetic countdown.
Errors distinguish the wait source, bounded client retry advice and eligible
model/protocol/health alternatives. A new request rechecks admission and budgets.
After SSE headers are committed, guidance and request IDs belong in the terminal
error event. Client SDKs may ignore custom retry fields or replace the message.

If a route becomes unavailable while queued, the adapter can yield before
initiating HTTP. The gateway releases that route's resources and reselects within
the configured-provider bound. This internal step emits no separate audit or ID;
the final attempt keeps the original start and accumulated queue time. An
exhausted request has one local diagnostic; queue cancellation keeps its terminal
adapter audit. A request that may have dispatched upstream never takes this path.

Availability shows provider/model scope, remaining known wait and model-health
alternatives; request-specific features and admission remain request checks.
The embedding binary supplies X-TideMux-Version independently of saved config;
compatible reloads retain that process version.

## Accounting and observability

Each downstream generation request has one terminal adapter audit and at most
one initiated upstream generation attempt. Queued cancellation is recorded
without upstream work. Request/events
commit together; final SSE completion waits for audit persistence. Budget pending
attempts survive crashes as unknown charges, fail closed when unresolved and
settle against the admitted provider/policy. Operational counts add no usage rows.
Prices/currencies stay with history; missing usage/cost is unknown, and local
estimates or supplied statement CSVs are not automatically fetched invoices.

Optional key counters observe initiated HTTP calls and complete outcomes; labels
are assigned independently of secrets. One state slot exists per configured key;
legacy failover history is bounded to 32 records per provider for one hour. The
single-attempt policy does not create in-request key failover records. Generation
or telemetry-epoch changes fence old completions; disabling counters preserves
operational routing/cooldowns. Status queries are authenticated and read-only.

The central logger supplies schema 2, service version, a random process identity
and unique event ID to all runtime events. Identity failure stops startup without
a colliding fallback. Terminal events are encoded once and copied unchanged to
stderr/session files. Known usage uses the Claude-compatible assistant envelope;
other events add no assistant usage. `event_id` deduplicates copies/replay;
`requestId` correlates events. SQLite is authoritative for accounting. Logs and
queries omit credentials, bodies, raw session IDs and provider error messages.
Failed calls carry the same normalized `failure` metadata in the audit JSON,
terminal event and client error. Upstream HTTP status and embedded SSE error
status are separate from downstream status: a failed open stream can still have
downstream HTTP 200. HTTP trace phases use bounded names and never retain host,
address or validation text. Unknown classifications remain unknown. Safe code
retention and provider request IDs follow the [privacy contract](../PRIVACY.md).
