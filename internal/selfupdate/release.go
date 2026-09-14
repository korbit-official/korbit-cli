// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/logging"
)

// maxArchiveBytes caps a downloaded release archive (defensive against a
// runaway/hostile response); the real archives are tens of MB.
const maxArchiveBytes = 256 << 20 // 256 MiB

// assetName is the release archive filename for this platform, matching the
// GoReleaser archive name_template (digitalx-cli_{{.Os}}_{{.Arch}}) and the
// per-OS format override (tar.gz everywhere, zip on Windows). The archive is
// named for the product; the binary inside it is dgx-cli. Keep in sync with
// .goreleaser.yaml.
//
// It is the FALLBACK: a release that publishes a manifest names its own
// archives (see resolveTarget).
func (c Config) assetName() string {
	ext := "tar.gz"
	if c.os() == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("digitalx-cli_%s_%s.%s", c.os(), c.arch(), ext)
}

// downloadURL is the release asset URL for a tag and a named asset.
func (c Config) downloadURL(tag, asset string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", c.repo(), tag, asset)
}

// releaseManifestName is the release asset mapping a platform to the archive to
// download and the basename of the binary inside it. The name is frozen: an
// installed binary asks for it by this exact name, so renaming it strands every
// install that reads it. resolveTarget holds the trust policy.
const releaseManifestName = "release-manifest.json"

// manifestURL is the release manifest's URL for a tag.
func (c Config) manifestURL(tag string) string {
	return c.downloadURL(tag, releaseManifestName)
}

// releaseTarget is one platform's entry in the release manifest: the archive to
// download and the basename of the CLI binary inside it.
//
// Both are independent of the names this binary was compiled with — which is
// the point. A release may rename either one, and installs made before it still
// find their download.
type releaseTarget struct {
	Archive string `json:"archive"`
	Binary  string `json:"binary"`
}

// releaseManifest is a parsed release-manifest.json: a per-platform map keyed
// "<goos>/<goarch>". The schema evolves by ADDING fields, never by
// restructuring or removing one, because every release is read by binaries
// compiled before it existed.
//
// Only the fields this code acts on are declared; encoding/json ignores the
// rest. The file's own `schema` is deliberately among the undeclared: binding it
// would make its VALUE part of the parse, so a release that wrote
// `"schema": "2"` or an object would fail json.Unmarshal — and a failed parse of
// an authentic manifest is fatal (see resolveTarget), stranding exactly the
// installs the manifest exists to carry through a rename.
type releaseManifest struct {
	Platforms map[string]releaseTarget `json:"platforms"`
}

// resolveTarget decides which archive to download for this platform and which
// binary basename to extract from it, preferring what the release itself
// declares over the names this binary was compiled with.
//
// sums is the checksum map parsed from the ALREADY signature-verified
// checksums.txt, and that is what authenticates the manifest: it is listed there
// like every other asset, so matching its sha256 against that map proves the
// release published it. No key and no trusted host of its own.
//
// Three outcomes:
//
//   - NOT OBTAINABLE (404, any other non-200, or a transport failure): fall back
//     to the compiled-in names. A release that publishes none is the ordinary
//     reason, and suppressing one gains an attacker nothing — the fallback
//     archive is still checked against the signed checksums. A 404 is a FACT
//     about the release and passes quietly; every other failure is an absence of
//     information and says so on the progress stream, because after a rename a
//     silently suppressed manifest pins an install to its current version, and
//     the only symptom is a missing-archive error that reads like a broken
//     release.
//   - Obtained but NOT AUTHENTIC (absent from checksums.txt, or its hash does
//     not match): fatal. A file that steers where code comes from is worth
//     nothing unsigned.
//   - AUTHENTIC: authoritative. Unparseable, or carrying no usable entry for
//     this platform, is fatal rather than a fallback — the release has stated
//     what it contains, and a compiled-in name it did not list would only 404
//     with a worse message.
func (c Config) resolveTarget(ctx context.Context, tag string, sums map[string]string, l Layout) (releaseTarget, error) {
	fallback := releaseTarget{Archive: c.assetName(), Binary: l.BinName()}

	status, body, err := c.httpGet(ctx, c.manifestURL(tag))
	switch {
	case err != nil:
		c.log().Debug("release manifest unreachable; using the compiled-in names", "tag", tag, "err", err.Error())
		c.progressf("could not read %s from release %s (%v) — falling back to the built-in asset names", releaseManifestName, tag, err)
		return fallback, nil
	case status == http.StatusNotFound:
		c.log().Debug("release publishes no manifest; using the compiled-in names", "tag", tag)
		return fallback, nil
	case status != http.StatusOK:
		c.log().Debug("release manifest fetch failed; using the compiled-in names", "tag", tag, "status", status)
		c.progressf("could not read %s from release %s (status %d) — falling back to the built-in asset names", releaseManifestName, tag, status)
		return fallback, nil
	}

	want := sums[releaseManifestName]
	if want == "" {
		// Not "the signed checksums.txt": under the empty-cert kill switch the
		// checksums carry no verified signature, and this message is read in
		// exactly that mode.
		return releaseTarget{}, fmt.Errorf("%s is published for %s but is not listed in checksums.txt — refusing to update against an unauthenticated release manifest", releaseManifestName, tag)
	}
	if got := sha256Bytes(body); got != want {
		c.log().Debug("release manifest checksum mismatch", "tag", tag, "expected", want, "got", got)
		return releaseTarget{}, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", releaseManifestName, want, got)
	}

	var m releaseManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return releaseTarget{}, fmt.Errorf("reading %s from release %s: %w", releaseManifestName, tag, err)
	}
	platform := c.os() + "/" + c.arch()
	target, ok := m.Platforms[platform]
	if !ok {
		return releaseTarget{}, fmt.Errorf("release %s has no build for %s", tag, platform)
	}
	if !isPlainFilename(target.Archive) || !isPlainFilename(target.Binary) {
		return releaseTarget{}, fmt.Errorf("release %s names an unusable %s entry for %s (archive %q, binary %q)", tag, releaseManifestName, platform, target.Archive, target.Binary)
	}
	c.log().Debug("release manifest resolved the download", "tag", tag, "platform", platform, "archive", target.Archive, "binary", target.Binary)
	return target, nil
}

// plainFilenameRe is the character set a manifest filename may use: an
// alphanumeric first character, then alphanumerics and the three punctuation
// marks real artifact names carry. An ALLOWLIST by design — blacklisting path
// separators would still admit percent-encoded ones (the archive name is
// interpolated raw into a release URL, where %2F separates paths again), query
// and fragment characters, a leading dash, and Windows reserved and
// alternate-data-stream forms.
var plainFilenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// isPlainFilename reports whether name is a single, ordinary path element. Both
// manifest fields must be one: the archive name is joined onto a release URL and
// the binary name is matched against archive entries, so a separator in either
// would reach outside the release the signature covers. "." and ".." are
// rejected by construction, both needing a leading dot.
//
// It stays a boundary even though the manifest is authenticated upstream: one
// that holds only while its caller is correct is not a boundary — the same
// reason managedAliasNames validates the names it reads out of the install
// manifest.
func isPlainFilename(name string) bool {
	return plainFilenameRe.MatchString(name)
}

// checksumsURL is the release checksums.txt URL for a tag.
func (c Config) checksumsURL(tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/checksums.txt", c.repo(), tag)
}

// checksumsSigURL is the detached RSA/SHA-256 signature over checksums.txt
// (goreleaser's openssl-sign hook) for a tag. It rides the GitHub release next
// to checksums.txt; the key that verifies it is pinned on the trusted docs host.
func (c Config) checksumsSigURL(tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/checksums.txt.sig", c.repo(), tag)
}

// resolveLatest returns the newest release tag by following the
// github.com/<repo>/releases/latest redirect and reading the tag off the final
// URL (…/releases/tag/vX.Y.Z). No API token, no rate limit; the shared Doer
// follows the redirect, so the resolved tag is the last path segment.
func (c Config) resolveLatest(ctx context.Context) (string, error) {
	if c.Doer == nil {
		return "", fmt.Errorf("no HTTP client configured")
	}
	url := fmt.Sprintf("https://github.com/%s/releases/latest", c.repo())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Doer.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("resolving the latest release: unexpected status %s", resp.Status)
	}
	// resp.Request is the final request after redirects; its path ends in
	// …/releases/tag/<tag> for a real release, or …/releases for an empty repo.
	final := resp.Request.URL.Path
	seg := path.Base(final)
	if seg == "" || seg == "releases" || seg == "latest" {
		return "", fmt.Errorf("resolving the latest release: no release found for %s", c.repo())
	}
	c.log().Debug("resolved latest release", "repo", c.repo(), "tag", seg)
	return seg, nil
}

// fetch GETs url and returns the (size-capped) body, treating a non-2xx status
// as an error (a 404 is the tell for a missing tag/asset).
func (c Config) fetch(ctx context.Context, url string) ([]byte, error) {
	if c.Doer == nil {
		return nil, fmt.Errorf("no HTTP client configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	logging.Trace(c.log(), "fetching release asset", "url", url)
	resp, err := c.Doer.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found: %s", url)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("downloading %s: unexpected status %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err == nil {
		logging.Trace(c.log(), "fetched release asset", "url", url, "status", resp.StatusCode, "bytes", len(body))
	}
	return body, err
}

// parseChecksums parses a GoReleaser checksums.txt ("<hex-sha256>  <filename>"
// per line, sha256sum format) into a filename→hash map.
func parseChecksums(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Split on whitespace: the hash, then the (possibly "*"-prefixed) name.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

// extractBinary returns the CLI binary's bytes from a release archive: the
// entry named binName (dgx-cli / dgx-cli.exe) inside a tar.gz (darwin/linux) or
// a zip (windows).
//
// An empty result is refused: binName comes from the release's own manifest, so
// it can select something that is not the program — a directory (whose zip entry
// opens as zero bytes) or, on a release-side typo, another file. Writing zero
// bytes over the installed CLI leaves an install that can neither run nor update
// itself, unrecoverable without a reinstall.
func extractBinary(archive []byte, goos, binName string) ([]byte, error) {
	var (
		b   []byte
		err error
	)
	if goos == "windows" {
		b, err = extractFromZip(archive, binName)
	} else {
		b, err = extractFromTarGz(archive, binName)
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("the archive entry %q is empty", binName)
	}
	return b, nil
}

func extractFromTarGz(archive []byte, binName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("reading the archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		if path.Base(h.Name) == binName && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, maxArchiveBytes))
		}
	}
	return nil, fmt.Errorf("archive did not contain %q", binName)
}

func extractFromZip(archive []byte, binName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("reading the archive: %w", err)
	}
	for _, f := range zr.File {
		// IsDir mirrors the tar reader's TypeReg check: "dgx-cli/" has the same
		// path.Base as the file, and opens as zero bytes rather than failing.
		if path.Base(f.Name) != binName || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxArchiveBytes))
	}
	return nil, fmt.Errorf("archive did not contain %q", binName)
}

// ---- version ordering ----

// normalizeTag canonicalizes a user-supplied version to the tag form the release
// URLs use — a leading "v" (so `self update --tag 1.2.3` and `v1.2.3` both
// resolve). An empty string stays empty.
func normalizeTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == devVersion {
		return v
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}
