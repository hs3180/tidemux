# Configure TideMux from Terminal

No manual Keychain Access setup is required. `configure` reads your API key
without echo and automatically creates a different local gateway credential.
Neither secret is accepted as a command-line argument or written into JSON.

## DeepSeek Flash: three commands

Run the installed CLI (or replace `tidemux` with `./tidemux` for an extracted binary):

```sh
tidemux configure --preset deepseek
tidemux doctor
tidemux serve
```

1. `configure` selects OpenAI format, `https://api.deepseek.com`, and
   `deepseek-flash`. At **API key (hidden)**, paste your DeepSeek key and press Enter.
   Nothing appears while entering the key; this is expected.
2. It generates a gateway token, stores both credentials under unique accounts
   in your login Keychain, creates the state directory, and saves a mode-0600
   configuration containing only references. It verifies read-back and directory
   access. This step makes **no API request**.
3. `doctor` confirms local readiness. `serve` listens at `127.0.0.1:8787`;
   keep this terminal open. Stop with Control-C.

Default files:

- Config: `~/Library/Application Support/TideMux/config.json`
- Ledger: `~/Library/Application Support/TideMux/ledger.db`

The CLI resolves its default path; manually supplied JSON paths still need
absolute paths (no `~` expansion). Query requests with `tidemux ledger`.

## If the login keychain is locked

`configure` automatically starts the macOS `security unlock-keychain` password
prompt in the current terminal, then continues the same setup after unlocking.
There is no separate command to copy or rerun.

1. At the **system keychain password** prompt, enter your keychain password
   (usually your Mac login password), **not** your API key. Input is hidden.
   The system utility reads this password directly; TideMux never receives it
   and never passes it through a command argument, environment variable or file.
2. After unlocking, TideMux displays **API key (hidden)**. Paste the API key here
   and press Enter. These are two different prompts for two different secrets.
3. Wrong password, cancellation or an unavailable keychain stops setup before
   collecting the API key or changing configuration. Press Control-C to cancel.

An already accessible keychain skips the system password prompt. This is a
terminal system prompt, not a separate macOS graphical authorization dialog.
If the keychain uses a password different from your Mac login password, enter
that keychain's password. TideMux does not reset keychains or request admin access.

## DeepSeek via Anthropic format

Use a separate profile so the first configuration remains available:

```sh
tidemux configure --preset deepseek --protocol anthropic \
  --config "$HOME/Library/Application Support/TideMux/anthropic.json"
tidemux serve --config "$HOME/Library/Application Support/TideMux/anthropic.json"
```

Stop the other server first: both profiles default to port 8787. This selects
`https://api.deepseek.com/anthropic/v1`; the gateway appends `/messages`.
Paste the same DeepSeek API key when prompted. Each profile gets separate
Keychain references and a local gateway token.

## Any other compatible API

```sh
tidemux configure --protocol openai \
  --base-url https://your-provider.example/v1 \
  --model your-model-id \
  --config "$HOME/Library/Application Support/TideMux/custom.json"
```

Use `--protocol anthropic` for an Anthropic-format API. Include the endpoint's
version/path prefix. Model IDs are not limited to a preset. `--max-in-flight 2`
changes the explicit concurrency cap. Inspect other options with
`tidemux configure --help`.

## Update an existing profile

By default the command refuses to overwrite an existing file. To intentionally
replace it, stop the server and repeat the configure command with `--replace`.
New Keychain accounts are generated; old credentials and ledger data are retained
so other profiles are not broken. Remove unused old entries later in Keychain
Access if desired. A failed setup rolls back newly saved entries; the previous
configuration remains in place.

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

A verified price entry is needed to finish cost reconciliation. `configure`
intentionally does not guess current prices: until you add them, ledger cost
is `null`. The helper can report successful usage reconciliation but stop at
unknown cost. For `deepseek-flash`, use the [official English USD pricing examples](deepseek-pricing.md).
See [pricing and live verification](demo.md). Never repeat a live
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

## Local rejection diagnostics (0.1.0)

```sh
./tidemux ledger --diagnostics --config /path/to/config.json
```

Rejected authentication, unsupported routes, oversized bodies and invalid
requests receive `X-TideMux-Request-ID`. Match that ID against the independent
`local_diagnostics` table or this command's JSON output. Each record contains
only a timestamp, protocol, normalized method/endpoint category, HTTP status and
safe error code. Original URLs, query strings, headers, unknown field names and
message contents are not stored.

These rows are not upstream attempts and do not contribute to token/cost totals.
The ordinary `ledger` command continues to show upstream attempt records. If
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
the upstream provider. The API version always comes from the TideMux profile;
client authentication headers and unrelated custom headers are not forwarded.

## Configuration rollback

`configure --replace` saves an exact, mode-0600 copy of the previous configuration
beside it as `<config>.backup-<unique-id>` before installing the replacement. Old
Keychain items are retained, so that backup still references its original keys.
If backup creation fails, the existing configuration is not replaced and newly
created credentials are cleaned up. A setup-time change to the configuration is
also rejected instead of overwriting that change.

To roll back, stop the gateway using the replacement profile and start the desired
binary with `serve --config /path/to/config.json.backup-<id>`, using the exact backup
name. Preserve the new profile before restoring the backup to the original path.
The ledger path comes from the selected configuration; backing up a configuration
does not snapshot its SQLite ledger. The full packaged-binary/data rollback check
remains part of release acceptance.
