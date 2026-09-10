#!/usr/bin/env bash
# Copyright (c) 2026 Digital X Co., Ltd.
#
# SPDX-License-Identifier: Apache-2.0
#
# RSA-sign one release artifact (the checksums file) with a detached PKCS#1 v1.5
# signature over its SHA-256 digest, using openssl. Invoked by GoReleaser's signs
# pipe (see .goreleaser.yaml). Signing the checksums file authenticates every
# artifact transitively: a verifier checks this signature on checksums.txt, then
# checks each download against the hashes inside it.
#
# A raw RSA/SHA-256 detached signature is verifiable with openssl and other
# common crypto libraries. Its public cert is pinned by the evergreen installer
# and fetched by `dgx-cli self update` (see RELEASING.md).
#
# The signing key is a path in the environment:
#   KORBIT_RSA_SIGN_KEY   path to the PEM private key to sign with
#
# Only the private key is referenced here; its public half is held by the
# verifier, never by this repo.
set -euo pipefail

artifact="${1:?usage: openssl-sign.sh <artifact> <signature-output>}"
signature="${2:?usage: openssl-sign.sh <artifact> <signature-output>}"

# Explicit opt-out, symmetric with macOS signing — for test builds without a key.
if [ -n "${KORBIT_SKIP_RSA_SIGN:-}" ]; then
	echo "openssl-sign: KORBIT_SKIP_RSA_SIGN set; skipping $artifact (UNSIGNED — do not ship)" >&2
	exit 0
fi

if [ -z "${KORBIT_RSA_SIGN_KEY:-}" ]; then
	# Tolerate a missing key for a local snapshot build so `make dist` works on
	# any machine; fail a real release rather than ship an unsigned checksums file.
	if [ "${IS_SNAPSHOT:-false}" = "true" ]; then
		echo "openssl-sign: KORBIT_RSA_SIGN_KEY not set; skipping $artifact (snapshot)" >&2
		exit 0
	fi
	echo "openssl-sign: ERROR KORBIT_RSA_SIGN_KEY not set for release build of $artifact" >&2
	echo "  set KORBIT_RSA_SIGN_KEY to the PEM private key path to sign with; see RELEASING.md" >&2
	exit 1
fi

echo "openssl-sign: signing $artifact" >&2
openssl dgst -sha256 -sign "$KORBIT_RSA_SIGN_KEY" -out "$signature" "$artifact"
