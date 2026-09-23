#!/bin/sh
# Install a verified release from https://github.com/hs3180/tidemux.
set -eu

fail() { printf '%s\n' "tidemux: $*" >&2; exit 1; }
[ "$(uname -s)" = Darwin ] || fail 'macOS is required.'
[ "$(uname -m)" = arm64 ] || fail 'Apple Silicon (arm64) is required.'
major=$(sw_vers -productVersion | cut -d . -f 1)
[ "$major" -ge 15 ] || fail 'macOS 15 or newer is required.'

version=${TIDEMUX_VERSION:-0.1.1}
case "$version" in ''|*[!0-9A-Za-z._-]*) fail 'Invalid TIDEMUX_VERSION.' ;; esac
install_dir=${TIDEMUX_INSTALL_DIR:-"$HOME/.local/bin"}
base="https://github.com/hs3180/tidemux/releases/download/v$version"
work=$(mktemp -d)
staged=
trap 'rm -rf "$work"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT
trap 'exit 1' HUP INT TERM

fetch() {
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    "$base/$1" -o "$work/$1" || fail "Cannot download $1. Check that release v$version is published at https://github.com/hs3180/tidemux/releases."
}
fetch SHA256SUMS
# Accept both plain release filenames and immutable commit-suffixed builds.
awk -v prefix="tidemux_${version}" '
  length($1) == 64 && $1 !~ /[^0-9a-f]/ && NF == 2 &&
  index($2, prefix) == 1 && $2 ~ /^tidemux_[0-9A-Za-z._-]+_darwin_arm64[.]tar[.]gz$/ {
    print $0
  }' "$work/SHA256SUMS" > "$work/selected.sha256"
[ "$(wc -l < "$work/selected.sha256" | tr -d ' ')" = 1 ] || fail 'Release must contain exactly one macOS arm64 archive checksum.'
asset=$(awk '{print $2}' "$work/selected.sha256")
fetch "$asset"
(cd "$work" && shasum -a 256 -c selected.sha256) || fail 'Archive checksum mismatch; existing installation was not changed.'
# Extract only the executable, never arbitrary archive paths.
tar -xOf "$work/$asset" "tidemux-$version/tidemux" > "$work/tidemux" || fail 'Archive does not contain the expected executable.'
chmod 755 "$work/tidemux"
[ "$("$work/tidemux" version)" = "$version" ] || fail 'Executable version does not match release.'
[ ! -d "$install_dir/tidemux" ] || fail 'Install target is a directory; existing contents were not changed.'
mkdir -p "$install_dir"
staged=$(mktemp "$install_dir/.tidemux-install.XXXXXX")
cp "$work/tidemux" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$install_dir/tidemux"
[ -f "$install_dir/tidemux" ] && [ -x "$install_dir/tidemux" ] || fail 'Installed executable is missing.'
[ "$("$install_dir/tidemux" version)" = "$version" ] || fail 'Installed version verification failed.'
staged=
printf '\nInstalled TideMux %s to %s/tidemux\n' "$version" "$install_dir"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'Add this directory to your shell PATH: %s\n' "$install_dir" ;;
esac
printf 'Next: tidemux configure\n'
