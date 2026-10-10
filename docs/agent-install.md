# Agent-led TideMux installation and setup

This guide is for a coding agent helping its user install TideMux and configure
one requested provider/model route. The agent may run in Claude Code, OpenAI
Codex, Hermes Agent or dsh. TideMux runs on macOS 15 or newer with
Apple Silicon.

## Installation

First inspect the platform, executable and package-manager ownership:

```sh
uname -s
uname -m
command -v tidemux || true
tidemux version 2>/dev/null || true
brew list --versions tidemux 2>/dev/null || true
```

Use Homebrew for a Homebrew-managed installation (`brew update` followed by
`brew upgrade tidemux`), and use the version-pinned installer for a direct
installation. For a new installation, use the Homebrew formula when Homebrew is
available; otherwise install the latest stable GitHub release. Resolve the
latest release tag from the GitHub Releases API and validate that it is a
stable `vMAJOR.MINOR.PATCH` tag. After a Homebrew install or upgrade, compare
`tidemux version` with that tag; if the tap is behind, stop and use the
version-pinned installer only when the existing installation is not
Homebrew-managed. Do not install from `main`, a branch archive or an unpinned
script URL.

For a direct install, download `scripts/install.sh` from the matching version
tag to a temporary file, inspect it, then run it with `TIDEMUX_VERSION` set to
that exact version. Do not pipe a network response directly into a shell. The
installer verifies the release archive checksum and binary version. It replaces
only the `tidemux` executable in its install directory; it does not edit the
TideMux config or Keychain. Respect the existing install directory and PATH.
Do not overwrite a Homebrew-managed executable using the standalone installer.

## Configure only the requested route

Ask for the provider and model only when the user has not already specified
them. Keep the provider endpoint, model ID and client scope explicit. Do not
guess a provider or model. Run `tidemux provider add ENDPOINT --model MODEL_ID`
for a restricted model scope, or omit `--model` when the user wants every model
from that endpoint. Use the upstream model ID in client requests when exactly
one configured provider scope matches it; use `REF/MODEL_ID` when scopes
overlap or the user asks for explicit provider selection. Client protocol does
not choose the provider. TideMux will prompt for the upstream key using hidden
terminal input. The key goes directly into the local Keychain; never ask the
user to paste it into the agent conversation, shell arguments, a file or a
configuration JSON. If the agent cannot provide a secure interactive terminal,
ask the user to run that one command in their own Terminal and resume after it
returns.

Provider protocol detection uses bounded authenticated `/models` schema probes.
When adding a provider, TideMux asks in the local terminal for the protocol if
detection is ambiguous or unsupported; ask the user to choose OpenAI Chat
Completions or Anthropic Messages there. If an already saved `auto` profile
fails detection, show its reference and update it with
`tidemux provider update REF --protocol openai|anthropic`. Do not edit the
configuration file by hand to bypass this check.

Configure the local gateway with `tidemux gateway configure`, then run
`tidemux doctor` and `tidemux provider list`. Start it with `tidemux serve` and
verify the live endpoint with `tidemux gateway check`. Gateway authentication
is local client authentication and is distinct from the upstream provider key.
If a gateway key must be created or rotated, keep its one-time output in the
user's terminal. Never include it in agent output, logs, a project file or an
external client transcript.

## Configure the requested client

- **Claude Code:** launch with `tidemux claude --model MODEL_ID`, or use
  `REF/MODEL_ID` when more than one provider can serve the same ID. TideMux
  supplies a local Anthropic Messages endpoint and credentials through its
  isolated client state.
- **Hermes Agent:** launch with
  `tidemux hermes --model MODEL_ID -- -q '…'`; qualify it with `REF/` only to
  disambiguate provider scopes. TideMux selects Hermes' `chat_completions`
  transport and isolated local client state.
- **dsh:** use Settings → Models → Add a custom provider (see the
  [official model configuration guide](https://deepseek-harness.github.io/deepseek-harness/en/guide/providers)). Select OpenAI Chat Completions or Anthropic Messages and set base URL
  `http://127.0.0.1:4000/v1` (or the configured loopback port), the upstream
  model ID (or `REF/MODEL_ID` if scopes overlap), and the local TideMux gateway
  key. The user should enter that key directly into dsh's credential UI from
  their local terminal; the agent must not read or relay it.

- **Codex:** Codex can perform TideMux installation and setup as an agent.
  Direct Codex-to-TideMux model routing is not supported: the
  [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
  currently defines `responses` as the only custom-provider `wire_api`, while
  TideMux serves OpenAI Chat Completions and Anthropic Messages.

Do not enable a non-loopback listener, replace an existing profile, rotate a
key, or change an existing provider/model scope unless the user explicitly
requested that exact action. Preserve other provider profiles, config values,
Keychain items, budgets and audit history. Make repeated setup runs idempotent:
check the existing binary and selected profile first, and update only the
requested fields.

## Verify and report

After installation, verify `tidemux version` matches the selected stable
release. After configuration, run `tidemux doctor`, `tidemux provider list`,
and `tidemux gateway check` while the gateway is running. Report the binary
version, provider reference, selected model scope, client command and check
results. Do not claim an upstream completion test unless one was actually run
with the user's authorization and provider credentials.

For a short reusable agent instruction, see the
[copyable install prompt](agent-install-prompt.md).
