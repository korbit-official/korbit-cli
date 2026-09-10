// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package balances

import (
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/stream/state"
)

func sampleData() Data {
	return Data{
		Balances: []state.Balance{
			{Currency: "btc", Available: "1.5", Balance: "2"},
			{Currency: "eth", Available: "10", Balance: "12"},
			{Currency: "krw", Available: "1000000", Balance: "1500000"},
		},
	}
}

func key() Key {
	return Key{BalanceRev: 1, Ready: true, W: 30, H: 12}
}

func TestRendersListAndDimensions(t *testing.T) {
	m := New()
	k := key()
	k.W = 34 // wide enough that a 7-figure total still renders exact (not compact)
	out := m.View(k, sampleData())
	if strings.Contains(out, "loading") {
		t.Fatalf("ready balances should not show loading: %q", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 12 {
		t.Errorf("panel height = %d lines, want 12", got)
	}
	if !strings.Contains(out, "currency") {
		t.Errorf("pinned header should be present; got: %q", out)
	}
	if !strings.Contains(out, "BTC") {
		t.Errorf("balance rows should be present (human-friendly); got: %q", out)
	}
	// Thousands grouping on the numeric columns.
	if !strings.Contains(out, "1,500,000") {
		t.Errorf("totals should be thousands-grouped; got: %q", out)
	}
}

// TestNumberColumnsKeepAGutter: a long available value clips to its full cell
// width — the gutter is all that separates it from the total, so two
// full-width figures must never read as one number.
func TestNumberColumnsKeepAGutter(t *testing.T) {
	m := New()
	k := key()
	k.W = 26 // inner 24: curW 8, numW 7 — both figures clip to full cells
	out := m.View(k, Data{Balances: []state.Balance{
		{Currency: "btc", Available: "11.057813859067", Balance: "11.057813859067"},
	}})
	if !strings.Contains(out, "11.057… 11.057…") {
		t.Errorf("clipped available and total must stay separated by the gutter; got: %q", out)
	}
}

// TestCellsCompactWhenNarrow: a figure that cannot render exact in its cell
// compacts to a unit form — magnitude stays readable where a clipped grouped
// string would fake precision.
func TestCellsCompactWhenNarrow(t *testing.T) {
	m := New()
	k := key()
	k.W = 26 // inner 24: numW 7 — a 10-digit KRW figure cannot render exact
	out := m.View(k, Data{Balances: []state.Balance{
		{Currency: "krw", Available: "9621139961.9", Balance: "9625266246.6"},
	}})
	if !strings.Contains(out, "9.62B") || !strings.Contains(out, "9.63B") {
		t.Errorf("narrow cells must compact, got: %q", out)
	}
}

func TestLoadingWhenNotReady(t *testing.T) {
	m := New()
	k := key()
	k.Ready = false
	if out := m.View(k, sampleData()); !strings.Contains(out, "loading") {
		t.Errorf("not-ready balances should show loading, got: %q", out)
	}
}

func TestNoBalances(t *testing.T) {
	m := New()
	if out := m.View(key(), Data{}); !strings.Contains(out, "no balances") {
		t.Errorf("ready but empty should show 'no balances', got: %q", out)
	}
}

func TestSearchFilters(t *testing.T) {
	m := New()
	k := key()
	k.Searching = true
	k.Query = "eth"
	out := m.View(k, sampleData())
	if !strings.Contains(out, "ETH") {
		t.Errorf("matching currency should appear; got: %q", out)
	}
	if strings.Contains(out, "BTC") {
		t.Errorf("non-matching currency should be filtered out; got: %q", out)
	}
	if !strings.Contains(out, "match") {
		t.Errorf("search footer should report matches; got: %q", out)
	}
	// A query matching nothing shows the no-match state.
	k.Query = "zzz"
	if out := m.View(k, sampleData()); !strings.Contains(out, "no match") {
		t.Errorf("non-matching query should show 'no match', got: %q", out)
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := m.View(k, sampleData())
	// Different DATA but same KEY → cache hit returns the first render (the
	// revision in the key is the contract for "data changed").
	other := sampleData()
	other.Balances[2].Balance = "9"
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render that reflects new data.
	k.BalanceRev = 2
	if got := m.View(k, other); strings.Contains(got, "1,500,000") {
		t.Error("after a BalanceRev bump the render should reflect the new data, not the cached one")
	}
}

func TestKeyChangeInvalidates(t *testing.T) {
	m := New()
	// A taller list with more rows than fit, so the scroll window is meaningful.
	d := Data{}
	for i := 0; i < 30; i++ {
		d.Balances = append(d.Balances, state.Balance{Currency: "c" + string(rune('a'+i%26)), Available: "1", Balance: "2"})
	}
	k := key()
	a := m.View(k, d)
	k.Scroll = 5
	b := m.View(k, d)
	if a == b {
		t.Error("a scroll change should produce a different (re-rendered) frame")
	}
}
