// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package clock

import "testing"

func TestSignNowLeansIntoThePast(t *testing.T) {
	local := int64(1_000_000)
	s := New(func() int64 { return local })

	// Unmeasured: bare local clock.
	if got := s.SignNow(); got != local {
		t.Errorf("unmeasured SignNow = %d, want %d (bare local clock)", got, local)
	}
	if s.Measured() {
		t.Error("Measured() = true before Install")
	}

	// Install offset +500, uncertainty (lean) 30. SignNow = local + offset - lean.
	s.Install(500, 30)
	if !s.Measured() {
		t.Error("Measured() = false after Install")
	}
	if got, want := s.SignNow(), local+500-30; got != want {
		t.Errorf("SignNow = %d, want %d (local + offset - lean)", got, want)
	}
	if got := s.Offset(); got != 500 {
		t.Errorf("Offset = %d, want 500 (raw, un-leaned)", got)
	}
}

func TestRecvWindowWideningThresholds(t *testing.T) {
	s := New(func() int64 { return 0 })

	// Unmeasured: the one window is 0 (omit — server applies its 5s default).
	if s.RecvWindowMs() != 0 {
		t.Fatalf("unmeasured RecvWindowMs must be 0, got %d", s.RecvWindowMs())
	}

	cases := []struct {
		name   string
		leanMs int64
		want   int // 6*lean capped at 60000, but 0 when <= the 5s default
	}{
		// 6*lean below the 5s default: omit (the server's 5s window covers it).
		{"small lean", 100, 0},
		// Exactly at the 5s default boundary: 6*833 = 4998 <= 5000 -> still omit.
		{"just under 5s", 833, 0},
		// Above 5s: the widened window is sent verbatim.
		{"above 5s", 1000, 6000},
		// Cap at the 60s server maximum.
		{"capped", 20000, 60000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s.Install(0, c.leanMs)
			if got := s.RecvWindowMs(); got != c.want {
				t.Errorf("RecvWindowMs = %d, want %d", got, c.want)
			}
		})
	}
}
