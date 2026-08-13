// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
	"syscall"
)

// unsafeToRewrite reports why an atomic temp+rename rewrite of the file described
// by fi would not faithfully recreate it — a non-empty reason means removeBlockFrom
// must skip the edit rather than change the file's identity. A rewrite mints a new
// inode owned by this process, so it breaks hard links and takes on our uid/gid;
// refuse in exactly those cases. Empty means the rewrite is safe.
func unsafeToRewrite(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "" // no unix metadata to check — rewrite as before
	}
	if st.Nlink > 1 {
		return "it has other hard links a rewrite would break"
	}
	// euid/egid is a conservative proxy for the new inode's owner: a new file's
	// group can instead follow the parent dir (BSD/macOS), so this may skip a file
	// the rewrite would in fact preserve — erring toward leaving it untouched.
	if int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
		return "it is owned by a different user or group a rewrite would change"
	}
	return ""
}

// effectivelyOnPath reports whether dir is on PATH. On unix a just-edited rc
// file only takes effect in a new shell, and there is no authoritative persisted
// list to consult, so the current process PATH is the honest answer — doctor run
// in the same shell right after install correctly reports "not yet" until the
// shell is restarted.
func (c Config) effectivelyOnPath(dir string) bool { return c.dirOnPath(dir) }

// wirePath ensures the installed binary dir is on PATH on unix. If it already is,
// it does nothing. Otherwise, when a confirm is available (c.PathConfirm, backed
// by /dev/tty for the `curl | sh` case) it shows a diff preview and asks PER
// shell startup file (see Layout.rcFiles — the interactive rc files and the login
// profiles, so desktop apps resolving PATH via a login shell see it too),
// appending an idempotent managed block to each file the user accepts. With no
// TTY (or every file declined) it leaves dotfiles untouched and reports the exact
// line to add — the install still succeeds; PATH is a convenience, not a failure.
func (c Config) wirePath(dir string) (PathResult, error) {
	l := c.Layout()
	pr := PathResult{Dir: dir}
	if c.effectivelyOnPath(dir) {
		pr.OnPath = true
		pr.Action = pathActionAlready
		c.log().Debug("PATH already configured", "dir", dir)
		return pr, nil
	}
	// exportLine is the plain one-liner shown in human hints (running it once by
	// hand needs no dedup guard); body is the guarded block actually written to the
	// rc files, which may each be sourced together (see pathBlockBody).
	shellDir := l.homeForm(dir)
	exportLine := fmt.Sprintf(`export PATH="%s:$PATH"`, shellDir)
	body := pathBlockBody(shellDir)

	// Ask per file that still lacks the block, appending to each the user accepts.
	if c.PathConfirm != nil {
		var edited []string
		for _, add := range c.pathAdditions(dir) {
			ok, err := c.PathConfirm(add)
			if err != nil {
				return pr, err
			}
			if !ok {
				continue
			}
			wrote, werr := appendBlockTo(add.Location, body)
			if werr != nil {
				return pr, fmt.Errorf("updating %s: %w", add.Location, werr)
			}
			if wrote {
				edited = append(edited, add.Location)
			}
		}
		if len(edited) > 0 {
			pr.Action = pathActionEditedRC
			pr.Files = edited
			pr.Hint = "restart your shell, or run: " + exportLine
			c.log().Debug("wired PATH into shell startup files", "dir", dir, "edited", edited)
			return pr, nil
		}
	}
	// Nothing was written this run. If ANY startup file already carries the block —
	// one the loop skipped as already-wired, or one an earlier run wrote — the dir
	// IS set up and only needs a fresh shell; report that rather than telling the
	// user (misleadingly) that it is missing.
	if len(c.pathEdits()) > 0 {
		pr.Action = pathActionEditedRC
		pr.Hint = "restart your shell, or run: " + exportLine
		c.log().Debug("PATH already wired on disk; needs a shell restart", "dir", dir)
		return pr, nil
	}
	// Non-interactive, or the user declined and nothing is wired anywhere: guidance.
	pr.Action = pathActionInstructions
	pr.Hint = "add this line to your shell profile: " + exportLine
	c.log().Debug("PATH not wired, printed instructions", "dir", dir, "interactive", c.PathConfirm != nil)
	c.progressf("%s is not on your PATH. Add this line to your shell profile (~/.bashrc, ~/.zshrc):\n  %s", dir, exportLine)
	return pr, nil
}

// pathAdditions returns a pending PATH-add edit for each shell rc file that does
// not yet carry the managed block, each with a line-numbered diff-with-context
// preview — so install can show exactly what it would add and ask per file.
// Empty when dir is already on PATH (nothing to wire) or every rc file already
// has the block. It edits nothing.
func (c Config) pathAdditions(dir string) []PathAddition {
	if c.effectivelyOnPath(dir) {
		return nil
	}
	l := c.Layout()
	body := pathBlockBody(l.homeForm(dir))
	var adds []PathAddition
	for _, rc := range l.rcFiles() {
		if a := additionFor(rc, body); a != nil {
			adds = append(adds, *a)
		}
	}
	return adds
}

// pathEdits returns a pending PATH-undo edit for each shell rc file that carries
// the managed block, with a line-numbered diff preview — so uninstall can show
// exactly what it would remove and ask per file. Files without the block are
// skipped.
func (c Config) pathEdits() []PathEdit {
	var edits []PathEdit
	for _, rc := range c.Layout().rcFiles() {
		if lines := blockLines(rc); len(lines) > 0 {
			edits = append(edits, PathEdit{Location: rc, Lines: lines})
		}
	}
	return edits
}

// applyPathEdit removes the managed block from one rc file atomically, touching
// only its own block. Returns whether it changed anything.
func (c Config) applyPathEdit(location string) (bool, error) {
	changed, err := removeBlockFrom(location)
	c.log().Debug("applied PATH-undo edit", "file", location, "changed", changed)
	return changed, err
}
