// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package clienv_test

import (
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
)

// TestParseTimeSyncMode pins the value vocabulary: case-insensitive, trimmed,
// empty defaults to auto, and any other word is rejected (the dispatch layer
// turns that ok=false into a usage error).
func TestParseTimeSyncMode(t *testing.T) {
	cases := []struct {
		in     string
		want   clienv.TimeSyncMode
		wantOK bool
	}{
		{"", clienv.TimeSyncAuto, true},
		{"auto", clienv.TimeSyncAuto, true},
		{"on", clienv.TimeSyncOn, true},
		{"off", clienv.TimeSyncOff, true},
		{"  On  ", clienv.TimeSyncOn, true},
		{"OFF", clienv.TimeSyncOff, true},
		{"AUTO", clienv.TimeSyncAuto, true},
		{"sometimes", clienv.TimeSyncAuto, false}, // unknown -> auto fallback, not ok
		{"true", clienv.TimeSyncAuto, false},      // a bare boolean is not a valid --time-sync value
	}
	for _, c := range cases {
		got, ok := clienv.ParseTimeSyncMode(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseTimeSyncMode(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// TestTimeSyncModeBehaviorGates pins the two gates the consumers read: Proactive
// (measure up front) is on only; Reactive (resync on rejection) is everything but
// off. The zero value degrades to auto's behavior.
func TestTimeSyncModeBehaviorGates(t *testing.T) {
	cases := []struct {
		mode          clienv.TimeSyncMode
		wantProactive bool
		wantReactive  bool
	}{
		{clienv.TimeSyncOn, true, true},
		{clienv.TimeSyncAuto, false, true},
		{clienv.TimeSyncOff, false, false},
		{clienv.TimeSyncMode(""), false, true}, // zero value behaves as auto
	}
	for _, c := range cases {
		if got := c.mode.Proactive(); got != c.wantProactive {
			t.Errorf("%q.Proactive() = %v, want %v", c.mode, got, c.wantProactive)
		}
		if got := c.mode.Reactive(); got != c.wantReactive {
			t.Errorf("%q.Reactive() = %v, want %v", c.mode, got, c.wantReactive)
		}
	}
}
