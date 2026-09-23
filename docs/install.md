# Installation

TideMux 0.1.1 targets macOS 15+ on Apple Silicon. Install your coding client
separately; TideMux does not install Claude Code, Kilo CLI or Hermes Agent.

## Homebrew (recommended)

```sh
brew install hs3180/tap/tidemux
```

If Homebrew asks you to trust the formula, run
`brew trust --formula hs3180/tap/tidemux`, then retry the install command.

The Homebrew and pinned-download instructions here install the 0.1.1 release.
The formula downloads the verified macOS arm64 archive from
[GitHub Releases](https://github.com/hs3180/tidemux/releases).

Upgrade with `brew upgrade tidemux`; uninstall with `brew uninstall tidemux`.

## Install without Homebrew

Install version 0.1.1:

```sh
curl -fsSL https://raw.githubusercontent.com/hs3180/tidemux/v0.1.1/scripts/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer verifies the archive's SHA256 checksum and installs the binary in
`~/.local/bin` without sudo. Add the PATH line to `~/.zshrc` for new terminals.
Reinstalling preserves your configuration and Keychain credentials. For the
released binary's setup steps, see the [0.1.1 demo guide](demo.md#recommended-configure-with-the-cli).

Set `TIDEMUX_INSTALL_DIR` to choose another destination, or `TIDEMUX_VERSION` to
select a published version. Pass these environment variables to `sh` when using
the pipeline above.

## Build from source

From a local source checkout containing `go.mod`, with Go 1.27+ installed:

```sh
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -o "$HOME/.local/bin/tidemux" ./cmd/tidemux
export PATH="$HOME/.local/bin:$PATH"
tidemux version
```

Add the PATH line to `~/.zshrc` for new terminals. For a source build from this
development branch, use the [provider and gateway CLI guide](provider-cli.md).
The published 0.1.1 binary continues to use the release-specific legacy setup
documented in the demo guide.

For clickable daily-report notifications, install the small macOS notification
helper once:

```sh
brew install terminal-notifier
```
