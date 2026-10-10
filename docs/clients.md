# Client setup

This guide describes the current named-provider CLI. Install the client you
want to use separately; TideMux does not install Claude Code, Kilo CLI or
Hermes Agent. Tested versions and error-display differences are documented in
[client compatibility](client-compatibility.md).

## Add the provider routes

Each provider has one upstream API protocol. Client protocol does not select a
provider; the request's `model` does. Add one or more provider endpoints:

```sh
tidemux provider add https://api.deepseek.com
tidemux provider add https://api.deepseek.com/anthropic/v1
tidemux provider list
```

Each command securely prompts for the upstream API key. Unless `--protocol` is
supplied, TideMux identifies the provider protocol from bounded authenticated
`/models` schema probes, not the endpoint's name. All models are allowed by
default; pass `--model MODEL[,MODEL...]` to restrict a provider during setup, or use
`tidemux provider models REF --only ...` later. If the gateway is already
running when a provider is added or updated, its valid provider configuration
is applied automatically to new requests. Already admitted requests keep their
original configuration. An invalid update leaves the last valid configuration
active; check the gateway's runtime log for `config_reload` failures. A
bare upstream model ID routes when exactly one configured model scope matches;
otherwise choose the provider reference shown by `provider list` and use
`REF/MODEL_ID` (for example, `openrouter/stealth/union-alpha`). The gateway
strips the first `REF/` prefix before forwarding an explicitly qualified model
and lists selectable qualified IDs at `/v1/models`.

Configure the shared gateway credential and listener, then start the gateway:

```sh
tidemux gateway configure
tidemux doctor
tidemux serve
```

Keep `serve` running in this terminal. The gateway API key is local client
authentication; it is separate from the upstream provider keys. Gateway-wide
settings such as the listener and active-session limit are shared by all
provider routes. Providers can also have independent logical-session
caps; see [provider capacity](provider-cli.md#provider-logical-session-capacity).

## Launch a client

In another terminal, from your project directory, start a client through
TideMux:

```sh
# Anthropic API client
tidemux claude --model deepseek-flash

# OpenAI API clients — choose one
tidemux kilo --model deepseek-flash -- run 'Explain this project'
tidemux hermes --model deepseek-flash -- -q 'Explain this project'
```

`--model` is required by these TideMux launch commands. If you configure a
client directly instead, use a bare model ID when it maps to one provider or a
shared-model strategy is enabled, or use `REF/MODEL_ID` to select explicitly. Either
OpenAI or Anthropic client can use any selected provider; TideMux converts
between client and upstream protocols when necessary. A provider/model error
does not trigger a retry on another provider. Install each client first. If
its executable is not on `PATH`, pass
`--executable /absolute/path/to/client` before `--`. Arguments after `--` go
to the client.

When multiple providers serve the same model, enable a
[shared-model strategy](protocols.md) to launch with its bare ID:

```sh
tidemux routing set --shared-model-strategy random
tidemux claude --model glm-5.3
```

The gateway selects the provider. With `random`, a stable client session ID
keeps the session bound to that provider. The default strategy is `off`, which
requires `REF/MODEL_ID` when provider scopes overlap. Shared-model launchers do
not advertise one candidate's declared context or output limits.

Hermes uses a named custom provider with `chat_completions` transport. Selecting
its `openai-api` provider can select the Responses API, which TideMux does not
currently serve; the TideMux launcher selects Chat Completions explicitly.

For a non-default TideMux configuration, pass the same `--config PATH` to both
`serve` and the client command, for example:

```sh
tidemux serve --config "$HOME/.config/tidemux/work.json"
tidemux claude --config "$HOME/.config/tidemux/work.json" --model REF/MODEL_ID
```

## Credentials and client state

Upstream keys stay in macOS Keychain and are never passed to client processes.
TideMux reads only the local gateway key from Keychain, checks authenticated
gateway access through `/v1/models`, and passes that local key to the client.
The selected model need not appear in discovery when the provider does not
expose a recognizable catalog. If Keychain needs to
be unlocked, run the client command from Terminal. If the gateway key is
missing, create or rotate it with `tidemux gateway configure --rotate-key`.

Client state is isolated beside the selected gateway configuration under
`client-state/`; TideMux does not overwrite the original Claude, Kilo or Hermes
profiles. Hermes's generated `config.yaml` is TideMux-managed and regenerated
on launch. Do not edit it for persistent customization. Client conversation
history remains with the client; TideMux's audit ledger does not store message
bodies.

The gateway must already be running with the same configuration. Connection
errors distinguish an unreachable gateway, rejected local credentials and an
unavailable model-list endpoint. Interactive Claude Code requires a terminal;
its `-p` mode also supports noninteractive use. Both require a bidirectional
streaming HTTP connection.

## Recover after a response error

Some Anthropic-compatible providers, including GLM, return built-in-tool output
as a generic `tool_result` alongside text in an assistant message. Native
Anthropic routes preserve these blocks and their fields without checking tool
roles or ID associations, removing duplicates, or reindexing the stream. The
same content can be sent back in native conversation history. The provider and
client decide how to interpret it; no provider setting is needed. OpenAI
conversion preserves available text, while server-tool-specific blocks follow
the limits in [protocol support](protocols.md).

If a reply stops after some normal text, check the terminal running `tidemux
serve` for its `request_terminal` JSON event. HTTP 200 can still have
`outcome: "error"`; use `error_code` to identify the failure. If runtime logs
are collected in a file, search that file, for example:

```sh
rg 'invalid_upstream_stream|invalid_upstream_response|incomplete_upstream_stream' /path/to/tidemux-runtime.jsonl
```

An invalid JSON or event envelope, missing terminal marker, invalid usage,
upstream error or size limit can end a response safely. A generic assistant
`tool_result` alone is not a gateway error. A terminal SSE error includes
`request_id`, matching `X-TideMux-Request-ID` and the runtime log's `requestId`.
Use the timestamp, request ID and safe error code when reporting the problem;
the gateway log does not contain prompts or tool output. See
[runtime logging](runtime-logging.md) for log locations and fields.

Clients can hide the gateway code behind a generic mid-response error or a
message saying that no answer was produced. Confirm the gateway error before
changing token limits or editing saved history. TideMux does not repair client
history or replay a dispatched request after output begins. A client may issue
its own retries, each of which is a new request.

If you need a clean conversation after correcting the cause, choose your
configured model and provide the task again:

```sh
tidemux claude --model REF/MODEL_ID -- -p 'Start this task in a new conversation.'
tidemux kilo --model REF/MODEL_ID -- run 'Start this task in a new conversation.'
tidemux hermes --model REF/MODEL_ID -- -q 'Start this task in a new conversation.' -Q --oneshot
```

Omit the client's `--continue` or `--resume` options for a clean conversation.
To retain history rejected by a provider or client, back it up and use that
client's supported history tools. Native passthrough preserves content; it
cannot guarantee that every provider and client accepts the same extensions.

## Recover after capacity is full

`active_session_limit` / HTTP 429 means that the selected provider or the whole
gateway has reached its configured logical-session capacity. It is separate
from an upstream rate limit and from a response error. Check the safe
error's `scope`, provider reference and limit, or the gateway's runtime error
code when the client hides its retries.

Wait for active work to finish and for retained sessions to reach their idle
timeout, then retry with bounded exponential backoff and jitter. The gateway
cannot predict an exact release time. Changing to a new conversation does not
free a different occupied session and can require another slot. If automatic
client retries stop, repeat the command once capacity is available; history
repair is not needed for this error. An operator can inspect the configured
limits and current session counts before deciding whether to increase a limit.

## Experimental implementation

The existing `tidemux kilo-ide` command is experimental. VS Code extension
compatibility is not part of the current release scope.

For agent-led installation and setup across Claude Code, Codex, Hermes and dsh,
see the [agent installation guide](agent-install.md) and its
[copyable prompt](agent-install-prompt.md). Codex can perform TideMux setup;
Codex itself cannot currently route through TideMux because its custom
providers use the Responses API, which TideMux does not serve.
