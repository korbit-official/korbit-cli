// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !unix && !windows

package useragent

// osVersion has no portable source on the remaining platforms (plan9, js/wasm,
// wasip1, …), so the version segment is omitted there. The build still succeeds,
// so the CLI compiles and runs everywhere — it just drops the OS-version field.
func osVersion() string { return "" }
