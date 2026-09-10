#!/usr/bin/env bash
# Copyright (c) 2026 Digital X Co., Ltd.
#
# SPDX-License-Identifier: Apache-2.0
#
# Pack one built binary into an MCP Bundle (.mcpb) — a Desktop Extension that
# installs `dgx-cli mcp serve` into Claude Desktop (and other MCPB hosts) with a
# drag-and-drop, no-terminal flow. Invoked by GoReleaser as a per-target
# post-build hook (see .goreleaser.yaml), after the macOS signing hook, so the
# binary we pack is the signed one. Notarization happens later (a separate step)
# and does not alter the binary bytes, so the packed binary stays the notarized
# one.
#
# An .mcpb is just a ZIP with a manifest.json at the root (the MCPB spec lives at
# https://github.com/anthropics/mcpb) — no Node tooling required. We ship ONE
# bundle per platform/arch (each carries a single binary) rather than one fat
# multi-platform bundle. The bundle is unsigned at the .mcpb level; the macOS
# binary inside is already code-signed + notarized, which is what Gatekeeper
# verifies on first run.
#
# Inputs (the path is GoReleaser's {{ .Path }}; the rest come from env):
#   $1                 path to the freshly built binary
#   GOOS  GOARCH       target platform/arch ({{ .Os }} / {{ .Arch }})
#   VERSION            release version, semver ({{ .Version }})
#
# Output: dist/digitalx_<goos>_<goarch>.mcpb (dist/ is inferred from the binary
# path, matching how GoReleaser lays out its build directories).
set -euo pipefail

bin="${1:?usage: build-mcpb.sh <binary-path>}"
goos="${GOOS:?build-mcpb: GOOS must be set}"
goarch="${GOARCH:?build-mcpb: GOARCH must be set}"
version="${VERSION:-0.0.0}"

# Fixed timezone so the mtimes we stamp below (and zip's DOS-time conversion of
# them) are independent of the build host's clock zone.
export TZ=UTC

if [ ! -f "$bin" ]; then
	echo "build-mcpb: ERROR binary not found: $bin" >&2
	exit 1
fi
if ! command -v zip >/dev/null 2>&1; then
	echo "build-mcpb: ERROR 'zip' not found on PATH (required to pack the .mcpb)" >&2
	exit 1
fi

# Repo root: this script lives in <root>/scripts, and GoReleaser runs hooks from
# the project root, but resolve it from the script path so it is invariant.
root="$(cd "$(dirname "$0")/.." && pwd)"

# dist/ is two levels up from the binary (dist/dgx-cli_<os>_<arch>.../dgx-cli).
dist="$(cd "$(dirname "$bin")/.." && pwd)"
out="$dist/digitalx_${goos}_${goarch}.mcpb"

# Map the Go target to the MCPB platform token and the on-disk binary name.
# MCPB uses Node's process.platform values: win32 (not "windows").
binname="dgx-cli"
platform="$goos"
case "$goos" in
windows)
	binname="dgx-cli.exe"
	platform="win32"
	;;
darwin) platform="darwin" ;;
linux) platform="linux" ;;
*)
	echo "build-mcpb: ERROR unsupported GOOS: $goos" >&2
	exit 1
	;;
esac

# ${__dirname} is an MCPB host substitution (the install directory) — it MUST
# reach the manifest verbatim, so keep it inside a single-quoted assignment and
# never let the shell try to expand it. (A heredoc inserts a variable's value
# literally, without re-expansion.)
entry_point="server/$binname"
command_path='${__dirname}/server/'"$binname"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

mkdir -p "$stage/server"
cp "$bin" "$stage/server/$binname"
chmod +x "$stage/server/$binname"
cp "$root/assets/digitalx.png" "$stage/icon.png"
# The bundle carries a copy of the binary, which statically links open-source
# modules, so it ships the same license/notice/disclaimer set as the release
# archives — attribution travels with every distributed copy. These sit at the
# bundle root alongside manifest.json; the MCPB spec only requires manifest.json
# there, so extra top-level files are fine.
for doc in LICENSE NOTICE THIRD_PARTY_LICENSES.txt DISCLAIMER.md DISCLAIMER.ko.md README.md README.ko.md; do
	cp "$root/$doc" "$stage/$doc"
done

# `name` is the extension's INSTALL KEY and `display_name` is what the user
# reads; both carry the product's name. A host keys the installed extension on
# `name`, so a host that already holds a bundle under a different key shows this
# one as a SECOND extension side by side — the two do not merge, and the user
# removes the one they no longer want by hand. That is the accepted behaviour: a
# Desktop Extension has no self-update and no migration path, so there is nothing
# for a stable key to carry forward, and the name a newcomer reads in the
# extension list matters more than continuity with a key they never see.
cat >"$stage/manifest.json" <<EOF
{
  "manifest_version": "0.3",
  "name": "digitalx",
  "display_name": "Digital X",
  "version": "$version",
  "description": "Operate the Digital X cryptocurrency exchange over MCP — every REST endpoint as a tool, with the same validation, signing, journaling, and retries as the CLI.",
  "long_description": "Exposes the Digital X Open API v2 as MCP tools backed by the dgx-cli binary running locally on your machine, so your API keys never leave it. Read market data, manage orders, and check balances; the order-placement tool supports a dry-run that simulates the fill against the live order book before anything is sent. First-time users with no key yet can complete setup entirely in chat via the setup and doctor tools.",
  "author": { "name": "Digital X Co., Ltd.", "url": "https://digitalx.miraeasset.com" },
  "homepage": "https://developers.digitalx.miraeasset.com/",
  "documentation": "https://developers.digitalx.miraeasset.com/",
  "repository": { "type": "git", "url": "https://github.com/digitalx-official/digitalx-cli.git" },
  "license": "Apache-2.0",
  "icon": "icon.png",
  "keywords": ["digitalx", "digitalx-cli", "dgx-cli", "korbit", "cryptocurrency", "exchange", "trading", "mcp"],
  "server": {
    "type": "binary",
    "entry_point": "$entry_point",
    "mcp_config": {
      "command": "$command_path",
      "args": ["mcp", "serve"],
      "env": {
        "DIGITALX_CLI_MCP_READ_ONLY": "\${user_config.read_only}",
        "DIGITALX_CLI_MCP_MULTI_KEY": "\${user_config.multi_key}"
      }
    }
  },
  "user_config": {
    "read_only": {
      "type": "boolean",
      "title": "Read-only mode",
      "description": "Expose only read tools (prices, balances, order status, fills). Hides every order, withdrawal, and transfer tool, so the server cannot move funds. Off by default.",
      "default": false
    },
    "multi_key": {
      "type": "boolean",
      "title": "Allow multiple accounts (multi-key)",
      "description": "Add an optional account-key argument to each authenticated tool so one server can act on any of your configured keys. Use with caution: the AI agent picks the key and can make a mistake, acting on the wrong Digital X account or API key — and keys may carry different permissions. Leave off to pin the server to your default key. Off by default.",
      "default": false
    }
  },
  "tools_generated": true,
  "compatibility": { "platforms": ["$platform"] }
}
EOF

# Best-effort manifest validity check (no hard dependency on python3).
if command -v python3 >/dev/null 2>&1; then
	if ! python3 -m json.tool "$stage/manifest.json" >/dev/null; then
		echo "build-mcpb: ERROR generated manifest.json is not valid JSON" >&2
		exit 1
	fi
fi

rm -f "$out"
# Normalize every staged file's mtime so the archive is byte-reproducible: zip
# embeds each file's modification time, which would otherwise be the (varying)
# copy time and change the bundle's hash on every build for identical inputs.
find "$stage" -exec touch -t 200001010000 {} +
# -X drops uid/gid and extra attributes; -9 = max compression (matches how the
# MCPB CLI packs). With the mtimes normalized above, the output is reproducible.
(cd "$stage" && zip -q -r -X -9 "$out" .)

echo "build-mcpb: wrote $out" >&2
