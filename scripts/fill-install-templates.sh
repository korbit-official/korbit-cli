#!/usr/bin/env bash
# Copyright (c) 2026 Digital X Co., Ltd.
#
# SPDX-License-Identifier: Apache-2.0
#
# Fill the install-script templates with a release's version + archive checksums,
# producing the runnable installers that are attached to the GitHub release.
#
# The in-repo templates (install/install.{sh,ps1}) carry an empty "release-pin"
# block; this reads the tag and the archive lines from dist/checksums.txt and
# writes dist/install.sh + dist/install.ps1 with the block filled in. Run after
# the archives + checksums exist (a GoReleaser `after` hook / `make release`),
# before scripts/publish.sh uploads dist/*.
#
# Usage: fill-install-templates.sh <tag> [checksums-file] [template-dir] [out-dir]
#   tag: the release tag, e.g. v1.2.3
set -euo pipefail

tag="${1:?usage: fill-install-templates.sh <tag> [checksums] [templates] [out]}"
checksums="${2:-dist/checksums.txt}"
srcdir="${3:-install}"
outdir="${4:-dist}"

[ -f "$checksums" ] || { echo "fill-install: no checksums file at $checksums" >&2; exit 1; }

# The installers download only the platform ARCHIVES (tar.gz / zip); the .mcpb
# Desktop Extensions and checksums.txt itself are not embedded.
shafile="$(mktemp)"
trap 'rm -f "$shafile"' EXIT
grep -E '\.(tar\.gz|zip)$' "$checksums" > "$shafile" || true
[ -s "$shafile" ] || { echo "fill-install: no archive lines in $checksums" >&2; exit 1; }

mkdir -p "$outdir"

# --- install.sh: PIN_VERSION="" and PIN_SHA256="" (a double-quoted string) ---
awk -v tag="$tag" -v shafile="$shafile" '
  BEGIN {
    while ((getline l < shafile) > 0) shas = shas l "\n"
    sub(/\n$/, "", shas)
  }
  /^PIN_VERSION=""$/ { print "PIN_VERSION=\"" tag "\""; next }
  /^PIN_SHA256=""$/  { print "PIN_SHA256=\"" shas "\""; next }
  { print }
' "$srcdir/install.sh" > "$outdir/install.sh"
chmod +x "$outdir/install.sh"

# --- install.ps1: $PIN_VERSION = '' and the $PIN_SHA256 = @' ... '@ here-string ---
awk -v tag="$tag" -v shafile="$shafile" '
  BEGIN {
    while ((getline l < shafile) > 0) shas = shas l "\n"
    sub(/\n$/, "", shas)
  }
  /^\$PIN_VERSION = ..$/ { print "$PIN_VERSION = '\''" tag "'\''"; next }
  /^\$PIN_SHA256 = @.$/  { print; print shas; inhere = 1; next }
  inhere && /^.@$/       { print; inhere = 0; next }
  inhere                 { next }   # drop any prior body (empty in the template)
  { print }
' "$srcdir/install.ps1" > "$outdir/install.ps1"

echo "fill-install: wrote $outdir/install.sh and $outdir/install.ps1 for $tag" >&2

# Sanity: the filled scripts must no longer look like the empty template.
grep -q "PIN_VERSION=\"$tag\"" "$outdir/install.sh" || { echo "fill-install: install.sh version not filled" >&2; exit 1; }
grep -q "\$PIN_VERSION = '$tag'" "$outdir/install.ps1" || { echo "fill-install: install.ps1 version not filled" >&2; exit 1; }
