// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PathResult reports how PATH was handled for the installed binary directory during
// install. It rides the install result so a stdout-only consumer learns whether
// a manual step is still needed.
type PathResult struct {
	Dir    string `json:"dir"`
	OnPath bool   `json:"onPath"`
	// Action is what was done: "already-on-path", "edited-shell-rc" (unix, on a
	// yes prompt), "added-user-path" (windows), or "instructions" (non-interactive
	// or declined — the user must add it themselves; see Hint).
	Action string   `json:"action"`
	Files  []string `json:"files,omitempty"` // shell rc files edited (unix)
	Hint   string   `json:"hint,omitempty"`  // the manual step / follow-up
}

const (
	pathActionAlready      = "already-on-path"
	pathActionEditedRC     = "edited-shell-rc"
	pathActionAddedUser    = "added-user-path"
	pathActionInstructions = "instructions"
)

// Managed-block markers delimiting the PATH lines this installer owns in a shell
// rc file, so a re-run is idempotent and uninstall can remove exactly its own
// lines.
const (
	pathBlockBegin = "# >>> korbit-cli >>>"
	pathBlockEnd   = "# <<< korbit-cli <<<"
)

// homeForm rewrites dir to a $HOME-relative form for a shell rc line when it sits
// under the user's home (so the exported line is portable), else returns dir
// verbatim.
func (l Layout) homeForm(dir string) string {
	home := l.userHome()
	if home == "" {
		return dir
	}
	if rel, err := filepath.Rel(home, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return "$HOME/" + filepath.ToSlash(rel)
	}
	return dir
}

// managedBlock wraps body (the shell lines it manages) in the begin/end markers,
// terminated by a newline.
func managedBlock(body string) string {
	return pathBlockBegin + "\n" + body + "\n" + pathBlockEnd + "\n"
}

// pathBlockBody is the shell the managed block wraps: a prepend of shellDir to
// PATH GUARDED so it is a no-op when shellDir is already on PATH. The guard
// matters because the block is written to several startup files and one shell can
// source more than one of them — a bash login shell reads a login profile
// (.profile) and then .bashrc — which an unguarded prepend would turn into a
// duplicate PATH entry. Framing PATH with colons (":$PATH:") and matching
// ":shellDir:" keeps the test to whole path components. It is POSIX sh
// (case/esac), valid in the .profile/.zprofile login files as well as
// .bashrc/.zshrc.
func pathBlockBody(shellDir string) string {
	return `case ":$PATH:" in` + "\n" +
		`  *":` + shellDir + `:"*) ;;` + "\n" +
		`  *) export PATH="` + shellDir + `:$PATH" ;;` + "\n" +
		`esac`
}

// rcFiles are the shell startup files the unix PATH wiring appends to / cleans.
// Both the interactive rc files (.bashrc/.zshrc — new terminal tabs) AND the
// login profiles (.profile/.zprofile) are covered: a GUI/desktop app (Claude
// Desktop, an IDE) typically learns PATH from a *login* shell, which sources the
// profiles, not the interactive rc files — so wiring only .bashrc/.zshrc would
// leave the binary invisible to those. The managed block is idempotent per file,
// and its prepend is self-guarded (see pathBlockBody), so a shell that sources
// both an rc file and a profile does not stack a duplicate PATH entry.
func (l Layout) rcFiles() []string {
	home := l.userHome()
	return []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".profile"),
		filepath.Join(home, ".zprofile"),
	}
}

// blockToAppend returns the exact bytes appendBlockTo would add to a file whose
// current content is existing (nil/empty for a missing file): a blank separator
// line, then the managed block. When existing lacks a trailing newline it first
// completes that line. It returns nil when the block is already present, so a
// re-run is idempotent. It is the single source of the appended text, so the
// diff preview (additionFor) and the write can never drift.
func blockToAppend(existing []byte, body string) []byte {
	if bytes.Contains(existing, []byte(pathBlockBegin)) {
		return nil
	}
	prefix := ""
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		prefix = "\n"
	}
	return []byte(prefix + "\n" + managedBlock(body))
}

// appendBlockTo adds the managed block to one rc file if it is not already
// present, creating the file when missing. Returns true when it wrote the block.
func appendBlockTo(path, body string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	add := blockToAppend(existing, body)
	if add == nil {
		return false, nil // already wired — idempotent
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Write(add); err != nil {
		return false, err
	}
	return true, nil
}

// removeBlockFrom strips every managed block (markers inclusive) from one rc
// file, writing the result atomically (temp file + rename) so a crash mid-edit
// can't truncate the user's shell profile. It removes ONLY its own marker lines
// and the lines between them, preserving the rest of the file byte-for-byte
// (blank lines and the final newline included). When the path is a symlink (a
// dotfiles-managed rc file), it resolves to and rewrites the real target so the
// symlink is kept and the edit lands where install wrote it.
//
// The atomic rewrite mints a NEW inode, so it cannot carry over anything tied to
// the original file object. Rather than silently change the file's identity, it
// refuses (returning a *skipEditError, block left in place) when a rewrite would
// not faithfully recreate the file — hard-linked, or owned by a different
// user/group than this process — and the caller tells the user to remove the
// block by hand. install appends in place, so there is no matching rewrite that
// would need to preserve foreign ownership. Returns true when it removed a block.
func removeBlockFrom(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	lines := strings.Split(string(raw), "\n")
	var out []string
	removed, inBlock := false, false
	for _, ln := range lines {
		switch strings.TrimSpace(ln) {
		case pathBlockBegin:
			inBlock, removed = true, true
			continue
		case pathBlockEnd:
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		out = append(out, ln)
	}
	if !removed {
		return false, nil
	}
	// Rewrite the real target, following a symlink, so a dotfiles-managed rc file
	// (~/.zshrc -> ~/dotfiles/zshrc) keeps its symlink and the block is removed
	// from the file install actually appended to.
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	fi, err := os.Stat(target)
	if err != nil {
		return false, err
	}
	if reason := unsafeToRewrite(fi); reason != "" {
		return false, &skipEditError{path: target, reason: reason}
	}
	return true, writeBytesAtomic(target, []byte(strings.Join(out, "\n")), fi.Mode().Perm())
}

// skipEditError reports that removeBlockFrom declined to rewrite a file because an
// atomic replace would not faithfully recreate it (hard-linked, or owned by
// another user/group). It is a notice, not a failure: the managed block is left
// in place and the caller tells the user to remove it by hand.
type skipEditError struct {
	path   string
	reason string
}

func (e *skipEditError) Error() string {
	return fmt.Sprintf("left the korbit-cli block in %s — %s", e.path, e.reason)
}

// DiffLine is one line of a pending PATH-undo edit shown to the user before it is
// applied. Num is the 1-based line number in the file, or 0 for a location that
// is not a file (the Windows User PATH), where only Text (the entry) is shown.
type DiffLine struct {
	Num  int    `json:"num,omitempty"`
	Text string `json:"text"`
}

// PathEdit is a pending "undo the installer's PATH change" at one location: a
// shell rc file that carries the managed block (unix), or the User PATH (windows).
// Lines are the parts that would be removed, for a diff preview before applying.
type PathEdit struct {
	// Location is the rc file path (unix) or a human label for the User PATH
	// (windows).
	Location string `json:"location"`
	// Lines are the removed lines, most-to-least specific as they appear; for a
	// file each carries its line number, for the User PATH a single entry (Num 0).
	Lines []DiffLine `json:"lines"`
}

// PathAdditions returns the pending "add the installer's PATH line" edits, one
// per location that does not yet carry the installer's entry (each shell rc file
// lacking the managed block on unix; the User PATH on windows), each with a diff
// preview. Empty when the install dir is already on PATH or every location
// already has the entry. It edits nothing — the interactive front-end renders the
// previews, asks per location, and applies only what the user accepts.
func (c Config) PathAdditions() []PathAddition { return c.pathAdditions(c.Layout().ExecutableDir()) }

// OnPath reports whether the install dir is already effectively on PATH, so PATH
// wiring would be a no-op. Read-only; the dry-run report uses it to tell
// "already on PATH" apart from "wired on disk but not yet in this shell" (both of
// which leave PathAdditions empty).
func (c Config) OnPath() bool { return c.effectivelyOnPath(c.Layout().ExecutableDir()) }

// PathAddition is a pending "add the installer's PATH line" at one location: a
// shell rc file that will receive the managed block (unix), or the User PATH
// (windows). Context are unchanged trailing lines of the current file, shown for
// orientation above the change; Added are the lines that would be appended, for a
// diff preview before applying (the inverse of PathEdit's removal). NewFile is
// true when the rc file does not exist yet, so the block creates it. For the
// Windows User PATH there is no file: Context is empty and Added is the single
// entry to add (Num 0).
type PathAddition struct {
	Location string     `json:"location"`
	Context  []DiffLine `json:"context,omitempty"`
	Added    []DiffLine `json:"added"`
	NewFile  bool       `json:"newFile,omitempty"`
}

// pathContextLines is how many trailing lines of the current file to show as
// context above the added block in a diff preview.
const pathContextLines = 3

// numberedLines splits s into 1-based numbered lines, dropping the empty element
// a trailing newline produces so a line count reflects real lines. Returns nil
// for empty input.
func numberedLines(s string) []DiffLine {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	out := make([]DiffLine, len(parts))
	for i, p := range parts {
		out[i] = DiffLine{Num: i + 1, Text: p}
	}
	return out
}

// additionFor builds the diff-with-context preview of appending body's managed
// block to path: a few trailing context lines of the current file, then the exact
// lines blockToAppend would write, all with their resulting 1-based line numbers.
// It returns nil when the block is already present (nothing to add). Because it
// derives the added lines from blockToAppend, the preview is byte-identical to
// what appendBlockTo writes.
func additionFor(path, body string) *PathAddition {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil
	}
	add := blockToAppend(existing, body)
	if add == nil {
		return nil // already wired
	}
	existingLines := numberedLines(string(existing))
	finalLines := numberedLines(string(existing) + string(add))
	ctx := existingLines
	if len(ctx) > pathContextLines {
		ctx = ctx[len(ctx)-pathContextLines:]
	}
	return &PathAddition{
		Location: path,
		Context:  ctx,
		Added:    finalLines[len(existingLines):],
		NewFile:  len(existing) == 0,
	}
}

// blockLines returns the lines of EVERY managed block (markers inclusive) with
// their 1-based line numbers, or nil if path has no block. It matches exactly
// what removeBlockFrom deletes, so the diff preview never under-represents the
// edit (a hand-duplicated block shows both).
func blockLines(path string) []DiffLine {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []DiffLine
	inBlock := false
	for i, ln := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(ln)
		if t == pathBlockBegin {
			inBlock = true
		}
		if inBlock {
			out = append(out, DiffLine{Num: i + 1, Text: ln})
		}
		if t == pathBlockEnd {
			inBlock = false
		}
	}
	return out
}
