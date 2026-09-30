# TideMux 0.3.0 release candidate

Status: local release candidate in review. The public stable release remains
v0.2.2. This plan does not create a v0.3.0 tag or GitHub Release, upload
assets, update the Homebrew tap, or announce availability.

Baseline: published v0.2.2, tag `v0.2.2`, source commit
`9223e0a802d7e15780ff189eaf0093e338121189`. The candidate keeps the existing
JSON config and SQLite ledger formats. The optional top-level `auto_chain`
field and `routing` field are omitted until configured; their disabled defaults
remain readable by v0.2.2.

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
- **[#40](https://github.com/hs3180/tidemux/issues/40): one ordered,
  instance-wide `model:auto` chain.** Configure cross-provider provider/model
  pairs with `tidemux auto-chain`. Stable session IDs stay pinned to one pair;
  requests without an ID get a request-scoped ID and count as new sessions.
  Model-not-found, temporarily-unavailable, insufficient-balance and
  pre-header transport classifications can advance the preferred pair for new
  sessions before output. A request never switches pairs after a failure;
  explicit model IDs stay pinned and output ends all retries.
- **[#41](https://github.com/hs3180/tidemux/issues/41): shared-model choice.**
  `routing.shared_model_strategy` is opt-in (`random` or `price_priority`).
  Eligible providers must match the model scope, cooldown and request protocol
  features. Price priority compares the sum of the three per-million-token
  rates only when every rate is present and currencies match; equal totals use
  provider-reference order.
- **[#32](https://github.com/hs3180/tidemux/issues/32): billing exhaustion
  failover.** Cross-provider attempts require the opt-in
  `routing.billing_exhaustion_failover` and an exact error-code mapping to
  `insufficient_balance`. Arbitrary 403 responses and 429 never cross
  providers. At most four alternate providers are tried per model. Each actual
  provider/model attempt receives its own audit and budget settlement.

## Upgrade and rollback

The candidate adds only optional config fields and does not migrate or rewrite
audit rows. With routing disabled, both new fields are omitted. When enabling
the features, use these commands to revert the config before replacing the
binary with v0.2.2:

```sh
tidemux auto-chain clear
tidemux routing set --shared-model-strategy off --billing-exhaustion-failover=false
```

The rollback commands remove the new fields from the config; v0.2.2 uses the
same Keychain references and ledger. If any
provider error map uses the new `temporarily_unavailable` category, remove that
mapping with `tidemux provider error-map remove REF --code CODE [--status STATUS]`
before starting v0.2.2, which does not recognize that category.

## Candidate acceptance gates

Run the complete source and package checks from the reviewed commit:

```sh
gofmt -l $(git ls-files '*.go')
CGO_ENABLED=0 go test ./...
go test -race ./...
CGO_ENABLED=0 go vet ./...
python3 scripts/test_anthropic_server_tools_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_client_commands.py
python3 scripts/test_context_management_claude.py --binary /path/to/extracted/tidemux
python3 scripts/test_provider_add_pty.py
python3 scripts/test_budget_recovery_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_budget_reservation_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_runtime_logs_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_install.py
```

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
