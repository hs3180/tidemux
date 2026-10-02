# Changelog

## 0.3.0 (unreleased)

- Accept Anthropic provider-hosted tool definitions such as
  `web_search_20250305` without requiring a custom-tool `input_schema`, and
  preserve provider-specific fields on native Anthropic routes. Reject
  unsupported cross-protocol use with an actionable `tools` error.
- Add one instance-wide ordered `model:auto` chain of provider/model pairs.
  Stable session IDs stay pinned; requests without IDs receive request-scoped
  IDs and count as new sessions. Classified failures advance the preferred
  entry for new sessions without switching models within a request.
- Add opt-in random routing for shared bare model IDs.
- Add opt-in same-model provider failover for errors exactly mapped to
  `insufficient_balance`; keep billing and usage records attributed per
  provider and cool down the exhausted provider's keys for five minutes.

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

## 0.1.0 installation and CLI polish — 2026-09-12

- Recommend Homebrew installation from `hs3180/tap`; provide a checksum-verifying
  release installer and offline installer tests. Public distribution is pending.
- Use DeepSeek presets in the quick start. Rename the recommended client command
  to `tidemux claude|kilo|hermes`; remove the `connect` and `launch` command forms.
- Reject directory installation targets, including symlinks to directories;
  verify the installed executable. Add direct-client process tests and document
  request accounting, price snapshots and reconciliation limitations.

## 0.1.0 — 2026-09-12 (final local CLI release)

- Dual-protocol SSE, tool calls/results, reasoning and cache fields; explicit
  parameter validation, safe upstream errors and selected Anthropic beta headers.
- Authenticated model discovery and declared model limits, configurable request
  and streaming limits, separate local diagnostics and accurate unknown usage.
- `connect claude|kilo|hermes` with Keychain handoff, persistent isolated profiles
  and Hermes Chat Completions routing. `configure --replace` preserves a private
  original config backup.
- All three CLIs passed installed DeepSeek read/edit/test/continuation and usage/
  price checks. Package/install/rollback and protocol/fault regression passed.
- VS Code compatibility is excluded from release scope; the experimental
  `connect kilo-ide` implementation and historical evidence are retained.
- Application version remains 0.1.0. Final assets use an immutable commit suffix;
  the original unpublished tag is archived before the final local tag is assigned.
  GitHub publication remains deferred.

## 0.1.0 — 2026-09-11 (local release)

- Freeze the dual-protocol local MVP with interactive system Keychain unlock,
  secure configure, concurrency control, atomic audit and explicit pricing.
- Real `deepseek-flash` calls succeeded through both OpenAI and Anthropic paths;
  response usage and ledger/cost arithmetic reconciled. Runtime code is unchanged
  from the live-tested rc.4; only version and release documentation changed.
- Local binary, checksums, SBOM and Homebrew assets prepared. GitHub publication
  remains deferred; other providers and clean-machine public installation unverified.

## 0.1.0-rc.4 — 2026-09-11

- `configure` automatically starts the system keychain unlock password prompt
  in the same terminal when needed, then continues API-key setup. The Mac/keychain
  password goes directly to `security`, never through TideMux or command arguments.
- PTY regressions cover already-unlocked, successful unlock, failed unlock and
  cancellation, including no secret echo and no state changes on failure.

## 0.1.0-rc.3 — 2026-09-11

- `configure` CLI with a DeepSeek Flash preset, generic endpoint/model options,
  hidden terminal key entry, generated local credentials, Keychain verification,
  private config files and rollback on setup failure. Default config path supports
  `serve` / `doctor` / `ledger` without repeated flags.
- Onboarding PTY tests and an opt-in real isolated Keychain test.

- Accept explicit `thinking: {"type":"disabled"}` for non-thinking text
  calls on compatible endpoints such as `deepseek-flash`; enabling thinking
  remains outside this text-only subset. Added a live-helper flag and regression.
- GitHub publication is deferred by the maintainer; candidates remain local
  until real endpoint verification is available.

## 0.1.0-rc.2 — 2026-09-11

Local candidate, not a public 0.1.0 release.

- Generic OpenAI Chat Completions and Anthropic Messages adapters with explicit
  API roots, model IDs, Keychain credentials and protocol authentication.
- Strict non-streaming text requests, safe errors, preserved upstream success
  JSON, request IDs, response size limits and redirect rejection.
- Atomic `request_audit` / `audit_events` tables preserve legacy tables without
  rewriting historical data. Transport, read, parse and canceled paths are audited.
- Explicit model price snapshots, currency and unknown usage/cost semantics.
- `ledger` CLI plus dual-protocol process, failure, pricing and race tests.
- Candidate packaging, SPDX SBOM, dependency notices, SHA256 and tap formula
  generation. Live validation and public installation remain pending.

Migration: replace old `deepseek_*` config keys with the generic examples.
Back up the database with the service stopped before upgrading. Old tables are
retained; the `ledger` command reads the new audit format only.

## 0.1.0-rc.1 — 2026-09-11

Initial DeepSeek-only source baseline with loopback auth, Keychain config,
concurrency, SQLite and CLI smoke tests. No public release was created.
