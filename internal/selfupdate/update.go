// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/digitalx-official/digitalx-cli/internal/fslock"
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
	// SignatureCheck records how the release signature was handled: SigVerified
	// (verified against the published cert) or SigDisabled (the docs host served
	// an empty cert — the kill switch). Set for an applied update and for a dry
	// run, which verifies the same way; empty when there was no release to check
	// (already-latest), so an agent can audit from --json output whether the
	// binary it runs was signature-verified.
	SignatureCheck string `json:"signatureCheck,omitempty"`
	// Archive is the release asset this platform's binary came from, or — under
	// CheckedOnly — would come from. The release declares it
	// (release-manifest.json), so it need not be the name this binary was
	// compiled with and a caller cannot infer it. Empty when no release was
	// resolved.
	Archive string `json:"archive,omitempty"`
	// Aliases are the extra command names now pointing at the updated binary (see
	// alias.go). Absent on an install that carries none.
	Aliases []string `json:"aliases,omitempty"`
	// LayoutRepaired lists the command-name fixes this run made without an update
	// to apply: creating the primary binary name on an install that only ever had
	// the alias name, or recreating/repointing an alias that was missing or ran
	// something other than the installed binary (see planLayout). Absent when the
	// layout was already correct. Updated stays false for a run that only
	// repaired the layout — no new version was installed.
	//
	// Under CheckedOnly (--dry-run) these are the fixes a real run WOULD make;
	// nothing on disk was touched. The pair (checkedOnly, layoutRepaired) is what
	// distinguishes the two, the same way checkedOnly governs updated.
	LayoutRepaired []string `json:"layoutRepaired,omitempty"`
	// Warnings are non-fatal problems with an otherwise applied update.
	Warnings []string `json:"warnings,omitempty"`
}

// UpdateOptions are the choices `self update` takes from its flags.
type UpdateOptions struct {
	// TargetVersion pins the release to install (--tag); empty resolves the
	// latest.
	TargetVersion string
	// DryRun changes nothing on disk (--dry-run).
	DryRun bool
}

// Update resolves the target release (latest, or opts.TargetVersion when set),
// and — unless opts.DryRun — downloads it, verifies the release checksums.txt
// against the release signature (verify.go), verifies the archive's sha256
// against that checksums.txt, and replaces the installed binary in place
// (minio/selfupdate, which re-verifies the exact bytes against the passed
// checksum and handles the Windows running-exe swap), then updates the manifest.
// It refuses on a non-managed or dev build (ProvenanceError).
func (c Config) Update(ctx context.Context, opts UpdateOptions) (*UpdateResult, error) {
	c.log().Debug("self update starting", "currentVersion", c.Version, "requestedTarget", opts.TargetVersion, "dryRun", opts.DryRun, "repo", c.repo())
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
	return c.applyUpdate(ctx, l, opts)
}

// resolveDownload fetches and authenticates everything needed to NAME this
// platform's download for tag, stopping short of transferring it: the release
// checksums and their signature, the release manifest, and the archive's
// expected sha256. Being the verification chain minus the download is what lets
// a dry run prove a release is installable — every way a published release can
// be uninstallable is decided here.
//
// Checksums are signature-verified BEFORE anything else is trusted: the
// archive's sha256 and the manifest's are only as good as the file they are read
// from, so the cheap gate runs first. Fail-closed — see
// verifyChecksumsSignature.
func (c Config) resolveDownload(ctx context.Context, tag string, l Layout) (asset releaseTarget, wantSum, sigCheck string, err error) {
	sumsRaw, err := c.fetch(ctx, c.checksumsURL(tag))
	if err != nil {
		return releaseTarget{}, "", "", fmt.Errorf("fetching checksums: %w", err)
	}
	sigCheck, err = c.verifyChecksumsSignature(ctx, tag, sumsRaw)
	if err != nil {
		c.log().Debug("release signature verification failed", "target", tag, "err", err.Error())
		return releaseTarget{}, "", "", err
	}
	c.log().Debug("checksums authenticated", "target", tag, "signatureCheck", sigCheck)

	// Ask the release which archive this platform downloads and what the binary
	// inside it is called, rather than assuming the compiled-in names. The answer
	// is authenticated by the checksums just verified; a release that publishes
	// no manifest yields the compiled-in names. See resolveTarget.
	sums := parseChecksums(sumsRaw)
	asset, err = c.resolveTarget(ctx, tag, sums, l)
	if err != nil {
		return releaseTarget{}, "", "", err
	}
	wantSum = sums[asset.Archive]
	if wantSum == "" {
		return releaseTarget{}, "", "", fmt.Errorf("checksums.txt has no entry for %s", asset.Archive)
	}
	return asset, wantSum, sigCheck, nil
}

// applyUpdate is the binary half of Update: resolve the target release, apply it
// (or report what a dry run would do), and keep the command layout correct.
func (c Config) applyUpdate(ctx context.Context, l Layout, opts UpdateOptions) (*UpdateResult, error) {
	targetVersion, dryRun := opts.TargetVersion, opts.DryRun
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
		// Already current is not the same as already CORRECT. An install that
		// updated itself under the alias name has no primary binary at all, an
		// alias can go missing under a healthy primary, and an alias copy can be
		// left behind a version; none of those states fixes itself, and waiting
		// for the next release to fix them would leave the `digitalx` command
		// absent for as long as no release ships.
		//
		// A dry run must still change nothing, so the fixes are PLANNED read-only
		// and only applied when this is not a dry run.
		plan := c.planLayout(l)
		if dryRun {
			c.log().Debug("self update dry-run: already current", "version", target, "wouldRepair", len(plan.descriptions(l)))
			res.CheckedOnly = true
			res.LayoutRepaired = plan.descriptions(l)
			return res, nil
		}
		// Apply under the same lock a real update takes.
		if err := c.withLock(l, func() error {
			c.repairLayout(l, res)
			return nil
		}); err != nil {
			return nil, err
		}
		return res, nil
	}
	if dryRun {
		// Authenticate the whole download — signature, manifest, archive hash —
		// and stop before the transfer, so a dry run cannot answer "an update is
		// available" for a release nobody can install. No lock is taken: nothing
		// is written.
		asset, _, sigCheck, err := c.resolveDownload(ctx, target, l)
		if err != nil {
			return nil, err
		}
		c.log().Debug("self update dry-run: update available, not applying", "from", c.Version, "to", target, "archive", asset.Archive)
		res.CheckedOnly = true
		res.Archive = asset.Archive
		res.SignatureCheck = sigCheck
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

	asset, want, sigCheck, err := c.resolveDownload(ctx, target, l)
	if err != nil {
		return nil, err
	}
	res.Archive = asset.Archive

	// Download the archive and verify it against the authenticated checksum, then
	// extract the binary.
	c.progressf("downloading %s %s…", c.repo(), target)
	archive, err := c.fetch(ctx, c.downloadURL(target, asset.Archive))
	if err != nil {
		return nil, err
	}
	if got := sha256Bytes(archive); got != want {
		c.log().Debug("archive checksum mismatch", "asset", asset.Archive, "expected", want, "got", got)
		return nil, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset.Archive, want, got)
	}
	c.log().Debug("archive checksum verified", "asset", asset.Archive, "bytes", len(archive))
	binBytes, err := extractBinary(archive, c.os(), asset.Binary)
	if err != nil {
		return nil, err
	}
	newSum := sha256Bytes(binBytes)
	c.log().Debug("binary extracted from archive", "binary", asset.Binary, "bytes", len(binBytes), "sha256", newSum)

	// Replace the installed binary in place, BEFORE any alias is touched. On the
	// layout where the running binary is the alias name and the primary name is not
	// on disk yet, writing the primary first means the alias is only ever replaced
	// once there is a working binary for it to point at. minio handles the Windows
	// running-exe move; OldSavePath is empty so it manages and cleans up its own
	// outgoing-binary swap file. We don't pass Options.Checksum: it is a pre-write guard over the
	// same bytes we hand to Apply, so it could only compare a hash of binBytes
	// against a hash of binBytes. The meaningful verification already happened above
	// — the archive was checked against the signature-authenticated checksum — and
	// no independent hash of the extracted binary exists to re-check here.
	c.log().Debug("applying in-place binary swap", "path", l.ExecutablePath())
	if err := c.placePrimary(l, binBytes); err != nil {
		return nil, err
	}

	// The primary binary is in place; now bring every alias command name this
	// install OWNS onto it, so both names run the version just installed. An
	// install with no alias gets none. A failed alias write is a warning, not a
	// failure: the primary is already updated and usable, and the next
	// install/update retries. The manifest read here is the PRIOR one — the new
	// one is written below — which is what records an already-adopted alias.
	exe, _ := c.runningExe()
	prevManifest, _, _ := loadManifest(l.ManifestPath())
	wanted, ownWarnings := c.aliasesToKeep(l, prevManifest, exe)
	owned := c.managedAliasNames(l, wanted)
	aliases, aliasWarnings := c.syncAliases(l, owned)
	res.Aliases = aliases
	res.Warnings = append(res.Warnings, ownWarnings...)
	res.Warnings = append(res.Warnings, aliasWarnings...)
	for _, w := range res.Warnings {
		c.progressf("warning: %s", w)
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
		// The names this install OWNS, not the ones written: see syncAliases.
		Aliases: owned,
	}
	if err := m.save(l.ManifestPath()); err != nil {
		return nil, err
	}
	// Clear the swap/scratch files the in-place replacements just left behind —
	// minio's .<bin>.old on the alias copy path (windows), and any temp file an
	// interrupted write dropped. Best-effort and last: the update itself has
	// already succeeded, and a leftover hidden file is untidy, not broken.
	if swept := c.sweepLeftovers(l); swept {
		c.log().Debug("swept leftover temp/swap files after update", "dir", l.ExecutableDir())
	}

	c.log().Debug("self update applied", "from", c.Version, "to", target, "sha256", newSum, "signatureCheck", sigCheck)
	res.Updated = true
	res.SHA256 = newSum
	res.SignatureCheck = sigCheck
	return res, nil
}

// withLock runs fn holding the advisory lock that serializes install / update /
// uninstall against each other, creating the home first (the lock lives in it).
func (c Config) withLock(l Layout, fn func() error) error {
	if err := os.MkdirAll(l.Home(), 0o700); err != nil {
		return err
	}
	unlock, err := fslock.Lock(l.LockPath())
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// layoutRepair is the set of command-name fixes an install needs when there is
// no new version to install. It is computed READ-ONLY by planLayout, so
// `self update --dry-run` can report exactly what a real run would change
// without touching a file, and applied by repairLayout.
type layoutRepair struct {
	// exe is the running binary; empty when it could not be resolved, which
	// makes the whole repair a no-op (nothing can be proved about the layout).
	exe string
	// manifest is the manifest as it stands, the basis for the rewrite.
	manifest Manifest
	// createPrimary marks that the primary command name is absent and the running
	// binary can supply it.
	createPrimary bool
	// aliases are the owned alias names that must be (re)written.
	aliases []string
	// owned is every alias name this install owns — what the manifest records,
	// whether or not each one needed writing.
	owned []string
	// warnings are the ownership warnings the plan surfaced (a file at the alias
	// name that this install does not own is reported, never touched).
	warnings []string
}

// primaryIsStale reports whether the file at the primary name is NOT the binary
// this install recorded, while the RUNNING alias-named binary is.
//
// Both halves are required, and each rules out a wrong repair:
//
//   - The running binary must be verified against the manifest, so its bytes are
//     known to be this install's current version. Without that check a hand-built
//     or older `korbit` on PATH would overwrite a perfectly good `digitalx`.
//   - The primary must fail the same check. A primary that matches is healthy,
//     whatever else is on disk.
//
// With no recorded sha256 there is nothing to validate either side against, so
// this reports false: an unprovable state is never repaired by overwriting a
// binary on a guess.
func (c Config) primaryIsStale(l Layout, m Manifest, exe string) bool {
	if m.SHA256 == "" || !fileExists(l.ExecutablePath()) {
		return false
	}
	// The alias-name file must be a real, separate binary. On unix a healthy alias
	// is a symlink onto the primary, so both paths resolve to the same file and
	// hash identically; isInstalledBinary tells that case apart.
	if l.isInstalledBinary(exe) {
		return false
	}
	if !fileHasSum(exe, m.SHA256) {
		return false // the running binary is not the recorded one either
	}
	return !fileHasSum(l.ExecutablePath(), m.SHA256)
}

// createdPrimaryLine / pointedAliasLine are the human lines for the two fixes a
// layout repair makes. They are shared by the plan's descriptions (what a dry
// run WOULD do) and repairLayout's report (what it did), so the two cannot drift.
func createdPrimaryLine(l Layout) string {
	return fmt.Sprintf("created the `%s` command at %s from the running `%s`", l.BinName(), l.ExecutablePath(), l.LegacyBinName())
}

func pointedAliasLine(l Layout, name string) string {
	return fmt.Sprintf("pointed the `%s` command at %s", name, l.BinName())
}

// descriptions are the human lines for this repair, in the order it applies
// them. Empty means the layout is already correct.
func (r layoutRepair) descriptions(l Layout) []string {
	var out []string
	if r.createPrimary {
		out = append(out, createdPrimaryLine(l))
	}
	for _, name := range r.aliases {
		out = append(out, pointedAliasLine(l, name))
	}
	return out
}

// planLayout works out what an install's command names need, changing nothing.
//
// Three states need a fix, and none of them is repaired by a new release:
//
//   - The primary name is ABSENT because an install placed under the alias name
//     updated itself in place. The primary is the name the docs, the installers,
//     and every example use, so the running binary's own bytes become it — which
//     is exactly right, since they ARE the current version.
//   - The primary name is PRESENT but STALE: the running alias-named binary's
//     bytes are the manifest's and the file at the primary name's are not (see
//     primaryIsStale). The `digitalx` command then runs something this install did
//     not place — a copy an interrupted repair left behind, or another install's
//     binary — and it is recreated from the running, verified bytes. It is never
//     ADOPTED: blessing it would record the wrong hash and make every later check
//     compare against the wrong binary.
//   - An owned alias is GONE (deleted, or left behind by an interrupted write).
//   - An owned alias is PRESENT but does not run the primary — a link pointing
//     elsewhere, or a copy left a version behind. This is the state that looks
//     like nothing at all: the command works, and silently runs the wrong
//     binary. It is checked with the same predicate doctor reports it by
//     (aliasRunsPrimary), so the problem doctor names and the fix this applies
//     cannot disagree.
//
// A VALID alias is deliberately absent from the plan, so an already-correct
// layout writes nothing on every update check.
func (c Config) planLayout(l Layout) layoutRepair {
	exe, err := c.runningExe()
	if err != nil {
		c.log().Debug("cannot plan a layout repair: the running binary is unresolvable", "err", err.Error())
		return layoutRepair{}
	}
	m, _, _ := loadManifest(l.ManifestPath())
	r := layoutRepair{exe: exe, manifest: m}
	r.createPrimary = l.isLegacyBinary(exe) &&
		(!fileExists(l.ExecutablePath()) || c.primaryIsStale(l, m, exe))

	wanted, warnings := c.aliasesToKeep(l, m, exe)
	r.owned = c.managedAliasNames(l, wanted)
	r.warnings = warnings
	for _, name := range r.owned {
		p := l.AliasPath(name)
		switch {
		case r.createPrimary:
			// The primary this run creates is what the alias must point at.
			r.aliases = append(r.aliases, name)
		case !pathPresent(p):
			r.aliases = append(r.aliases, name)
		case !c.aliasRunsPrimary(l, m, p, symlinkTarget(p)):
			c.log().Debug("alias does not run the installed binary", "alias", name, "path", p)
			r.aliases = append(r.aliases, name)
		}
	}
	return r
}

// repairLayout applies planLayout's fixes, recording each in res.LayoutRepaired
// (and leaving res.Updated false — nothing was updated). It must be called
// under the install lock, and never on a dry run.
func (c Config) repairLayout(l Layout, res *UpdateResult) {
	plan := c.planLayout(l)
	if plan.exe == "" {
		return
	}
	m := plan.manifest
	res.Warnings = append(res.Warnings, plan.warnings...)

	// 1. The primary name, from the running alias-named binary's own bytes.
	if plan.createPrimary {
		if err := copyFileAtomic(l.ExecutablePath(), plan.exe, 0o755); err != nil {
			c.log().Debug("could not create the primary binary during a layout repair", "path", l.ExecutablePath(), "err", err.Error())
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not create the `%s` command at %s (%v) — run the install one-liner again to repair it", l.BinName(), l.ExecutablePath(), err))
			return
		}
		res.LayoutRepaired = append(res.LayoutRepaired, createdPrimaryLine(l))
		c.log().Debug("created the primary binary from the running alias-named binary", "from", plan.exe, "to", l.ExecutablePath())
	}

	// 2. Every alias name the plan found wanting, pointed at the primary.
	if len(plan.aliases) > 0 {
		written, aliasWarnings := c.syncAliases(l, plan.aliases)
		res.Warnings = append(res.Warnings, aliasWarnings...)
		for _, name := range written {
			res.LayoutRepaired = append(res.LayoutRepaired, pointedAliasLine(l, name))
		}
	}
	if len(plan.owned) > 0 {
		res.Aliases = c.presentAliases(l, plan.owned)
	}

	// 3. The manifest, so it describes the layout that is now on disk. Only when
	// something changed, and only over the fields this repair is responsible for.
	if len(res.LayoutRepaired) == 0 && slices.Equal(m.Aliases, plan.owned) {
		return
	}
	owned := plan.owned
	m.Method = MethodManagedScript
	m.Executable = l.ExecutablePath()
	// The names this install OWNS, so a name whose write failed is retried
	// rather than forgotten (see syncAliases).
	m.Aliases = owned
	if m.Version == "" {
		m.Version = c.Version
	}
	if m.OS == "" {
		m.OS = c.os()
	}
	if m.Arch == "" {
		m.Arch = c.arch()
	}
	if m.Repo == "" {
		m.Repo = c.repo()
	}
	// Record the primary's hash only when it is TRUSTWORTHY: either this repair
	// just wrote those bytes, or they already match what the manifest recorded, or
	// the manifest carries no hash to contradict. A primary that hashes
	// differently from a verified running alias is stale, and recording its hash
	// would bless it — every later check (doctor's binary check, the alias-copy
	// comparison, the next repair) would then compare against the wrong binary and
	// call the stale one healthy.
	if sum, err := sha256File(l.ExecutablePath()); err == nil && (plan.createPrimary || m.SHA256 == "" || sum == m.SHA256) {
		m.SHA256 = sum
		res.SHA256 = sum
	}
	if err := m.save(l.ManifestPath()); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not record the repaired layout in %s (%v)", l.ManifestPath(), err))
		return
	}
	if len(res.LayoutRepaired) > 0 {
		res.LayoutRepaired = append(res.LayoutRepaired, "recorded the layout in the install manifest")
	}
	if swept := c.sweepLeftovers(l); swept {
		c.log().Debug("swept leftover temp/swap files after a layout repair", "dir", l.ExecutableDir())
	}
}

// presentAliases returns the subset of names that are on disk now — what the
// result reports as working command names, as against the owned names the
// manifest records.
func (c Config) presentAliases(l Layout, names []string) []string {
	var out []string
	for _, name := range names {
		if pathPresent(l.AliasPath(name)) {
			out = append(out, name)
		}
	}
	return out
}

// placePrimary writes the freshly extracted binary to the primary path: through
// minio/selfupdate when a binary is already there, and as a plain atomic create
// when the primary name is absent (the alias-only layout) — the same split, for
// the same reason, as applyCopy.
func (c Config) placePrimary(l Layout, binBytes []byte) error {
	if !fileExists(l.ExecutablePath()) {
		if err := writeBytesAtomic(l.ExecutablePath(), binBytes, 0o755); err != nil {
			return fmt.Errorf("installing the binary to %s: %w", l.ExecutablePath(), err)
		}
		c.log().Debug("created the primary binary", "path", l.ExecutablePath())
		return nil
	}
	err := selfupdate.Apply(bytes.NewReader(binBytes), selfupdate.Options{
		TargetPath: l.ExecutablePath(),
		TargetMode: 0o755,
	})
	if err == nil {
		return nil
	}
	if rerr := selfupdate.RollbackError(err); rerr != nil {
		c.log().Debug("in-place swap failed and rollback failed", "err", err.Error(), "rollbackErr", rerr.Error())
		return fmt.Errorf("update failed and rollback also failed: %v (rollback: %v)", err, rerr)
	}
	c.log().Debug("in-place swap failed, rolled back", "err", err.Error())
	return fmt.Errorf("applying the update: %w", err)
}

// assertManaged verifies the running binary is a managed-script install: a
// manifest with method managed-script exists AND the running executable is a
// binary this install owns on PATH — the primary name, or its alias name in the
// same directory (Layout.isManagedBinary), so an install invoked under either
// name manages itself. Otherwise it returns a ProvenanceError with
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
	if !l.isManagedBinary(exe) {
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
