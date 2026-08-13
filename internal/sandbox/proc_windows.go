// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
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

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p
	// FindProcess always succeeds on Windows; without a signal-0 probe, treat a
	// resolvable pid as possibly-alive. The pidfile + TCP probe disambiguate.
	return true
}
