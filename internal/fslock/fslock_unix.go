// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package fslock

import (
	"os"
	"syscall"
)

// lockFD takes a blocking exclusive flock on the descriptor. flock locks are
// associated with the open file description and are released on close or
// process exit, which gives us crash-safe auto-release for free. No cgo:
// syscall.Flock is a direct kernel call.
func lockFD(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFD(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
