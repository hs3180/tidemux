# Privacy

TideMux is a local macOS gateway. Its JSON configuration stores Keychain
references; the upstream key and local Bearer token are read at runtime.

Requests, including message contents, are sent to the configured upstream
(selected OpenAI or Anthropic compatible API). The provider processes this traffic under its own terms.
TideMux has no separate hosted relay, account service, telemetry, or crash uploader.

The local SQLite ledger stores request metadata, token/cache counts, status,
latency, events, and estimated-cost fields. It does not intentionally store
API keys or full prompt/response content. Unknown usage and estimated cost
remain null. See [billing](docs/guide.md#billing-and-budgets) for cost estimates and
[compatibility](docs/guide.md#compatibility) for request-routing boundaries.

Failed upstream calls also retain bounded failure metadata in the ledger and
terminal JSONL: source/stage, upstream HTTP and embedded error status, safe
provider code, validated provider request ID, category and retryability.
Codes are limited to short numeric codes, standard API error types or codes
explicitly trusted by configured error mappings. Other provider strings are
redacted, with a presence marker retained. Known credentials and raw session
values echoed in these fields are removed. Provider error bodies, messages,
account details and network addresses are never copied into failure metadata.
The same metadata is returned in client errors; request IDs may allow the
configured provider to locate its own request record.
Failure metadata also records generated downstream retry signals: a boolean
header value and optional bounded waits in seconds/milliseconds. No arbitrary
upstream header values are copied into this response snapshot.

[Usage logging](docs/guide.md#logs-and-collection) runs by default and writes ccusage-compatible
JSONL directly when a request ends. It records model and token/cache counts;
unknown required counts are omitted. Session groups are keyed pseudonyms based
on the gateway credential, stable across restarts with that credential. Runtime
terminal events also include this usage envelope. Raw client session IDs,
credentials and message content are absent. Groups and token counts can reveal
activity patterns. Files are private; manage retention under your local log policy.

Configuration and the ledger remain at the paths you choose. Diagnostic export
and upload features are not implemented. If you manually share logs or ledger
extracts, review them first; metadata may reveal usage patterns.

Material changes will be recorded in [CHANGELOG](CHANGELOG.md).

Updated: 2026-10-15.
