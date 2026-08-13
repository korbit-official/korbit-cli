// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/fslock"
)

// FailedRemoval is one thing uninstall could not remove or edit, with why — so
// the summary tells the user exactly what is left and what to do about it.
type FailedRemoval struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// UninstallResult reports what `self uninstall` did. Each piece is chosen by the
// caller (the interactive selection lives in the cli layer); an empty field means
// that piece was not selected or nothing of it existed.
type UninstallResult struct {
	// Removed lists files/dirs actually deleted (binary, manifest, data files,
	// caches, the emptied CLI home) plus a line per cleared API key.
	Removed []string `json:"removed,omitempty"`
	// Edited lists the PATH locations whose installer change was undone (a shell rc
	// file's managed block, or the User PATH entry).
	Edited []string `json:"edited,omitempty"`
	// Failed lists things that could not be removed/edited, with a reason (a locked
	// binary, a permission error, a non-empty dir, a failed edit).
	Failed []FailedRemoval `json:"failed,omitempty"`
	// KeptPaths lists PATH locations left as-is (the user declined, or none was
	// selected) so the summary can tell the user to edit them by hand. Set by the
	// cli layer, which knows what was offered vs accepted.
	KeptPaths []string `json:"keptPaths,omitempty"`
	// Warnings are non-fatal problems that aren't a specific failed removal (a
	// sandbox that could not be stopped, a home-containing cache refused).
	Warnings []string `json:"warnings,omitempty"`
}

// UninstallOptions selects what Uninstall removes. The interactive front-end (the
// cli layer) resolves the candidate paths, shows the previews, collects the
// user's choices, and passes them here; this layer does the filesystem work under
// one lock and reports the outcome.
type UninstallOptions struct {
	// RemoveBinary removes the installed binary, the install manifest, and any
	// leftover swap file. A binary that cannot delete itself (the locked running
	// korbit.exe on Windows) is reported in Failed instead.
	RemoveBinary bool
	// RemoveData removes the CLI-home data files in DataPaths (config.json, the
	// journal) and, when the home is then empty, the home dir. Key material is
	// cleared first via ClearKeys.
	RemoveData bool
	// RemoveCaches removes the regenerable sandbox artifacts in Artifacts.
	RemoveCaches bool

	// Artifacts are the regenerable sandbox artifact dirs (resolved by the caller).
	// Removed only under RemoveCaches; non-existent entries ignored.
	Artifacts []string
	// DataPaths are the CLI-home data files to remove under RemoveData (config +
	// journal + sidecars). The key registry/vault are removed by ClearKeys instead.
	DataPaths []string
	// EditPaths are the PATH locations (from PathEdits) the user accepted undoing.
	EditPaths []string

	// ClearKeys clears every key's material from its backend and removes the key
	// files, returning removed lines + warnings. Called under the lock before
	// deleting DataPaths (RemoveData only); nil = skip. It lives in the cli layer
	// because it needs the key manager; running it here keeps every destructive
	// step under the one lock and in one returned result.
	ClearKeys func() (removed, warnings []string)
	// StopSandbox stops a sandbox running under the default state dir before its
	// caches are removed (RemoveCaches only); nil = skip. A stop error is a warning.
	StopSandbox func() error
}

// AssertManaged reports whether this is a managed install self uninstall can
// operate on (the running binary is the one the managed install script placed,
// with a matching manifest). The interactive front-end calls it BEFORE asking any
// removal question or clearing keys, so a non-managed install is refused before
// any destructive work — Uninstall re-checks it under the lock as a backstop.
func (c Config) AssertManaged() error { return c.assertManaged(c.Layout()) }

// PathEdits returns the pending "undo the installer's PATH change" edits, one per
// location that currently carries the installer's entry (each shell rc file with
// the managed block on unix; the User PATH on windows), each with a diff preview.
// Empty when there is nothing to undo. It edits nothing.
func (c Config) PathEdits() []PathEdit { return c.pathEdits() }

// Uninstall removes the parts of a managed install the caller selected, all under
// one lock. It refuses on a non-managed install before touching anything. Once
// past the lock it never returns an error — every problem is recorded in the
// result (Failed/Warnings) so a partial removal is always fully reported.
func (c Config) Uninstall(opts UninstallOptions) (*UninstallResult, error) {
	l := c.Layout()
	c.log().Debug("self uninstall starting", "binary", opts.RemoveBinary, "data", opts.RemoveData, "caches", opts.RemoveCaches, "edits", opts.EditPaths)
	if err := c.assertManaged(l); err != nil {
		c.log().Debug("self uninstall refused: not a managed install", "err", err.Error())
		return nil, err
	}
	unlock, err := fslock.Lock(l.LockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &UninstallResult{}
	if opts.RemoveBinary {
		c.removeBinary(l, res)
	}
	if opts.RemoveData {
		if opts.ClearKeys != nil {
			rm, warn := opts.ClearKeys()
			res.Removed = append(res.Removed, rm...)
			res.Warnings = append(res.Warnings, warn...)
		}
		for _, p := range opts.DataPaths {
			removePath(p, res)
		}
	}
	if opts.RemoveCaches {
		if opts.StopSandbox != nil {
			if err := opts.StopSandbox(); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("could not stop the running sandbox before removing its caches (%v) — a live sandbox may have been left running; stop it manually", err))
			}
		}
		for _, p := range opts.Artifacts {
			c.removeCache(p, l, res)
		}
	}
	for _, loc := range opts.EditPaths {
		c.editPath(loc, res)
	}
	// After clearing data, remove the CLI home if it is now empty (only korbit's
	// own lock files may remain).
	if opts.RemoveData {
		c.pruneHome(l, res)
	}
	c.log().Debug("self uninstall complete", "removed", res.Removed, "edited", res.Edited, "failed", res.Failed)
	return res, nil
}

// removeBinary deletes the installed binary, leftover swap files, and the
// manifest. On Windows the running korbit.exe is locked and cannot delete itself;
// that is recorded in Failed for manual deletion, not swallowed.
func (c Config) removeBinary(l Layout, res *UninstallResult) {
	if fileExists(l.ExecutablePath()) {
		if err := os.Remove(l.ExecutablePath()); err == nil {
			res.Removed = append(res.Removed, l.ExecutablePath())
		} else {
			c.log().Debug("could not remove installed binary", "path", l.ExecutablePath(), "err", err.Error())
			res.Failed = append(res.Failed, FailedRemoval{l.ExecutablePath(), "in use — delete it manually after this process exits"})
		}
	}
	for _, swap := range []string{"." + l.BinName() + ".old", "." + l.BinName() + ".new"} {
		_ = os.Remove(filepath.Join(l.ExecutableDir(), swap))
	}
	if fileExists(l.ManifestPath()) {
		if err := os.Remove(l.ManifestPath()); err == nil {
			res.Removed = append(res.Removed, l.ManifestPath())
		} else {
			res.Failed = append(res.Failed, FailedRemoval{l.ManifestPath(), err.Error()})
		}
	}
}

// removePath removes one file/dir (a data file), recording success or the reason
// it failed. A path that does not exist is silently skipped.
func removePath(p string, res *UninstallResult) {
	if !fileExists(p) && !dirExists(p) {
		return
	}
	if err := os.RemoveAll(p); err == nil {
		res.Removed = append(res.Removed, p)
	} else {
		res.Failed = append(res.Failed, FailedRemoval{p, err.Error()})
	}
}

// removeCache removes one regenerable cache dir, refusing any dir that IS or
// contains the CLI home (a misconfigured cache path) so config/keys/journal can
// never be wiped by it.
func (c Config) removeCache(p string, l Layout, res *UninstallResult) {
	if !dirExists(p) {
		return
	}
	if pathContainsOrEquals(p, l.Home()) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("did not remove %s — it contains your CLI home, so removing it would delete your config, keys, and journal", p))
		return
	}
	if err := os.RemoveAll(p); err == nil {
		res.Removed = append(res.Removed, p)
	} else {
		res.Failed = append(res.Failed, FailedRemoval{p, err.Error()})
	}
}

// editPath undoes the installer's PATH change at one location, recording it in
// Edited on success or Failed (with a fix-it-yourself reason) on error. A skip
// (removeBlockFrom refused to rewrite a hard-linked or foreign-owned file, to
// avoid changing its identity) and a no-op (the block was already gone by the
// time the lock was held) are both surfaced as warnings, so an accepted edit
// never vanishes silently from the summary.
func (c Config) editPath(loc string, res *UninstallResult) {
	changed, err := c.applyPathEdit(loc)
	var skip *skipEditError
	switch {
	case errors.As(err, &skip):
		res.Warnings = append(res.Warnings, skip.Error()+"; remove it by hand")
	case err != nil:
		res.Failed = append(res.Failed, FailedRemoval{loc, "could not undo the korbit-cli PATH change (" + err.Error() + ") — edit it manually"})
	case changed:
		res.Edited = append(res.Edited, loc)
	default:
		res.Warnings = append(res.Warnings, fmt.Sprintf("no korbit-cli block found in %s — nothing to undo", loc))
	}
}

// pruneHome removes the CLI home dir only when the sole things left in it are
// korbit's own lock files — i.e. the user removed everything else. If any real
// file remains (a kept manifest because the binary wasn't removed, a kept
// sandbox/ because caches weren't removed, or a file whose own removal already
// failed and was reported), the home is left in place silently: those are
// deliberate keeps, not a failure the user must act on. A best-effort removal
// that can't complete (a lock still held on Windows) is likewise left silent —
// the leftover is just korbit's own lock file.
func (c Config) pruneHome(l Layout, res *UninstallResult) {
	home := l.Home()
	entries, err := os.ReadDir(home)
	if err != nil {
		return // already gone, or unreadable — nothing to prune
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".lock") {
			return // real files kept — leave the home dir, don't report it
		}
	}
	if err := os.RemoveAll(home); err == nil {
		res.Removed = append(res.Removed, home)
	}
}

// pathContainsOrEquals reports whether child is parent itself or nested under it
// (both cleaned first), so a cache removal can refuse an artifact dir that would
// take the CLI home down with it.
func pathContainsOrEquals(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
