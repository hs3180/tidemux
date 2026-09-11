# Client connections (0.1.0 development build)

Build the current source before using `connect`; the previously installed 0.1.0
binary does not contain this command. This workflow is under acceptance testing.
Each gateway process uses one protocol and one profile. Start `serve` before
launching a client. `connect` checks authenticated model discovery, reads only the
local gateway credential from Keychain, and passes it in the child environment.
The upstream API key stays in the gateway.

## OpenAI profile: Kilo CLI and Hermes

```sh
./tidemux configure --preset deepseek
./tidemux serve
```

In another terminal, from your working directory:

```sh
./tidemux connect kilo -- run 'Explain this project'
./tidemux connect hermes -- -q 'Explain this project'
```

Install the respective client first. If it is not on PATH, put
`--executable /absolute/path/to/client` before `--`. Use an absolute path to
`tidemux` when working outside its build directory. Arguments after `--` go to the
client, including its own permission and session controls.

Hermes uses a named custom provider with `chat_completions` transport. Selecting
its `openai-api` provider can select the Responses API, which TideMux does not
currently serve. The launcher makes this transport choice explicitly.

## Anthropic profile: Claude Code

Create a separate configuration with a different port. The configure prompt
collects the upstream key without echoing it:

```sh
./tidemux configure --preset deepseek --protocol anthropic \
  --listen 127.0.0.1:8788 --config "$HOME/.config/tidemux/anthropic.json"
./tidemux serve --config "$HOME/.config/tidemux/anthropic.json"
```

In another terminal:

```sh
./tidemux connect claude --config "$HOME/.config/tidemux/anthropic.json"
```

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

## Kilo VS Code extension

Install VS Code and the official `kilocode.kilo-code` extension first. With the
OpenAI gateway running, launch the project using the development binary:

```sh
./tidemux connect kilo-ide -- /absolute/path/to/project
```

If `code` is not on PATH:

```sh
./tidemux connect kilo-ide \
  --executable "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code" \
  -- /absolute/path/to/project
```

Use `--extensions-dir /path/to/installed/extensions` before `--` only for a
nondefault extension installation. After `--`, supply one existing directory;
omitting it opens the current directory. Open **Kilo Code: Open in Tab** in VS
Code and verify the configured model. Complete Kilo's first-run workflow and
choose your preferred tool approval behavior. A Kilo cloud login is not needed
for this custom provider. Trust only the project you intend the extension to run.

The launcher checks authenticated model discovery and passes only the local
gateway credential in the child environment. It uses the same trusted Kilo
configuration and permission preservation as the CLI. IDE state is stored in
`~/.tidemux/ide/<config-path-hash>/`, including its own XDG directories and VS Code
user data. This shorter location avoids long macOS IPC socket paths when the
gateway configuration is deeply nested. It does not overwrite your ordinary VS
Code profile; VS Code may still share its application-wide trust metadata.

The terminal waits for the dedicated window to close. Before reconnecting with
changed credentials or model settings, quit that dedicated VS Code instance:
an existing process cannot acquire newly supplied environment variables. A
concurrent launcher or still-running dedicated instance is rejected with a
specific explanation. A normal window reload keeps the current connection and
conversation. Do not put keys in workspace files or shell arguments.

The tested DeepSeek Anthropic-compatible endpoint accepts Claude's system-role
messages within the message list. TideMux preserves those messages and their
order, along with top-level system content. This is a provider compatibility
extension; standard Anthropic endpoints may reject it. TideMux does not silently
change system messages into user messages or relocate them. Parameter acceptance
alone does not establish that an upstream implements every thinking or context
management behavior; those capabilities depend on the chosen provider/model.
