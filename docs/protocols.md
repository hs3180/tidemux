# Protocol support — 0.3.2

TideMux exposes both client protocols simultaneously. OpenAI clients use
`/v1/chat/completions`; Anthropic clients use `/v1/messages`. Each named
provider has one inferred or explicitly selected upstream `protocol`, its own
`base_url`, Keychain reference, model limits and prices. There is no default
provider or model. A request may use a bare upstream model ID when exactly one
provider's configured `supported_models` scope selects that ID. An unscoped
provider allows every model and can therefore make a bare ID ambiguous; use
`REF/MODEL_ID` for explicit provider selection. When a request has a known
`REF/` prefix, the gateway routes to that provider and removes only the prefix
before forwarding. Upstream model IDs containing slashes remain supported when
identified by a configured scope or discovered catalog.

Provider selection is independent of the client's API protocol. Bare IDs are
resolved against eligible model scopes across providers. If several providers
can serve an ID, the default is to return `model_ambiguous`; `routing`
configuration can opt into `random` or `price_priority` selection. Either
OpenAI or Anthropic clients can select any provider when its upstream protocol
can represent the request. Anthropic provider-hosted tools require an
Anthropic-protocol upstream. Once selected, the provider remains fixed for the
response unless explicit billing-exhaustion failover is enabled.

With `random`, stable `X-TideMux-Session-ID` or Anthropic `metadata.user_id`
(header first) binds a shared bare model to one provider. The hashed binding
includes the authenticated caller namespace, client protocol and model ID,
has a refreshed 24-hour idle TTL, and exists only in the gateway process.
No stable ID means per-request random selection. A valid active binding takes
priority over connection reuse. Removed, out-of-scope or unavailable
provider/model pairs, including unavailable clients or all-key cooldown, lose
their bindings; later requests select again under the routing rules.
Session-bound requests do not switch providers after dispatch, including on
mapped billing exhaustion; 429 retries
stay on the selected provider. Explicit provider routes and auto model chains
have separate behavior, and gateway restarts clear the bindings.

`model:auto` uses the single instance `auto_chain` of ordered provider/model
pairs. `REF/auto` is rejected. Stable auto sessions retain their original pair
while it remains valid. Safe exact model-not-found, temporarily-unavailable or
insufficient-balance failures invalidate the affected route for later requests;
those requests reselect without immediately choosing the failed pair again.
The failed auto-chain entry remains skipped until its connection generation
changes or the route is removed from an applied configuration.
A safe pre-header transport failure can also advance the preference before
output. The failed request is never replayed on the next entry. Requests without
stable IDs receive fresh request-scoped IDs.
Bindings and the preference are process-local and reset on restart. Explicit
model requests do not use the chain, and streaming output never triggers replay
on another entry.
Confirmed model-not-found, insufficient-balance or temporarily-unavailable
errors after SSE output has started still invalidate the route for the next
request, including one with the same session ID. The current stream retains
its route and settlement snapshot. Cancellation, unclassified errors and
transport errors after output do not themselves invalidate bindings or advance
the preference.
Authentication failures and HTTP 429 retries stay within the selected
provider's key group; 429 cooldowns honor `Retry-After` and never switch
providers. Cross-provider failover is opt-in and only follows an exact
`insufficient_balance` error mapping to a provider using the same client protocol
and requested model. Session-bound shared-model requests and auto-chain requests
do not switch providers within a dispatched request. Arbitrary 403 responses do
not trigger failover.
Gateway authentication and the local ledger remain shared. An optional
gateway-wide logical-session ceiling coexists with independent provider limits.

Endpoint, resolved-key, protocol or API-version changes preserve a healthy
binding when the same provider/model remains configured, eligible and available;
later requests use the new view. Connection generations isolate prompt-cache
history and failed-route state, and an older view cannot contaminate active
bindings or a newer generation's failure state. Removing a provider clears its
bindings; re-adding it creates a new generation. Already-started SSE and
requests queued on an older view keep
their original configuration and settlement snapshot. A successfully applied
configuration removes bindings invalidated by provider/model removal or scope
changes; an invalid reload retains the entire last valid configuration. See
[automatic provider application](provider-cli.md#automatic-provider-application-031).

For shared-model bindings, a model-not-found failure remains ineligible until
the connection generation changes or the route is removed from an applied
configuration. Insufficient-balance and temporarily-unavailable failures have
a five-minute protection period. Recovery makes the route eligible again but
does not move a healthy binding back from another provider.

The 0.1.x single-provider configuration remains a migration/compatibility path.
New 0.3.0 routing fields are omitted until enabled, so an unchanged 0.2.2
configuration loads with routing disabled.

For named providers, an explicitly configured `protocol` of `openai` or
`anthropic` forces that upstream format and skips detection. Otherwise TideMux
sends bounded, authenticated `GET base_url/models` probes using both
authentication shapes and checks each successful JSON response against the
OpenAI and Anthropic model-list schemas. URL host and path names do not select a
protocol. TideMux checks both candidates even after one schema matches; it
starts a provider only when exactly one protocol is confirmed. If both or
neither match, that provider remains unavailable. When other providers resolve,
the gateway starts in a degraded state, logs the provider reference and the
`tidemux provider update REF --protocol openai|anthropic` recovery command,
excludes the unresolved provider from model listings, and returns
`provider_unavailable` for explicit requests to it. If no provider resolves,
startup fails with the same recovery guidance. No completion or message is sent
for detection. The probe has a five-second total deadline, reads at most 1 MiB
per response, and refuses redirects so credentials cannot be forwarded. Model
discovery then uses the resolved protocol and the same endpoint. Old `auto`
profiles may need an explicit protocol if their `/models` response is
ambiguous or non-standard; detection does not rewrite stored configuration.

| Feature | Named OpenAI provider | Named Anthropic provider |
| --- | --- | --- |
| OpenAI client endpoint | `/v1/chat/completions` | `/v1/chat/completions` |
| Anthropic client endpoint | `/v1/messages` | `/v1/messages` |
| Upstream endpoint | `/chat/completions` | `/messages` |
| Upstream credential | Bearer key | `x-api-key` |
| Upstream headers | Gateway-created auth and `X-TideMux-Session-ID` when present | Configured version, translated beta/session headers and `X-TideMux-Session-ID` when present |
| Provider model discovery | OpenAI-shaped `/models` at this provider's endpoint | Anthropic-shaped `/models` at this provider's endpoint |
| OpenAI client | Bare model ID when one provider scope matches, otherwise `REF/MODEL_ID`; native when provider is OpenAI | Bare model ID when one provider scope matches, otherwise `REF/MODEL_ID`; conversion applies |
| Anthropic client | Bare model ID when one provider scope matches, otherwise `REF/MODEL_ID`; conversion applies | Bare model ID when one provider scope matches, otherwise `REF/MODEL_ID`; native when provider is Anthropic |
| Shared bare model | Ambiguous by default; `routing.shared_model_strategy` can select among eligible providers | Same policy; native Anthropic tools restrict candidates to Anthropic upstreams |
| `model:auto` | Uses the single instance provider/model chain; valid active bindings survive, invalid routes are reselected on later requests, and failed requests are not replayed; `REF/auto` is rejected | Same policy; unsupported features do not themselves invalidate a route |
| Streaming | Native OpenAI stream | Native Anthropic stream |
| Usage | Prompt/completion; cache details or DeepSeek hit/miss | Input/output plus cache read/creation translated to prompt/completion |

The gateway's `GET /models` response combines available provider catalogs and
returns qualified `REF/MODEL_ID` values in the requesting client's protocol
shape. When a provider exposes a complete, recognized model list, those IDs
appear with the provider reference prefixed. By default the catalog is
informational: a qualified model ID is forwarded to the selected provider.
Setting a provider's optional `supported_models` list restricts requests and
the catalog to that provider's upstream model IDs (without the `REF/` prefix).
If a provider does not expose a complete recognizable list, it contributes an
empty catalog unless an explicit allowlist is configured; direct qualified
requests are still sent to that provider.

The adapter provides cross-protocol conversion for named providers and the
single-provider compatibility configuration. A same-protocol request uses its
native route and is not round-tripped through the converter.

Cross-protocol conversion covers supported text/system messages, tools and
tool results, token limits, stop conditions, JSON-schema output where an
equivalent exists, usage, finish reasons and SSE events. Unsupported optional
or out-of-dialect fields are omitted and their paths are logged. For example,
reasoning_effort and Anthropic effort controls are not treated as interchangeable
without a declared mapping; cache markers have no OpenAI Chat Completions
equivalent. An explicit Anthropic `thinking` control can pass to an Anthropic
provider, while enabled thinking and context-management requests cannot be
represented by an OpenAI provider and are rejected as
`unsupported_request_feature` with the precise `param` path and a message that
names the field and explains that the configured upstream protocol cannot
represent it. Use an Anthropic-protocol provider for these controls; if a
client offers a setting to disable context management, that is another option.
Native Anthropic routes preserve the field. OpenAI
system/developer messages that occur after conversation messages must be moved
to Anthropic's top-level system field; this is done with a diagnostic. OpenAI
tool `strict` settings are likewise omitted with a diagnostic when the target
does not support them.

`end_turn`, `max_tokens` and `tool_use` stop reasons map to their OpenAI
counterparts. An Anthropic stop sequence or refusal marker can be flattened to
an ordinary stop and is logged. A pause-turn, content-filter, or unknown finish
reason has no safe equivalent and returns a field-specific translation error.

Provider-owned response values are never fabricated. Anthropic thinking text
can be represented as OpenAI reasoning_content, but an OpenAI reasoning-only
response cannot be turned into an Anthropic thinking block without a valid
provider signature; TideMux returns an explicit translation error instead of
an empty successful response. When ordinary text or tool output is also
available, unrepresentable reasoning is omitted with a diagnostic. TideMux does
not rewrite system instructions into user text.

The client-provided `X-TideMux-Session-ID` is validated and forwarded to the
configured provider unchanged. If active-session limiting is enabled and the
client does not provide an ID, TideMux creates a request-scoped ID for the
provider call.

OpenAI `max_tokens` and `max_completion_tokens` are mutually exclusive. Anthropic
requires `max_tokens`. Temperature, top_p and protocol-specific stop fields are
validated. Tools and history use the selected protocol's wire format; no tool
execution occurs inside TideMux. Thinking/format options must match their wire
schema, but actual reasoning and schema enforcement depend on the upstream.

Unknown request fields are ignored with a gateway log warning. Unknown fields
inside an Anthropic provider-hosted tool are preserved on native Anthropic
routes without logging their values. Unknown config fields, duplicate JSON
keys, multiple JSON documents, malformed custom-tool envelopes and invalid
beta headers are rejected.
Native Anthropic routes preserve documented image, document, citation and
server-tool content blocks for the upstream to validate. Provider-hosted tool
definitions such as `web_search_20250305` retain their custom fields and do not
require the custom-tool `input_schema`. TideMux does not translate these tools
or blocks to OpenAI Chat Completions; a cross-protocol request receives an
actionable `unsupported_request_feature` error on `tools`. Audio, Responses API,
embeddings, batches and token-counting endpoints are not implemented. These
remain explicit boundaries; normal tested client workflows do not prove every
client feature or every upstream model is supported. See the
[current client acceptance matrix](client-compatibility.md) for the tested
versions, route combinations and artifact boundaries; historical release
evidence is identified separately on that page.

## Streaming and errors

A generic Anthropic `tool_result` belongs in a **user** message, following an
assistant `tool_use`. Some upstream built-in `webReader` / `analyze_image`
implementations emit a generic `tool_result` in an assistant response instead
of their provider-specific server-tool result block. Saving that response can
poison subsequent history, with or without compaction. This is an upstream
protocol defect; TideMux does not execute those built-in tools or repair stored
client history silently.

TideMux rejects already-poisoned native or converted requests before dispatch
with HTTP 400, `invalid_tool_history`, and an exact path such as
`messages[1].content[1].type`. Malformed upstream responses produce HTTP 502
`invalid_upstream_tool_history`; a malformed SSE block is withheld and the
stream ends with an error event, possibly after HTTP 200 has started. Previously
delivered valid frames remain delivered. The diagnostic exposes a structural
path, never tool contents or IDs, and includes `recovery.retryable: false`.
Unchanged poisoned history must not be retried. Start a clean conversation or
explicitly repair the saved history in the client, and use client-executed
tools as a workaround. TideMux preserves valid native `server_tool_use` and
provider-specific result blocks, valid user `tool_result`, and compaction;
it cannot guarantee the provider's built-in tool correctness.

The binary-level reproduction and valid two-turn controls are in
`scripts/test_tool_history_package.py`. Existing server-tool and compaction
gates remain required.

Frames are delivered incrementally, preserving LF/CRLF. Anthropic event names must
agree with their payload type. The final frame is held until terminal audit
storage succeeds. A missing terminal marker, upstream error, read failure or
size limit produces a safe failure, not a successful completion. A failure after
HTTP 200 starts is emitted as an SSE error. Client cancellation retains the
concurrency slot until the upstream body is closed.

HTTP 429 is preserved as a rate-limit error. Upstream 400/422 parameter rejections
retain their status with a safe, recognized parameter code when available; raw
provider bodies are not returned. This allows Hermes to retry without an
unsupported structured-output field. Provider-specific error codes can be
classified per named provider with `error_code_mappings`; only exact codes
match, optional HTTP status conditions take precedence over unqualified rules,
and arbitrary provider message text is never used to infer billing or policy
errors. Mapped failures use protocol-native error envelopes, stable public
codes, and safe guidance; a safe provider code is included when available.
Mapped errors retain a compatible upstream 4xx status where that preserves the
meaning of the classification; rate limits normalize to 429. Exact mappings
can distinguish balance, authentication, permission, policy, malformed
request, model-access and temporary-unavailability failures without treating
every 403 as a billing problem.

TideMux retries HTTP 429 on the same selected key and provider at most three
total attempts, following `Retry-After` seconds or HTTP-date values. Invalid or
missing values use a one-then-two-second backoff. The request timeout bounds
total waiting; if the indicated delay does not fit, TideMux returns the mapped
rate-limit error without an early retry and includes `Retry-After`. A 429 never
moves to a different key; if same-key attempts are exhausted, TideMux returns
the normalized rate-limit error. Authentication and safe pre-write transport
failures keep their existing same-provider key failover behavior.
Cancellation interrupts a wait, and no retry occurs after response bytes may
have reached the client. A recognized error reported inside an SSE stream is
sent as the current client protocol's `error` event; a stream already in
progress is never replayed.

Local session-capacity exhaustion is HTTP 429 with
`error.code: "active_session_limit"` in both protocols, distinct from an
upstream rate limit. The error includes `scope: "gateway"` for the overall
ceiling or `scope: "provider"` and `provider_ref` for a provider limit, the
configured `limit`, and a `retry` object with `strategy: "exponential_backoff_with_jitter"`,
`initial_delay_seconds: 1` and `max_delay_seconds: 30`. There is no `Retry-After`
header because ongoing activity makes capacity-release timing unknown. Clients
can retry an unadmitted request using full jitter: choose a random delay between
zero and `min(30, 2^attempt)` seconds, starting at attempt zero, while respecting
their own deadline. An initial capacity rejection sends no upstream request.
During an already-supported provider failover, a full target is not dispatched;
previous attempts' audit and budget records remain intact. Capacity exhaustion
does not introduce a new reason to switch providers.

SIGTERM, Ctrl-C and graceful server shutdown stop admission of new model calls
and cancel active upstream calls and queued requests. Before headers are sent,
clients receive HTTP 503 with `error.code: "server_shutting_down"`. A stream
that has started instead receives an `event: error` frame with that code and
then closes; it does not receive a success terminal marker. Partial output
must not be treated as a completed turn or automatically replayed. Handler
panics use the same JSON/SSE boundary with a redacted `internal_error`; panic
values and stack traces are excluded from runtime logs. Shutdown waits for
terminal audits and budget settlement before closing the ledger. A forced
termination or a disconnected client cannot receive a final error event.

## Configuration and accounting

Defaults are 32 MiB request (33,554,432 bytes), 8 MiB non-streaming response,
64 MiB SSE stream and 1 MiB SSE event. After concurrency admission, non-streaming
requests have a 60-second total upstream timeout; streams have ten minutes by
default, including time waiting for the first event. An explicit
`limits.upstream_timeout_seconds` overrides both modes. An explicit
`limits.request_bytes` continues to override the default. HTTP 413 includes
`error.code: "request_too_large"`, `limit_bytes`, and the configuration field
needed to adjust the limit. These defaults provide headroom for the Anthropic
[32 MB Messages contract](https://platform.claude.com/docs/en/api/errors#request-size-limits);
the configured provider may impose its own lower limit. All byte limits remain
configurable through `limits`. Retained active sessions use
a five-minute input/output idle timeout by default, configurable independently.
Queue waiting is cancelable and reported separately. Upstream redirects are not
followed.

Model limits are omitted unless explicitly configured under `model_capabilities`;
they are declarations, not measured model capabilities. Authentication and
validation failures receive a correlation ID and a separate `local_diagnostics`
record. They do not become upstream attempts or token/cost records.

Audit input tokens are total input: Anthropic cache read/creation counts are added
to input_tokens; OpenAI cache counts are subsets of prompt_tokens. Output includes
reasoning tokens when the provider includes them in completion usage. Missing
usage or pricing remains unknown. Pricing is never discovered automatically;
when configured for a provider, its rolling five-hour and seven-day budget is
enforced before upstream requests and isolated from other providers. No cache
tuning or claimed savings are provided.
