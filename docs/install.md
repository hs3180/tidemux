# Installation

TideMux v0.3.1 targets macOS 15+ on Apple Silicon. Install your coding client
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

TideMux 0.3.1 preserves the existing config and SQLite table formats. Before
upgrading, back up your config, launchd plist, old executable and ledger using
SQLite's online backup API. Stop the old gateway before replacing its binary,
then restart it and verify `tidemux version`, `tidemux doctor` and
`tidemux gateway check`. See the [0.3.1 release guide](release-0.3.1.md) for the
full integrity and service checks.

For an emergency rollback to 0.3.0, stop the gateway and restore the retained
0.3.0 executable, or inspect that version's installer and run it with
`TIDEMUX_VERSION=0.3.0`. Keep the current ledger and config; do not restore an
older ledger over newer accounting records. The 0.3.0 binary can read them,
but restores its known live-report, prompt-cache and tool-history defects.
The `.gateway.lock` sidecar belongs to 0.3.1's running gateway; do not remove
it while that process owns the ledger.

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
