# Privacy

TideMux is a local macOS gateway. Its JSON configuration stores Keychain
references; the upstream key and local Bearer token are read at runtime.

Requests, including message contents, are sent to the configured upstream
(selected OpenAI or Anthropic compatible API). The provider processes this traffic under its own terms.
TideMux has no separate hosted relay, account service, telemetry, or crash uploader.

The local SQLite ledger stores request metadata, token/cache counts, status,
latency, events, and estimated-cost fields. It does not intentionally store
API keys or full prompt/response content. Unknown usage and estimated cost
remain null. See [accounting](docs/accounting.md) for ledger fields and
[protocol support](docs/protocols.md) for request-routing boundaries.

Configuration and the ledger remain at the paths you choose. Diagnostic export
and upload features are not implemented. If you manually share logs or ledger
extracts, review them first; metadata may reveal usage patterns.

Material changes will be recorded in [CHANGELOG](CHANGELOG.md).

Updated: 2026-09-24.
