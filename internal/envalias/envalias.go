// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package envalias resolves the CLI's environment variables under their
// canonical DIGITALX_CLI_* names while still honoring the legacy KORBIT_CLI_*
// spellings, so an existing shell profile, CI job, or agent wrapper keeps
// working unchanged.
//
// Every environment read in the CLI goes through Lookup with the canonical
// name; the legacy name is derived by swapping the prefix, so the two spellings
// cannot drift apart. The canonical name wins when both are set.
//
// # Empty is unset
//
// A variable set to the empty string reads as unset, matching how the CLI
// already treats every one of these knobs (an empty --base-url overrides
// nothing). The consequence worth knowing: exporting DIGITALX_CLI_X="" does NOT
// blank a KORBIT_CLI_X that is set — the lookup falls through to it. To turn a
// legacy variable off, unset the legacy name.
//
// It deliberately does NOT cover KORBIT_SANDBOX_*: those are the sandbox
// bundle's own configuration knobs, part of the CLI→bundle contract rather than
// this CLI's environment surface.
package envalias

import "strings"

// Prefix is the canonical environment-variable prefix; LegacyPrefix is the
// accepted older spelling. These are the single source for the swap Lookup
// performs — never write either spelling out at a call site.
const (
	Prefix       = "DIGITALX_CLI_"
	LegacyPrefix = "KORBIT_CLI_"
)

// Lookup reads the variable named by its canonical DIGITALX_CLI_* name from
// getenv, falling back to the equivalent KORBIT_CLI_* name when the canonical
// one is unset or empty. A name outside the canonical prefix is read verbatim
// with no fallback. getenv nil yields "".
func Lookup(getenv func(string) string, name string) string {
	if getenv == nil {
		return ""
	}
	if v := getenv(name); v != "" {
		return v
	}
	if legacy, ok := legacyName(name); ok {
		return getenv(legacy)
	}
	return ""
}

// legacyName maps a canonical name to its legacy spelling, reporting false for
// a name that carries no canonical prefix (nothing to fall back to).
func legacyName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, Prefix)
	if !ok {
		return "", false
	}
	return LegacyPrefix + rest, true
}
