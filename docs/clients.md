# Client setup

This guide describes the current named-provider CLI. Install the client you
want to use separately; TideMux does not install Claude Code, Kilo CLI or
Hermes Agent. The tested 0.1.1 release matrix is documented in
[client compatibility](client-compatibility.md); its single-provider routing
is historical and does not describe the current provider model.

## Add the provider routes

Each provider has one API protocol, and each client is routed to the default
provider for its protocol. To use Claude Code and OpenAI-compatible clients
with DeepSeek, add both endpoint forms:

```sh
tidemux provider add https://api.deepseek.com --model deepseek-flash
tidemux provider add https://api.deepseek.com/anthropic/v1 --model deepseek-flash
tidemux provider list
```

Each command securely prompts for the upstream API key. TideMux infers the
provider protocol from its endpoint; all models are allowed by default, while
`--model` selects the fallback when a client omits a model. The first provider
for each protocol becomes its default. To use only one client protocol, add
only its provider. To change a route later, use
`tidemux provider default PROTOCOL REF` with a reference from `provider list`.

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
tidemux claude

# OpenAI API clients — choose one
tidemux kilo -- run 'Explain this project'
tidemux hermes -- -q 'Explain this project'
```

Claude Code uses the Anthropic provider default. Kilo and Hermes use the
OpenAI provider default. TideMux does not translate between provider protocols
in the current routing model; add a provider for each protocol you need. Install
each client first. If its executable is not on `PATH`, pass
`--executable /absolute/path/to/client` before `--`. Arguments after `--` go
to the client.

Hermes uses a named custom provider with `chat_completions` transport. Selecting
its `openai-api` provider can select the Responses API, which TideMux does not
currently serve; the TideMux launcher selects Chat Completions explicitly.

For a non-default TideMux configuration, pass the same `--config PATH` to both
`serve` and the client command, for example:

```sh
tidemux serve --config "$HOME/.config/tidemux/work.json"
tidemux claude --config "$HOME/.config/tidemux/work.json"
```

## Credentials and client state

Upstream keys stay in macOS Keychain and are never passed to client processes.
TideMux reads only the local gateway key from Keychain, checks authenticated
model discovery, and passes that local key to the client. If Keychain needs to
be unlocked, run the client command from Terminal. If the gateway key is
missing, create or rotate it with `tidemux gateway configure --rotate-key`.

Client state is isolated beside the selected gateway configuration under
`client-state/`; TideMux does not overwrite the original Claude, Kilo or Hermes
profiles. Hermes's generated `config.yaml` is TideMux-managed and regenerated
on launch. Do not edit it for persistent customization. Client conversation
history remains with the client; TideMux's audit ledger does not store message
bodies.

The gateway must already be running with the same configuration. Connection
errors distinguish an unreachable gateway, rejected local credentials,
unavailable model discovery and a model mismatch. Claude Code requires an
interactive terminal and a bidirectional streaming HTTP connection.

## Experimental implementation

The existing `tidemux kilo-ide` command is experimental. VS Code extension
compatibility is not part of the current release scope.
