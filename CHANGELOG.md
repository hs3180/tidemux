# Changelog

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
