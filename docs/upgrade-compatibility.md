# Upgrade and rollback compatibility

0.3.2 is an unpublished candidate; public stable assets remain v0.3.1. This
document describes the compatibility contract and required verification, not a
claim that an unbuilt or untested combination has passed. Use `BUILD.txt` and
archive/binary SHA256 to identify the exact candidate.

| State | 0.3.2 candidate | Rollback to 0.3.1 |
| --- | --- | --- |
| Existing provider/routing/global settings | Retained | Retained; keep gateway-wide `max_active_sessions` |
| Provider `max_active_sessions` | Independent provider conversation cap | Remove the field from every profile; caps are disabled |
| Top-level `usage_log` | Optional startup-time export | Remove the entire field; export is disabled |
| Keychain references | Same service/account references | Preserve them; do not recreate or erase credentials |
| SQLite audit/budget/history | Existing tables; optional `session_group` / `usage_source` in audit JSON | Older reader ignores optional metadata; historical JSON, prices and settled budgets must remain intact |
| `.usage-key`, usage status, JSONL, checkpoint and lock files | Private export state | Preserve files; 0.3.1 does not produce new usage export |
| Routing affinity and active-session counters | Process-local state | Restart resets memory; it does not delete ledger rows |

The strict 0.3.1 configuration decoder rejects `usage_log` and provider-level
`max_active_sessions` even when disabled or zero. `scripts/prepare_rollback.py`
creates a new mode-0600 file that removes exactly those two extensions. It refuses
existing destinations, including source aliases, and ambiguous JSON with
duplicate keys. It never opens the Keychain or ledger, changes the source file,
or prints configuration values. Unknown unrelated fields are retained; the
helper does not certify arbitrary future configuration formats. Run the old
binary's `provider list` / `doctor` against the converted configuration before
activating it.

The archive installs that standalone helper as `tools/prepare_rollback.py`;
Python 3 is required. Homebrew includes it under its `share/tidemux/tools`
directory. The shell installer installs the executable only, so use the helper
from the separately extracted archive or matching source checkout. Test
harnesses live in the source checkout and are not assumed to exist in an archive.

Stop the gateway and exporter before switching executable/configuration. Keep
both original and converted configurations, the old binary, plist and collector/
consumer settings. Keep a consistent latest SQLite backup using the backup API,
not an unsynchronized `.db` copy without its WAL. Preserve the private usage
identity key and the dedicated export directory together. Do not clear Filebeat
registry, reset indices, delete export files or truncate user history as part of
rollback.

The compatibility gate reads the same upgraded ledger using the actual public
0.3.1 executable, checks all prior audit JSON remains byte-for-byte unchanged,
compares historical token/cost/price fields through the old billing command,
and verifies subsequent old-version requests still settle once. It also reads
a latest consistent backup through the old binary. If either path fails, the
candidate is blocked; a pre-upgrade snapshot that hides newer accounting rows
does not satisfy rollback acceptance.

The format boundary is deliberate: old billing responses omit newer metadata,
and rows created by 0.3.1 have no persisted pseudonymous session group. Exporting
those rows after a later re-upgrade cannot infer a raw session identity that was
never stored. Historical JSONL is preserved, but 0.3.1 adds no new export rows.
Keeping `.usage-key` maintains grouping for future candidate requests; resetting
it would create another source/group namespace. Basic history and monetary
records remain the accounting truth, independent of optional export.

Rollback restores 0.3.1 behavior, including the earlier auto-chain reload/session
and client-recovery limitations. A new profile on the same Mac is isolated
configuration evidence, not verification on another physical machine. See the
[release gates](release-0.3.2.md) for the source/archive/client evidence required
before release or production upgrade.
