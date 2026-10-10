# Client compatibility

The CLI matrix is verified on macOS arm64 with actual installed clients,
isolated profiles and a scripted loopback provider. Each
client ran through the TideMux launcher. The provider was a test fixture;
these results do not certify a paid model's reasoning, pricing or invoice.
The first verification used the v0.3.1 source baseline `4886a58`; the same
acceptance script must pass against the final 0.3.2 archive before release.
Use BUILD.txt and binary SHA256 to identify the artifact being tested.

| Client | Client → provider API | Real read/edit/test | New conversation recovery | Persistent continuation |
| --- | --- | --- | --- | --- |
| Claude Code 2.1.283 | Anthropic → Anthropic | Passed | Passed | Passed |
| Kilo CLI 7.8.1 | OpenAI → Anthropic | Passed | Passed | Passed |
| Hermes Agent 0.21.2 (2026.9.11), upstream `952c941e` | OpenAI → Anthropic | Passed | Passed | Passed |

The workflows read a fixture and the source file, repair a small function, run
`python3 -m unittest -v` through the client's actual shell tool, verify the test
file is unchanged, and continue the successful conversation in a new client
process. The mock model scripts the tool choices; TideMux does not execute the
tools. These checks use Claude's print mode, Kilo's `run` mode and Hermes's
oneshot mode. Interactive TUI rendering and the VS Code extension are outside
this verification.

## Error display and recovery

The controlled failure sends legal text before a content block whose `type`
is not a string. The native and converted routes retain the delivered prefix
and emit one safe terminal error with `code: invalid_upstream_stream` and the
request ID. The invalid block and its private fields are withheld. HTTP remains
200 after streaming begins; the gateway log and audit record report failure.
Buffered structural failures use `invalid_upstream_response`.

Client displays and automatic retry counts vary. Claude can display a generic
mid-response error, Kilo can expose the gateway code, and Hermes can report that
no visible answer was produced. Check the wire or gateway log before attributing
a failure to token limits. Each client request causes at most one dispatched
attempt after output begins; the gateway does not replay partial responses.
Logs and the ledger contain neither withheld content nor tool IDs.

Generic assistant `tool_result` is a separate compatibility case: native routes
preserve it without a tool-role or ID-pairing check. The packaged GLM
reproduction checks completion and continuation with matching IDs, unmatched
IDs and no preceding server-tool block. Each Claude turn must finish in exactly
one client request and upstream dispatch. Converted routes retain
available text and report server-tool fields without a target representation.
The fixture reproduces the reported structure; it is not a production capture.

The recovery check starts a new conversation, performs the complete tool
workflow, then resumes that successful conversation.
[Client setup and recovery](clients.md#recover-after-a-response-error) provides
the commands and safe diagnostics. The gateway does not edit client history.

## Protocol and profile boundaries

The gateway exposes OpenAI `/v1/chat/completions` and Anthropic `/v1/messages`.
The selected model chooses the provider; the provider's `protocol` controls its
upstream API. Matching pairs pass through and the other combinations use the
bidirectional adapter. See [protocol support](protocols.md) for the current
native server-tool, compaction and conversion boundaries.

Launchers read only the local gateway credential, check authenticated model
discovery and configure isolated persistent client profiles. Upstream keys are
not passed to clients. Hermes uses an explicit custom provider with Chat
Completions transport; its OpenAI provider can select Responses API, which
TideMux does not serve. Kilo uses an environment-variable credential reference.

The package wire regression also checks opaque web/image results, native
history with and without compaction, local rejection of unsupported converted
history, valid client tool-result continuation and structural failures after a
legal prefix. Provider-hosted tools cannot be translated to every client API.

## Capacity rejection and recovery

The installed-client check also occupies one logical session, then starts each
real CLI against a full gateway limit and, separately, a full provider limit.
Both cases must return `active_session_limit`, the correct `scope` and limit,
bounded backoff guidance, and no invented `Retry-After`. Rejected requests cause
zero upstream dispatches. The fixture releases the occupied session after the
first rejection and uses a one-second idle retention; that duration is a test
setting, not the production default or a promised release time.

| Client | Gateway limit | Provider limit | Observed recovery |
| --- | --- | --- | --- |
| Claude Code 2.1.283 | Passed | Passed | Retried automatically and completed after capacity became available. |
| Kilo CLI 7.8.1 | Passed | Passed | Retried automatically and completed after capacity became available. |
| Hermes 0.21.2 | Passed | Passed | Retried automatically and completed after capacity became available. |

These capacity checks used the combined 0.3.2 development binary; they remain
subject to the final archive gate. A client can hide a transient 429 while its
SDK retries. The evidence records actual client HTTP requests and the gateway's
`local_rejection` or `request_terminal` events as well as the final CLI output;
an eventual successful answer does not mean no rejection occurred. Use the
[capacity recovery instructions](clients.md#recover-after-capacity-is-full)
when the slot does not become available promptly.

## Reproduce acceptance

Install the three CLIs separately and put them on `PATH`. On macOS, use the
extracted candidate binary with the repository's acceptance scripts:

```sh
python3 scripts/test_tool_history_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_client_recovery_package.py --binary /path/to/extracted/tidemux --evidence /path/to/private/evidence
```

The real-client script requires all three clients and both capacity scopes by
default; `--client claude`,
`--client kilo` or `--client hermes` selects a diagnostic subset and does not
satisfy the three-client release gate. `--capacity-scope gateway` checks the
older baseline that lacks provider limits and does not satisfy the 0.3.2 gate.
It records exact versions, commands,
terminal output, safe wire errors, dispatch counts and audit outcomes. It uses
synthetic Keychain responses, temporary child-process homes/profiles and local
HTTP endpoints; it does not use production profiles or provider credentials.
The required wire regression runs in CI. The real-client check is a separate
installed-client acceptance gate, since CI does not install these clients.

## Historical evidence

The 2026-09-11 v0.1.1 matrix used Claude Code 2.1.263, Kilo 7.6.2 and Hermes
0.21.1 with real DeepSeek `deepseek-flash`. Its read/edit/test, persistent
continuation, usage estimates and install/rollback checks remain historical
release evidence. They do not certify the current named-provider routing,
current CLI versions or the 0.3.2 artifact. The original private evidence is
retained separately and is not substituted for the current acceptance results.
