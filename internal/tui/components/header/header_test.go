// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/stream/state"
	"github.com/digitalx-official/digitalx-cli/internal/tui/uikit"
)

func sampleData() Data {
	return Data{
		Status: state.StatusPresent,
		Ticker: state.Ticker{
			Symbol:             "btc_krw",
			Close:              "99500000",
			PriceChange:        "+100",
			PriceChangePercent: "1.5",
			BestBidPrice:       "99490000",
			BestAskPrice:       "99510000",
			Low:                "98000000",
			High:               "100000000",
			Volume:             "12.3",
		},
	}
}

func key() Key {
	return Key{TickerRev: 1, Symbol: "btc_krw", MarketCount: 3, KeyName: "main", BaseURL: "https://api.digitalx.miraeasset.com", W: 100}
}

func TestRendersTwoLinesAndContent(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if strings.Contains(out, "loading") {
		t.Fatalf("ready ticker should not show loading: %q", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 2 {
		t.Errorf("header = %d lines, want 2", got)
	}
	// The product name, not the binary name (`dgx-cli`): the header names the
	// application, not a command to type.
	if !strings.Contains(out, "digitalx-cli tui") {
		t.Errorf("line 1 should name the app; got: %q", out)
	}
	if !strings.Contains(out, "3 markets") {
		t.Errorf("line 1 should show the market count; got: %q", out)
	}
	if !strings.Contains(out, "key:main") {
		t.Errorf("line 1 should show the key identity; got: %q", out)
	}
	// Thousands-grouped last price appears on line 2.
	if !strings.Contains(out, "99,500,000") {
		t.Errorf("line 2 should show the grouped last price; got: %q", out)
	}
}

func TestPublicWhenNoKey(t *testing.T) {
	m := New()
	k := key()
	k.KeyName = ""
	out := m.View(k, sampleData())
	if !strings.Contains(out, "public") {
		t.Errorf("absent key should render the public identity; got: %q", out)
	}
}

func TestLoadingWhenNotReady(t *testing.T) {
	m := New()
	k := key()
	if out := m.View(k, Data{Status: state.StatusNotReady}); !strings.Contains(out, "loading") {
		t.Errorf("not-ready ticker should show loading, got: %q", out)
	}
}

func TestEmptyShowsAwaitingFirstTrade(t *testing.T) {
	m := New()
	if out := m.View(key(), Data{Status: state.StatusEmpty}); strings.Contains(out, "loading") || !strings.Contains(out, "awaiting first trade") {
		t.Errorf("empty ticker should await first trade (not load), got: %q", out)
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := m.View(k, sampleData())
	// Different DATA but same KEY → cache hit returns the first render (the
	// revision in the key is the contract for "data changed").
	other := sampleData()
	other.Ticker.Close = "1"
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render that reflects new data.
	k.TickerRev = 2
	if got := m.View(k, other); strings.Contains(got, "99,500,000") {
		t.Error("after a TickerRev bump the render should reflect the new ticker, not the cached one")
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
