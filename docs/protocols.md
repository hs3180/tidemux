# Protocol support — 0.2.0 development

TideMux exposes both client protocols simultaneously. OpenAI clients use
`/v1/chat/completions`; Anthropic clients use `/v1/messages`. Named profiles
are keyed by provider name and each has one inferred or explicitly selected
`protocol`, its own `base_url`,
Keychain reference, default model, billing identity, model limits and prices.
`default_providers` maps each client protocol to the named provider that serves
it. Multiple named providers may use the same protocol; only the selected
default receives requests. In the target `provider add` flow, the first
provider added for a protocol is recorded as its default, and adding another
provider does not replace it. A hand-written configuration with multiple
providers for one protocol and no default must specify one explicitly. Gateway
authentication, active-session limits and the local ledger remain shared.

The 0.1.x single-provider configuration and its cross-protocol translation are
historical compatibility behavior, not part of the 0.2.0 provider model. A 0.2.0
provider serves only clients using its protocol; add one provider per protocol
when both client APIs are needed. Do not rely on migration of a legacy
single-provider configuration as a 0.2.0 compatibility guarantee.

For named providers, an explicitly configured `protocol` of `openai` or
`anthropic` forces that upstream format and skips detection. Otherwise TideMux
checks recognized endpoint host/path hints such as OpenAI, Anthropic and
DeepSeek. For other roots it sends an authenticated `GET` to `base_url + /models`
using OpenAI and Anthropic authentication shapes, then classifies the returned
model objects. It never sends a completion or message just to detect the
protocol. If it cannot confidently identify the format, startup fails with the
provider reference so the user can explicitly set the protocol. Model discovery
then uses the resolved protocol and the same endpoint. The older single-provider
detection behavior belongs to the 0.1.x compatibility path only.

| Feature | Named OpenAI provider | Named Anthropic provider |
| --- | --- | --- |
| OpenAI client endpoint | `/v1/chat/completions` | `/v1/chat/completions` |
| Anthropic client endpoint | `/v1/messages` | `/v1/messages` |
| Upstream endpoint | `/chat/completions` | `/messages` |
| Upstream credential | Bearer key | `x-api-key` |
| Upstream headers | Gateway-created auth and `X-TideMux-Session-ID` when present | Configured version, translated beta/session headers and `X-TideMux-Session-ID` when present |
| Provider model discovery | OpenAI-shaped `/models` at this provider's endpoint | Anthropic-shaped `/models` at this provider's endpoint |
| Matching client protocol | OpenAI client passes through after validation | Anthropic client passes through after validation |
| Non-matching client protocol | Not routed to this provider | Not routed to this provider |
| Streaming | Native OpenAI stream | Native Anthropic stream |
| Usage | Prompt/completion; cache details or DeepSeek hit/miss | Input/output plus cache read/creation translated to prompt/completion |

The client endpoints are independent of provider registration, but routing is
protocol-specific: each client goes only to the configured default with the
same protocol, with no cross-provider retry or cross-protocol translation. Each
named provider's `GET /models`
response is used only on its client route when it is a complete, recognized model list.
By default that catalogue is informational: any model ID is forwarded to the
selected provider. Setting the provider's optional `supported_models` list
restricts completion requests and the gateway's model listing to that subset.
If a provider does not expose a complete recognizable list, its route returns
an empty model catalogue unless an explicit allowlist is configured; direct
requests are still sent to that provider. The current development build may
accept existing 0.1.x single-provider configurations during transition; this is
a compatibility bridge, not a 0.2.0 guarantee.

The following translation details describe the historical 0.1.x
single-provider mode only; they do not apply to 0.2.0 named-provider routing.
That legacy mode translated text, system/developer instructions, tools, tool
calls/results, stop sequences, output schemas and streaming terminal events in
both directions. Provider-specific features without an equivalent on the other
wire format remained unsupported rather than silently forwarded.

Explicit parameters are retained; provider acceptance is not inferred from the
model name. The 0.1.x compatibility adapter handled Anthropic system-role
messages and cache markers when its upstream was Anthropic; for an OpenAI
upstream it translated only the supported top-level system/text subset and
rejected unsupported provider-only blocks. This historical behavior is not a
cross-protocol routing promise for 0.2.0. Providers can reject beta features;
TideMux does not change system instructions into user text.

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
every upstream model is supported. See the [0.1.1 client acceptance matrix](client-compatibility.md)
for prior-release evidence; it does not certify the 0.2.0 routing model.

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
