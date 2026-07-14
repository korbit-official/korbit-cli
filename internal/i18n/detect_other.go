// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package i18n

// osUILanguages has no portable non-Windows implementation: Unix hosts express
// the locale through the POSIX env vars hostLocales already reads, so this
// returns nil and the env is the only source there.
func osUILanguages() []string { return nil }
