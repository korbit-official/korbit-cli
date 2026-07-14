// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package monitorcmd

import "testing"

// The queue-saturation diagnostic must fire at most once per interval over the
// injectable clock — so a sustained backlog warns once, not at frame rate.
func TestRateLimiterAllowsOncePerInterval(t *testing.T) {
	rl := newRateLimiter(2000)

	if !rl.allow(0) {
		t.Fatalf("the first call must always allow")
	}
	if rl.allow(1) {
		t.Fatalf("a call within the interval must be suppressed")
	}
	if rl.allow(1999) {
		t.Fatalf("still within the interval (1999 < 2000) must be suppressed")
	}
	if !rl.allow(2000) {
		t.Fatalf("exactly one interval later must allow again")
	}
	if rl.allow(2500) {
		t.Fatalf("the next within-interval call must be suppressed")
	}
	if !rl.allow(4000) {
		t.Fatalf("another interval later must allow again")
	}
}

// A fresh limiter (lastAt 0, unarmed) must allow even when the first clock
// reading is itself 0 — the zero time must not be mistaken for "just warned".
func TestRateLimiterFirstCallAtZero(t *testing.T) {
	rl := newRateLimiter(2000)
	if !rl.allow(0) {
		t.Fatalf("first call at t=0 must allow")
	}
	if rl.allow(0) {
		t.Fatalf("immediate repeat at t=0 must be suppressed")
	}
}
