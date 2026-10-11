# Provider and model availability

Query the running gateway with its gateway credential:

```sh
tidemux gateway availability
tidemux gateway availability --json --config /path/to/config.json
```

The read-only endpoint is `GET /tidemux/availability-status`, protected by the
same gateway authentication as inference. The CLI reads only the gateway
Keychain item and contacts the configured loopback listener. Queries send no
upstream requests, do not probe recovery and do not renew state retention.

Each provider reports its connection `generation`, provider state and observed
model states. An absent model is `unknown`. `available` means a completed
inference succeeded; model discovery is not inference evidence. `cooling`
remembers a confirmed failure. When `probe_due` becomes true, the next eligible
real request can test recovery. `probing` means one such request owns the claim.
An unresolved protocol reports `unavailable` with `protocol_unresolved`.

`reason` is a fixed classification; `last_success_at` and `last_failure_at`
are observation times. Missing timestamps have no observation. `retry_at` is
the earliest recovery eligibility time, not a promise that the provider has
recovered. `consecutive_failures` counts confirmed failures of that scope,
including unsuccessful recovery probes, and resets on successful inference.

| Evidence | Scope | Initial cooldown before jitter |
| --- | --- | --- |
| Classified `model_not_found` | Provider/model | 30 seconds |
| Classified `temporarily_unavailable` | Provider/model | 30 seconds |
| Classified `insufficient_balance` | Provider | 5 minutes |
| Proven connection establishment failure before request headers | Provider | 30 seconds |
| Authentication failure or 429 | Existing key pool | Existing key cooldown / Retry-After |

Capacity, local budgets, validation/history errors and client cancellation do
not establish an upstream failure. Unclassified errors do not add a new scope.
An attempted recovery probe that cannot confirm success keeps its prior failure
classification and renews backoff. A locally rejected or canceled probe releases
the claim without extending failure history.

Success restores future eligibility. Repeated failed probes double the base
delay up to 30 minutes and add positive jitter of 0–20% (at most 36 minutes).
Longer upstream waiting instructions take precedence, bounded to 24 hours.
There is no timer that marks a route healthy and no background inference traffic.
With traffic, a due target can recover on its next eligible request; without
traffic it remains unconfirmed. Routing order, random choice, price priority and
healthy session bindings determine whether that request selects the target.

The same state applies to auto-chain, shared-model random and price-priority
routes, with or without a session ID, and explicit `REF/MODEL` requests. Selection
and the dispatch boundary after queuing recheck availability. Explicit cooling
targets return HTTP 503 with a safe scope/reason code and `Retry-After`; use the
query to inspect details. Dynamic selection can choose again after a proven
pre-dispatch conflict. Existing retry and billing-failover rules still govern
actual upstream attempts. Partially emitted SSE responses are not replayed.

Compatible reloads preserve observations and healthy semantic bindings. A
compatible in-flight recovery claim remains held until its request finishes.
Its old result only releases the claim; it cannot confirm new-epoch health.
Old requests keep their admitted configuration,
credentials, prices and accounting snapshot; their results cannot overwrite a
new configuration epoch. Connection/credential changes and removal/re-addition
reset the affected generation. Restart resets all in-memory availability.

Observed targets share a limit of 4,096 entries and a 24-hour idle TTL. Model
labels are retained only for ASCII identifiers of at most 128 bytes. At the
limit, the oldest entry outside an active probe can be evicted and becomes
unknown. Query results therefore describe retained observations, not a permanent
history of all models. Active probes are retained until their request finishes.
Transition logs use `event: "availability_transition"`, fixed states/reasons,
provider/model identifiers and generation; no provider error text is retained.

See [routing and capacity](clients.md), [accounting](accounting.md) and
[runtime logs](runtime-logging.md).

## Per-key diagnostics

Each provider includes a `key_pool` with capacity, eligible/cooling counts,
assigned opaque labels, cooldown expiry and the last fixed failure class.
Labels are random assignments, independent of credentials and Keychain names.
Eligibility reflects the key cooldown; it does not prove model or provider
health. A successful same-key retry can still retain an existing key cooldown.

Optional counters are enabled by default. Change them without restarting:

```sh
tidemux gateway configure --health-diagnostics=false
```

Set `health_diagnostics` to `true` (or omit it) to enable them. Disabling counters
preserves key choice, cooldowns, provider/model availability, budgets and audit
records. The query still reports pool capacity, labels and operational cooldown
state; `counters` are absent and `recent_failovers` is empty.

| Counter | Meaning within the current counter epoch |
| --- | --- |
| `requests` | Adapter calls that initiate at least one HTTP attempt with this key; one call can count once on multiple keys. |
| `http_attempts` | Initiated HTTP `Do` calls, including each same-key 429 retry and each failover attempt. |
| `successes` | Attempts whose response or stream validates and completes successfully. |
| `failures` | Other unsuccessful attempts, excluding cancellation and timeout. |
| `cancellations` / `timeouts` | Attempts ended by cancellation or timeout, counted separately. |
| `in_flight` | Initiated attempts whose response/stream processing has not completed. |

Skipped cooled candidates, validation failures, local capacity/budget rejection
and cancellation before HTTP initiation add no attempts. Counts describe the
adapter call and are not cross-provider gateway root-request counts. SQLite
and JSONL retain their existing authoritative audit/usage semantics.
`http_attempts` equals completed outcomes plus `in_flight` in an uninterrupted
epoch; counters saturate at the unsigned 64-bit maximum.

A recent failover records only an actual switch between attempted keys, with
opaque `from`/`to` labels and a fixed reason. Same-key retries and skipped
candidates add no decision. Each provider retains at most 32 decisions for one
hour; queries prune expiry without refreshing retention. Key state has one slot
per configured credential and no registry of retired labels.

Compatible policy reloads preserve labels, counters and cooldowns. Connection,
protocol, API-version or credential changes create a new pool and generation;
removal/re-addition and process restart also reset labels/counters. Changing the
telemetry flag clears counters/history and increments `counter_epoch` while
preserving labels and cooldowns. Requests already in flight finish against their
old counter objects and cannot write the new window. A retry initiated after a
reset can therefore have an attempt with zero new-window `requests` if that
adapter call first used the key before the reset. These are process-local
observations, not persistent accounting totals.
