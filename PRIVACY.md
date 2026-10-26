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

Routing decisions also record bounded provider/model labels, retention/switch
reason, threshold, known wait/deadline and whether completion time is unknown.
Terminal JSONL/audit JSON share this metadata. Local rejections and optional
SQLite decision details commit together without becoming generation/cost rows.
No raw session IDs, request content or credentials belong in these decisions.

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

**Full-content output is separate and defaults to off**, including upgrades.
When explicitly enabled through `content_output`, authenticated generation
requests and downstream JSON/SSE response bytes are written in bounded chunks to
an independently configured private directory. This includes original prompts,
history, system messages, tool definitions/calls/results, visible reasoning and
inline media; a media URL is retained as a reference without downloading it.
Authentication headers, provider transport credentials and arbitrary HTTP
headers are not captured. The correlation envelope uses a pseudonymous session
ID. The original body itself is byte-exact: identifiers, credentials or other
secrets deliberately embedded in a message/metadata/tool payload are **not
redacted**. `data` and searchable `text` both contain body information. Restrict
directory/collector/index access accordingly. Summary logs and SQLite do not
receive these bodies.

TideMux does not ship this content or inherit a destination from summary log
collection. An independently configured collector can transmit it to destinations
you select and owns its credentials, retention and confirmed-delivery cleanup.
Disabling stops new captures after reload, lets admitted captures finish, and
does not erase existing files or stop that collector. Directory capacity is
bounded without automatic deletion; overflow can lose content. Read the
[operator setup](docs/guide.md#optional-full-content-collection) and
[completeness contract](docs/design.md#optional-full-content-output). No previous
content is recovered or backfilled when enabling the feature.

Configuration and the ledger remain at the paths you choose. Diagnostic export
and upload features are not implemented. If you manually share logs or ledger
extracts, review them first; metadata may reveal usage patterns.

Material changes will be recorded in [CHANGELOG](CHANGELOG.md).

Updated: 2026-10-26.
