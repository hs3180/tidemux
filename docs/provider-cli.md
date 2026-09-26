# Provider and gateway setup

The 0.2.0 development CLI uses resource-oriented commands. Provider setup and
lifecycle belong to `tidemux provider`; listener and session limits belong to
`tidemux gateway`. The legacy top-level `tidemux configure` command is not
retained. The published 0.1.1 binary predates this command structure; this guide
describes the current source CLI.

## Add a provider

Guided setup starts by asking for an API Base URL:

```sh
tidemux provider add
```

TideMux reads the API key through hidden terminal input and infers the upstream
protocol from the endpoint or authenticated `GET /models`. Model discovery is
informational and never sends a completion request. In guided setup, choose
model IDs to create an allowlist or leave the selection blank to allow all
models. If discovery is unavailable, enter IDs to restrict access or leave it
blank to allow all. Every client request must identify its provider and model
explicitly as `REF/MODEL_ID`; setup never chooses a default provider or model.
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
to get the provider reference, then select a model with `REF/MODEL_ID` in the
client or with the launcher's required `--model` option.

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
response stream. Before any response is delivered, TideMux can try the other
keys in the same group after an upstream 401/403, 429, or a transport failure
that occurred before request headers were written. It never changes keys after
an SSE frame may have reached the client. Failed keys enter a process-local
cooldown: 30 seconds for 401/403, 5 seconds for a safe pre-write transport
failure, and the upstream `Retry-After` for 429 (one minute if absent, bounded
to five minutes). If every key is cooling down, the gateway returns 503 with
`Retry-After` instead of sending another request upstream. Cooldowns reset when
the gateway process restarts. A configured `supported_models` allowlist
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

Every request explicitly selects a provider using the model ID
`REF/MODEL_ID`; `REF` is the stable reference shown by `provider list`, and
`MODEL_ID` is the upstream model name (which may itself contain slashes). The
client protocol does not choose a provider. TideMux strips only the first
`REF/` prefix before forwarding the request. Either client protocol can target
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

## Gateway-wide settings

Run without options for a short guided setup, or specify only the settings to
change:

```sh
tidemux gateway configure
tidemux gateway configure --listen loopback
tidemux gateway configure --max-active-sessions 20 \
  --active-session-idle-timeout-seconds 300
```

The only listener modes are `loopback` and `0.0.0.0`; the configured port is
preserved. Loopback is the default, and a gateway API key is required in either
mode. First-time gateway setup, key rotation, or enabling external access
prompts for the key with hidden input; press Enter to generate a random key. A
generated key is printed once so remote clients can use it. Traffic is
plain HTTP, so keep external access on a trusted LAN and protect it with a
firewall; do not expose it directly to the internet.

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
