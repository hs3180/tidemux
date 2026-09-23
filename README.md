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

For Kilo CLI or Hermes Agent:

```sh
tidemux configure --preset deepseek-flash
```

This preset uses the single-provider compatibility mode: TideMux detects the
upstream protocol from the API root and exposes both client APIs, translating
between them when needed. To use DeepSeek's Anthropic-compatible API as that
endpoint, override the root:

```sh
tidemux configure --preset deepseek-flash \
  --base-url https://api.deepseek.com/anthropic/v1
```

For another provider, run `tidemux configure` without flags for guided setup.
Enter the provider API Base URL and API key; TideMux derives a name from the
URL, detects the upstream protocol and discovers models when possible. It allows
all model IDs by default: press Enter at the optional model-scope prompt, or
enter a subset to restrict which IDs TideMux forwards. The upstream still
determines which models are actually available. For command-line setup, use
`--provider NAME,BASE_URL,MODEL` (add a protocol to force it) and, only when
needed, `--provider-models NAME,MODEL[,MODEL...]` (include the fallback model).
The key is still entered at a hidden prompt. Named providers serve only clients
using the same protocol and do not cross-translate. To route both client
protocols to named providers, configure one for each and select their defaults.
See [configuration](docs/configure.md) for pricing, routing and other details.

### 2. Start the gateway

```sh
tidemux doctor
tidemux serve
```

`doctor` checks local configuration. Keep the gateway terminal open; stop it with **Ctrl-C**.
Next time, just run `tidemux serve` to reuse your configuration.

### 3. Launch your client

In another terminal, from your project directory, run the client you want. In
the single-provider compatibility mode used by this preset, both client APIs
are exposed and TideMux detects the upstream protocol automatically:

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
(used by the preset and `--base-url`/`--model` setup), either client protocol
can use the upstream through translation. Guided setup creates a named provider
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

## Documentation

[Configuration](docs/configure.md) · [Client setup](docs/clients.md) ·
[Accounting](docs/accounting.md) ·
[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) ·
[Privacy](PRIVACY.md) · [Security](SECURITY.md)

[Apache-2.0](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md) · [Brand reservation](NOTICE)
