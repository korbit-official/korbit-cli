// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package orderbook renders the depth (orderbook) pane: asks above, a mid line,
// bids below, each level drawn over a side-colored depth bar. It is a pure view
// component — [Model.View] is a function of its explicit [Key] and [Data], owns
// no shared state, and caches its render via [uikit.Memo] keyed on [Key]. The
// parent supplies the data and a [Key] whose store revisions stand in for "did
// the book/ticker change", so an unrelated frame reuses the cached render.
package orderbook

import (
	"image/color"
	"math"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
	"github.com/shopspring/decimal"
)

// Key is the comparable cache key: equal Keys render identically. BookRev and
// TickerRev are the store's section revisions (book/ticker change ⇒ rev change ⇒
// re-render); Status is the book pane's shared classification (loading / no
// resting orders / present) that gates what the pane shows; W and H are the OUTER
// panel size; Style is the palette identity.
type Key struct {
	BookRev     uint64
	TickerRev   uint64
	Symbol      string
	Status      state.DataStatus
	TickerReady bool
	LastTick    state.Direction // colors the last-price mid line
	CursorPrice string          // order mode's ladder cursor: highlight this level's row ("" = none)
	Level       string          // the book's grouping level ("" = raw): title chip only
	W, H        int
	Style       uikit.StyleID
}

// Data is what the component renders on a cache miss. HasTicker mirrors the
// store's "ok" return; the book first arriving bumps BookRev, so the Key still
// decides the hit. There is no HasBook: whether the book is loading, empty, or
// present is Key.Status, the shared classification, and a second source for the
// same question is exactly how two panes come to disagree.
type Data struct {
	Book      state.Orderbook
	Ticker    state.Ticker
	HasTicker bool
}

// Model is the orderbook component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns an orderbook component.
func New() *Model { return &Model{} }

// View renders the full bordered pane for k/d, reusing the cached render when k
// is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	title := i18n.T("orderbook — %s", uikit.FmtSymbol(k.Symbol))
	if k.Level != "" {
		// Disclose that every level (and any figure derived from the book,
		// like order previews) is at this grouping, not the raw tick grid.
		title += " · " + i18n.T("grp") + " " + uikit.GroupThousands(k.Level)
	}
	switch k.Status {
	case state.StatusNotReady:
		return uikit.Panel(title, []string{uikit.StyDim.Render(i18n.T("loading…"))}, k.W, k.H)
	case state.StatusEmpty:
		return uikit.Panel(title, []string{uikit.StyDim.Render(i18n.T("no resting orders"))}, k.W, k.H)
	}
	pal := uikit.PaletteFor(uikit.ColorScheme(k.Style.Scheme), k.Style.Profile)
	w := k.W - 2    // inner content width (panel border)
	rows := k.H - 3 // content rows (panel border + title)
	lines := depthLines(w, rows, d, pal, k.TickerReady, k.LastTick, k.CursorPrice)
	return uikit.Panel(title, lines, k.W, k.H)
}

// depthHeader renders the column-label row: each label centered over its
// column (matching depthRow's price/qty widths and the two-space gap). Labels
// clip (never overflow) on a narrow column.
func depthHeader(priceW, qtyW int) string {
	h := uikit.PadCenter(i18n.T("price"), priceW) + "  " + uikit.PadCenter(i18n.T("qty"), qtyW)
	return uikit.StyDim.Render(h)
}

// splitHeader reserves the column-header row when the pane is tall enough to
// afford it, returning whether to draw it and how many rows are left for depth.
// It is the single place that decision is made: depthLines draws from it and
// RowPrices skips a row for it, so the render and the cursor/click mapping
// cannot disagree about which row is which. Making the header conditional on
// anything further (width, data state) belongs HERE, never at one call site.
func splitHeader(rows int) (header bool, body int) {
	if rows >= uikit.MinHeaderRows {
		return true, rows - 1
	}
	return false, rows
}

// visibleSides slices the book to what `rows` DEPTH rows (the header already
// deducted by splitHeader) can show per side. It is the single source of the
// depth slicing — depthLines renders from it and RowPrices exposes it — so a
// cursor/click mapping can never drift from the render.
func visibleSides(rows int, book state.Orderbook) (asks, bids []state.PriceLevel, perSide int) {
	perSide = (rows - 1) / 2
	if perSide < 1 {
		perSide = 1
	}
	asks = book.Asks
	if len(asks) > perSide {
		asks = asks[:perSide]
	}
	bids = book.Bids
	if len(bids) > perSide {
		bids = bids[:perSide]
	}
	return
}

// RowPrices returns, for a pane of outer height h, the price shown on each
// content row top to bottom — "" for the column header (when shown), the
// ask-side padding, and the mid line. It mirrors the render's slicing exactly:
// both read the same splitHeader and visibleSides.
func RowPrices(h int, book state.Orderbook) []string {
	rows := h - 3
	if rows < 1 {
		return nil
	}
	out := make([]string, 0, rows)
	header, body := splitHeader(rows)
	if header {
		out = append(out, "") // the column-header row: not a price
	}
	asks, bids, perSide := visibleSides(body, book)
	for i := 0; i < perSide-len(asks); i++ {
		out = append(out, "")
	}
	for i := len(asks) - 1; i >= 0; i-- {
		out = append(out, asks[i].Price)
	}
	out = append(out, "") // the mid line
	for _, b := range bids {
		out = append(out, b.Price)
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return out
}

// depthLines builds the inner rows: the column header (dropped on a short pane,
// see splitHeader), then worst ask on top → best at the spread, a mid line (last
// price when a fresh ticker exists), then bids best-first.
func depthLines(w, rows int, d Data, pal uikit.Palette, tickerReady bool, lastTick state.Direction, cursorPrice string) []string {
	priceW := clamp(w-12, 10, 16)
	qtyW := w - priceW - 2
	lines := make([]string, 0, rows)
	header, body := splitHeader(rows)
	if header {
		lines = append(lines, depthHeader(priceW, qtyW))
	}
	asks, bids, perSide := visibleSides(body, d.Book)
	// Scale every depth bar against the largest visible level on either side so
	// bids and asks are directly comparable. Display-only: the qty parse feeds a
	// bar width, never an order — the wire string is rendered untouched.
	maxQty := 0.0
	for _, l := range asks {
		maxQty = maxFloat(maxQty, parseDepthQty(l.Qty))
	}
	for _, l := range bids {
		maxQty = maxFloat(maxQty, parseDepthQty(l.Qty))
	}

	for i := 0; i < perSide-len(asks); i++ {
		lines = append(lines, "")
	}
	for i := len(asks) - 1; i >= 0; i-- { // worst ask on top, best at the spread
		cur := cursorPrice != "" && asks[i].Price == cursorPrice
		lines = append(lines, depthRow(pal.Down.Fg, pal.Down.BarBG, asks[i].Price, asks[i].Qty, priceW, qtyW, w, maxQty, cur))
	}

	mid := uikit.StyDim.Render(strings.Repeat("─", w))
	if d.HasTicker && tickerReady {
		label := " " + uikit.GroupThousands(d.Ticker.Close) + " "
		rendered := label // Neutral: no tick observed yet — uncolored last price
		switch lastTick {
		case state.Up:
			rendered = pal.Up.Fg.Render(label)
		case state.Down:
			rendered = pal.Down.Fg.Render(label)
		}
		pad := (w - len(label)) / 2
		if pad < 0 {
			pad = 0
		}
		mid = uikit.StyDim.Render(strings.Repeat("─", pad)) + rendered + uikit.StyDim.Render(strings.Repeat("─", max(0, w-pad-len(label))))
	}
	lines = append(lines, mid)

	for _, b := range bids {
		cur := cursorPrice != "" && b.Price == cursorPrice
		lines = append(lines, depthRow(pal.Up.Fg, pal.Up.BarBG, b.Price, b.Qty, priceW, qtyW, w, maxQty, cur))
	}
	return lines
}

// depthRow renders one level over an inline depth bar (a dim side-colored
// background whose width is proportional to the level's size vs maxQty). The bar
// is right-anchored — it grows leftward from the qty column it encodes, matching
// the combined-ladder convention of major exchanges. A nil barBG (a color-poor
// terminal) renders the plain row. cursor marks the order-mode ladder cursor's
// row: a ▸ in the left gutter and a bold row.
func depthRow(side lipgloss.Style, barBG color.Color, price, qty string, priceW, qtyW, w int, maxQty float64, cursor bool) string {
	text := uikit.PadLeft(uikit.GroupThousands(price), priceW) + "  " + uikit.PadLeft(uikit.ClipTail(qty, qtyW), qtyW)
	r := []rune(text)
	if len(r) > w {
		r = r[:w]
	}
	for len(r) < w {
		r = append(r, ' ')
	}
	if cursor {
		side = side.Bold(true)
		if len(r) > 0 && r[0] == ' ' {
			r[0] = '▸'
		}
	}
	barW := depthBarW(qty, maxQty, w)
	if barBG == nil || barW <= 0 {
		return side.Render(string(r))
	}
	if barW > len(r) {
		barW = len(r)
	}
	split := len(r) - barW // right-anchored: bar fills the trailing barW cells
	return side.Render(string(r[:split])) + side.Background(barBG).Render(string(r[split:]))
}

// depthBarW is the bar width in cells for a level of size qty against maxQty,
// scaled to w. Any non-zero level shows at least one cell; an unparseable or zero
// qty shows none. Display-only float math — never an order quantity.
func depthBarW(qty string, maxQty float64, w int) int {
	if maxQty <= 0 || w <= 0 {
		return 0
	}
	v := parseDepthQty(qty)
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

// parseDepthQty parses a decimal-string quantity through decimal.Decimal and
// converts it to a float for bar sizing only. Returns 0 on any parse failure
// (the level then renders without a bar).
func parseDepthQty(qty string) float64 {
	d, err := decimal.NewFromString(strings.TrimSpace(qty))
	if err != nil {
		return 0
	}
	v, _ := d.Float64()
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	return v
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
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
