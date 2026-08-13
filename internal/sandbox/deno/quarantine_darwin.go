// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package deno

import "golang.org/x/sys/unix"

// stripQuarantine best-effort removes the com.apple.quarantine xattr so a
// self-downloaded, exec-launched Deno runs without a Gatekeeper prompt. The
// attribute is almost certainly absent on a file korbit-cli just wrote, so any
// error (notably ENOATTR) is ignored — this only mirrors the manual
// `xattr -d com.apple.quarantine ./deno` step for the rare case it is present.
func stripQuarantine(path string) {
	_ = unix.Removexattr(path, "com.apple.quarantine")
}
