// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package transfers

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func data() Data {
	return Data{
		Cols:    []string{"time", "amount", "status", "id"},
		Weights: []int{11, 14, 12, 8},
		Rows: [][]string{
			{"07-01 10:00", "0.0100", "reviewing", "9812"},
			{"06-21 09:41", "0.2000", "done", "9755"},
		},
	}
}

func TestRenderTable(t *testing.T) {
	m := New()
	out := plain(m.View(Key{Rev: 1, Title: "withdrawals · BTC (2)", Cursor: 0, Count: 2, W: 60, H: 10}, data()))
	for _, want := range []string{"withdrawals · BTC (2)", "9812", "reviewing", "done", "1/2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render must contain %q:\n%s", want, out)
		}
	}
}

func TestStates(t *testing.T) {
	m := New()
	out := plain(m.View(Key{Rev: 1, Title: "t", Loading: true, W: 60, H: 8}, Data{}))
	if !strings.Contains(out, "loading…") {
		t.Fatalf("loading state:\n%s", out)
	}
	out = plain(m.View(Key{Rev: 2, Title: "t", Err: "boom", W: 60, H: 8}, Data{}))
	if !strings.Contains(out, "load failed: boom") {
		t.Fatalf("error state:\n%s", out)
	}
	out = plain(m.View(Key{Rev: 3, Title: "t", W: 60, H: 8}, Data{Cols: data().Cols, Weights: data().Weights}))
	if !strings.Contains(out, "no records") {
		t.Fatalf("empty state:\n%s", out)
	}
}

func TestMemoKeyedOnRev(t *testing.T) {
	m := New()
	k := Key{Rev: 1, Title: "t", Count: 2, W: 60, H: 10}
	first := m.View(k, data())
	if got := m.View(k, Data{}); got != first {
		t.Fatal("equal keys must reuse the cached render")
	}
	k.Cursor = 1
	if got := m.View(k, data()); got == first {
		t.Fatal("a cursor move must re-render")
	}
}
