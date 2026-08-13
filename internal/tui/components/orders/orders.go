// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package orders renders the orders pane, a fixed five-column table with a dim
// header, the data rows windowed by the scroll offset, and the selected row
// reverse-highlighted. It has two tabs, selected by [Key.Closed]:
//
//   - Open (symbol, side, price, qty, filled): the live cancel targets. There
//     is no status column: this tab only ever lists OPEN orders (a small fixed
//     status set), so the steady-state status carried little that the filled
//     column didn't already show, and dropping it buys width for the columns
//     that matter on a narrow terminal. The one status worth surfacing — a
//     cancel in flight — is shown by graying that row out (parallel
//     [Data.Canceling]), the same "style the row, don't spend a column"
//     approach FlashID uses for a just-accepted order.
//   - Closed (symbol, side, price, filled/qty, status): the terminal orders
//     observed this session, read-only. Here the status column IS the point —
//     it is where a rejected (expired) or canceled order becomes visible at
//     all — and filled/qty makes a partial fill's returned remainder legible.
//
// It is a pure view component — [Model.View] is a function
// of its explicit [Key] and [Data], owns no shared state, and caches its render
// via [uikit.Memo] keyed on [Key]. The parent builds the table rows and bumps
// RowsRev whenever it rebuilds them (including when the canceling set changes),
// so the Key alone decides a cache hit.
package orders

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// The table's five columns per tab. Weights size them in proportion to fit
// the panel; one space separates columns, so the rendered width is
// sum(widths) + 4. All columns stay visible at any supported terminal size
// (fitWidths truncates cell content instead of dropping a column).
// colTitles is a function, not a package var: the active language is decided
// after package init, so the headers must localize at render time.
func colTitles(closed bool) []string {
	if closed {
		return []string{i18n.T("symbol"), i18n.T("side"), i18n.T("price"), i18n.T("filled/qty"), i18n.T("status")}
	}
	return []string{i18n.T("symbol"), i18n.T("side"), i18n.T("price"), i18n.T("qty"), i18n.T("filled")}
}

var colWeights = []int{8, 4, 14, 12, 10}

// Key is the comparable cache key: equal Keys render identically. RowsRev is the
// parent's monotonic stamp for the projected rows (it bumps on order events AND
// on cancel-in-flight changes, so the component never keys on the raw order
// revision); Cursor is the selected (cancel-target) row; Scroll is the window
// offset; AllPairs selects the all-pairs scope for the title; Focused selects
// the border color and footer label color; Loading shows the title's loading
// hint; Count is the order total for the title/footer; Symbol is the per-pair
// scope label; W and H are the OUTER panel size; Style is the palette identity.
type Key struct {
	RowsRev  uint64
	Cursor   int
	Scroll   int
	AllPairs bool
	Focused  bool
	Loading  bool
	Count    int
	Symbol   string
	FlashID  int64 // a just-accepted order's id: its row renders highlighted (0 = none)
	// Closed selects the closed tab: its column set, its title, and no
	// loading hint (the closed set is session-observed, never "loading").
	Closed bool
	W, H   int
	Style  uikit.StyleID
}

// Data is what the component renders on a cache miss: the parent-built table
// projection (one []string of cell text per order, in column order), the
// parallel order IDs, and a parallel per-row "cancel in flight" flag (rendered
// as a grayed-out row instead of a status column). Canceling may be
// nil or shorter than Rows — a missing entry reads as false. RowsRev in the Key
// stands in for "did these rows change", and the parent bumps it when the
// canceling set changes too.
type Data struct {
	Rows      [][]string
	OrderIDs  []int64
	Canceling []bool
}

// Model is the orders component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns an orders component.
func New() *Model { return &Model{} }

// View renders the full bordered pane for k/d, reusing the cached render when k
// is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	w := k.W - 2    // inner content width (panel border)
	rows := k.H - 3 // content rows (panel border + title)
	lines := tableLines(w, rows, k, d)
	// The title is pre-styled per-segment (the tab chips carry their own color),
	// so it goes through the styled-title panel path rather than the single-style
	// one, which would end the styling at the first tab's color reset.
	return uikit.PanelWithFooterStyled(k.Focused, false, "", title(k), footer(k), lines, k.W, k.H)
}

// tabSep separates the title's two tab labels; it renders dim so the two
// clickable tab chips read as distinct from it.
const tabSep = " │ "

// titlePrefix is the pane's noun, drawn before the tab switcher ("orders: ").
func titlePrefix() string { return i18n.T("orders") + ": " }

// tabLabel is a tab's plain text, bracketing the active one. The bracket is a
// style-independent marker of the current tab — it reads the same with color
// stripped and in every locale — that the color emphasis in styleTab layers on
// top of. It is also the exact text TitleTab measures the click ranges from.
func tabLabel(name string, active bool) string {
	if active {
		return "[" + name + "]"
	}
	return name
}

// styleTab colors one tab label: both tabs get the clickable style (the same
// cyan-underline the key-hint caps use, so a reader can see the tab acts on a
// click), and the active one is additionally reverse-highlighted so the current
// tab is unmistakable.
func styleTab(label string, active bool) string {
	if active {
		return styTabActive.Render(label)
	}
	return uikit.StyKey.Render(label)
}

// styTabActive is the clickable style plus a reverse highlight, for the active
// tab.
var styTabActive = uikit.StyKey.Bold(true).Reverse(true)

// titleTabs is the title's tab switcher: both tab labels always visible so the
// inactive tab is discoverable by sight, each styled clickable and the active one
// highlighted. TitleTab maps a click back onto a tab.
func titleTabs(closed bool) string {
	return styleTab(tabLabel(i18n.T("open"), !closed), !closed) +
		uikit.StyDim.Render(tabSep) +
		styleTab(tabLabel(i18n.T("closed"), closed), closed)
}

// title is the panel title: the "orders:" noun, the tab switcher, then the active
// scope (the current pair in per-pair view, or "all pairs"), the count, and a
// loading hint while the displayed pair(s) still lack an authoritative snapshot.
// The whole line is pre-styled — the tab chips carry their own color, so the
// non-tab text is bolded here to match a plain panel title (the styled-title
// panel path adds no style of its own). Switcher near the front: at a narrow
// width the truncation drops scope and count before the tabs.
func title(k Key) string {
	scope := uikit.FmtSymbol(k.Symbol)
	if k.AllPairs {
		scope = i18n.T("all pairs")
	}
	count := fmt.Sprintf("(%d)", k.Count)
	if k.Loading {
		count = fmt.Sprintf("(%d · %s)", k.Count, i18n.T("loading…"))
	}
	return uikit.StyTitle.Render(titlePrefix()) +
		titleTabs(k.Closed) +
		uikit.StyTitle.Render(" · "+scope+" "+count)
}

// TitleTab maps innerX — a click's cell offset into the pane's title line — to
// the tab it selects. ok is false off the two tab segments (the "orders:" prefix,
// the separator, and the trailing scope/count are all inert; a click on the
// active segment is a valid, idempotent selection). Widths are measured off the
// localized, PLAIN labels (the color styling does not change cell width), so the
// hit ranges track the rendered title in every locale.
func TitleTab(innerX int, closed bool) (toClosed, ok bool) {
	x := innerX - lipgloss.Width(titlePrefix())
	openW := lipgloss.Width(tabLabel(i18n.T("open"), !closed))
	sepW := lipgloss.Width(tabSep)
	closedW := lipgloss.Width(tabLabel(i18n.T("closed"), closed))
	switch {
	case x < 0:
		return false, false
	case x < openW:
		return false, true
	case x < openW+sepW:
		return false, false
	case x < openW+sepW+closedW:
		return true, true
	}
	return false, false
}

// footer is the table's position indicator (cursor/total), or empty when there
// are no orders.
func footer(k Key) string {
	if k.Count == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d", clamp(k.Cursor, 0, k.Count-1)+1, k.Count)
}

// tableLines renders the table into w-wide lines: a dim column header, then the
// data rows windowed by Scroll, with the selected row (Cursor) reverse-
// highlighted. rows is the number of content lines available (header included).
func tableLines(w, rows int, k Key, d Data) []string {
	titles := colTitles(k.Closed)
	widths := fitWidths(w-(len(titles)-1), colWeights, 3)
	cell := func(s string, i int) string { return uikit.PadRight(uikit.ClipTail(s, widths[i]), widths[i]) }
	rowText := func(cells []string) string {
		out := make([]string, len(titles))
		for i := range titles {
			out[i] = cell(cells[i], i)
		}
		return strings.Join(out, " ")
	}
	lines := []string{uikit.StyDim.Render(rowText(titles))}
	dataRows := rows - 1 // minus the pinned header
	if dataRows < 1 {
		dataRows = 1
	}
	start := clamp(k.Scroll, 0, max(0, len(d.Rows)-dataRows))
	for r := start; r < len(d.Rows) && len(lines) < rows; r++ {
		line := rowText(d.Rows[r])
		// A cancel in flight is the dominant state (the row is leaving): gray it
		// out regardless of cursor/flash, so the feedback survives even when the
		// row being canceled is the selected one (the common case).
		switch {
		case r < len(d.Canceling) && d.Canceling[r]:
			line = uikit.StyCanceling.Render(line) // cancel in flight
		case r == k.Cursor:
			line = uikit.StyActive.Render(line) // selected row (the cancel target)
		case k.FlashID != 0 && r < len(d.OrderIDs) && d.OrderIDs[r] == k.FlashID:
			line = uikit.StyOK.Render(line) // the just-accepted order, flashed briefly
		}
		lines = append(lines, line)
	}
	return lines
}

// fitWidths distributes total across columns in proportion to weights, each at
// least min, guaranteeing the returned widths sum to no more than total (so a
// fixed-column table never overflows its container). When the desired weights
// already fit, they are returned as-is.
func fitWidths(total int, weights []int, min int) []int {
	sum := 0
	for _, x := range weights {
		sum += x
	}
	out := make([]int, len(weights))
	if total >= sum {
		copy(out, weights)
		return out
	}
	if total < min*len(weights) {
		total = min * len(weights) // degenerate; content truncates, nothing panics
	}
	used := 0
	for i, x := range weights {
		v := x * total / sum
		if v < min {
			v = min
		}
		out[i] = v
		used += v
	}
	// Integer rounding + the min floor can overshoot; trim from the widest.
	for used > total {
		widest := 0
		for i := range out {
			if out[i] > out[widest] {
				widest = i
			}
		}
		if out[widest] <= min {
			break
		}
		out[widest]--
		used--
	}
	return out
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
