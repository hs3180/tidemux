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

Failed upstream calls retain bounded failure metadata in the ledger and terminal
JSONL: source/stage, upstream HTTP and embedded status, safe provider type/code,
validated provider request ID, category, retryability and explicit integer token
facts. Codes are short numeric values, recognized standard API identifiers or
codes explicitly trusted by configured mappings. Other identifiers are redacted.
Opaque request IDs accept 8–128 ASCII letters/digits and `._:-`; URLs, whitespace
and credentials matching known values are excluded. IDs can let the provider
locate its own request record. Arbitrary upstream headers are never copied.

**Provider error reasons are forwarded and persisted by default.** The same
redacted reason appears in the client's primary `error.message` and in
`failure.provider_message` in client errors, SQLite and terminal/session JSONL.
Set `forward_error_messages: false` per provider to omit that text everywhere;
legacy single-provider configurations support the field at the root. Configure
false before upgrading from 0.3.4 to retain its text-free behavior. Already
admitted requests retain their admitted policy. Existing audit rows are unchanged.

Before truncation, TideMux removes known admitted gateway/provider credentials,
raw session values and common credential shapes (Bearer tokens, sk-prefixed keys,
long hex values and JWTs), email addresses and labeled account/key/token values.
It cleans control/format characters and terminal escapes and retains at most 512
Unicode characters. This is a bounded disclosure policy, not a guarantee that
free text contains no secrets: an upstream can echo prompt fragments, private
paths or unrecognized account details. Use false for providers requiring strict
text exclusion. Review logs/ledger exports and downstream access accordingly.
Unambiguous explicit token comparisons survive independently of the text switch;
these request-local facts never change configured or discovered model windows.

Terminal audits/logs also retain independently assigned key and connection labels,
not credentials or secret-derived hashes. They correlate the actual selected key
with authenticated runtime status within a pool/process and can reveal usage
patterns. Labels survive compatible policy reloads but reset with the process or
connection/key pool. Connection labels do not identify a physical provider server;
that remains unknown without provider-side correlation.
Failure metadata also records generated downstream retry signals: a boolean
header value and optional bounded waits in seconds/milliseconds. No arbitrary
upstream header values are copied into this response snapshot.

[Usage logging](docs/guide.md#logs-and-collection) runs by default and writes ccusage-compatible
JSONL directly when a request ends. It records model and token/cache counts;
unknown required counts are omitted. Session groups are keyed pseudonyms based
on the gateway credential, stable across restarts with that credential. Runtime
terminal events also include this usage envelope. Raw client session IDs and
credentials are excluded; full request/response content is not recorded, while
provider error reasons follow the policy above. Groups and token counts can reveal
activity patterns. Files are private; manage retention under your local log policy.

Configuration and the ledger remain at the paths you choose. Diagnostic export
and upload features are not implemented. If you manually share logs or ledger
extracts, review them first; metadata may reveal usage patterns.

Material changes will be recorded in [CHANGELOG](CHANGELOG.md).

Updated: 2026-10-16.
