// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// maxArchiveBytes caps a downloaded release archive (defensive against a
// runaway/hostile response); the real archives are tens of MB.
const maxArchiveBytes = 256 << 20 // 256 MiB

// assetName is the release archive filename for this platform, matching the
// GoReleaser name_template ({{.ProjectName}}_{{.Os}}_{{.Arch}}) and the per-OS
// format override (tar.gz everywhere, zip on Windows). Keep in sync with
// .goreleaser.yaml.
func (c Config) assetName() string {
	ext := "tar.gz"
	if c.os() == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("korbit_%s_%s.%s", c.os(), c.arch(), ext)
}

// downloadURL is the release asset URL for a tag and this platform's archive.
func (c Config) downloadURL(tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", c.repo(), tag, c.assetName())
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
// entry named binName (korbit / korbit.exe) inside a tar.gz (darwin/linux) or a
// zip (windows).
func extractBinary(archive []byte, goos, binName string) ([]byte, error) {
	if goos == "windows" {
		return extractFromZip(archive, binName)
	}
	return extractFromTarGz(archive, binName)
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
		if path.Base(f.Name) != binName {
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
