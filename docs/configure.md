# Configure TideMux from Terminal

No manual Keychain Access setup is required. `configure` reads your provider
API key(s) and optional gateway API key without echo. Leave the gateway prompt
empty to generate a random credential, or enter a custom value. Neither secret
is accepted as a command-line argument or written into JSON.

## Two provider setup modes

Run `tidemux configure` without `--provider`, `--base-url`, `--model` or
`--preset` for the guided terminal setup. It starts by asking for the provider
API Base URL, derives a provider name from the host, then asks for the upstream
API key using hidden input. TideMux infers the
protocol from the endpoint or its authenticated `GET /models` response. If the
endpoint returns exactly one model, that model is selected automatically; when
it returns several, choose a default model from the displayed list. By default,
the provider supports all models: press Enter at the optional supported-models
prompt, or enter a comma-separated subset to restrict it. If discovery is
unavailable, model IDs for an optional allowlist can be entered directly. If
protocol or the default model cannot be discovered, the wizard asks only for
the missing value. It never sends a completion request during setup.

To set provider details directly, pass them on the command line. The provider
name is explicit, and the protocol may be inferred or forced:

```sh
tidemux configure \
  --provider my-service,https://api.example.com/v1,my-model \
  --pricing-input-cache-hit 1 \
  --pricing-input-cache-miss 2 \
  --pricing-output 4
```

Use `--provider NAME,PROTOCOL,BASE_URL,MODEL` to force `openai` or `anthropic`.
Omit `--provider-models` to allow all model IDs. To restrict one provider to a
subset, repeat `--provider-models NAME,MODEL[,MODEL...]`:

```sh
tidemux configure \
  --provider my-service,https://api.example.com/v1,my-model \
  --provider-models my-service,my-model,another-model
```

The provider's `model` is the fallback used when a client omits its model and
must also appear in an explicit allowlist. In JSON, `supported_models` is
optional; omitted or empty means all models. To restrict a provider in JSON:

```json
"supported_models": ["my-model", "another-model"]
```

The upstream API key remains a hidden prompt so it is not exposed in shell
history or process listings. For command-line setup, custom models require all
three price flags shown above. The wizard uses TideMux's built-in price only
when the endpoint and model match a verified preset; otherwise it leaves
pricing unknown rather than guessing. A configured budget still requires
explicit matching prices.

## DeepSeek Flash: three commands

Run the installed CLI (or replace `tidemux` with `./tidemux` for an extracted binary):

```sh
tidemux configure --preset deepseek-flash
tidemux doctor
tidemux serve
```

To configure a budget without editing JSON, use the interactive budget TUI:

```sh
tidemux budget
```

For scripting or direct changes, provide only the fields to update:

```sh
tidemux budget \
  --budget-5h 0.001 \
  --budget-weekly 0.01
```

Both commands automatically use the user profile at
`~/Library/Application Support/TideMux/config.json`. The optional `--config`
flag is only for managing an alternate profile.

Use `--budget-mode alert|soft|hard`, `--budget-currency`, and
`--budget-alert-threshold` to adjust individual fields. Use `--disable` to
remove budget enforcement. A budget without matching pricing is rejected
before any configuration is written; TideMux never estimates cost from a
fallback amount.

`budget` only changes budget limits. Pricing is selected when the provider API
key is configured, so changing a budget never changes the rates used for
accounting.

The budget schema is intentionally not migrated from earlier pre-release
profiles. If an existing profile contains conflicting legacy budget fields,
replace it with the new schema using `tidemux budget` or recreate it with
`tidemux configure --replace`.

1. `configure` selects the shared provider API root and model. The DeepSeek preset
   uses `https://api.deepseek.com` and `deepseek-flash`; TideMux automatically
   detects the provider protocol from that root. To use DeepSeek's Anthropic
   compatible API, override the root with
   `--base-url https://api.deepseek.com/anthropic/v1`. Before the **API key (hidden)** prompt, TideMux asks for a
   daily report notification time in local time (`HH:MM`). Enter a time to enable
   scheduled notifications, or press Enter to leave them disabled. At the API-key
   prompt, paste your DeepSeek key and press Enter. The following **Gateway API
   key (hidden)** prompt accepts a custom value; press Enter without entering a
   value to generate one randomly. Nothing appears while entering either key;
   this is expected. When enabled, TideMux installs a private per-profile macOS
   LaunchAgent for the notification.
2. It stores the provider and gateway credentials under unique accounts in your
   login Keychain, creates the state directory, and saves a mode-0600
   configuration containing only references. It verifies read-back and directory
   access. This step makes **no API request**.
3. `doctor` confirms local readiness. By default, `serve` listens only at
   `127.0.0.1:4000`; keep this terminal open. Stop with Control-C.

Default files:

- Config: `~/Library/Application Support/TideMux/config.json`
- Ledger: `~/Library/Application Support/TideMux/ledger.db`

The CLI uses this local configuration automatically. Paths stored inside JSON
must be absolute (`~` is not expanded). Use `tidemux billing` for a readable summary
of your local ledger for the current calendar month in your computer's local
timezone. Billing reads `ledger_path` from the default configuration above and
keeps that existing database location. Add `--details` to inspect its request
records. The output shows exact RFC3339 date bounds; `--from` and `--to` select
another period. Add `--json` for structured
output, or `--download billing.csv` to save the selected period's billing details.

The notification choice can also be supplied on the initial configure command:

```sh
tidemux configure --preset deepseek-flash --notification-time 09:00
```

The time uses the Mac's local timezone, and scheduled notifications use macOS
Notification Center.

## Allow LAN clients (external listen mode)

TideMux has two listen modes. It uses loopback by default. To let clients on
the local network reach the gateway, select the external mode by setting the
listen address to `0.0.0.0`:

```sh
tidemux configure --preset deepseek-flash \
  --listen 0.0.0.0:4000
```

`127.0.0.1:4000` (or another loopback IP) keeps the gateway local;
`0.0.0.0:4000` binds all IPv4 interfaces. Other non-loopback IPs are rejected.
The selected address is persisted in `listen_addr`, and existing profiles remain
loopback-only.

External mode uses the Gateway API key for remote clients; it is stored in
Keychain and is separate from the upstream provider key. The key can be custom
or randomly generated by pressing Enter at the TUI prompt. Loopback mode uses
the same credential flow.

TideMux prints a warning when external access is configured or started. The
gateway still requires its local Bearer token, but the service is plain HTTP:
use a trusted network and firewall, do not expose the port directly to the
internet, and use a TLS-terminating reverse proxy when traffic can leave the
trusted LAN. Treat the gateway token as a bearer secret for every client that
can reach the listener.

To change the schedule of an existing profile without replacing its API-key
references:

```sh
tidemux report schedule --time 09:00
tidemux report schedule --disable
```

Use `--config /absolute/path/to/profile.json` for a non-default profile. The
schedule command updates the JSON atomically, keeps a mode-0600 backup, and
loads or unloads the corresponding user LaunchAgent. `tidemux report notify`
is the LaunchAgent entry point; it generates the current local-day report,
delivers it, and records the delivery result.

## Active session limit (0.2.0)

Use `--max-active-sessions` during `configure` to cap distinct logical sessions
for the local gateway:

```sh
tidemux configure --preset deepseek-flash \
  --max-active-sessions 20 \
  --active-session-idle-timeout-seconds 300
```

The default `0` for `max_active_sessions` disables this limit. The idle timeout
defaults to 300 seconds (5 minutes); set
`active_session_idle_timeout_seconds` to 1–86400 seconds to override it, or
leave it at `0` to use the default. A session is identified by
`X-TideMux-Session-ID` (or supported protocol metadata when available).
Requests in an already admitted session share one slot. An in-flight request is
active while it has input or output activity. A retained session is eligible for
release only when no request is in flight and both input and output have been
idle for the configured timeout. Streaming sessions release their slot when the
stream completes or is canceled.
New sessions receive HTTP 429 with the stable error code
`active_session_limit` and `Retry-After: 1` when the cap is reached. The limit
is a top-level, gateway-wide setting: it applies to all sessions handled by the
process, not to an individual model or request. When a session ID is present, it
is forwarded upstream in the `X-TideMux-Session-ID` header. In a multi-process
deployment, each gateway instance enforces its own configured cap.

## If the login keychain is locked

`configure` automatically starts the macOS `security unlock-keychain` password
prompt in the current terminal, then continues the same setup after unlocking.
There is no separate command to copy or rerun.

1. At the **system keychain password** prompt, enter your keychain password
   (usually your Mac login password), **not** your API key. Input is hidden.
   The system utility reads this password directly; TideMux never receives it
   and never passes it through a command argument, environment variable or file.
2. After unlocking, TideMux asks for the optional daily notification time and then
   displays **API key (hidden)**. Paste the provider API key here and press Enter.
   It then displays **Gateway API key (hidden)**; enter a custom key or press
   Enter to generate one randomly. These are separate prompts for the schedule
   setting and the two credentials.
3. Wrong password, cancellation or an unavailable keychain stops setup before
   collecting the API key or changing configuration. Press Control-C to cancel.

An already accessible keychain skips the system password prompt. This is a
terminal system prompt, not a separate macOS graphical authorization dialog.
If the keychain uses a password different from your Mac login password, enter
that keychain's password. TideMux does not reset keychains or request admin access.

## DeepSeek via Anthropic format

To switch the current local gateway to Anthropic format, stop it with Control-C
and replace the local configuration:

```sh
tidemux configure --preset deepseek-flash \
  --base-url https://api.deepseek.com/anthropic/v1 --replace
tidemux doctor
tidemux serve
```

This selects
`https://api.deepseek.com/anthropic/v1`; the gateway appends `/messages`.
Paste your DeepSeek API key when prompted, then enter a custom gateway key or
press Enter to generate one randomly. TideMux creates new Keychain references
and backs up the previous configuration. For a first setup with Anthropic format,
omit `--replace`.

## Any other compatible API

```sh
tidemux configure \
  --base-url https://your-provider.example/v1 \
  --model your-model-id \
  --pricing-input-cache-hit 1 \
  --pricing-input-cache-miss 2 \
  --pricing-output 4 \
  --pricing-currency USD
```

Include the endpoint's version/path prefix. TideMux detects whether the endpoint
uses OpenAI or Anthropic format from its API root/model discovery; no protocol
flag is needed. Model IDs are not limited to a preset. Prices are per million
tokens. The three rates are input cache hit, input cache miss and output;
`--pricing-currency` defaults to `USD`, while source and version default to
`manual-cli` and `manual`. `--max-in-flight 2` changes the explicit concurrency cap.
To replace an existing local setup,
stop the gateway first and add `--replace`. Inspect other options with
`tidemux configure --help`.

### Configure protocol-routed providers

Each named provider uses exactly one upstream API protocol, endpoint and API
key. Provider specs are repeatable. By default, the protocol is inferred from
the endpoint; explicitly include a protocol to force it:

```sh
tidemux configure \
  --provider openai-service,https://api.example.com/v1,model-id \
  --pricing-input-cache-hit 0.5 \
  --pricing-input-cache-miss 1 \
  --pricing-output 2
```

The automatic form is `NAME,BASE_URL,MODEL`. TideMux first checks recognizable
endpoint host/path hints, then—when needed—uses the provider API key to inspect
`GET /models` with OpenAI and Anthropic authentication. It never sends a billable
completion to identify the protocol. This detection happens when the gateway
starts. If the endpoint cannot be identified or does not expose a recognizable
model list, startup reports that provider by name. To force the protocol and
skip detection, use the four-part form. In JSON, omit `protocol` or set it to
`auto` for detection; setting it to `openai` or `anthropic` forces that format.

```sh
tidemux configure \
  --provider deepseek-openai,openai,https://api.deepseek.com/v1,openai-model-id \
  --provider deepseek-anthropic,anthropic,https://api.deepseek.com/anthropic/v1,anthropic-model-id \
  --default-provider openai=deepseek-openai \
  --default-provider anthropic=deepseek-anthropic \
  --pricing-input-cache-hit 1 \
  --pricing-input-cache-miss 2 \
  --pricing-output 4
```

OpenAI clients route to the provider named by `default_providers.openai`, and
Anthropic clients to `default_providers.anthropic`; their upstream paths are
`/chat/completions` and `/messages`. If there is only one provider for a detected
protocol, it is selected as that protocol's default automatically. When multiple
providers resolve to the same protocol, use `--default-provider PROTOCOL=NAME`
to choose one. Each provider may use a different API root and credential.
Requests use only the selected provider and are never retried through another
profile. API keys are entered at hidden prompts and stored in Keychain; the JSON
config contains references only. Pricing flags seed each provider's price table
using that provider's model. `--anthropic-version YYYY-MM-DD` applies to
Anthropic providers; when using automatic detection, the version is checked
after the protocol is identified.
`--replace` updates the full configuration while preserving a backup and old
Keychain items.

The selected provider's `GET /models` response is used only on its client route
when it is a complete, recognized model list. It populates model discovery but
does not restrict completion requests by default; requests are forwarded to the
selected provider unless `supported_models` is configured. An explicit
allowlist also limits the gateway's `/models` response. A provider without a
complete recognizable list returns an empty catalogue unless it has an explicit
allowlist; direct requests are still sent to that provider.
See the non-secret [named provider configuration example](../examples/named-providers.json).

## Update the current local configuration

By default the command refuses to overwrite an existing file. To intentionally
replace it, stop the server and repeat the configure command with `--replace`.
New Keychain accounts are generated; old credentials and ledger data are retained
in place. Inspect the JSON configuration before replacing it: it contains each
provider endpoint and Keychain references, never the API key values. To retain
named providers, pass all desired `--provider` entries and
`--default-provider` mappings again. The command replaces the configuration as
a whole, including provider pricing selected alongside the new API keys.
Before restarting, review the new pricing and limits against the saved backup,
and keep the existing `ledger_path` so billing continues to read your history.
Remove unused old entries later in Keychain Access if desired. A failed setup
rolls back newly saved entries; the previous configuration remains in place.

## Connect clients and test

The local credential is separate from the upstream key. Clients send the local
token as Bearer auth (Anthropic clients can use `x-api-key`). Its Keychain
service/account are in `access_token_keychain` in your config; use Keychain
Access to reveal/copy it locally if the client needs manual entry. Do not paste
credentials into chat or put them into shell history.

For a minimal authorized DeepSeek call, the source helper retrieves the local
credential itself (no token copy needed):

```sh
python3 scripts/verify_live.py \
  --config "$HOME/Library/Application Support/TideMux/config.json" \
  --disable-thinking --output /tmp/tidemux-live-openai.json
```

The configured provider profile always carries its pricing. `configure --preset
deepseek-flash` uses TideMux's fixed peak DeepSeek preset. For another provider,
pass `--pricing-input-cache-hit`, `--pricing-input-cache-miss`, and
`--pricing-output` when setting the API key; use `--pricing-source` and
`--pricing-version` for the verified reference. See [pricing and live
verification](demo.md). Never repeat a live
request solely to fix a documentation step without considering its API cost.

## Request limits (0.1.0)

Existing 0.1.0 configurations remain valid. An optional `limits` object adjusts
bounds for longer client sessions:

```json
"limits": {
  "upstream_timeout_seconds": 300,
  "request_bytes": 4194304,
  "response_bytes": 16777216,
  "stream_bytes": 134217728,
  "event_bytes": 2097152
}
```

Omitted or zero fields keep the previous defaults: 60 seconds after concurrency
admission, 1 MiB request, 8 MiB non-streaming response, 64 MiB total SSE stream,
and 1 MiB SSE event. Queue waiting remains cancelable and is not included in the
upstream timeout. Timeout can be 1–3600 seconds; byte limits can be up to 1 GiB,
and the event limit cannot exceed the stream limit. Limits do not imply that an
upstream model supports the corresponding context or output size.

An upstream timeout before streaming returns 504 `upstream_timeout`; after
streaming starts it emits a safe SSE error. The attempt is recorded as an error
with unknown usage/cost, rather than a successful response or user cancellation.

## Local rejection diagnostics

```sh
tidemux doctor --diagnostics

# Structured output for tools:
tidemux doctor --diagnostics --json
```

Rejected authentication, unsupported routes, oversized bodies and invalid
requests receive `X-TideMux-Request-ID`. Match that ID against the independent
`local_diagnostics` table or this command's output. The default is a readable
table of the latest 100 local rejections; `--json` returns those records as a JSON
array. Each record contains only a timestamp, protocol, normalized method/endpoint
category, HTTP status and safe error code. Original URLs, query strings, headers,
unknown field names and message contents are not stored.

These rows are not upstream attempts and do not contribute to token/cost totals.
Use `billing --details` for upstream attempt records. If
writing a local diagnostic fails, the response is 500 `local_diagnostic_failed`
with the generated ID; no upstream call was made. The new table is additive and
does not modify existing 0.1.0 request records.

## Declared model limits (0.1.0)

Use `configure --context-tokens <n> --output-tokens <n>` when you have verified
these values with your provider, or set:

```json
"model_capabilities": {
  "context_tokens": 65536,
  "max_output_tokens": 8192
}
```

These numbers are illustrative, not DeepSeek defaults. Omitted/zero means unknown.
Both list and detail model discovery expose configured values as `context_length`
and `max_output_tokens`. `tidemux kilo` places them in the custom model's limit
configuration; `tidemux hermes` sets `model.context_length`. Hermes's per-request
output budget and Claude's client-specific limits remain under their own client
settings. No request is silently shortened or rewritten to meet these declarations.
The gateway's byte limits are separate from model token capabilities.

## Anthropic beta features

On the Anthropic route, TideMux forwards valid `anthropic-beta` feature lists from
the client. Multiple header values are joined with commas; invalid or oversized
lists are rejected locally with `invalid_beta_header`. Beta support is decided by
the upstream provider. The API version always comes from the local configuration;
client authentication headers and unrelated custom headers are not forwarded.

## Configuration rollback

`configure --replace` saves an exact, mode-0600 copy of the previous configuration
beside it as `<config>.backup-<unique-id>` before installing the replacement. Old
Keychain items are retained, so that backup still references its original keys.
If backup creation fails, the existing configuration is not replaced and newly
created credentials are cleaned up. A setup-time change to the configuration is
also rejected instead of overwriting that change.

To roll back, stop the gateway and preserve the current configuration. Restore
the chosen backup to `~/Library/Application Support/TideMux/config.json`, then
run `tidemux doctor` and `tidemux serve`. The ledger path comes from that local
configuration; backing up a configuration
does not snapshot its SQLite ledger. The full packaged-binary/data rollback check
remains part of release acceptance.
