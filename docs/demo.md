# Demo

This example applies to the current DeepSeek-only source candidate. The 0.1.0
release requires configurable OpenAI and Anthropic examples after those adapters
are implemented; the configuration below does not yet support that selection.

This is the executable local-MVP path. TideMux supports one non-streaming
DeepSeek chat route and listens only on a loopback address.

## 1. Build and create a local state directory

```bash
# From the source repository root
CGO_ENABLED=0 go build -o tidemux ./cmd/tidemux
mkdir -p "$HOME/Library/Application Support/TideMux"
```

## 2. Add the two secrets in Keychain Access

In **Keychain Access**, add two generic-password items. Do not put either
secret in the JSON file, shell history, logs, or SQLite database.

| Service | Account | Password |
| --- | --- | --- |
| `com.tidemux.deepseek` | `default` | Your DeepSeek API key |
| `com.tidemux.gateway` | `default` | A locally generated Bearer token for clients of this gateway |

Create `tidemux.json`, replacing only the ledger path with your actual home
directory (the JSON parser does not expand `~`):

```json
{
  "listen_addr": "127.0.0.1:8787",
  "deepseek_keychain": {"service": "com.tidemux.deepseek", "account": "default"},
  "access_token_keychain": {"service": "com.tidemux.gateway", "account": "default"},
  "max_in_flight": 1,
  "ledger_path": "/Users/you/Library/Application Support/TideMux/ledger.db"
}
```

## 3. Verify and serve

```bash
./tidemux doctor --config ./tidemux.json
./tidemux serve --config ./tidemux.json
```

`doctor` validates loopback configuration, verifies both Keychain items without
printing them, and checks that the ledger directory is writable. `serve` prints
its loopback URL and stops cleanly on Control-C.

## 4. Make one non-streaming OpenAI-compatible request

Use the local Bearer token you stored in Keychain:

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer <your-local-gateway-token>" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"Say hello in one sentence."}]}'
```

Successful and upstream HTTP-failed requests are appended to the local SQLite
ledger. The current candidate can miss transport/read failures or misclassify
malformed responses; pricing is not yet wired into the gateway. See
[acceptance status](mvp-acceptance.md). Invalid authentication, streaming requests, cancellation, and upstream
errors return a stable JSON error envelope without echoing secrets, prompts, or
provider response details.

## Reproducible no-key smoke test

No real API key is needed for the build and CLI smoke test:

```bash
CGO_ENABLED=0 go test ./...
```

The `cmd/tidemux` smoke test builds a fresh binary, uses a temporary Keychain
test double, and runs `tidemux doctor` against a temporary config and ledger
directory. It does not contact DeepSeek or persist a credential.
