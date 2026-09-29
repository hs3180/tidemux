# TideMux 0.2.2 release plan

Keep the public `v0.2.1` tag and assets unchanged. Version `0.2.2` carries
forward the unfinished `0.2.1` acceptance gates and fixes that landed after
the `v0.2.1` tag. It also includes the structured-runtime-logging feature
tracked by #63 and the budget-reservation fix tracked by #69. This plan does
not authorize a merge or release.

## Carry-forward from 0.2.1

- **#31 and #47 — P0 provider error handling:** map configured provider error
  codes to safe protocol-native client errors, preserve compatible status
  semantics, and retry rate limits only on the selected provider/key with
  bounded attempts, `Retry-After`, deadlines, and cancellation. The user
  reopened these because the published implementation did not meet the issue
  acceptance criteria.
- **#51 — P0 protocol detection:** infer protocol from bounded authenticated
  endpoint behavior, including misleading URL paths, ambiguity, unavailable
  endpoints, and a provider whose probe must not take down unrelated providers.
- **#52 — P0 bare model IDs:** route a bare upstream ID when exactly one
  configured provider scope can serve it; reject ambiguous matches without an
  upstream request. Preserve explicit `REF/MODEL`, model allowlists, and
  cross-protocol routing.
- **#53 — P0 Claude Code context management:** preserve and forward the entire
  `context_management` JSON value. Native Anthropic requests must preserve the
  field and relevant headers; compaction blocks and stop reasons must round-trip
  through buffered and SSE responses and follow-up requests. PR #67 only
  improved the local error and is insufficient.
- **#54 — P0 native Anthropic content:** preserve valid Messages content blocks
  and citation metadata on the native Anthropic route.
- **#61 — P1 provider additions:** tell users when a running gateway needs a
  restart to load a newly added provider, and verify availability after restart.
- **#62 — P0 recoverable `protocol:auto` cold start:** isolate unresolved
  providers, serve resolved providers, return a provider-scoped unavailable
  error for unresolved routes, and fail clearly only when no provider resolves.

## Post-tag fixes and 0.2.2 budget integrity

- **#64 packages #57 and #59:** include the provider-scoped 5h/7d budget reset,
  safe recovery diagnostics, and reliable known-cost settlement after an audit
  append failure. Unknown usage remains fail-closed. Both fixes landed after
  the public `v0.2.1` tag and are absent from its archive.
- **[#69 — budget reservation on pre-dispatch rejection](https://github.com/hs3180/tidemux/issues/69):** remove a pending reservation when
  request validation or protocol conversion proves that no provider request was
  dispatched. A later valid request must still be admitted; any request that
  may have reached the provider with unknown usage must remain fail-closed.

## New 0.2.2 feature

- **#63 — structured runtime logs for external collection:** use Go's standard
  library `log/slog` JSON handler for one-event-per-line runtime logs on stderr
  during `serve`, while keeping human CLI output on stdout. Cover lifecycle,
  HTTP server errors, terminal upstream attempts, local rejections, and
  statement-sync failures with stable, request-ID-correlated metadata. Keep
  HTTP status distinct from terminal outcome for failures after streaming has
  started. Document the schema and operator-managed Elasticsearch collection
  boundary. Do not add an Elasticsearch client, new logging dependency, or
  SQLite schema changes.

## Deferred

- **#38 — deferred:** agent install/config acceptance remains open for later
  planning and is not a 0.2.2 release gate.

Full per-key health metrics in #17 remain deferred; emit only fields required
by #63.

## Acceptance and release checks

- Verify model routing for bare and qualified IDs across both client protocols,
  unique and ambiguous scopes, model allowlists, and slash-containing upstream
  IDs. Confirm ambiguous requests never reach an upstream. Verify the Claude,
  Kilo and Hermes launchers accept bare IDs and select model capabilities from
  the same unique provider scope with
  `TIDEMUX_TEST_BINARY=/path/to/tidemux python3 scripts/test_client_commands.py`.
- Verify a resolved Anthropic `auto` probe starts; an inconclusive provider is
  isolated while other providers serve; all-inconclusive configuration fails
  with each provider reference and the explicit override command.
- Verify auto detection ignores misleading path labels: an `/anthropic` path
  returning OpenAI model-list semantics resolves as OpenAI, an `/openai` path
  returning Anthropic semantics resolves as Anthropic, and a DeepSeek-style
  `/anthropic` path resolves from its response behavior. Probes remain bounded,
  authenticated, redirect-free GETs with no model-generation request.
- Verify provider addition with a running-gateway fixture produces the restart
  instruction and that a restart makes the provider discoverable and routable.
- Verify audit-append failure preserves known cost, unknown cost still fails
  closed, locally rejected requests release only their pending reservation,
  and the budget reset command changes only the selected provider and rolling
  window. Run the packaged reset and known-cost audit-recovery checks with
  `python3 scripts/test_budget_recovery_package.py --binary /path/to/tidemux`.
  Run the no-dispatch release and unknown-settlement fail-closed checks against
  the packaged binary with
  `python3 scripts/test_budget_reservation_package.py --binary /path/to/tidemux`.
  The test uses fake Keychain credentials and a local provider mock, then scans
  SQLite/WAL files for request and response body sentinels.
- Verify `serve` emits valid JSON Lines to stderr and leaves regular CLI output
  human-readable on stdout. Check request-ID correlation, terminal outcome,
  HTTP status, latency, queue time, provider/model metadata, server errors, and
  statement-sync failure summaries. Prove logs exclude credentials, auth
  headers, session IDs, bodies, raw upstream error bodies, full URL query
  strings, and pricing snapshots. Run
  `python3 scripts/test_runtime_logs_package.py --binary /path/to/tidemux` on
  the extracted package. See [runtime logging](runtime-logging.md) for the
  event schema and external collection/privacy boundaries; add no Elasticsearch
  or other logging dependency.
- On macOS with Claude Code installed, run
  `python3 scripts/test_context_management_claude.py --binary /path/to/tidemux`
  against the extracted package binary. Without `--binary`, it builds the
  current source. It isolates Claude Code
  configuration and inherited environment, substitutes fake Keychain
  credentials, and uses local mocks.
  Confirm the captured inbound request has a non-null `context_management`
  object and applicable beta header, then verify the same complete object
  reaches the selected upstream on both native Anthropic and OpenAI-compatible
  routes. A run that does not emit the field fails this gate. Verify buffered/SSE
  compaction responses and stop reasons round-trip through a follow-up request;
  local rejection or silent field drop also fails.
- Run `gofmt`, `CGO_ENABLED=0 go test ./...`, `CGO_ENABLED=0 go vet ./...`,
  `go test -race ./...`, provider-add PTY, shell syntax, installer, client
  command and license checks.
- Build from the reviewed commit with `scripts/release.py`; verify SHA256,
  SBOM, BUILD.txt and the generated Homebrew formula. Test the packaged binary,
  clean macOS arm64 install, 0.2.1-to-0.2.2 upgrade and rollback with
  `python3 scripts/test_install_upgrade_rollback.py --candidate-dir /path/to/0.2.2-candidate --baseline-dir /path/to/v0.2.1-assets`.
- Use only existing user-authorized production credentials for limited live
  client checks. Do not read or print provider secrets or billing details.

After every gate passes, prepare the concrete source and artifact candidate with
its validation evidence for user review. Do not merge, tag, publish, upload
release assets, update the Homebrew tap, or announce availability until the user
explicitly authorizes those actions. If any gate fails, keep the release goal
open and report the failure.
