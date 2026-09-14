#!/usr/bin/env bash
# Copyright (c) 2026 Digital X Co., Ltd.
#
# SPDX-License-Identifier: Apache-2.0
#
# Notarize the signed macOS binaries GoReleaser produced under dist/.
#
# This is a SEPARATE step from the build on purpose: it talks to Apple's notary
# service (slow, network, needs credentials) and is only meaningful for a real,
# published release. A snapshot/test build never runs it. Run it AFTER
# `goreleaser release` (the binaries must already be signed):
#
#   ./scripts/macos-notarize.sh [dist-dir]   # default: dist
#
# Apple notarizes an archive, not a bare executable, so each signed binary is
# zipped, submitted, and waited on. We do NOT staple: a stand-alone CLI binary
# can't carry a stapled ticket, so Gatekeeper verifies the notarization online
# the first time an end user runs a downloaded copy.
#
# Credentials — an App Store Connect API key (issuer id + key id + .p8). Encode
# it once with `rcodesign encode-app-store-connect-api-key <issuer> <key-id>
# AuthKey_<key-id>.p8 > asc-key.json`, then:
#
#   ASC_API_KEY_FILE   path to that JSON file
#
# See RELEASING.md for how to obtain the key.
set -euo pipefail

dist="${1:-dist}"

if [ -z "${ASC_API_KEY_FILE:-}" ]; then
	echo "macos-notarize: ERROR ASC_API_KEY_FILE is not set" >&2
	echo "  encode an App Store Connect API key with rcodesign and point ASC_API_KEY_FILE at it" >&2
	echo "  see RELEASING.md" >&2
	exit 1
fi
if [ ! -f "$ASC_API_KEY_FILE" ]; then
	echo "macos-notarize: ERROR ASC_API_KEY_FILE not found: $ASC_API_KEY_FILE" >&2
	exit 1
fi

# GoReleaser lays each build target out as dist/<id>_<os>_<arch>[...]/<binary>,
# where <binary> is the build's `binary:` value in .goreleaser.yaml — so the name
# matched here must stay in step with that value. Collected with a read loop
# rather than `mapfile`, which doesn't exist in macOS's bash 3.2.
bins=()
while IFS= read -r line; do
	bins+=("$line")
done < <(find "$dist" -type f -name dgx-cli -path '*darwin*')
if [ ${#bins[@]} -eq 0 ]; then
	echo "macos-notarize: ERROR no darwin binaries found under $dist (run goreleaser first)" >&2
	exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for bin in "${bins[@]}"; do
	zip="$work/$(basename "$(dirname "$bin")").zip"
	echo "macos-notarize: zipping $bin" >&2
	# zip from the binary's own directory, naming the member by its own basename,
	# so the archive holds just the binary and nothing else.
	(cd "$(dirname "$bin")" && zip -q -X "$zip" "$(basename "$bin")")
	echo "macos-notarize: submitting $zip" >&2
	rcodesign notary-submit --api-key-file "$ASC_API_KEY_FILE" --wait "$zip"
	echo "macos-notarize: accepted $bin" >&2
done

echo "macos-notarize: notarized ${#bins[@]} binary(ies)" >&2
