// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package balances renders the balances pane: a pinned column header
// (currency / available / total) above a scrolling list of per-currency
// balances. It is a pure view component — [Model.View] is a function of its
// explicit [Key] and [Data], owns no shared state, and caches its render via
// [uikit.Memo] keyed on [Key]. The parent supplies the data and a [Key] whose
// store revision stands in for "did the balances change", so an unrelated frame
// reuses the cached render.
package balances

import (
	"fmt"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/i18n"
	"github.com/digitalx-official/digitalx-cli/internal/stream/state"
	"github.com/digitalx-official/digitalx-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. BalanceRev is
// the store's balances section revision (balances change ⇒ rev change ⇒
// re-render); Ready gates the loading state. AccountSeq is the active
// sub-account whose balances Data carries — it MUST be in the key because a
// pure account switch changes which balances are shown (BalancesFor) without
// bumping BalanceRev, so without it the memo would keep the previous account's
// render. Scroll is the list window top row. Searching/Query/SearchScroll drive
// the live currency filter and its own window. Focused selects the border
// color. W and H are the OUTER panel size; Style is the palette identity.
type Key struct {
	BalanceRev   uint64
	AccountSeq   int
	Ready        bool
	Scroll       int
	Searching    bool
	Query        string
	SearchScroll int
	Focused      bool
	W, H         int
	Style        uikit.StyleID
}

// Data is what the component renders on a cache miss. Balances is the store's
// sorted balance slice; a change to it bumps BalanceRev, so the Key still
// decides the hit.
type Data struct {
	Balances []state.Balance
}

// Model is the balances component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a balances component.
func New() *Model { return &Model{} }

// View renders the full bordered pane for k/d, reusing the cached render when k
// is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	w := k.W - 2    // inner content width (panel border)
	rows := k.H - 3 // content rows (panel border + title)
	lines := balanceLines(k, d, w, rows)
	return uikit.PanelWithFooter(k.Focused, k.Searching, k.Query, i18n.T("balances"), footer(k, d), lines, k.W, k.H)
}

// balanceLines builds the pinned header plus the visible balance entries: while
// searching the balances pane it filters by Query and windows by SearchScroll;
// otherwise it windows the full list by Scroll.
func balanceLines(k Key, d Data, w, rows int) []string {
	if !k.Ready {
		return []string{uikit.StyDim.Render(i18n.T("loading…"))}
	}
	bals := d.Balances
	if len(bals) == 0 {
		return []string{uikit.StyDim.Render(i18n.T("no balances"))}
	}
	curW := clamp(w/3, 4, 10)
	// A one-column gutter on each side of the available column: a clipped
	// currency code or number fills its full width, and without the gutters a
	// full-width available value runs straight into the code before it and the
	// total after it.
	numW := (w - curW - 2) / 2
	if numW < 6 {
		numW = 6
	}
	// Cells render exact when they fit and compact ("9.62B") when they
	// don't — a narrow account column keeps magnitude readable instead of
	// clipping a grouped figure into a digit soup.
	num := func(v string) string { return uikit.PadLeft(uikit.Compact(v, numW), numW) }
	row := func(b state.Balance) string {
		return uikit.PadRight(uikit.FmtCurrency(b.Currency), curW) + " " + num(b.Available) + " " + num(b.Balance)
	}
	// The column header is pinned; the entries below it scroll. Its labels
	// clip lead-keeping (ClipTail) like the numeric cells do, so a crowded
	// header reads "avail…", not PadLeft's tail-keeping "…lable".
	hdr := func(s string, cw int) string { return uikit.PadLeft(uikit.ClipTail(s, cw), cw) }
	lines := []string{uikit.StyDim.Render(uikit.PadRight(uikit.ClipTail(i18n.T("currency"), curW), curW) + " " + hdr(i18n.T("available"), numW) + " " + hdr(i18n.T("total"), numW))}
	if k.Searching {
		// Filtered to matches, windowed by SearchScroll.
		idxs := filteredBalances(k.Query, bals)
		if len(idxs) == 0 {
			return append(lines, uikit.StyDim.Render(i18n.T("no match")))
		}
		start := clamp(k.SearchScroll, 0, maxInt(0, len(idxs)-(rows-1)))
		for p := start; p < len(idxs) && len(lines) < rows; p++ {
			lines = append(lines, row(bals[idxs[p]]))
		}
		return lines
	}
	for i := scrollClamped(k.Scroll, len(bals), visible(rows)); i < len(bals) && len(lines) < rows; i++ {
		lines = append(lines, row(bals[i]))
	}
	return lines
}

// footer is the scroll-position indicator: the visible row range over the total
// (e.g. 3-8/20), or empty when nothing is loaded yet. While searching it ranges
// over the matches instead.
func footer(k Key, d Data) string {
	total := len(d.Balances)
	if total == 0 {
		return ""
	}
	vis := visible(k.H - 3)
	if k.Searching {
		n := len(filteredBalances(k.Query, d.Balances))
		if n == 0 {
			return i18n.T("no match")
		}
		start := clamp(k.SearchScroll, 0, maxInt(0, n-vis))
		end := start + vis
		if end > n {
			end = n
		}
		return fmt.Sprintf("%d-%d/%d match", start+1, end, n)
	}
	start := scrollClamped(k.Scroll, total, vis)
	end := start + vis
	if end > total {
		end = total
	}
	return fmt.Sprintf("%d-%d/%d", start+1, end, total)
}

// filteredBalances returns the indices into the balances whose currency contains
// the search query (all when empty), preserving order.
func filteredBalances(query string, bals []state.Balance) []int {
	q := uikit.SymbolSearchKey(strings.TrimSpace(query))
	out := make([]int, 0, len(bals))
	for i, b := range bals {
		if q == "" || strings.Contains(uikit.SymbolSearchKey(b.Currency), q) {
			out = append(out, i)
		}
	}
	return out
}

// visible is the number of balance entry rows the list can show: the content
// rows (panel height minus border and title) minus the pinned column header.
func visible(rows int) int {
	v := rows - 1
	if v < 1 {
		v = 1
	}
	return v
}

// scrollClamped is the effective scroll offset, clamped so the window never runs
// past the end of the list.
func scrollClamped(scroll, total, vis int) int {
	max := total - vis
	if max < 0 {
		max = 0
	}
	return clamp(scroll, 0, max)
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
