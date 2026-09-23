# Configuration examples

These files cover two configuration generations. The current published 0.1.x
CLI uses a single-provider configuration; the target 0.2.0 provider model uses
the `providers` map and routes each client protocol to its own provider.

## Target 0.2.0 provider model

- [`named-providers.json`](named-providers.json) demonstrates separate OpenAI
  and Anthropic protocol providers and per-protocol defaults. The identifiers
  in this sample are illustrative; users do not need to invent provider names
  when adding providers through the target CLI.

## 0.1.x single-provider format

These samples are for the released CLI and its compatibility configuration;
they are not templates for the 0.2.0 provider model:

- [`openai.json`](openai.json)
- [`anthropic.json`](anthropic.json)
- [`deepseek-openai.json`](deepseek-openai.json)
- [`deepseek-anthropic.json`](deepseek-anthropic.json)
- [`tidemux.example.json`](../tidemux.example.json), the packaged generic
  single-provider template.

For runnable 0.1.1 setup steps, see the [local demo guide](../docs/demo.md).
