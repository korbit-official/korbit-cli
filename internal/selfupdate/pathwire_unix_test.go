// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoveBlockFromSkipsHardLinked pins that removeBlockFrom refuses to rewrite
// an rc file that carries another hard link: the atomic temp+rename would mint a
// new inode and leave the other name holding the old content (block still in it).
// Instead it returns a *skipEditError with the managed block left untouched, so
// uninstall reports the notice and the user removes the block by hand.
func TestRemoveBlockFromSkipsHardLinked(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, "rc")
	content := "keep me\n" + managedBlock(pathBlockBody("~/bin"))
	if err := os.WriteFile(rc, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second name for the same inode makes nlink == 2.
	if err := os.Link(rc, filepath.Join(dir, "rc.hardlink")); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	removed, err := removeBlockFrom(rc)
	var skip *skipEditError
	if !errors.As(err, &skip) {
		t.Fatalf("want *skipEditError, got removed=%v err=%v", removed, err)
	}
	// The hard-link check runs first in unsafeToRewrite, so a hard-linked file
	// always reports that reason — pin it, so this stays a hard-link test even in a
	// setgid temp dir where the ownership arm could otherwise fire.
	if !strings.Contains(skip.reason, "hard link") {
		t.Errorf("want hard-link skip reason, got %q", skip.reason)
	}
	if removed {
		t.Error("removed must be false when the edit is skipped")
	}
	if got := mustContent(t, rc); !strings.Contains(got, pathBlockBegin) {
		t.Errorf("managed block must be left in place on skip:\n%s", got)
	}
}

// TestRemoveBlockFromPreservesMode pins the happy path: a normal single-link,
// self-owned rc file has its block removed and its permission bits carried across
// the atomic rewrite.
func TestRemoveBlockFromPreservesMode(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(rc, []byte("keep me\n"+managedBlock(pathBlockBody("~/bin"))), 0o600); err != nil {
		t.Fatal(err)
	}
	// Normalize owner/group to this process so the ownership arm can't fire in a
	// setgid temp dir — this test pins the rewrite path, not the skip check.
	if err := os.Chown(rc, os.Geteuid(), os.Getegid()); err != nil {
		t.Skipf("cannot normalize ownership: %v", err)
	}
	if err := os.Chmod(rc, 0o640); err != nil { // a distinctive, non-default mode
		t.Fatal(err)
	}

	removed, err := removeBlockFrom(rc)
	if err != nil || !removed {
		t.Fatalf("removeBlockFrom: removed=%v err=%v", removed, err)
	}
	got := mustContent(t, rc)
	if strings.Contains(got, pathBlockBegin) {
		t.Errorf("block should be removed:\n%s", got)
	}
	if !strings.Contains(got, "keep me") {
		t.Errorf("user content dropped:\n%s", got)
	}
	if fi, err := os.Stat(rc); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o640 {
		t.Errorf("permission bits not preserved across rewrite: got %o want 640", perm)
	}
}

// altManagedBlock wraps body in the alternate marker pair — what an rc file
// wired by an earlier-named installer carries. Only tests write it; the code
// always writes the primary pair.
func altManagedBlock(body string) string {
	return altPathBlockBegin + "\n" + body + "\n" + altPathBlockEnd + "\n"
}

// TestBothMarkerPairsAreIdempotent pins that an rc file already carrying EITHER
// marker pair is treated as wired: nothing is appended, so a second block never
// stacks a duplicate PATH entry on a shell that sources the file.
func TestBothMarkerPairsAreIdempotent(t *testing.T) {
	body := pathBlockBody("$HOME/.local/bin")
	for name, existing := range map[string]string{
		"primary": "keep me\n" + managedBlock(body),
		"alt":     "keep me\n" + altManagedBlock(body),
	} {
		if add := blockToAppend([]byte(existing), body); add != nil {
			t.Errorf("%s: blockToAppend wanted to append %q to an already-wired file", name, add)
		}
		rc := filepath.Join(t.TempDir(), "rc")
		if err := os.WriteFile(rc, []byte(existing), 0o644); err != nil {
			t.Fatal(err)
		}
		wrote, err := appendBlockTo(rc, body)
		if err != nil || wrote {
			t.Errorf("%s: appendBlockTo wrote=%v err=%v, want no write", name, wrote, err)
		}
		if got := mustContent(t, rc); got != existing {
			t.Errorf("%s: file changed:\n%s", name, got)
		}
	}
}

// TestRemoveBlockFromHandlesAltMarkers pins that uninstall strips whichever
// marker pair an rc file carries — including both in one file — and that
// blockLines previews exactly what it removes.
func TestRemoveBlockFromHandlesAltMarkers(t *testing.T) {
	body := pathBlockBody("$HOME/.local/bin")
	rc := filepath.Join(t.TempDir(), "rc")
	content := "keep me\n" + altManagedBlock(body) + "and me\n" + managedBlock(body)
	if err := os.WriteFile(rc, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if lines := blockLines(rc); len(lines) != 12 { // 2 blocks x (2 markers + 4 body lines)
		t.Errorf("blockLines previewed %d lines, want both blocks (12)", len(lines))
	}
	removed, err := removeBlockFrom(rc)
	if err != nil || !removed {
		t.Fatalf("removeBlockFrom: removed=%v err=%v", removed, err)
	}
	got := mustContent(t, rc)
	for _, marker := range []string{pathBlockBegin, pathBlockEnd, altPathBlockBegin, altPathBlockEnd} {
		if strings.Contains(got, marker) {
			t.Errorf("marker %q survived:\n%s", marker, got)
		}
	}
	if !strings.Contains(got, "keep me") || !strings.Contains(got, "and me") {
		t.Errorf("user content dropped:\n%s", got)
	}
}

// TestAppendWritesPrimaryMarkers pins that a NEW block is always written with
// the primary marker pair, never the alternate one.
func TestAppendWritesPrimaryMarkers(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "rc")
	wrote, err := appendBlockTo(rc, pathBlockBody("$HOME/.local/bin"))
	if err != nil || !wrote {
		t.Fatalf("appendBlockTo: wrote=%v err=%v", wrote, err)
	}
	got := mustContent(t, rc)
	if !strings.Contains(got, pathBlockBegin) || !strings.Contains(got, pathBlockEnd) {
		t.Errorf("primary markers missing:\n%s", got)
	}
	if strings.Contains(got, altPathBlockBegin) {
		t.Errorf("the alternate marker pair must never be written:\n%s", got)
	}
}
