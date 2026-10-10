# Provider and gateway setup

The CLI uses resource-oriented commands. Provider setup, lifecycle and
provider session limits belong to `tidemux provider`; listener and gateway-wide
session limits belong to `tidemux gateway`. The legacy top-level `tidemux configure` command is not
retained. The new routing commands below require v0.3.0 or later.

## Add a provider

Guided setup starts by asking for an API Base URL:

```sh
tidemux provider add
```

TideMux reads the API key through hidden terminal input and infers the upstream
protocol from authenticated `GET /models` response schemas. URL names do not
select a protocol; when both or neither schema match, setup asks for an explicit
`--protocol openai` or `--protocol anthropic`. Model discovery is
informational and never sends a completion request. In guided setup, choose
model IDs to create an allowlist or leave the selection blank to allow all
models. If discovery is unavailable, enter IDs to restrict access or leave it
blank to allow all. A request may use a bare upstream model ID when it matches
exactly one provider's configured model scope; use `REF/MODEL_ID` to select a
provider explicitly or when multiple providers can serve the same ID. Client
protocol does not choose a provider, and setup never chooses a default.
The first guided setup also asks whether to schedule a daily usage-report
notification in local time; leave it blank to skip.

For direct setup, supply the endpoint and any known choices:

```sh
tidemux provider add https://api.deepseek.com
```

The API key is still prompted securely. The provider reference is inferred from
the endpoint; use `--name LABEL` only when you want a different reference. If
the endpoint does not identify the protocol, `--protocol openai` or
`--protocol anthropic` forces it. `--model MODEL[,MODEL...]` restricts the
provider to those IDs; omit it to allow all models. Use `tidemux provider list`
to get the provider reference. A running v0.3.1 gateway automatically applies
provider changes from its config file; wait for applied status before routing
requests to a newly added provider. In a client, use the
bare upstream model ID when its configured scope selects one provider, or use
`REF/MODEL_ID` for explicit routing.

Provider credentials are stored in macOS Keychain, never in command arguments
or configuration JSON. Existing 0.1.x single-provider configurations are
migrated to a named provider when one is added; listener, report, ledger and
gateway credential settings are retained. Remove an obsolete top-level `budget`
field before configuring a provider policy with `tidemux provider budget REF`.
Configuration changes are validated and atomically installed.

## Automatic provider application (0.3.1)

Provider automatic application was added in 0.3.1. The 0.3.2 contract also
coordinates compatible route and session state across configuration changes.

A running `tidemux serve --config PATH` polls that file and its provider
Keychain references every second. Add/update/remove, scope, credentials,
pricing, budget and provider protocol/capabilities form one immutable view.
The catalog and new requests use that view together. Routing/auto-chain changes
also use a new view; valid auto-chain and shared-model session state survive.
Already-admitted requests, including queued work and SSE, finish with their
original endpoint, keys, price/budget policy and accounting. The ledger,
concurrency gate and active-session limiter remain shared; reload never creates
another gateway or retries an admitted request.

The request selects its configuration view once at entry to `ServeHTTP`, before
body validation, route selection and session admission. A request that has
selected an older view continues to use it while waiting for admission or an
execution slot; the next request selects the current view. Invalid requests do
not gain admission. Publication changes one complete view, never individual
provider or routing fields visible to readers.

Routing first applies the current configuration's eligibility and selection
rules. Explicit provider selection, auto-chain order for unbound sessions and
`price_priority` remain authoritative. For session-aware auto-chain and shared
`random` routing, a valid active binding takes priority over connection reuse.
Adding a reusable candidate does not move an otherwise valid bound session.
When no valid binding exists, shared `random` routing prefers ready candidates
whose connection configuration was reused from the immediately preceding view.
Random selection is uniform within that tier; when it is empty, all ready
candidates participate. The tier records configuration compatibility at this
reload, not whether a live TCP connection happens to be idle. A later reload
may place a previously added provider in the reused tier. Cooldowns and
request-feature eligibility still apply; capacity limits remain admission
checks and do not create a new provider failover rule.

A successfully applied configuration removes bindings whose provider/model
was removed or whose model is outside the provider's `supported_models` scope.
An unavailable provider/client or a provider with all keys cooling down also
invalidates its bindings. Later requests select and bind an eligible route
under the current rules. A safely classified `model_not_found`,
`insufficient_balance` or `temporarily_unavailable` failure invalidates the
affected route for later requests, which avoid immediately selecting the failed
provider/model pair again. This does not replay the already-dispatched request.
If one of those classifications is confirmed after SSE output has started,
the current stream retains its route and settlement snapshot, but the invalid
binding affects the next request, including one with the same session ID.
Cancellation, unclassified errors and transport errors after output do not
themselves invalidate a binding or advance the preference.

Endpoint, protocol, API-version or resolved-key changes create a new connection
generation. Healthy bindings retain their provider/model identity when that
same pair remains configured, eligible and available; later requests use the
new view's connection and policy. Connection generations isolate prompt-cache
history and route-failure state rather than defining session identity. Removing
and re-adding a provider creates a fresh generation, but its removal has already
cleared the old bindings. Scope restrictions discard bindings for models that
are no longer permitted. Policy-only changes retain compatible transport, key
cooldown and local prompt-prefix history while using a fresh price/budget
snapshot. An older view cannot overwrite active bindings or transfer stale
failure state into a newer connection generation.

CLI provider mutations report **saved / automatic application pending** until
an authenticated gateway response acknowledges the exact saved file. They report
**applied** only after that acknowledgement. An unreachable/older gateway has
not acknowledged the change. This status is also available through authenticated
`GET /tidemux/config-status`, with `status`, `applied_revision`, `error_code`
and the last successful `credentials_checked_at` (UTC). It is read-only; no
manual reload is needed. Same-reference Keychain rotation is noticed at the next
credential sample, even when the file has not changed.

Each validation attempt has a 10-second total credential/probe deadline, with
individual model probes capped at five seconds. When no load is in progress,
a valid change applies within 11 seconds under normal local file I/O. A change
arriving during another load is checked within 21 seconds. These bounds are
attempt deadlines, not promises that an unavailable provider will become ready.
Endpoint/protocol/key changes make an authenticated, redirect-free `GET /models`
probe. Automatic protocol detection requires recognizable evidence. Explicit
protocol profiles retain their existing support for unavailable model discovery:
404/405 or an unrecognized successful catalog does not invalidate the profile;
a configured scope supplies the catalog, otherwise undiscovered models are not
listed. Scope/pricing/policy-only updates reuse the validated connection/catalog.
Transport, authentication, redirect and other unsuccessful probes remain failures.
A failed probe,
missing/invalid/duplicate key, invalid file or a file changed during validation
keeps the entire last valid view and leaves the change pending. Unchanged
providers retain clients, catalog and key cooldowns. Failures emit safe
`config_reload` events; neither secret values nor raw diagnostics are logged.

Listener, ledger location, gateway credential, limits, reconciliation and report
settings retain startup semantics. A combined edit that changes those settings
is pending with `config_requires_restart`; restore those fields to apply only
provider changes automatically. Removing every named provider is supported in a
running gateway and leaves an empty model catalog. No persisted config or ledger
schema is added by automatic application.

## Provider logical-session capacity (0.3.2)

Configure a cap for one provider without changing the optional gateway-wide
ceiling:

```sh
tidemux provider update glm --max-active-sessions 5
tidemux provider show glm
```

The persisted field is `providers.glm.max_active_sessions`. Omitted or zero
means unlimited; accepted values are 0–4096. Negative values are rejected
without changing the file. A running gateway applies provider cap changes
automatically; wait for the mutation's applied status. The gateway-wide
`max_active_sessions` retains its existing startup semantics.

A slot represents a logical conversation within one provider reference, not
a key, model or HTTP request. Repeated or concurrent requests with the same
session ID reuse one slot across that provider's models and key retries.
The gateway ceiling counts that conversation once even when it uses multiple
providers; each provider counts it separately. Identity includes the local
caller and client protocol. OpenAI uses `X-TideMux-Session-ID`; Anthropic uses
that header first, then `metadata.user_id`. Matching raw IDs in different
client protocols are separate conversations. There is no cross-protocol
session bridging. IDs and caller credentials are not exposed by capacity
diagnostics.

Requests without a stable ID are independent, concurrent slots. Their slots
are released when the request ends, including successful buffered responses;
generated IDs cannot retain a prompt prefix. Stable successful buffered
conversations remain admitted until the configured gateway idle timeout
(default five minutes). Streams, failures and cancellation release only that
request's reference. Another concurrent request or a previously retained
successful conversation is preserved; an idle sweep never expires in-flight
work. Routing affinity has its own lifetime and does not itself reserve capacity.

Both applicable limits must admit the initial request. A full provider returns
protocol-native HTTP 429 / `active_session_limit` with `scope: "provider"`,
`provider_ref` and `limit`; a full overall ceiling uses `scope: "gateway"`.
There is no `Retry-After` value because the release time is unknown; use the
response's bounded exponential backoff with jitter. Initial refusal dispatches
no upstream request and releases only newly acquired admission references.
Capacity is checked again before each target in an already-supported safe
provider failover. Refusing that target does not remove prior attempts' audit
or budget settlement and does not add a new reason to reroute a request.

Reloading a profile's cap, endpoint, keys, scope, prices or budget preserves
its actual occupancy. Reducing the cap does not cancel existing work or stop
an already admitted conversation from reusing its slot. New distinct sessions
must meet the new cap. Unlimited providers still track active/retained logical
sessions so enabling a cap counts existing work. Removing and re-adding the
same reference cannot erase retained or in-flight occupancy. Renaming the
reference creates a separate capacity scope.

Authenticated `GET /tidemux/session-status` returns `gateway` and `providers`
entries with `limit`, `current` and `rejected`. A disabled gateway-wide limiter
reports zero current sessions; provider counters still track unlimited profiles.
Provider `current` reflects distinct conversations after idle cleanup. Rejection
counters last for the lifetime of that capacity scope; retired empty scopes may
be removed. The endpoint is read-only and contains no session IDs or usage.

## Add API keys to a provider

A named provider profile is also its API-key group: all keys in the group share
one endpoint, protocol, model scope and budget. The initial
`provider add` flow creates the first key. Add or inspect additional keys with:

```sh
tidemux provider key add REF
tidemux provider key list REF
tidemux provider key remove REF INDEX
```

`key add` reads the new secret through hidden terminal input, stores it in
Keychain and atomically appends only its reference to the profile. Keys are
selected round-robin per request; one selected key is retained for the entire
response stream. Authentication failures and transport failures known to have
happened before request headers were written can try the next ready key within
the same group. HTTP 429 first retries the same key at most three total
attempts; it does not switch keys to bypass the provider's wait. TideMux follows
`Retry-After` seconds or HTTP-date values when the delay fits inside the request
timeout. Otherwise it returns the rate-limit error without retrying early. An
invalid or missing header uses one-then-two-second backoff. Every 429 updates a
process-local key cooldown (minimum one second, maximum 24 hours). A safe
pre-write transport failure cools a key for five seconds and an authentication
failure for 30 seconds. If every key is cooling down, the gateway returns 503
with `Retry-After` instead of sending another request upstream. Cooldowns reset
when the gateway process restarts. A configured `supported_models` allowlist
applies to the whole group. With no allowlist, every model remains eligible.

`key list` prints numbered redacted slots, not credentials or Keychain account
details. `key remove` uses the listed index, asks for confirmation and requires
the group to retain at least one key; use `--yes` for non-interactive operation.
A Keychain item is deleted only when no remaining provider references it. The
older `provider update --rotate-key`
continues to rotate a single-key profile; use the `provider key` commands for a
multi-key group.

## Edit a provider

Use the explicit CRUD commands to inspect and change provider profiles:

```sh
tidemux provider list
tidemux provider show REF
tidemux provider update REF
tidemux provider remove REF
```

`provider update REF` opens a short, line-by-line form. Press Enter to keep a
field; enter a new endpoint or protocol override to change it, and enter
comma-separated model IDs to restrict the group or `all` to allow every model.
Changing an endpoint clears the old model allowlist unless a new list is
entered. API keys remain a separate CRUD resource:

```sh
tidemux provider key add REF
tidemux provider key list REF
tidemux provider key remove REF INDEX
```

These commands never display key material or Keychain account details. Model
scope can also be changed directly with `provider models REF --only`, `--all`,
or the single-prompt `--select` option.

The equivalent local validation command is:

```sh
tidemux provider validate REF
```

It checks that the provider's configured Keychain entries are readable and
valid, distinct from the gateway credential and unique within the group. It
makes no upstream request and never prints credential values.

## Inspect and manage providers

```sh
tidemux provider list
tidemux provider show REF
tidemux provider update REF --model model-a,model-b
tidemux provider models REF
tidemux provider models REF --select
tidemux provider models REF --only model-a,model-b
tidemux provider models REF --all
tidemux provider validate REF
tidemux provider remove REF
```

`list` and `show` never display credentials. `update` changes only the named
fields: `--endpoint URL`, `--protocol openai|anthropic`,
`--anthropic-version DATE` (Anthropic only), `--rotate-key`, and
`--model ID[,ID...]` (replace the allowlist) or `--model all` (allow all).
Without field options it opens the short form described above. Updating
credentials requires hidden terminal input. Changing an endpoint re-evaluates
its protocol by default using the stored key; combine
`--rotate-key` when the new endpoint requires a different key. Use `--protocol`
to force a wire format. Changing an endpoint clears any model allowlist,
restoring the default all-models scope.

Every request may use a bare upstream model ID when exactly one provider's
configured model scope matches. Multiple unscoped providers can serve any model,
so a bare ID is ambiguous between them. Use `REF/MODEL_ID` for explicit routing;
`REF` is the stable reference shown by `provider list`, and `MODEL_ID` is the
upstream model name (which may itself contain slashes). The client protocol does
not choose a provider. TideMux strips only the first `REF/` prefix before
forwarding an explicitly qualified request. Either client protocol can target
any provider; cross-protocol conversion is applied when needed. Provider
failures do not trigger a retry on another provider. Removal asks for
confirmation; non-interactive removal requires `--yes`. A Keychain item is
removed only when no remaining provider references it.

With no scope option, `provider models` queries and prints the authenticated
model catalog when the endpoint exposes a complete, recognizable list. Use
those upstream model IDs as the suffix in `REF/MODEL_ID`; `/v1/models` exposes
the same IDs with the provider reference included. `--only` sets an allowlist
of upstream model IDs; `--all` clears the allowlist. An unavailable model
catalog does not restrict requests unless an explicit allowlist is configured.
`--select` interactively chooses catalog entries by number or exact ID; when
discovery is unavailable, model IDs can still be entered manually. Enter `all`
to clear the allowlist or press Enter to leave the current scope unchanged.

## Provider pricing

Prices are stored per provider and model, in currency units per million tokens:

```sh
tidemux provider pricing list REF
tidemux provider pricing set REF MODEL_ID \
  --input-cache-hit 0.006 \
  --input-cache-miss 0.30 \
  --output 1.20 \
  --currency USD \
  --source https://api-docs.deepseek.com/quick_start/pricing/ \
  --version 2026-09-12
tidemux provider pricing remove REF MODEL_ID
```

All three rate flags are required when setting a price. Currency defaults to
USD; source and version identify the rate reference. A matching built-in rate
may be used when no explicit rate exists. Unknown prices remain unknown rather
than guessed. An enabled budget requires a matching price for each requested
model while that budget is active. Unpriced models remain usable when the
provider has no budget.

## Provider error-code mappings

Use exact upstream codes to give a provider-specific failure a stable client
category. The mapping is stored on one provider profile and never applies to
another provider:

```sh
tidemux provider error-map list REF
tidemux provider error-map add REF --code billing_quota --category insufficient_balance
tidemux provider error-map add REF --code key_throttled --status 429 --category rate_limited
tidemux provider error-map remove REF --code billing_quota
tidemux provider error-map remove REF --code key_throttled --status 429
```

`--status` is an optional exact upstream HTTP status from 400 through 599. An
exact code-and-status entry wins over the same code's unqualified entry;
duplicate code/status pairs are rejected. Valid categories are
`insufficient_balance`, `rate_limited`, `authentication`, `permission_denied`,
`policy_denied`, `invalid_request`, `model_not_found`, and
`temporarily_unavailable`. Provider messages are
never inspected to infer a category or returned to the client. The response
includes the protocol-native envelope, a safe actionable explanation, a stable
TideMux code, and the upstream code if it contains only safe ASCII characters.
Mapped errors retain the upstream 4xx status when it agrees with the mapped
category; rate-limit mappings always use HTTP 429. Rate-limit mappings use the
same bounded retry policy as HTTP 429.

## Model fallback and shared-model routing

Exactly one auto chain belongs to the TideMux instance. Each ordered entry
names a configured provider and a model allowed by that provider's scope:

```sh
tidemux auto-chain show
tidemux auto-chain set --entries provider-a/model-fast,provider-b/model-capable
tidemux auto-chain clear
```

`set` replaces the entire chain, and `clear` removes the optional top-level
`auto_chain` field. Entries can span providers and models; the limit is 64
distinct pairs. A valid chain change is applied automatically by a running
gateway together with its provider configuration; it reaches new requests
while already-admitted requests keep their previous configuration. Use only
`model:auto`; `REF/auto` returns `auto_model_must_be_unqualified`.

The first auto request for a stable `X-TideMux-Session-ID` (or Anthropic
`metadata.user_id` fallback) atomically binds that session to the current
preferred eligible provider/model. Later requests and agent retries keep that
pair while the binding remains valid. Removal, scope restriction, provider/client
unavailability, all-key cooldown or a safely classified route failure invalidates
the binding, and a later request binds to an eligible chain entry. TideMux
returns the original dispatched request's failure and does not replay it on
another chain entry. The selected pair appears in response metadata, usage,
prices and audit.
An explicit model request never uses the auto chain.

An exact, safe model-not-found, temporarily-unavailable or insufficient-balance
classification makes that route invalid for later requests. Existing sessions
bound to it must select again, as do unbound sessions; valid bindings to other
entries are retained. A failed pair cools down for 30 seconds, or five minutes
for insufficient balance; a longer upstream retry delay or key cooldown also
applies. After that, one later request can test the pair while concurrent
requests use another eligible route (or receive `auto_chain_exhausted`). A
successful test restores its configured preference for new bindings; a failed
test starts another cooldown. An exhausted chain therefore recovers without a
restart or configuration change. Healthy existing bindings do not move back.
A transport failure proven to precede request headers
may also advance the preference when safe and before response output.
Validation, authentication, unsupported-request errors, unclassified billing
errors and HTTP 429 do not themselves mark a provider/model pair invalid.
All-key cooldown still makes a provider unavailable for later selection.
Concurrent failures from an old entry cannot skip the next preference. When no
eligible chain entry remains, requests needing a new binding receive
`auto_chain_exhausted`; other valid bindings remain usable. The chain does not
wrap. Existing bounded 429 retries stay on the selected provider/model and honor
`Retry-After`.

Without a stable ID, every auto request receives a fresh request-scoped ID and
uses the current preference without creating a persistent binding. Stable auto
bindings use hashed caller/protocol/session keys and a refreshed 24-hour idle
TTL; restart clears bindings and resets the preference. Multi-instance
state sharing is not provided. Auto requests never replay a dispatched request
through billing provider failover. Invalidating its binding affects later
requests only.

On chain reorder, addition or partial removal, unexpired bindings retain their
eligible provider/model identity and receive its new index. Removed entries
lose their bindings; a changed connection generation alone does not remove a
healthy binding. Changing chain content or order starts new sessions at the new
first entry, including after exhaustion; an unchanged chain with unchanged
connection generations retains its preference and exhausted state. Existing valid
sessions stay bound even when their entry moves later in the chain. A failure
from a request using an older changed chain affects only that older view.

Shared bare model IDs remain ambiguous unless a strategy is enabled:

```sh
tidemux routing show
tidemux routing set --shared-model-strategy random
tidemux routing set --shared-model-strategy price_priority
tidemux routing set --shared-model-strategy off
tidemux routing set --billing-exhaustion-failover=true
tidemux routing set --billing-exhaustion-failover=false
```

The strategy applies to an unqualified bare model ID only. Eligible providers
must include the model in `supported_models` when a scope is configured, be
available and outside key cooldown, and accept the request's protocol-specific
features. `random` preserves a valid active binding before considering the
connection-reuse tier described above. Without a valid binding it selects
uniformly within that tier (all eligible providers at startup). `price_priority`
chooses the lowest sum of input-cache-hit, input-cache-miss and output rates per
million tokens. Every candidate must have all three rates in the same currency;
missing or incomparable rates fail closed with `routing_price_unavailable`.
Equal totals break ties by provider reference. A strategy and billing failover
are both disabled by default.

With `random`, set `X-TideMux-Session-ID` to a stable conversation ID. Anthropic
requests can instead use `metadata.user_id` when the header is absent. The
header takes precedence; surrounding whitespace is removed. The first request
atomically chooses an eligible provider within that tier, and later requests with the
same caller namespace, client protocol, bare model ID and session ID reuse it
while its binding remains valid, even when another provider enters the reused
connection tier.
Concurrent first requests share one binding. Model IDs remain case-sensitive,
and upstream model IDs containing slashes are supported. Requests without a
stable ID keep per-request random selection; generated admission IDs do not
create routing affinity.

Bindings expire after 24 hours of idle time, refreshed on use. They store only
a SHA-256 composite key, model/provider references, connection generation and
timestamp in process memory.
The current gateway has one authenticated local access token, which defines
one caller namespace. Credentials, raw session IDs and request bodies are not
stored in the binding or logged. Restart clears all bindings; separate gateway
instances do not share them. This can improve cache locality but cannot guarantee
an upstream cache hit.

If the bound provider loses model/protocol eligibility or becomes unavailable,
including all-key cooldown, TideMux removes that binding and selects another
eligible provider for a later request. A safely classified model-not-found,
insufficient-balance or temporarily-unavailable failure likewise invalidates
the affected route for later requests and prevents immediately selecting that
failed pair again. Model-not-found and temporarily-unavailable failures cool
down for 30 seconds; insufficient balance cools down for five minutes. A longer
upstream retry delay or key cooldown also applies. After expiry, one later
request tests the failed pair; concurrent requests select another eligible
provider or receive `provider_keys_cooling_down`. A successful test restores
eligibility; a failed test starts another cooldown. No background request is
sent, and recovery needs no restart or configuration change. An already valid
binding to another provider does not move back merely
because the failed route recovers. Before any upstream dispatch, readiness is
rechecked within the request's original configuration view. Once dispatched,
a session-bound request never switches providers, even if billing-exhaustion failover is
enabled. Its existing bounded 429 retries remain on the selected provider.
Already-started SSE and requests queued on an older view keep their original
configuration and settlement snapshot.
`REF/MODEL` and `model:auto` do not use shared-model bindings;
`price_priority` also has no session affinity.

Cross-provider failover is a separate, explicit opt-in because it sends the
request to another provider. It happens only when that provider profile maps an
exact upstream code (and optional status) to `insufficient_balance`, and only
for providers whose upstream protocol matches the client protocol and whose
model scope permits the same requested model. It never uses a provider of the
other upstream protocol. Arbitrary 403 responses, policy errors, authentication failures,
unmapped 5xx responses and 429 do not cross providers. TideMux tries at most
four additional providers for one model and records each provider/model attempt
separately. A mapped balance exhaustion cools all keys in that provider profile
for five minutes; cooldowns reset when the gateway restarts. Budget reservations,
usage and cost remain attached to the provider that handled each attempt.

The new config fields are omitted while routing is disabled, so an untouched
0.2.2 config remains readable by both versions. Before rolling back a config
that uses 0.3.0 routing fields, clear the instance auto chain and disable
both routing options with the commands above; then install v0.2.2. The local
ledger and Keychain entries do not need migration. Also remove any
`temporarily_unavailable` error-code mapping first because v0.2.2 does not
recognize that new category; list and remove mappings with
`tidemux provider error-map list REF` and
`tidemux provider error-map remove REF --code CODE [--status STATUS]`.

## Provider budget

Budget policy and budgeted usage belong to an individual provider. Configure or
edit its rolling five-hour and seven-day limits interactively, or pass flags:

```sh
tidemux provider budget REF
tidemux provider budget REF --budget-5h 5 --budget-weekly 80 \
  --budget-currency USD --budget-mode hard --budget-alert-threshold 0.8
tidemux provider budget REF --disable
```

When exactly one provider is configured, `REF` may be omitted. With multiple
providers it is required. Modes are `alert`, `soft` and `hard`; a zero limit
disables that window, and setting both limits to zero disables the budget. An
enabled budget requires a price whose currency matches the budget currency for
every model used while that budget is active. Other providers' usage does not
count against this provider's limits.

Only `providers.REF.budget` is supported. Remove any obsolete top-level
`budget` field before loading the configuration or running provider commands,
then configure the desired policy with `tidemux provider budget REF`.
Existing ledger charges without a provider label are conservatively counted
toward each provider's budget until they age out of the rolling windows.

To start one of a provider's budget windows over, stop the gateway and run:

```sh
tidemux provider budget reset REF --window 5h
tidemux provider budget reset REF --window 7d
```

The reset applies only to that provider and window. The other window and the
request audit/billing history remain unchanged. Restart the gateway after the
reset.

## Gateway-wide settings

Run without options for a short guided setup, or specify only the settings to
change:

```sh
tidemux gateway configure
tidemux gateway configure --listen loopback
tidemux gateway configure --max-active-sessions 20 \
  --active-session-idle-timeout-seconds 300
tidemux gateway check
```

The only listener modes are `loopback` and `0.0.0.0`; the configured port is
preserved. Loopback is the default, and a gateway API key is required in either
mode. First-time gateway setup, key rotation, or enabling external access
prompts for the key with hidden input; press Enter to generate a random key. A
generated key is printed once so remote clients can use it. Traffic is
plain HTTP, so keep external access on a trusted LAN and protect it with a
firewall; do not expose it directly to the internet.

`tidemux gateway check` reads the gateway key from Keychain and verifies the
running local listener's authenticated `/v1/models` endpoint without printing
the key or changing configuration. Start `tidemux serve` with the same config
first; the check always connects through loopback, including when the listener
is bound to `0.0.0.0`.

`--max-in-flight` controls concurrent upstream requests. `--max-active-sessions`
sets the gateway-wide logical-session cap; zero disables it. A retained session
is released after both input and output have been idle for the configured
timeout. The default is five minutes; set
`--active-session-idle-timeout-seconds` to override it (zero selects the
five-minute default).

Report scheduling remains a separate resource command:

```sh
tidemux report schedule --time 09:00
tidemux report schedule --disable
```

For IM delivery, configure the destination first and select the webhook channel
when scheduling:

```sh
tidemux report webhook --provider lark
tidemux report schedule --time 09:00 --channel webhook
tidemux report notify
```

The endpoint is entered through hidden input and stored in Keychain. Supported
adapters are `generic`, `telegram`, `discord` and `lark`; messages are JSON
wrappers around a bounded plain-text summary, not the HTML export. Generic and
Telegram use a `text` field, Discord uses `content`, and Lark uses
`msg_type: text` with `content.text`. For Telegram Bot API, include the bot
`sendMessage` endpoint and `chat_id` in the hidden URL. To manually deliver or retry a
saved report, use `report deliver --id N --channel webhook` or
`report retry --id N --channel webhook`. Retry requires a recorded failed
attempt. Inspect the status and attempt count with `report deliveries --id N`.
`report webhook --disable` removes the endpoint and disables a webhook schedule;
`report schedule --disable` only disables scheduling.

Pass `--config PATH` to any command to manage a non-default profile. To inspect
the effective configuration and Keychain readiness, use `tidemux doctor`.
