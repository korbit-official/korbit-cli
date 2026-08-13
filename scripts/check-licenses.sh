#!/usr/bin/env bash
# Copyright (c) 2026 Korbit Inc.
#
# SPDX-License-Identifier: Apache-2.0
#
# Fail if THIRD_PARTY_LICENSES.txt is out of date with the current dependency
# set. Runs as a release gate (a GoReleaser `before` hook) and behind `make
# licenses-check`, so a dependency change that was not followed by `make
# licenses` can never ship stale or missing third-party attribution.
#
# It regenerates the notices into a temp file and diffs; a mismatch (or a
# missing committed file) exits non-zero, which aborts the release build.
set -euo pipefail

# Resolve the repo root from this script's path so the check is invariant to the
# working directory GoReleaser or make invokes it from.
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

go run ./tools/licensegen -o "$tmp"

if ! diff -u THIRD_PARTY_LICENSES.txt "$tmp"; then
	echo "check-licenses: THIRD_PARTY_LICENSES.txt is stale — run 'make licenses' and commit the result" >&2
	exit 1
fi
