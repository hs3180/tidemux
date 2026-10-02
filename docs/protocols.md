# Protocol support — 0.3.0 candidate

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
resolved against model scope across all providers; when multiple providers can
serve an ID, the gateway returns `model_ambiguous` instead of picking one.
Configure `routing.shared_model_strategy` as `random` to opt in to choosing one
eligible provider for a shared bare model. Explicit
`REF/MODEL_ID` requests remain pinned to their selected provider.
Either OpenAI or Anthropic clients can select any provider; TideMux uses the
provider's configured upstream protocol and converts request/response
semantics only when the client and provider protocols differ. Model, budget and
timeout failures do not cause an implicit route change. Cross-provider billing
failover is disabled by default; when enabled, it requires an exact upstream
error code mapped to `insufficient_balance`, keeps the model unchanged and
tries providers that advertise the same model. Requests with Anthropic
server-hosted tools can use only Anthropic upstreams. It does not apply to
`model:auto` requests, whose provider/model choice remains pinned for the
request and stable session. Authentication failures, rate limits and safe
pre-header transport failures may retry another key only within the selected
provider's key group;
429 cooldowns honor `Retry-After`. No key or provider retry occurs after
response bytes may have reached the client. Gateway authentication,
active-session limits and the local ledger remain shared.

The 0.1.x single-provider configuration remains a migration/compatibility path;
do not rely on it as a 0.2.2 configuration guarantee.

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
| Shared bare model | Ambiguous by default; an opt-in strategy selects one eligible provider | Same policy; native Anthropic tools restrict candidates to Anthropic upstreams |
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

Unknown request fields are ignored with a gateway log warning. Unknown config
fields, duplicate JSON keys, multiple JSON documents, malformed tool envelopes
and invalid beta headers are rejected.
Native Anthropic routes preserve documented image, document, citation and
server-tool content blocks for the upstream to validate. Provider-hosted tools
such as `web_search_20250305` may omit `input_schema`; their provider-specific
fields are forwarded on native Anthropic routes. TideMux does not translate
these tools to OpenAI Chat Completions; a cross-protocol request receives a
field-specific unsupported-feature error. Audio, Responses API,
embeddings, batches and token-counting endpoints are not implemented. These
remain explicit boundaries; normal tested client workflows do not prove every
client feature or every upstream model is supported. See the
[0.1.1 client acceptance matrix](client-compatibility.md) for prior-release
evidence; it does not certify the 0.2.2 routing model.

## Streaming and errors

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
request, and model-access failures without treating every 403 as a billing
problem.

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

## Configuration and accounting

Defaults remain 1 MiB request, 8 MiB non-streaming response, 64 MiB SSE stream,
1 MiB SSE event, and 60 seconds after concurrency admission. All are configurable
through `limits`; see [provider and gateway CLI](provider-cli.md). Retained active sessions use
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
