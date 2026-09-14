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
# A release carries the digitalx-cli_<os>_<arch> archives, the .mcpb Desktop
# Extensions, the filled install.sh/install.ps1, release-manifest.json,
# checksums.txt and checksums.txt.sig — everything the installers and `dgx-cli
# self update` fetch. The one checksums file lists every asset, so the single
# signature over it authenticates all of them.
#
# Target repository:
#   KORBIT_RELEASE_REPO=<owner>/<name>   default is the current checkout's repo
#                                        (gh auto-detects it from the git
#                                        remote). Set it to publish the SAME
#                                        built artifacts to a staging repo for
#                                        review first.
#
# HEAD must be on a tag — that tag is the release version. Requires `gh`
# authenticated (`gh auth login`, or GH_TOKEN/GITHUB_TOKEN in CI). Re-running for
# a release that already exists re-uploads the assets (--clobber).
#
# Usage: publish.sh [dist-dir] [--dry-run]
#
# --dry-run runs every gate below over dist/, prints the asset list and the
# target repository, and exits without contacting GitHub. Test this script that
# way rather than by neutralizing its `gh` calls by hand: an edit that misses a
# call site fails OPEN and publishes for real.
set -euo pipefail

dist="dist"
dry_run=false
for arg in "$@"; do
	case "$arg" in
	--dry-run) dry_run=true ;;
	-*)
		echo "publish: ERROR unknown option $arg (usage: publish.sh [dist-dir] [--dry-run])" >&2
		exit 1
		;;
	*) dist="$arg" ;;
	esac
done

# The release manifest is checked in rather than built (RELEASING.md, "The
# release manifest"), so it is uploaded from the source tree — the same path
# .goreleaser.yaml folds into checksums.txt. Resolved relative to this script,
# not to the caller's working directory.
manifest="$(cd "$(dirname "$0")/.." && pwd)/release-manifest.json"

tag="$(git describe --tags --exact-match 2>/dev/null || true)"
if [ -z "$tag" ]; then
	# The gates below never read the tag, so a dry run works from an untagged
	# checkout — the usual state when testing a build.
	if [ "$dry_run" = true ]; then
		tag="(untagged)"
	else
		echo "publish: ERROR HEAD is not on a tag; tag the release commit first" >&2
		exit 1
	fi
fi

# Collect the release assets out of dist/ — the top-level files only, not the
# intermediate per-target build directories. The `[ -e ]` guard drops any glob
# that matched nothing (e.g. checksums.txt.sig when signing was skipped).
assets=()
archives=()
for f in "$dist"/*.tar.gz "$dist"/*.zip; do
	[ -e "$f" ] || continue
	assets+=("$f")
	archives+=("$(basename "$f")")
done
for f in "$dist"/*.mcpb "$dist"/install.sh "$dist"/install.ps1 \
	"$dist"/checksums.txt "$dist"/checksums.txt.sig; do
	[ -e "$f" ] && assets+=("$f")
done

if [ ${#assets[@]} -eq 0 ]; then
	echo "publish: ERROR no release artifacts in $dist/ (run 'make release' first)" >&2
	exit 1
fi

# Appended AFTER the emptiness check: the manifest comes from the source tree,
# so adding it earlier would keep that check from ever firing, turning "you
# forgot to build" into a confusing complaint about a missing archive.
assets+=("$manifest")

# Cross-check release-manifest.json against what was built BEFORE publishing
# anything: an entry naming an archive this release does not carry
# breaks `self update` on that platform for every install that reads it, and
# nothing surfaces it until someone tries to update. TestReleaseManifestMatchesBuild
# guards the same file against the names this build uses; this guards it against
# the artifacts on disk.
if [ ! -e "$manifest" ]; then
	echo "publish: ERROR no release-manifest.json at $manifest" >&2
	exit 1
fi

# checksums_hash <name> — the hash checksums.txt records for that exact
# filename, empty when it lists none. Reads the last field and strips
# sha256sum's binary-mode "*" marker, matching how the updater parses the file.
checksums_hash() {
	awk -v want="$1" '{ n = $NF; sub(/^\*/, "", n); if (n == want) { print $1; exit } }' "$dist/checksums.txt"
}

# in_checksums <name> — true when checksums.txt lists that filename at all.
in_checksums() {
	[ -n "$(checksums_hash "$1")" ]
}

# file_sha256 <path> — lowercase hex sha256, via whichever tool this OS has
# (sha256sum on Linux, shasum on macOS), matching the installers' own probe.
file_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# The manifest holds one "archive" per platform; pull the values out by field
# name. A read that finds none means the file's shape changed and this gate is
# checking nothing, so it fails rather than pass vacuously.
manifest_archives=()
while IFS= read -r name; do
	[ -n "$name" ] && manifest_archives+=("$name")
done <<EOF
$(grep -o '"archive"[[:space:]]*:[[:space:]]*"[^"]*"' "$manifest" | sed 's/.*"\([^"]*\)"$/\1/')
EOF
if [ ${#manifest_archives[@]} -eq 0 ]; then
	echo "publish: ERROR no \"archive\" entries found in $manifest" >&2
	exit 1
fi

# archive_contains <archive> <entry> — true when the archive holds an entry with
# that exact basename. The only check tying the manifest's `binary` field to
# reality: rename `binary:` in .goreleaser.yaml alone and every other gate here
# passes, while each user downloads tens of MB and then finds no program in it.
archive_contains() {
	case "$1" in
	*.zip) unzip -Z1 "$dist/$1" ;;
	*) tar -tzf "$dist/$1" ;;
	esac | sed 's|/$||; s|.*/||' | grep -qxF "$2"
}

for name in "${manifest_archives[@]}"; do
	if [ ! -e "$dist/$name" ]; then
		echo "publish: ERROR release-manifest.json names $name, which $dist/ does not contain" >&2
		exit 1
	fi
	if ! in_checksums "$name"; then
		echo "publish: ERROR release-manifest.json names $name, which checksums.txt does not list" >&2
		exit 1
	fi
done

# Each platform's archive must actually hold the binary its entry names. The two
# fields sit on separate lines in a pretty-printed manifest and `grep -o` matches
# within one line, so the newlines come out first; `[^}]*` then keeps each match
# inside a single platform object.
manifest_pairs=()
while IFS= read -r pair; do
	[ -n "$pair" ] && manifest_pairs+=("$pair")
done <<EOF
$(tr -d '\n' <"$manifest" |
	grep -o '"archive"[[:space:]]*:[[:space:]]*"[^"]*"[^}]*"binary"[[:space:]]*:[[:space:]]*"[^"]*"' |
	sed 's/.*"archive"[[:space:]]*:[[:space:]]*"\([^"]*\)".*"binary"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1|\2/')
EOF
# A pair count that disagrees with the archive count means this parse stopped
# understanding the file: the loop below would then check only some platforms,
# or none, while still reporting success. An unrecognized shape is an error, not
# a pass.
if [ ${#manifest_pairs[@]} -ne ${#manifest_archives[@]} ]; then
	echo "publish: ERROR read ${#manifest_pairs[@]} archive/binary pairs from release-manifest.json, but it names ${#manifest_archives[@]} archives" >&2
	echo "         (each platform entry must give \"archive\" before \"binary\")" >&2
	exit 1
fi
for pair in "${manifest_pairs[@]}"; do
	arch_name="${pair%%|*}"
	bin_name="${pair#*|}"
	if ! archive_contains "$arch_name" "$bin_name"; then
		echo "publish: ERROR release-manifest.json says $arch_name contains \"$bin_name\", but it does not" >&2
		exit 1
	fi
done

# The manifest must be listed in checksums.txt AND the bytes about to be
# uploaded must be the bytes that were hashed. The updater treats a mismatch as
# fatal with NO fallback, so a manifest edited after `make release` breaks
# `self update` for every install at once.
manifest_recorded="$(checksums_hash "$(basename "$manifest")")"
if [ -z "$manifest_recorded" ]; then
	echo "publish: ERROR checksums.txt does not list release-manifest.json — it would be unauthenticated" >&2
	echo "         (check the checksum.extra_files glob in .goreleaser.yaml)" >&2
	exit 1
fi
manifest_actual="$(file_sha256 "$manifest")"
if [ "$manifest_recorded" != "$manifest_actual" ]; then
	echo "publish: ERROR release-manifest.json does not match its checksums.txt entry" >&2
	echo "         recorded $manifest_recorded" >&2
	echo "         actual   $manifest_actual" >&2
	echo "         (it changed after the build signed the checksums; re-run 'make release')" >&2
	exit 1
fi

# And the other direction: an archive built but not named by the manifest is a
# platform whose installs fall back to a compiled-in name this release may not
# use. The "${arr[@]+…}" guard is for bash 3.2 — a plain "${arr[@]}" on an empty
# array trips `set -u` there.
for name in "${archives[@]+"${archives[@]}"}"; do
	case " ${manifest_archives[*]} " in
	*" $name "*) ;;
	*)
		echo "publish: ERROR $name was built but release-manifest.json does not name it" >&2
		exit 1
		;;
	esac
done

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

# The dry-run branch sits where the upload is dispatched, not inside
# publish_release: a second call site added later cannot quietly skip it.
if [ "$dry_run" = true ]; then
	echo "publish: DRY RUN — every gate passed; nothing was uploaded" >&2
	echo "publish: tag $tag" >&2
	echo "publish: -> ${KORBIT_RELEASE_REPO:-the current repository} (${#assets[@]} assets)" >&2
	for f in "${assets[@]}"; do echo "           $(basename "$f")" >&2; done
	exit 0
fi

publish_release "${KORBIT_RELEASE_REPO:-}" "${assets[@]}"
