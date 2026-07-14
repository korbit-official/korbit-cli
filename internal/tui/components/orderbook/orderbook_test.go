// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package orderbook

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func sampleData() Data {
	return Data{
		HasBook: true,
		Book: state.Orderbook{
			Symbol: "btc_krw",
			Asks:   []state.PriceLevel{{Price: "100", Qty: "1"}, {Price: "101", Qty: "2"}},
			Bids:   []state.PriceLevel{{Price: "99", Qty: "3"}, {Price: "98", Qty: "1"}},
		},
		HasTicker: true,
		Ticker:    state.Ticker{Close: "99500", PriceChange: "+100"},
	}
}

func key() Key {
	return Key{BookRev: 1, TickerRev: 1, Symbol: "btc_krw", Settled: true, Ready: true, TickerReady: true, W: 30, H: 12}
}

func TestRendersDepthAndDimensions(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if strings.Contains(out, "loading") {
		t.Fatalf("ready book should not show loading: %q", out)
	}
	// Panel is H lines tall, each H... wide; assert the line count matches the box.
	if got := strings.Count(out, "\n") + 1; got != 12 {
		t.Errorf("panel height = %d lines, want 12", got)
	}
	// Prices/qty appear (thousands-grouped close in the mid line).
	if !strings.Contains(out, "99,500") {
		t.Errorf("mid line should show the grouped last price; got: %q", out)
	}
}

func TestLoadingWhenNotReady(t *testing.T) {
	m := New()
	k := key()
	k.Ready = false
	if out := m.View(k, sampleData()); !strings.Contains(out, "loading") {
		t.Errorf("not-ready book should show loading, got: %q", out)
	}
	k = key()
	if out := m.View(k, Data{HasBook: false}); !strings.Contains(out, "loading") {
		t.Errorf("absent book should show loading, got: %q", out)
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
	other.Ticker.Close = "1"
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render that reflects new data.
	k.TickerRev = 2
	if got := m.View(k, other); strings.Contains(got, "99,500") {
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

func TestDepthBarW(t *testing.T) {
	cases := []struct {
		qty    string
		maxQty float64
		w      int
		want   int
	}{
		{"4", 4, 20, 20},    // the largest level fills the width
		{"2", 4, 20, 10},    // half size → half width
		{"0.001", 4, 20, 1}, // a tiny non-zero level still shows one cell
		{"0", 4, 20, 0},     // a zero level shows no bar
		{"x", 4, 20, 0},     // an unparseable qty shows no bar
		{"2", 0, 20, 0},     // no liquidity anywhere → no bar
		{"2", 4, 0, 0},      // no room → no bar
	}
	for _, c := range cases {
		if got := depthBarW(c.qty, c.maxQty, c.w); got != c.want {
			t.Fatalf("depthBarW(%q, %v, %d) = %d, want %d", c.qty, c.maxQty, c.w, got, c.want)
		}
	}
}

// TestDepthRowKeepsTextAndDrawsBar pins the two contracts the depth graph rests
// on: the visible text (grouped price + qty) is unchanged, and a non-zero level
// emits a styled bar segment while the remainder is styled separately.
func TestDepthRowKeepsTextAndDrawsBar(t *testing.T) {
	up := uikit.PaletteFor(uikit.ColorSchemeGreenRed, colorprofile.TrueColor).Up
	row := depthRow(up.Fg, up.BarBG, "99120000", "0.003", 12, 6, 20, 0.003, false)
	if got := plain(row); !strings.Contains(got, "99,120,000") || !strings.Contains(got, "0.003") {
		t.Fatalf("depth row dropped its visible text: %q", got)
	}
	// A full-width bar (qty == maxQty) plus a non-bar tail means at least two
	// distinct SGR-styled segments in the rendered output.
	if n := strings.Count(row, "\x1b["); n < 2 {
		t.Fatalf("expected a styled depth bar, got %d escape sequences: %q", n, row)
	}
	// A level with no liquidity context (maxQty 0) renders plainly — no bar.
	if depthBarW("0.003", 0, 20) != 0 {
		t.Fatal("a level with no comparison size must render without a bar")
	}
	// A nil barBG (a color-poor terminal) renders the row with no bar at all: the
	// visible text survives but there is no second, background-styled segment.
	plainRow := depthRow(up.Fg, nil, "99120000", "0.003", 12, 6, 20, 0.003, false)
	if got := plain(plainRow); !strings.Contains(got, "99,120,000") {
		t.Fatalf("no-bar row dropped its text: %q", got)
	}
}

// TestDepthBarPaletteProfile pins the profile/color-scheme depth behavior: bars
// draw only where the terminal can render a subtle background, and a 256-color
// bar emits a 256-palette background rather than truecolor.
func TestDepthBarPaletteProfile(t *testing.T) {
	for _, c := range []struct {
		p    colorprofile.Profile
		bars bool
	}{
		{colorprofile.TrueColor, true},
		{colorprofile.ANSI256, true},
		{colorprofile.ANSI, false},
		{colorprofile.Ascii, false},
		{colorprofile.NoTTY, false},
	} {
		if got := uikit.PaletteFor(uikit.ColorSchemeGreenRed, c.p).DrawBars(); got != c.bars {
			t.Errorf("DrawBars on %v = %v, want %v", c.p, got, c.bars)
		}
	}
	// 256-color draws a bar as a 256-palette background (48;5;…), not truecolor.
	up256 := uikit.PaletteFor(uikit.ColorSchemeGreenRed, colorprofile.ANSI256).Up
	row256 := depthRow(up256.Fg, up256.BarBG, "99120000", "0.003", 12, 6, 20, 0.003, false)
	if !strings.Contains(row256, "48;5;") {
		t.Errorf("256-color depth bar must emit a 256-palette background: %q", row256)
	}
}

// ladderBook has distinct level counts per side to pin the row layout.
var ladderBook = state.Orderbook{
	Symbol: "btc_krw",
	Asks:   []state.PriceLevel{{Price: "101", Qty: "1"}, {Price: "102", Qty: "2"}, {Price: "103", Qty: "3"}},
	Bids:   []state.PriceLevel{{Price: "100", Qty: "1"}, {Price: "99", Qty: "2"}},
}

// RowPrices mirrors the render's slicing: ask-side padding, asks worst→best,
// the mid line, bids best→worst, tail padding.
func TestRowPrices(t *testing.T) {
	// h=13 → 10 content rows → perSide 4: pad 1, asks 103/102/101, mid, bids.
	got := RowPrices(13, ladderBook)
	want := []string{"", "103", "102", "101", "", "100", "99", "", "", ""}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q (%v)", i, got[i], want[i], got)
		}
	}
	// A tiny pane still yields one level per side around the mid.
	got = RowPrices(6, ladderBook)
	want = []string{"101", "", "100"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("small pane row %d = %q, want %q (%v)", i, got[i], want[i], got)
		}
	}
	if RowPrices(3, ladderBook) != nil {
		t.Fatal("no content rows → nil")
	}
}

// The rendered pane and RowPrices agree: the cursor row (matched by price)
// carries the ▸ marker on exactly the row RowPrices names.
func TestCursorRowMatchesRowPrices(t *testing.T) {
	k := Key{
		BookRev: 1, Symbol: "btc_krw", Settled: true, Ready: true,
		CursorPrice: "100", W: 24, H: 13,
	}
	lines := strings.Split(New().View(k, Data{Book: ladderBook, HasBook: true}), "\n")
	rows := RowPrices(k.H, ladderBook)
	cursorRow := -1
	for i, p := range rows {
		if p == "100" {
			cursorRow = i
			break
		}
	}
	if cursorRow < 0 {
		t.Fatal("cursor price not in RowPrices")
	}
	for i, l := range lines {
		has := strings.Contains(plain(l), "▸")
		if has && i != 2+cursorRow { // content rows start after border + title
			t.Fatalf("stray cursor marker on line %d: %q", i, l)
		}
		if !has && i == 2+cursorRow {
			t.Fatalf("cursor row %d should carry the ▸ marker: %q", i, l)
		}
	}
}
