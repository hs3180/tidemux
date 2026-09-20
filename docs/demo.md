# Local demo

Requires macOS, Keychain Access and a non-streaming text-capable OpenAI or
Anthropic compatible endpoint. The local release is `0.1.0`.

## Build or extract

From the source root with Go 1.27 or later:

```sh
CGO_ENABLED=0 go build -o tidemux ./cmd/tidemux
./tidemux version
mkdir -p "$HOME/Library/Application Support/TideMux"
```

Alternatively extract the candidate archive and run its `tidemux` binary.
The arm64 local release has been tested on macOS 15.7.4; other OS versions are not
verified. See [installation](install.md) for release downloads and Homebrew.

## Recommended: configure with the CLI

```sh
./tidemux configure --preset deepseek
./tidemux doctor
./tidemux serve
```

Paste your DeepSeek API key at the hidden prompt and press Enter. The CLI creates
the application directory, stores both secrets in Keychain, writes a private JSON
config, and verifies local readiness. It does not send a billable request.
See [configuration guide](configure.md) for other providers, Anthropic mode,
reconfiguration and unlocking a locked login keychain.

## Manual configuration (optional)

In Keychain Access create two generic-password items:

| Service | Account | Password |
| --- | --- | --- |
| `com.tidemux.openai` (or `com.tidemux.anthropic`) | `default` | Your upstream API key |
| `com.tidemux.gateway` | `default` | A different locally generated secret for gateway clients |

Copy [OpenAI config](../examples/openai.json) or
[Anthropic config](../examples/anthropic.json) to
`~/Library/Application Support/TideMux/config.json`, with file permissions 0600.
If a configuration already exists, stop the gateway and back it up before
replacing it. Set:

- `base_url`: the exact API root **including its version/prefix**, for example
  `https://your-endpoint.example/api/v1`. TideMux appends `/chat/completions` or
  `/messages` exactly once. Trailing slashes are ignored. HTTPS is required
  except for numeric loopback HTTP. URLs cannot contain credentials/query/fragment.
- `model`: your actual model ID, used when a request omits model.
- `upstream_id`: a short non-secret label for ledger records.
- `ledger_path`: the existing local ledger location, or for a first setup the
  absolute path to `~/Library/Application Support/TideMux/ledger.db` with your
  home directory written out. The directory must exist; `~` is not expanded in JSON.
- `max_in_flight`: 1–1024. Start with 1; this caps simultaneous upstream calls.
- `anthropic_version`: explicit protocol version for Anthropic, example `2023-06-01`.

Do not put credentials in JSON, terminal history, logs, or source control.
Old `deepseek_*` configs are rejected; migrate to the generic example explicitly.

```sh
./tidemux doctor
./tidemux serve
```

`doctor` checks local config, Keychain retrieval and ledger-directory write
access. It does not contact the upstream. `serve` stays in the foreground;
Control-C stops it. There is one configured upstream per process, no fallback.

## Request and inspect

Configure your HTTP client with the local gateway token (not the upstream key):

- OpenAI: `POST http://127.0.0.1:8787/v1/chat/completions`, Bearer authentication,
  body `{"model":"your-model","messages":[{"role":"user","content":"Hi"}]}`.
- Anthropic: `POST http://127.0.0.1:8787/v1/messages`, Bearer or `x-api-key`
  authentication with the **local token**, body
  `{"model":"your-model","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`.

See [protocol support](protocols.md) for accepted fields. The response keeps
upstream JSON and adds `X-TideMux-Request-ID` for audit correlation.

Inspect the local ledger with:

```sh
./tidemux billing
./tidemux billing --details
```

The first command shows a readable billing summary; the second shows its request
audit details. Billing reads the existing `ledger_path` from the default local
configuration used by the gateway. Both views default to the current
calendar month in the computer's local timezone and display exact RFC3339 bounds.
Use `--from` and `--to` for another period; if either is supplied, the missing
boundary is open. Add `--json`
for structured output or use `--download billing.csv` to save the selected period's
stored billing details. These queries do not retrieve keys or contact the provider.
Use `doctor --diagnostics` for recent local rejections, adding `--json` for tools.

Unknown token counts or costs remain unknown (`null` in JSON); a `cost_source`
of `local_estimated_cache_prefix` identifies a transparent local estimate rather
than provider-reported usage. Cancellation is not evidence of zero upstream
billing. If the gateway reports `audit_failed_do_not_retry_blindly`, the
provider may already have executed the call; inspect storage and upstream usage
before retrying. TideMux does not automatically retry any call.

## Optional pricing

For `deepseek-flash`, use the [DeepSeek USD pricing examples](deepseek-pricing.md),
which distinguish peak and off-peak rates from the English official price list.
The generic example below is only for explaining the configuration format.

Add a `prices` object keyed by exact request model ID. Rates are **per million
tokens**, in an explicit three-letter currency, with your verified source and
version. The following numbers are synthetic and must not be used as real rates:

```json
{"prices":{"your-model":{
  "currency":"USD","source":"synthetic-example-only","version":"example-1",
  "input_per_million":2,"output_per_million":4,
  "cache_read_per_million":1,"cache_write_per_million":3
}}}
```

Missing prices produce unknown cost. Positive provider-reported cached usage
requires the corresponding price. Unknown provider cache breakdowns can use the
local content estimator when a session and pricing are available; otherwise the
cost remains unknown. Estimates are token arithmetic, not provider invoices;
request fees, service-tier adjustments, tool fees and taxes are outside this
model.

## Live verification

With `serve` running, a verified price entry configured, and authorization to make
one minimal request, run from the source root:

```sh
python3 scripts/verify_live.py \
  --config "$HOME/Library/Application Support/TideMux/config.json" \
  --output /tmp/tidemux-live-openai.json
```

For `deepseek-flash`, add `--disable-thinking`. For models requiring
`max_completion_tokens`, add `--openai-token-limit-field max_completion_tokens`.
To verify the other protocol, stop the gateway, switch the local configuration
using `configure` with that protocol's API settings and `--replace`, and restore
the verified price settings from the saved backup before restarting. Use a
separate evidence file for that authorized run. The helper
reads the local token into memory, makes one request, and checks response usage
against the persisted record and recomputes cost. It does not print/save the
prompt, response content or key. Review sanitized evidence before sharing it.

## Persistence and removal

New records are in `request_audit` and `audit_events`, committed together.
Legacy `ledger_requests`/`ledger_events` tables are preserved, not reinterpreted;
`billing --details` shows the current audit format. Back up the database while the
service is stopped before switching versions. Removing the binary does not delete the
configured database, JSON file, or Keychain entries. Remove those separately only
when you intentionally want to erase your local state.
