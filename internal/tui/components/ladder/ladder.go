// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package ladder renders the trade-ladder body: a DOM-style vertical price
// ladder over the live book — MINE | bid qty | price | ask qty | MINE — asks
// above a last/spread line, bids below, the account's resting orders shown in
// the MINE columns at their price row. It is a pure view component —
// [Model.View] is a function of its explicit [Key] and [Data], owns no shared
// state, and caches its render via [uikit.Memo] keyed on [Key]. [Rows] is the
// single source of the row layout: the render draws from it and the parent's
// cursor walking and click mapping read it, so they can never drift apart.
package ladder

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/shopspring/decimal"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// Row is one ladder row. Rows are the live book's levels (not one-per-tick —
// on a KRW book a per-tick ladder would show a sliver of the market), plus
// synthetic rows for the account's resting orders that sit on no visible
// level: a distant own order pulls in as an edge row, an inside-spread order
// inserts at its price position — nothing the account owns is ever off-screen.
type Row struct {
	Price    string // "" for padding and the mid line
	Mid      bool   // the last/spread separator row
	Edge     bool   // synthetic: carries an own order, not a visible book level
	BidQty   string // book qty at this level on the bid side ("" = none)
	AskQty   string // book qty on the ask side ("" = none)
	MineBuy  string // the account's resting buy qty at this price ("" = none)
	MineSell string // the account's resting sell qty at this price ("" = none)
}

// Key is the comparable cache key: equal Keys render identically. BookRev,
// OrderRev and TickerRev are the store's section revisions (book / own orders /
// ticker change ⇒ rev change ⇒ re-render); AccountSeq is the active sub-account
// whose resting orders fill the MINE columns (Data.Mine) — it MUST be in the
// key because a pure account switch changes which orders are shown
// (OpenOrdersFor) without bumping OrderRev, so without it the memo would keep
// the previous account's MINE overlay; Settled and Ready gate the loading
// state; CursorPrice is the j/k cursor's row, ArmedPrice marks an armed order's
// price, FlashID highlights a just-accepted order in its MINE cell. W and H are
// the CONTENT size (the parent owns the panel border, title, and strip).
type Key struct {
	BookRev     uint64
	OrderRev    uint64
	TickerRev   uint64
	AccountSeq  int
	Symbol      string
	Settled     bool
	Ready       bool
	TickerReady bool
	LastTick    state.Direction // colors the last price on the mid line
	CursorPrice string          // the ladder cursor's row ("" = none)
	ArmedPrice  string          // an armed order's price row marker ("" = none)
	FlashID     int64           // a just-accepted order's id: flash its MINE cell (0 = none)
	Level       string          // the book's grouping level ("" = raw): buckets MINE prices
	W, H        int
	Style       uikit.StyleID
}

// Data is what the component renders on a cache miss. Mine is the account's
// open orders for the symbol (any revision-worthy change bumps OrderRev, so
// the Key still decides the hit).
type Data struct {
	Book      state.Orderbook
	HasBook   bool
	Ticker    state.Ticker
	HasTicker bool
	Mine      []state.Order
}

// Model is the ladder component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a ladder component.
func New() *Model { return &Model{} }

// View renders the ladder body (exactly k.H rows, no border) for k/d, reusing
// the cached render when k is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

// mineAt aggregates the account's resting orders by price: the summed
// remaining (unfilled) quantity per side, and whether the flashed order rests
// at the price.
type mineAt struct {
	buy, sell decimal.Decimal
	flash     bool
}

// mineByPrice indexes the open orders by price row — the exact price on a raw
// book, the order's bucket on a grouped one (Bucket). An order's remaining
// size is qty − filledQty (advisory display math on decimal copies; the wire
// strings are untouched); a row that fails to parse contributes nothing.
func mineByPrice(mine []state.Order, flashID int64, level string) map[string]mineAt {
	out := map[string]mineAt{}
	for _, o := range mine {
		if o.Price == "" {
			continue
		}
		qty, err := decimal.NewFromString(o.Qty)
		if err != nil {
			continue
		}
		if filled, err := decimal.NewFromString(o.FilledQty); err == nil {
			qty = qty.Sub(filled)
		}
		if !qty.IsPositive() {
			continue
		}
		price := Bucket(o.Price, level, o.Side == "sell")
		at := out[price]
		if o.Side == "sell" {
			at.sell = at.sell.Add(qty)
		} else {
			at.buy = at.buy.Add(qty)
		}
		if flashID != 0 && o.OrderID == flashID {
			at.flash = true
		}
		out[price] = at
	}
	return out
}

// Bucket maps an order's exact price onto a grouped book's level grid: a buy
// truncates down (it aggregates with the bids, which group downward), a sell
// rounds up (the asks group upward). level "" (the raw book) or an
// unparseable input returns the price unchanged. The parent's cancel-at-row
// matching uses the same mapping, so a MINE cell and the x key can never
// disagree about which row an order is on.
//
// The merge onto a book row compares decimal STRINGS, so it relies on the
// server's grouped level prices and Mul's output sharing one textual form
// (they do: both are plain integer-grid multiples with no trailing
// fractional zeros). A mismatch would only cost cosmetics — the order would
// render as its own synthetic row instead of merging onto the level's.
func Bucket(price, level string, sell bool) string {
	if level == "" {
		return price
	}
	p, err1 := decimal.NewFromString(price)
	l, err2 := decimal.NewFromString(level)
	if err1 != nil || err2 != nil || !l.IsPositive() {
		return price
	}
	q := p.Div(l)
	if sell {
		q = q.Ceil()
	} else {
		q = q.Floor()
	}
	return q.Mul(l).String()
}

// sideEntry is one candidate row on a ladder side while merging levels with
// own-order prices.
type sideEntry struct {
	price string
	qty   string // book qty ("" for a synthetic own-order row)
	dec   decimal.Decimal
}

// Rows lays out `rows` content rows for the book and the account's resting
// orders: asks above the mid line (worst at the top, best at the spread), bids
// below (best first). Every price the account rests at is guaranteed a row —
// a resting order on no visible level becomes a synthetic row at its sorted
// position, displacing the level farthest from the spread on its side. Buys
// merge into the bid side and sells into the ask side (a resting buy is always
// below the best ask, a resting sell above the best bid). It is the single
// row-layout source: the render and the parent's cursor/click mapping both
// read it.
func Rows(rows int, book state.Orderbook, mine []state.Order, level string) []Row {
	if rows < 1 {
		return nil
	}
	perSide := (rows - 1) / 2
	if perSide < 1 {
		perSide = 1
	}
	mineIdx := mineByPrice(mine, 0, level)

	asks := mergeSide(book.Asks, mineIdx, false, perSide)
	bids := mergeSide(book.Bids, mineIdx, true, perSide)

	out := make([]Row, 0, rows)
	for i := 0; i < perSide-len(asks); i++ {
		out = append(out, Row{})
	}
	for i := len(asks) - 1; i >= 0; i-- { // worst ask on top, best at the spread
		e := asks[i]
		r := Row{Price: e.price, AskQty: e.qty, Edge: e.qty == ""}
		if at, ok := mineIdx[e.price]; ok {
			r.MineBuy, r.MineSell = fmtMine(at.buy), fmtMine(at.sell)
		}
		out = append(out, r)
	}
	out = append(out, Row{Mid: true})
	for _, e := range bids {
		r := Row{Price: e.price, BidQty: e.qty, Edge: e.qty == ""}
		if at, ok := mineIdx[e.price]; ok {
			r.MineBuy, r.MineSell = fmtMine(at.buy), fmtMine(at.sell)
		}
		out = append(out, r)
	}
	for len(out) < rows {
		out = append(out, Row{})
	}
	return out[:rows]
}

// mergeSide merges one side's book levels with the account's own-order prices
// for that side (bids carry buys, asks carry sells) and keeps at most perSide
// entries: every own-order row survives, and the remaining slots go to the
// levels closest to the spread. The result is ordered best-first (bids
// descending, asks ascending).
func mergeSide(levels []state.PriceLevel, mine map[string]mineAt, bidSide bool, perSide int) []sideEntry {
	entries := make([]sideEntry, 0, len(levels)+len(mine))
	seen := map[string]bool{}
	for _, l := range levels {
		d, err := decimal.NewFromString(l.Price)
		if err != nil {
			continue
		}
		entries = append(entries, sideEntry{price: l.Price, qty: l.Qty, dec: d})
		seen[l.Price] = true
	}
	for price, at := range mine {
		has := at.buy.IsPositive()
		if !bidSide {
			has = at.sell.IsPositive()
		}
		if !has || seen[price] {
			continue
		}
		d, err := decimal.NewFromString(price)
		if err != nil {
			continue
		}
		entries = append(entries, sideEntry{price: price, dec: d})
	}
	// Best-first: bids descending, asks ascending. Levels arrive sorted, but
	// the synthetic rows were appended — order the merged set.
	sortEntries(entries, bidSide)
	if len(entries) <= perSide {
		return entries
	}
	// Keep every own-order (synthetic) row; fill the rest with the levels
	// closest to the spread (the best-first prefix).
	keep := make([]bool, len(entries))
	slots := perSide
	for i, e := range entries {
		if e.qty == "" {
			keep[i] = true
			slots--
		}
	}
	for i := range entries {
		if slots <= 0 {
			break
		}
		if !keep[i] {
			keep[i] = true
			slots--
		}
	}
	out := make([]sideEntry, 0, perSide)
	for i, e := range entries {
		if keep[i] && len(out) < perSide {
			out = append(out, e)
		}
	}
	return out
}

// sortEntries orders a side best-first (insertion sort — a ladder side is a
// couple dozen rows at most).
func sortEntries(entries []sideEntry, bidSide bool) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0; j-- {
			better := entries[j].dec.GreaterThan(entries[j-1].dec)
			if !bidSide {
				better = entries[j].dec.LessThan(entries[j-1].dec)
			}
			if !better {
				break
			}
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// fmtMine renders an aggregated own-side size ("" when none).
func fmtMine(d decimal.Decimal) string {
	if !d.IsPositive() {
		return ""
	}
	return d.String()
}

// render draws the ladder body: exactly k.H lines of k.W cells.
func render(k Key, d Data) string {
	if !d.HasBook || !k.Settled || !k.Ready {
		lines := make([]string, k.H)
		if k.H > 0 {
			lines[0] = uikit.StyDim.Render(i18n.T("loading…"))
		}
		return strings.Join(lines, "\n")
	}
	pal := uikit.PaletteFor(uikit.ColorScheme(k.Style.Scheme), k.Style.Profile)
	rows := Rows(k.H, d.Book, d.Mine, k.Level)
	flash := mineByPrice(d.Mine, k.FlashID, k.Level)

	mineW, qtyW, priceW := colWidths(k.W)
	maxQty := 0.0
	for _, r := range rows {
		maxQty = maxFloat(maxQty, parseQty(r.BidQty))
		maxQty = maxFloat(maxQty, parseQty(r.AskQty))
	}

	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		switch {
		case r.Mid:
			lines = append(lines, midLine(k, d, pal))
		case r.Price == "":
			lines = append(lines, "")
		default:
			cursor := k.CursorPrice != "" && r.Price == k.CursorPrice
			armed := k.ArmedPrice != "" && r.Price == k.ArmedPrice
			lines = append(lines, levelLine(r, pal, flash[r.Price].flash, cursor, armed, mineW, qtyW, priceW, maxQty))
		}
	}
	return strings.Join(lines, "\n")
}

// colWidths splits the content width into the five columns:
// ▸ | MINE | bid qty | price | ask qty | MINE. On a narrow ladder the MINE
// columns are dropped first (the row marker ► still flags own-order rows).
func colWidths(w int) (mineW, qtyW, priceW int) {
	priceW = clampInt(w/5, 10, 16)
	qtyW = clampInt((w-priceW-8)/4, 8, 14)
	mineW = (w - 5 - priceW - 2*qtyW) / 2
	if mineW < 5 {
		mineW = 0
		qtyW = clampInt((w-3-priceW)/2, 4, 18)
	}
	return
}

// levelLine renders one price row.
func levelLine(r Row, pal uikit.Palette, flash, cursor, armed bool, mineW, qtyW, priceW int, maxQty float64) string {
	marker := " "
	if cursor {
		marker = "▸"
	}
	var b strings.Builder
	b.WriteString(marker)

	buySty, sellSty := pal.Up.Fg, pal.Down.Fg
	if cursor {
		buySty, sellSty = buySty.Bold(true), sellSty.Bold(true)
	}
	if mineW > 0 {
		b.WriteString(mineCell(r.MineBuy, "%s ►", mineW, buySty, flash, true))
		b.WriteString(" ")
	}
	b.WriteString(qtyCell(r.BidQty, qtyW, pal.Up, cursor, true, maxQty))
	b.WriteString(" ")

	price := uikit.PadLeft(uikit.GroupThousands(r.Price), priceW)
	priceSty := uikit.StyDim
	switch {
	case armed:
		priceSty = uikit.StyWarn
	case cursor:
		priceSty = lipgloss.NewStyle().Bold(true)
	case r.Edge:
		// A synthetic own-order row off the visible book window.
	default:
		priceSty = lipgloss.NewStyle()
	}
	b.WriteString(priceSty.Render(price))
	b.WriteString(" ")

	b.WriteString(qtyCell(r.AskQty, qtyW, pal.Down, cursor, false, maxQty))
	if mineW > 0 {
		b.WriteString(" ")
		b.WriteString(mineCell(r.MineSell, "◄ %s", mineW, sellSty, flash, false))
	}
	return b.String()
}

// mineCell renders an own-order cell (right-aligned toward the ladder on the
// buy side, left-aligned on the sell side). A flashed cell (the just-accepted
// order) renders in the OK style.
func mineCell(qty, format string, w int, sty lipgloss.Style, flash, alignRight bool) string {
	if qty == "" {
		return strings.Repeat(" ", w)
	}
	text := strings.Replace(format, "%s", uikit.ClipTail(qty, w-2), 1)
	if alignRight {
		text = uikit.PadLeft(text, w)
	} else {
		text = uikit.PadRight(text, w)
	}
	if flash {
		return uikit.StyOK.Render(text)
	}
	return sty.Render(text)
}

// qtyCell renders a book-qty cell over a proportional depth bar (the
// orderbook pane's convention: a dim side-colored background, ≥1 cell for any
// non-zero level, dropped on a color-poor terminal). The bar anchors toward
// the price column: right-anchored on the bid side, left-anchored on the ask
// side.
func qtyCell(qty string, w int, side uikit.SideStyle, bold, bidSide bool, maxQty float64) string {
	sty := side.Fg
	if bold {
		sty = sty.Bold(true)
	}
	if qty == "" {
		return strings.Repeat(" ", w)
	}
	text := uikit.ClipTail(qty, w)
	if bidSide {
		text = uikit.PadLeft(text, w)
	} else {
		text = uikit.PadRight(text, w)
	}
	barW := barWidth(qty, maxQty, w)
	if side.BarBG == nil || barW <= 0 {
		return sty.Render(text)
	}
	r := []rune(text)
	if barW > len(r) {
		barW = len(r)
	}
	if bidSide { // bar fills the trailing cells (toward the price column)
		split := len(r) - barW
		return sty.Render(string(r[:split])) + sty.Background(side.BarBG).Render(string(r[split:]))
	}
	return sty.Background(side.BarBG).Render(string(r[:barW])) + sty.Render(string(r[barW:]))
}

// midLine renders the last/spread separator: a dim rule carrying the last
// price (colored by its tick direction) and the current spread.
func midLine(k Key, d Data, pal uikit.Palette) string {
	label := ""
	if d.HasTicker && k.TickerReady && d.Ticker.Close != "" {
		rendered := " last " + uikit.GroupThousands(d.Ticker.Close) + " "
		switch k.LastTick {
		case state.Up:
			rendered = pal.Up.Fg.Render(rendered)
		case state.Down:
			rendered = pal.Down.Fg.Render(rendered)
		}
		label = rendered
	}
	if s := spread(d.Book); s != "" {
		label += uikit.StyDim.Render(" spread " + uikit.GroupThousands(s) + " ")
	}
	plainW := lipgloss.Width(label)
	pad := (k.W - plainW) / 2
	if pad < 0 {
		pad = 0
	}
	return uikit.StyDim.Render(strings.Repeat("─", pad)) + label +
		uikit.StyDim.Render(strings.Repeat("─", maxInt(0, k.W-pad-plainW)))
}

// spread is best ask − best bid ("" when either side is empty or unparseable).
func spread(book state.Orderbook) string {
	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		return ""
	}
	bid, err1 := decimal.NewFromString(book.Bids[0].Price)
	ask, err2 := decimal.NewFromString(book.Asks[0].Price)
	if err1 != nil || err2 != nil {
		return ""
	}
	return ask.Sub(bid).String()
}

// parseQty converts a decimal-string qty to a float for bar sizing only (0 on
// any parse failure — the level then renders without a bar).
func parseQty(qty string) float64 {
	if qty == "" {
		return 0
	}
	d, err := decimal.NewFromString(qty)
	if err != nil {
		return 0
	}
	v, _ := d.Float64()
	if v < 0 {
		return 0
	}
	return v
}

// barWidth is the depth-bar width for a level of size qty against maxQty.
func barWidth(qty string, maxQty float64, w int) int {
	if maxQty <= 0 || w <= 0 {
		return 0
	}
	v := parseQty(qty)
	if v <= 0 {
		return 0
	}
	barW := int(float64(w) * v / maxQty)
	if barW < 1 {
		barW = 1
	}
	if barW > w {
		barW = w
	}
	return barW
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
