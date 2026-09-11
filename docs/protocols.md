# Protocol support — 0.1.0

One configured API root per process. No provider/model whitelist or protocol
conversion. This table defines the supported text subset, not all vendor APIs.

| Feature | OpenAI compatible | Anthropic compatible |
| --- | --- | --- |
| Local endpoint | `/v1/chat/completions` | `/v1/messages` |
| Upstream suffix | `/chat/completions` | `/messages` |
| Upstream auth | Bearer upstream key | `x-api-key` upstream key |
| Protocol version | Endpoint configuration | Configured `anthropic-version` |
| Common input | model, messages, stream=false, temperature, top_p | Same |
| Token limit | max_tokens or max_completion_tokens | Required max_tokens |
| Instructions | system/developer messages | Top-level system |
| Stop | string/string-array stop | stop_sequences |
| Content | string or text blocks | string or text blocks |
| Success output | Provider JSON choices/usage preserved | Provider JSON content/usage preserved |
| Error output | OpenAI-style error object | Anthropic-style type=error envelope |
| Usage | prompt/completion, cached detail or DeepSeek hit/miss | input/output plus cache read/creation |

An optional `thinking: {"type":"disabled"}` is forwarded exactly when explicitly
provided; this supports DeepSeek Flash and compatible Anthropic endpoints. It is
not automatically added for official OpenAI endpoints. Enabled thinking is rejected.

Text blocks have only `type: "text"` and `text`. Other provider-specific fields,
images, tools, streaming, Responses API, batches and embeddings are rejected or
unavailable. Models can impose stricter limits than this gateway; their errors
return a safe upstream-error code without raw provider detail. No client brand
is claimed fully compatible.

Request bodies are limited to 1 MiB; upstream success responses to 8 MiB.
Duplicate JSON keys, multiple documents and unknown request/config fields are
rejected. Requests use a 60-second upstream timeout and never follow redirects.
The local gateway credential is never forwarded upstream. Authentication and
preflight validation errors are not provider attempts and do not create audit rows.

Input tokens in the audit are total input: Anthropic cache read/creation counts
are added to input_tokens; OpenAI cached counts are subsets of prompt_tokens.
Missing/invalid usage cannot become a fabricated successful cost estimate.
No automatic pricing discovery, retries, budget enforcement or cache tuning.

Implementation references, checked 2026-09-11:

- [OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
- [Anthropic Messages](https://platform.claude.com/docs/en/api/http/messages/create)

DeepSeek-specific compatibility references:

- [Anthropic format](https://api-docs.deepseek.com/guides/anthropic_api/)
- [Thinking toggle](https://api-docs.deepseek.com/guides/thinking_mode/)
