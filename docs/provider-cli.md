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

TideMux reads the API key through hidden terminal input, infers the upstream
protocol from the endpoint or authenticated `GET /models`, and selects the
only discovered model automatically. If several models are returned, it asks
which one clients should use when they omit a model. If protocol or model
discovery is unavailable, it asks only for the missing information. Setup
never sends a completion request. The first guided setup also asks whether to
schedule a daily usage-report notification in local time; leave it blank to
skip.

For direct setup, supply the endpoint and any known choices:

```sh
tidemux provider add https://api.deepseek.com \
  --model deepseek-flash
```

The API key is still prompted securely. The provider reference is inferred from
the endpoint; use `--name LABEL` only when you want a different reference. If
the endpoint does not identify the protocol, `--protocol openai` or
`--protocol anthropic` forces it. `--model` is the fallback for clients that
omit a model; all models are allowed unless an allowlist is set separately.

Provider credentials are stored in macOS Keychain, never in command arguments
or configuration JSON. Existing 0.1.x single-provider configurations are
migrated to a named provider when one is added; listener, report, ledger and
gateway credential settings are retained. A development-era global budget must
be assigned explicitly with `tidemux provider budget REF`. Configuration
changes are validated and atomically installed.

## Inspect and manage providers

```sh
tidemux provider list
tidemux provider show REF
tidemux provider update REF --model MODEL_ID
tidemux provider default openai REF
tidemux provider models REF
tidemux provider models REF --only model-a,model-b
tidemux provider models REF --all
tidemux provider remove REF
```

`list` and `show` never display credentials. `update` changes only the named
fields: `--endpoint URL`, `--protocol openai|anthropic`, `--model ID`,
`--anthropic-version DATE` (Anthropic only), and `--rotate-key`. Updating
credentials requires hidden terminal input. Changing
an endpoint re-evaluates its protocol by default using the stored key; combine
`--rotate-key` when the new endpoint requires a different key. Use `--protocol`
to force a wire format. Changing an endpoint clears any model allowlist,
restoring the default all-models scope.

The first provider added for a protocol becomes its default. Later additions
do not silently change the route. `provider default` explicitly chooses the
provider used by OpenAI or Anthropic clients. Removal asks for confirmation. If
the removed provider is a protocol default and alternatives remain, choose a
replacement or pass `--default REF`; non-interactive removal requires `--yes`.
Removing the last provider clears that protocol route. The gateway cannot serve
requests until another provider is added. A Keychain item is removed only when
no remaining provider references it.

With no scope option, `provider models` queries and prints the authenticated
model catalog when the endpoint exposes a complete, recognizable list. `--only`
sets an allowlist and must include the configured default model; `--all` clears
the allowlist. An unavailable model catalog does not restrict requests unless
an explicit allowlist is configured.

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
than guessed. An enabled budget requires a matching price for that provider's
default model and for each model used while that budget is active. Unpriced
models remain usable when the provider has no budget.

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
enabled budget requires a price whose currency matches
the budget currency for the provider's default model. Other providers' usage
does not count against this provider's limits.

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

Pass `--config PATH` to any command to manage a non-default profile. To inspect
the effective configuration and Keychain readiness, use `tidemux doctor`.
