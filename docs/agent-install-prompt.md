# Copyable TideMux setup prompt for coding agents

```text
Install TideMux and set up only the provider/model route and coding client I
request. Read the official TideMux agent-install guide first:
https://github.com/hs3180/tidemux/blob/main/docs/agent-install.md

Check macOS 15+ and Apple Silicon, inspect the existing TideMux binary and
whether Homebrew owns it, then install or upgrade the latest stable GitHub
release. Resolve and validate the exact vMAJOR.MINOR.PATCH release tag. Use the
matching version-tagged installer and pin TIDEMUX_VERSION; never install from
main or pipe an unreviewed network response into a shell. Preserve the existing
install directory, config, provider profiles, Keychain, budgets and audit
history. Make reruns idempotent and modify only the route I requested.

Do not guess a missing provider, endpoint, model or client scope. Ask only for
details needed to make the requested route unambiguous. Never ask me to paste an
upstream API key or TideMux gateway key into this conversation, command-line
arguments, a file or logs. Use TideMux's hidden local terminal prompt for the
provider key. If you cannot safely hand off that prompt, ask me to run the
specific TideMux command in my own Terminal. Keep any newly generated gateway
key in my terminal and have me enter it directly into the client credential
UI.

Use bounded /models protocol detection; if it cannot identify exactly one
supported protocol during a new provider add, ask me to choose OpenAI Chat
Completions or Anthropic Messages at TideMux's local terminal prompt. For an
existing saved auto profile, update only that profile with its explicit
protocol. Keep the gateway bound to loopback. Do not replace an
existing profile, rotate keys, expose the gateway to the network or change
provider/model scope unless I explicitly requested that action.

Configure Claude Code with `tidemux claude --model MODEL_ID`, Hermes with
`tidemux hermes --model MODEL_ID -- ...`, or dsh as a custom provider at
`http://127.0.0.1:4000/v1` with model `MODEL_ID` and the local gateway key.
Use `REF/MODEL_ID` for explicit provider selection or when model scopes overlap.
If dsh saves the custom provider but omits its model from the picker, follow the
full install guide's verified `agent-default-model` profile workaround; keep
the gateway key in dsh's credential UI and preserve the rest of the profile.
Codex may run this setup, but direct Codex model routing is unsupported because
TideMux does not implement the Responses API. See the Codex custom-provider
`wire_api` reference: https://developers.openai.com/codex/config-reference/.

Verify the pinned binary version, run `tidemux doctor`, list the selected
provider/model scope and run `tidemux gateway check` against the running local
gateway. Summarize exactly what changed and the verification results. Do not
claim a real upstream request was tested unless it was actually run with my
authorization.
```
