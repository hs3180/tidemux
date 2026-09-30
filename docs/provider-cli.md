# Provider and gateway setup

The 0.3.0 candidate CLI uses resource-oriented commands. Provider setup and
lifecycle belong to `tidemux provider`; listener and session limits belong to
`tidemux gateway`. The legacy top-level `tidemux configure` command is not
retained. The published release is still v0.2.2; use a 0.3.0 candidate binary
to run the routing commands below.

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
to get the provider reference. After adding a provider, restart a running
`tidemux serve` process before routing requests to it. In a client, use the
bare upstream model ID when its configured scope selects one provider, or use
`REF/MODEL_ID` for explicit routing.

Provider credentials are stored in macOS Keychain, never in command arguments
or configuration JSON. Existing 0.1.x single-provider configurations are
migrated to a named provider when one is added; listener, report, ledger and
gateway credential settings are retained. A development-era global budget must
be assigned explicitly with `tidemux provider budget REF`. Configuration
changes are validated and atomically installed.

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

An auto model chain belongs to one provider and preserves the listed order:

```sh
tidemux provider auto-chain REF
tidemux provider auto-chain REF --models model-fast,model-capable
tidemux provider auto-chain REF --clear
```

Use `model:auto` when exactly one provider has a configured chain, or
`REF/auto` to select that provider explicitly. TideMux sends the actual chain
model upstream and records it in the response path, usage, cost and audit row.
An explicit `REF/MODEL` request stays pinned to that model. An auto chain can
advance on an exact model-not-found or temporarily-unavailable mapping, an
explicit insufficient-balance classification, or a transport failure proven
to have happened before request headers were written. It never retries after
stream output begins. HTTP 429 remains a bounded same-provider retry using
`Retry-After`; it never selects another provider.

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
features. `random` selects uniformly from the eligible providers. `price_priority`
chooses the lowest sum of input-cache-hit, input-cache-miss and output rates per
million tokens. Every candidate must have all three rates in the same currency;
missing or incomparable rates fail closed with `routing_price_unavailable`.
Equal totals break ties by provider reference. A strategy and billing failover
are both disabled by default.

Cross-provider failover is a separate, explicit opt-in because it sends the
request to another provider. It happens only when that provider profile maps an
exact upstream code (and optional status) to `insufficient_balance`, and only
for providers that support the same request model and protocol-specific
features. Arbitrary 403 responses, policy errors, authentication failures,
unmapped 5xx responses and 429 do not cross providers. TideMux tries at most
four additional providers for one model and records each provider/model attempt
separately. A mapped balance exhaustion cools all keys in that provider profile
for five minutes; cooldowns reset when the gateway restarts. Budget reservations,
usage and cost remain attached to the provider that handled each attempt.

The new config fields are omitted while routing is disabled, so an untouched
0.2.2 config remains readable by both versions. Before rolling back a config
that uses 0.3.0 routing fields, clear each provider's auto chain and disable
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

If a profile still has the former gateway-wide budget, the first
`provider budget REF` operation moves it to the selected provider. Existing
ledger charges without a provider label are conservatively counted toward each
provider's budget until they age out of the rolling windows.
Older daily/monthly budget fields cannot be translated to rolling windows; this
command replaces them with the newly configured provider policy (or removes
them with `--disable`).

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
