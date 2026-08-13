// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package fslock provides an advisory, cross-process file lock used to
// serialize read-modify-write cycles over the CLI's on-disk state.
//
// # Why it exists
//
// Every mutation of keys.json (the registry), keystore.json (the file vault),
// and config.json is a load → modify → atomic-rename cycle. The atomic rename
// prevents a torn file, but it does NOT prevent a lost update: two concurrent
// korbit-cli processes — the expected case is a long-running monitor bot plus
// ad-hoc human commands — can each read the same snapshot and last-writer-wins
// the whole file. For the file vault that can silently drop a freshly-added
// key's PRIVATE material while its registry record survives (unrecoverable;
// forces a portal rotation). An advisory lock held across the whole
// read-modify-write closes that window.
//
// # The lock
//
// Lock takes an EXCLUSIVE advisory lock on a dedicated sidecar file
// (e.g. "<home>/keys.json.lock") — never on the data file itself, so the
// data file's atomic-rename is undisturbed. The lock is advisory: it only
// excludes other callers that also go through Lock on the same path. Reads
// that rely on the atomic rename for a consistent snapshot need no lock.
//
// Blocking acquire is intentional. These critical sections are milliseconds
// (load a small JSON file, mutate, atomic-rename), so a contending process
// simply waits its turn rather than failing.
//
// # Crash safety
//
// OS advisory locks (flock on unix, LockFileEx on Windows) are released
// automatically when the owning process exits or its file descriptor/handle is
// closed — including on a crash or kill. So there is NO stale-lock recovery to
// do and the lock file is never deleted by the CLI (deleting it would race a
// concurrent acquirer holding it open). A leftover ".lock" file is expected and
// harmless; it carries only a human-readable note (see lockNote) explaining that
// a user is free to delete it whenever no korbit-cli process is running.
//
// # Lock ordering (callers' contract)
//
// fslock itself imposes no ordering, but its callers must: the only nested
// acquisition in this codebase is keys.Manager holding the registry lock
// (keys.json.lock) while it drives the file vault's Set/Delete, which take the
// SEPARATE vault lock (keystore.json.lock). That nesting is safe ONLY because
// the two locks are different files. The fixed order is registry → vault, never
// the reverse, and nothing in the vault layer may acquire the registry lock —
// otherwise two processes could deadlock. See internal/keys/doc.go and
// internal/keystore/doc.go.
package fslock

import (
	"os"
	"path/filepath"
)

// lockNote is written into every lock file so a user who inspects one understands
// what it is and that removing it is safe when no korbit-cli process is running.
// The file holds no application data; this text is purely informational.
const lockNote = `korbit-cli lock file

korbit-cli writes this file to coordinate concurrent updates to its on-disk
state (keys, key store, config). It holds no data of its own.

The actual lock is an OS advisory lock held only while a korbit-cli process is
running; the OS releases it automatically when that process exits, even on a
crash. So this file is safe to delete whenever no korbit-cli process is running
— it is recreated as needed.
`

// Lock acquires an exclusive advisory lock on the sidecar file at path
// (conventionally "<datafile>.lock"), creating it 0600 if needed. The parent
// directory is created 0700 first, because the lock may be taken before the CLI
// home exists (`key add` creates it on first use). It blocks until the lock is
// held, then returns a release func the caller MUST defer; release unlocks and
// closes the descriptor/handle. A non-nil error means the lock was NOT acquired
// and release is nil.
func Lock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// 0600: the lock file carries only an informational note (lockNote), but the
	// home holds secrets, so keep permissions tight and consistent with the data
	// files beside it.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFD(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	// Annotate the sidecar so a curious reader knows what it is. Best-effort: the
	// note is cosmetic and must never fail acquisition. Safe because we hold the
	// exclusive lock here — no other acquirer can be writing concurrently. Write
	// then truncate so a note shortened across versions leaves no trailing bytes.
	if _, werr := f.WriteAt([]byte(lockNote), 0); werr == nil {
		_ = f.Truncate(int64(len(lockNote)))
	}
	// Closing the descriptor releases the OS lock; we both unlock explicitly and
	// close so the release is unambiguous regardless of platform semantics.
	return func() {
		_ = unlockFD(f)
		_ = f.Close()
	}, nil
}
