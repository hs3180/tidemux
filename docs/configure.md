# Configure TideMux from Terminal

No manual Keychain Access setup is required. `configure` reads your API key
without echo and automatically creates a different local gateway credential.
Neither secret is accepted as a command-line argument or written into JSON.

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

1. `configure` selects OpenAI format, `https://api.deepseek.com`, and
   `deepseek-flash`. Before the **API key (hidden)** prompt, TideMux asks for a
   daily report notification time in local time (`HH:MM`). Enter a time to enable
   scheduled notifications, or press Enter to leave them disabled. At the API-key
   prompt, paste your DeepSeek key and press Enter. Nothing appears while entering
   the key; this is expected. When enabled, TideMux installs a private per-profile
   macOS LaunchAgent for the notification.
2. It generates a gateway token, stores both credentials under unique accounts
   in your login Keychain, creates the state directory, and saves a mode-0600
   configuration containing only references. It verifies read-back and directory
   access. This step makes **no API request**.
3. `doctor` confirms local readiness. `serve` listens at `127.0.0.1:8787`;
   keep this terminal open. Stop with Control-C.

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

## If the login keychain is locked

`configure` automatically starts the macOS `security unlock-keychain` password
prompt in the current terminal, then continues the same setup after unlocking.
There is no separate command to copy or rerun.

1. At the **system keychain password** prompt, enter your keychain password
   (usually your Mac login password), **not** your API key. Input is hidden.
   The system utility reads this password directly; TideMux never receives it
   and never passes it through a command argument, environment variable or file.
2. After unlocking, TideMux asks for the optional daily notification time and then
   displays **API key (hidden)**. Paste the API key here and press Enter. These are
   separate prompts for the schedule setting and the upstream secret.
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
tidemux configure --preset deepseek-flash --protocol anthropic --replace
tidemux doctor
tidemux serve
```

This selects
`https://api.deepseek.com/anthropic/v1`; the gateway appends `/messages`.
Paste your DeepSeek API key when prompted. TideMux creates new Keychain references
and a local gateway token, and backs up the previous configuration. For a first
setup with Anthropic format, omit `--replace`.

## Any other compatible API

```sh
tidemux configure --protocol openai \
  --base-url https://your-provider.example/v1 \
  --model your-model-id \
  --pricing-input-cache-hit 1 \
  --pricing-input-cache-miss 2 \
  --pricing-output 4 \
  --pricing-currency USD
```

Use `--protocol anthropic` for an Anthropic-format API. Include the endpoint's
version/path prefix. Model IDs are not limited to a preset. Prices are per
million tokens. The three rates are input cache hit, input cache miss and output;
`--pricing-currency` defaults to `USD`, while source and version default to
`manual-cli` and `manual`. `--max-in-flight 2` changes the explicit concurrency cap.
To replace an existing local setup,
stop the gateway first and add `--replace`. Inspect other options with
`tidemux configure --help`.

## Update the current local configuration

By default the command refuses to overwrite an existing file. To intentionally
replace it, stop the server and repeat the configure command with `--replace`.
New Keychain accounts are generated; old credentials and ledger data are retained
in place. The command replaces the configuration as a whole, including the
provider pricing selected alongside the new API key. Before restarting, review
the new pricing and limits against the saved backup, and keep the existing
`ledger_path` so billing continues to read your history.
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
