// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package netbind

import (
	"net"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// deviceBindSupported reports whether SO_BINDTODEVICE can be set. It is a
// privileged operation (root OR CAP_NET_RAW), so rather than assume root it
// probes the actual capability: a throwaway socket is bound to "lo" (which always
// exists, and the fd is closed immediately). The probe succeeds with the
// capability however it was granted — root or file-capability cap_net_raw — and
// fails with EPERM otherwise, so the resolver chooses the source-IP fallback up
// front instead of dying at connect. The probe runs once (lazily, only if an
// interface bind is requested) and the result is cached for the process — the
// process's privilege does not change underneath us.
var deviceBindSupported = sync.OnceValue(func() bool {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	return unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "lo") == nil
})

// bindDevice pins the socket to iface via SO_BINDTODEVICE (a single option that
// covers both IP families, so network is unused here).
func bindDevice(c syscall.RawConn, _ string, iface *net.Interface) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface.Name)
	}); err != nil {
		return err
	}
	return serr
}
