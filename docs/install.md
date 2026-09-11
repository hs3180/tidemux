# Installation

TideMux 0.1.0 targets macOS 15+ on Apple Silicon. Install your coding client
separately; TideMux does not install Claude Code, Kilo CLI or Hermes Agent.

## Homebrew (recommended after publication)

```sh
brew install hs3180/tap/tidemux
```

The GitHub Release and `hs3180/homebrew-tap` are not published yet. Until they
are available, build from a local source checkout below.

Upgrade with `brew upgrade tidemux`; uninstall with `brew uninstall tidemux`.

## Install without Homebrew

After version 0.1.0 is published:

```sh
curl -fsSL https://raw.githubusercontent.com/hs3180/tidemux/v0.1.0/scripts/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer verifies the archive's SHA256 checksum and installs the binary in
`~/.local/bin` without sudo. Add the PATH line to `~/.zshrc` for new terminals.
Reinstalling preserves your configuration and Keychain credentials.

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

Add the PATH line to `~/.zshrc` for new terminals. Return to the
[quick start](../README.md#quick-start) to configure your provider and client.
