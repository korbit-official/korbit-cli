// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/tui/components/ladder"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// The trade ladder's rendering and geometry: the ladder component fills the
// orderbook+trades center region, and the sub-model's confirm strip docks
// into the panel's foot. The component's Rows layout is the single row
// source — the render, the cursor walking, and the click mapping all read it
// through ladderPrices/ladderRowAt, so they can never drift apart.

// ladderChipSpan records a title chip's click columns (relative to the title's
// column 0): the ‹ (prev) and › (next) cells, and the value cells in between (a
// click there just focuses the chip).
type ladderChipSpan struct {
	kind         ladderChip
	prevX, nextX int
	valX0, valX1 int
}

// titleLayout renders the ladder title — symbol plus the size and default-tif
// chips — and returns each chip's click span. Only the focused chip is
// highlighted (browse only), so the guilleted chips read as real steppers, not
// always-focused decoration. Render and the click hit-test share this, so the
// targets can't drift from what is drawn. The grouping chip is appended later
// by renderLadder, AFTER both chips, so it never shifts their columns. The size
// persists per symbol, the tif is session-wide.
func (l ladderModel) titleLayout() (string, ladderChipSpan, ladderChipSpan) {
	focusable := l.view == ladderBrowse
	var b strings.Builder
	col := 0
	add := func(plain string) {
		b.WriteString(plain)
		col += lipgloss.Width(plain)
	}
	chip := func(kind ladderChip, inner string, hint bool) ladderChipSpan {
		w := lipgloss.Width(inner)
		span := ladderChipSpan{kind: kind, prevX: col, valX0: col + 2, valX1: col + 1 + w, nextX: col + 3 + w}
		s := "‹ " + inner + " ›"
		switch {
		case focusable && l.titleFocus == kind:
			s = uikit.StyFocusLbl.Render(s)
		case hint: // an unset size chip is a dim prompt, not a value
			s = uikit.StyDim.Render(s)
		}
		b.WriteString(s)
		col += w + 4
		return span
	}

	add(i18n.T("trade ladder —") + " ")
	add(uikit.FmtSymbol(l.symbol))
	add(" · " + i18n.T("size") + " ")
	sizeInner, unset := i18n.T("press %s", sizeKeysLabel(l.sizeLevels)), l.sizePct() == 0
	if !unset {
		sizeInner = presetLabel(l.sizePct())
	}
	sizeSpan := chip(chipSize, sizeInner, unset)
	add(" · tif ")
	tifSpan := chip(chipTif, tifOptions[l.defaultTifIdx], false)
	return b.String(), sizeSpan, tifSpan
}

// title is the rendered ladder title (without the grouping chip renderLadder
// appends).
func (l ladderModel) title() string {
	s, _, _ := l.titleLayout()
	return s
}

// stripLines is the ladder's foot strip for the current view, separator
// included: a one-line size/error hint while browsing, the armed review
// (resolved order + live preview + controls) on confirm, and the on-the-wire
// note while busy. The render and the body-height math both read it, so the
// ladder rows above always shrink by exactly what the strip occupies.
func (l ladderModel) stripLines(inner int, gate string, bands []ops.TickBand, bounds ops.OrderValueBounds, fees *FeeRates, pal uikit.Palette) []string {
	sep := uikit.StyDim.Render(strings.Repeat("─", maxInt(0, inner)))
	switch l.view {
	case ladderConfirm:
		if l.armed.kind == armCancel {
			return append([]string{sep}, l.cancelStripLines(inner)...)
		}
		return append([]string{sep}, l.confirmStripLines(inner, gate, bands, bounds, fees)...)
	case ladderBusy:
		verb := i18n.T("order")
		if l.armed.kind == armCancel {
			verb = i18n.T("cancel")
		}
		return []string{sep,
			uikit.StyDim.Render(uikit.Truncate(i18n.T("%s on the wire — the result lands in a moment (esc hides this)", verb), inner))}
	}
	return []string{sep, l.browseFootLine(inner, bands, bounds, fees, pal)}
}

// browseFootLine is the browse strip: an inline error when one stands,
// otherwise what the armed size means — resolved live at the cursor price,
// per side, with any notional-bound breach flagged — and the verbs it feeds.
// Resolving BEFORE the arm is the point: a preset whose notional the server
// would reject must not look placeable until enter is one key away.
func (l ladderModel) browseFootLine(inner int, bands []ops.TickBand, bounds ops.OrderValueBounds, fees *FeeRates, pal uikit.Palette) string {
	if l.stripErr != "" {
		return uikit.StyErr.Render(uikit.Truncate("⚠ "+l.stripErr, inner))
	}
	pct := l.sizePct()
	if pct == 0 {
		return uikit.StyDim.Render(uikit.Truncate(
			i18n.T("press %s to arm a size (%s %% of balance), then b/s places at the cursor", sizeKeysLabel(l.sizeLevels), sizeValuesLabel(l.sizeLevels)), inner))
	}
	// Three groups, each visually distinct: the size label + per-side estimates
	// (the estimates in the book's bid/ask colors, so buy vs sell reads at a
	// glance and the live block stands out from the hints), a " │ " group
	// boundary, then the dim action hints. Style each fragment on its own — never
	// wrap the whole line in one style, because the warn ⚠ / side-color segments
	// end with an ANSI reset that would otherwise leak into the following text.
	line := uikit.StyDim.Render(i18n.T("size %s", presetLabel(pct)))
	if est := l.presetEstimates(pct, bands, bounds, fees, pal); est != "" {
		line += uikit.StyDim.Render(" ≈ ") + est
	} else {
		line += uikit.StyDim.Render(" " + i18n.T("of balance"))
	}
	line += uikit.StyDim.Render(" │ " + i18n.T("b/s:limit at cursor · B/S:market · x:cancel at row · t:tif"))
	return uikit.Truncate(line, inner)
}

// presetEstimates renders what the armed preset resolves to at the cursor
// price, per side ("" when nothing resolves yet — no cursor row or no
// balances). Each side runs the same engine path the arm-plus-confirm pair
// would (applyPreset, then buildPreview over the sized draft), so a notional
// the server would bounce carries its ⚠ here, before the arm.
func (l ladderModel) presetEstimates(pct int, bands []ops.TickBand, bounds ops.OrderValueBounds, fees *FeeRates, pal uikit.Palette) string {
	if l.cursorPrice == "" {
		return ""
	}
	book, hasBook := l.store.Orderbook(l.symbol)
	var parts []string
	for _, side := range []string{"buy", "sell"} {
		d := newOrderDraft(l.symbol, side)
		d.price = l.cursorPrice
		sized, reason := applyPreset(d, l.store.BalancesFor(l.accountSeq), l.store.BalancesReady(l.accountSeq), fees, pct)
		if reason != "" {
			continue
		}
		p := buildPreview(sized, book, hasBook, l.store.BalancesFor(l.accountSeq), bands, bounds, fees)
		// Each fragment carries its own style (see browseFootLine): the side in
		// the book's bid/ask color (buy = Up, sell = Down — the same colors the
		// ladder rows use), plus any warn ⚠ pop, so callers concatenate without
		// an outer wrap.
		sideSty := pal.Up.Fg
		if side == "sell" {
			sideSty = pal.Down.Fg
		}
		seg := sideSty.Render(side + " " + sized.sizeValue() + " " + uikit.FmtCurrency(p.BaseCcy))
		for _, w := range p.Warnings {
			switch w.Code {
			case ops.WarnNotionalAboveMax:
				seg += uikit.StyWarn.Render(" ⚠ >max")
			case ops.WarnNotionalBelowMin:
				seg += uikit.StyWarn.Render(" ⚠ <min")
			}
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, uikit.StyDim.Render(" · "))
}

// confirmStripLines is the armed review: the fully resolved order, the
// engine's live preview against the current book (notional, fee estimate,
// freshness), the preplace warnings — each on its own line, like the panel:
// the facts line truncates at narrow widths, and a clipped warning is a
// safety disclosure silently lost — any inline rejection, and the controls.
func (l ladderModel) confirmStripLines(inner int, gate string, bands []ops.TickBand, bounds ops.OrderValueBounds, fees *FeeRates) []string {
	d := l.armed.draft
	book, hasBook := l.store.Orderbook(d.symbol)
	p := buildPreview(d, book, hasBook, l.store.BalancesFor(l.accountSeq), bands, bounds, fees)

	size := d.sizeValue()
	unit := uikit.FmtCurrency(p.BaseCcy)
	if d.usesAmt() {
		// GroupExact, not GroupThousands: the amount and price are the wire
		// values this armed review commits to, so the figures on screen must
		// equal the bytes sent to the last digit (the order panel's confirm rule).
		size, unit = uikit.GroupExact(size), uikit.FmtCurrency(p.QuoteCcy)
	}
	head := []string{strings.ToUpper(d.side) + " " + size + " " + unit}
	if d.typ != "limit" {
		// "@ price" already says limit; naming the type would spend the
		// notional's room on this one-line strip.
		head = append(head, d.typ)
	}
	if d.usesPrice() {
		head = append(head, "@ "+uikit.GroupExact(d.price))
	}
	head = append(head, d.tif())
	if d.typ == "market" && d.pp {
		head = append(head, "pp")
	}
	if p.Notional != "" {
		lead := "= "
		if d.notionalIsEstimate() { // a market sell's notional moves with the book
			lead = "~ "
		}
		head = append(head, lead+uikit.GroupThousands(p.Notional)+" "+uikit.FmtCurrency(p.QuoteCcy))
	}
	line1 := uikit.StyWarn.Render(i18n.T("CONFIRM")+" ") + strings.Join(head, " · ")

	var facts []string
	if p.FeeEst != "" {
		facts = append(facts, i18n.T("fee")+" ~"+uikit.GroupThousands(p.FeeEst)+" ("+p.FeeKind+")")
	}
	if t := fmtTickHint(bands, d.price); t != "" {
		facts = append(facts, i18n.T("tick")+" "+uikit.GroupThousands(t))
	}
	if p.OK && p.Sim.Marketable && p.Sim.EstAvgFillPrice != "" {
		facts = append(facts, i18n.T("est fill")+" ~"+p.Sim.EstFilledQty+" @ avg "+uikit.GroupThousands(p.Sim.EstAvgFillPrice))
	}
	switch {
	case !p.OK:
		facts = append(facts, uikit.StyWarn.Render(p.Err))
	case len(p.Warnings) == 0:
		facts = append(facts, i18n.T("⚠ none"))
	}
	if gate == "" {
		facts = append(facts, uikit.StyOK.Render(i18n.T("book ● live")))
	} else {
		facts = append(facts, uikit.StyWarn.Render(i18n.T("book ○ %s", gate)))
	}

	out := []string{
		uikit.Truncate(line1, inner),
		uikit.Truncate(strings.Join(facts, " · "), inner),
	}
	for _, w := range p.Warnings {
		out = append(out, warningLine(w, inner))
	}
	if l.stripErr != "" {
		out = append(out, uikit.StyErr.Render(uikit.Truncate("⚠ "+l.stripErr, inner)))
	}
	hint := i18n.T("enter:place · esc:back")
	if d.usesPrice() {
		hint = i18n.T("enter:place · %s:±1 tick · %s:±10 · t:tif (this order) · esc:back", keyTickStep1, keyTickStep10)
	}
	return append(out, uikit.StyDim.Render(uikit.Truncate(hint, inner)))
}

// cancelStripLines is the armed cancel review: the captured resting order and
// the controls.
func (l ladderModel) cancelStripLines(inner int) []string {
	a := l.armed
	line := uikit.StyWarn.Render(i18n.T("CANCEL")+" ") + a.cancelSide + " " + a.cancelQty +
		" @ " + uikit.GroupThousands(a.cancelPrice) + uikit.StyDim.Render("  (orderId "+strconv.FormatInt(a.cancelID, 10)+")")
	out := []string{uikit.Truncate(line, inner)}
	if l.stripErr != "" {
		out = append(out, uikit.StyErr.Render(uikit.Truncate("⚠ "+l.stripErr, inner)))
	}
	return append(out, uikit.StyDim.Render(uikit.Truncate(i18n.T("enter:cancel the order · esc:back"), inner)))
}

// --- parent-side geometry and rendering ---

// ladderBands/ladderFees are the active symbol's cached market metadata. They
// live on the order panel's caches — one fetch (kicked when any entry surface
// opens) feeds the panel, the command bar, and the ladder alike.
func (m model) ladderBands() []ops.TickBand { return m.order.bands[m.symbol()] }

func (m model) ladderBounds() ops.OrderValueBounds { return m.order.boundsFor(m.symbol()) }
func (m model) ladderFees() *FeeRates              { return m.order.symFeesFor(m.symbol()) }

// ladderBusyGate is the money single-flight alone — the arm/confirm gate for
// a ladder cancel, which (like x elsewhere) must not require a fresh book.
func (m model) ladderBusyGate() string {
	if m.moneyActionInFlight() {
		return "another money action is already in flight — wait for it to finish"
	}
	return ""
}

// ladderGeom is the ladder panel's left screen column and outer width: the
// orderbook+trades center region.
func (m model) ladderGeom() (left, w int) {
	sideW, bookW, tradesW, _ := m.colWidths()
	return sideW, bookW + tradesW
}

// ladderStrip is the current foot strip (the single source renderLadder and
// the body-row math read).
func (m model) ladderStrip(inner int) []string {
	return m.ladder.stripLines(inner, m.orderGate(), m.ladderBands(), m.ladderBounds(), m.ladderFees(), m.pal)
}

// ladderBodyRows is how many ladder content rows fit above the strip.
func (m model) ladderBodyRows() int {
	_, w := m.ladderGeom()
	strip := m.ladderStrip(w - 2)
	return maxInt(1, m.bodyHeight()-3-len(strip)) // border (2) + title (1)
}

// ladderRows is the ladder's visible rows for the current geometry — the
// single row-layout source (ladder.Rows) evaluated at the live body height.
// Empty while the book is loading/unsettled (nothing to walk or click).
func (m model) ladderRows() []ladder.Row {
	sym := m.symbol()
	book, ok := m.store.Orderbook(sym)
	if !ok || !m.marketSettled() || !m.store.OrderbookReady(sym) {
		return nil
	}
	return ladder.Rows(m.ladderBodyRows(), book, m.store.OpenOrdersFor(m.accountSeq(), sym), m.bookGrp[sym])
}

// ladderPrices is the per-row price list the cursor walks ("" for padding and
// the mid line), top to bottom.
func (m model) ladderPrices() []string {
	rows := m.ladderRows()
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Price
	}
	return out
}

// reconcileLadderCursor keeps the ladder cursor on a live row after a book or
// own-order update churns the price levels (see ladderModel.reconcileCursor).
// Only while browsing — a confirm's price is frozen and the cursor is idle, and
// the busy view is on the wire. A no-op outside ladder mode.
func (m model) reconcileLadderCursor() model {
	if m.mode == modeLadder && m.ladder.view == ladderBrowse {
		m.ladder = m.ladder.reconcileCursor(m.ladderPrices())
	}
	return m
}

// renderLadder draws the full ladder panel: the memoized component body plus
// the sub-model's foot strip, inside the focused border.
func (m model) renderLadder(w, bodyH int) string {
	sym := m.symbol()
	inner := w - 2
	strip := m.ladderStrip(inner)
	bodyRows := maxInt(1, bodyH-3-len(strip))

	book, hasBook := m.store.Orderbook(sym)
	t, hasTicker := m.store.Ticker(sym)
	armedPrice := ""
	if m.ladder.view != ladderBrowse && m.ladder.armed.kind == armPlace && m.ladder.armed.draft.usesPrice() {
		armedPrice = m.ladder.armed.draft.price
	}
	flash := int64(0)
	if m.flashOrderID != 0 && m.cfg.Now() < m.flashUntil {
		flash = m.flashOrderID
	}
	body := m.cLadder.View(ladder.Key{
		BookRev: m.store.BookRev(), OrderRev: m.store.OrderRev(), TickerRev: m.store.TickerRev(),
		AccountSeq: m.accountSeq(),
		Symbol:     sym, Settled: m.marketSettled(), Ready: m.store.OrderbookReady(sym),
		TickerReady: m.store.TickerReady(sym), LastTick: m.store.LastTick(sym),
		CursorPrice: m.ladder.cursorPrice, ArmedPrice: armedPrice, FlashID: flash,
		Level: m.bookGrp[sym],
		W:     inner, H: bodyRows, Style: m.styleID(),
	}, ladder.Data{Book: book, HasBook: hasBook, Ticker: t, HasTicker: hasTicker, Mine: m.store.OpenOrdersFor(m.accountSeq(), sym)})

	title := m.ladder.title()
	if lvl := m.bookGrp[sym]; lvl != "" {
		// The grouping chip is the disclosure that every figure on screen —
		// the rows, the cursor price, the armed preview — reads the grouped book.
		title += " · grp " + uikit.GroupThousands(lvl)
	}
	lines := append(strings.Split(body, "\n"), strip...)
	return uikit.PanelStyled(uikit.BorderFor(true), title, lines, w, bodyH)
}

// ladderRowAt maps a screen position inside the ladder panel to the price row
// it lands on (ok=false outside the content rows or on a non-price row).
// Clicking reads the same Rows layout the render draws from.
func (m model) ladderRowAt(x, y int) (string, bool) {
	left, w := m.ladderGeom()
	if x < left || x >= left+w {
		return "", false
	}
	row := y - (m.bodyTop() + 2) // panel border + title
	if row < 0 || row >= m.ladderBodyRows() {
		return "", false
	}
	prices := m.ladderPrices()
	if row >= len(prices) || prices[row] == "" {
		return "", false
	}
	return prices[row], true
}

// ladderTitleChipAt maps a screen click on the ladder title line to a chip and
// the step it triggers: dir -1 (‹ prev) / +1 (› next) / 0 (value → focus only).
// ok=false off the title line or off any chip. Browse-only: the title chips set
// the ladder's standing defaults, so they are idle while an order is armed. The
// title sits one row below the panel's top border, its column 0 one cell inside
// the left border — the same origin orderPanelPos uses.
func (m model) ladderTitleChipAt(x, y int) (kind ladderChip, dir int, ok bool) {
	if m.ladder.view != ladderBrowse {
		return 0, 0, false
	}
	left, w := m.ladderGeom()
	if y != m.bodyTop()+1 || x < left+1 || x >= left+w-1 {
		return 0, 0, false
	}
	col := x - (left + 1)
	_, sizeSpan, tifSpan := m.ladder.titleLayout()
	for _, sp := range []ladderChipSpan{sizeSpan, tifSpan} {
		switch {
		case col == sp.prevX:
			return sp.kind, -1, true
		case col == sp.nextX:
			return sp.kind, 1, true
		case col >= sp.valX0 && col <= sp.valX1:
			return sp.kind, 0, true
		}
	}
	return 0, 0, false
}

// footerLadderView is the ladder's view for the footer hints (0 = ladder mode
// closed; 1 browse, 2 confirm place, 3 confirm cancel, 4 busy).
func (m model) footerLadderView() int {
	if m.mode != modeLadder {
		return 0
	}
	switch m.ladder.view {
	case ladderConfirm:
		if m.ladder.armed.kind == armCancel {
			return 3
		}
		return 2
	case ladderBusy:
		return 4
	}
	return 1
}
