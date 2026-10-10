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

Compatible reloads preserve observations and healthy semantic bindings while
releasing old recovery claims. Old requests keep their admitted configuration,
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
