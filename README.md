# TideMux

Use Claude Code, Kilo CLI or Hermes Agent with your own LLM API.
TideMux is a local macOS gateway for OpenAI Chat Completions and Anthropic
Messages, with routing, usage accounting and private logs.

## Install

Requires macOS 15+ on Apple Silicon. Install your client CLI separately.

```sh
brew install hs3180/tap/tidemux
```

If prompted, run `brew trust --formula hs3180/tap/tidemux` and retry.
Use `brew upgrade tidemux` to upgrade and `brew uninstall tidemux` to uninstall.
See [other installation methods](docs/guide.md#installation).

## First request

Add your provider's API root. Enter its key in the hidden terminal prompt;
TideMux stores credentials in macOS Keychain.

```sh
tidemux provider add https://api.example.com/v1 --model MODEL_ID
tidemux provider list
tidemux doctor
tidemux serve
```

Keep `serve` running. In another terminal, use the reference from `provider list`:

```sh
tidemux claude --model REF/MODEL_ID
tidemux kilo --model REF/MODEL_ID -- run 'Explain this project'
tidemux hermes --model REF/MODEL_ID -- -q 'Explain this project'
```

## Read next

- [User guide](docs/guide.md): providers, clients, routing, billing, logs and troubleshooting.
- [Core design](docs/design.md): request, configuration and accounting contracts.
- [Contributing and releases](https://github.com/hs3180/tidemux/blob/main/CONTRIBUTING.md).

The gateway supports `/v1/chat/completions` and `/v1/messages`.
Responses API, audio, embeddings and IDE extensions are outside its supported
interface. Cross-protocol features must be representable by the target API.
See [compatibility](docs/guide.md#compatibility).

[Privacy](PRIVACY.md) · [Security](SECURITY.md) · [Changelog](CHANGELOG.md) · [License](LICENSE)
