# Installation

The published TideMux v0.2.2 targets macOS 15+ on Apple Silicon. This checkout
builds a local v0.3.0 candidate that is not published to GitHub Releases or
Homebrew. Install your coding client separately; TideMux does not install
Claude Code, Kilo CLI or Hermes Agent.

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

Install the currently published version, 0.2.2:

```sh
curl -fsSL https://raw.githubusercontent.com/hs3180/tidemux/v0.2.2/scripts/install.sh -o /tmp/tidemux-install.sh
less /tmp/tidemux-install.sh
TIDEMUX_VERSION=0.2.2 sh /tmp/tidemux-install.sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer verifies the archive's SHA256 checksum and installs the binary in
`~/.local/bin` without sudo. Add the PATH line to `~/.zshrc` for new terminals.
Reinstalling preserves your configuration and Keychain credentials.

For a local 0.3.0 candidate, extract the archive under `dist/` and run the
included `tidemux` binary directly. The candidate formula and archive are
review artifacts; their GitHub Release URL is not live until a separately
authorized release is published.

Set `TIDEMUX_INSTALL_DIR` to choose another destination, or `TIDEMUX_VERSION` to
select a published version. Review the version-pinned installer before running
it. The installer does not change configuration or Keychain credentials.

## Upgrade and rollback

The 0.3.0 candidate keeps the existing provider profiles, Keychain references,
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
