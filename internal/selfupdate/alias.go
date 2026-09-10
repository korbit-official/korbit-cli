// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/minio/selfupdate"
)

// An ALIAS is a second command name on PATH, in the install dir, that runs the
// primary binary. Layout.LegacyBinName ("korbit") is the one alias this package
// manages, so both command names invoke the same version.
//
// The two platforms need different mechanisms, and both are exact about what a
// caller can rely on:
//
//   - unix: a RELATIVE symlink (korbit -> dgx-cli). Relative so the pair still
//     resolves if the bin dir is moved or reached through a symlinked home, and
//     so an update of the primary is instantly an update of the alias — there is
//     nothing to refresh. It is written under a temp name and renamed into place,
//     so the alias is never absent mid-write.
//   - windows: a second COPY. An unprivileged Windows install cannot create a
//     file symlink (that needs admin or Developer Mode), so the copy is refreshed
//     on every install/update. The copy may itself be the running executable;
//     minio/selfupdate renames the target aside before writing, which Windows
//     permits for a running image, and rolls back on failure.
//
// Adoption is CONDITIONAL, and the condition is OWNERSHIP, not mere presence —
// see ownsLegacyAlias. The install dir is a plain directory on the user's PATH,
// so an unrelated `korbit` sitting in it may be their own wrapper script or a
// hand-built binary. Overwriting that with a symlink, and later deleting it on
// uninstall, would destroy a file that was never ours, so an unowned name is
// left strictly alone and reported. A fresh install owns nothing at that name
// and so ends up with the primary binary alone.

// ownsLegacyAlias reports whether the alias name in the install dir belongs to
// THIS install, which is what makes it safe to overwrite and to delete. Any one
// of four independent proofs suffices, between them covering every shape a
// managed install can be in:
//
//   - the manifest records the name as an alias — an install that already
//     adopted it;
//   - the manifest's own executable IS that path — the alias-only layout, whose
//     manifest was written while that name was the primary;
//   - the running binary is that path — the same layout, reached before any
//     manifest says so (a repaired or rebuilt install);
//   - its bytes hash to the sha256 the manifest recorded — it is, byte for byte,
//     the binary this install placed. A unix alias symlink hashes through to the
//     primary, so this also recognizes an alias whose manifest entry was lost.
//
// Anything else at that name is someone else's file.
//
// The manifest proof compares the recorded name for EQUALITY against the one
// managed name, so a manifest carrying anything else — a relative path, another
// command's name — proves nothing here. See managedAliasNames for why every
// other consumer of that field filters it the same way.
func (c Config) ownsLegacyAlias(l Layout, m Manifest, runningExe string) bool {
	aliasPath := l.LegacyExecutablePath()
	switch {
	case slices.Contains(m.Aliases, l.LegacyBinName()):
		return true
	case m.Executable != "" && resolveOrClean(m.Executable) == resolveOrClean(aliasPath):
		return true
	case l.isLegacyBinary(runningExe):
		return true
	case m.SHA256 != "" && fileHasSum(aliasPath, m.SHA256):
		return true
	}
	return false
}

// aliasesToKeep returns the alias names this install OWNS and should therefore
// keep pointing at the primary binary, plus a warning for a name occupied by a
// file this install does not own (left untouched — the user put it there). No
// names for an install with no alias, which is what a fresh install produces. A
// name the manifest records but that is missing from disk is still returned, so
// a re-run recreates the command instead of silently dropping it.
func (c Config) aliasesToKeep(l Layout, m Manifest, runningExe string) (names, warnings []string) {
	if c.ownsLegacyAlias(l, m, runningExe) {
		return []string{l.LegacyBinName()}, nil
	}
	if pathPresent(l.LegacyExecutablePath()) {
		c.log().Debug("left an unowned file at the alias name", "path", l.LegacyExecutablePath())
		return nil, []string{fmt.Sprintf("%s exists but is not a managed install; left alone", l.LegacyExecutablePath())}
	}
	return nil, nil
}

// managedAliasNames filters a list of alias names — a manifest's `aliases`
// field, or anything derived from it — down to the one name this package
// manages, logging and dropping the rest.
//
// The filter is a safety boundary, not tidiness. Every alias name is JOINED
// ONTO THE INSTALL DIR to produce a path that install overwrites and uninstall
// deletes, so an entry like "../config.json" would reach outside that directory
// entirely. The manifest is a plain file in the user's home that any process
// can rewrite, so its contents are input to be validated, not a source of
// truth about which paths are ours.
func (c Config) managedAliasNames(l Layout, names []string) []string {
	var out []string
	for _, name := range names {
		if name != l.LegacyBinName() {
			c.log().Debug("ignoring an unmanaged alias name recorded in the manifest", "name", name, "managed", l.LegacyBinName())
			continue
		}
		out = append(out, name)
	}
	return out
}

// syncAliases writes/refreshes every alias in names so it runs the primary
// binary, and returns the names that are now in place plus a warning per alias
// that could not be written. It is called AFTER the primary binary is in place:
// on the alias-only layout the primary is what the alias will point at, and
// writing it first means neither name is ever missing.
//
// The returned list is what an install actually achieved, which is NOT what the
// manifest records: the manifest records every name the install OWNS
// (aliasesToKeep), including one whose write just failed, so the next run
// retries it and doctor reports it as a broken command in the meantime. A
// manifest that only ever listed successful writes would silently forget the
// alias on its first transient failure.
func (c Config) syncAliases(l Layout, names []string) (kept []string, warnings []string) {
	for _, name := range c.managedAliasNames(l, names) {
		if err := c.writeAlias(l, name); err != nil {
			c.log().Debug("could not write alias", "alias", name, "err", err.Error())
			warnings = append(warnings, fmt.Sprintf("could not keep the `%s` command pointing at %s (%v) — run the install one-liner again to repair it", name, l.BinName(), err))
			continue
		}
		c.log().Debug("alias in place", "alias", name, "target", l.BinName())
		kept = append(kept, name)
	}
	return kept, warnings
}

// writeAlias puts one alias in the install dir pointing at the primary binary,
// replacing whatever is there. See the package-level note above for why unix
// gets a relative symlink and windows a refreshed copy.
func (c Config) writeAlias(l Layout, name string) error {
	if name == l.BinName() {
		// Compared by NAME, not by resolved path: once the alias is a symlink onto
		// the primary the two paths resolve to the same file, which is the healthy
		// state, not a collision.
		return fmt.Errorf("alias %q is the primary binary's own name", name)
	}
	aliasPath := l.AliasPath(name)
	if c.os() == "windows" {
		return applyCopy(l.ExecutablePath(), aliasPath)
	}
	return symlinkAtomic(l.BinName(), aliasPath)
}

// applyCopy writes primary's bytes to aliasPath as a second copy of the binary.
//
// The mechanism depends on whether anything is at aliasPath, because
// minio/selfupdate starts by renaming the existing target aside — that rename
// is exactly how a RUNNING Windows executable gets replaced, and it also rolls
// the old file back if the write fails. With nothing at aliasPath there is
// nothing to rename and Apply fails, so an absent target takes a plain atomic
// create instead: nothing is running at a path that does not exist, so there is
// neither a swap to perform nor a state to roll back to.
func applyCopy(primary, aliasPath string) error {
	if !pathPresent(aliasPath) {
		return copyFileAtomic(aliasPath, primary, 0o755)
	}
	f, err := os.Open(primary)
	if err != nil {
		return err
	}
	defer f.Close()
	return selfupdate.Apply(f, selfupdate.Options{TargetPath: aliasPath, TargetMode: 0o755})
}

// symlinkAtomic points aliasPath at target (a bare name, resolved relative to
// aliasPath's own directory) by creating the link under a temp name in that
// directory and renaming it over aliasPath — so a crash mid-write leaves either
// the old alias or the new one, never nothing. rename replaces an existing
// symlink or regular file in one step.
func symlinkAtomic(target, aliasPath string) error {
	dir := filepath.Dir(aliasPath)
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	// os.Symlink refuses an existing name, so clear the placeholder CreateTemp
	// made; it only reserved a name no concurrent run will reuse.
	if err := os.Remove(tmpName); err != nil {
		return err
	}
	if err := os.Symlink(target, tmpName); err != nil {
		return err
	}
	if err := os.Rename(tmpName, aliasPath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// pathPresent reports whether path exists as anything at all — a regular file or
// a symlink, including one whose target is missing. Alias checks use it rather
// than fileExists because an alias is a symlink on unix, and a dangling one is
// still a name occupying PATH that must be repaired rather than ignored.
func pathPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// symlinkTarget returns the target path stored in the symlink at path, or "" if
// path is not a symlink. It is what self doctor shows so a user can see which
// binary an alias runs.
func symlinkTarget(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return target
}
