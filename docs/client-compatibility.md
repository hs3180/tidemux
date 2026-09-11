# Client compatibility — 0.1.0

Checked on macOS on 2026-09-11 against the installed TideMux 0.1.0 binary.
**None of the three featured clients completes an agent workflow through this
release.** API format support alone does not establish client compatibility.

## Development repair progress

The repaired release remains **0.1.0**. The installed baseline below is unchanged;
these results apply to the development build on 2026-09-11.

| Client | Real DeepSeek file read, edit and test | Conversation continuation |
| --- | --- | --- |
| Claude Code 2.1.263 | Passed with an isolated profile | Product launcher, second-process continuation passed |
| Kilo CLI 7.6.2 | Passed with an isolated profile | Product launcher, second-process continuation passed |
| Hermes Agent 0.21.1 | Passed with an isolated profile | Product launcher, second-process continuation passed |
| Kilo VS Code extension 7.6.2 | Passed with an isolated profile | Window reload and full application restart follow-up passed |

The IDE run completed five successful SSE calls, including tool results and a
follow-up after window reload. The client fixed subtraction to addition, ran one
successful unittest, and left the test file unchanged. A subsequent `connect kilo-ide` run also passed real Keychain handoff, model
discovery, file read/edit/test, duplicate-launch rejection and continuation after
fully quitting and relaunching the dedicated VS Code instance.

Hermes handles unsupported structured output by retrying without response_format
after a safe 400 response. A LiteLLM comparison confirmed the same client-side
fallback unless LiteLLM explicitly drops that parameter. These are distinct
upstream attempts and remain separately audited.

The three CLIs have also exercised missing usage, truncation, cancellation,
queueing and recovery from upstream 429/5xx using controlled local upstreams.
Those fault scenarios remain pending for the IDE. Repaired-package installation,
installed-client acceptance and complete pricing reconciliation also remain open.
Unknown cost stays null. See [connection instructions](clients.md) and
[protocol boundaries](protocols.md); these results do not imply every provider
supports every client feature.

## Actual client runs — initial installed build

Each client received an isolated workspace containing `fixture.txt` and the task
“Read fixture.txt and reply with its contents.” Separate client profiles pointed
to a loopback observation proxy, which forwarded the original requests to
TideMux without changing the request body. Only request shape, status and safe
error metadata were recorded, not credentials or message bodies.

| Client tested | Connection | Observed result |
| --- | --- | --- |
| Claude Code 2.1.263 | Anthropic gateway; explicit `deepseek-flash`; bare print mode | `POST /v1/messages?beta=true` → 400 `invalid_request`. Request contains `stream`, `tools`, `metadata`, `output_config`, and `cache_control` on content blocks. |
| Kilo CLI 7.6.2 | Custom OpenAI-compatible provider; explicit model | `POST /v1/chat/completions` → 400 `invalid_request`. Request contains `stream=true`, `stream_options`, `tool_choice`, and `tools`. |
| Hermes Agent 0.21.1 | Named custom provider, Chat Completions transport, file tools | Capability probes return 404; an auxiliary request with `response_format` returns 400; the main request with `stream=true`, `stream_options`, and `tools` returns 400. |

These failures occur before any upstream call. Hermes proceeds after its failed
capability probes, so those 404s are not the immediate chat blocker. Kilo's IDE
extensions and each client's full interactive UI remain untested.

Two independent minimal HTTP controls through TideMux to DeepSeek succeeded,
one per protocol: 8 input tokens and 1 output token each, reconciled with SQLite.
The test configuration had no price snapshot, so estimated cost correctly stayed
unknown. These controls verify credentials, upstream reachability and the existing
text subset; they do not validate agent compatibility.

## Gaps identified in the initial installed build

| Priority | Gap | Completion criterion |
| --- | --- | --- |
| P0 | SSE streaming in both protocols, including usage and terminal/error events | Client receives incremental output; disconnects and truncated streams leave accurate audit status and unknown usage where needed. |
| P0 | Tool definitions, choices, assistant tool calls and subsequent tool results | A client reads the fixture through a tool and completes a second model turn; the gateway preserves tool IDs and structured content. |
| P0 | Explicit handling of client request fields | Support or clearly reject `stream_options`, `response_format`, `metadata`, `output_config`, cache controls and reasoning options, with protocol-specific validation and tests. Do not silently drop unknown fields. |
| P1 | Model metadata and endpoint discovery | Authenticated `/v1/models`, documented model/context limits and manual selection; evaluate Anthropic token counting separately. Do not emulate unrelated Ollama probe endpoints just to eliminate 404s. |
| P1 | Actionable diagnostics | Authenticated validation failures have a correlation ID and a field-specific safe explanation; keep rejected requests distinct from billable upstream attempts. |
| P1 | Client setup and credentials | Generate client-specific URL/model/protocol settings and supply the local gateway credential safely, without manually locating it in Keychain. |
| P1 | Profile management | Make OpenAI and Anthropic profile/port selection explicit. One current process serves only one protocol; no automatic conversion. |
| P1 | Operational limits | Validate the current 1 MiB request limit and 60-second upstream timeout against real agent contexts and long-running streams; expose appropriate configuration. |

The initial installed response validation accepts only text: supporting request-side
tools alone is insufficient. In that initial build, Anthropic beta headers are not forwarded
by the adapter; supported features and header handling need an explicit policy.

## Configuration findings

- Claude Code reached the proxy with `ANTHROPIC_BASE_URL` pointing at its root
  and the local gateway token supplied as `ANTHROPIC_API_KEY`. Setting a model
  explicitly avoids assuming a Claude model name is valid on the upstream.
- Kilo requires credentials referenced with `{env:...}` to be in trusted config,
  such as global config or `KILO_CONFIG_CONTENT`. The initial project-only setup
  failed before inference; a trusted configuration reached TideMux. This is a
  client configuration constraint, not a gateway protocol error.
- Hermes was configured with a named provider, `transport: chat_completions`,
  a `/v1` base URL, `key_env` and explicit model/context size. Auxiliary model
  requests must be included in compatibility and accounting tests.

Official configuration references:

- [Claude Code gateways](https://code.claude.com/docs/en/llm-gateway)
- [Kilo custom models](https://kilo.ai/docs/code-with-ai/agents/custom-models)
- [Kilo CLI configuration](https://kilo.ai/docs/code-with-ai/platforms/cli)
- [Hermes providers](https://hermes-agent.nousresearch.com/docs/integrations/providers/)

## Acceptance gate

For each client, test setup → model selection → text response → file read tool →
tool result → final response → ledger reconciliation. Then test cancellation,
upstream errors and concurrent requests. Record exact client versions and test
IDE extensions separately. Until that gate passes, the README examples remain
adaptation targets rather than working agent integrations.

## Development launcher

The current source includes [`tidemux connect`](clients.md), with real Keychain
handoff verified for all three CLIs. Hermes explicitly uses Chat Completions.
Claude preserves its observed beta headers, thinking signatures, cache markers
and tool history. DeepSeek accepts its mid-conversation system messages; this
provider extension is not guaranteed on every Anthropic-compatible endpoint.

The repaired build is not yet installed. Full compatibility is gated on remaining
setup, IDE fault and package acceptance, not only successful normal requests.
