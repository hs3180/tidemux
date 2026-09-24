# Client compatibility — 0.1.1

Verified on macOS arm64 on 2026-09-11 with real DeepSeek `deepseek-flash`.
The 0.1.1 release carries forward the verified client matrix from the 0.1.0
repair; use BUILD.txt and binary SHA256 to identify the exact release artifact.
This is 0.1.1 release evidence only; it does not certify the 0.2.0
protocol-specific provider routing. See [protocol support](protocols.md) for
that development contract.

The gateway exposes both client APIs in every profile: OpenAI clients call
`/v1/chat/completions`, and Anthropic clients call `/v1/messages`. The selected
`protocol` controls only the provider side. Matching client/provider pairs pass
through; the other two combinations use the bidirectional adapter.

| Client | Installed CLI read/edit/test | Persistent continuation | Status |
| --- | --- | --- | --- |
| Claude Code 2.1.263 | Passed | Passed | CLI verified |
| Kilo CLI 7.6.2 | Passed | Passed | CLI verified |
| Hermes Agent 0.21.1 | Passed | Passed | CLI verified |

VS Code extension compatibility is outside the 0.1.1 release scope. Existing
experimental code and historical development evidence are retained, without
a pending IDE acceptance requirement.

## Verified CLI behavior

- Product launchers read the local gateway credential from Keychain, check
  authenticated model discovery and configure isolated persistent profiles.
- Real workflows stream output, read a fixture, repair a small function, execute
  unittest without modifying the test file, and continue in a new client process.
- Controlled local upstream tests cover missing usage, truncation, cancellation,
  queueing and recovery from upstream 429/5xx. Hermes may recover a truncated
  stream through a non-streaming retry; each attempt remains independently audited.
- Client-delivered input/output/cache usage and configured DeepSeek peak pricing
  reconcile with installed gateway records. Failed/unknown amounts remain
  null. These checks validate usage and estimates, not provider invoices.
- Package checksums, SPDX/build provenance, Homebrew install/rollback/reinstall
  and dual-protocol old/new/old configuration and ledger compatibility passed.

## Compatibility decisions

Hermes uses an explicit custom provider with Chat Completions transport. Its
OpenAI provider can select Responses API, which TideMux does not serve. Optional
Ollama-style capability probes return 404 without blocking the tested workflow.
No token-counting request appeared in the observed core client workflows; the
endpoint remains unsupported rather than returning a fabricated token count.

DeepSeek rejects Hermes's JSON-schema auxiliary request. TideMux preserves a safe
400 response, allowing Hermes to retry without response_format. A LiteLLM control
experiment confirmed the same client-side fallback unless parameter dropping
was explicitly enabled. No silent parameter removal is performed by TideMux.

Claude's observed beta headers, thinking signatures, cache markers and tool
history survive forwarding. DeepSeek accepts mid-conversation system messages;
this extension and other advanced behavior are not guaranteed on every
Anthropic-compatible service. Kilo credentials use a trusted environment-variable
reference, not a secret in workspace configuration.

This page records 0.1.1 compatibility evidence, not setup instructions.
Current launcher commands are in [client setup](clients.md); the
[0.2.0 protocol boundaries](protocols.md) describe provider routing.
Image/audio, provider server tools and other unimplemented APIs remain explicit limitations.
The original failed baseline and complete private test evidence remain archived;
they are not substituted for passing repair results. Release artifacts are available from the project’s GitHub Releases page.
