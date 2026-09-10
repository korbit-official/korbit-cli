// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// Windows process management (detached spawn + signalled stop) is a follow-up;
// download/cache/status stay cross-platform. These stubs keep the package
// building on Windows while `sandbox start`/`stop` report the limitation.

func detach(*exec.Cmd) {}

func signalStop(p *os.Process) error {
	return fmt.Errorf("stopping a detached sandbox is not yet supported on Windows; kill PID %d manually", p.Pid)
}

func signalKill(p *os.Process) error {
	return fmt.Errorf("force-killing a detached sandbox is not yet supported on Windows; kill PID %d manually", p.Pid)
}

// stillActive is Windows' STILL_ACTIVE (STATUS_PENDING): the exit code
// GetExitCodeProcess reports for a process that has not exited.
const stillActive = 259

// processAlive reports whether pid names a live process. os.FindProcess always
// succeeds on Windows, so this opens a handle instead: PROCESS_QUERY_LIMITED_INFORMATION
// is the least-privileged right that answers GetExitCodeProcess, and it is
// granted for processes this user owns without any elevation.
//
// A pid that no longer exists fails to open (ERROR_INVALID_PARAMETER), which is
// the case that matters here — a pidfile left behind by a crashed run must read
// as dead so the caller does not treat it as a running server. The known corner
// is a process that exited with code 259, which is indistinguishable from a live
// one; callers that need certainty pair this with a probe of the recorded port.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		// The handle opened, so the pid exists; without an exit code there is
		// nothing to disprove liveness with.
		return true
	}
	return code == stillActive
}
