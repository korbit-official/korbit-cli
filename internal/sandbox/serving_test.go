// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The pidfile is what every caller reads to decide whether a sandbox is serving
// this state directory — `start` reuses or refuses over it, `stop` acts on it,
// `status` reports it. A state dir may carry one under EITHER database spelling,
// so the tests here are about that pair: which pidfiles belong to a state dir,
// and that a leftover under the spelling not in use is still found and reported
// rather than silently outranked.

// writePidfile puts a {pid,port} pidfile beside the named database, naming a pid
// that is alive (this test process) or one that is not.
func writePidfile(t *testing.T, home, dbName string, pid, port int) {
	t.Helper()
	dir := filepath.Join(home, "sandbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid":%d,"port":%d}`, pid, port)
	if err := os.WriteFile(filepath.Join(dir, dbName+"-pid"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStopClearsStalePidfilesUnderBothSpellings: a stale pidfile is what makes a
// state dir read as busy — a crashed run whose pid was recycled reads as live, so
// `start` refuses. `sandbox stop` is the command that clears it, so it has to
// reach the blocker WHICHEVER spelling left it, and to say that it did.
func TestStopClearsStalePidfilesUnderBothSpellings(t *testing.T) {
	home := t.TempDir()
	dead := 1 << 30 // above every platform's pid maximum, so never alive
	writePidfile(t, home, DBDefaultName, dead, 9999)
	writePidfile(t, home, LegacyDBFileName, dead, 9998)
	m := New(Config{Home: home}, Deps{})

	res, err := m.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped {
		t.Error("nothing was running, so nothing was stopped")
	}
	for _, name := range []string{DBDefaultName, LegacyDBFileName} {
		path := filepath.Join(home, "sandbox", name+"-pid")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not cleared: %v", path, err)
		}
		if !slices.Contains(res.StaleRemoved, path) {
			t.Errorf("staleRemoved = %v, want it to report %s", res.StaleRemoved, path)
		}
	}
}

// TestStatusReportsStalePidfilesUnderBothSpellings: status is read-only, so it
// names them rather than removing them — but it must name them, because a
// leftover pidfile is the thing that makes this state dir look busy for no
// reason, and nothing else in the report would show it.
func TestStatusReportsStalePidfilesUnderBothSpellings(t *testing.T) {
	home := t.TempDir()
	dead := 1 << 30
	writePidfile(t, home, DBDefaultName, dead, 9999)
	writePidfile(t, home, LegacyDBFileName, dead, 9998)
	m := New(Config{Home: home, CacheDir: t.TempDir()}, Deps{
		LookPath: func(string) (string, error) { return "", errors.New("no deno") },
	})

	res := m.Status(context.Background())
	for _, name := range []string{DBDefaultName, LegacyDBFileName} {
		path := filepath.Join(home, "sandbox", name+"-pid")
		if !slices.Contains(res.StalePidfiles, path) {
			t.Errorf("stalePidfiles = %v, want it to report %s", res.StalePidfiles, path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("status must not remove %s: %v", path, err)
		}
	}
}

// TestPidfileCandidates is the mechanism behind "both spellings": the list every
// pidfile-reading path iterates. The one this home's layout implies comes FIRST,
// so a live server there outranks a leftover beside it; an explicit --db has
// exactly one pidfile and no second spelling to consider.
//
// This is asserted directly rather than by driving `Stop` against a live pid:
// Stop SIGTERMs what it finds, and the only pid a test can honestly call live is
// its own.
func TestPidfileCandidates(t *testing.T) {
	t.Run("default state dir, current layout", func(t *testing.T) {
		home := filepath.Join("/home/u", ".digitalx-cli")
		m := New(Config{Home: home}, Deps{})
		want := []string{
			filepath.Join(home, "sandbox", DBDefaultName+"-pid"),
			filepath.Join(home, "sandbox", LegacyDBFileName+"-pid"),
		}
		assertPaths(t, m.pidfileCandidates(), want)
	})
	t.Run("default state dir, legacy layout puts its own name first", func(t *testing.T) {
		home := filepath.Join("/home/u", ".korbit-cli")
		m := New(Config{Home: home}, Deps{})
		want := []string{
			filepath.Join(home, "sandbox", LegacyDBFileName+"-pid"),
			filepath.Join(home, "sandbox", DBDefaultName+"-pid"),
		}
		assertPaths(t, m.pidfileCandidates(), want)
	})
	t.Run("an explicit --db has exactly one", func(t *testing.T) {
		m := New(Config{Home: "/home/u/.digitalx-cli", DB: "/tmp/mine.db"}, Deps{})
		assertPaths(t, m.pidfileCandidates(), []string{"/tmp/mine.db-pid"})
	})
}

func assertPaths(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestDBFileNameFollowsTheHomeDirectoryName: the HOME's directory name decides
// the database filename, not the state dir's (which is always "sandbox").
func TestDBFileNameFollowsTheHomeDirectoryName(t *testing.T) {
	for _, tc := range []struct {
		home string
		want string
	}{
		{filepath.Join("/home/u", ".digitalx-cli"), DBDefaultName},
		{filepath.Join("/home/u", ".korbit-cli"), LegacyDBFileName},
		{filepath.Join("/srv", "agent-home"), DBDefaultName},
	} {
		if got := DBFileName(tc.home); got != tc.want {
			t.Errorf("DBFileName(%q) = %q, want %q", tc.home, got, tc.want)
		}
		m := New(Config{Home: tc.home}, Deps{})
		if got, want := m.dbPath(), filepath.Join(tc.home, "sandbox", tc.want); got != want {
			t.Errorf("dbPath for %q = %q, want %q", tc.home, got, want)
		}
	}
}
