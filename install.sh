#!/bin/sh
# Installs the ts client (tamsyam) from the latest GitHub release.
#   curl -fsSL https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.sh | sh
# Env: TS_VERSION=0.1.0 to pin a version, TS_INSTALL_DIR to change the target dir.
set -eu

REPO="Mi113r/tamsyam"
BIN_DIR="${TS_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "install: $*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux|darwin) ;; *) fail "unsupported OS: $os" ;; esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

version="${TS_VERSION:-}"
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || fail "cannot determine the latest release"
fi
version="${version#v}"

archive="tamsyam_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/v$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading ts $version ($os/$arch)..."
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d ' ' -f 1)
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | cut -d ' ' -f 1)
else
  actual=$(shasum -a 256 "$tmp/$archive" | cut -d ' ' -f 1)
fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || fail "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" ts
mkdir -p "$BIN_DIR"
install -m 755 "$tmp/ts" "$BIN_DIR/ts"
echo "Installed $("$BIN_DIR/ts" version) to $BIN_DIR/ts"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "Add $BIN_DIR to your PATH, e.g.: echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.profile" ;;
esac
