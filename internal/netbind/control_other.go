// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package netbind

import (
	"errors"
	"net"
	"syscall"
)

// deviceBindSupported is false on platforms with no known device-bind socket
// option; the resolver falls back to binding the interface's source IP.
func deviceBindSupported() bool { return false }

// bindDevice is never wired on these platforms (deviceBindSupported is false),
// but exists so the package compiles everywhere.
func bindDevice(_ syscall.RawConn, _ string, _ *net.Interface) error {
	return errors.New("device binding not supported on this platform")
}
