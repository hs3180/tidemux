# TideMux

**Connect Claude Code, Kilo CLI and Hermes Agent to your own LLM API.**

TideMux is a local macOS gateway for **OpenAI-compatible and Anthropic-compatible APIs**.

- **Streaming and tools** — forward conversations and tool calls to your chosen provider.
- **Simple client setup** — run `tidemux claude`, `tidemux kilo` or `tidemux hermes` with secure credentials and separate profiles.
- **Concurrency control** — limit active requests and queue the rest.
- **Local usage ledger** — track outcomes, tokens and cost estimates without storing message bodies.

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
tidemux configure --preset deepseek
```

For Claude Code, use this command **instead** to select the Anthropic-compatible endpoint:

```sh
tidemux configure --preset deepseek --protocol anthropic
```

Paste your API key at the hidden prompt; TideMux handles Keychain storage.
Other OpenAI-compatible or Anthropic-compatible providers work through
[custom API configuration](docs/configure.md#any-other-compatible-api).

### 2. Start the gateway

```sh
tidemux doctor
tidemux serve
```

`doctor` checks local configuration. Keep the gateway terminal open; stop it with **Ctrl-C**.
Next time, just run `tidemux serve` to reuse your configuration.

### 3. Launch your client

In another terminal, from your project directory, run the command matching your configured protocol:

```sh
# Anthropic profile
tidemux claude

# OpenAI profile — choose one
tidemux kilo -- run 'Explain this project'
tidemux hermes -- -q 'Explain this project'
```

TideMux supplies the gateway URL, model and local credential to the client
without overwriting its original profile.

Inspect requests with `tidemux ledger`. Cost estimates require
[configured model prices](docs/configure.md); unknown amounts stay unknown.
Use the [DeepSeek USD rates](docs/deepseek-pricing.md) for this example.
For separate profiles or simultaneous use of both protocols, see [client setup](docs/clients.md).

## Compatibility

**0.1.0** supports Chat Completions and Messages, one upstream and protocol per
instance. The three CLIs have passed DeepSeek file read/edit/test and continued
conversation workflows. You can configure other compatible providers, though they have not been tested.

Responses API, protocol conversion, images/audio and IDE extensions are outside
this release's scope. See [tested clients](docs/client-compatibility.md) and
[protocol support](docs/protocols.md).

## Documentation

[Configuration](docs/configure.md) · [Client setup](docs/clients.md) ·
[Accounting](docs/accounting.md) ·
[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) ·
[Privacy](PRIVACY.md) · [Security](SECURITY.md)

[Apache-2.0](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md) · [Brand reservation](NOTICE)
