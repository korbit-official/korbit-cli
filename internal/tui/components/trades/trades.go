// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package trades renders the recent-trades (time & sales) pane: newest trade on
// top, each row showing local time, price, and quantity, with the price colored
// by taker side (buyer-taker up, seller-taker down). It is a pure view component
// — [Model.View] is a function of its explicit [Key] and [Data], owns no shared
// state, and caches its render via [uikit.Memo] keyed on [Key]. The parent
// supplies the data and a [Key] whose trade revision stands in for "did the
// trades change", so an unrelated frame reuses the cached render.
package trades

import (
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. TradeRev is
// the store's trade-section revision (trades change ⇒ rev change ⇒ re-render);
// Settled (the active-symbol subscription has settled) and Ready (an
// authoritative trade history has arrived) gate the loading state; W and H are
// the OUTER panel size; Style is the palette identity.
type Key struct {
	TradeRev uint64
	Symbol   string
	Settled  bool
	Ready    bool
	W, H     int
	Style    uikit.StyleID
}

// Data is what the component renders on a cache miss. Trades is newest-first; a
// new trade bumps TradeRev, so the Key still decides the hit.
type Data struct {
	Trades []state.Trade
}

// Model is the trades component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a trades component.
func New() *Model { return &Model{} }

// View renders the full bordered pane for k/d, reusing the cached render when k
// is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	title := i18n.T("trades — %s", uikit.FmtSymbol(k.Symbol))
	if !k.Settled || !k.Ready {
		return uikit.Panel(title, []string{uikit.StyDim.Render(i18n.T("loading…"))}, k.W, k.H)
	}
	pal := uikit.PaletteFor(uikit.ColorScheme(k.Style.Scheme), k.Style.Profile)
	w := k.W - 2    // inner content width (panel border)
	rows := k.H - 3 // content rows (panel border + title)
	lines := tradeLines(w, rows, d, pal)
	return uikit.Panel(title, lines, k.W, k.H)
}

// tradeLines builds the inner rows: newest trade on top, each row time + price +
// qty over two single-space gaps.
func tradeLines(w, rows int, d Data, pal uikit.Palette) []string {
	trades := d.Trades
	if len(trades) == 0 {
		return []string{uikit.StyDim.Render(i18n.T("no trades yet"))}
	}
	if len(trades) > rows {
		trades = trades[:rows]
	}
	// Layout: time(8) + price + qty, two single-space gaps. Bias width toward
	// price (a KRW price is ~10-11 digits) while keeping qty non-zero.
	avail := w - 10
	if avail < 9 {
		avail = 9
	}
	priceW := clamp(avail-4, 6, 13)
	qtyW := avail - priceW
	if qtyW < 3 {
		qtyW = 3
		priceW = avail - qtyW
	}
	lines := make([]string, 0, len(trades))
	for _, t := range trades {
		sty := pal.Up.Fg
		if !t.IsBuyerTaker {
			sty = pal.Down.Fg
		}
		lines = append(lines, uikit.StyDim.Render(uikit.FmtClock(t.Timestamp))+" "+
			sty.Render(uikit.PadLeft(uikit.ClipTail(uikit.GroupThousands(t.Price), priceW), priceW))+" "+
			uikit.PadLeft(uikit.ClipTail(t.Qty, qtyW), qtyW))
	}
	return lines
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
