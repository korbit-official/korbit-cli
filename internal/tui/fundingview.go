// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/tui/components/curlist"
	"github.com/korbit-official/korbit-cli/internal/tui/components/keystrip"
	"github.com/korbit-official/korbit-cli/internal/tui/components/transfers"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// The funding screen's rendering and hit-testing. The layout is master–detail
// with a persistent request pane: the currency list on the left, and on the
// right the request pane (direction chips, the form's selector/input rows, and
// the primary action button) over the transfer history, with a key strip on
// the last body row. There are NO overlays: the confirm and busy states swap
// the request pane's content in place. The pane is sized to its content and
// the history takes the rest, so only the list | detail seam is draggable.

const (
	// Per-region minimums so a resize/drag can never collapse a region.
	fundingMinSideW  = 24 // currency list width
	fundingMinRightW = 40 // detail column width (the form rows need room)
	fundingMinFormH  = 5  // request pane height (border 2 + title + 2 rows)
	fundingMinHistH  = 6  // history panel height
)

// fundingLabelW is the request pane's label column: two cells past the widest
// localized label, so values all start at the same column in either language
// (14 with the English labels).
func fundingLabelW() int {
	w := 0
	for _, l := range []string{
		i18n.T("direction"), i18n.T("network"), i18n.T("address"), i18n.T("amount"),
		i18n.T("withdrawable"), i18n.T("memo/tag"), i18n.T("available"),
	} {
		if lw := lipgloss.Width(l); lw > w {
			w = lw
		}
	}
	return w + 2
}

// geom derives the browse layout from the resizable side seam: the currency
// list column and the detail column, each held at its minimum.
func (f fundingModel) geom() (leftW, rightW int) {
	leftW = clamp(scale(f.sideDiv, f.w), fundingMinSideW, maxInt(fundingMinSideW, f.w-fundingMinRightW))
	return leftW, f.w - leftW
}

// formHeight is the request pane's height: sized to its content, clamped so
// the history below always keeps its minimum. The pane tail-truncates when the
// terminal is too short — which is why the banner renders FIRST in it.
func (f fundingModel) formHeight() int {
	_, rightW := f.geom()
	listH := f.bodyH - 1
	h := len(f.paneLines(rightW-2)) + 3 // border (2) + title (1)
	return clamp(h, fundingMinFormH, maxInt(fundingMinFormH, listH-fundingMinHistH))
}

// listVisible is the number of currency rows the list panel can show.
func (f fundingModel) listVisible() int {
	v := f.bodyH - 1 - 4 // strip; border (2) + title (1) + column header (1)
	if v < 1 {
		v = 1
	}
	return v
}

// histVisible is the number of transfer rows the history panel can show.
func (f fundingModel) histVisible() int {
	v := f.bodyH - 1 - f.formHeight() - 4
	if v < 1 {
		v = 1
	}
	return v
}

// ensureListVisible scrolls the currency window to keep row idx on screen.
func (f *fundingModel) ensureListVisible(idx, total int) {
	vis := f.listVisible()
	if vis < 1 {
		return
	}
	if idx < f.listScroll {
		f.listScroll = idx
	} else if idx >= f.listScroll+vis {
		f.listScroll = idx - vis + 1
	}
	f.listScroll = clamp(f.listScroll, 0, maxInt(0, total-vis))
}

// ensureHistVisible scrolls the history window to keep the cursor on screen.
func (f *fundingModel) ensureHistVisible() {
	vis := f.histVisible()
	if vis < 1 {
		return
	}
	if f.histCursor < f.histScroll {
		f.histScroll = f.histCursor
	} else if f.histCursor >= f.histScroll+vis {
		f.histScroll = f.histCursor - vis + 1
	}
	n := len(f.currentHistRows())
	f.histScroll = clamp(f.histScroll, 0, maxInt(0, n-vis))
}

// clampScrolls re-clamps the windows and the history cursor after a resize or
// a data change.
func (f *fundingModel) clampScrolls() {
	rows := f.currencyRows()
	f.listScroll = clamp(f.listScroll, 0, maxInt(0, len(rows)-f.listVisible()))
	n := len(f.currentHistRows())
	f.histCursor = clamp(f.histCursor, 0, maxInt(0, n-1))
	f.histScroll = clamp(f.histScroll, 0, maxInt(0, n-f.histVisible()))
}

// render draws the funding screen: the currency list, the request pane (whose
// content the confirm/busy states replace in place), and the history.
func (f fundingModel) render(style uikit.StyleID) string {
	leftW, rightW := f.geom()
	listH := f.bodyH - 1

	rows := f.currencyRows()
	selIdx := fundingSelIdx(rows, f.selected)
	if f.searching {
		// The filter moves a highlight, not the selection — the highlighted row
		// is the enter target; the detail panes keep showing f.selected.
		selIdx = fundingSelIdx(rows, f.searchSel)
	}
	crows := make([]curlist.Row, len(rows))
	for i, r := range rows {
		crows[i] = curlist.Row{
			Code: uikit.FmtCurrency(r.Cur), Avail: r.Avail, Value: r.Value,
			Held: r.Held || r.Cur == fundingKRW, Suspended: r.Suspended, Divider: r.Divider,
		}
	}
	left := f.cList.View(curlist.Key{
		Rev: f.rev, BalanceRev: f.store.BalanceRev(), TickerRev: f.store.TickerRev(),
		Ready: f.curReady, SelIdx: selIdx, Scroll: clamp(f.listScroll, 0, maxInt(0, len(rows)-f.listVisible())),
		Searching: f.searching, Query: f.query, Focused: f.focus == fundingFocusList,
		W: leftW, H: listH, Style: style,
	}, curlist.Data{Rows: crows})

	formH := f.formHeight()
	form := f.renderForm(rightW, formH)
	hist := f.renderHistory(rightW, listH-formH, style)
	right := uikit.VJoin(form, hist)

	body := uikit.HJoin(listH,
		uikit.Col{Text: left, W: leftW},
		uikit.Col{Text: right, W: rightW})
	strip, _ := keystrip.Layout(f.stripItems(), f.w)
	return body + "\n" + strip
}

// renderForm draws the request pane. The border highlights when it holds
// focus; the confirm/busy views render inside the same frame.
func (f fundingModel) renderForm(w, h int) string {
	lines := make([]string, 0, 16)
	for _, l := range f.paneLines(w - 2) {
		lines = append(lines, l.text)
	}
	focused := f.view != fundingBrowse || f.focus == fundingFocusForm
	return uikit.PanelStyled(uikit.BorderFor(focused), f.paneTitle(), lines, w, h)
}

// renderHistory draws the transfer-history panel for the selected currency+tab.
func (f fundingModel) renderHistory(w, h int, style uikit.StyleID) string {
	h = maxInt(h, 3)
	dir := i18n.T("deposits")
	if f.tab == fundingWithdraw {
		dir = i18n.T("withdrawals")
	}
	k := transfers.Key{
		Rev: f.rev, Cursor: f.histCursor, Scroll: f.histScroll,
		Focused: f.focus == fundingFocusHistory,
		W:       w, H: h, Style: style,
	}
	var d transfers.Data
	hist := f.hist[histKey{f.selected, f.tab}]
	switch {
	case hist == nil || hist.loading:
		k.Loading = true
	case hist.err != "":
		k.Err = hist.err
	default:
		d = f.historyTable(hist.rows)
		k.Count = len(hist.rows)
	}
	k.Title = i18n.T("%s · %s (%d)", dir, uikit.FmtCurrency(f.selected), k.Count)
	return f.cHist.View(k, d)
}

// historyTable projects transfer records into the table's columns: deposits
// have no fee column; KRW rows simply carry empty network/hash cells, so one
// column set per direction serves both.
func (f fundingModel) historyTable(rows []FundingTransfer) transfers.Data {
	if f.tab == fundingDeposit {
		d := transfers.Data{Cols: []string{i18n.T("time"), i18n.T("amount"), i18n.T("status"), "id"}, Weights: []int{11, 14, 12, 8}}
		for _, t := range rows {
			d.Rows = append(d.Rows, []string{
				fmtFundingTime(t.CreatedAt), uikit.GroupThousands(t.Amount), t.Status, strconv.FormatInt(t.ID, 10),
			})
		}
		return d
	}
	d := transfers.Data{Cols: []string{i18n.T("time"), i18n.T("amount"), i18n.T("fee"), i18n.T("status"), "id"}, Weights: []int{11, 13, 8, 12, 8}}
	for _, t := range rows {
		d.Rows = append(d.Rows, []string{
			fmtFundingTime(t.CreatedAt), uikit.GroupThousands(t.Amount), uikit.GroupThousands(t.Fee),
			t.Status, strconv.FormatInt(t.ID, 10),
		})
	}
	return d
}

// fmtFundingTime renders a transfer timestamp as a local month-day clock time
// (histories span days, so a bare clock would be ambiguous).
func fmtFundingTime(unixMs int64) string {
	if unixMs <= 0 {
		return ""
	}
	return time.UnixMilli(unixMs).Format("01-02 15:04")
}

// --- the request pane's content ---

// fundingSpanKind is what a click on a sub-region of a pane line does.
type fundingSpanKind int

const (
	spanStepBack    fundingSpanKind = iota // the ‹ arrow of the row's selector
	spanStepForward                        // the › arrow
	spanChipDeposit                        // the deposit direction chip
	spanChipWithdraw
	spanCopy      // the deposit address / memo text (copies via OSC 52)
	spanActivate  // the action button
	spanConfirmY  // the confirm view's [ enter confirm ]
	spanConfirmNo // the confirm view's [ esc back ]
)

// fundingSpan is one clickable sub-region of a pane line, an inclusive column
// range relative to the pane's content column 0.
type fundingSpan struct {
	x0, x1 int
	kind   fundingSpanKind
}

// fundingFormLine is one rendered line of the request pane plus its
// interactivity: the field it belongs to (ffNone for facts/banner lines) and
// any clickable sub-regions. The render and the mouse hit-test share it, so a
// row's on-screen position can never drift from its click behavior.
type fundingFormLine struct {
	text  string
	field formField
	spans []fundingSpan
}

// fact makes a non-interactive line.
func fundingFact(text string) fundingFormLine { return fundingFormLine{text: text, field: ffNone} }

// paneTitle is the request pane's title for the current view.
func (f fundingModel) paneTitle() string {
	switch f.view {
	case fundingConfirmView:
		switch f.action.kind {
		case factWithdraw:
			return i18n.T("confirm withdrawal")
		case factKRWDeposit:
			return i18n.T("confirm KRW deposit request")
		case factKRWWithdraw:
			return i18n.T("confirm KRW withdrawal request")
		case factCancel:
			return i18n.T("cancel withdrawal?")
		}
	case fundingBusy:
		return i18n.T("request in flight")
	}
	c, _ := f.currencyFor(f.selected)
	name := c.FullName
	if name == "" && f.selected == fundingKRW {
		name = i18n.T("Korean won")
	}
	title := uikit.FmtCurrency(f.selected)
	if name != "" {
		title += " · " + name
	}
	if p := f.lastPrice(f.selected); p != "" {
		title += " · " + i18n.T("last") + " " + uikit.GroupThousands(p)
	}
	return title
}

// paneLines builds the request pane's content for the current view. renderForm
// and the mouse hit-test both consume it.
func (f fundingModel) paneLines(inner int) []fundingFormLine {
	switch f.view {
	case fundingConfirmView:
		return f.confirmLines(inner)
	case fundingBusy:
		return f.busyLines()
	}
	return f.browseLines(inner)
}

// browseLines is the form: the standing banner FIRST (the pane tail-truncates
// to its height — the outcome-unknown warning is a safety signal and must
// never be the line that falls off), then the field rows for the current tab
// and selection, the action button, and any inline validation error.
func (f fundingModel) browseLines(inner int) []fundingFormLine {
	var out []fundingFormLine
	// The standing banner renders FIRST (see the doc above) — the main-only
	// warning goes after it so it can never push a safety banner off a truncated
	// pane.
	if b := f.banner; b.kind != bannerNone && b.cur == f.selected && b.tab == f.tab {
		sty := uikit.StyOK
		switch b.kind {
		case bannerWarn:
			sty = uikit.StyWarn
		case bannerErr:
			sty = uikit.StyErr
		}
		for _, l := range strings.Split(uikit.Wrap(b.text, inner), "\n") {
			out = append(out, fundingFact(sty.Render(l)))
		}
	}
	// Warn on a non-main account: deposits/withdrawals are rejected server-side
	// (the screen never retargets).
	if !f.onMainAccount() {
		msg := i18n.T("account %d — funding is main-account-only; the server rejects it.", f.accountSeq)
		for _, l := range strings.Split(uikit.Wrap(msg, inner), "\n") {
			out = append(out, fundingFact(uikit.StyWarn.Render(l)))
		}
	}

	out = append(out, f.directionLine())
	if f.selected == fundingKRW {
		out = append(out, f.krwLines(inner)...)
	} else {
		if f.tab == fundingWithdraw {
			// The withdrawable bound leads (context), then the flow reads top to
			// bottom: network → address → amount.
			out = append(out, f.withdrawableLine())
		}
		if nl, ok := f.networkLine(); ok {
			out = append(out, nl)
		}
		if f.tab == fundingWithdraw {
			out = append(out, f.withdrawLines(inner)...)
		} else {
			out = append(out, f.depositLines(inner)...)
		}
	}

	if label, ok := f.formActionLabel(); ok {
		out = append(out, fundingFormLine{}) // breathing room above the button
		out = append(out, f.buttonLine(label))
	}
	if f.formErr != "" {
		for _, l := range strings.Split(uikit.Wrap(f.formErr, inner), "\n") {
			out = append(out, fundingFact(uikit.StyErr.Render(l)))
		}
	}
	return out
}

// directionLine is the deposit ⁄ withdraw chips row: ←/→ (or a click on a
// chip) pick the direction. The active chip is always reverse-highlighted;
// with the cursor on the row the chips underline as the selection cue.
func (f fundingModel) directionLine() fundingFormLine {
	onRow := f.focus == fundingFocusForm && f.curField() == ffDirection
	chip := func(label string, on bool) string {
		s := " " + label + " "
		switch {
		case on && onRow:
			return uikit.StyFocusLbl.Underline(true).Render(s)
		case on:
			return uikit.StyFocusLbl.Render(s)
		case onRow:
			return uikit.StyKey.Render(s) // cyan underline: the other side is selectable
		default:
			return uikit.StyDim.Render(s)
		}
	}
	dep, wd := i18n.T("deposit"), i18n.T("withdraw")
	c1 := chip(dep, f.tab == fundingDeposit)
	c2 := chip(wd, f.tab == fundingWithdraw)
	x1 := fundingLabelW()
	w1 := lipgloss.Width(" " + dep + " ")
	x2 := x1 + w1 + 1
	w2 := lipgloss.Width(" " + wd + " ")
	return fundingFormLine{
		text:  uikit.PadRight(i18n.T("direction"), fundingLabelW()) + c1 + " " + c2,
		field: ffDirection,
		spans: []fundingSpan{
			{x0: x1, x1: x1 + w1 - 1, kind: spanChipDeposit},
			{x0: x2, x1: x2 + w2 - 1, kind: spanChipWithdraw},
		},
	}
}

// selectorText renders a ‹ value › (i/n) selector and its arrow spans at the
// value column; highlighted when the row is under the form cursor.
func (f fundingModel) selectorText(value string, idx, n int, onRow bool) (string, []fundingSpan) {
	s := "‹ " + value + " ›"
	backX := fundingLabelW()
	fwdX := fundingLabelW() + lipgloss.Width(s) - 1
	if onRow {
		s = uikit.StyFocusLbl.Render(s)
	}
	if n > 1 {
		s += uikit.StyDim.Render(fmt.Sprintf("  (%d/%d)", idx+1, n))
	}
	return s, []fundingSpan{
		{x0: backX, x1: backX, kind: spanStepBack},
		{x0: fwdX, x1: fwdX, kind: spanStepForward},
	}
}

// networkLine is the ‹ network › selector row (a plain fact with a single
// network; absent for fiat). Suspension of the current direction shows here.
func (f fundingModel) networkLine() (fundingFormLine, bool) {
	nets := f.selectedNetworks()
	label := uikit.PadRight(i18n.T("network"), fundingLabelW())
	if len(nets) == 0 {
		if _, known := f.currencyFor(f.selected); !known {
			return fundingFact(label + uikit.StyDim.Render(i18n.T("(catalog loading…)"))), true
		}
		return fundingFact(label + uikit.StyDim.Render(i18n.T("(none listed)"))), true
	}
	net, _ := f.selectedNetwork()
	suffix := ""
	launched, suspended := net.DepositLaunched, i18n.T("(deposits suspended)")
	if f.tab == fundingWithdraw {
		launched, suspended = net.WithdrawalLaunched, i18n.T("(withdrawals suspended)")
	}
	if !launched {
		suffix = "  " + uikit.StyErr.Render(suspended)
	}
	if len(nets) == 1 {
		return fundingFact(label + net.Name + suffix), true
	}
	onRow := f.focus == fundingFocusForm && f.curField() == ffNetwork
	sel, spans := f.selectorText(net.Name, clamp(f.netIdx, 0, len(nets)-1), len(nets), onRow)
	return fundingFormLine{text: label + sel + suffix, field: ffNetwork, spans: spans}, true
}

// withdrawLines are the crypto-withdrawal form rows: the withdrawable bound,
// the ‹ destination › selector over the network-filtered registered addresses
// (starting UNCHOSEN — a money destination is never picked implicitly), the
// picked address's memo, the amount, and the network's constraints.
func (f fundingModel) withdrawLines(inner int) []fundingFormLine {
	var out []fundingFormLine
	label := uikit.PadRight(i18n.T("address"), fundingLabelW())
	onRow := f.focus == fundingFocusForm && f.curField() == ffAddress
	addrs := f.wdAddrChoices()
	switch {
	case f.wdErr != "":
		out = append(out, fundingFormLine{
			text:  label + uikit.StyErr.Render(i18n.T("registered addresses unavailable: %s", uikit.Truncate(f.wdErr, inner-fundingLabelW()-35))),
			field: ffAddress,
		})
	case !f.wdAddrsReady:
		out = append(out, fundingFormLine{
			text:  label + uikit.StyDim.Render(i18n.T("loading registered addresses…")),
			field: ffAddress,
		})
	case len(addrs) == 0:
		// The row explains the missing registration in place, before any action.
		msg := i18n.T("none registered here — register one in the Korbit developers portal (then r to refresh)")
		wrapped := strings.Split(uikit.Wrap(msg, inner-fundingLabelW()), "\n")
		for i, l := range wrapped {
			ln := fundingFact(strings.Repeat(" ", fundingLabelW()) + uikit.StyWarn.Render(l))
			if i == 0 {
				ln = fundingFormLine{text: label + uikit.StyWarn.Render(l), field: ffAddress}
			}
			out = append(out, ln)
		}
	default:
		a, chosen := f.chosenWdAddr()
		if !chosen {
			s := i18n.T("‹ select — %d registered ›", len(addrs))
			spans := []fundingSpan{
				{x0: fundingLabelW(), x1: fundingLabelW(), kind: spanStepBack},
				{x0: fundingLabelW() + lipgloss.Width(s) - 1, x1: fundingLabelW() + lipgloss.Width(s) - 1, kind: spanStepForward},
			}
			sty := uikit.StyWarn
			if onRow {
				sty = uikit.StyFocusLbl
			}
			out = append(out, fundingFormLine{text: label + sty.Render(s), field: ffAddress, spans: spans})
		} else {
			sel, spans := f.selectorText(shortAddr(a.Address), f.wdAddrIdx, len(addrs), onRow)
			out = append(out, fundingFormLine{text: label + sel, field: ffAddress, spans: spans})
			if a.SecondaryAddress != "" {
				out = append(out, fundingFact(strings.Repeat(" ", fundingLabelW())+
					uikit.StyDim.Render(i18n.T("memo/tag")+" "+a.SecondaryAddress)))
			}
		}
	}

	out = append(out, f.amountLine())

	// The constraints the amount is checked against, visible at the point of
	// entry: from the selected network, or the picked address's network when
	// the catalog carries no network list.
	net, ok := f.selectedNetwork()
	if !ok {
		if a, chosen := f.chosenWdAddr(); chosen {
			net, ok = f.networkConstraints(a.Network)
		}
	}
	if ok {
		var ff []string
		if net.WithdrawalFee != "" {
			ff = append(ff, i18n.T("fee %s (charged in addition)", net.WithdrawalFee))
		}
		if net.WithdrawalMin != "" {
			ff = append(ff, i18n.T("min %s", net.WithdrawalMin))
		}
		if net.WithdrawalPrecision >= 0 {
			ff = append(ff, i18n.T("%d decimals", net.WithdrawalPrecision))
		}
		if len(ff) > 0 {
			for _, l := range strings.Split(uikit.Wrap(strings.Join(ff, " · "), inner-fundingLabelW()), "\n") {
				if l != "" {
					out = append(out, fundingFact(strings.Repeat(" ", fundingLabelW())+uikit.StyDim.Render(l)))
				}
			}
		}
	}
	return out
}

// withdrawableLine is the withdrawable-amount fact: what can be sent now, and
// what is locked in pending withdrawals.
func (f fundingModel) withdrawableLine() fundingFormLine {
	label := uikit.PadRight(i18n.T("withdrawable"), fundingLabelW())
	amt := f.wdAmts[f.selected]
	switch {
	case amt != nil && amt.loaded:
		return fundingFact(label + uikit.GroupThousands(amt.v.Amount) +
			uikit.StyDim.Render("  ·  "+i18n.T("pending out %s", uikit.GroupThousands(amt.v.InUse))))
	case amt != nil && amt.err != "":
		return fundingFact(uikit.StyErr.Render(i18n.T("withdrawable amount unavailable: %s", uikit.Truncate(amt.err, 50))))
	default:
		return fundingFact(uikit.StyDim.Render(label + i18n.T("loading…")))
	}
}

// depositLines are the crypto deposit rows: the assigned address (a copy
// target, with its memo), or the state that explains why there is none yet.
func (f fundingModel) depositLines(inner int) []fundingFormLine {
	var out []fundingFormLine
	label := uikit.PadRight(i18n.T("address"), fundingLabelW())
	switch {
	case f.depErr != "":
		out = append(out, fundingFact(uikit.StyErr.Render(i18n.T("addresses unavailable: %s", uikit.Truncate(f.depErr, inner-25)))))
	case !f.depAddrsReady:
		out = append(out, fundingFact(label+uikit.StyDim.Render(i18n.T("loading addresses…"))))
	case f.generating:
		out = append(out, fundingFact(label+f.spin.View()+" "+i18n.T("generating address…")))
	default:
		a, ok := f.depositAddrFor(f.selected, f.selectedNetworkName())
		if !ok {
			out = append(out, fundingFact(label+uikit.StyWarn.Render(i18n.T("none assigned yet"))))
			break
		}
		onAddr := f.focus == fundingFocusForm && f.curField() == ffAddress
		addr := a.Address
		if onAddr {
			addr = uikit.StyFocusLbl.Render(addr)
		}
		out = append(out, fundingFormLine{
			text:  label + addr,
			field: ffAddress,
			spans: []fundingSpan{{x0: fundingLabelW(), x1: fundingLabelW() + lipgloss.Width(a.Address) - 1, kind: spanCopy}},
		})
		if a.SecondaryAddress != "" {
			onMemo := f.focus == fundingFocusForm && f.curField() == ffMemo
			memo := a.SecondaryAddress
			if onMemo {
				memo = uikit.StyFocusLbl.Render(memo)
			}
			out = append(out, fundingFormLine{
				text:  uikit.PadRight(i18n.T("memo/tag"), fundingLabelW()) + memo + "  " + uikit.StyWarn.Render(i18n.T("(required — a deposit without it can be lost)")),
				field: ffMemo,
				spans: []fundingSpan{{x0: fundingLabelW(), x1: fundingLabelW() + lipgloss.Width(a.SecondaryAddress) - 1, kind: spanCopy}},
			})
		}
		out = append(out, fundingFact(strings.Repeat(" ", fundingLabelW())+
			uikit.StyDim.Render(i18n.T("enter/click copies to the clipboard (OSC 52)"))))
	}
	return out
}

// krwLines are the KRW rows: the available balance, the amount, and the
// app-push note — both directions only send an app-confirmed push.
func (f fundingModel) krwLines(inner int) []fundingFormLine {
	var out []fundingFormLine
	if b, ok := f.balanceFor(fundingKRW); ok {
		out = append(out, fundingFact(uikit.PadRight(i18n.T("available"), fundingLabelW())+uikit.GroupThousands(b.Available)+" krw"))
	}
	out = append(out, f.amountLine())
	note := i18n.T("sends a confirmation push to your Korbit app — the deposit proceeds only after you complete the verification there")
	if f.tab == fundingWithdraw {
		note = i18n.T("sends a confirmation push to your Korbit app — the withdrawal proceeds only after you complete the verification there")
	}
	for _, l := range strings.Split(uikit.Wrap(note, inner-fundingLabelW()), "\n") {
		out = append(out, fundingFact(strings.Repeat(" ", fundingLabelW())+uikit.StyDim.Render(l)))
	}
	return out
}

// amountLine is the amount input row (the pane's one text field).
func (f fundingModel) amountLine() fundingFormLine {
	return fundingFormLine{
		text:  uikit.PadRight(i18n.T("amount"), fundingLabelW()) + tiView(&f.amount),
		field: ffAmount,
	}
}

// buttonLine is the primary action button, reverse-highlighted when it is the
// field under the cursor.
func (f fundingModel) buttonLine(label string) fundingFormLine {
	btn := "[ " + label + " ]"
	sty := uikit.StyKey
	if f.focus == fundingFocusForm && f.curField() == ffButton {
		sty = uikit.StyFocusLbl
	}
	text := sty.Render(btn)
	return fundingFormLine{
		text:  text,
		field: ffButton,
		spans: []fundingSpan{{x0: 0, x1: lipgloss.Width(btn) - 1, kind: spanActivate}},
	}
}

// confirmLines is the in-pane review step before a money mover dispatches.
func (f fundingModel) confirmLines(inner int) []fundingFormLine {
	var out []fundingFormLine
	add := func(s string) { out = append(out, fundingFact(s)) }
	switch f.action.kind {
	case factWithdraw:
		o := f.action.order
		add(i18n.T("send %s %s → %s  (%s network)",
			uikit.GroupThousands(o.Amount), uikit.FmtCurrency(o.Currency), shortAddr(o.Address), o.Network))
		if o.SecondaryAddress != "" {
			add(i18n.T("memo/tag") + " " + o.SecondaryAddress)
		}
		if n, ok := f.networkConstraints(o.Network); ok && n.WithdrawalFee != "" {
			add(uikit.StyDim.Render(i18n.T("the network fee of %s %s is charged in addition", n.WithdrawalFee, uikit.FmtCurrency(o.Currency))))
		}
		add("")
		add(uikit.StyWarn.Render(i18n.T("crypto withdrawals cannot be reversed once processed")))
	case factKRWDeposit, factKRWWithdraw:
		line := i18n.T("send a deposit push for %s KRW to your Korbit app", uikit.GroupThousands(f.action.amount))
		if f.action.kind == factKRWWithdraw {
			line = i18n.T("send a withdrawal push for %s KRW to your Korbit app", uikit.GroupThousands(f.action.amount))
		}
		add(line)
		add(uikit.StyDim.Render(i18n.T("nothing moves until you complete the verification in the app")))
	case factCancel:
		line := i18n.T("withdrawal %d (%s)", f.action.cancelID, uikit.FmtCurrency(f.action.cancelCur))
		if tr, ok := f.selectedTransfer(); ok && tr.ID == f.action.cancelID {
			line = i18n.T("withdrawal %d — %s %s, %s", tr.ID, uikit.GroupThousands(tr.Amount), uikit.FmtCurrency(f.action.cancelCur), tr.Status)
		}
		add(line)
	}
	add("")
	yes := i18n.T("[ enter confirm ]")
	no := i18n.T("[ esc back ]")
	out = append(out, fundingFormLine{
		text:  uikit.StyKey.Render(yes) + "   " + uikit.StyKey.Render(no),
		field: ffNone,
		spans: []fundingSpan{
			{x0: 0, x1: lipgloss.Width(yes) - 1, kind: spanConfirmY},
			{x0: lipgloss.Width(yes) + 3, x1: lipgloss.Width(yes) + 3 + lipgloss.Width(no) - 1, kind: spanConfirmNo},
		},
	})
	return out
}

// busyLines is the in-pane state while the request is on the wire.
func (f fundingModel) busyLines() []fundingFormLine {
	text := ""
	switch f.action.kind {
	case factWithdraw:
		text = i18n.T("requesting withdrawal…")
	case factKRWDeposit:
		text = i18n.T("sending deposit push…")
	case factKRWWithdraw:
		text = i18n.T("sending withdrawal push…")
	case factCancel:
		text = i18n.T("canceling withdrawal…")
	}
	return []fundingFormLine{
		fundingFact(f.spin.View() + " " + text),
		fundingFact(uikit.StyDim.Render(i18n.T("the request is on the wire — it is never resent automatically"))),
	}
}

// --- the key strip ---

// stripItems is the funding key strip for the current view: only the keys
// live for the focused pane and the field under the cursor.
func (f fundingModel) stripItems() []keystrip.Item {
	switch f.view {
	case fundingConfirmView:
		return []keystrip.Item{
			keystrip.Prose(i18n.T("[funding]")),
			keystrip.One("enter", i18n.T("confirm")),
			keystrip.One("esc", i18n.T("back")),
		}
	case fundingBusy:
		return []keystrip.Item{
			keystrip.Prose(i18n.T("[funding]")),
			keystrip.Prose(f.spin.View() + " " + i18n.T("request on the wire — never resent automatically")),
			keystrip.One("esc", i18n.T("hide (the request continues)")),
		}
	}
	if f.searching {
		return []keystrip.Item{
			keystrip.Prose(i18n.T("filter currencies: type to filter")),
			keystrip.Multi(i18n.T("move"), keystrip.Btn("↑", "up"), keystrip.Btn("↓", "down")),
			keystrip.One("enter", i18n.T("select")),
			keystrip.One("esc", i18n.T("cancel")),
		}
	}
	items := []keystrip.Item{
		keystrip.Prose(i18n.T("[funding]")),
		keystrip.One("tab", i18n.T("focus")),
	}
	// While the amount input owns the keyboard, letters are text — don't
	// advertise caps (/, r) that would type into it if clicked.
	if !f.amountFocused() {
		items = append(items, keystrip.One("/", i18n.T("filter")), keystrip.One("r", i18n.T("refresh")))
	}
	if f.focus == fundingFocusForm {
		items = append(items, keystrip.Multi(i18n.T("field"), keystrip.Btn("↑", "up"), keystrip.Btn("↓", "down")))
		switch f.curField() {
		case ffDirection:
			items = append(items, keystrip.Multi(i18n.T("direction"), keystrip.Btn("←", "left"), keystrip.Btn("→", "right")))
		case ffNetwork:
			items = append(items, keystrip.Multi(i18n.T("network"), keystrip.Btn("←", "left"), keystrip.Btn("→", "right")))
		case ffAddress:
			if f.tab == fundingWithdraw {
				items = append(items, keystrip.Multi(i18n.T("pick address"), keystrip.Btn("←", "left"), keystrip.Btn("→", "right")))
			} else {
				items = append(items, keystrip.One("enter", i18n.T("copy address")))
			}
		case ffMemo:
			items = append(items, keystrip.One("enter", i18n.T("copy memo")))
		case ffAmount:
			items = append(items, keystrip.One("enter", i18n.T("next")))
		case ffButton:
			if label, ok := f.formActionLabel(); ok {
				items = append(items, keystrip.One("enter", label))
			}
		}
	}
	if tr, ok := f.selectedTransfer(); ok && fundingCancelable(f.tab, f.selected, tr) {
		items = append(items, keystrip.One("x", i18n.T("cancel selected")))
	}
	items = append(items, keystrip.One("esc", i18n.T("back")))
	if f.actionInFlight {
		items = append(items, keystrip.Prose(f.spin.View()+" "+i18n.T("action in flight…")))
	}
	return items
}

// stripHits is the strip's click hitmap (relative to column 0), matching the
// strip render exactly.
func (f fundingModel) stripHits() []keystrip.Hit {
	_, hits := keystrip.Layout(f.stripItems(), f.w)
	return hits
}

// shortAddr elides the middle of a long address for display; the full address
// is never edited, so the elision is safe.
func shortAddr(a string) string {
	r := []rune(a)
	if len(r) <= 24 {
		return a
	}
	return string(r[:14]) + "…" + string(r[len(r)-7:])
}

// --- mouse ---

// click maps a left click to its action. In the browse view a currency row
// selects it, a request-pane row moves the field cursor there (with the
// row's sub-regions — chips, ‹ › arrows, the copy targets, the button — doing
// what enter/←/→ would), and a history row moves the cursor. The confirm view
// takes only its two in-pane buttons; busy takes nothing. bodyTop is the first
// body row on screen; blocked is the parent's money-action gate.
func (f fundingModel) click(x, y, bodyTop int, blocked bool) (fundingModel, tea.Cmd) {
	// The currency filter owns the mouse as well as the keyboard (like the
	// main quick search): a body click while filtering must not move the real
	// selection behind the highlight-only contract. The strip caps stay live —
	// stripKeyAt handles them before this is reached.
	if f.searching {
		return f, nil
	}
	leftW, rightW := f.geom()
	inner := rightW - 2
	formH := f.formHeight()

	// The request pane's content rows (below its border and title).
	inPane := x >= leftW && y >= bodyTop+2 && y < bodyTop+formH-1
	paneIdx := y - (bodyTop + 2)
	rel := x - (leftW + 1)

	if f.view != fundingBrowse {
		if f.view == fundingConfirmView && inPane {
			lines := f.paneLines(inner)
			if paneIdx < len(lines) {
				for _, sp := range lines[paneIdx].spans {
					if rel >= sp.x0 && rel <= sp.x1 {
						switch sp.kind {
						case spanConfirmY:
							fm, cmd, _ := f.dispatchAction(blocked)
							return fm, cmd
						case spanConfirmNo:
							f.view = fundingBrowse
							return f, nil
						}
					}
				}
			}
		}
		return f, nil
	}

	if x < leftW {
		// Currency rows start below the panel border, title, and column header.
		first := bodyTop + 3
		row := y - first
		if row < 0 || row >= f.listVisible() {
			return f, nil
		}
		rows := f.currencyRows()
		idx := clamp(f.listScroll, 0, maxInt(0, len(rows)-f.listVisible())) + row
		if idx < 0 || idx >= len(rows) || rows[idx].Divider {
			return f, nil
		}
		f.focus = fundingFocusList
		f.amount.Blur()
		if rows[idx].Cur == f.selected {
			return f, nil
		}
		return f.selectCurrency(rows[idx].Cur, idx, len(rows))
	}

	if inPane {
		lines := f.paneLines(inner)
		if paneIdx >= len(lines) {
			return f, nil
		}
		line := lines[paneIdx]
		if line.field == ffNone {
			return f, nil
		}
		// The click moves the field cursor to the row, then a sub-region acts
		// exactly as the matching key would.
		f.focus = fundingFocusForm
		if i := f.fieldIndex(line.field); i >= 0 {
			f.formCursor = i
		}
		focusCmd := f.syncAmountFocus()
		for _, sp := range line.spans {
			if rel < sp.x0 || rel > sp.x1 {
				continue
			}
			switch sp.kind {
			case spanStepBack, spanStepForward:
				fm, cmd := f.adjustField(sp.kind == spanStepForward)
				return fm, tea.Batch(cmd, focusCmd)
			case spanChipDeposit:
				fm, cmd := f.switchTab(fundingDeposit)
				return fm, tea.Batch(cmd, focusCmd)
			case spanChipWithdraw:
				fm, cmd := f.switchTab(fundingWithdraw)
				return fm, tea.Batch(cmd, focusCmd)
			case spanCopy:
				return f, tea.Batch(f.copyDepositField(line.field == ffMemo), focusCmd)
			case spanActivate:
				fm, cmd := f.doAction(blocked)
				return fm, tea.Batch(cmd, focusCmd)
			}
		}
		return f, focusCmd
	}

	// History rows: below the request pane, past the history panel's border,
	// title, and column header.
	first := bodyTop + formH + 3
	row := y - first
	if row < 0 || row >= f.histVisible() {
		return f, nil
	}
	n := len(f.currentHistRows())
	idx := f.histScroll + row
	if idx < 0 || idx >= n {
		return f, nil
	}
	f.focus = fundingFocusHistory
	f.amount.Blur()
	f.histCursor = idx
	f.ensureHistVisible()
	return f, nil
}

// wheel scrolls the browse pane under the pointer. Suppressed while the
// currency filter owns the keyboard (like the main quick search) — a notch
// would scroll the filtered window out from under the highlight.
func (f *fundingModel) wheel(x, y, step, bodyTop int) {
	if f.searching {
		return
	}
	leftW, _ := f.geom()
	if x < leftW {
		rows := len(f.currencyRows())
		f.listScroll = clamp(f.listScroll+step, 0, maxInt(0, rows-f.listVisible()))
		return
	}
	if y >= bodyTop+f.formHeight() {
		n := len(f.currentHistRows())
		f.histScroll = clamp(f.histScroll+step, 0, maxInt(0, n-f.histVisible()))
	}
}

// hitDivider reports whether the point (x,y) grabs the currency-list | detail
// seam (the request pane is content-sized, so there is no row seam). The key
// strip's row is excluded.
func (f fundingModel) hitDivider(x, y, bodyTop int) dragKind {
	if y < bodyTop || y >= bodyTop+f.bodyH-1 {
		return dragNone
	}
	leftW, _ := f.geom()
	if absInt(x-leftW) <= 1 {
		return dragFundSide
	}
	return dragNone
}
