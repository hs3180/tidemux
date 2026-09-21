# Protocol support — 0.1.1

The original 0.1.0 supported text/non-streaming requests. The 0.1.1 release
extends that baseline as below. Release acceptance covers three CLIs.
Each process uses one configured upstream root. It may expose one protocol or,
with `protocol: "both"`, both native routes at the same time. No provider/model
whitelist or cross-protocol conversion is applied.

| Feature | OpenAI compatible | Anthropic compatible |
| --- | --- | --- |
| Inference endpoint | `/v1/chat/completions` | `/v1/messages` |
| Upstream suffix | `/chat/completions` | `/messages` |
| Upstream credential | Configured Bearer key | Configured `x-api-key` |
| Protocol headers | Gateway-created auth and `X-TideMux-Session-ID` when present | Configured version, validated client `anthropic-beta`, and `X-TideMux-Session-ID` when present |
| Model discovery | `/v1/models`, `/models`, and model detail | Same paths, Anthropic-shaped model objects |
| Text | String or text blocks | String or text blocks; top-level system |
| Tools | Function definitions, choices, call IDs, tool messages | Custom tools, choices, tool_use/tool_result blocks |
| Streaming | Chat Completions SSE and `[DONE]` | Messages SSE and `message_stop` |
| Usage | Prompt/completion; cache details or DeepSeek hit/miss | Input/output plus cache read/creation |
| Reasoning | `reasoning_content`, `reasoning_effort`, compatible thinking toggle | Thinking modes/display; signed and redacted history blocks |
| Formatting | `response_format` text/json_object/json_schema | `output_config` effort/schema |
| Client metadata | `stream_options`, parallel tool calls | `metadata.user_id`, cache_control, context_management object |

With `protocol: "both"`, `/v1/chat/completions` selects the OpenAI upstream
root and `/v1/messages` selects the Anthropic root. `anthropic_base_url` is
optional; when omitted, the configured `base_url` is reused. Credentials,
gateway authentication, model identity and the local ledger remain shared.

Explicit parameters are retained; provider acceptance is not inferred from the
model name. Anthropic-compatible system-role messages within the message list
are preserved, including position and cache markers. Claude's observed requests
carry a mid-conversation-system beta declaration. Providers can reject this or
other beta features; TideMux does not change system instructions into user text.

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
