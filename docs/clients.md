# Client connections (0.1.1)

Use `tidemux <client>` to start an installed client with TideMux settings.
The direct commands replace the earlier `connect` interface. VS Code extension compatibility is outside the 0.1.1 scope.
Each gateway process uses one upstream provider protocol and one profile. Start `serve` before
launching a client. The client command checks authenticated model discovery, reads only the
local gateway credential from Keychain, and passes it in the child environment.
The upstream API key stays in the gateway.

## OpenAI profile: Kilo CLI and Hermes

```sh
./tidemux configure --preset deepseek-flash
./tidemux serve
```

In another terminal, from your working directory:

```sh
./tidemux kilo -- run 'Explain this project'
./tidemux hermes -- -q 'Explain this project'
```

Install the respective client first. If it is not on PATH, put
`--executable /absolute/path/to/client` before `--`. Use an absolute path to
`tidemux` when working outside its build directory. Arguments after `--` go to the
client, including its own permission and session controls.

Hermes uses a named custom provider with `chat_completions` transport. Selecting
its `openai-api` provider can select the Responses API, which TideMux does not
currently serve. The launcher makes this transport choice explicitly.

## Anthropic provider: OpenAI clients and Claude Code

Create a provider profile with the Anthropic API root. The configure prompt
collects the upstream key without echoing it:

```sh
./tidemux configure --preset deepseek-flash --protocol anthropic \
  --listen 127.0.0.1:8788 --config "$HOME/.config/tidemux/anthropic.json"
./tidemux serve --config "$HOME/.config/tidemux/anthropic.json"
```

Kilo and Hermes can use this profile through the same OpenAI-compatible
`/v1/chat/completions` endpoint; TideMux translates requests and responses to
Anthropic Messages upstream.

In another terminal:

```sh
./tidemux claude
```

When using a non-default profile, add `--config`:

```sh
./tidemux claude --config "$HOME/.config/tidemux/anthropic.json"
```

The Claude launcher supplies `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`,
`ANTHROPIC_MODEL`, and `CLAUDE_CODE_SIMPLE` in the child environment. It does
not pass the model as a client-specific flag. The gateway remains a separate
process started with `tidemux serve`; this command only performs the local
credential/model preflight and launches Claude Code.

## Profiles and credentials

Client state lives beside the gateway configuration, in
`client-state/<client>-<config-path-hash>/`. Claude and Hermes retain sessions
there; Kilo receives separate XDG directories. Original client home profiles are
not overwritten. Existing `KILO_CONFIG_CONTENT` options are preserved, with the
TideMux provider and model selected for the child process.

Hermes's dedicated `config.yaml` is TideMux-managed and regenerated on each
launch; do not edit that file for persistent customization. An existing file
without the ownership marker is refused. The file contains an environment
variable reference, not a token. Local credentials are not added to command-line
arguments. Client programs may keep their own conversation history in their
profiles; TideMux's audit ledger does not store message bodies.

The gateway must be running with the same profile. Connection errors distinguish
unreachable gateway, rejected local credentials, unavailable discovery, and a
model mismatch. If credential lookup fails, an interactive Terminal session checks the Keychain
and lets the macOS `security` utility request an unlock password when needed.
TideMux does not read that password. Lookup is retried after the check; a missing
credential still requires `configure`. Without a controlling terminal, the error
explains how to retry interactively.

Client launch is an environment handoff rather than a Unix pipe. Claude Code
needs a bidirectional streaming HTTP endpoint and an interactive TTY; the same
environment-based boundary can be reused by future clients without coupling
them to a shell pipeline.

## Provider compatibility

The tested DeepSeek Anthropic-compatible endpoint accepts Claude's system-role
messages within the message list. TideMux preserves those messages and their
order, along with top-level system content. This is a provider compatibility
extension; standard Anthropic endpoints may reject it. TideMux does not silently
change system messages into user messages or relocate them. Parameter acceptance
alone does not establish that an upstream implements every thinking or context
management behavior; those capabilities depend on the chosen provider/model.

## Experimental implementation

The existing `tidemux kilo-ide` command is retained as experimental code. VS Code
extension compatibility is not part of 0.1.1 acceptance or a pending release gate.
