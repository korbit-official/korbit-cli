// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package useragent

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// osVersion returns the Windows version as "major.minor.build", e.g.
// "10.0.22631". RtlGetVersion reports the true OS version (it is not subject to
// the manifest-based version shimming that affects GetVersionEx).
func osVersion() string {
	v := windows.RtlGetVersion()
	return strconv.FormatUint(uint64(v.MajorVersion), 10) + "." +
		strconv.FormatUint(uint64(v.MinorVersion), 10) + "." +
		strconv.FormatUint(uint64(v.BuildNumber), 10)
}
