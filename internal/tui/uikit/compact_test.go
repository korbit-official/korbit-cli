// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import "testing"

func TestCompact(t *testing.T) {
	cases := []struct {
		v    string
		w    int
		want string
	}{
		// Exact when it fits — Compact never touches a figure it doesn't have to.
		{"1234", 5, "1,234"},
		{"9621139961", 14, "9,621,139,961"},
		// Unit forms, three significant digits.
		{"9621139961", 8, "9.62B"},
		{"48654.52", 6, "48.7k"},
		{"1000000", 6, "1.00M"},
		{"1267240609", 6, "1.27B"},
		{"9621139961.9", 6, "9.62B"},
		// Rounding carries an order of magnitude.
		{"999999", 5, "1.00M"},
		// A tiny cell sheds fraction digits before giving up.
		{"9996", 4, "10k"},
		// Sign survives.
		{"-9621139961", 7, "-9.62B"},
		// Sub-thousand magnitude: a unit gains nothing; clip like before.
		{"0.000000000801", 8, "0.00000…"},
		// Non-numeric input: clip, never mangle.
		{"n/a-value", 5, "n/a-…"},
		// Beyond T the integer head grows (no larger unit is offered).
		{"12345678901234567", 8, "12300T"},
	}
	for _, c := range cases {
		if got := Compact(c.v, c.w); got != c.want {
			t.Errorf("Compact(%q, %d) = %q, want %q", c.v, c.w, got, c.want)
		}
	}
}
