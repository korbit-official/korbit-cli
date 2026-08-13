// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux

package doctorcmd

// diagnoseClockConfig has no OS time-sync diagnosis on this platform yet
// (Windows uses clockconfig_windows.go; Linux uses clockconfig_linux.go).
// Returning Supported=false makes doctorClockConfigCheck emit a single "not
// supported on <GOOS>" line. To add macOS support, create clockconfig_darwin.go
// implementing diagnoseClockConfig for real and narrow this file's build tag
// (e.g. `!windows && !linux && !darwin`) accordingly.
func diagnoseClockConfig(timeoutMs int, clockConfirmedOK bool) clockConfigReport {
	return clockConfigReport{Supported: false}
}
