// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package netbind

import (
	"context"
	"net"
	"syscall"
)

// deviceControl builds the ControlContext hook that binds a socket to iface
// before connect. It reads the resolved network ("tcp4"/"tcp6") to choose the
// right per-family socket option (see the per-OS bindDevice). It is only wired
// when deviceBindSupported() is true; otherwise the resolver uses source-IP
// binding instead.
func deviceControl(iface *net.Interface) controlFunc {
	return func(_ context.Context, network, _ string, c syscall.RawConn) error {
		return bindDevice(c, network, iface)
	}
}
