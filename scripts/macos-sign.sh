#!/usr/bin/env bash
# Copyright (c) 2026 Digital X Co., Ltd.
#
# SPDX-License-Identifier: Apache-2.0
#
# Sign one macOS (Mach-O) binary with rcodesign, for notarization eligibility.
# Invoked by GoReleaser as a per-target post-build hook (see .goreleaser.yaml),
# once per build target.
#
# It is a no-op for non-darwin targets. The signing identity is taken from the
# environment — macOS keychain OR a PKCS#12 (.p12/.pfx) file, so the same script
# works whether the release runs on macOS or Linux:
#
#   macOS keychain:
#     MACOS_SIGN_KEYCHAIN_FINGERPRINT  SHA-256 fingerprint of the Developer ID
#                                      Application certificate in the keychain
#
#   PKCS#12 file (any OS):
#     MACOS_SIGN_P12                   path to the .p12/.pfx file
#     MACOS_SIGN_P12_PASSWORD_FILE     path to a file holding its password, OR
#     MACOS_SIGN_P12_PASSWORD          the password inline (less safe)
#
# `--for-notarization` enables the hardened runtime, requires a Developer ID
# certificate, and stamps a secure timestamp — the prerequisites Apple's notary
# service checks. Notarization itself is a separate step (scripts/macos-notarize.sh).
set -euo pipefail

bin="${1:?usage: macos-sign.sh <binary-path>}"

# Reverse-DNS code-signing identifier, pinned (not derived from the on-disk
# filename). The macOS keychain backend binds each stored secret's ACL to this
# binary's designated requirement, which embeds this identifier; deriving it
# from the filename would change the requirement whenever the binary is
# renamed, and break the ACL match. Keep it stable across versions AND across
# the binary name: every key already in a user's Keychain carries an ACL bound
# to this exact identifier, so a new one makes macOS re-prompt for
# authorization on each stored key.
bundle_id="kr.co.korbit.korbit-cli"

# rcodesign only signs Mach-O; skip every other target.
if [ "${GOOS:-}" != "darwin" ]; then
	exit 0
fi

# Explicit opt-out, for testing a real release build on a machine without a
# signing certificate. When set, signing is skipped in ANY mode (snapshot or
# release). The resulting macOS binaries are UNSIGNED and must never ship.
if [ -n "${KORBIT_SKIP_MACOS_SIGN:-}" ]; then
	echo "macos-sign: KORBIT_SKIP_MACOS_SIGN set; skipping $bin (UNSIGNED — do not ship)" >&2
	exit 0
fi

creds=()
if [ -n "${MACOS_SIGN_KEYCHAIN_FINGERPRINT:-}" ]; then
	# The SHA-256 fingerprint identifies the certificate uniquely across
	# keychains, so it is passed on its own; rcodesign rejects combining it with
	# --keychain-domain.
	creds=(--keychain-fingerprint "$MACOS_SIGN_KEYCHAIN_FINGERPRINT")
elif [ -n "${MACOS_SIGN_P12:-}" ]; then
	creds=(--p12-file "$MACOS_SIGN_P12")
	if [ -n "${MACOS_SIGN_P12_PASSWORD_FILE:-}" ]; then
		creds+=(--p12-password-file "$MACOS_SIGN_P12_PASSWORD_FILE")
	elif [ -n "${MACOS_SIGN_P12_PASSWORD:-}" ]; then
		creds+=(--p12-password "$MACOS_SIGN_P12_PASSWORD")
	fi
fi

if [ ${#creds[@]} -eq 0 ]; then
	# No identity configured. Tolerate this for a local snapshot build so the
	# full matrix still compiles on any machine; fail a real release build,
	# where an unsigned macOS binary must never ship.
	if [ "${IS_SNAPSHOT:-false}" = "true" ]; then
		echo "macos-sign: no signing identity configured; skipping $bin (snapshot)" >&2
		exit 0
	fi
	echo "macos-sign: ERROR no signing identity configured for release build of $bin" >&2
	echo "  set MACOS_SIGN_KEYCHAIN_FINGERPRINT (keychain) or MACOS_SIGN_P12[_PASSWORD[_FILE]] (file)" >&2
	echo "  see RELEASING.md" >&2
	exit 1
fi

echo "macos-sign: signing $bin" >&2
rcodesign sign --for-notarization \
	--binary-identifier "$bundle_id" \
	"${creds[@]}" "$bin"
