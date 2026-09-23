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

[Build from source or install without Homebrew →](docs/install.md)

Upgrade with `brew upgrade tidemux`; uninstall with `brew uninstall tidemux`.

## Quick start

Install your preferred client CLI. The example below uses **DeepSeek
`deepseek-flash`**; have your DeepSeek API key ready.

### 1. Configure DeepSeek

In the current build, configure the single-provider compatibility mode for
Kilo CLI or Hermes Agent:

```sh
tidemux configure --base-url https://api.deepseek.com --model deepseek-flash
```

TideMux detects the upstream protocol from the API root and exposes both client
APIs, translating between them when needed. To use DeepSeek's
Anthropic-compatible API as that endpoint, set its root and model explicitly:

```sh
tidemux configure --base-url https://api.deepseek.com/anthropic/v1 \
  --model deepseek-flash
```

For another provider, run `tidemux configure` without flags for guided setup.
Enter the provider API Base URL and API key; TideMux derives a name from the
URL, detects the upstream protocol and discovers models when possible. It allows
all model IDs by default: press Enter at the optional model-scope prompt, or
enter a subset to restrict which IDs TideMux forwards. The upstream still
determines which models are actually available. For command-line setup in the
current build, use
`--provider NAME,BASE_URL,MODEL` (add a protocol to force it) and, only when
needed, `--provider-models NAME,MODEL[,MODEL...]` (include the fallback model).
The key is still entered at a hidden prompt. In the current build, named
providers are protocol-specific; configure one for each client protocol and
select the desired defaults. This setup syntax is being replaced by the
resource-oriented provider commands described below. See
[configuration](docs/configure.md) for options available in the current build.

### 2. Start the gateway

```sh
tidemux doctor
tidemux serve
```

`doctor` checks local configuration. Keep the gateway terminal open; stop it with **Ctrl-C**.
Next time, just run `tidemux serve` to reuse your configuration.

### 3. Launch your client

In another terminal, from your project directory, run the client you want. In
the single-provider compatibility mode, both client APIs are exposed and
TideMux detects the upstream protocol automatically:

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
`--json` for structured output. Cost estimates require
[configured model prices](docs/configure.md); unknown amounts stay unknown.
Use the [DeepSeek USD rates](docs/deepseek-pricing.md) for this example.
Reconciliation runs automatically with the gateway. Place normalized supplier
CSVs in the `statements` directory beside the ledger. Use
`tidemux billing --download billing.csv` to save that month's stored billing
details, or select a period with `--from` and `--to`. The summary shows its exact
RFC3339 date bounds. Use `tidemux doctor --diagnostics` to inspect local request
rejections. See [accounting](docs/accounting.md#billing-statistics-and-download)
for periods, synchronization status, statement coverage and the CSV format.
For client invocation options and setup details, see [client setup](docs/clients.md).

## Compatibility

The 0.2.0 development line exposes OpenAI Chat Completions and Anthropic
Messages client APIs simultaneously. In single-provider compatibility mode
(selected with `--base-url`/`--model`), either client protocol can use the
upstream through translation. Guided setup creates a named provider
with one upstream protocol; it serves the matching client protocol only. To
route both client protocols to separate endpoints, configure one named provider
for each protocol and choose their defaults. Gateway auth, session limits and
ledger accounting remain shared. The three CLIs have passed DeepSeek file
read/edit/test and continued-conversation workflows. Other compatible providers
can be configured, though they have not all been tested.

Responses API, images/audio and IDE extensions are outside this release's
scope. Supported bidirectional Chat Completions/Messages conversion is covered
in the protocol boundary; provider-specific features without a client
equivalent are not forwarded silently. See [tested clients](docs/client-compatibility.md)
and [protocol support](docs/protocols.md).

## CLI design principles

The target CLI is resource-oriented: a top-level noun identifies what is being
managed, and a subcommand states the action. Provider setup and lifecycle belong
under `tidemux provider`; the old top-level `tidemux configure` provider flow is
not a second mode or compatibility alias to preserve. Gateway-wide settings
belong under `tidemux gateway`. The commands below define the intended
contract; the current build still uses `tidemux configure`, so see
[configuration](docs/configure.md) for syntax that is available today.

| Command | Semantics |
| --- | --- |
| `tidemux provider add [ENDPOINT] [--name LABEL] [--protocol PROTOCOL] [--model ID]` | Add exactly one provider without replacing others. With no endpoint, guide the user through setup; with an endpoint, infer protocol and discover models, prompting only for missing choices. |
| `tidemux provider list [--json]` | List provider references, endpoint, protocol, default model, model scope and whether each is a protocol default. Never reveal credentials. |
| `tidemux provider show REF` | Show one provider's effective settings, but not its API key. |
| `tidemux provider update REF [options]` | Change only the supplied fields. Updating a key uses hidden input; omitted fields and other providers remain unchanged. |
| `tidemux provider remove REF [--default REF] [--yes]` | Remove only that provider. Confirm interactively; without a terminal require `--yes`. If it is a protocol default and alternatives remain, prompt for a replacement or require `--default`; if none remain, clear the route. Delete a Keychain item only when no remaining provider references it. |
| `tidemux provider default PROTOCOL REF` | Select the provider used by default for OpenAI or Anthropic clients. The provider must serve that protocol. Adding another provider never silently changes an existing default. |
| `tidemux provider models REF [--only MODELS] [--all]` | With no scope option, query and display the provider's models when available. `--only` restricts allowed IDs; `--all` removes that restriction. The options are mutually exclusive. This is not a separate top-level `models` command. |
| `tidemux provider pricing list REF` | Show that provider's per-model rates and their source/version. |
| `tidemux provider pricing set REF MODEL --input-cache-hit RATE --input-cache-miss RATE --output RATE [other options]` | Set or replace all required per-million-token rates for one model; incomplete rate sets are rejected. |
| `tidemux provider pricing remove REF MODEL` | Remove that model's explicit rate. This does not change the provider or model scope. |
| `tidemux gateway configure [options]` | Set process-wide listener (`loopback` by default or `0.0.0.0`), gateway credential, request-concurrency and active-session settings. `0.0.0.0` requires a gateway API key. It does not add or modify upstream providers. |

Provider identity does not depend on a user-chosen name. TideMux creates and
prints a stable reference and derives a readable label from the endpoint;
`--name` may set an optional display label. Multiple profiles may use the same
endpoint (for example, separate accounts); each `add` creates a distinct
reference and never silently updates or replaces an existing provider. The
endpoint determines the protocol by default, with
`--protocol openai|anthropic` available only to override inference. API keys
are always collected through hidden input, never command-line arguments or
configuration JSON; they are stored in macOS Keychain. Without a terminal for
that prompt, setup fails before changing configuration. Model discovery uses
the authenticated model-list endpoint only; setup never sends a completion
request to select a model.

All model IDs are allowed by default. A provider's default model is only the
fallback for clients that omit a model; it is separate from the optional model
allowlist. Discovery selects a sole available model automatically. If there are
several and no default is supplied, interactive setup asks the user to choose;
if discovery is unavailable, setup asks for the fallback model. The first
provider for a protocol becomes its default; later providers do not replace it
implicitly. Explicit `provider update`, `provider remove`, and default-route
operations affect only the selected provider or route. An update that would
make a default route invalid must also select a valid replacement. Every
mutation is validated and written atomically; a failure leaves the previous
configuration and credentials intact.

Provider-specific values (endpoint, credentials, protocol, model scope and
prices) stay with the provider. Gateway-wide values such as listen mode and
active-session limits are configured separately and apply to the whole gateway.
An automatically recognized built-in rate may be used when no explicit rate
exists; otherwise cost remains unknown rather than guessed. Budget and report
settings remain under their existing `budget` and `report` command groups.

Rates are scoped by provider and model. `provider pricing set` requires all
three per-million-token rate flags: `--input-cache-hit`, `--input-cache-miss`,
and `--output`; `--currency` defaults to USD, while `--source` and `--version`
identify the rate reference. Removing a custom rate reveals a matching built-in
rate if one exists; otherwise cost remains unknown. Unpriced models remain
usable, but an enabled budget still requires matching prices.

Typical resource operations look like this; `<ref>` is the stable reference
shown by `provider list`, not a name the user must invent:

```sh
tidemux provider add https://api.example.com/v1
tidemux provider list
tidemux provider models REF_FROM_LIST --only model-a,model-b
tidemux provider default openai REF_FROM_LIST
tidemux gateway configure --listen loopback
```

## Documentation

[Configuration](docs/configure.md) · [Client setup](docs/clients.md) ·
[Accounting](docs/accounting.md) ·
[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) ·
[Privacy](PRIVACY.md) · [Security](SECURITY.md)

[Apache-2.0](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md) · [Brand reservation](NOTICE)
