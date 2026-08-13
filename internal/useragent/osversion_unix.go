// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package useragent

import "golang.org/x/sys/unix"

// osVersion returns the kernel release string (uname -r), e.g. "6.1.0" on Linux,
// "14.1-RELEASE" on FreeBSD, or "25.6.0" (the Darwin kernel version) on macOS.
// On macOS this is the kernel version, not the marketing product version — a
// stable, cheap identifier that needs no subprocess. The `unix` build tag covers
// every Unix-like GOOS the toolchain targets (Linux, the BSDs, macOS, Solaris,
// illumos, AIX, …); x/sys/unix supplies Uname/Utsname on all of them, so this
// one implementation serves them all rather than an enumerated allowlist. An
// unavailable uname yields "" (the segment is omitted).
func osVersion() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}
