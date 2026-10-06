# TideMux 0.3.2 candidate guide

**Unpublished candidate. Public stable remains
[v0.3.1](https://github.com/hs3180/tidemux/releases/tag/v0.3.1)**, source
`4886a58acd1ad5d12e1044b972e700b94082ada1`. The version number in a branch or
local archive does not mean a release is available. Record each scope's latest
PR/SHA, human review, source checks and combined-package acceptance separately.
No agent/CI approval replaces human review or authorizes merging or publishing.

The required scopes are #115 reload/session priority, #79 provider capacity,
#116 usage export consumed by the actual pinned ccusage adapter, #100 early
startup JSON events, #117 malformed-tool-history client recovery, and current
documentation/client acceptance. #102 remains independent and does not block
these scopes; it must not be described as delivered unless independently verified.

## Source and combined package

From the final committed checkout, run the source checks in
[releasing.md](releasing.md), including tests/vet/race, provider PTY, installer,
rollback-helper and license checks. Build with a unique unpublished ID:

```sh
python3 scripts/release.py --build-id commit
```

Verify every `SHA256SUMS` entry, extract once into a clean directory, verify
`BUILD.txt` identifies the expected clean source, and retain binary/archive
SHA256. The archive includes the operator rollback helper under `tools/` and
the pinned optional consumer adapter source under `integrations/ccusage/`.
The ccusage executable has its own source/patch/lockfile/license provenance and
build acceptance; JSONL existence alone does not prove consumer compatibility.

Run test scripts from that same **source checkout**, passing the extracted
binary. Scripts are not assumed to be bundled. The final binary must pass:

```sh
python3 scripts/test_startup_logs_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_routing_reload_package.py --binary /path/to/extracted/tidemux
python3 scripts/test_provider_sessions_package.py --binary /path/to/extracted/tidemux
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
python3 scripts/test_client_reliability_package.py \
  --binary /path/to/extracted/tidemux --long-stream-seconds 65
python3 scripts/test_ccusage_package.py --binary /path/to/extracted/tidemux \
  --ccusage /path/to/installed/ccusage --evidence-dir /private/usage-evidence
python3 scripts/test_client_recovery_package.py --binary /path/to/extracted/tidemux \
  --client claude --client kilo --client hermes --capacity-scope both \
  --evidence /private/client-recovery-evidence
```

Current real Claude Code/Kilo/Hermes versions must also complete read/edit/test,
continuation, malformed-history recovery and capacity-refusal/recovery against
the final package. Record native and supported conversion protocols, buffered/
SSE/tool workflows, client/model versions and the distinction between real CLI,
loopback upstream and paid provider. Older 0.1.1 matrices are historical evidence.
Run collector verification against an explicitly isolated destination, including
startup/refusal/HTTP-200 stream errors, multiple events for one request ID and
parse failures. Preserve namespace, event-time and type contracts; usage files
must remain separate from operational logs.

## Upgrade and rollback gate

Verify/download the public v0.3.1 manifest and BUILD anonymously. Its arm64
archive SHA256 is
`3542be1640dbbec8ecb39bbb373545b8f0ae7561fc5517228b9acf7db5f461e1`.
Then run both installation and actual ledger/config compatibility gates:

```sh
python3 scripts/test_install_upgrade_rollback.py \
  --baseline-dir /path/to/public-0.3.1-assets --baseline-version 0.3.1 \
  --candidate-dir /path/to/candidate-assets --candidate-version 0.3.2
python3 scripts/test_upgrade_rollback_package.py \
  --baseline-dir /path/to/public-0.3.1-assets \
  --candidate-dir /path/to/candidate-assets --candidate-version 0.3.2 \
  --candidate-source FULL_COMMIT_SHA --evidence-dir /private/rollback-evidence
```

The second gate executes public 0.3.1 → candidate → public 0.3.1 in isolated
synthetic state, verifies both protocols, new provider caps and JSONL metadata,
reproduces each strict-decoder incompatibility, invokes the **packaged** rollback
helper, and checks retained references/history/price snapshots, backup integrity
and unique dispatch/audit/budget totals. Config conversion disables only the new
provider caps/export. Usage identity/status/output/checkpoint files remain intact.
Its optional `--candidate-binary` mode is development preview only and explicitly
does not count as package acceptance.

See [upgrade compatibility](upgrade-compatibility.md) for removed fields and
metadata boundaries. A same-Mac isolated profile is not a second physical Mac.
Any commit after acceptance requires a new immutable candidate and rerun of the
final affected/full gates. Never overwrite published v0.3.1 tags or assets.

## Publication and production

Keep candidate-passed, waiting-for-review, merged, published and production-
verified stages distinct. Publish only after required latest-commit human review
and explicit authorization, using the exact accepted assets and matching tap.
Verify anonymous downloads, pinned installer and isolated Homebrew installation,
doctor/gateway/persistence/uninstall data retention before claiming public delivery.

A production replacement requires its own authorization and consistent binary/
config/plist/consumer/ledger backup, including private usage identity/export state.
Preserve Filebeat registry. Verify actual launchd/GUI Keychain, applied view,
provider capacity, usage consumer, retained history and fresh ES events with the
accepted hash. Missing authorization or failed external/client gates remain
pending; a local archive is not a public release or production deployment.
