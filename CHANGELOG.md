# Changelog

All notable public-facing changes are documented here.

## 0.1.0-rc.1 — 2026-09-11

First local-MVP release candidate.

- Loopback-only OpenAI-compatible `POST /v1/chat/completions` for one
  non-streaming DeepSeek provider and a caller Bearer token.
- macOS Keychain references for the provider key and gateway credential; no
  credential fields are accepted in JSON configuration.
- Explicit concurrency gate, SQLite request/event ledger, and safe structured
  failures for validation, cancellation, and upstream errors.
- `tidemux serve`, `tidemux doctor`, example config, and a no-real-key smoke
  test for a clean built binary.

This is not a GitHub release, Homebrew formula, or production calibration.
