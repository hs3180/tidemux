# TideMux

**Connect Claude Code, Kilo CLI and Hermes Agent to your own LLM API.**

TideMux is a local macOS gateway for **OpenAI-compatible and Anthropic-compatible APIs**.

- **Streaming and tools** — forward conversations and tool calls to your chosen provider.
- **Simple client setup** — run `tidemux claude`, `tidemux kilo` or `tidemux hermes` with secure local credentials.
- **Concurrency control** — limit active requests and queue the rest.
- **Local usage ledger** — track outcomes, tokens and cost estimates without storing message bodies.
- **Automatic reconciliation** — match locally supplied statement CSVs while the gateway runs; query statistics or download billing details with `tidemux billing`.
- **Daily reports** — generate private HTML reports and schedule macOS notifications from the local usage ledger.

## Install

Requires **macOS 15+ on Apple Silicon** and [Homebrew](https://brew.sh).

```sh
brew install hs3180/tap/tidemux
```

If Homebrew asks you to trust the formula, run
`brew trust --formula hs3180/tap/tidemux`, then retry the install command.

Homebrew currently installs the published 0.1.1 binary, which predates the
0.2.0 commands below. The quick start assumes a current source build; see
[build from source](docs/install.md#build-from-source).

[Build from source or install without Homebrew →](docs/install.md)

Upgrade with `brew upgrade tidemux`; uninstall with `brew uninstall tidemux`.

## Quick start

This walkthrough describes the **target 0.2.0 CLI**. The published 0.1.1
binary predates the provider commands below; build the current source version
using [the installation guide](docs/install.md#build-from-source) first.

Install your preferred client CLI. The example below uses **DeepSeek
`deepseek-flash`**; have your DeepSeek API key ready.

### 1. Add providers

The target 0.2.0 CLI uses one provider per upstream API protocol. To use both
client protocols with DeepSeek, add one endpoint for each; TideMux infers each
provider's protocol and makes the first provider for that protocol its default:

```sh
tidemux provider add https://api.deepseek.com --model deepseek-flash
tidemux provider add https://api.deepseek.com/anthropic/v1 --model deepseek-flash
```

Each command prompts for its API key without echo. Provider references and
readable labels are generated from the endpoint; no provider name is required.
All models are allowed by default. `--model` selects the fallback for clients
that omit a model; it does not restrict the allowed model list. Use
`tidemux provider models REF --only MODEL[,MODEL...]` only when you want to
restrict that list. To configure just one protocol, add only its endpoint.

### 2. Start the gateway

```sh
tidemux doctor
tidemux serve
```

`doctor` checks local configuration. Keep the gateway terminal open; stop it with **Ctrl-C**.
Next time, just run `tidemux serve` to reuse your configuration.

### 3. Launch your client

In another terminal, from your project directory, run the client you want. With
both protocol routes configured, TideMux sends each client to the corresponding
default provider:

```sh
# Anthropic API client
tidemux claude

# OpenAI API clients — choose one
tidemux kilo -- run 'Explain this project'
tidemux hermes -- -q 'Explain this project'
```

TideMux supplies the gateway URL, model and local credential to the client
without overwriting its existing settings.

Run `tidemux billing` for a readable summary of the current calendar month in
your computer's local timezone. It reads your existing local ledger from the
default TideMux configuration. Add `--details` to inspect request records or
`--json` for structured output. Cost estimates require a built-in or configured
[model price](docs/provider-cli.md#provider-pricing); unknown amounts stay unknown.
Use the [DeepSeek USD rates](docs/deepseek-pricing.md) for this example.
Reconciliation runs automatically with the gateway. Place normalized supplier
CSVs in the `statements` directory beside the ledger. Use
`tidemux billing --download billing.csv` to save that month's stored billing
details, or select a period with `--from` and `--to`. The summary shows its exact
RFC3339 date bounds. Use `tidemux doctor --diagnostics` to inspect local request
rejections. See [accounting](docs/accounting.md#billing-statistics-and-download)
for periods, synchronization status, statement coverage and the CSV format.
For current client launch commands and profile behavior, see the
[client setup guide](docs/clients.md). The tested 0.1.1 release matrix is
documented separately in [client compatibility](docs/client-compatibility.md).

## Compatibility

The 0.2.0 development line exposes OpenAI Chat Completions and Anthropic
Messages client APIs simultaneously. Each provider has one upstream protocol;
requests route only to that protocol's default provider. To serve both client
APIs, configure one provider for each protocol—even when both endpoints belong
to the same service. Requests are validated and normalized within their selected
protocol; an OpenAI client request is not translated to an Anthropic provider,
or vice versa. Gateway auth, session limits and ledger accounting remain shared.
The three CLI workflows were verified for the 0.1.1 release; that evidence does
not certify the 0.2.0 named-provider routing. Other compatible providers can be
configured, though they have not all been tested.

Responses API, images/audio and IDE extensions are outside this release's
scope. Provider-specific features without an equivalent in the selected
protocol are not forwarded silently. See the
[0.1.1 tested-client matrix](docs/client-compatibility.md) for release evidence
and [protocol support](docs/protocols.md) for the 0.2.0 routing contract.

## CLI design principles

The CLI is resource-oriented: a top-level noun identifies what is being
managed, and a subcommand states the action. Provider setup and lifecycle belong
under `tidemux provider`; the old top-level `tidemux configure` command is not
retained as a compatibility alias. Gateway-wide settings belong under
`tidemux gateway`. The commands below describe the 0.2.0 development CLI;
published older binaries may expose a different command set.

| Command | Semantics |
| --- | --- |
| `tidemux provider add [ENDPOINT] [--name LABEL] [--protocol PROTOCOL] [--model ID]` | Add exactly one provider without replacing others. With no endpoint, start guided setup and offer the initial daily-notification prompt; with an endpoint, infer protocol and discover models, prompting only for missing choices. |
| `tidemux provider list [--json]` | List provider references, endpoint, protocol, default model, key count, model scope, budget status and protocol defaults. Never reveal credentials. |
| `tidemux provider show REF` | Show one provider's effective settings, including its budget, but not its API key. |
| `tidemux provider update REF [--endpoint URL] [--protocol PROTOCOL] [--model ID] [--anthropic-version DATE] [--rotate-key]` | Change only the supplied fields. Updating a key uses hidden input; omitted fields and other providers remain unchanged. |
| `tidemux provider key add REF` | Add another hidden-input API key to the selected provider profile. All keys share that profile's endpoint, protocol and model scope. |
| `tidemux provider key list REF` | List redacted key slots only; never reveal credentials or Keychain account details. |
| `tidemux provider key remove REF INDEX [--yes]` | Remove one key from the profile. A provider must retain at least one key; the Keychain item is deleted only when no remaining provider references it. |
| `tidemux provider remove REF [--default REF] [--yes]` | Remove only that provider. Confirm interactively; without a terminal require `--yes`. If it is a protocol default and alternatives remain, prompt for a replacement or require `--default`; if none remain, clear the route. Delete a Keychain item only when no remaining provider references it. |
| `tidemux provider default PROTOCOL REF` | Select the provider used by default for OpenAI or Anthropic clients. The provider must serve that protocol. Adding another provider never silently changes an existing default. |
| `tidemux provider models REF [--only MODELS] [--all]` | With no scope option, query and display the provider's models when available. `--only` restricts allowed IDs; `--all` removes that restriction. The options are mutually exclusive. This is not a separate top-level `models` command. |
| `tidemux provider pricing list REF` | Show that provider's per-model rates and their source/version. |
| `tidemux provider pricing set REF MODEL --input-cache-hit RATE --input-cache-miss RATE --output RATE [other options]` | Set or replace all required per-million-token rates for one model; incomplete rate sets are rejected. |
| `tidemux provider pricing remove REF MODEL` | Remove that model's explicit rate. This does not change the provider or model scope. |
| `tidemux provider budget [REF] [options]` | Configure rolling spending limits for one provider. If exactly one provider exists, `REF` may be omitted. |
| `tidemux gateway configure [options]` | Set process-wide listener (`loopback` by default or `0.0.0.0`), gateway credential, request-concurrency and active-session settings. `0.0.0.0` requires a gateway API key. It does not add or modify upstream providers. |

Provider identity does not depend on a user-chosen name. TideMux creates and
prints a stable reference derived from the endpoint; `--name` optionally
overrides that reference. Multiple profiles may use the same endpoint (for
example, separate accounts); each `add` creates a distinct reference and never
silently updates or replaces an existing provider. Use `tidemux provider key
add REF` to attach additional credentials to one profile; they share that
profile's endpoint and model scope. The endpoint determines the protocol by
default, with `--protocol openai|anthropic` available only to override
inference. API keys are always collected through hidden input, never
command-line arguments or configuration JSON; they are stored in macOS
Keychain. Without a terminal for that prompt, setup fails before changing
configuration. Model discovery uses
the authenticated model-list endpoint only; setup never sends a completion
request to select a model.

On a first guided setup, TideMux also asks whether to schedule a daily local-time
notification for usage reports. Leave it blank to skip. Set or change this later
with `tidemux report schedule --time HH:MM`; disable it with
`tidemux report schedule --disable`.

All model IDs are allowed by default. A provider's default model is only the
fallback for clients that omit a model; it is separate from the optional model
allowlist. Discovery selects a sole available model automatically. If there are
several and no default is supplied, interactive setup asks the user to choose;
if discovery is unavailable, setup asks for the fallback model. The first
provider for a protocol becomes its default; later providers do not replace it
implicitly. Explicit `provider update`, `provider remove`, and default-route
operations affect only the selected provider or route. Before changing a
default provider's protocol, select a replacement route if multiple providers
remain on its old protocol. Configuration writes are validated and atomic;
credentials created for a provider addition are rolled back if its config
write fails.

Provider-specific values (endpoint, credentials, protocol, model scope, prices
and spending budget) stay with the provider. Budget limits and accrued usage are
isolated per provider, even when providers use the same currency. Gateway-wide
values such as listen mode and active-session limits apply to the whole gateway.
An automatically recognized built-in rate may be used when no explicit rate
exists; otherwise cost remains unknown rather than guessed. Report scheduling
remains under `tidemux report`.

Rates are scoped by provider and model. `provider pricing set` requires all
three per-million-token rate flags: `--input-cache-hit`, `--input-cache-miss`,
and `--output`; `--currency` defaults to USD, while `--source` and `--version`
identify the rate reference. Removing a custom rate reveals a matching built-in
rate if one exists; otherwise cost remains unknown. An enabled provider budget
requires matching prices for the default model and every requested model;
without a budget, unpriced models remain usable.

Typical resource operations look like this; `<ref>` is the stable reference
shown by `provider list`, not a name the user must invent:

```sh
tidemux provider add https://api.example.com/v1
tidemux provider list
tidemux provider models REF_FROM_LIST --only model-a,model-b
tidemux provider default openai REF_FROM_LIST
tidemux provider budget REF_FROM_LIST --budget-5h 5 --budget-weekly 80
tidemux gateway configure --listen loopback
```

## Documentation

[Provider and gateway CLI](docs/provider-cli.md) ·
[Client setup](docs/clients.md) ·
[Accounting](docs/accounting.md) ·
[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) ·
[Privacy](PRIVACY.md) · [Security](SECURITY.md)

[Apache-2.0](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md) · [Brand reservation](NOTICE)
