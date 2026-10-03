# TideMux 0.3.0 release guide

This document records the scope and acceptance gates for v0.3.0. The release
source commit is recorded in `BUILD.txt`, and artifact checksums are recorded in
`SHA256SUMS`.

Baseline: published v0.2.2, tag `v0.2.2`, source commit
`9223e0a802d7e15780ff189eaf0093e338121189`. The release keeps the existing
JSON config and SQLite ledger formats. The optional top-level `auto_chain` field
and top-level `routing` field are omitted until configured; their disabled
defaults remain readable by v0.2.2.

Candidate history (2026-10-02): [#91](https://github.com/hs3180/tidemux/issues/91)
session affinity is included in v0.3.0. The earlier `b9c66c8ced30` package was
superseded because it did not include #91 or the current #40 session semantics.

Client reliability update (2026-10-03): merged
[PR #97](https://github.com/hs3180/tidemux/pull/97) fixes
[#92](https://github.com/hs3180/tidemux/issues/92) graceful shutdown,
[#94](https://github.com/hs3180/tidemux/issues/94) large agent requests and
[#90](https://github.com/hs3180/tidemux/issues/90) session-capacity retry guidance.
The release acceptance below covers these fixes and long streamed generations.

0.3.1 correction: [#61](https://github.com/hs3180/tidemux/issues/61) was reopened
with an automatic-application requirement. The historical restart guidance
below does not satisfy that requirement; see the [0.3.1 guide](release-0.3.1.md).

## 0.2.2 carry-forward audit

The 11 issues below were checked against the published source, changelog,
acceptance text and release checks. The `0.2.2` milestone was closed with all
11 issues completed on 2026-09-30.

| Issue | Result | Acceptance evidence |
| --- | --- | --- |
| [#31](https://github.com/hs3180/tidemux/issues/31) | Delivered | Exact provider error-code mappings and safe protocol-native errors: `internal/adapter/errors_test.go`, `internal/gateway/provider_error_mapping_test.go`. |
| [#47](https://github.com/hs3180/tidemux/issues/47) | Delivered | Bounded 429 retries on the selected provider/key, `Retry-After`, deadlines and cancellation: `internal/adapter/client_test.go`, `internal/gateway/provider_error_mapping_test.go`. No cross-provider behavior is added to 429. |
| [#51](https://github.com/hs3180/tidemux/issues/51) | Delivered | Bounded authenticated protocol probes, behavior-based detection and explicit override: `internal/gateway/provider_test.go`. |
| [#52](https://github.com/hs3180/tidemux/issues/52) | Delivered | Bare model routing and provider-scope launcher behavior: `internal/gateway/endpoints_test.go`, `scripts/test_client_commands.py`. |
| [#53](https://github.com/hs3180/tidemux/issues/53) | Delivered | Native and converted context-management requests plus buffered/SSE compaction round trips: `internal/gateway/context_management_test.go`; packaged Claude Code check passed against the release binary. |
| [#54](https://github.com/hs3180/tidemux/issues/54) | Delivered | Native Anthropic content, document and citation preservation: `internal/adapter/request_test.go`, `internal/gateway/gateway_test.go`. |
| [#61](https://github.com/hs3180/tidemux/issues/61) | Delivered | Provider-add restart guidance and PTY acceptance: `internal/gateway/provider_test.go`, `scripts/test_provider_add_pty.py`. |
| [#62](https://github.com/hs3180/tidemux/issues/62) | Delivered | Unresolved `protocol:auto` providers are isolated while resolved providers serve: `internal/gateway/provider_test.go`, `internal/gateway/provider_detection_behavior_test.go`. |
| [#63](https://github.com/hs3180/tidemux/issues/63) | Delivered | Structured runtime event schema and packaged privacy check: `internal/gateway/runtime_logs_test.go`, `scripts/test_runtime_logs_package.py`. |
| [#64](https://github.com/hs3180/tidemux/issues/64) | Delivered | Provider budget recovery and known-cost settlement: `scripts/test_budget_recovery_package.py`. |
| [#69](https://github.com/hs3180/tidemux/issues/69) | Delivered | Pre-dispatch reservation release and unknown-usage fail-closed behavior: `internal/gateway/budget_reservation_test.go`, `scripts/test_budget_reservation_package.py`. |

## 0.3.0 scope

- **[#80](https://github.com/hs3180/tidemux/issues/80): Anthropic server-side
  tools.** Accept `web_search_20250305` and other provider-hosted Anthropic
  tool definitions without requiring custom `input_schema`; preserve their
  provider-specific fields on native Anthropic routes. Preserve object-schema
  validation for `type: custom`. Reject unsupported cross-protocol use on
  `tools` before dispatch. See `internal/adapter/tools_test.go`,
  `internal/gateway/tools_test.go` and
  `scripts/test_anthropic_server_tools_package.py`.
- **[#40](https://github.com/hs3180/tidemux/issues/40): one instance auto chain.**
  Configure ordered provider/model pairs with top-level `auto-chain show|set|clear`.
  Only `model:auto` uses the chain; reject `REF/auto`. Stable sessions stay pinned
  across failures. Classified safe failures before output advance the preference
  only for new sessions, without replaying the failed request. Requests without
  IDs receive fresh request-scoped IDs. Preserve actual route attribution.
  See `internal/gateway/auto_chain_test.go` and `scripts/test_auto_chain_package.py`.
- **[#41](https://github.com/hs3180/tidemux/issues/41): shared-model choice.**
  `routing.shared_model_strategy` is opt-in (`random` or `price_priority`).
  Eligible providers must match the model scope, cooldown and request protocol
  features. Price priority compares the sum of the three per-million-token
  rates only when every rate is present and currencies match; equal totals use
  provider-reference order.
- **[#91](https://github.com/hs3180/tidemux/issues/91): session affinity for
  shared-model routing.** When random shared-model selection is enabled, bind
  a hashed key composed of caller namespace when available, protocol, normalized
  bare model ID and stable session ID to the first randomly selected eligible
  provider. Use `X-TideMux-Session-ID`, with Anthropic `metadata.user_id` as a
  fallback. Reuse the binding for 24 hours of idle time; requests without a
  stable session ID remain request-scoped random. Keep bindings in process
  memory only. Rebind only before dispatch when the bound provider is no longer
  eligible or available; never retry or switch providers after dispatch or
  during a stream. This is separate from `model:auto` chain state. Session-bound
  requests do not use billing provider failover, but retain the existing bounded
  same-provider 429 retries. Source checks are in
  `internal/gateway/shared_model_affinity_test.go`; package checks are in
  `scripts/test_shared_model_affinity_package.py`.
- **[#32](https://github.com/hs3180/tidemux/issues/32): billing exhaustion
  failover.** Cross-provider attempts require the opt-in
  `routing.billing_exhaustion_failover` and an exact error-code mapping to
  `insufficient_balance`. Arbitrary 403 responses and 429 never cross
  providers. At most four alternate providers are tried per model. Each actual
  provider/model attempt receives its own audit and budget settlement.

## Upgrade and rollback

Client reliability fixes raise the default request-body cap to 32 MiB, preserve
explicit byte limits, and include the actual gateway limit in HTTP 413 errors.
Default streaming timeout is ten minutes; non-streaming timeout remains 60
seconds. Explicit `limits.upstream_timeout_seconds` settings still apply to
both modes. Users with an explicitly saved 60-second timeout must increase it
to allow long streamed agent turns. No new config field or ledger column is
required. Graceful shutdown sends `server_shutting_down` errors and settles
active/queued requests; panics produce a redacted `internal_error`. Local
session-capacity rejection exposes consistent codes and backoff guidance without
promising one-second recovery. See [streaming and errors](protocols.md#streaming-and-errors).

The release adds optional config fields and an optional `provider_ref` field
to new audit JSON, without changing SQLite columns or rewriting historical
rows. The v0.2.2 billing reader accepts these new rows. With routing disabled,
both new config fields are omitted. When enabling
the features, use these commands to revert the config before replacing the
binary with v0.2.2:

```sh
tidemux auto-chain clear
tidemux routing set --shared-model-strategy off --billing-exhaustion-failover=false
```

The rollback commands remove the new fields from
the config; v0.2.2 uses the same Keychain references and ledger. If any
provider error map uses the new `temporarily_unavailable` category, remove that
mapping with `tidemux provider error-map remove REF --code CODE [--status STATUS]`
before starting v0.2.2, which does not recognize that category.

## Release acceptance gates

Run the complete source and package checks from the reviewed commit:

```sh
gofmt -l $(git ls-files '*.go')
CGO_ENABLED=0 go test ./...
go test -race ./...
CGO_ENABLED=0 go vet ./...
python3 scripts/test_anthropic_server_tools_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_shared_model_affinity_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_auto_chain_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_client_commands.py
python3 scripts/test_context_management_claude.py --binary /path/to/extracted/tidemux
python3 scripts/test_provider_add_pty.py
python3 scripts/test_budget_recovery_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_budget_reservation_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_runtime_logs_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_client_reliability_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_install.py
```

For ledger rollback proof, run `test_auto_chain_package.py` again with
`--baseline-binary /path/to/extracted/v0.2.2/tidemux`. It verifies that v0.2.2
can read every new audit record without modifying the candidate ledger.

The client reliability gate verifies image requests above 1 MiB, explicit
request limits, session-capacity guidance in both protocols, SIGTERM during
buffered and streamed requests, persisted cancellation and privacy. Source
tests also cover exact default/custom byte boundaries, handler panics, queued
budget cleanup and a 61-second generation under simulated time. For a real
clock test of the former 60-second cutoff, additionally run the package gate
with `--long-stream-seconds 65`.

Build the immutable local archive, checksums, SPDX SBOM, `BUILD.txt` and
Homebrew formula with `scripts/release.py --build-id commit`. Verify those
files against the candidate commit, then run the packaged upgrade/rollback and
clean Homebrew installation checks. Release package contents must be rebuilt
from the reviewed commit; do not modify generated assets by hand.

Run the offline installer check with the public v0.2.2 archive as the baseline:

```sh
python3 scripts/test_install_upgrade_rollback.py \
  --candidate-dir /path/to/dist/0.3.0+COMMIT \
  --baseline-dir /path/to/v0.2.2-assets \
  --candidate-version 0.3.0 --baseline-version 0.2.2
```

Before user review, record the candidate commit, archive checksum, SBOM and
formula validation, issue states and every gate result. Publishing v0.3.0
requires separate user authorization.
