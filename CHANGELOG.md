# Changelog

## 0.2.0 development

- Treat model IDs selected during provider setup or supplied with `--model` as
  the provider's allowed-model list; use `--default-model` only to set the
  fallback when a client omits its model.
- Remove the DeepSeek `--preset` shortcut; use the general `--base-url` and
  `--model` options, which still select verified built-in rates for known models.
- Expose OpenAI Chat Completions and Anthropic Messages APIs simultaneously to
  clients and automatically detect the provider protocol from the configured
  API root/model discovery.
- Translate requests, responses, tool calls and streaming events in both
  directions through the single configured `base_url`; remove the separate
  Anthropic base URL and native `protocol: both` configuration mode.

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
