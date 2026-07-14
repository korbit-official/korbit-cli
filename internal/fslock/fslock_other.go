// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !unix && !windows

package fslock

import (
	"os"
	"runtime"

	"github.com/korbit-official/korbit-cli/internal/output"
)

// This CLI targets linux, darwin, and windows — all covered by the unix and
// windows build tags above. This stub exists only so a build for any other
// platform (e.g. js/wasm, plan9) fails with a clear "unsupported" error at lock
// time instead of an opaque "undefined: lockFD" compile error, making the
// platform gap explicit rather than silent.
func lockFD(*os.File) error {
	return output.Configf("file locking is not supported on %s", runtime.GOOS)
}

func unlockFD(*os.File) error { return nil }
