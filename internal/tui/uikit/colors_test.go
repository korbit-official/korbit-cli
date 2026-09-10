// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/config"
)

// TestColorSchemeStringRoundTrips pins String and ParseColorScheme as exact
// inverses over the real schemes, and unrecognized input to the default.
func TestColorSchemeStringRoundTrips(t *testing.T) {
	for _, s := range []ColorScheme{ColorSchemeGreenRed, ColorSchemeRedBlue} {
		if got := ParseColorScheme(s.String()); got != s {
			t.Errorf("round-trip %v: String=%q parsed back to %v", s, s.String(), got)
		}
	}
	for _, bad := range []string{"", "greenred", "blue-red", "GREEN-RED"} {
		if got := ParseColorScheme(bad); got != ColorSchemeGreenRed {
			t.Errorf("ParseColorScheme(%q) = %v, want default green-red", bad, got)
		}
	}
}

// TestColorSchemeNamesMatchConfig guards against drift between the scheme names
// this package emits and config's independent accepted-value list (config can't
// import this charm-dependent package, so it keeps its own copy). Every scheme
// String must be a value config persists, and the counts must match so a scheme
// added on either side without the other trips the test.
func TestColorSchemeNamesMatchConfig(t *testing.T) {
	schemes := []ColorScheme{ColorSchemeGreenRed, ColorSchemeRedBlue}
	for _, s := range schemes {
		if !config.ValidColorScheme(s.String()) {
			t.Errorf("scheme %q is not accepted by config.ValidColorScheme", s.String())
		}
	}
	if len(config.ColorSchemes) != len(schemes) {
		t.Errorf("config.ColorSchemes has %d entries, uikit has %d — lists drifted",
			len(config.ColorSchemes), len(schemes))
	}
}
