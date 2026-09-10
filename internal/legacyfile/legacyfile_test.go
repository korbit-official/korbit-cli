// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package legacyfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestAdoptMovesLegacyAndCompanions(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "digitalx-cli.db")
	old := filepath.Join(dir, "korbit-cli.db")
	write(t, old, "data")
	write(t, old+"-wal", "wal")
	// -shm deliberately absent: a missing companion must not be an error.

	adopted, err := Adopt(cur, old, "-wal", "-shm")
	if err != nil || !adopted {
		t.Fatalf("Adopt = %v, %v; want adopted with no error", adopted, err)
	}
	if got := read(t, cur); got != "data" {
		t.Fatalf("adopted content = %q", got)
	}
	if got := read(t, cur+"-wal"); got != "wal" {
		t.Fatalf("adopted -wal = %q", got)
	}
	if exists(old) || exists(old+"-wal") {
		t.Fatal("legacy files still present")
	}
	if exists(cur + "-shm") {
		t.Fatal("absent companion was created")
	}
}

func TestAdoptLeavesCurrentAlone(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "digitalx-cli.db")
	old := filepath.Join(dir, "korbit-cli.db")
	write(t, cur, "current")
	write(t, old, "legacy")

	adopted, err := Adopt(cur, old, "-wal")
	if err != nil || adopted {
		t.Fatalf("Adopt = %v, %v; want no adoption and no error", adopted, err)
	}
	if got := read(t, cur); got != "current" {
		t.Fatalf("current overwritten: %q", got)
	}
	if got := read(t, old); got != "legacy" {
		t.Fatalf("legacy disturbed: %q", got)
	}
}

func TestAdoptNoLegacyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "digitalx-cli.db")
	adopted, err := Adopt(cur, filepath.Join(dir, "korbit-cli.db"), "-wal")
	if err != nil || adopted {
		t.Fatalf("Adopt = %v, %v; want no adoption and no error", adopted, err)
	}
	if exists(cur) {
		t.Fatal("Adopt created a file out of nothing")
	}
}

func TestAdoptSamePathAndEmptyArgs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "same.db")
	write(t, p, "x")
	for _, tc := range [][2]string{{p, p}, {"", p}, {p, ""}} {
		if adopted, err := Adopt(tc[0], tc[1]); err != nil || adopted {
			t.Fatalf("Adopt(%q, %q) = %v, %v; want no adoption and no error", tc[0], tc[1], adopted, err)
		}
	}
	if got := read(t, p); got != "x" {
		t.Fatalf("content = %q", got)
	}
}

// TestAdoptReportsAStrandedCompanion: when the database moves but a companion
// does not, the caller must still be sent to the CURRENT path — legacy is gone —
// with the failure reported so it can name the stranded file. A directory in
// place of the companion's target makes the rename fail deterministically.
func TestAdoptReportsAStrandedCompanion(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "digitalx-cli.db")
	old := filepath.Join(dir, "korbit-cli.db")
	write(t, old, "data")
	write(t, old+"-wal", "wal")
	// A non-empty directory at the destination cannot be replaced by a rename.
	if err := os.MkdirAll(filepath.Join(cur+"-wal", "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}

	adopted, err := Adopt(cur, old, "-wal")
	if !adopted {
		t.Fatal("the primary rename succeeded, so adopted must be true")
	}
	if err == nil {
		t.Fatal("a failed companion rename must be reported")
	}
	if got := read(t, cur); got != "data" {
		t.Fatalf("adopted database = %q", got)
	}
	if exists(old) {
		t.Fatal("legacy database still present — the caller must not be sent back to it")
	}
}

// TestAdoptReportsAnUnreadableDirectory: a stat failure that is not "absent"
// must be reported, not swallowed as "nothing to adopt" — otherwise the caller
// starts an empty database over data it simply could not see.
func TestAdoptReportsAnUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not gate stat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "state")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "korbit-cli.db"), "data")
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Skipf("cannot drop directory permissions here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })

	adopted, err := Adopt(filepath.Join(sub, "digitalx-cli.db"), filepath.Join(sub, "korbit-cli.db"))
	if adopted {
		t.Fatal("nothing was adopted")
	}
	if err == nil {
		t.Fatal("an unreadable legacy name must be reported, not read as absent")
	}
}
