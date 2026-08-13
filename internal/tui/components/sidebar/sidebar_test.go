// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sidebar

import (
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

func sampleData() Data {
	return Data{Rows: []Row{
		{Symbol: "btc_krw", PriceChangePercent: "1.20", PriceChange: "+100", Ready: true},
		{Symbol: "eth_krw", PriceChangePercent: "-0.50", PriceChange: "-5", Ready: true},
		{Symbol: "xrp_krw", Ready: false},
	}}
}

func key() Key {
	return Key{TickerRev: 1, Active: "btc_krw", W: 24, H: 10}
}

func TestRendersRowsAndDimensions(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if got := strings.Count(out, "\n") + 1; got != 10 {
		t.Errorf("panel height = %d lines, want 10", got)
	}
	if !strings.Contains(out, "BTC/KRW") || !strings.Contains(out, "ETH/KRW") {
		t.Errorf("watched symbols should appear (human-friendly); got: %q", out)
	}
	if !strings.Contains(out, "1.20%") {
		t.Errorf("ready row should show the 24h change; got: %q", out)
	}
	// Footer is the active symbol's 1-based index over the total (btc is index 0).
	if !strings.Contains(out, "1/3") {
		t.Errorf("footer should show position 1/3; got: %q", out)
	}
}

func TestLoadingMarkerBeforeTicker(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	// The not-ready row (xrp) shows the compact loading marker, not a stale value.
	if !strings.Contains(out, "…") {
		t.Errorf("a row without a ticker should show the loading marker; got: %q", out)
	}
}

func TestSearchFiltersAndFooter(t *testing.T) {
	m := New()
	k := key()
	k.Searching = true
	k.Query = "eth"
	out := m.View(k, sampleData())
	if !strings.Contains(out, "ETH/KRW") {
		t.Errorf("search should keep the matching symbol; got: %q", out)
	}
	if strings.Contains(out, "BTC/KRW") {
		t.Errorf("search should drop non-matching symbols; got: %q", out)
	}
	if !strings.Contains(out, "1/1 match") {
		t.Errorf("search footer should show match position; got: %q", out)
	}
	// The live query renders in the bottom-left as "/eth".
	if !strings.Contains(out, "/eth") {
		t.Errorf("the live query should appear in the footer; got: %q", out)
	}
}

func TestSearchNoMatch(t *testing.T) {
	m := New()
	k := key()
	k.Searching = true
	k.Query = "zzz"
	out := m.View(k, sampleData())
	if !strings.Contains(out, "no match") {
		t.Errorf("a query matching nothing should show 'no match'; got: %q", out)
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := m.View(k, sampleData())
	// Different DATA but same KEY → cache hit returns the first render (the ticker
	// revision in the key is the contract for "data changed").
	other := sampleData()
	other.Rows[0].PriceChangePercent = "9.99"
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render reflecting the new data.
	k.TickerRev = 2
	if got := m.View(k, other); !strings.Contains(got, "9.99%") {
		t.Error("after a TickerRev bump the render should reflect the new ticker")
	}
}

func TestKeyChangeInvalidates(t *testing.T) {
	m := New()
	k := key()
	a := m.View(k, sampleData())
	k.Active = "eth_krw"
	b := m.View(k, sampleData())
	if a == b {
		t.Error("an active-symbol change should produce a different (re-rendered) frame")
	}
	k = key()
	k.Style = uikit.StyleID{Scheme: uint8(uikit.ColorSchemeRedBlue)}
	c := m.View(k, sampleData())
	if a == c {
		t.Error("a style change should produce a different (re-rendered) frame")
	}
}
