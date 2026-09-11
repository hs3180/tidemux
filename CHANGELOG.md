# Changelog

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
