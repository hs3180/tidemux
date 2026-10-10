# TideMux 0.3.1 release guide

Reliability patch, baseline published v0.3.0 (source
`055d98a7375d0b6c2fc626042337829af34fa22f`). The five required fixes are in
[milestone 5](https://github.com/hs3180/tidemux/milestone/5).
The release source is recorded in `BUILD.txt`, with arm64 archive, SPDX SBOM,
formula and checksums in the immutable release assets. Existing published tags
and assets must remain unchanged. Config and SQLite table formats are preserved;
an OS lock sidecar and bounded in-memory state do not migrate accounting data.

## Scope and evidence

| Issue / PR | Result and regression |
| --- | --- |
| [#61](https://github.com/hs3180/tidemux/issues/61), [PR #110](https://github.com/hs3180/tidemux/pull/110), [PR #113](https://github.com/hs3180/tidemux/pull/113) | Automatic provider/scope/key application, one atomic catalog/routing view, accurate acknowledgement, last-valid preservation and admitted SSE price/budget snapshots. Explicit providers without a model-list endpoint retain supported scope and pricing updates. `config_reload_test.go`, `test_hot_reload_package.py`. |
| [#98](https://github.com/hs3180/tidemux/issues/98), [PR #107](https://github.com/hs3180/tidemux/pull/107) | Report commands preserve live reservations; queued cancellation releases without dispatch/latching. Gateway ownership excludes a second process and preserves genuine SIGKILL recovery. `report_reservations_test.go`, ledger ownership tests, `test_report_budget_package.py`. |
| [#99](https://github.com/hs3180/tidemux/issues/99), [PR #108](https://github.com/hs3180/tidemux/pull/108) | 256-entry / 16 MiB / five-minute idle prompt retention, quiet background expiry and no generated-ID retention; prefix estimation and cost fallback preserved. Cache churn/LRU/byte/expiry/concurrency tests and gateway anonymous-request tests. |
| [#106](https://github.com/hs3180/tidemux/issues/106), [PR #109](https://github.com/hs3180/tidemux/pull/109) | 0.3.0 SSE reproduction saved malformed assistant tool_result then returned 400. Withhold malformed web/image blocks and reject poisoned replay with exact paths, both provider protocols, with/without compaction. Valid native/client tool round trips preserved. `tool_history_test.go`, `test_tool_history_package.py`. |
| [#105](https://github.com/hs3180/tidemux/issues/105), [PR #111](https://github.com/hs3180/tidemux/pull/111) | Actual Logstash/ES typed ingestion of packaged stderr; success/rejection/lifecycle/HTTP 200 stream failure searchable; same-request events distinct; parse failures retained and mapping failures read from DLQ. test-only collector fixtures, `test_elasticsearch_package.py`. |

Each bug's failing reproduction was captured before implementation. Source
build, tests, vet, gofmt and race checks remain mandatory. Issues stay open
until the accepted archive, public downloads/installs, tap and local upgrade
have been verified. #100–#104 remain deferred, outside these gates.

## Candidate checks

Build from a clean committed tree with `python3 scripts/release.py`, or use a
unique `--build-id commit` for an unpublished candidate. Verify every manifest
entry and extract into a fresh directory. Run the same binary through:

```sh
python3 scripts/test_hot_reload_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_report_budget_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_tool_history_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_anthropic_server_tools_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_shared_model_affinity_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_auto_chain_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_budget_recovery_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_budget_reservation_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_client_reliability_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_runtime_logs_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_context_management_claude.py --binary /path/to/extracted/tidemux
python3 scripts/test_install_upgrade_rollback.py \
  --candidate-dir /path/to/candidate-assets --candidate-version 0.3.1 \
  --baseline-dir /path/to/public-0.3.0-assets --baseline-version 0.3.0
```

Also run the client reliability gate with `--long-stream-seconds 65` on a real
clock and the packaged PTY provider setup. The Claude gate uses an isolated real
Claude Code CLI against loopback mocks, including native/conversion and
compaction follow-ups. It sends no paid provider request. Run
`scripts/test_elasticsearch_package.py` with an explicit isolated target; this
release smoke exercises the Logstash test-only collector fixture and
is separate from the basic Filebeat example in the README.
Record source commit, binary/archive hashes, test results and index evidence in
private operations records; do not put real configuration or credentials in
public artifacts. The CI archive gates cover normal routing, sessions, budgets,
tools and privacy; the isolated ES smoke is a separate required release check.

## Install, upgrade and limits

Publish only the accepted immutable package, then update the generated formula.
Verify public anonymous downloads and checksums, the actual installer, and a
fresh Homebrew installation with an isolated configuration. Check version,
doctor, authenticated gateway check, persistence, graceful stop and uninstall.
Keep user state when uninstalling. A fresh profile on the release machine is
isolated installation evidence, not a claim of testing a second physical Mac.

Before upgrading the real launchd service, preserve the old executable, config,
plist and consistent ledger backup. Stop the old process, install the exact
accepted hash, start the service, and check real GUI Keychain access, gateway
identity, ledger integrity, unchanged table schema and historical rows. The
ledger/config remain readable by v0.3.0; it retains its known old report/cache/
tool-history defects, so rollback is an emergency path, not an equivalent fix.

Provider application has one-second polling and a ten-second attempt deadline;
failed probes or global startup-setting edits remain pending. See
[provider-cli.md](provider-cli.md#automatic-provider-application-031).
The sidecar lock must remain in place; distinct hard links to one ledger are
unsupported. Cache eviction may reduce local estimated cache hits; upstream
usage has priority. Malformed provider built-in tools remain upstream defects:
use a clean conversation or explicit client repair and client-executed tools;
TideMux never silently repairs stored history. Collector deployment/TLS,
retention and long-outage/rotation guarantees remain operator-owned.
