# Changelog

## 0.3.2 — 2026-10-10

- Coordinate hot-reloaded routes by provider/model identity. New configuration
  rules take priority over compatible connection reuse and retained session
  affinity; queued requests and streams keep their admitted configuration.
- Add independent provider logical-session limits, live CLI updates and an
  authenticated capacity status endpoint. Retain the optional gateway limit and
  count anonymous requests only while they are in flight.
- Use one Claude-compatible JSONL format for usage and diagnostics. External
  ccusage reads private session files directly; Elasticsearch can collect the
  same records. Missing usage stays unknown, and anonymous session identifiers
  remain stable across gateway restarts with the same credential. Provider
  accounting continues through `tidemux billing`.
- Preserve native Anthropic content and conversation history without local
  tool-role or ID-pairing checks. Keep JSON, event, usage and size checks;
  cross-protocol adapters validate fields needed for conversion.
- Include the audited request ID in terminal SSE errors. Document Claude Code,
  Kilo and Hermes recovery after structural upstream failures.
- Emit one private-safe structured failure for early `serve` startup errors,
  including argument, configuration, credential, ledger and listener failures.

## 0.3.1 — 2026-10-03

- Automatically validate and apply provider additions, updates, removals, model
  scope and Keychain key changes in a running gateway. Swap catalog/routing
  together; admitted requests and SSE retain original credentials, pricing and
  budget. Failed loads preserve the last valid view. Report pending/applied
  status against the saved config instead of requiring provider-add restarts.
- Recover abandoned budget attempts only at gateway startup under an exclusive
  ledger sidecar lock. Reports preserve live pending attempts; proven queued
  cancellation releases the reservation without permanently blocking a provider.
- Bound prompt-cache retention to 256 entries, 16 MiB and five minutes of idle
  time, with a background sweep. Generated request-scoped IDs retain no prompts;
  evictions preserve estimation fallback and provider usage/price semantics.
- Withhold malformed assistant generic Anthropic tool_result blocks, including
  SSE. Reject poisoned native/converted history before dispatch with exact
  paths and explicit recovery; preserve valid native server tools, client tool
  results and compaction. Built-in upstream tool defects remain upstream-owned.
- Ship a minimal Logstash 8.19.5 / Elasticsearch 8.19.5 collector, typed template,
  tidemux namespace/ECS action, parse diagnostics, DLQ reader and actual packaged
  ingestion gate. Deployment, credentials and retention remain operator-owned.
- Retain existing JSON config and SQLite formats. Logging features #100–#104
  remain deferred and are not prerequisites for basic ingestion.

## 0.3.0 — 2026-10-03

- Raise the default request body limit to 32 MiB so image-heavy agent
  conversations can continue; report the configured byte limit on HTTP 413.
  Explicit `limits.request_bytes` settings remain unchanged.
- Cancel active and queued model requests during graceful shutdown and send
  `server_shutting_down` as JSON or an SSE error before closing connections.
  Recover handler panics with redacted errors and preserve audit/budget cleanup.
- Allow streamed generations up to ten minutes by default, including long
  Claude Code compaction turns. Non-streaming requests retain the 60-second
  default; explicit `limits.upstream_timeout_seconds` overrides both modes.
- Return consistent `active_session_limit` codes, gateway capacity and bounded
  backoff guidance in both protocols; omit an unjustified `Retry-After: 1`.
- Accept and forward Anthropic provider-hosted tools such as
  `web_search_20250305`, including provider extension fields, without requiring
  custom-tool `input_schema`; reject unsupported cross-protocol use with a
  field-specific error.
- Add one ordered instance `auto_chain` of provider/model pairs for `model:auto`.
  Stable sessions remain pinned across failures; classified safe failures before
  output advance only the preference for new sessions. Requests without IDs get
  fresh request-scoped IDs. Reject `REF/auto` and attribute actual routes in
  responses, usage, cost and audit records.
- Add opt-in `random` and `price_priority` routing for shared bare model IDs.
  Price priority requires complete, same-currency rates and has deterministic
  provider-reference tie breaking.
- Bind random shared-model routing to stable sessions with atomic first
  selection, hashed caller/protocol/model/session keys and a 24-hour idle TTL.
  Rebind unavailable providers before dispatch; never replay a dispatched
  request to maintain affinity. Bindings reset on gateway restart.
- Add opt-in cross-provider failover only for exact `insufficient_balance`
  error mappings; arbitrary 403 responses and 429 never switch providers.
- Keep both routing options disabled by default, omit the new routing config
  fields unless enabled, and retain the v0.2.2 config and ledger format.
- Add top-level auto-chain and routing CLI commands plus rollback guidance.
- Record the selected `provider_ref` in audit JSON, distinguishing profiles
  that share an upstream label while remaining readable by v0.2.2.

## 0.2.2 — 2026-09-29

- Route bare upstream model IDs when exactly one configured provider scope can
  serve them. Return `model_ambiguous` rather than choosing when scopes overlap;
  preserve explicit `REF/MODEL` routing and cross-protocol conversion.
- Keep the gateway available for resolved providers when an `auto` protocol
  probe cannot classify another provider. Log a provider-specific recovery
  command, exclude the unresolved provider from listings and return
  `provider_unavailable` for its explicit routes. Fail startup if none resolve.
- Tell users to restart a running gateway after `provider add` so the new
  provider is available.
- Ship the post-v0.2.1 budget recovery command and the known-cost settlement
  fix for audit append failures.
- Preserve valid native Anthropic Messages content blocks and citation metadata
  instead of rejecting the request locally or pruning supported fields.
- Release a pending budget reservation when request validation or protocol
  conversion proves no provider request was sent; keep uncertain attempted
  requests fail-closed.
- Complete provider error classification for billing, permissions, policy,
  invalid requests and model access; retain compatible upstream 4xx statuses.
  Keep 429 retries bounded to the selected provider, honor cancellation, and
  never retry once response output may have reached the client.
- Forward Anthropic `context_management` to the configured upstream without
  local rejection or silent omission; preserve native Anthropic compaction
  responses and their round-trip data.
- Emit privacy-bounded JSON Lines runtime events to stderr from `serve`, with
  request-ID-correlated terminal and local-rejection summaries, lifecycle and
  background-task events, and human-readable CLI output on stdout. External
  collection remains operator-managed.
- Update provider, protocol, install and rollback documentation for the 0.2.2
  carry-forward scope while keeping the published v0.2.1 package unchanged.

## 0.2.1 — 2026-09-28

- Classify provider errors by exact, per-provider code mappings and return
  protocol-native safe errors without exposing provider message text.
- Retry HTTP 429 on the same key for up to three total attempts, honoring
  `Retry-After` and the request deadline; return a normalized rate-limit error
  when exhausted. Eligible authentication and pre-write transport failures
  retain same-provider key failover.
- Detect provider protocol from bounded authenticated model-list responses;
  remove endpoint-name heuristics and retain explicit protocol overrides.
- Add read-only gateway connectivity checks and agent-led install/setup
  guidance for Claude Code, Codex, Hermes and dsh.
- Keep configurations, Keychain references, provider scopes, budgets and audit
  records intact; clarify the limits of direct Codex routing.
- Preserve documented Anthropic image, document, citation and server-tool
  blocks on native Anthropic routes; return precise errors for unsupported
  cross-protocol content.
- For rollback to 0.2.0, remove any newly added `error_code_mappings` from the
  config before reinstalling 0.2.0; the older binary does not recognize that
  field.

## 0.2.0 — 2026-09-26

- Require clients to select providers explicitly with `REF/MODEL`. Provider
  model selection is an allowlist; when no IDs are selected, all models remain
  allowed. There is no default provider or default model.
- Remove the DeepSeek `--preset` shortcut; use the general `--base-url` and
  `--model` options, which still select verified built-in rates for known models.
- Expose OpenAI Chat Completions and Anthropic Messages APIs simultaneously.
  Each named provider has one endpoint and protocol; TideMux infers the protocol
  by default and routes requests using the explicit provider/model reference.
- Translate supported request, response, tool and streaming fields between the
  client and provider protocols, preferring a native-protocol route when one is
  available.
- Add a gateway-wide active-session cap with configurable idle release, plus
  Keychain-backed provider key groups with round-robin selection, request
  affinity and safe pre-stream failover within the selected provider only.
- Send bounded, localized plain-text daily report summaries to generic,
  Telegram, Discord and Lark webhooks; keep webhook endpoints in Keychain and
  do not upload the HTML report. Record delivery attempts per report and
  channel, expose delivery history, and allow retries only for failed attempts.

## 0.1.1 — 2026-09-21

- Add independent daily usage reports with JSON history, private self-contained
  HTML exports and delivery tracking, without storing request or response bodies.
- Add macOS Notification Center delivery, clickable report opening through
  `terminal-notifier` when available, permission recovery guidance and retry
  tracking for failed deliveries.
- Add first-run TUI guidance and CLI controls for a local-time daily notification
  schedule, backed by a private per-profile macOS LaunchAgent and `report notify`.
- Add conservative five-hour/seven-day budget controls and automatic local
  reconciliation of normalized supplier statements to the public release line.
- Refresh the 0.1.1 arm64 release procedure, installer defaults, acceptance
  evidence and dependency/license packaging. SMTP and webhook delivery remain
  deferred to follow-up issues.

## 0.1.0 — initial public release

- macOS arm64 gateway for OpenAI-compatible and Anthropic-compatible APIs.
- Direct Claude Code, Kilo CLI and Hermes launchers, streaming and tools, secure
  Keychain setup, explicit concurrency, local diagnostics and usage accounting.
- Homebrew and verified archive installation; English documentation and a
  DeepSeek USD peak price preset. See the protocol matrix for supported features.

Earlier entries below describe local development builds, not separate public releases.

## 0.1.0 English-language polish

- Add the verified DeepSeek English/USD peak preset and CLI-configured custom
  pricing; document that pricing is selected with the provider API key.

- Use English throughout CLI prompts and test fixtures; clarify international
  contribution conventions and provider/currency neutrality.
- Refresh project descriptions and distinguish the active currency-aware audit
  ledger from historical database compatibility fields.

## 0.1.0 — 2026-09-12

- Local dual-protocol gateway with streaming, tool conversations, bounded
  request validation, authenticated model discovery and private Keychain setup.
- SQLite audit, explicit model prices, unknown-usage semantics and local
  reconciliation; isolated Claude, Kilo and Hermes launcher profiles.
- Checksum-verified archive installation, dependency notices and SPDX metadata.
