# Local demo

Requires macOS, Keychain Access and a non-streaming text-capable OpenAI or
Anthropic compatible endpoint. The release is `0.1.1`.

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
./tidemux configure --base-url https://api.deepseek.com --model deepseek-flash
./tidemux doctor
./tidemux serve
```

Paste your DeepSeek API key at the hidden prompt and press Enter. The CLI creates
the application directory, stores both secrets in Keychain, writes a private JSON
config, and verifies local readiness. It does not send a billable request.
See the [provider and gateway CLI guide](provider-cli.md) for the 0.2.0
development interface, including named providers and active-session settings.
Those commands are not available in the 0.1.1 release shown here; use the
single-provider commands and 0.1.x samples in this guide for the release.
The JSON samples linked below are 0.1.x single-provider examples; see the
[examples index](../examples/README.md) for their version scope and the 0.2.0
named-provider sample.

## Manual configuration (optional)

In Keychain Access create two generic-password items:

| Service | Account | Password |
| --- | --- | --- |
| `com.tidemux.provider` | `default` | Your upstream API key |
| `com.tidemux.gateway` | `default` | A different locally generated secret for gateway clients |

Copy [OpenAI config](../examples/openai.json) or
[Anthropic config](../examples/anthropic.json) to
`~/Library/Application Support/TideMux/config.json`, with file permissions 0600.
If a configuration already exists, stop the gateway and back it up before
replacing it. Set:

- `base_url`: the exact upstream API root **including its version/prefix**, for example
  `https://your-endpoint.example/api/v1`. TideMux appends `/chat/completions` or
  `/messages` exactly once. Trailing slashes are ignored. HTTPS is required
  except for numeric loopback HTTP. URLs cannot contain credentials/query/fragment.
- `listen_addr`: defaults to `127.0.0.1:4000` for loopback-only access. Set it
  to exactly `0.0.0.0:4000` to bind all IPv4 interfaces; other non-loopback IPs
  are rejected.
- provider protocol: detected automatically from `base_url` and, for generic
  roots, a safe `GET /models` response. Both client routes are always available:
  OpenAI clients use `/v1/chat/completions`, and Anthropic clients use
  `/v1/messages`. Matching client/provider protocols pass through; mismatched
  protocols are translated in either direction.
- `model`: your actual model ID, used when a request omits model.
- `upstream_id`: a short non-secret label for ledger records.
- `ledger_path`: the existing local ledger location, or for a first setup the
  absolute path to `~/Library/Application Support/TideMux/ledger.db` with your
  home directory written out. The directory must exist; `~` is not expanded in JSON.
- `max_in_flight`: 1–1024. Start with 1; this caps simultaneous upstream calls.
- `max_active_sessions`: optional top-level, gateway-wide 0–4096 cap for
  distinct logical sessions; zero disables it. New sessions receive HTTP 429
  after the cap is reached.
- `active_session_idle_timeout_seconds`: optional top-level idle timeout for
  retained sessions; zero uses the five-minute default, or set 1–86400 seconds.
- `anthropic_version`: optional Anthropic API version; when omitted, TideMux
  uses `2023-06-01` after detecting an Anthropic provider.

Do not put credentials in JSON, terminal history, logs, or source control.
Old `deepseek_*` configs are rejected; migrate to the generic example explicitly.

```sh
./tidemux doctor
./tidemux serve
```

`doctor` checks local config, Keychain retrieval and ledger-directory write
access. It does not contact the upstream. `serve` stays in the foreground;
Control-C stops it. There is one configured upstream per process, no fallback.
When `listen_addr` is `0.0.0.0`, `serve` warns that the gateway is reachable
from non-loopback interfaces; protect the port with a firewall and trusted
network.

## Request and inspect

Configure your HTTP client with the gateway API key/token (not the upstream key):

- OpenAI: `POST http://127.0.0.1:4000/v1/chat/completions`, Bearer authentication,
  body `{"model":"your-model","messages":[{"role":"user","content":"Hi"}]}`.
- Anthropic compatibility: `POST http://127.0.0.1:4000/v1/messages`, Bearer or `x-api-key`
  authentication with the **local token**, body
  `{"model":"your-model","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`.

In the 0.1.1 single-provider mode, the response keeps the shape of the client
API that was called and mismatched client/provider protocols are translated.
See the [0.1.1 client compatibility notes](client-compatibility.md) for release
evidence. This is not the 0.2.0 routing model; see
[0.2.0 protocol support](protocols.md). Every response adds
`X-TideMux-Request-ID` for audit correlation.

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

## Pricing

Pricing is selected together with the provider API key by `configure`. TideMux
uses its verified DeepSeek rates when the endpoint and model match a built-in
entry. For another provider, set rates on the same command; rates are **per
million tokens**:

```sh
tidemux configure \
  --base-url https://provider.example/v1 \
  --model your-model \
  --pricing-input-cache-hit 1 \
  --pricing-input-cache-miss 2 \
  --pricing-output 4 \
  --pricing-currency USD
```

Use `--pricing-currency`, `--pricing-source`, and `--pricing-version` when the
defaults (`USD`, `manual-cli`, and `manual`) are not appropriate. `budget` only
changes limits and does not accept or modify pricing.

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
using `configure` with that API root and `--replace`, and restore
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
