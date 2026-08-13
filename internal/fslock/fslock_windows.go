// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package fslock

import (
	"os"

	"golang.org/x/sys/windows"
)

// On Windows we lock a byte range of the file via LockFileEx. Without
// LOCKFILE_FAIL_IMMEDIATELY the call BLOCKS until the lock is granted, matching
// the unix flock(LOCK_EX) blocking semantics. The lock is released on
// UnlockFileEx and, like flock, automatically when the handle closes or the
// process exits — so it is crash-safe with no stale-lock handling.
//
// golang.org/x/sys/windows is a pure-Go syscall binding (no cgo), so this keeps
// cross-compilation toolchain-free; x/sys is already in go.sum as an indirect
// dependency.

// lockRegionLen is the byte range locked. The lock is whole-file in effect: we
// always lock the same fixed region [0, lockRegionLen) from offset 0, so any two
// acquirers contend. The file carries only an informational note; the locked
// range need not map to real bytes, and the range stays fixed regardless of the
// note's length so acquirers always contend on the same region.
const lockRegionLen = 1

func lockFD(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,             // reserved, must be 0
		lockRegionLen, // low 32 bits of the range length
		0,             // high 32 bits
		ol,
	)
}

func unlockFD(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0, // reserved, must be 0
		lockRegionLen,
		0,
		ol,
	)
}
