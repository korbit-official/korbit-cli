// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/korbit-official/korbit-cli/internal/fslock"
	"github.com/minio/selfupdate"
)

// ProvenanceError marks a self update refused because the running binary is not
// a managed-script install (Homebrew, go install, a hand-downloaded archive, a
// dev build). Message is the reason; Guidance is the method-specific fix. The
// cli layer maps it to a config-class error so an agent branches on it.
type ProvenanceError struct {
	Message  string
	Guidance string
}

func (e *ProvenanceError) Error() string { return e.Message }

// UpdateResult reports the outcome of `self update`. Updated is false when
// already current or when only checking (--dry-run); CheckedOnly marks the
// dry-run case.
type UpdateResult struct {
	PreviousVersion string `json:"previousVersion"`
	LatestVersion   string `json:"latestVersion"`
	Updated         bool   `json:"updated"`
	CheckedOnly     bool   `json:"checkedOnly"`
	Executable      string `json:"executable,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	// SignatureCheck records how the release signature was handled for an applied
	// update: SigVerified (verified against the published cert) or SigDisabled
	// (the docs host served an empty cert — the kill switch). Empty when no update
	// was applied (dry-run / already-latest), so an agent can audit from --json
	// output whether the binary it now runs was signature-verified.
	SignatureCheck string `json:"signatureCheck,omitempty"`
}

// Update resolves the target release (latest, or targetVersion when set),
// and — unless dryRun — downloads it, verifies the release checksums.txt against
// the release signature (verify.go), verifies the archive's sha256 against that
// checksums.txt, and replaces the installed binary in place (minio/selfupdate,
// which re-verifies the exact bytes against the passed checksum and handles the
// Windows running-exe swap), then updates the manifest. It refuses on a
// non-managed or dev build (ProvenanceError).
func (c Config) Update(ctx context.Context, targetVersion string, dryRun bool) (*UpdateResult, error) {
	c.log().Debug("self update starting", "currentVersion", c.Version, "requestedTarget", targetVersion, "dryRun", dryRun, "repo", c.repo())
	if err := c.requireReleaseBuild(); err != nil {
		c.log().Debug("self update refused: development build", "version", c.Version)
		return nil, &ProvenanceError{
			Message:  err.Error(),
			Guidance: "build and install a release, or run the install one-liner from the project's README",
		}
	}
	l := c.Layout()
	if err := c.assertManaged(l); err != nil {
		c.log().Debug("self update refused: not a managed install", "err", err.Error())
		return nil, err
	}

	target := normalizeTag(targetVersion)
	if target == "" {
		latest, err := c.resolveLatest(ctx)
		if err != nil {
			c.log().Debug("resolving latest release failed", "err", err.Error())
			return nil, err
		}
		target = latest
	}

	res := &UpdateResult{PreviousVersion: c.Version, LatestVersion: target, Executable: l.ExecutablePath()}
	if target == normalizeTag(c.Version) {
		c.log().Debug("self update: already on target version", "version", target)
		return res, nil // already on the target version
	}
	if dryRun {
		c.log().Debug("self update dry-run: update available, not applying", "from", c.Version, "to", target)
		res.CheckedOnly = true
		return res, nil
	}

	// Serialize the swap against a concurrent install/update.
	if err := os.MkdirAll(l.Home(), 0o700); err != nil {
		return nil, err
	}
	unlock, err := fslock.Lock(l.LockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch checksums.txt and authenticate it with the release signature BEFORE
	// downloading the (large, attacker-influenceable) archive — the archive's
	// sha256 is only as good as the file it's read from, so the cheap signature
	// gate runs first. Fail-closed: see verifyChecksumsSignature.
	sumsRaw, err := c.fetch(ctx, c.checksumsURL(target))
	if err != nil {
		return nil, fmt.Errorf("fetching checksums: %w", err)
	}
	sigCheck, err := c.verifyChecksumsSignature(ctx, target, sumsRaw)
	if err != nil {
		c.log().Debug("release signature verification failed", "target", target, "err", err.Error())
		return nil, err
	}
	c.log().Debug("checksums authenticated", "target", target, "signatureCheck", sigCheck)
	want := parseChecksums(sumsRaw)[c.assetName()]
	if want == "" {
		return nil, fmt.Errorf("checksums.txt has no entry for %s", c.assetName())
	}

	// Download the archive and verify it against the authenticated checksum, then
	// extract the binary.
	c.progressf("downloading %s %s…", c.repo(), target)
	archive, err := c.fetch(ctx, c.downloadURL(target))
	if err != nil {
		return nil, err
	}
	if got := sha256Bytes(archive); got != want {
		c.log().Debug("archive checksum mismatch", "asset", c.assetName(), "expected", want, "got", got)
		return nil, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", c.assetName(), want, got)
	}
	c.log().Debug("archive checksum verified", "asset", c.assetName(), "bytes", len(archive))
	binBytes, err := extractBinary(archive, c.os(), l.BinName())
	if err != nil {
		return nil, err
	}
	newSum := sha256Bytes(binBytes)
	c.log().Debug("binary extracted from archive", "binary", l.BinName(), "bytes", len(binBytes), "sha256", newSum)

	// Replace the installed binary in place. minio handles the Windows running-exe
	// move; OldSavePath is empty so it manages and cleans up its own outgoing-binary
	// swap file. We don't pass Options.Checksum: it is a pre-write guard over the
	// same bytes we hand to Apply, so it could only compare a hash of binBytes
	// against a hash of binBytes. The meaningful verification already happened above
	// — the archive was checked against the signature-authenticated checksum — and
	// no independent hash of the extracted binary exists to re-check here.
	c.log().Debug("applying in-place binary swap", "path", l.ExecutablePath())
	if err := selfupdate.Apply(bytes.NewReader(binBytes), selfupdate.Options{
		TargetPath: l.ExecutablePath(),
		TargetMode: 0o755,
	}); err != nil {
		if rerr := selfupdate.RollbackError(err); rerr != nil {
			c.log().Debug("in-place swap failed and rollback failed", "err", err.Error(), "rollbackErr", rerr.Error())
			return nil, fmt.Errorf("update failed and rollback also failed: %v (rollback: %v)", err, rerr)
		}
		c.log().Debug("in-place swap failed, rolled back", "err", err.Error())
		return nil, fmt.Errorf("applying the update: %w", err)
	}

	// Record the new installed version in the manifest.
	m := Manifest{
		Method:      MethodManagedScript,
		Executable:  l.ExecutablePath(),
		Version:     target,
		OS:          c.os(),
		Arch:        c.arch(),
		SHA256:      newSum,
		Repo:        c.repo(),
		InstalledAt: strconv.FormatInt(c.now(), 10),
	}
	if err := m.save(l.ManifestPath()); err != nil {
		return nil, err
	}
	c.log().Debug("self update applied", "from", c.Version, "to", target, "sha256", newSum, "signatureCheck", sigCheck)
	res.Updated = true
	res.SHA256 = newSum
	res.SignatureCheck = sigCheck
	return res, nil
}

// assertManaged verifies the running binary is a managed-script install: a
// manifest with method managed-script exists AND the running executable is the
// installed binary on PATH. Otherwise it returns a ProvenanceError with
// method-specific guidance so a copy dragged elsewhere, or a Homebrew/go-install
// binary, is never updated in place.
func (c Config) assertManaged(l Layout) error {
	m, found, err := loadManifest(l.ManifestPath())
	if err != nil && found {
		return &ProvenanceError{
			Message:  "the install manifest is unreadable",
			Guidance: "re-run the install one-liner to repair the install, then try again",
		}
	}
	if !found || m.Method != MethodManagedScript {
		return &ProvenanceError{
			Message:  "this binary was not installed by the managed install script, so it can't update itself",
			Guidance: provenanceGuidance(),
		}
	}
	exe, err := c.runningExe()
	if err != nil {
		return err
	}
	if !l.isInstalledBinary(exe) {
		return &ProvenanceError{
			Message:  fmt.Sprintf("the running binary (%s) is not the managed installed binary (%s)", exe, l.ExecutablePath()),
			Guidance: "run the managed copy on your PATH, or re-run the install one-liner",
		}
	}
	return nil
}

// provenanceGuidance is the fix for an unmanaged install. It stays generic (the
// installer can't always tell Homebrew from a hand-download apart) and points at
// the one-liner, which repairs/creates a managed install.
func provenanceGuidance() string {
	return "if you installed via a package manager (e.g. Homebrew) update it there; if via `go install`, re-run `go install …@latest`; otherwise re-run the install one-liner from the project's README"
}
