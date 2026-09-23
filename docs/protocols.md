# Protocol support — 0.2.0 development

TideMux exposes both client protocols simultaneously. OpenAI clients use
`/v1/chat/completions`; Anthropic clients use `/v1/messages`. A legacy profile
can keep one `base_url`, whose provider protocol TideMux detects at startup.
New profiles may instead configure independent protocol-keyed
`providers.openai` and `providers.anthropic` records below one shared
`base_url`. Each provider owns its Keychain reference, default model, billing
identity, model limits and prices. When both are present, each client protocol
routes strictly to its matching provider at that common API root; when only one
is present, the other client route uses the existing translator. Gateway
authentication, active-session limits and the local ledger remain shared.

Detection is local for recognized roots such as OpenAI, Anthropic and the
DeepSeek preset. For another root, TideMux sends an authenticated `GET` to
`base_url + /models` using the two standard authentication shapes and classifies
the returned model objects. It never sends a completion or message just to
detect the protocol. If neither a root hint nor a recognizable model response
is available, `serve` stops with an actionable configuration error.

| Feature | Detected OpenAI provider | Detected Anthropic provider |
| --- | --- | --- |
| OpenAI client endpoint | `/v1/chat/completions` | `/v1/chat/completions` |
| Anthropic client endpoint | `/v1/messages` | `/v1/messages` |
| Upstream endpoint | `/chat/completions` | `/messages` |
| Upstream credential | Bearer key | `x-api-key` |
| Upstream headers | Gateway-created auth and `X-TideMux-Session-ID` when present | Configured version, translated beta/session headers and `X-TideMux-Session-ID` when present |
| Provider model discovery | OpenAI-shaped `/models` or root hint | Anthropic-shaped `/models` or root hint |
| OpenAI client ↔ provider | Passed through after validation | Chat Completions translated to/from Messages |
| Anthropic client ↔ provider | Messages translated to/from Chat Completions | Passed through after validation |
| Streaming | Client format is preserved or translated to the selected provider format | Client format is preserved or translated to the selected provider format |
| Usage | Prompt/completion; cache details or DeepSeek hit/miss | Input/output plus cache read/creation translated to prompt/completion |

The client endpoints are independent of upstream selection. A profile with one
configured provider continues to support both clients through pass-through or
translation. A profile with both providers routes each client to its matching
provider, with no cross-provider retry. Each provider's `GET /models` response
is used only on its client route when it is a complete, recognized model list.
Requests for a model absent from a recognized provider list receive
`model_not_found` before an upstream completion is sent. If a provider does not
expose a complete recognizable list, its route returns an empty model catalogue
and does not claim its configured default model is available; direct requests
are still sent to that provider. Existing single-provider profiles that
explicitly contain `"protocol": "openai"` or `"protocol": "anthropic"` remain
compatible.

The translation boundary covers text, system/developer instructions, tools,
tool calls/results, stop sequences, output schemas and streaming terminal
events in both directions. Provider-specific features that have no equivalent
on the other wire format remain explicitly unsupported rather than silently
forwarded.

Explicit parameters are retained; provider acceptance is not inferred from the
model name. Native Anthropic client requests can include Anthropic-compatible
system-role messages and cache markers when the upstream is Anthropic. When the
upstream is OpenAI, only the supported top-level system/text subset is
translated; unsupported provider-only blocks are rejected. Claude's observed
requests carry a mid-conversation-system beta declaration. Providers can
reject this or other beta features; TideMux does not change system instructions
into user text.

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
Images, document/audio blocks, provider server tools, Responses API, embeddings,
batches and token-counting endpoints are not implemented. These remain explicit
boundaries; normal tested client workflows do not prove every client feature or
every upstream model is supported. See [client acceptance](client-compatibility.md).

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
unsupported structured-output field. Other upstream failures use safe gateway
errors. TideMux itself does not automatically retry. Clients may retry or choose
a different transport, creating separate auditable attempts.

## Configuration and accounting

Defaults remain 1 MiB request, 8 MiB non-streaming response, 64 MiB SSE stream,
1 MiB SSE event, and 60 seconds after concurrency admission. All are configurable
through `limits`; see [configuration](configure.md). Retained active sessions use
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
when configured, rolling five-hour and seven-day budget checks are enforced
before upstream requests. No cache tuning or claimed savings are provided.
