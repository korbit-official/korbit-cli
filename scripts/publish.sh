#!/usr/bin/env bash
# Copyright (c) 2026 Korbit Inc.
#
# SPDX-License-Identifier: Apache-2.0
#
# Publish the already-built release artifacts in dist/ to a GitHub Release with
# the `gh` CLI. This is deliberately separate from `make release` (which never
# contacts GitHub): build + sign + notarize first and verify locally, then
# publish here.
#
# Target repository: the current checkout's default (`gh` auto-detects from the
# git remote). Override it with KORBIT_RELEASE_REPO=<owner>/<name> to publish the
# SAME built artifacts to another repo — e.g. a staging repo for review, then the
# public repo.
#
# HEAD must be on a tag — that tag is the release version. Requires `gh`
# authenticated (`gh auth login`, or GH_TOKEN/GITHUB_TOKEN in CI). Re-running for
# a release that already exists re-uploads the assets (--clobber).
set -euo pipefail

dist="${1:-dist}"

tag="$(git describe --tags --exact-match 2>/dev/null || true)"
if [ -z "$tag" ]; then
	echo "publish: ERROR HEAD is not on a tag; tag the release commit first" >&2
	exit 1
fi

# Optional explicit target repo. Built as an array so it expands to nothing when
# unset — note the bash-3.2-safe "${arr[@]+…}" guard (a plain "${arr[@]}" on an
# empty array trips `set -u` in macOS's bash 3.2).
repo_args=()
if [ -n "${KORBIT_RELEASE_REPO:-}" ]; then
	repo_args=(--repo "$KORBIT_RELEASE_REPO")
fi
target="${KORBIT_RELEASE_REPO:-the current repository}"

# Collect the release artifacts — archives, the .mcpb Desktop Extensions, the
# filled install scripts, checksums, and the signature — not the intermediate
# per-target build directories. The `[ -e ]` guard drops any glob that matched
# nothing (e.g. checksums.txt.sig when signing was skipped).
assets=()
for f in "$dist"/*.tar.gz "$dist"/*.zip "$dist"/*.mcpb "$dist"/install.sh "$dist"/install.ps1 "$dist"/checksums.txt "$dist"/checksums.txt.sig; do
	[ -e "$f" ] && assets+=("$f")
done
if [ ${#assets[@]} -eq 0 ]; then
	echo "publish: ERROR no release artifacts in $dist/ (run 'make release' first)" >&2
	exit 1
fi

if gh release view "$tag" "${repo_args[@]+"${repo_args[@]}"}" >/dev/null 2>&1; then
	echo "publish: release $tag exists on $target; re-uploading ${#assets[@]} assets (--clobber)" >&2
	gh release upload "$tag" "${repo_args[@]+"${repo_args[@]}"}" --clobber "${assets[@]}"
else
	# Empty release body: --notes "" keeps gh non-interactive (no notes flag
	# would open an editor) and skips the auto-generated changelog.
	create_args=(--notes "")
	# A tag with a pre-release segment (e.g. v1.2.3-rc1) is marked pre-release.
	case "$tag" in
	*-*) create_args+=(--prerelease) ;;
	esac
	echo "publish: creating release $tag on $target with ${#assets[@]} assets" >&2
	gh release create "$tag" "${repo_args[@]+"${repo_args[@]}"}" "${create_args[@]}" "${assets[@]}"
fi

echo "publish: done ($tag -> $target)" >&2
