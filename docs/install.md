# Installation

TideMux targets macOS 15+ on Apple Silicon. The current public stable release is
**v0.3.1**; **0.3.2 is an unpublished candidate**. The installation commands below
select the public stable release. A version-pinned 0.3.2 URL becomes usable only
after its exact assets are published and verified. Use a local checksum-verified
archive for candidate testing; see the [0.3.2 release guide](release-0.3.2.md).
Install your coding client
separately; TideMux does not install Claude Code, Kilo CLI or Hermes Agent.

## Homebrew (recommended)

```sh
brew install hs3180/tap/tidemux
```

If Homebrew asks you to trust the formula, run
`brew trust --formula hs3180/tap/tidemux`, then retry the install command.

The formula downloads the verified macOS arm64 archive from
[GitHub Releases](https://github.com/hs3180/tidemux/releases).

Upgrade with `brew upgrade tidemux`; uninstall with `brew uninstall tidemux`.

## Install without Homebrew

Install version 0.3.1:

```sh
curl -fsSL https://raw.githubusercontent.com/hs3180/tidemux/v0.3.1/scripts/install.sh -o /tmp/tidemux-install.sh
less /tmp/tidemux-install.sh
TIDEMUX_VERSION=0.3.1 sh /tmp/tidemux-install.sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer verifies the archive's SHA256 checksum and installs the binary in
`~/.local/bin` without sudo. Add the PATH line to `~/.zshrc` for new terminals.
Reinstalling preserves your configuration and Keychain credentials.

To run a locally built package, extract its archive under `dist/` and run the
included `tidemux` binary directly. Verify the archive against its accompanying
`SHA256SUMS`.

Set `TIDEMUX_INSTALL_DIR` to choose another destination, or `TIDEMUX_VERSION` to
select a published version. Review the version-pinned installer before running
it. The installer does not change configuration or Keychain credentials.

## Upgrade and rollback

Before a 0.3.1 → 0.3.2 upgrade, preserve the old executable, config, launchd plist,
consumer settings and a consistent SQLite backup. Use SQLite's backup API;
copying only an active `.db` can miss committed WAL records. Preserve the usage
identity key, status and dedicated JSONL/checkpoint directory if export is enabled.
Do not modify Filebeat registry or delete accounting/export data. Stop the gateway
and exporter before replacing the executable, then check the accepted binary
hash, `version`, `doctor`, authenticated `gateway check`, configuration application
and retained ledger history. Candidate testing does not authorize a production
replacement. See [upgrade compatibility](upgrade-compatibility.md).

An emergency rollback to **0.3.1** must remove the new top-level `usage_log` and
each provider's `max_active_sessions`; the old strict decoder rejects either
field even when its value disables the feature. The gateway-wide
`max_active_sessions` is an older field and remains unchanged. The candidate
archive supplies a Python 3 helper (the installer installs only the binary):

```sh
python3 /path/to/extracted/tidemux-0.3.2/tools/prepare_rollback.py \
  --input /private/path/config-0.3.2.json \
  --output /private/path/config-rollback-0.3.1.json
```

The output must be a new file. The helper creates it with mode 0600, preserves
the source, retains all other settings and Keychain references, and prints only
removed field names/counts. Review it, stop the gateway/exporter, and activate
that converted configuration with the retained 0.3.1 executable. Keep the
original 0.3.2 config for re-upgrade. Ensure service `--config` and CLI default
configuration point to the intended converted configuration.

Keep the latest ledger, usage key and export files. Older reports omit the new
optional audit metadata while the stored JSON remains intact; requests made
during rollback cannot acquire the newer session/source metadata retroactively.
Provider caps and JSONL export are disabled, and 0.3.1 restores its earlier
reload/session and client-recovery behavior. The same accepted combined archive
must pass the [upgrade/rollback gate](release-0.3.2.md#upgrade-and-rollback-gate)
before compatibility is certified. Never restore a pre-upgrade ledger over
newer records or remove `.gateway.lock` while a process owns it.

For the older 0.3.1 → 0.3.0 emergency path, use the
[0.3.1 release guide](release-0.3.1.md). Earlier versions restore their known
report/cache/tool-history limitations.

### Returning to versions before 0.3.0

TideMux 0.3.0 keeps the existing provider profiles, Keychain references,
budgets and audit ledger. It adds optional top-level `auto_chain` and `routing`
fields; both are omitted while routing is disabled. Back up the config before
an upgrade as part of your normal local-data practice. If you enable routing
and later want to return to 0.2.2, clear the instance chain and disable the
routing settings:

```sh
tidemux auto-chain clear
tidemux routing set --shared-model-strategy off --billing-exhaustion-failover=false
```

The config then contains no 0.3.0 routing fields and can be read by v0.2.2.
The ledger and Keychain entries remain usable. If you added a `temporarily_unavailable`
error mapping, remove that mapping before v0.2.2 rollback because the older
binary does not recognize the new category:

```sh
tidemux provider error-map list REF
tidemux provider error-map remove REF --code CODE [--status STATUS]
```

To return to 0.2.0, remove every `error_code_mappings` entry; 0.2.0 rejects
that newer config field. Then download and inspect the installer from the
`v0.2.0` tag and run it with
`TIDEMUX_VERSION=0.2.0`. To return to 0.2.1, use its version-pinned installer.
That version requires provider-qualified model IDs, lacks the post-tag budget
recovery and audit settlement fixes, and restores all-or-none startup behavior
for `protocol:auto`; configure ambiguous providers explicitly before restarting
0.2.1.

## Build from source

From a local source checkout containing `go.mod`, with Go 1.27+ installed:

```sh
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -o "$HOME/.local/bin/tidemux" ./cmd/tidemux
export PATH="$HOME/.local/bin:$PATH"
tidemux version
```

Add the PATH line to `~/.zshrc` for new terminals. For configuration and client
setup on this source build, use the [provider and gateway CLI guide](provider-cli.md)
and [client setup](clients.md).

For clickable daily-report notifications, install the small macOS notification
helper once:

```sh
brew install terminal-notifier
```
