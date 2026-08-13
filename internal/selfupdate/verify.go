// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// maxMetaBytes caps the small metadata downloads (the release-signing cert and
// the detached signature). Both are well under a KiB; the cap is defensive
// against a runaway/hostile response.
const maxMetaBytes = 1 << 20 // 1 MiB

// SigVerified / SigDisabled are the outcomes verifyChecksumsSignature reports
// (surfaced in UpdateResult.SignatureCheck): the release signature was verified
// against the published cert, or verification was disabled by an empty cert.
const (
	SigVerified = "verified"
	SigDisabled = "disabled"
)

// verifyChecksumsSignature authenticates checksums.txt (the file every archive's
// sha256 is checked against) with a detached RSA/SHA-256 signature, keyed by a
// certificate published on the trusted docs host. This is what makes self update
// resistant to a compromised GitHub release: the archive, its checksums, and the
// signature all come from GitHub, but the verification key comes from a
// different, access-controlled host, so a forged release fails here.
//
// The trust anchor is TLS to the docs host, and the policy is fail-closed with
// exactly one deliberate off switch:
//
//   - Cert endpoint returns 200 with one or more parseable certs: verification
//     is ON. The signature is then REQUIRED — a missing/empty signature is a
//     downgrade signal and an invalid one is tampering; both are fatal. A
//     release verifies if ANY pinned cert's key validates it (lossless rotation).
//   - Cert endpoint returns 200 with an empty body: verification is DISABLED.
//     This is the intended kill switch — a positive, served artifact, not the
//     absence of one — so it can't be reached by a mistuned 404/403 or a
//     soft-404 HTML page.
//   - Anything else — a non-200 status, a transport/TLS error, or a 200 whose
//     non-empty body holds no parseable cert (a soft-404 page, junk) — is FATAL.
//     Absence is never a skip: the disable state is the empty body above, so a
//     failure to reach a clear verdict fails the update rather than silently
//     proceeding unverified.
//
// It returns SigVerified or SigDisabled (never both, never empty) on success, so
// the caller can record which path an applied update took.
func (c Config) verifyChecksumsSignature(ctx context.Context, tag string, checksums []byte) (string, error) {
	keys, enabled, err := c.fetchReleaseKeys(ctx)
	if err != nil {
		return "", err
	}
	if !enabled {
		c.log().Debug("release signature verification disabled by empty cert", "certURL", c.releaseCertURL())
		c.progressf("release signature verification disabled at %s — TLS + SHA-256 only", c.releaseCertURL())
		return SigDisabled, nil
	}
	c.log().Debug("release-signing cert fetched", "certURL", c.releaseCertURL(), "keys", len(keys))

	// A cert is published, so this release must carry a verifiable signature.
	sig, err := c.fetchSignature(ctx, tag)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(checksums)
	for _, k := range keys {
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) == nil {
			c.log().Debug("checksums.txt signature verified", "tag", tag)
			c.progressf("checksums.txt signature verified")
			return SigVerified, nil
		}
	}
	c.log().Debug("checksums.txt signature did not verify against any published key", "tag", tag, "keys", len(keys))
	return "", fmt.Errorf("the release signature did not verify against the published release-signing certificate — the download may be tampered with; not updating")
}

// fetchReleaseKeys fetches the release-signing certificate bundle from the
// trusted docs host and returns its RSA public keys. enabled is false only for
// the empty-body kill switch; every other non-verdict outcome is an error. See
// verifyChecksumsSignature for the policy this enforces.
func (c Config) fetchReleaseKeys(ctx context.Context) (keys []*rsa.PublicKey, enabled bool, err error) {
	url := c.releaseCertURL()
	status, body, err := c.httpGet(ctx, url)
	if err != nil {
		return nil, false, fmt.Errorf("fetching the release-signing certificate from %s: %w", url, err)
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("fetching the release-signing certificate from %s: unexpected status %d (expected 200 with the cert, or an empty body to disable verification)", url, status)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false, nil // empty body: the deliberate kill switch
	}
	keys = parseReleaseKeys(body)
	if len(keys) == 0 {
		return nil, false, fmt.Errorf("the release-signing certificate at %s is neither empty nor a valid PEM certificate — refusing to update against an unverifiable pin", url)
	}
	return keys, true, nil
}

// fetchSignature downloads the detached checksums.txt.sig from the release. A
// missing (404/empty) or unreachable signature is fatal here: reaching this path
// means a cert is published, so the release is expected to be signed, and a
// silent skip would make the whole check bypassable by stripping the signature.
func (c Config) fetchSignature(ctx context.Context, tag string) ([]byte, error) {
	url := c.checksumsSigURL(tag)
	status, body, err := c.httpGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetching the release signature from %s: %w", url, err)
	}
	if status == http.StatusNotFound || (status == http.StatusOK && len(bytes.TrimSpace(body)) == 0) {
		return nil, fmt.Errorf("this release is not signed, but a release-signing certificate is published — refusing to update (possible downgrade or tampering)")
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("fetching the release signature from %s: unexpected status %d", url, status)
	}
	return body, nil
}

// httpGet performs a GET and returns the HTTP status and the size-capped body.
// A transport-level failure (DNS/TLS/connection) is the error; an HTTP error
// status is not — the caller decides what each status means, which is central to
// the fail-closed policy in verifyChecksumsSignature.
func (c Config) httpGet(ctx context.Context, url string) (int, []byte, error) {
	if c.Doer == nil {
		return 0, nil, fmt.Errorf("no HTTP client configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	logging.Trace(c.log(), "fetching release metadata", "url", url)
	resp, err := c.Doer.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	logging.Trace(c.log(), "fetched release metadata", "url", url, "status", resp.StatusCode, "bytes", len(body))
	return resp.StatusCode, body, nil
}

// parseReleaseKeys extracts the RSA public keys from a PEM bundle of one or more
// CERTIFICATE blocks (concatenated for lossless key rotation). A block that
// isn't a parseable certificate, or whose key isn't RSA, is skipped so one bad
// entry doesn't sink a bundle that also holds a good one. Returns nil when no
// usable key is found.
func parseReleaseKeys(pemBytes []byte) []*rsa.PublicKey {
	var keys []*rsa.PublicKey
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if k, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			keys = append(keys, k)
		}
	}
	return keys
}
