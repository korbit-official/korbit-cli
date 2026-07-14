// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/tui/components/balances"
	"github.com/korbit-official/korbit-cli/internal/tui/components/fills"
	"github.com/korbit-official/korbit-cli/internal/tui/components/footer"
	"github.com/korbit-official/korbit-cli/internal/tui/components/header"
	"github.com/korbit-official/korbit-cli/internal/tui/components/keystrip"
	"github.com/korbit-official/korbit-cli/internal/tui/components/notices"
	"github.com/korbit-official/korbit-cli/internal/tui/components/orderbook"
	"github.com/korbit-official/korbit-cli/internal/tui/components/orders"
	"github.com/korbit-official/korbit-cli/internal/tui/components/sidebar"
	"github.com/korbit-official/korbit-cli/internal/tui/components/trades"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

const (
	// Terminal size floors. Below the hard minimum the four-column layout
	// cannot hold its panes (columns crush against their minimums and money
	// figures clip into ambiguity), so the screen is replaced by a resize
	// notice. Between the hard and the recommended size the TUI runs normally
	// with a warning chip in the header — panels work, but long figures and
	// fact lines may truncate.
	minWidth  = 100
	minHeight = 28
	recWidth  = 120 // at and above this everything renders whole
	recHeight = 32

	// Public mode has no account right column (its notices live in the 'n'
	// popup, not a pane), so its three-column layout fits a far smaller terminal
	// than the private four-column one — it is comfortable at the classic 80×24.
	// sizeFloors selects between these and the private floors above.
	pubMinWidth  = 60
	pubMinHeight = 18
	pubRecWidth  = 80
	pubRecHeight = 24

	// Per-panel minimums so a resize drag can never collapse a column/panel.
	minSideW  = 16 // market sidebar width (fits "symbol  +12.3%")
	minColW   = 14 // orderbook / trades width
	minRightW = 18 // account column (orders/fills/balances) width — private only
	minPanelH = 4  // a right-column panel's height

	// Inline candle pane (G): the strip needs border (2) + title (1) + a usable
	// plot, and the orderbook/trades below it need real depth — so the pane only
	// shows on a tall enough body, and the seam clamps to keep both alive.
	minInlineChartH = 9  // inline chart strip minimum
	minInlineBelowH = 10 // orderbook/trades-below minimum when the chart is shown
)

func (m model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	// Capture mouse clicks/drags so panel seams are resizable; CellMotion also
	// reports motion while a button is held, which is what a drag needs. (This
	// takes over the terminal's native text selection while the TUI runs.)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m model) render() string {
	if !m.ready {
		return i18n.T("starting…")
	}
	minW, minH, recW, recH := m.sizeFloors()
	if m.w < minW || m.h < minH {
		return uikit.Wrap(i18n.T("terminal too small (%s) — korbit-cli tui needs at least %s (%s recommended); resize the terminal to continue",
			fmt.Sprintf("%dx%d", m.w, m.h), fmt.Sprintf("%dx%d", minW, minH), fmt.Sprintf("%dx%d", recW, recH)), maxInt(10, m.w-1))
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	bodyH := m.h - lipgloss.Height(header) - lipgloss.Height(footer)

	var body string
	switch m.mode {
	case modeOrder, modeLadder:
		body = m.renderBody(bodyH) // the order panel / trade ladder dock into the body, not an overlay
	case modeConfirm:
		body = m.overlay(bodyH, m.renderConfirm())
	case modeCancelAll:
		body = m.overlay(bodyH, m.renderCancelAll())
	case modeQuitConfirm:
		body = m.overlay(bodyH, m.renderQuitConfirm())
	case modeHelp:
		body = m.overlay(bodyH, m.renderHelp())
	case modeNotices:
		body = m.overlay(bodyH, m.renderNotices(bodyH))
	case modeChart:
		body = m.overlay(bodyH, m.renderChart())
	case modeAccountSwitch:
		body = m.overlay(bodyH, m.renderAccountSwitch())
	case modeFunding:
		body = m.renderFunding(bodyH)
	default:
		if m.cmdBarVisible() {
			bar, echo := m.renderCmdBarLines(m.w)
			body = m.renderBody(bodyH-2) + "\n" + bar + "\n" + echo
		} else {
			body = m.renderBody(bodyH)
		}
	}
	return header + "\n" + body + "\n" + footer
}

// overlay centers a boxed surface in the body area.
func (m model) overlay(bodyH int, content string) string {
	return uikit.Overlay(m.w, bodyH, content)
}

// renderFunding draws the funding screen (its confirm/busy states render
// inside the request pane — no overlay). The sub-model renders on a copy fed
// the live geometry, so a frame between a resize and the next setSize can
// never lay out on stale dimensions.
func (m model) renderFunding(bodyH int) string {
	f := m.funding
	f.setSize(m.w, bodyH)
	return f.render(m.styleID())
}

// renderChart draws the candle-chart overlay: a title line (symbol, interval,
// loading hint), the chart sized to most of the body, and a key-hint line. The
// chart is sized here from the live geometry rather than tracked on resize.
func (m model) renderChart() string {
	// The chart is already sized to m.chartDims() on resize (see the
	// WindowSizeMsg handler), so its scroll math matches what is shown here.
	title := m.chartTitle()
	switch {
	case m.feed.Empty() && m.chartErr != "":
		title += "  " + uikit.StyErr.Render(i18n.T("load failed: %s", uikit.Truncate(m.chartErr, 50)))
	case m.feed.Empty():
		title += "  " + m.chartSpin.View() + " " + uikit.StyDim.Render(i18n.T("loading…"))
	case m.chartLoadingOlder:
		title += "  " + m.chartSpin.View() + " " + uikit.StyDim.Render(i18n.T("loading history…"))
	}
	// Color the chart from the shared palette so its candles match the orderbook's
	// up/down (and a 'C' color-scheme toggle recolors it live). Render-time only — no
	// scroll/selection state depends on styling.
	m.chart.SetStyles(chartStyles(m.pal))
	help, _ := chartHelpLine()
	// Clamp the title and readout to the content width (plot vs. help). A selected
	// candle's readout (large OHLCV) or a "load failed: …" title can otherwise be
	// the widest line, which would make lipgloss.Place center the box on it — wider
	// than the hit-test geometry assumes, drifting every chart click. Truncating
	// here keeps the box width == chartContentW (so the geometry matches) and stable
	// as the spinner animates (the box never grows/shrinks with the title).
	cw := m.chartContentW()
	title = uikit.Truncate(uikit.StyTitle.Render(title), cw)
	return title + "\n" + uikit.Truncate(m.chartReadout(), cw) + "\n" + m.chart.View() + "\n" + help
}

// chartContentW is the chart overlay's content width: the wider of the plot and
// the help line. The title and readout are truncated to it in renderChart so no
// content line exceeds it — keeping the centered box's width equal to it (so the
// click hit-test geometry matches what is drawn) and stable as the loading
// spinner animates.
func (m model) chartContentW() int {
	cw, _ := m.chartDims()
	return maxInt(cw, chartHelpWidth())
}

// chartHelpItems is the chart overlay's key-hint as clickable tokens. The close
// key leads so a user who opened the overlay by clicking the inline pane sees how
// to get back out first; each paired command (select/scroll/interval/zoom)
// renders as two separately-clickable caps that press their own real key.
func chartHelpItems() []keystrip.Item {
	return []keystrip.Item{
		keystrip.One("esc", i18n.T("close")),
		keystrip.Multi(i18n.T("select"), keystrip.Btn("←", "left"), keystrip.Btn("→", "right")),
		keystrip.Multi(i18n.T("scroll"), keystrip.Btn("pgup", "pgup"), keystrip.Btn("pgdn", "pgdown")),
		keystrip.Multi(i18n.T("interval"), keystrip.Btn("[", "["), keystrip.Btn("]", "]")),
		keystrip.Multi(i18n.T("zoom"), keystrip.Btn("+", "+"), keystrip.Btn("-", "-")),
		keystrip.One("g", i18n.T("live")),
		keystrip.One("v", i18n.T("vol")),
		keystrip.One("i", i18n.T("ind")),
		keystrip.One("C", i18n.T("color")),
	}
}

// chartHelpLine renders the chart hint line and its hitmap (relative to the
// line's first column). It is not truncated: the help line participates in the
// overlay's content width, so the box is sized to fit it (it can be the widest
// content line, and so it decides the centered box's left edge).
func chartHelpLine() (string, []keystrip.Hit) {
	return keystrip.Layout(chartHelpItems(), 1<<20)
}

// chartHelpWidth is the display width of the rendered chart hint line.
func chartHelpWidth() int {
	line, _ := chartHelpLine()
	return lipgloss.Width(line)
}

// chartScreenOrigin is the top-left screen cell of the chart component inside the
// centered overlay box. The box fills the body height (its content is exactly the
// body's height), so its top is the body's first row; it is centered horizontally
// on its widest content line (the chart plot or the help line — the title and
// readout are truncated to that width in renderChart, so they never widen the
// box). The chart sits below the top border plus the title and readout rows.
func (m model) chartScreenOrigin() (col, row int) {
	contentW := m.chartContentW()
	boxLeft := (m.w - (contentW + 2)) / 2
	if boxLeft < 0 {
		boxLeft = 0
	}
	return boxLeft + 1, m.bodyTop() + 3 // +1 left border; +1 top border + title + readout
}

// chartHelpScreen is the top-left screen cell of the chart hint line: the same
// inner-left column as the plot (the content block is left-aligned in the box),
// one row below the plot's last row. Used to hit-test clicks on the hint caps.
func (m model) chartHelpScreen() (col, row int) {
	col, plotRow := m.chartScreenOrigin()
	_, ch := m.chartDims()
	return col, plotRow + ch
}

// selectChartAt maps an overlay mouse click to a candle selection, a no-op
// outside the chart's rows or left of its plot (the gutter/past-the-candles cases
// are dropped inside SelectAtX). Overlay-only.
func (m *model) selectChartAt(x, y int) {
	col, row := m.chartScreenOrigin()
	_, ch := m.chartDims()
	if y < row || y >= row+ch || x < col {
		return
	}
	m.chart.SelectAtX(x - col)
}

// chartOverlayBounds is the inclusive screen rect of the bordered overlay box,
// derived from the plot origin/size (the same geometry renderChart uses). The
// border cells ARE the rect edges, so a click on the frame counts as inside; only
// a click strictly outside the rect dismisses the overlay.
func (m model) chartOverlayBounds() (left, top, right, bottom int) {
	col, row := m.chartScreenOrigin()
	_, ch := m.chartDims()
	contentW := m.chartContentW()
	left = col - 1              // plot col is 1 past the left border
	top = row - 3               // plot row is below border + title + readout
	right = left + contentW + 1 // contentW plus the two border columns
	bottom = top + ch + 4       // border+title+readout+plot(ch)+help+border
	return
}

// inChartOverlay reports whether (x,y) lands on the overlay box (frame included).
func (m model) inChartOverlay(x, y int) bool {
	left, top, right, bottom := m.chartOverlayBounds()
	return x >= left && x <= right && y >= top && y <= bottom
}

// centeredOverlayBounds is the inclusive screen rect of a StyBorder box holding
// content centered by uikit.Overlay (lipgloss.Place over the body area). The
// border cells ARE the rect edges, so a click on the frame counts as inside; only
// a click strictly outside dismisses the overlay. Backs the click-outside-to-close
// gesture for the help/notices overlays and the modal dialogs, the same as the chart.
//
// This is the SINGLE source of the centered-overlay box geometry: overlayLineOrigin
// derives the clickable hint/body origins from it, so the close hit-test and the
// hitmaps can never drift out of sync. Any change to how uikit.Overlay centers a box
// must be reflected here and only here.
func (m model) centeredOverlayBounds(content string) (left, top, right, bottom int) {
	boxW := lipgloss.Width(content) + 2  // StyBorder adds one column each side
	boxH := lipgloss.Height(content) + 2 // and one row top and bottom
	left = maxInt((m.w-boxW)/2, 0)
	top = m.bodyTop() + maxInt((m.bodyHeight()-boxH)/2, 0)
	right = left + boxW - 1
	bottom = top + boxH - 1
	return
}

// inHelpOverlay reports whether (x,y) lands on the help box (frame included).
func (m model) inHelpOverlay(x, y int) bool {
	left, top, right, bottom := m.centeredOverlayBounds(m.renderHelp())
	return x >= left && x <= right && y >= top && y <= bottom
}

// inNoticesOverlay reports whether (x,y) lands on the notices box (frame included).
func (m model) inNoticesOverlay(x, y int) bool {
	left, top, right, bottom := m.centeredOverlayBounds(m.renderNotices(m.bodyHeight()))
	return x >= left && x <= right && y >= top && y <= bottom
}

// inActiveOverlay reports whether (x,y) lands on the currently open centered
// overlay's box (help or notices); false for any other mode.
func (m model) inActiveOverlay(x, y int) bool {
	switch m.mode {
	case modeHelp:
		return m.inHelpOverlay(x, y)
	case modeNotices:
		return m.inNoticesOverlay(x, y)
	}
	return false
}

// inlineChartAt reports whether (x,y) lands on the inline candle strip, used to
// open the full overlay on a click. False unless the strip is actually showing.
func (m model) inlineChartAt(x, y int) bool {
	if !m.inlineChartVisible() {
		return false
	}
	sideW, bookW, tradesW, _ := m.colWidths()
	rightStart := sideW + bookW + tradesW
	top, bodyH := m.bodyTop(), m.bodyHeight()
	return x >= sideW && x < rightStart && y >= top && y < top+m.centerChartH(bodyH)
}

// --- header / footer ---

func (m model) renderHeader() string {
	sym := m.symbol()
	t, ok := m.store.Ticker(sym)
	return m.cHeader.View(m.headerKey(), header.Data{Ticker: t, HasTicker: ok, TickerReady: m.store.TickerReady(sym)})
}

// headerKey builds the header component's cache key (the single source both the
// header render and the acct-chip hit-test read). The acct chip shows only in
// private mode.
func (m model) headerKey() header.Key {
	acctSeq := 0
	if m.cfg.Private {
		acctSeq = m.accountSeq()
	}
	return header.Key{
		TickerRev:    m.store.TickerRev(),
		Symbol:       m.symbol(),
		MarketCount:  len(m.cfg.Symbols),
		KeyName:      m.cfg.KeyName,
		BaseURL:      m.cfg.BaseURL,
		AccountSeq:   acctSeq,
		AccountCount: len(m.accountSeqs),
		Cramped:      m.crampedNote(),
		W:            m.w,
		Style:        m.styleID(),
	}
}

// acctChipAt reports whether (x,y) lands on the header's "accountSeq:" chip (the first
// header row), so a click there opens the sub-account switcher from the main
// layout. The chip's column span is the header component's single source, shared
// with its render.
func (m model) acctChipAt(x, y int) bool {
	if y != 0 {
		return false
	}
	start, w, ok := header.AcctChipSpan(m.headerKey())
	return ok && x >= start && x < start+w
}

// sizeFloors returns the hard minimum and recommended terminal size for the
// current session: below the minimum render() replaces the screen with a resize
// notice, and between the two crampedNote warns. Public mode drops the account
// right column, so its three-column layout fits a smaller terminal.
func (m model) sizeFloors() (minW, minH, recW, recH int) {
	if m.rightPaneVisible() {
		return minWidth, minHeight, recWidth, recHeight
	}
	return pubMinWidth, pubMinHeight, pubRecWidth, pubRecHeight
}

// crampedNote is the header's size warning for a terminal between the hard
// minimum and the recommended size ("" when the size is comfortable): the TUI
// keeps working, but long figures and fact lines may truncate until a resize.
func (m model) crampedNote() string {
	_, _, recW, recH := m.sizeFloors()
	if m.w >= recW && m.h >= recH {
		return ""
	}
	return fmt.Sprintf("⚠ %dx%d — best ≥%dx%d", m.w, m.h, recW, recH)
}

// footerKey builds the footer component's cache key from the current model — the
// single source for both the footer render and its click hit-testing, so the
// clickable regions always match the line on screen.
func (m model) footerKey() footer.Key {
	return footer.Key{
		HealthRev:      m.store.HealthRev(),
		NoticeRev:      m.store.NoticeRev(),
		Private:        m.cfg.Private,
		HasTrader:      m.cfg.Trader != nil,
		Focus:          int(m.focus),
		OrdersAllPairs: m.ordersAllPairs,
		OrdersClosed:   m.ordersClosed,
		Searching:      m.searching,
		SearchPane:     int(m.searchPane),
		CandlesWired:   m.cfg.Candles != nil,
		FundingWired:   m.cfg.Funding != nil,
		MultiAccount:   m.multiAccount(),
		OverlayOpen:    m.mode != modeNormal && m.mode != modeOrder && m.mode != modeLadder,
		OrderView:      m.footerOrderView(),
		LadderView:     m.footerLadderView(),
		SizeKeys:       sizeKeysLabel(m.ladder.sizeLevels),
		CmdBarOpen:     m.cmdBarVisible(),
		ToastText:      m.toast.text,
		ToastUntil:     m.toast.until,
		ToastIsError:   m.toast.isError,
		NowSec:         m.cfg.Now() / 1000,
		W:              m.w,
		Style:          m.styleID(),
	}
}

// footerOrderView is the order panel's view for the footer hints (0 = order
// mode closed; 1/2/3 = form/confirm/busy).
func (m model) footerOrderView() int {
	if m.mode != modeOrder {
		return 0
	}
	return int(m.order.view) + 1
}

func (m model) renderFooter() string {
	k := m.footerKey()
	d := footer.Data{Health: m.store.Health()}
	if n := m.store.Notices(1); len(n) > 0 {
		d.HasNotice, d.NoticeCode, d.NoticeMsg, d.NoticeLvl = true, n[0].Code, n[0].Message, n[0].Level
	}
	return m.cFooter.View(k, d)
}

// --- body ---

// rightPaneVisible reports whether the body has a fourth (right) column — the
// account panels (orders/fills/balances). Only private mode does; public mode
// is three columns (sidebar | orderbook | trades) with its notices reached
// through the 'n' popup, not a pane.
func (m model) rightPaneVisible() bool { return m.cfg.Private }

// colWidths derives the body column widths from the resizable seam fractions,
// holding each column at its minimum so a drag can't collapse one. The seams are
// the sidebar|orderbook boundary (sideDiv), the orderbook|trades boundary
// (colDivA), and (private only) the trades|right boundary (colDivB); the last
// column takes the remainder. Public mode has no right column, so rightW is 0,
// colDivB is unused, and the trades column runs to the edge.
func (m model) colWidths() (sideW, bookW, tradesW, rightW int) {
	w := m.w
	if !m.rightPaneVisible() {
		sideW = clamp(scale(m.sideDiv, w), minSideW, w-2*minColW)
		tradesStart := clamp(scale(m.colDivA, w), sideW+minColW, w-minColW)
		bookW = tradesStart - sideW
		tradesW = w - tradesStart
		return
	}
	sideW = clamp(scale(m.sideDiv, w), minSideW, w-2*minColW-minRightW)
	tradesStart := clamp(scale(m.colDivA, w), sideW+minColW, w-minColW-minRightW)
	rightStart := clamp(scale(m.colDivB, w), tradesStart+minColW, w-minRightW)
	bookW = tradesStart - sideW
	tradesW = rightStart - tradesStart
	rightW = w - rightStart
	return
}

// rightHeights splits the private right column into orders/fills/balances from
// the resizable seam fractions (of the body height), each at least minPanelH.
func (m model) rightHeights(bodyH int) (orders, fills, bals int) {
	orders = clamp(scale(m.rowDivA, bodyH), minPanelH, bodyH-2*minPanelH)
	fillsBottom := clamp(scale(m.rowDivB, bodyH), orders+minPanelH, bodyH-minPanelH)
	fills = fillsBottom - orders
	bals = bodyH - fillsBottom
	return
}

// bodyTop is the first body row (the header is two lines); bodyHeight matches
// the bodyH used by render()/layout() (full height minus header+footer, minus
// the command bar's two lines while it is open).
func (m model) bodyTop() int { return 2 }
func (m model) bodyHeight() int {
	h := m.h - 4
	if m.cmdBarVisible() {
		h -= 2
	}
	return h
}

// cmdBarVisible reports whether the command bar owns the two rows above the
// footer (normal view only — an overlay or another mode suspends it).
func (m model) cmdBarVisible() bool { return m.mode == modeNormal && m.cmdbar.active }

// hitDivider reports which resizable seam (if any) the point (x,y) grabs. The
// vertical column seams win at their exact x even inside the right column, so
// a right-column horizontal seam is only grabbed away from the left edge.
func (m model) hitDivider(x, y int) dragKind {
	top, bodyH := m.bodyTop(), m.bodyHeight()
	if y < top || y >= top+bodyH {
		return dragNone
	}
	sideW, bookW, tradesW, rightW := m.colWidths()
	bookEnd := sideW + bookW
	rightStart := bookEnd + tradesW
	switch {
	case absInt(x-sideW) <= 1:
		return dragColS
	case absInt(x-bookEnd) <= 1:
		return dragColA
	case m.rightPaneVisible() && absInt(x-rightStart) <= 1:
		// The trades|right seam exists only when the right column does; in public
		// mode rightStart sits at the screen edge and must not grab a drag.
		return dragColB
	}
	if m.cfg.Private && x >= rightStart && x < rightStart+rightW {
		oh, fh, _ := m.rightHeights(bodyH)
		switch {
		case absInt(y-(top+oh)) <= 1:
			return dragRowA
		case absInt(y-(top+oh+fh)) <= 1:
			return dragRowB
		}
	}
	// The inline chart strip | orderbook/trades seam, spanning the center columns.
	if m.inlineChartVisible() && x >= sideW && x < rightStart {
		if absInt(y-(top+m.centerChartH(bodyH))) <= 1 {
			return dragRowChart
		}
	}
	return dragNone
}

// paneAt maps a click to the focusable pane it lands on, reporting ok=false when
// it lands on nothing focusable so the caller leaves focus untouched. Only three
// panes take focus: the market sidebar, and (private mode) the open-orders and
// balances sub-panels. A click on the header/footer, the orderbook/trades center,
// the fills panel, or any gap is a no-op — it must not steal focus.
func (m model) paneAt(x, y int) (focusPane, bool) {
	top, bodyH := m.bodyTop(), m.bodyHeight()
	if y < top || y >= top+bodyH {
		return 0, false // header, footer, or outside the body
	}
	sideW, bookW, tradesW, _ := m.colWidths()
	if x < sideW {
		return focusMarket, true // the market sidebar
	}
	if m.cfg.Private {
		rightStart := sideW + bookW + tradesW
		if x >= rightStart {
			oh, fh, _ := m.rightHeights(bodyH)
			switch {
			case y < top+oh:
				return focusOrders, true
			case y >= top+oh+fh:
				return focusBalances, true
			}
		}
	}
	return 0, false // orderbook/trades center, fills panel, or a gap
}

// applyDrag moves the seam currently being dragged to follow the pointer,
// clamped so every column/panel keeps its minimum, then re-fits the table.
func (m *model) applyDrag(x, y int) {
	bodyH := m.bodyHeight()
	top := m.bodyTop()
	switch m.drag {
	case dragColS:
		// sidebar | orderbook: leave room for orderbook + trades, plus the right
		// column's minimum in private mode (public mode has no right column, so
		// reserving it would wrongly cap — and at the public floor pin — the sidebar).
		hi := m.w - 2*minColW
		if m.rightPaneVisible() {
			hi -= minRightW
		}
		ns := clamp(x, minSideW, hi)
		m.sideDiv = float64(ns) / float64(m.w)
	case dragColA:
		sideW, _, _, rightW := m.colWidths()
		rightStart := m.w - rightW
		nb := clamp(x, sideW+minColW, rightStart-minColW)
		m.colDivA = float64(nb) / float64(m.w)
	case dragColB:
		sideW, bookW, _, _ := m.colWidths()
		nr := clamp(x, sideW+bookW+minColW, m.w-minRightW)
		m.colDivB = float64(nr) / float64(m.w)
	case dragRowA:
		oh, fh, _ := m.rightHeights(bodyH)
		no := clamp(y-top, minPanelH, (oh+fh)-minPanelH)
		m.rowDivA = float64(no) / float64(bodyH)
	case dragRowB:
		oh, _, _ := m.rightHeights(bodyH)
		nf := clamp(y-top, oh+minPanelH, bodyH-minPanelH)
		m.rowDivB = float64(nf) / float64(bodyH)
	case dragRowChart:
		nh := clamp(y-top, minInlineChartH, bodyH-minInlineBelowH)
		m.chartDiv = float64(nh) / float64(bodyH)
	case dragFundSide:
		ns := clamp(x, fundingMinSideW, maxInt(fundingMinSideW, m.w-fundingMinRightW))
		m.funding.sideDiv = float64(ns) / float64(m.w)
		m.funding.clampScrolls()
	}
	m.layout()
}

func (m model) renderBody(bodyH int) string {
	sideW, bookW, tradesW, rightW := m.colWidths()
	side := m.viewSidebar(sideW, bodyH)
	var center string
	if m.mode == modeLadder {
		// Ladder mode rebuilds the center region as the full-height trade
		// ladder (the inline chart yields); the flanking columns stay.
		center = m.renderLadder(bookW+tradesW, bodyH)
	} else {
		center = m.renderCenter(bookW, tradesW, bodyH)
	}

	cols := []uikit.Col{
		{Text: side, W: sideW},
		{Text: center, W: bookW + tradesW},
	}
	// The account right column exists in private mode only (public mode's
	// notices are reached through the 'n' popup, not a pane).
	if m.rightPaneVisible() {
		var right string
		switch {
		case m.mode == modeOrder:
			// Order mode docks the entry panel over the right column, keeping the
			// open-orders slice beneath it — the accepted order lands (and flashes)
			// right below where it was placed. Fills/balances yield for the mode
			// (the panel's preview shows the balances that matter to the order).
			panelH := m.order.orderPanelHeight(rightW-2, bodyH, m.orderGate())
			right = uikit.VJoin(
				m.order.renderOrderPanel(rightW, panelH, m.orderGate(), m.styleID()),
				m.viewOrders(rightW, bodyH-panelH))
		default:
			oh, fh, bh := m.rightHeights(bodyH)
			right = uikit.VJoin(
				m.viewOrders(rightW, oh),
				m.viewFills(rightW, fh),
				m.viewBalances(rightW, bh))
		}
		cols = append(cols, uikit.Col{Text: right, W: rightW})
	}
	return uikit.HJoin(bodyH, cols...)
}

// viewSidebar/viewOrderbook/… build each pane component's Key (store revisions +
// geometry + style + this pane's UI scalars) and Data (the store slices it
// renders), then delegate to the cached component. The Data is built every frame
// but read only on a cache miss; the revisions in the Key decide the hit.

func (m model) viewSidebar(w, h int) string {
	rows := make([]sidebar.Row, len(m.cfg.Symbols))
	for i, s := range m.cfg.Symbols {
		t, _ := m.store.Ticker(s)
		rows[i] = sidebar.Row{Symbol: s, PriceChangePercent: t.PriceChangePercent, PriceChange: t.PriceChange, Ready: m.store.TickerReady(s)}
	}
	searching := m.searching && m.searchPane == focusMarket
	q := ""
	if searching {
		q = m.searchQuery
	}
	return m.cSidebar.View(sidebar.Key{
		TickerRev: m.store.TickerRev(), Active: m.symbol(), Scroll: m.scroll,
		Searching: searching, Query: q, SearchCursor: m.searchCursor, SearchScroll: m.searchScroll,
		Focused: m.focus == focusMarket, W: w, H: h, Style: m.styleID(),
	}, sidebar.Data{Rows: rows})
}

func (m model) viewOrderbook(w, h int) string {
	sym := m.symbol()
	book, ok := m.store.Orderbook(sym)
	t, tok := m.store.Ticker(sym)
	cursor := ""
	if m.mode == modeOrder && m.order.view == orderForm && m.order.draft.usesPrice() {
		cursor = m.order.cursorPrice // a market order has no price to mark
	}
	return m.cOrderbook.View(orderbook.Key{
		BookRev: m.store.BookRev(), TickerRev: m.store.TickerRev(), Symbol: sym,
		Settled: m.marketSettled(), Ready: m.store.OrderbookReady(sym), TickerReady: m.store.TickerReady(sym),
		LastTick:    m.store.LastTick(sym),
		CursorPrice: cursor,
		Level:       m.bookGrp[sym],
		W:           w, H: h, Style: m.styleID(),
	}, orderbook.Data{Book: book, HasBook: ok, Ticker: t, HasTicker: tok})
}

func (m model) viewTrades(w, h int) string {
	sym := m.symbol()
	return m.cTrades.View(trades.Key{
		TradeRev: m.store.TradeRev(), Symbol: sym, Settled: m.marketSettled(),
		Ready: m.store.TradesReady(sym), W: w, H: h, Style: m.styleID(),
	}, trades.Data{Trades: m.store.Trades(sym)})
}

func (m model) viewOrders(w, h int) string {
	flash := int64(0)
	// The flash marks a just-ACCEPTED order; on the closed tab the same id can
	// only mean the order already died, so a green flash there would lie.
	if !m.ordersClosed && m.flashOrderID != 0 && m.cfg.Now() < m.flashUntil {
		flash = m.flashOrderID
	}
	cursor := m.orderCursor
	if m.mode == modeOrder {
		cursor = -1 // display-only under the order panel: no cancel target
	}
	return m.cOrders.View(orders.Key{
		RowsRev: m.ordersRev, Cursor: cursor, Scroll: m.orderScroll,
		AllPairs: m.ordersAllPairs, Focused: m.focus == focusOrders && m.mode != modeOrder,
		Loading: !m.ordersClosed && m.ordersLoading(), Count: len(m.orderRows), Symbol: m.symbol(),
		FlashID: flash, Closed: m.ordersClosed,
		W: w, H: h, Style: m.styleID(),
	}, orders.Data{Rows: m.orderRows, OrderIDs: m.orderIDs, Canceling: m.orderCanceling})
}

func (m model) viewFills(w, h int) string {
	return m.cFills.View(fills.Key{
		FillRev: m.store.FillRev(), HealthRev: m.store.HealthRev(), AccountSeq: m.accountSeq(),
		PrivateUp: m.store.Health().Private.Up, W: w, H: h, Style: m.styleID(),
	}, fills.Data{Fills: m.store.FillsFor(m.accountSeq())})
}

func (m model) viewBalances(w, h int) string {
	searching := m.searching && m.searchPane == focusBalances
	q := ""
	if searching {
		q = m.searchQuery
	}
	return m.cBalances.View(balances.Key{
		BalanceRev: m.store.BalanceRev(), AccountSeq: m.accountSeq(), Ready: m.store.BalancesReady(m.accountSeq()), Scroll: m.balScroll,
		Searching: searching, Query: q, SearchScroll: m.searchScroll, Focused: m.focus == focusBalances,
		W: w, H: h, Style: m.styleID(),
	}, balances.Data{Balances: m.store.BalancesFor(m.accountSeq())})
}

// renderCenter builds the orderbook+trades center region. Normally the two
// columns are full height side by side; while the inline candle pane is shown it
// sits on top, with the orderbook and trades stacked beneath it (same combined
// width), split by the chartDiv seam.
func (m model) renderCenter(bookW, tradesW, bodyH int) string {
	if !m.inlineChartVisible() {
		return uikit.HJoin(bodyH,
			uikit.Col{Text: m.viewOrderbook(bookW, bodyH), W: bookW},
			uikit.Col{Text: m.viewTrades(tradesW, bodyH), W: tradesW})
	}
	chartH := m.centerChartH(bodyH)
	belowH := bodyH - chartH
	chartPane := m.renderInlineChart(bookW+tradesW, chartH)
	below := uikit.HJoin(belowH,
		uikit.Col{Text: m.viewOrderbook(bookW, belowH), W: bookW},
		uikit.Col{Text: m.viewTrades(tradesW, belowH), W: tradesW})
	return uikit.VJoin(chartPane, below)
}

// inlineChartFits reports whether the body is tall enough to show the inline
// chart without starving the orderbook/trades beneath it.
func (m model) inlineChartFits() bool {
	return m.bodyHeight() >= minInlineChartH+minInlineBelowH
}

// inlineChartVisible reports whether the inline candle pane should render: the
// preference is on, a candles seam is wired, and the terminal is tall enough.
func (m model) inlineChartVisible() bool {
	return m.chartInline && m.cfg.Candles != nil && m.inlineChartFits()
}

// centerChartH is the inline chart strip's height from the chartDiv seam, held
// so the chart and the orderbook/trades beneath it each keep their minimum.
func (m model) centerChartH(bodyH int) int {
	return clamp(scale(m.chartDiv, bodyH), minInlineChartH, bodyH-minInlineBelowH)
}

// renderInlineChart draws the compact, display-only candle pane. It value-copies
// the stored chart model so it inherits every setting (interval, zoom, volume
// toggle, time axis, indicator) and its current candle + overlay data, then
// overrides only what makes it a glance: the inline size, pinned to the live
// edge, with no selection.
//
// It does not re-feed candles. The stored chart is held current with the feed by
// the Update path (foldChartTrades/applyCandles call SetCandles on every feed
// change), and the copy reads that shared candle/overlay backing read-only —
// SetSize, ScrollToLive, ClearSelection, and View never mutate it. So each frame
// avoids a candle copy (feed.Series + SetCandles) and any indicator recompute;
// it reuses the overlays the stored chart already computed. The colors come from
// the shared palette, so a 'C' color-scheme toggle recolors it live.
func (m model) renderInlineChart(w, h int) string {
	title := m.chartTitle()
	var lines []string
	if m.feed.Empty() {
		if m.chartErr != "" {
			title += "  " + uikit.StyErr.Render(i18n.T("load failed"))
		} else {
			title += "  " + uikit.StyDim.Render(i18n.T("loading…"))
		}
	} else {
		c := m.chart // value copy: shares the stored chart's candle + overlay data
		c.SetStyles(chartStyles(m.pal))
		c.SetSize(w-2, h-3) // inside the panel border (2) and title (1)
		c.ScrollToLive()    // always the current position — the inline pane never scrolls
		c.ClearSelection()  // display-only: no cursor
		lines = strings.Split(c.View(), "\n")
	}
	return uikit.Panel(title, lines, w, h)
}

// sidebarStart is the first visible symbol index: the wheel scroll offset,
// clamped so a resize or a shrunk list can't leave the window past the end.
func (m model) sidebarStart(visible int) int {
	n := len(m.cfg.Symbols)
	if n <= visible || visible <= 0 {
		return 0
	}
	return clamp(m.scroll, 0, n-visible)
}

// sidebarRowAt maps a click in the sidebar to the symbol index it lands on
// (false when the click is outside the sidebar's content rows). The sidebar
// panel reserves its top border (1) and title (1) before the content rows.
func (m model) sidebarRowAt(x, y int) (int, bool) {
	sideW, _, _, _ := m.colWidths()
	if x < 0 || x >= sideW {
		return 0, false
	}
	visible := m.bodyHeight() - 3
	row := y - (m.bodyTop() + 2)
	if row < 0 || row >= visible {
		return 0, false
	}
	idx := m.sidebarStart(visible) + row
	if idx < 0 || idx >= len(m.cfg.Symbols) {
		return 0, false
	}
	return idx, true
}

// ordersPaneTop is the orders pane's top screen row. It is the body top in the
// normal and ladder layouts, but in order mode the entry panel docks above the
// pane, pushing it down by the panel's height.
func (m model) ordersPaneTop() int {
	top := m.bodyTop()
	if m.mode == modeOrder {
		_, _, _, rightW := m.colWidths()
		top += m.order.orderPanelHeight(rightW-2, m.bodyHeight(), m.orderGate())
	}
	return top
}

// ordersTitleTab maps a click onto the orders pane's title tab switcher: which
// tab a click at (x, y) selects, ok=false off the two segments (the title text
// renders one row below the pane's top border). The mouse analogue of 'o'; the
// segment geometry itself lives with the title in the orders component.
func (m model) ordersTitleTab(x, y int) (toClosed, ok bool) {
	if !m.cfg.Private {
		return false, false
	}
	sideW, bookW, tradesW, rightW := m.colWidths()
	rightStart := sideW + bookW + tradesW
	if y != m.ordersPaneTop()+1 || x < rightStart+1 || x >= rightStart+rightW-1 {
		return false, false
	}
	return orders.TitleTab(x-(rightStart+1), m.ordersClosed)
}

// ordersRowAt maps a click in the orders panel to the order row index it
// lands on (false when outside the data rows). The panel reserves its top border
// (1), title (1), and column header (1) before the data rows; the bottom border
// row is excluded. Private mode only.
func (m model) ordersRowAt(x, y int) (int, bool) {
	if !m.cfg.Private {
		return 0, false
	}
	sideW, bookW, tradesW, rightW := m.colWidths()
	rightStart := sideW + bookW + tradesW
	if x < rightStart || x >= rightStart+rightW {
		return 0, false
	}
	top, bodyH := m.bodyTop(), m.bodyHeight()
	oh, _, _ := m.rightHeights(bodyH)
	first := top + 3                // first data row (border + title + column header)
	if y < first || y >= top+oh-1 { // exclude the bottom border row
		return 0, false
	}
	idx := m.orderScrollClamped() + (y - first)
	if idx < 0 || idx >= len(m.orderIDs) {
		return 0, false
	}
	return idx, true
}

// --- order-mode geometry ---

// bookPaneGeom is the orderbook pane's top screen row and outer height for the
// current layout (the pane sits below the inline chart strip when it shows).
func (m model) bookPaneGeom() (top, h int) {
	top, bodyH := m.bodyTop(), m.bodyHeight()
	if m.inlineChartVisible() {
		ch := m.centerChartH(bodyH)
		return top + ch, bodyH - ch
	}
	return top, bodyH
}

// bookPriceAt maps a screen position inside the orderbook pane to the price
// level rendered on that row (ok=false outside the pane or on a non-level row
// — padding or the mid line). Order mode's click-to-price and wheel walking
// use it; it reads the same RowPrices mapping the pane renders from.
func (m model) bookPriceAt(x, y int) (string, bool) {
	sideW, bookW, _, _ := m.colWidths()
	if x < sideW || x >= sideW+bookW {
		return "", false
	}
	top, h := m.bookPaneGeom()
	row := y - (top + 2) // panel border + title before the content rows
	if row < 0 || row >= h-3 {
		return "", false
	}
	rows := m.orderLadderRows()
	if row >= len(rows) || rows[row] == "" {
		return "", false
	}
	return rows[row], true
}

// orderColumnGeom is the order panel's column: its left screen column, the
// panel's outer height, and the column width.
func (m model) orderColumnGeom() (left, panelH, rightW int) {
	sideW, bookW, tradesW, w := m.colWidths()
	panelH = m.order.orderPanelHeight(w-2, m.bodyHeight(), m.orderGate())
	return sideW + bookW + tradesW, panelH, w
}

// orderPanelPos maps a screen position to the order panel's content
// coordinates (row 0 = first content line, col 0 = first content column);
// ok=false outside the panel's content area.
func (m model) orderPanelPos(x, y int) (row, col int, ok bool) {
	left, panelH, rightW := m.orderColumnGeom()
	if x < left+1 || x >= left+rightW-1 {
		return 0, 0, false
	}
	top := m.bodyTop()
	row = y - (top + 2) // panel border + title
	if row < 0 || row >= panelH-3 {
		return 0, 0, false
	}
	return row, x - (left + 1), true
}

// orderPanelKeyAt maps a click at screen (x,y) onto a key-cap in the order
// panel's hint lines and returns the key it presses — the panel-hint analogue of
// stripKeyAt, so clicking a hinted key behaves like pressing it. The ‹/› arrows,
// chips, and buttons are handled by panelClick instead.
func (m model) orderPanelKeyAt(x, y int) (tea.KeyPressMsg, bool) {
	row, col, ok := m.orderPanelPos(x, y)
	if !ok {
		return tea.KeyPressMsg{}, false
	}
	_, _, rightW := m.orderColumnGeom()
	lines := m.order.panelLines(rightW-2, m.orderGate())
	if row < 0 || row >= len(lines) {
		return tea.KeyPressMsg{}, false
	}
	return keystrip.At(lines[row].hits, col)
}

// stripKeyAt maps a click at screen (x,y) onto the key-hint strip visible in the
// current mode and returns the key that strip would press — the mechanism behind
// "clicking a hint behaves like pressing it". It returns false when the click is
// not on a clickable cap. The footer strip is clickable only in the normal view
// (where its hints are live); an open overlay's own hint strip owns clicks.
func (m model) stripKeyAt(x, y int) (tea.KeyPressMsg, bool) {
	switch m.mode {
	case modeNormal, modeOrder, modeLadder:
		if y == m.h-1 { // the footer's key-hint line is the last screen row, at column 0
			return keystrip.At(m.cFooter.Hits(m.footerKey()), x)
		}
	case modeChart:
		col, row := m.chartHelpScreen()
		if y == row {
			_, hits := chartHelpLine()
			return keystrip.At(hits, x-col)
		}
	case modeConfirm, modeCancelAll, modeQuitConfirm, modeAccountSwitch, modeFunding:
		// The funding screen's strip sits on the last body row in every one of
		// its views (browse, confirm, busy) — it has no overlays.
		if m.mode == modeFunding {
			if y == m.bodyTop()+m.bodyHeight()-1 {
				return keystrip.At(m.funding.stripHits(), x)
			}
			break
		}
		parts, ok := m.modalOverlayParts()
		if !ok {
			break
		}
		for _, cl := range parts.clicks {
			col, row := m.overlayLineOrigin(parts.lines, cl.idx)
			if y == row {
				if send, hit := keystrip.At(cl.hits, x-col); hit {
					return send, true
				}
			}
		}
	}
	return tea.KeyPressMsg{}, false
}

// overlayBodyAt returns the body action for a left click at screen (x,y) inside
// the current modal overlay, or ok=false when the click is not on an interactive
// body region. Regions are tested in append order, so a toggle arrow (recorded
// first) wins over the row-focus region that contains it.
func (m model) overlayBodyAt(x, y int) (func(m *model) tea.Cmd, bool) {
	parts, ok := m.modalOverlayParts()
	if !ok {
		return nil, false
	}
	for _, b := range parts.body {
		col, row := m.overlayLineOrigin(parts.lines, b.idx)
		if y == row && x >= col+b.x0 && x <= col+b.x1 {
			return b.do, true
		}
	}
	return nil, false
}

// inModalOverlay reports whether (x,y) lands on the open modal dialog's box (frame
// included), using the same centered-box geometry as the click-outside-to-close
// hit-test for the help/notices overlays.
func (m model) inModalOverlay(x, y int) bool {
	parts, ok := m.modalOverlayParts()
	if !ok {
		return false
	}
	left, top, right, bottom := m.centeredOverlayBounds(parts.render())
	return x >= left && x <= right && y >= top && y <= bottom
}

// modalOverlayParts returns the rendered parts of the modal overlay open in the
// current mode (so the mouse handler can hit-test its hint lines), or ok=false
// when no such overlay is open.
func (m model) modalOverlayParts() (overlayParts, bool) {
	switch m.mode {
	case modeConfirm:
		return m.confirmParts(), true
	case modeCancelAll:
		return m.cancelAllParts(), true
	case modeQuitConfirm:
		return m.quitConfirmParts(), true
	case modeAccountSwitch:
		return m.accountSwitchParts(), true
	}
	return overlayParts{}, false
}

// overlayLineOrigin is the top-left screen cell of content line lineIdx inside a
// centered overlay box. It derives from centeredOverlayBounds — the single source
// of the centered-overlay box geometry — and offsets by one cell past the border,
// so the clickable-hint hitmap, the body hitmap, and the click-outside-to-close
// bounds can never drift apart.
func (m model) overlayLineOrigin(lines []string, lineIdx int) (col, row int) {
	left, top, _, _ := m.centeredOverlayBounds(strings.Join(lines, "\n"))
	return left + 1, top + 1 + lineIdx // +1 past the inner-left / top border
}

// --- overlays ---

// overlayParts is a modal overlay's rendered lines plus the clickable regions
// within them. clicks carries, per keyed line, the line's index in lines and the
// keystrip hitmap (relative to the line's first column) — so the mouse handler can
// map a click on a centered overlay's hint to the key it presses. body carries the
// interactive regions in the dialog *body* (cancel-all
// scope chips), whose click runs a model mutation directly rather than pressing a
// key (there is no single key for "focus field N" or "select this scope").
type overlayParts struct {
	lines  []string
	clicks []clickLine
	body   []bodyHit
}

// clickLine is one keyed line of an overlay: its row index within the overlay's
// content and the relative hitmap of its caps.
type clickLine struct {
	idx  int
	hits []keystrip.Hit
}

// bodyHit is one interactive body region: the content line it sits on, the
// inclusive column span within that line (relative to the line's first column,
// in display cells), and the model mutation to run when it is clicked. Body hits
// are matched in append order, so a narrow region (a toggle's ‹/› arrow) recorded
// before the broad row-focus region that contains it wins.
type bodyHit struct {
	idx    int
	x0, x1 int
	do     func(m *model) tea.Cmd
}

// add appends a plain (non-clickable) content line.
func (p *overlayParts) add(s string) { p.lines = append(p.lines, s) }

// addBody records an interactive body region on content line idx spanning columns
// [x0,x1] (relative to the line's first column).
func (p *overlayParts) addBody(idx, x0, x1 int, do func(m *model) tea.Cmd) {
	p.body = append(p.body, bodyHit{idx: idx, x0: x0, x1: x1, do: do})
}

// addHint renders items as a key-hint line, appends it, and records its caps as a
// clickable line. The hint is not truncated — the overlay box grows to fit it.
func (p *overlayParts) addHint(items []keystrip.Item) {
	line, hits := keystrip.Layout(items, 1<<20)
	p.clicks = append(p.clicks, clickLine{idx: len(p.lines), hits: hits})
	p.lines = append(p.lines, line)
}

func (p overlayParts) render() string { return strings.Join(p.lines, "\n") }

func (m model) renderConfirm() string { return m.confirmParts().render() }

func (m model) confirmParts() overlayParts {
	o, ok := m.store.Order(m.confirmOrderID)
	if !ok {
		return overlayParts{lines: []string{i18n.T("order no longer known — esc to close")}}
	}
	var p overlayParts
	p.add(uikit.StyTitle.Render(i18n.T("cancel order?")))
	p.add("")
	p.add(i18n.T("%s  %s %s  price %s  qty %s  filled %s  (%s)",
		uikit.FmtSymbol(o.Symbol), o.Side, o.OrderType, uikit.GroupThousands(o.Price), o.Qty, o.FilledQty, o.Status))
	// orderId/clientOrderId are wire field names — never localized.
	p.add(fmt.Sprintf("orderId %d  clientOrderId %s", o.OrderID, o.ClientOrderID))
	p.add("")
	p.addHint([]keystrip.Item{
		keystrip.One("enter", i18n.T("cancel the order")),
		keystrip.One("esc", i18n.T("keep it")),
	})
	return p
}

// renderQuitConfirm draws the soft-quit dialog: q was pressed while a money
// action is still in flight, so quitting means its result lands nowhere.
// ctrl+c (the unconditional quit) is deliberately not gated by this.
func (m model) renderQuitConfirm() string { return m.quitConfirmParts().render() }

func (m model) quitConfirmParts() overlayParts {
	var p overlayParts
	p.add(uikit.StyTitle.Render(i18n.T("quit now?")))
	p.add("")
	what := i18n.T("an order action is still on the wire")
	if m.funding.busy() {
		what = i18n.T("a funding request is still on the wire")
	}
	p.add(uikit.StyWarn.Render(i18n.T("%s — its result will not be shown", what)))
	p.add(uikit.StyDim.Render(i18n.T("what was already sent stays sent; check open orders / history after")))
	p.add("")
	p.addHint([]keystrip.Item{
		keystrip.One("enter", i18n.T("quit anyway")),
		keystrip.One("esc", i18n.T("stay")),
	})
	return p
}

// renderCancelAll draws the cancel-all dialog: a scope selector (this pair / all
// pairs), the snapshotted order count (or a loading state while all-pairs orders
// are still arriving), and a running progress line during the batch.
func (m model) renderCancelAll() string { return m.cancelAllParts().render() }

func (m model) cancelAllParts() overlayParts {
	ca := m.cancelAll
	scopeOpt := func(label string, on bool) string {
		if on {
			return uikit.StyFocusLbl.Render(" " + label + " ")
		}
		return uikit.StyDim.Render(" " + label + " ")
	}
	thisPair := i18n.T("this pair (%s)", uikit.FmtSymbol(m.symbol()))
	allPairs := i18n.T("all pairs")
	var p overlayParts
	p.add(uikit.StyTitle.Render(i18n.T("cancel all open orders")))
	p.add("")
	scopePrefix := i18n.T("scope:") + "  " // each chip carries its own leading/trailing space
	scopeIdx := len(p.lines)
	p.add(scopePrefix + scopeOpt(thisPair, !ca.allPairs) + "  " + scopeOpt(allPairs, ca.allPairs))
	if !ca.running {
		// The scope chips are clickable: each selects its scope directly (a no-op if
		// already active), routed through the same setCancelAllScope the keys use.
		setScope := func(allPairs bool) func(*model) tea.Cmd {
			return func(m *model) tea.Cmd { m.setCancelAllScope(allPairs); return nil }
		}
		chip1W := lipgloss.Width(" " + thisPair + " ")
		chip2W := lipgloss.Width(" " + allPairs + " ")
		x1 := lipgloss.Width(scopePrefix) // first chip starts right after the prefix
		p.addBody(scopeIdx, x1, x1+chip1W-1, setScope(false))
		x2 := x1 + chip1W + 2 // + the "  " separator between the chips
		p.addBody(scopeIdx, x2, x2+chip2W-1, setScope(true))
		p.addHint([]keystrip.Item{keystrip.Multi(i18n.T("switch scope"),
			keystrip.Btn("←", "left"), keystrip.Btn("→", "right"), keystrip.Btn("tab", "tab"))})
	}
	p.add("")

	switch {
	case ca.running:
		p.add(i18n.T("canceling… %s/%s   (%s ok, %s failed)",
			strconv.Itoa(ca.idx), strconv.Itoa(len(ca.snapshot)), strconv.Itoa(ca.ok), strconv.Itoa(ca.failed)))
		if ca.aborted {
			p.add(uikit.StyWarn.Render(i18n.T("stopping after the current cancel…")))
		}
		p.add("")
		p.addHint([]keystrip.Item{keystrip.One("esc", i18n.T("stop"))})
	case !ca.snapped:
		loaded, total := m.cancelAllLoaded()
		p.add(uikit.StyWarn.Render(i18n.T("loading open orders… %d/%d pairs ready — please wait", loaded, total)))
		p.add(uikit.StyDim.Render(i18n.T("the count appears once every pair has loaded")))
		p.add("")
		p.addHint([]keystrip.Item{keystrip.One("esc", i18n.T("close"))})
	case len(ca.snapshot) == 0:
		p.add(i18n.T("no open orders to cancel in this scope."))
		p.add("")
		p.addHint([]keystrip.Item{keystrip.One("esc", i18n.T("close"))})
	default:
		p.add(uikit.StyWarn.Render(i18n.T("will cancel %d open order(s)", len(ca.snapshot))))
		p.add(uikit.StyDim.Render(i18n.T("(snapshot taken now — later order changes are ignored)")))
		p.add("")
		p.addHint([]keystrip.Item{
			keystrip.One("enter", i18n.T("cancel them")),
			keystrip.One("esc", i18n.T("keep them")),
		})
	}
	return p
}

func (m model) renderHelp() string {
	rows := [][2]string{
		{"tab / shift+tab", i18n.T("move focus (markets → open orders → balances)")},
		{"↑/↓", i18n.T("active market / move in open orders / scroll balances")},
		{"/", i18n.T("filter the focused list (markets / balances); enter picks a match, esc cancels")},
	}
	rows = append(rows, [2]string{"b / s", i18n.T("order panel (buy / sell): u toggles a limit order between quantity and amount; price controls are limit-only — j/k picks a price from the book, b/s set the side (price follows to its best), a crosses to the opposite touch, m/l anchor mid/last; ←/→ move the caret in a number field and cycle the selector rows, %s/%s nudge the price ±1/±10 ticks, %s cycles the balance presets, enter reviews then places", keyTickStep1, keyTickStep10, keyPreset)})
	rows = append(rows, [2]string{":", i18n.T("command bar: type an order (b 0.05 @ b-2 · s 25%% @ a · b 500k krw @ mkt), enter reviews then places")})
	rows = append(rows, [2]string{"t", i18n.T("trade ladder: %s arms a size and t sets the default tif (setting a chip focuses it; ←/→ or clicking ‹ › then adjusts the focused chip), j/k walks the book, b/s bids/asks at the cursor row (B/S market), x cancels at the row", sizeKeysLabel(m.ladder.sizeLevels))})
	rows = append(rows,
		[2]string{"x", i18n.T("cancel the selected order (with confirm)")},
		[2]string{"X", i18n.T("cancel ALL orders (choose scope: this pair / all pairs)")},
		[2]string{"o", i18n.T("orders pane: open ↔ closed tab (terminal orders seen this session; read-only; also in the ladder/order panel, or click the tabs)")},
		[2]string{"a", i18n.T("orders: toggle active-pair / all-pairs view")},
		[2]string{"- / +", i18n.T("orderbook grouping: coarser / finer (the pair's levels come from its tick-size policy; the grp chip on the book/ladder title shows the active level)")},
	)
	if m.cfg.Candles != nil {
		rows = append(rows,
			[2]string{"g", i18n.T("candle chart ([ ] interval, +/- zoom, ←/→ scroll)")},
			[2]string{"G", i18n.T("inline candle pane (over orderbook/trades; click it to open the full chart)")},
		)
	}
	if m.cfg.Funding != nil {
		rows = append(rows,
			[2]string{"f", i18n.T("deposits & withdrawals (also: enter on the balances pane)")},
		)
	}
	if m.multiAccount() {
		rows = append(rows,
			[2]string{"@", i18n.T("switch sub-account")},
		)
	}
	rows = append(rows,
		[2]string{"n", i18n.T("notices log (stream health: gaps, reconnects, backfill)")},
		[2]string{"wheel / drag", i18n.T("scroll a list / drag a border to resize (mouse)")},
		[2]string{"=", i18n.T("reset the panel layout")},
		[2]string{"C", i18n.T("toggle color scheme (green-red / red-blue)")},
		[2]string{"?", i18n.T("this help")},
		[2]string{"esc", i18n.T("one step back (close overlay / cancel search / leave a mode); never quits")},
		[2]string{"q / ctrl+c", i18n.T("quit (q asks first while an action is in flight; ctrl+c never asks)")},
	)
	lines := []string{uikit.StyTitle.Render(i18n.T("keys")), ""}
	for _, r := range rows {
		lines = append(lines, uikit.PadRight(r[0], 18)+uikit.StyDim.Render(r[1]))
	}
	return strings.Join(lines, "\n")
}

func (m model) renderNotices(bodyH int) string {
	body := m.cNotices.Lines(notices.Key{NoticeRev: m.store.NoticeRev(), W: m.w - 8, H: bodyH - 6},
		notices.Data{Notices: m.store.Notices(bodyH - 6)})
	lines := append([]string{uikit.StyTitle.Render(i18n.T("stream notices (newest first)")), ""}, body...)
	return strings.Join(lines, "\n")
}

// accountSwitchMaxRows caps how many account rows the '@' switcher shows at once.
// A session with more subscribed sub-accounts scrolls within this fixed window
// (acctScroll) instead of growing the box off the screen.
const accountSwitchMaxRows = 8

// accountName is the display name shown in the switcher's "name" column. The main
// sub-account (seq 1) is named "main"; other sub-accounts have no name.
func accountName(seq int) string {
	if seq == accountseq.Main {
		return i18n.T("main")
	}
	return ""
}

// renderAccountSwitch draws the '@' sub-account switcher popup: see
// accountSwitchParts for the layout. It is the render side of the shared
// overlayParts so its rows and hint caps are click-mapped like every other modal.
func (m model) renderAccountSwitch() string { return m.accountSwitchParts().render() }

// accountSwitchParts builds the sub-account switcher as a two-column table
// (seq · name) with a pinned column header, a fixed-height scrolling window over
// the subscribed accounts, and a clickable key-hint line. The active account is
// marked by a "›" chevron in the left gutter; the pending selection (the keyboard
// cursor) is the reverse-highlighted row. Every account row is a clickable body
// region that selects it (clicking the already-selected row confirms, like enter
// — see clickAccountRow), and the hint caps press their own keys — the shared
// overlayParts machinery, so nothing can drift from what is drawn. Column widths
// are computed over the whole (fixed) account set, so the box never jitters as
// the cursor or window moves.
func (m model) accountSwitchParts() overlayParts {
	var p overlayParts
	seqs := m.accountSeqs

	const gutter = 2 // "› " active chevron / "  " otherwise
	const colGap = 2 // gap between the seq and name columns
	// The headers set each column's floor. "seq" is the accountSeq wire name
	// (never localized); the name header localizes, so measure display cells.
	seqW := lipgloss.Width("seq")
	nameHeader := i18n.T("name")
	nameW := lipgloss.Width(nameHeader)
	for _, seq := range seqs {
		if w := lipgloss.Width(strconv.Itoa(seq)); w > seqW {
			seqW = w
		}
		if w := lipgloss.Width(accountName(seq)); w > nameW {
			nameW = w
		}
	}
	barW := gutter + seqW + colGap + nameW
	// cell lays out one table row's plain text (no styling) at the fixed columns,
	// padded to barW so a highlight bar or the header underline fills the box.
	cell := func(lead, seq, name string) string {
		return uikit.PadRight(lead+uikit.PadRight(seq, seqW+colGap)+name, barW)
	}

	p.add(uikit.StyTitle.Render(i18n.T("sub-account")))
	p.add("")
	p.add(uikit.StyDim.Render(cell(strings.Repeat(" ", gutter), "seq", nameHeader)))

	vis := accountSwitchMaxRows
	if vis > len(seqs) {
		vis = len(seqs)
	}
	start := clamp(m.acctScroll, 0, maxInt(0, len(seqs)-vis))
	for off := 0; off < vis; off++ {
		i := start + off
		seq := seqs[i]
		lead := "  "
		if seq == m.accountSeq() {
			lead = "› " // the active account
		}
		text := cell(lead, strconv.Itoa(seq), accountName(seq))
		idx := len(p.lines)
		if i == m.acctCursor {
			p.add(uikit.StyActive.Render(text)) // the keyboard cursor row
		} else {
			p.add(text)
		}
		seqCopy := seq // click selects this row; a click on the selected row confirms
		p.addBody(idx, 0, barW-1, func(m *model) tea.Cmd { m.clickAccountRow(seqCopy); return nil })
	}
	// A scroll-position indicator only when the list overflows the fixed window.
	if len(seqs) > vis {
		p.add(uikit.StyDim.Render(fmt.Sprintf("%d-%d/%d", start+1, start+vis, len(seqs))))
	}

	p.add("")
	p.addHint([]keystrip.Item{
		keystrip.Multi(i18n.T("move"), keystrip.Btn("↑", "up"), keystrip.Btn("↓", "down")),
		keystrip.One("enter", i18n.T("switch")),
		keystrip.One("esc", i18n.T("close")),
	})
	return p
}

// layout recomputes size-dependent state after a resize: the orders panel is
// self-rendered, so there is no component to size — just re-clamp the scroll
// windows (and keep the orders selection visible) to the new panel heights.
func (m *model) layout() {
	m.orderScroll = clamp(m.orderScroll, 0, m.orderScrollMax())
	m.ensureOrderVisible()
	m.balScroll = clamp(m.balScroll, 0, m.balScrollMax())
}

// --- small helpers ---

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

// scale rounds a fraction of n to the nearest cell.
func scale(frac float64, n int) int { return int(frac*float64(n) + 0.5) }
