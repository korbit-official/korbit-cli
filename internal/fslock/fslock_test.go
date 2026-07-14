// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package fslock

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestLockBlocksUntilRelease asserts the core mutual-exclusion property: while
// one Lock is held, a second Lock on the SAME path blocks, and only proceeds
// once the first releases. The pattern is deterministic enough for CI: the
// second acquirer flips a flag the instant it returns, and we check that flag is
// still unset after a short window while the first holder is still in.
func TestLockBlocksUntilRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	release1, err := Lock(path)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	var acquired atomic.Bool
	gotIt := make(chan struct{})
	go func() {
		release2, err := Lock(path)
		if err != nil {
			t.Errorf("second Lock: %v", err)
			close(gotIt)
			return
		}
		acquired.Store(true)
		close(gotIt)
		release2()
	}()

	// While the first lock is held the goroutine must NOT have acquired.
	time.Sleep(100 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("second Lock acquired while the first was still held")
	}

	// Releasing must let the waiter through promptly.
	release1()
	select {
	case <-gotIt:
		if !acquired.Load() {
			t.Fatal("second Lock returned without acquiring")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Lock did not acquire within 5s of release (deadlock?)")
	}
}

// TestLockCreatesHomeAndFile proves Lock can be taken before the home exists:
// it MkdirAll's the parent 0700 and creates the 0600 lock file. This covers
// the `key add` shape where the CLI home does not yet exist.
func TestLockCreatesHomeAndFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", "home")
	path := filepath.Join(home, "keys.json.lock")

	release, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer release()

	if info, err := os.Stat(home); err != nil {
		t.Fatalf("home not created: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("home perm = %o, want 0700", info.Mode().Perm())
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("lock file not created: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("lock file perm = %o, want 0600", info.Mode().Perm())
	}
}

// TestLockWritesNote proves the lock file carries the human-readable note, so a
// user who inspects it learns it is safe to delete when korbit-cli is not
// running. It also checks the note survives a release/re-lock unchanged (no
// trailing bytes, idempotent rewrite).
func TestLockWritesNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	release, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	release()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if string(b) != lockNote {
		t.Fatalf("lock file content = %q, want lockNote", string(b))
	}

	// Re-locking must not corrupt or append to the note.
	release2, err := Lock(path)
	if err != nil {
		t.Fatalf("re-Lock: %v", err)
	}
	release2()
	if b2, _ := os.ReadFile(path); string(b2) != lockNote {
		t.Fatalf("note changed after re-lock: %q", string(b2))
	}
}

// TestLockReleaseRelock sanity-checks that a released lock can be re-taken
// (no leaked descriptor / no self-block after release).
func TestLockReleaseRelock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	for i := 0; i < 3; i++ {
		release, err := Lock(path)
		if err != nil {
			t.Fatalf("Lock #%d: %v", i, err)
		}
		release()
	}
}
