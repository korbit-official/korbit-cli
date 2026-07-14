// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package deno

// stripQuarantine is a no-op off macOS: only macOS Gatekeeper uses the
// com.apple.quarantine xattr.
func stripQuarantine(string) {}
