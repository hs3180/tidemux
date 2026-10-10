# Client compatibility

TideMux serves OpenAI `/v1/chat/completions` and Anthropic `/v1/messages`.
The selected provider determines the upstream protocol. Matching pairs pass
through; the other pairs use bidirectional conversion within the
[protocol support](protocols.md) boundaries.

| Client | Local gateway API | Setup |
| --- | --- | --- |
| Claude Code | Anthropic Messages | `tidemux claude --model REF/MODEL` |
| Kilo CLI | OpenAI Chat Completions | `tidemux kilo --model REF/MODEL -- run '…'` |
| Hermes Agent | OpenAI Chat Completions | `tidemux hermes --model REF/MODEL -- -q '…'` |
| dsh | Select Chat Completions or Anthropic Messages in its custom provider | [Agent setup](agent-install.md#configure-the-requested-client) |
| Codex | Requires Responses for a custom model provider | Installation assistant; direct gateway model routing is unsupported. |

Launchers read only the gateway credential and check authenticated model
discovery. They maintain isolated persistent profiles and do not pass upstream
keys to clients. Hermes must use its custom `chat_completions` transport; its
OpenAI transport can select Responses, which TideMux does not serve. Kilo uses
an environment-variable credential reference. See [client setup](clients.md).

## Tools and continuation

TideMux carries tool calls/results but does not execute client tools. Native
Anthropic routes preserve provider-hosted blocks, citations and compaction,
including generic assistant `tool_result`, without enforcing tool-role or ID
pairing. Providers and clients validate their semantics. Converted routes reject
unsupported request features before dispatch; response conversion preserves
available text and reports fields without a target representation.

A continuing conversation stays in the launcher's persistent client profile.
TideMux does not rewrite client history. After a malformed response, start a new
conversation and resume only a successful history; commands are in
[recovery instructions](clients.md#recover-after-a-response-error).

## Streaming errors

An invalid block after legal content retains the delivered prefix and ends
with a safe `invalid_upstream_stream` error and request ID. The invalid private
fields are withheld. HTTP remains 200 after streaming begins, while the gateway
log and audit report failure. Buffered structural failures use
`invalid_upstream_response`. There is no replay after output may have reached
the client. Error displays and client-side retry behavior depend on the client;
use the wire or gateway log to diagnose the result.

## Capacity and recovery

Gateway and provider logical-session ceilings return `active_session_limit`
with the relevant `scope`, limit and bounded backoff guidance. They do not
invent `Retry-After`, and rejection causes zero upstream dispatches. A slot's
release depends on active calls and idle retention. Clients may hide a transient
429 while their SDK retries. See
[capacity recovery](clients.md#recover-after-capacity-is-full) and
[availability](availability.md) before interpreting an eventual answer.

## Verification boundary

Release acceptance uses actual installed Claude, Kilo and Hermes CLIs with
isolated profiles and a scripted loopback provider. It exercises file reading,
editing, shell tests, recovery into a new conversation, persistent continuation,
and both capacity scopes. The model scripts tool choices, so this verifies
client/gateway behavior rather than model reasoning, paid-provider pricing or
invoice reconciliation. Interactive TUIs, IDE extensions and complete dsh/Codex
agent workflows are outside that acceptance.

Exact client versions, source/archive/binary hashes, commands, dispatch counts
and audit outcomes belong in private release evidence and PRs. Historical
client versions do not certify a later archive. Reusable validation commands
are in the [release procedure](releasing.md#runtime-checks).
