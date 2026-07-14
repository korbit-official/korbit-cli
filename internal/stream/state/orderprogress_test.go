// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package state

import "testing"

// TestOrderProgressSupersedes is the single-source-of-truth table for the
// clock-free order merge rule: which of two views of the SAME order wins.
func TestOrderProgressSupersedes(t *testing.T) {
	cases := []struct {
		name            string
		inStatus, inQty string
		stStatus, stQty string
		want            bool
	}{
		// Rule 4: forward lifecycle within the book.
		{"pending->open", "open", "0", "pending", "0", true},
		{"open->partiallyFilled", "partiallyFilled", "0.3", "open", "0", true},
		{"open->open same", "open", "0", "open", "0", false},
		{"partiallyFilled more fills", "partiallyFilled", "0.6", "partiallyFilled", "0.3", true},
		{"partiallyFilled same fills", "partiallyFilled", "0.3", "partiallyFilled", "0.3", false},
		{"partiallyFilled fewer fills (stale)", "partiallyFilled", "0.1", "partiallyFilled", "0.3", false},
		// Lifecycle regressions never win, even with a (spurious) fill value.
		{"open after partiallyFilled (stale reopen)", "open", "0", "partiallyFilled", "0.3", false},
		{"open after partiallyFilled, equal qty", "open", "0.3", "partiallyFilled", "0.3", false},
		{"partiallyFilled->pending", "pending", "0", "partiallyFilled", "0.3", false},

		// Rule 3: incoming terminal beats any non-terminal — regardless of fill.
		{"open->filled", "filled", "0.9", "open", "0", true},
		{"partiallyFilled->filled", "filled", "0.9", "partiallyFilled", "0.3", true},
		{"open->canceled", "canceled", "0", "open", "0", true},
		{"partiallyFilled->partiallyFilledCanceled", "partiallyFilledCanceled", "0.3", "partiallyFilled", "0.3", true},
		{"pending->expired", "expired", "0", "pending", "0", true},

		// Rule 2: terminal stored is absorbing.
		{"filled then stale open", "open", "0", "filled", "0.9", false},
		{"filled then partiallyFilled", "partiallyFilled", "0.5", "filled", "0.9", false},
		{"canceled then open", "open", "0", "canceled", "0", false},
		{"filled then second terminal (dup)", "filled", "0.9", "filled", "0.9", false},
		{"filled then conflicting canceled", "canceled", "0", "filled", "0.9", false},

		// Rule 1: the synthetic closed placeholder yields to any real status.
		{"placeholder->open (revive)", "open", "0", StatusClosedUnknown, "0", true},
		{"placeholder->partiallyFilled (revive)", "partiallyFilled", "0.3", StatusClosedUnknown, "0", true},
		{"placeholder->canceled (real terminal)", "canceled", "0", StatusClosedUnknown, "0", true},
		{"placeholder->placeholder (no-op)", StatusClosedUnknown, "0", StatusClosedUnknown, "0", false},
		{"placeholder->empty status (no-op)", "", "0", StatusClosedUnknown, "0", false},

		// Forward compatibility: terminal is "not open", so a status the API adds
		// in the future is terminal automatically — it goes through rules 2/3, NOT
		// a special unrecognized-status path. Rule 3: a future terminal supersedes
		// any non-terminal, even with no new fill.
		{"future terminal over open", "rejected", "0", "open", "0", true},
		{"future terminal over partiallyFilled, no new fill", "rejected", "0", "partiallyFilled", "0.3", true},
		// Rule 2: a future terminal stored status is absorbing too.
		{"open after future terminal (stale)", "open", "0", "rejected", "0", false},
		{"future terminal dup", "rejected", "0", "rejected", "0", false},

		// Rule 5: the empty "status not yet known" string is the ONLY non-terminal
		// value outside the open set (it is neither open nor terminal), so it is the
		// only case that still reaches the fill-increase fallback. An order stored
		// with an unknown status must still take a real update.
		{"empty incoming, fill grew", "", "0.6", "open", "0.3", true},
		{"empty incoming, fill same", "", "0.3", "open", "0.3", false},
		{"open over empty stored, fill grew", "open", "0.6", "", "0.3", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := orderProgress{status: c.inStatus, filledQty: c.inQty}
			st := orderProgress{status: c.stStatus, filledQty: c.stQty}
			if got := in.supersedes(st); got != c.want {
				t.Fatalf("supersedes(%+v, %+v) = %v, want %v", in, st, got, c.want)
			}
		})
	}
}

func TestCmpDecimal(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0", "0", 0},
		{"", "", 0},
		{"", "0", 0},
		{"0.5", "0.5", 0},
		{"0.5", "0.50", 0}, // trailing-zero-insensitive (exact value)
		{"1", "0.9", 1},
		{"0.3", "0.30000001", -1},
		{"0.00000001", "0.00000002", -1},
		// Large quantities that would lose precision as float64 compare exactly.
		{"100000000.00000001", "100000000.00000002", -1},
		{"100000000.00000002", "100000000.00000002", 0},
		{"100000000000000000000000000.00000001", "100000000000000000000000000.00000002", -1},
		// Unparseable strings degrade to zero rather than panicking.
		{"garbage", "0", 0},
		{"garbage", "0.5", -1},
		{"1.5", "nonsense", 1},
	}
	for _, c := range cases {
		if got := cmpDecimal(c.a, c.b); got != c.want {
			t.Fatalf("cmpDecimal(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
