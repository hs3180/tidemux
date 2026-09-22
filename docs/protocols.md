# Protocol support — 0.2.0 development

TideMux exposes both client protocols simultaneously. OpenAI clients use
`/v1/chat/completions`; Anthropic clients use `/v1/messages`. The configuration's
`protocol` selects only the upstream provider wire format, either `openai` or
`anthropic`; `base_url` is the single API root for that provider. Credentials,
gateway authentication, model identity and the local ledger remain shared.

| Feature | OpenAI provider | Anthropic provider |
| --- | --- | --- |
| OpenAI client endpoint | `/v1/chat/completions` | `/v1/chat/completions` |
| Anthropic client endpoint | `/v1/messages` | `/v1/messages` |
| Upstream endpoint | `/chat/completions` | `/messages` |
| Upstream credential | Bearer key | `x-api-key` |
| Upstream headers | Gateway-created auth and `X-TideMux-Session-ID` when present | Configured version, translated beta/session headers and `X-TideMux-Session-ID` when present |
| Model discovery | OpenAI-shaped `/v1/models` | OpenAI-shaped `/v1/models` |
| OpenAI client ↔ provider | Passed through after validation | Chat Completions translated to/from Messages |
| Anthropic client ↔ provider | Messages translated to/from Chat Completions | Passed through after validation |
| Streaming | Client format is preserved or translated to the selected provider format | Client format is preserved or translated to the selected provider format |
| Usage | Prompt/completion; cache details or DeepSeek hit/miss | Input/output plus cache read/creation translated to prompt/completion |

The two client endpoints are independent of the provider selection. For
example, Claude Code can call `/v1/messages` while `protocol: openai` is
configured, and an OpenAI client can call `/v1/chat/completions` while
`protocol: anthropic` is configured. Matching pairs are passed through;
non-matching pairs are translated. This is one provider configuration, not a
second base URL or a `protocol: both` mode.

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

Unknown top-level request/config fields, duplicate JSON keys, multiple JSON
documents, malformed tool envelopes and invalid beta headers are rejected.
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
