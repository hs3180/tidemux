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

Named providers cache per-model token limits from their bounded, redirect-free
`GET /models` catalog at startup. Connection changes fetch a new catalog;
policy-only reloads reuse it, while admitted requests keep their original view.
There is no periodic refresh or inference probe; restart to refresh an unchanged
connection. Incomplete/unrecognized catalogs supply no limits. Accept integer
`context_length`/`max_model_len` and `max_output_tokens`, plus Anthropic catalog
`max_input_tokens`/`max_tokens`. Invalid or contradictory values remain unknown.
Nonzero operator `model_capabilities` fields take priority per field; omit a
discovered field that contradicts the configured field. Startup/applied reload
warnings identify unknown contexts, invalid metadata and configuration conflicts
without retaining response bodies. No limits are inferred from names or 4xx text.

Both discovery protocols publish effective limits for each qualified model.
Launchers use that running catalog for a selected provider/model and omit any
single candidate's limits for shared bare models. A discovered output limit can
reduce the generic 4,096-token OpenAI-to-Anthropic default, never increase it;
explicit configured defaults and caller-supplied output budgets retain priority.

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
An exact `quota_exhausted` mapping (or the standard `insufficient_quota` code)
keeps quota exhaustion distinct from balance errors and temporary rate limits.
It retains HTTP 429, provider scope and any full server wait; absent a wait,
request-driven recovery starts after five minutes with bounded backoff.
`provider_access_denied` and `fair_use_throttled` are explicit, vendor-neutral
provider-scoped terminal mappings. The former translates subscription access
rejection to permission semantics; the latter remains distinct from quota and
rate-limit recovery and always carries conditional account/key investigation
and manual-unlock guidance. Both reuse the five-minute provider recovery and
bounded failed-probe backoff. Shared/auto recovery can only select a healthy
alternative on a new request; explicit and fully restricted routes stay terminal.
Local gates retain distinct restriction codes, rather than relabeling fair use
or entitlement as quota. Mixed terminal restrictions use a generic restriction
code without a fabricated cause. This policy never infers a reset/unlock time.
Numeric vendor codes have no global meaning and require a provider mapping.
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
Server-supplied provider waits have a separate identity: a continuously
configured provider name with the same resolved protocol and endpoint shares
its deadline, observation version and recovery claim across credential changes
and admitted views. A late Retry-After still pauses current and queued requests
after policy reload or key rotation, including during view preparation. Changing
the endpoint/protocol, removal/re-addition or process restart starts new server
wait state. This does not relax epoch/generation fences for other health results.

Only one real request probes a rate-limited provider after silence, even if its
idle availability observation has expired; cancellation releases that claim.
The dispatch check also claims recovery if a wait appeared and expired while
the request was queued; sharing a server wait also shares this single probe.

### Session capacity

`providers.<ref>.max_active_sessions` is the only logical-session ceiling; zero
or omission makes that provider unlimited. Each reference counts its own
caller/protocol/session identities across models and keys. A session using two
providers occupies a slot in each; there is no overall session limit. Unlimited
providers still track occupancy so lowering a cap preserves admitted sessions.
Compatible reloads and removal/re-addition keep retained and in-flight entries.
Concurrent requests in one session share its slot. Successful non-stream calls
retain the session until both input and output are idle for the configured
`active_session_idle_timeout_seconds` (zero selects five minutes). Active calls
do not expire; streaming completion, cancellation and failures release their
request's reference without removing another request's retained state. Anonymous
requests use request-scoped identities and never retain a conversation.

Logical sessions are separate from concurrent request execution. The optional
global request gate is disabled when
`max_in_flight` is zero or omitted; new configurations default to zero. Positive
values share a single gate across providers and queue until a slot is available
or the caller cancels. Changing this process-wide setting requires a restart.
Session admission, budgets, cancellation and terminal accounting still apply
with the request gate disabled.

### Dispatch and recovery

TideMux dispatches at most one upstream generation request for each downstream
generation request. After dispatch, it does not wait for a retry, retry a 429,
switch keys, or fail over to another provider. It returns the mapped upstream
result with bounded gateway retry advice; the downstream client owns
retry timing and submits each retry as a new request. Key and route health can
influence selection for later requests, but never replay the current request.
For stable shared-random/auto bindings, the next request compares the remaining
maximum independent cooldown against an admitted threshold: model override,
provider override, routing override, then 60 seconds. Known waits strictly above
the threshold can select a healthy compatible alternative; short/equal waits and
unknown probe completion retain affinity. A known lower bound can justify a
switch despite another unknown gate. No eligible alternative retains the binding;
zero explicitly requests the earlier immediate eligible-route policy. Price
priority applies the comparison to its ordered preference without adding affinity.
Fresh random/auto sessions retain ordinary eligible selection. Auto retains its
configured model choices; bare shared routing keeps the same model. A healthy
binding remains stable. An explicitly qualified provider stays explicit; without
an eligible alternative, cooldown is reported rather than bypassed.

Local temporary cooldowns use HTTP 429 and native rate-limit error types.
Route eligibility is the maximum independent gate for each route, then the
minimum across compatible routes. This is reported separately from client wait.
When policy permits reselection, an eligible alternative yields a two-second client wait; otherwise known client
waits are capped at 30 seconds. The full server deadline remains in provider
state and is rechecked after queueing, before any HTTP attempt. An active probe
makes its completion time unknown and never supplies a synthetic countdown.
Errors distinguish the wait source, bounded client retry advice and eligible
model/protocol/health alternatives. A new request rechecks admission and budgets.
Recoverable HTTP errors send `x-should-retry: true`; bounded integer
`Retry-After` and `retry-after-ms` describe the same client cadence. Known
terminal errors send `false` without a wait. Confirmed quota exhaustion allows
automatic retry only when the requested shared/auto route has an eligible
alternative; explicit selection and routes all blocked by quota stay terminal.
After SSE headers are committed, guidance and request IDs belong in the terminal
error event. Client SDKs may ignore custom retry fields or replace the message.

Cooling bindings survive compatible reloads. The selector retains them until a
new request makes the threshold decision; failure feedback never eagerly deletes
an auto binding under unified availability. Generation/epoch fences still apply.
A retained choice also constrains retry advice to that route, so a short wait does
not promise immediate reselection. The post-queue yield path re-evaluates health
and thresholds without adding an attempt/audit; a retained cooldown exits locally.
Healthy random/auto sessions are not moved back by another provider's recovery.

Routing decisions contain only bounded provider/model references, action/reason,
threshold and known wait/deadline. Terminal audit JSON and JSONL share the same
normalized snapshot. Local rejection plus optional decision details commit in one
transaction using an additive `local_routing_decisions` table; the historical
local-diagnostic table shape is unchanged. Read-only inspection also handles old
databases without the optional table. Decisions add no generated usage or cost.

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
commit together; SSE stop/finish reasons and terminal markers wait for audit
persistence. Ordinary content remains incremental; the ordered completion tail
uses the configured stream byte limit. Budget pending
attempts survive crashes as unknown charges, fail closed when unresolved and
settle against the admitted provider/policy. Operational counts add no usage rows.
Prices/currencies stay with history; missing usage/cost is unknown, and local
estimates or supplied statement CSVs are not automatically fetched invoices.
If audit persistence fails after dispatch, the terminal outcome is error even
when the upstream completed successfully. Preserve known usage/cost and upstream
health evidence independently. Before headers are sent, return `x-should-retry:
false` without a retry wait. The error's `manual_check` strategy permits zero
automatic attempts; inspect the request ID/accounting state before resubmitting.
An open SSE response carries that guidance in its terminal error, with no success
completion or late-header claim. Final response metadata replaces any earlier
retry decision before the terminal event is logged.
The shared ledger latches an audit failure for the process lifetime. Later
generation requests are rejected locally as `audit_unavailable` before dispatch,
including queued calls and clients created by compatible reloads. Existing
budget guards retain priority; unattempted calls release their reservations.
Provider health is independent. Already-dispatched calls can still finish; a
later successful write does not repair missing accounting or clear the latch.
Reconcile the failed request and repair storage, then restart to resume work.

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
queries omit credentials, full bodies and raw session IDs. Provider error reasons
follow the bounded/redacted message policy below.
Failed calls carry the same normalized `failure` metadata in the audit JSON,
terminal event and client error. Upstream HTTP status and embedded SSE error
status are separate from downstream status: a failed open stream can still have
downstream HTTP 200. HTTP trace phases use bounded names and never retain host,
address or validation text. Unknown classifications remain unknown. Safe code
retention and provider request IDs follow the [privacy contract](../PRIVACY.md).
HTTP and explicit SSE errors retain the provider's bounded reason in the primary
`error.message`, where SDKs and agents read it. Explicit category mappings have
priority; otherwise standard API type/code identifiers supply classification.
Recognized native codes occupy `error.code` with the gateway code separately in
`gateway_code`; mapped vendor codes retain the category code and `provider_code`.
Numeric vendor codes alone never imply a category. Unknown identifiers remain
redacted. The per-provider `forward_error_messages` policy defaults to true;
false omits text from clients and all persistence. Legacy single-provider configs
support the same field at the root. Policy reloads retain key/connection identity,
and admitted calls keep their original policy and credential redaction set.

Known admitted credentials and raw sessions are removed before message bounds;
credential shapes, control/format characters and terminal escapes are cleaned.
Reasons contain at most 512 Unicode characters. Explicit, consistent integer token
comparisons yield request-local `actual_tokens` and `limit_tokens` even when text
is disabled or truncated. Ambiguous or overflowing counts remain unknown; no
catalog or configured capability is changed. The [privacy contract](../PRIVACY.md)
describes residual free-text risk and strict-policy migration.

Each initiated attempt persists an independently assigned `key_label` and
`backend_id` (the admitted provider connection, not an asserted physical server).
The selected candidate's real pool slot supplies its existing status label;
round-robin reordering never substitutes the candidate ordinal. These opaque
labels do not depend on secrets or optional counters, survive compatible policy
reloads and reset with the connection/key pool or process. Upstream physical
backend identity remains unknown unless the provider supplies correlation;
request IDs support provider-side investigation without copying arbitrary headers.
The audit JSON can grow these optional fields without altering historical rows
or migrating SQLite tables. Schema 2 retains the canonical `message.model`.

After health feedback, the gateway freezes its response retry decision before
the adapter commits the terminal audit. `failure.response_retry_headers` retains
only the generated retry boolean and bounded second/millisecond waits; the
response, audit and terminal event use that same snapshot. An empty object
records absent retry headers, including an already-open SSE response. Later
requests still use current routing state. Arbitrary upstream headers are excluded.
