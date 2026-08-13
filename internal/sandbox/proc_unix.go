// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package sandbox

import (
	"os"
	"os/exec"
	"syscall"
)

// detach puts the child in its own session/process group so it outlives the CLI
// process (the CLI starts it and exits; stop is later via the pidfile). Its
// stdin is detached from the parent's (nil ⇒ /dev/null) — a background server
// never reads stdin, and sharing the parent's would tie it to the CLI's tty.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
}

// signalStop asks a process to shut down gracefully (SIGTERM → the bundle closes
// cleanly and removes its pidfile).
func signalStop(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// signalKill force-kills a process that ignored SIGTERM (SIGKILL → cannot be
// caught), matching how signalStop addresses it.
func signalKill(p *os.Process) error { return p.Signal(syscall.SIGKILL) }

// processAlive reports whether pid names a live process (signal 0 probe).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
