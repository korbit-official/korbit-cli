// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package trades

import (
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

func sampleData() Data {
	return Data{
		Trades: []state.Trade{
			{TradeID: 2, Timestamp: 1_700_000_001_000, Price: "99500", Qty: "0.5", IsBuyerTaker: true},
			{TradeID: 1, Timestamp: 1_700_000_000_000, Price: "99400", Qty: "1.25", IsBuyerTaker: false},
		},
	}
}

func key() Key {
	return Key{TradeRev: 1, Symbol: "btc_krw", Status: state.StatusPresent, W: 30, H: 12}
}

func TestRendersTradesAndDimensions(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if strings.Contains(out, "loading") {
		t.Fatalf("ready trades should not show loading: %q", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 12 {
		t.Errorf("panel height = %d lines, want 12", got)
	}
	// Price appears thousands-grouped.
	if !strings.Contains(out, "99,500") {
		t.Errorf("a trade row should show the grouped price; got: %q", out)
	}
}

func TestLoadingWhenNotReady(t *testing.T) {
	m := New()
	k := key()
	k.Status = state.StatusNotReady
	if out := m.View(k, sampleData()); !strings.Contains(out, "loading") {
		t.Errorf("not-ready trades should show loading, got: %q", out)
	}
}

func TestEmptyShowsNoTradesYet(t *testing.T) {
	m := New()
	k := key()
	k.Status = state.StatusEmpty
	out := m.View(k, Data{})
	if strings.Contains(out, "loading") {
		t.Errorf("live-but-empty trades must not show loading, got: %q", out)
	}
	if !strings.Contains(out, "no trades yet") {
		t.Errorf("live-but-empty trades should show 'no trades yet', got: %q", out)
	}
}

func TestEmptyTradesMessage(t *testing.T) {
	m := New()
	if out := m.View(key(), Data{}); !strings.Contains(out, "no trades yet") {
		t.Errorf("empty trades should show the empty message, got: %q", out)
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := m.View(k, sampleData())
	// Different DATA but same KEY → cache hit returns the first render (the
	// revision in the key is the contract for "data changed", so a caller must
	// bump it; this proves the cache trusts the key).
	other := sampleData()
	other.Trades[0].Price = "1"
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render that reflects new data.
	k.TradeRev = 2
	if got := m.View(k, other); strings.Contains(got, "99,500") {
		t.Error("after a TradeRev bump the render should reflect the new trades, not the cached one")
	}
}

func TestStyleChangeInvalidates(t *testing.T) {
	m := New()
	k := key()
	a := m.View(k, sampleData())
	k.Style = uikit.StyleID{Scheme: uint8(uikit.ColorSchemeRedBlue)}
	b := m.View(k, sampleData())
	if a == b {
		t.Error("a style change should produce a different (re-rendered) frame")
	}
}

// TestHeaderGating: the time/price/qty header appears once the pane has
// uikit.MinHeaderRows content rows and is dropped below that so trades win.
func TestHeaderGating(t *testing.T) {
	k := key()
	k.H = uikit.MinHeaderRows + 3 // content rows == MinHeaderRows → header shows
	if out := New().View(k, sampleData()); !strings.Contains(out, "time") {
		t.Errorf("a pane with %d content rows should show the time/price/qty header: %q", uikit.MinHeaderRows, out)
	}
	k.H = uikit.MinHeaderRows + 2 // one fewer content row → header dropped
	if out := New().View(k, sampleData()); strings.Contains(out, "time") {
		t.Errorf("a pane with %d content rows should drop the header: %q", uikit.MinHeaderRows-1, out)
	}
}
