// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/tui/components/keystrip"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// The order panel's rendering: the form / confirm / busy views as shared
// line+span structures, so the render and the mouse hit-test can never drift
// apart (the funding pane's pattern).

// orderSpanKind is what a click on a sub-region of a panel line does.
type orderSpanKind int

const (
	spanOrdBack    orderSpanKind = iota // the ‹ arrow of the row's selector
	spanOrdForward                      // the › arrow
	spanOrdPreset                       // a %-of-balance chip (arg = preset index)
	spanOrdButton                       // the [ place ] button
	spanOrdConfirmY
	spanOrdConfirmBack
)

// orderSpan is one clickable sub-region of a panel line, an inclusive column
// range relative to the panel's content column 0.
type orderSpan struct {
	x0, x1 int
	kind   orderSpanKind
	arg    int
}

// orderFormLine is one rendered line plus its interactivity: the field whose
// row it is (ofNone for facts), any clickable sub-regions (‹/› arrows, chips,
// buttons), and any key-cap hits — a click on a cap presses that key (columns
// relative to content col 0, the same frame as orderSpan).
type orderFormLine struct {
	text  string
	field orderField
	spans []orderSpan
	hits  []keystrip.Hit
}

func orderFact(text string) orderFormLine { return orderFormLine{text: text, field: ofNone} }

// orderLabelW is the form's label column: one cell past the widest localized
// field label, so every value starts at the same column in either language
// (7 with the English labels).
func orderLabelW() int {
	w := 0
	for _, l := range []string{i18n.T("side"), i18n.T("type"), i18n.T("price"), i18n.T("amount"), i18n.T("qty"), "tif", "pp"} {
		if lw := lipgloss.Width(l); lw > w {
			w = lw
		}
	}
	return w + 1
}

// hintCaps builds a dim hint line of clickable key-caps and plain labels; the
// caps render in the shortcut style and each carries a click hit that presses
// its key (like the footer strip). The returned hits are relative to content
// col 0, so they hit-test in the same frame as orderSpan.
type hintCaps struct {
	b   strings.Builder
	col int
	out orderFormLine
}

// newHintCaps starts a hint line indented by indent columns (the label column
// width for the form rows, 0 for the armed view which has no label column).
func newHintCaps(indent int) *hintCaps {
	h := &hintCaps{col: indent, out: orderFormLine{field: ofNone}}
	h.b.WriteString(strings.Repeat(" ", indent))
	return h
}

// cap appends a clickable key-cap whose glyph is the key it presses.
func (h *hintCaps) cap(key string) *hintCaps {
	w := lipgloss.Width(key)
	h.out.hits = append(h.out.hits, keystrip.Hit{X: h.col, W: w, Send: keystrip.Press(key)})
	h.b.WriteString(uikit.StyKey.Render(key))
	h.col += w
	return h
}

// dim appends non-clickable label text.
func (h *hintCaps) dim(s string) *hintCaps {
	h.b.WriteString(uikit.StyDim.Render(s))
	h.col += lipgloss.Width(s)
	return h
}

func (h *hintCaps) line() orderFormLine {
	h.out.text = h.b.String()
	return h.out
}

// panelTitle names the panel for the current view, side-colored so a sell
// panel can never be mistaken for a buy panel.
func (o orderModel) panelTitle(style uikit.StyleID) string {
	pal := uikit.PaletteFor(uikit.ColorScheme(style.Scheme), style.Profile)
	side := pal.Up.Fg
	if o.draft.side == "sell" {
		side = pal.Down.Fg
	}
	label := side.Render(strings.ToUpper(o.draft.side)) + " " + uikit.FmtSymbol(o.draft.symbol)
	switch o.view {
	case orderConfirm:
		return i18n.T("confirm — %s", label)
	case orderBusy:
		return i18n.T("placing — %s", label)
	}
	return i18n.T("new order — %s", label)
}

// panelLines builds the panel content for the current view. render and the
// mouse hit-test both consume it. gate is the parent's money/freshness gate
// reason ("" = clear).
func (o orderModel) panelLines(inner int, gate string) []orderFormLine {
	switch o.view {
	case orderConfirm:
		return o.confirmOrderLines(inner, gate)
	case orderBusy:
		return o.busyOrderLines()
	}
	return o.formLines(inner, gate)
}

// selector renders a ‹ value › row with its arrow spans and label.
func (o orderModel) selector(field orderField, label, value string) orderFormLine {
	labelW := orderLabelW()
	v := "‹ " + value + " ›"
	if o.curField() == field {
		v = uikit.StyFocusLbl.Render(v)
	}
	text := uikit.PadRight(label, labelW) + v
	valW := lipgloss.Width(value)
	left := labelW
	right := labelW + 3 + valW
	return orderFormLine{text: text, field: field, spans: []orderSpan{
		{x0: left, x1: left, kind: spanOrdBack},
		{x0: right, x1: right, kind: spanOrdForward},
	}}
}

// inputRow renders a text-input row (the input carries its own cursor).
func (o orderModel) inputRow(field orderField, label, view string) orderFormLine {
	return orderFormLine{text: uikit.PadRight(label, orderLabelW()) + view, field: field}
}

// inputView is a numeric input's displayed value: the live textinput while
// the field cursor is on it (editing) or it is empty (placeholder), otherwise
// the value grouped for reading — a nine-digit price is unreadable raw. The
// draft keeps the wire string; only the display is formatted.
func (o orderModel) inputView(field orderField, ti *textinput.Model) string {
	v := strings.TrimSpace(ti.Value())
	if o.curField() == field || v == "" {
		return tiView(ti)
	}
	return uikit.GroupThousands(v)
}

// tiView is a textinput's view with the NUL padding artifact removed: the
// placeholder renderer slices the placeholder as runes but measures it in
// cells, so a wide-glyph (Hangul) placeholder leaves zero-width NUL runes in
// the output. Terminals mostly ignore them, but multiplexers and copy-paste
// don't; zero-width, so stripping never changes layout.
func tiView(ti *textinput.Model) string {
	return strings.ReplaceAll(ti.View(), "\x00", "")
}

// fitQuote renders a derived quote figure with its unit into w cells: grouped
// when it fits, compact ("9.62B KRW") when it doesn't. For the notional — the
// number a mistake hides in — an order-of-magnitude error stays visible in the
// compact form where a clipped digit string would hide it.
func fitQuote(v, ccy string, w int) string {
	unit := uikit.FmtCurrency(ccy)
	s := uikit.GroupThousands(v) + " " + unit
	if lipgloss.Width(s) <= w {
		return s
	}
	return uikit.Compact(v, maxInt(4, w-len(unit)-1)) + " " + unit
}

// formLines is the form view: the field rows with their inline hints, the
// action button with the gate/validation state, then the live preview (last,
// so a short panel tail-truncates the preview, never the controls).
func (o orderModel) formLines(inner int, gate string) []orderFormLine {
	p := o.preview()
	var out []orderFormLine

	out = append(out, o.selector(ofSide, i18n.T("side"), o.draft.side))
	out = append(out, o.selector(ofType, i18n.T("type"), o.draft.typ))

	if o.draft.usesPrice() {
		row := o.inputRow(ofPrice, i18n.T("price"), o.inputView(ofPrice, &o.price))
		if t := o.tickIndicator(); t != "" {
			row.text += "  " + t // the tick size rides the price row; the hint line is all keystrip
		}
		out = append(out, row)
		out = append(out, o.priceHintLine())
	}

	if o.draft.inputAmt() {
		out = append(out, o.inputRow(ofAmt, i18n.T("amount"), o.inputView(ofAmt, &o.amt)))
	} else {
		out = append(out, o.inputRow(ofQty, i18n.T("qty"), o.inputView(ofQty, &o.qty)))
	}
	if o.draft.typ == "limit" { // amount-vs-quantity entry is a limit-only choice
		out = append(out, o.sizeUnitLine())
	}
	out = append(out, o.presetLine())

	out = append(out, o.selector(ofTIF, "tif", o.draft.tif()))
	if o.draft.typ == "market" {
		pp := i18n.T("off")
		if o.draft.pp {
			pp = i18n.T("on")
		}
		out = append(out, o.selector(ofPP, "pp", pp+" "+i18n.T("(price protection)")))
	}

	out = append(out, orderFact(""))
	out = append(out, o.buttonLine(gate))
	if o.formErr != "" {
		for _, l := range strings.Split(uikit.Wrap(o.formErr, maxInt(10, inner)), "\n") {
			out = append(out, orderFact(uikit.StyErr.Render(l)))
		}
	}

	out = append(out, orderFact(uikit.StyDim.Render(strings.Repeat("─", maxInt(0, inner)))))
	out = append(out, o.previewLines(inner, p, gate, false)...)
	return out
}

// priceHintLine is the price row's key-cap hint (the tick size itself rides the
// price row, tickIndicator): [ ] step ±1 tick, { } step ±10, j/k walk the book,
// b/s/a/m/l set the side / cross to the touch / anchor mid/last.
func (o orderModel) priceHintLine() orderFormLine {
	h := newHintCaps(orderLabelW())
	h.cap("[").dim(" ").cap("]").dim(":±1  ").
		cap("{").dim(" ").cap("}").dim(":±10  ").
		cap("j").dim("/").cap("k").dim(":" + i18n.T("book") + "  ").
		cap("b").dim("/").cap("s").dim("/").cap("a").dim("/").cap("m").dim("/").cap("l")
	return h.line()
}

// tickIndicator is the price row's trailing tick-size figure ("tick 1,000"),
// or "tick unknown" while the policy is still loading; "" when there is no
// policy to load.
func (o orderModel) tickIndicator() string {
	if t := fmtTickHint(o.symBands(), o.draft.price); t != "" {
		return uikit.StyDim.Render(i18n.T("tick %s", uikit.GroupThousands(t)))
	}
	if o.fetchBands != nil {
		return uikit.StyDim.Render(i18n.T("tick unknown"))
	}
	return ""
}

// sizeUnitLine is the size row's u-toggle hint for a limit order; in amount mode
// it also shows the derived quantity that will be placed. The u cap presses u.
func (o orderModel) sizeUnitLine() orderFormLine {
	h := newHintCaps(orderLabelW())
	h.cap("u").dim(": ")
	if o.draft.sizeInAmt {
		h.dim(i18n.T("quantity"))
		if q := o.draft.wireQty(); q != "" {
			base, _ := splitSymbol(o.draft.symbol)
			h.dim("  ≈ " + uikit.GroupThousands(q) + " " + uikit.FmtCurrency(base))
		}
	} else {
		h.dim(i18n.T("amount"))
	}
	return h.line()
}

// presetLine renders the %-of-balance chips under the size row, led by the
// clickable % cap that cycles them (click = press %); each chip is click-to-arm.
func (o orderModel) presetLine() orderFormLine {
	labelW := orderLabelW()
	line := orderFormLine{field: ofNone}
	if len(o.sizeLevels) == 0 { // no presets configured — no chips, no cue
		return line
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", labelW))
	col := labelW
	// The % cap cycles the presets; a colon then the chips, matching "u: …" above.
	line.hits = append(line.hits, keystrip.Hit{X: col, W: lipgloss.Width(keyPreset), Send: keystrip.Press(keyPreset)})
	b.WriteString(uikit.StyKey.Render(keyPreset))
	col += lipgloss.Width(keyPreset)
	b.WriteString(uikit.StyDim.Render(": "))
	col += 2
	for i, pct := range o.sizeLevels {
		label := presetLabel(pct)
		w := lipgloss.Width(label)
		rendered := uikit.StyDim.Render(label)
		if i == o.presetIdx {
			rendered = uikit.StyFocusLbl.Render(label)
		}
		b.WriteString(rendered)
		line.spans = append(line.spans, orderSpan{x0: col, x1: col + w - 1, kind: spanOrdPreset, arg: i})
		col += w
		if i < len(o.sizeLevels)-1 {
			b.WriteString(" ")
			col++
		}
	}
	line.text = b.String()
	return line
}

// buttonLine is the [ place buy/sell ] action row; a standing gate renders it
// dimmed with the reason beside it (the click/enter still just reports the
// gate — nothing to arm against a stale book).
func (o orderModel) buttonLine(gate string) orderFormLine {
	labelW := orderLabelW()
	label := i18n.T("[ review %s order ]", o.draft.side)
	sty := uikit.StyTitle
	if gate != "" {
		sty = uikit.StyDim
	} else if o.curField() == ofButton {
		sty = uikit.StyFocusLbl
	}
	text := uikit.PadRight("", labelW) + sty.Render(label)
	w := lipgloss.Width(label)
	line := orderFormLine{text: text, field: ofButton, spans: []orderSpan{
		{x0: labelW, x1: labelW + w - 1, kind: spanOrdButton},
	}}
	if gate != "" {
		line.text += " " + uikit.StyWarn.Render("⚠")
	}
	return line
}

// previewLines renders the live analysis block: reference figures first, then
// the preplace warnings, then the book/freshness state. The notional and its
// distance from mid get a row each — the notional is the number a mistake
// hides in, so it never shares (or loses) its line to another figure. The
// confirm view prints the notional itself (as the resolved order's own line),
// so it asks for the block without it.
func (o orderModel) previewLines(inner int, p orderPreview, gate string, omitNotional bool) []orderFormLine {
	// The figure column starts one cell past the widest localized label
	// (9 with the English labels).
	notional, vsMid, feeEst, avail, estFill :=
		i18n.T("notional"), i18n.T("vs mid"), i18n.T("fee est"), i18n.T("avail"), i18n.T("est fill")
	kw := 0
	for _, k := range []string{notional, vsMid, feeEst, avail, estFill} {
		if w := lipgloss.Width(k); w+1 > kw {
			kw = w + 1
		}
	}
	kv := func(k, v string) orderFormLine {
		return orderFact(uikit.PadRight(k, kw) + v)
	}
	var out []orderFormLine
	if !p.OK {
		out = append(out, orderFact(uikit.StyDim.Render(uikit.Truncate(p.Err, inner))))
		return out
	}
	if !omitNotional && p.Notional != "" {
		out = append(out, kv(notional, fitQuote(p.Notional, p.QuoteCcy, maxInt(0, inner-kw))))
	}
	if p.PctFromMid != "" {
		out = append(out, kv(vsMid, p.PctFromMid))
	}
	if p.FeeEst != "" {
		out = append(out, kv(feeEst, "~"+uikit.GroupThousands(p.FeeEst)+" ("+p.FeeKind+")"))
	}
	if p.AvailQuote != "" || p.AvailBase != "" {
		out = append(out, kv(avail, availLine(p, maxInt(0, inner-kw))))
	}
	if p.Sim.Marketable && p.Sim.EstAvgFillPrice != "" {
		v := "~" + p.Sim.EstFilledQty + " @ avg " + uikit.GroupThousands(p.Sim.EstAvgFillPrice)
		out = append(out, kv(estFill, uikit.Truncate(v, maxInt(0, inner-kw))))
	}
	out = append(out, o.warningLines(inner, p)...)
	out = append(out, o.bookStateLine(gate))
	return out
}

// availLine renders the funding balances into w cells. The unit leads its
// figure — this line tail-truncates, and a clipped trailing unit misreads
// ("9,621,139,961 K" looks like thousands). Figures render exact when the
// line fits and compact when it doesn't (each figure gets up to 10 cells —
// room for any exact short value).
func availLine(p orderPreview, w int) string {
	build := func(quote, base string) string {
		s := ""
		if quote != "" {
			s = uikit.FmtCurrency(p.QuoteCcy) + " " + quote
		}
		if base != "" {
			if s != "" {
				s += " · "
			}
			s += uikit.FmtCurrency(p.BaseCcy) + " " + base
		}
		return s
	}
	exact := build(uikit.GroupThousands(p.AvailQuote), p.AvailBase)
	if lipgloss.Width(exact) <= w {
		return exact
	}
	return uikit.Truncate(build(uikit.Compact(p.AvailQuote, 10), uikit.Compact(p.AvailBase, 10)), w)
}

// warningLines renders the preplace warnings, each on one truncated line.
func (o orderModel) warningLines(inner int, p orderPreview) []orderFormLine {
	var out []orderFormLine
	for _, w := range p.Warnings {
		out = append(out, orderFact(warningLine(w, inner)))
	}
	return out
}

// fatalWarnCodes are the would-not-execute preplace warning classes — the
// server rejects (or kills) the order outright, or it expires with no fill —
// rendered as errors. WarnNoOpposingLiquidity belongs here for the same reason:
// the side the order would take from holds nothing and the order cannot rest, so
// nothing executes (the gate refuses it too — see fillSideRefusal).
// WarnMidPriceUnavailable deliberately does NOT: it reports that a check could
// not run, which is a caveat on the analysis, not a verdict on the order — error
// styling would claim a failure the analysis never found.
var fatalWarnCodes = map[ops.PlaceWarningCode]bool{
	ops.WarnPostOnlyWouldReject: true, ops.WarnFOKWouldKill: true, ops.WarnIOCWouldExpire: true,
	ops.WarnNotionalBelowMin: true, ops.WarnNotionalAboveMax: true, ops.WarnPriceOffTick: true,
	ops.WarnBestPegUnavailable: true, ops.WarnNoOpposingLiquidity: true,
}

// warningLine renders one preplace warning as a styled truncated line: a
// leading label, then the message (a truncated tail loses detail, never the
// label). The label localizes off the code (English renders the stable code
// itself; a language session shows a translated label) and the message is
// re-rendered from the warning's template/args in the active language — English
// (the default) reproduces w.Message exactly. Shared by every entry surface
// that gives warnings their own lines.
func warningLine(w ops.PlaceWarning, inner int) string {
	sty := uikit.StyWarn
	if fatalWarnCodes[w.Code] {
		sty = uikit.StyErr
	}
	msg := w.Message
	if w.Format != "" {
		msg = i18n.PreplaceWarnMessage(w.Format, w.Args...)
	}
	return sty.Render(uikit.Truncate("⚠ "+i18n.PreplaceWarnLabel(string(w.Code))+" — "+msg, maxInt(10, inner)))
}

// bookStateLine shows what the numbers were computed against — and, when the
// gate stands, why placing is blocked.
func (o orderModel) bookStateLine(gate string) orderFormLine {
	if gate == "" {
		return orderFact(uikit.StyOK.Render(i18n.T("book ● live")))
	}
	return orderFact(uikit.StyWarn.Render(i18n.T("book ○ %s", gate)))
}

// confirmOrderLines is the armed review: the fully resolved order, its
// preview figures, and the place/back controls. [ ] { } still nudge the price.
func (o orderModel) confirmOrderLines(inner int, gate string) []orderFormLine {
	p := o.preview()
	var out []orderFormLine

	base, quote := splitSymbol(o.draft.symbol)
	size := o.draft.sizeValue()
	unit := uikit.FmtCurrency(base)
	if o.draft.inputAmt() {
		unit = uikit.FmtCurrency(quote)
		size = uikit.GroupExact(size) // exact — see the price note below
	}
	out = append(out, orderFact(strings.ToUpper(o.draft.side)+" "+size+" "+unit))

	desc := o.draft.typ
	if o.draft.usesPrice() {
		// The review shows the authoritative wire values — the size above and
		// the price here — with GroupExact, never the fraction-capping
		// GroupThousands, so the figures on screen equal the bytes sent.
		desc += " @ " + uikit.GroupExact(o.draft.price)
	}
	desc += " · " + o.draft.tif()
	if o.draft.typ == "market" && o.draft.pp {
		desc += " · pp"
	}
	out = append(out, orderFact(desc))
	if o.draft.typ == "limit" && o.draft.sizeInAmt { // amount mode: show the derived quantity
		if q := o.draft.wireQty(); q != "" {
			out = append(out, orderFact(uikit.StyDim.Render("≈ "+uikit.GroupThousands(q)+" "+uikit.FmtCurrency(base))))
		}
	}
	if p.Notional != "" {
		// The notional is the figure enter commits to — its own line, never
		// shared. A market sell is the exception: its notional is valued against
		// the live book, so lead it with "~" (approximately) rather than "=".
		lead := "= "
		if o.draft.notionalIsEstimate() {
			lead = "~ "
		}
		out = append(out, orderFact(lead+fitQuote(p.Notional, p.QuoteCcy, maxInt(0, inner-2))))
	}
	out = append(out, orderFact(""))
	out = append(out, o.previewLines(inner, p, gate, true)...)
	if o.formErr != "" {
		out = append(out, orderFact(uikit.StyErr.Render(uikit.Truncate(o.formErr, maxInt(10, inner)))))
	}
	out = append(out, orderFact(""))

	yes := i18n.T("[ enter place ]")
	back := i18n.T("[ esc back ]")
	sty := uikit.StyTitle
	if gate != "" {
		sty = uikit.StyDim
	}
	text := sty.Render(yes) + "  " + uikit.StyDim.Render(back)
	line := orderFormLine{text: text, field: ofNone, spans: []orderSpan{
		{x0: 0, x1: lipgloss.Width(yes) - 1, kind: spanOrdConfirmY},
		{x0: lipgloss.Width(yes) + 2, x1: lipgloss.Width(yes) + 2 + lipgloss.Width(back) - 1, kind: spanOrdConfirmBack},
	}}
	out = append(out, line)
	h := newHintCaps(0)
	h.cap("[").dim(" ").cap("]").dim(":±1 · ").
		cap("{").dim(" ").cap("}").dim(":±10 " + i18n.T("ticks while armed"))
	out = append(out, h.line())
	return out
}

// busyOrderLines is the on-the-wire view.
func (o orderModel) busyOrderLines() []orderFormLine {
	desc := strings.ToUpper(o.draft.side) + " " + o.draft.sizeValue()
	if o.draft.usesPrice() {
		desc += " @ " + uikit.GroupThousands(o.draft.price)
	}
	return []orderFormLine{
		orderFact(desc),
		orderFact(""),
		orderFact(uikit.StyDim.Render(i18n.T("order on the wire — the result lands in a moment"))),
		orderFact(uikit.StyDim.Render(i18n.T("esc hides this; the outcome still arrives as a toast"))),
	}
}

// renderOrderPanel draws the bordered panel at w×h.
func (o orderModel) renderOrderPanel(w, h int, gate string, style uikit.StyleID) string {
	lines := make([]string, 0, 24)
	for _, l := range o.panelLines(w-2, gate) {
		lines = append(lines, l.text)
	}
	return uikit.PanelStyled(uikit.BorderFor(true), o.panelTitle(style), lines, w, h)
}

// orderPanelHeight is the panel's preferred outer height for a body of
// bodyH rows: its content plus the border/title chrome, leaving the
// open-orders panel below at least its minimum.
func (o orderModel) orderPanelHeight(inner, bodyH int, gate string) int {
	want := len(o.panelLines(inner, gate)) + 3
	return clamp(want, minPanelH, maxInt(minPanelH, bodyH-minPanelH))
}

// panelClick maps a click at content coordinates (row, col) — relative to the
// panel's first content line and content column 0 — to its action. inner must
// be the same content width the render used, so the hit-test rows match the
// drawn rows exactly. Returns the updated sub-model and the parent action (a
// confirm click can place).
func (o orderModel) panelClick(row, col, inner int, gate string) (orderModel, orderAction) {
	lines := o.panelLines(inner, gate)
	if row < 0 || row >= len(lines) {
		return o, orderActNone
	}
	l := lines[row]
	for _, sp := range l.spans {
		if col < sp.x0 || col > sp.x1 {
			continue
		}
		switch sp.kind {
		case spanOrdBack, spanOrdForward:
			o = o.moveCursorTo(l.field)
			return o.adjustField(sp.kind == spanOrdForward), orderActNone
		case spanOrdPreset:
			return o.applyPresetIdx(sp.arg), orderActNone
		case spanOrdButton:
			return o.arm(gate), orderActNone
		case spanOrdConfirmY:
			return o.confirmPlace(gate)
		case spanOrdConfirmBack:
			o.view = orderForm
			o.formErr = ""
			o.syncInputFocus()
			return o, orderActNone
		}
	}
	if l.field != ofNone && o.view == orderForm {
		return o.moveCursorTo(l.field), orderActNone
	}
	return o, orderActNone
}
