#!/bin/sh
# Installs the infinitic binary from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/stevejkang/infinitic/main/install.sh | sh
#
# INFINITIC_VERSION  release to install, e.g. 0.4.0 or 0.4.0-rc1 (default: the latest release)
# INSTALL_DIR        where to put the binary (default: ~/.local/bin)
set -eu

repo=stevejkang/infinitic
dir=${INSTALL_DIR:-$HOME/.local/bin}

fail() { echo "infinitic install: $*" >&2; exit 1; }

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS $(uname -s); infinitic runs on Linux and macOS" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac
command -v curl >/dev/null || fail "curl is required"

version=${INFINITIC_VERSION:-}
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
  [ -n "$version" ] || fail "could not find the latest release of $repo"
fi
version=${version#v}

archive="infinitic_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/v$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading infinitic $version ($os/$arch)"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || fail "no $archive in release v$version"
curl -fsSL -o "$tmp/checksums.txt" "$base/infinitic_${version}_checksums.txt" || fail "no checksums in release v$version"

want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null; then
  got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
fi
[ -n "$want" ] && [ "$want" = "$got" ] || fail "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" infinitic
mkdir -p "$dir"
mv "$tmp/infinitic" "$dir/infinitic"
chmod 755 "$dir/infinitic"

echo "installed $dir/infinitic"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "add $dir to your PATH, e.g. echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac
