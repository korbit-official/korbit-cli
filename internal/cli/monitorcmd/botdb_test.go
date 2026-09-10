// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package monitorcmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestBotDBPathAdoptsLegacyDatabase: a home created under the earlier product
// name carries the bot database under the legacy file name. Resolving the
// default path moves it (SQLite sidecars included) so a script's own tables and
// rows survive.
func TestBotDBPathAdoptsLegacyDatabase(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, legacyBotDBFileName)
	if err := os.WriteFile(legacy, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := botDBPath(home, nil)
	want := filepath.Join(home, botDBFileName)
	if got != want {
		t.Fatalf("botDBPath = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != "db" {
		t.Fatalf("adopted database = %q, %v", b, err)
	}
	if b, err := os.ReadFile(want + "-wal"); err != nil || string(b) != "wal" {
		t.Fatalf("adopted -wal = %q, %v", b, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy database still present: %v", err)
	}
}

// TestBotDBPathLeavesAnExistingCurrentDatabase: once the current name exists it
// wins, and a leftover legacy file is never merged into it.
func TestBotDBPathLeavesAnExistingCurrentDatabase(t *testing.T) {
	home := t.TempDir()
	current := filepath.Join(home, botDBFileName)
	if err := os.WriteFile(current, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, legacyBotDBFileName), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := botDBPath(home, nil); got != current {
		t.Fatalf("botDBPath = %q, want %q", got, current)
	}
	if b, _ := os.ReadFile(current); string(b) != "current" {
		t.Fatalf("current database overwritten: %q", b)
	}
}

// TestLegacyBotDBPaths pins the list a caller removing the CLI's data uses for a
// home that was never opened since the rename.
func TestLegacyBotDBPaths(t *testing.T) {
	home := filepath.Join("/home", "u", ".digitalx-cli")
	legacy := filepath.Join(home, legacyBotDBFileName)
	got := LegacyBotDBPaths(home)
	want := []string{legacy, legacy + "-wal", legacy + "-shm"}
	if len(got) != len(want) {
		t.Fatalf("LegacyBotDBPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LegacyBotDBPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestBotDBPathKeepsTheLegacyPathWhenTheRenameFails: a failed rename must send
// the caller to the database that EXISTS. Returning the current path would
// create an empty database there, and the next run would then see the current
// name present and never retry — orphaning the script's tables. A read-only
// directory reproduces the failure the way an open file does on Windows.
func TestBotDBPathKeepsTheLegacyPathWhenTheRenameFails(t *testing.T) {
	home := readOnlyHomeWithLegacyBotDB(t)

	got := botDBPath(home, nil)
	if want := filepath.Join(home, legacyBotDBFileName); got != want {
		t.Fatalf("botDBPath = %q, want the legacy path %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, botDBFileName)); !os.IsNotExist(err) {
		t.Fatalf("nothing may be created under the current name: %v", err)
	}
}

// TestBotDBPathRetriesOnTheNextRun: because the failed run created nothing under
// the current name, a later run whose rename CAN succeed still adopts.
func TestBotDBPathRetriesOnTheNextRun(t *testing.T) {
	home := readOnlyHomeWithLegacyBotDB(t)
	botDBPath(home, nil) // fails, returns legacy

	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	got, want := botDBPath(home, nil), filepath.Join(home, botDBFileName)
	if got != want {
		t.Fatalf("botDBPath = %q, want %q on the retry", got, want)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != "db" {
		t.Fatalf("adopted database = %q, %v", b, err)
	}
}

func readOnlyHomeWithLegacyBotDB(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not gate rename on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, legacyBotDBFileName), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Skipf("cannot drop directory permissions here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	return home
}
