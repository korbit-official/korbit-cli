#!/bin/sh
# digitalx-cli installer (Linux / macOS).
#
# This is a TEMPLATE for the release-pinned installer. The runnable copy is
# filled with this release's version + archive checksums and attached to the
# GitHub release, fetchable directly at:
#
#   curl -fsSL https://github.com/digitalx-official/digitalx-cli/releases/latest/download/install.sh | sh
#
# (The README's advertised one-liner instead fetches the evergreen installer
# hosted at https://docs.digitalx.miraeasset.com/install.sh.)
#
# What it does: detect your platform, download that release's archive, verify its
# SHA-256 against the value embedded below, extract it, and hand off to
# `dgx-cli self install`, which places the binary, wires PATH, and writes the
# install manifest. Trust is TLS + SHA-256.
#
# It touches the command layout only: an existing CLI home is left exactly where
# it is (MIGRATION.md covers moving it).
set -eu

# >>> release-pin (filled at release time) >>>
PIN_VERSION=""
# One "<sha256>  <archive-name>" per line (sha256sum format), for this release's
# archives. Empty in the in-repo template.
PIN_SHA256=""
# <<< release-pin <<<

REPO="digitalx-official/digitalx-cli"

err() { echo "install: $*" >&2; exit 1; }

[ -n "$PIN_VERSION" ] || err "this is the in-repo template, not a released installer — install with:
  curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sh"

# --- platform detection (keep the matrix in sync with .goreleaser.yaml) ---
os=$(uname -s)
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (this installer covers Linux and macOS; on Windows use install.ps1)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) err "unsupported architecture: $arch" ;;
esac

# Apple Silicon only: no Intel-mac (darwin/amd64) build exists, and an arm64
# binary can't run under Rosetta. Fail loudly rather than grab the wrong one.
if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
  err "Intel Macs aren't supported — build from source with 'go install github.com/$REPO@latest'"
fi

asset="digitalx-cli_${os}_${arch}.tar.gz"

# The expected hash for this platform's archive, from the embedded pin block.
expected=$(printf '%s\n' "$PIN_SHA256" | awk -v a="$asset" '$2==a {print $1}' | head -n1)
[ -n "$expected" ] || err "no embedded checksum for $asset in this installer"

# --- download ---
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
url="https://github.com/$REPO/releases/download/$PIN_VERSION/$asset"
echo "install: downloading dgx-cli $PIN_VERSION ($os/$arch)…" >&2
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$url" -o "$tmp/$asset" || err "download failed: $url"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$tmp/$asset" "$url" || err "download failed: $url"
else
  err "need curl or wget to download the release"
fi

# --- verify SHA-256 (always enforced; no skip) ---
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
  err "need sha256sum or shasum to verify the download"
fi
[ "$actual" = "$expected" ] || err "checksum mismatch for $asset
  expected $expected
  actual   $actual"

# --- extract and hand off to the binary ---
tar -xzf "$tmp/$asset" -C "$tmp" || err "failed to extract $asset"
[ -f "$tmp/dgx-cli" ] || err "archive did not contain the dgx-cli binary"
chmod +x "$tmp/dgx-cli"

# The binary owns install policy (PATH location, PATH entry, manifest) and is
# reconciling, so this both installs fresh and repairs a broken install. It
# copies itself to its PATH location before returning, so the temp dir (cleaned
# by the EXIT trap) is no longer needed afterward. Run it rather than exec so
# cleanup still fires.
"$tmp/dgx-cli" self install "$@"
