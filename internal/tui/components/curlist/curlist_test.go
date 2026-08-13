// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package curlist

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func rows() []Row {
	return []Row{
		{Code: "KRW", Avail: "900000", Value: "1000000", Held: true},
		{Code: "ETH", Avail: "2", Value: "6000000", Held: true},
		{Divider: true},
		{Code: "ADA", Held: false},
		{Code: "BTC", Held: false, Suspended: true},
	}
}

func TestRenderRowsAndDivider(t *testing.T) {
	m := New()
	out := plain(m.View(Key{Rev: 1, Ready: true, SelIdx: 1, W: 34, H: 12}, Data{Rows: rows()}))
	for _, want := range []string{"currencies", "KRW", "1,000,000", "ETH", "6,000,000", "no balance", "ADA", "⊘BTC", "2/4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render must contain %q:\n%s", want, out)
		}
	}
	// A row without an estimate leaves the value column blank, never "0".
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "ADA") && strings.Contains(line, "0") {
			t.Fatalf("no-value row must stay blank: %q", line)
		}
	}
}

func TestMemoHitAndMiss(t *testing.T) {
	m := New()
	k := Key{Rev: 1, Ready: true, SelIdx: 0, W: 34, H: 12}
	first := m.View(k, Data{Rows: rows()})
	// Same key: the cached render comes back even with different data.
	if got := m.View(k, Data{Rows: nil}); got != first {
		t.Fatal("equal keys must reuse the cached render")
	}
	k.Rev = 2
	if got := m.View(k, Data{Rows: nil}); got == first {
		t.Fatal("a bumped Rev must re-render")
	}
}

func TestLoadingAndEmptyStates(t *testing.T) {
	m := New()
	out := plain(m.View(Key{Rev: 1, Ready: false, W: 34, H: 10}, Data{}))
	if !strings.Contains(out, "loading…") {
		t.Fatalf("not-ready empty list must say loading:\n%s", out)
	}
	out = plain(m.View(Key{Rev: 2, Ready: true, W: 34, H: 10}, Data{}))
	if !strings.Contains(out, "no match") {
		t.Fatalf("ready empty list (filtered out) must say no match:\n%s", out)
	}
}
