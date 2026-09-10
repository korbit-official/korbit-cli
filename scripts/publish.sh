#!/usr/bin/env bash
# Copyright (c) 2026 Digital X Co., Ltd.
#
# SPDX-License-Identifier: Apache-2.0
#
# Publish the already-built release artifacts in dist/ to GitHub Releases with
# the `gh` CLI. This is deliberately separate from `make release` (which never
# contacts GitHub): build + sign + notarize first and verify locally, then
# publish here.
#
# A release goes to TWO repositories, because two generations of installed
# binaries look for it under two different names (see RELEASING.md):
#
#   primary  digitalx-cli_<os>_<arch> archives, the .mcpb Desktop Extensions, the
#            filled install.sh/install.ps1, checksums.txt, checksums.txt.sig.
#            This is what the installers and `dgx-cli self update` fetch.
#   legacy   korbit_<os>_<arch> archives plus the SAME checksums.txt and
#            checksums.txt.sig. This is the only route an already-installed
#            `korbit` binary has to update itself: it resolves
#            github.com/<repo>/releases/latest under the org/repo name compiled
#            into it and reads the tag off the final URL, so the release must
#            exist there under the SAME tag.
#
# One checksums file lists every asset of both sets, so each side looks up its
# own entry and the single signature authenticates both.
#
# Target repositories:
#   KORBIT_RELEASE_REPO=<owner>/<name>          primary; default is the current
#                                               checkout's repo (gh auto-detects
#                                               it from the git remote). Set it to
#                                               publish the SAME built artifacts
#                                               to a staging repo for review first.
#   KORBIT_LEGACY_RELEASE_REPO=<owner>/<name>   legacy; default
#                                               korbit-official/korbit-cli. Set it
#                                               to the EMPTY string to skip the
#                                               legacy upload entirely (e.g. when
#                                               publishing to a staging repo).
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

# `-` (not `:-`) so an explicitly empty value disables the legacy upload while an
# unset variable still gets the default.
legacy_repo="${KORBIT_LEGACY_RELEASE_REPO-korbit-official/korbit-cli}"

# Split dist/ into the two asset sets — not the intermediate per-target build
# directories. The `[ -e ]` guard drops any glob that matched nothing (e.g.
# checksums.txt.sig when signing was skipped).
primary=()
legacy=()
for f in "$dist"/*.tar.gz "$dist"/*.zip; do
	[ -e "$f" ] || continue
	case "$(basename "$f")" in
	korbit_*) legacy+=("$f") ;;
	*) primary+=("$f") ;;
	esac
done
for f in "$dist"/*.mcpb "$dist"/install.sh "$dist"/install.ps1; do
	[ -e "$f" ] && primary+=("$f")
done
# Count the legacy ARCHIVES before anything else is folded into that array: the
# checksums below land in both sets, so once they are appended the array is never
# empty and could not tell "no korbit_* archives were built" from "some were".
legacy_archives=${#legacy[@]}

# The checksums file and its signature ride BOTH releases: it lists every asset
# of both sets, so whichever side a binary is on, it finds its own entry.
for f in "$dist"/checksums.txt "$dist"/checksums.txt.sig; do
	[ -e "$f" ] || continue
	primary+=("$f")
	legacy+=("$f")
done

if [ ${#primary[@]} -eq 0 ]; then
	echo "publish: ERROR no release artifacts in $dist/ (run 'make release' first)" >&2
	exit 1
fi

# Validate the legacy set BEFORE publishing anything, so a build that is missing
# it fails with nothing uploaded rather than leaving a primary release published
# and the legacy one absent. Publishing a legacy release with no archives in it
# would be worse than publishing none at all: every installed korbit resolves it
# as the latest release and then 404s on the archive it expects to find there.
if [ -n "$legacy_repo" ] && [ "$legacy_archives" -eq 0 ]; then
	echo "publish: ERROR no legacy korbit_* archives in $dist/ — every release must publish them" >&2
	echo "         (set KORBIT_LEGACY_RELEASE_REPO='' to publish without them deliberately)" >&2
	exit 1
fi

# publish_release <repo> <assets...> — create the release for $tag on <repo> (an
# empty <repo> means the current checkout's own repository) and upload the given
# assets, or re-upload over an existing release.
#
# Built as an array so it expands to nothing when the repo is empty — note the
# bash-3.2-safe "${arr[@]+…}" guard (a plain "${arr[@]}" on an empty array trips
# `set -u` in macOS's bash 3.2).
publish_release() {
	repo="$1"
	shift
	repo_args=()
	if [ -n "$repo" ]; then
		repo_args=(--repo "$repo")
	fi
	target="${repo:-the current repository}"

	if gh release view "$tag" "${repo_args[@]+"${repo_args[@]}"}" >/dev/null 2>&1; then
		echo "publish: release $tag exists on $target; re-uploading $# assets (--clobber)" >&2
		gh release upload "$tag" "${repo_args[@]+"${repo_args[@]}"}" --clobber "$@"
	else
		# Empty release body: --notes "" keeps gh non-interactive (no notes flag
		# would open an editor) and skips the auto-generated changelog.
		create_args=(--notes "")
		# A tag with a pre-release segment (e.g. v1.2.3-rc1) is marked pre-release.
		case "$tag" in
		*-*) create_args+=(--prerelease) ;;
		esac
		echo "publish: creating release $tag on $target with $# assets" >&2
		gh release create "$tag" "${repo_args[@]+"${repo_args[@]}"}" "${create_args[@]}" "$@"
	fi

	echo "publish: done ($tag -> $target)" >&2
}

publish_release "${KORBIT_RELEASE_REPO:-}" "${primary[@]}"

if [ -z "$legacy_repo" ]; then
	echo "publish: KORBIT_LEGACY_RELEASE_REPO is empty — skipping the legacy korbit_* upload" >&2
else
	publish_release "$legacy_repo" "${legacy[@]}"
fi
