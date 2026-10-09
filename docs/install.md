# Installation

TideMux targets macOS 15+ on Apple Silicon. Install your coding client
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

Install version 0.3.2:

```sh
curl -fsSL https://raw.githubusercontent.com/hs3180/tidemux/v0.3.2/scripts/install.sh -o /tmp/tidemux-install.sh
less /tmp/tidemux-install.sh
TIDEMUX_VERSION=0.3.2 sh /tmp/tidemux-install.sh
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

## Verify installation

Check the installed version with `tidemux version`. Configure providers and
the gateway using the [CLI guide](provider-cli.md), then start the gateway.
Run `tidemux doctor` and `tidemux gateway check` to verify configuration,
credential access and the authenticated local endpoint.

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
