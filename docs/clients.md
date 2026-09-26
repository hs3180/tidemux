# Client setup

This guide describes the current named-provider CLI. Install the client you
want to use separately; TideMux does not install Claude Code, Kilo CLI or
Hermes Agent. The tested 0.1.1 release matrix is documented in
[client compatibility](client-compatibility.md); its single-provider routing
is historical and does not describe the current provider model.

## Add the provider routes

Each provider has one upstream API protocol. Client protocol does not select a
provider; the request's `model` does. Add one or more provider endpoints:

```sh
tidemux provider add https://api.deepseek.com
tidemux provider add https://api.deepseek.com/anthropic/v1
tidemux provider list
```

Each command securely prompts for the upstream API key. TideMux infers the
provider protocol from its endpoint. All models are allowed by default; use
`tidemux provider models REF --only ...` to restrict a provider. Choose the
provider reference shown by `provider list`, and always request the model as
`REF/MODEL_ID` (for example, `openrouter/stealth/union-alpha`). The gateway
strips the first `REF/` prefix before forwarding the upstream model ID. Its
`/v1/models` endpoint lists selectable qualified IDs.

Configure the shared gateway credential and listener, then start the gateway:

```sh
tidemux gateway configure
tidemux doctor
tidemux serve
```

Keep `serve` running in this terminal. The gateway API key is local client
authentication; it is separate from the upstream provider keys. Gateway-wide
settings such as the listener and active-session limit are shared by all
provider routes.

## Launch a client

In another terminal, from your project directory, start a client through
TideMux:

```sh
# Anthropic API client
tidemux claude --model REF/deepseek-flash

# OpenAI API clients — choose one
tidemux kilo --model REF/deepseek-flash -- run 'Explain this project'
tidemux hermes --model REF/deepseek-flash -- -q 'Explain this project'
```

`--model` is required by these TideMux launch commands. If you configure a
client directly instead, set its model to the same `REF/MODEL_ID` form. Either
OpenAI or Anthropic client can use any selected provider; TideMux converts
between client and upstream protocols when necessary. A provider/model error
does not trigger a retry on another provider. Install each client first. If
its executable is not on `PATH`, pass
`--executable /absolute/path/to/client` before `--`. Arguments after `--` go
to the client.

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
unavailable model-list endpoint. Claude Code requires an
interactive terminal and a bidirectional streaming HTTP connection.

## Experimental implementation

The existing `tidemux kilo-ide` command is experimental. VS Code extension
compatibility is not part of the current release scope.
