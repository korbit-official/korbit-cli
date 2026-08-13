// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package netbind

import (
	"math/bits"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// IP_UNICAST_IF / IPV6_UNICAST_IF are not defined in golang.org/x/sys/windows;
// both option numbers are 31.
const (
	ipUnicastIF   = 31
	ipv6UnicastIF = 31
)

// deviceBindSupported is true on Windows: the *_UNICAST_IF options are
// unprivileged.
func deviceBindSupported() bool { return true }

// bindDevice pins the socket's egress interface via the per-family *_UNICAST_IF
// option. The IPv4 option takes the interface index in network byte order (it is
// overloaded with an address form); the IPv6 option takes it in host order.
func bindDevice(c syscall.RawConn, network string, iface *net.Interface) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		h := windows.Handle(fd)
		if network == "tcp6" {
			serr = windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIF, iface.Index)
		} else {
			idx := int(bits.ReverseBytes32(uint32(iface.Index)))
			serr = windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIF, idx)
		}
	}); err != nil {
		return err
	}
	return serr
}
