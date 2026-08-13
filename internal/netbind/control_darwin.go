// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package netbind

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// deviceBindSupported is true on macOS: IP_BOUND_IF / IPV6_BOUND_IF are
// unprivileged.
func deviceBindSupported() bool { return true }

// bindDevice pins the socket to iface via the per-family *_BOUND_IF option,
// chosen from the resolved network. The value is the interface index.
func bindDevice(c syscall.RawConn, network string, iface *net.Interface) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		if network == "tcp6" {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, iface.Index)
		} else {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
		}
	}); err != nil {
		return err
	}
	return serr
}
