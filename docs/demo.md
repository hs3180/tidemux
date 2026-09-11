# Local demo

Requires macOS, Keychain Access and a non-streaming text-capable OpenAI or
Anthropic compatible endpoint. The source candidate is `0.1.0-rc.2`.

## Build or extract

From the source root with Go 1.27 or later:

```sh
CGO_ENABLED=0 go build -o tidemux ./cmd/tidemux
./tidemux version
mkdir -p "$HOME/Library/Application Support/TideMux"
```

Alternatively extract the candidate archive and run its `tidemux` binary.
The arm64 candidate has been tested on macOS 15.7.4; other OS versions are not
verified. No public download or Homebrew installation is claimed yet.

## Credentials and configuration

In Keychain Access create two generic-password items:

| Service | Account | Password |
| --- | --- | --- |
| `com.tidemux.openai` (or `com.tidemux.anthropic`) | `default` | Your upstream API key |
| `com.tidemux.gateway` | `default` | A different locally generated secret for gateway clients |

Copy [OpenAI config](../examples/openai.json) or
[Anthropic config](../examples/anthropic.json) to `tidemux.json`. Set:

- `base_url`: the exact API root **including its version/prefix**, for example
  `https://your-endpoint.example/api/v1`. TideMux appends `/chat/completions` or
  `/messages` exactly once. Trailing slashes are ignored. HTTPS is required
  except for numeric loopback HTTP. URLs cannot contain credentials/query/fragment.
- `model`: your actual model ID, used when a request omits model.
- `upstream_id`: a short non-secret label for ledger records.
- `ledger_path`: an absolute path in an existing writable directory; `~` is not expanded.
- `max_in_flight`: 1–1024. Start with 1; this caps simultaneous upstream calls.
- `anthropic_version`: explicit protocol version for Anthropic, example `2023-06-01`.

Do not put credentials in JSON, terminal history, logs, or source control.
Old `deepseek_*` configs are rejected; migrate to the generic example explicitly.

```sh
./tidemux doctor --config ./tidemux.json
./tidemux serve --config ./tidemux.json
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

```sh
./tidemux ledger --config ./tidemux.json
```

This prints the 100 most recent audit records as JSON without retrieving keys.
`null` token/count/cost means unknown. Cancellation is not evidence of zero
upstream billing. If the gateway reports `audit_failed_do_not_retry_blindly`,
the provider may already have executed the call; inspect storage and upstream
usage before retrying. TideMux does not automatically retry any call.

## Optional pricing

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

Missing prices produce unknown cost. Positive cached usage requires the
corresponding price. Unknown cache breakdown with differential rates also produces
unknown cost. Estimates are token arithmetic, not provider invoices; request fees,
service-tier adjustments, tool fees and taxes are outside this model.

## Live verification

With `serve` running, a verified price entry configured, and authorization to make
one minimal request, run from the source root:

```sh
python3 scripts/verify_live.py --config ./tidemux.json --output /tmp/tidemux-live-openai.json
```

Use a separate Anthropic config/evidence file for the other protocol. The helper
reads the local token into memory, makes one request, and checks response usage
against the persisted record and recomputes cost. It does not print/save the
prompt, response content or key. Review sanitized evidence before sharing it.

## Persistence and removal

New records are in `request_audit` and `audit_events`, committed together.
Legacy `ledger_requests`/`ledger_events` tables are preserved, not reinterpreted;
`ledger` shows only the new audit format. Back up the database while the service
is stopped before switching versions. Removing the binary does not delete the
configured database, JSON file, or Keychain entries. Remove those separately only
when you intentionally want to erase your local state.
