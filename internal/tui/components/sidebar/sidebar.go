// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package sidebar renders the markets pane: one row per watched symbol (symbol +
// 24h %change), windowed by a scroll offset. The active row is reverse-
// highlighted; every other row is tinted by its 24h direction so the list reads
// as a heat map. While quick search targets this pane the list is FILTERED to
// substring matches on the symbol, windowed by a separate search scroll, with
// the search candidate highlighted instead. It is a pure view component —
// [Model.View] is a function of its explicit [Key] and [Data], owns no shared
// state, and caches its render via [uikit.Memo] keyed on [Key]. The parent
// supplies the data and a [Key] whose ticker revision stands in for "did the
// tickers change", so an unrelated frame reuses the cached render.
package sidebar

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. TickerRev is
// the store's ticker section revision (any ticker change ⇒ rev change ⇒
// re-render). Active is the selected symbol (reverse-highlighted in the normal
// list). Scroll is the wheel offset of the normal list; Searching/Query/
// SearchCursor/SearchScroll drive the filtered list while quick search targets
// this pane. Focused selects the border color and footer label color. W and H
// are the OUTER panel size; Style is the palette identity.
type Key struct {
	TickerRev    uint64
	Active       string
	Scroll       int
	Searching    bool
	Query        string
	SearchCursor int
	SearchScroll int
	Focused      bool
	W, H         int
	Style        uikit.StyleID
}

// Row is one watched symbol's render input: the symbol, its 24h change fields,
// and whether a current ticker has arrived. PriceChangePercent is shown (with a
// "%" suffix); PriceChange supplies the sign for the heat-map tint. Ready false
// means no current ticker yet — the change is replaced by a loading marker and
// the row dims rather than showing a stale tint.
type Row struct {
	Symbol             string
	PriceChangePercent string
	PriceChange        string
	Ready              bool
}

// Data is what the component renders on a cache miss: one Row per watched
// symbol, in list order. A ticker change bumps TickerRev in the Key, so the Key
// still decides the hit.
type Data struct {
	Rows []Row
}

// Model is the markets pane component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a markets pane component.
func New() *Model { return &Model{} }

// View renders the full bordered pane (with the position indicator and live
// search query spliced into the bottom border) for k/d, reusing the cached
// render when k is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	pal := uikit.PaletteFor(uikit.ColorScheme(k.Style.Scheme), k.Style.Profile)
	w := k.W - 2    // inner content width (panel border)
	rows := k.H - 3 // content rows (panel border + title)
	lines := sidebarLines(k, d, pal, w, rows)
	return uikit.PanelWithFooter(k.Focused, k.Searching, k.Query, i18n.T("markets"), footer(k, d), lines, k.W, k.H)
}

// footer is the position indicator: the selected symbol's 1-based index over the
// total, or — while searching — the candidate's position over the match count.
func footer(k Key, d Data) string {
	n := len(d.Rows)
	if k.Searching {
		idxs := filtered(k, d)
		if len(idxs) == 0 {
			return i18n.T("no match")
		}
		return fmt.Sprintf("%d/%d match", clamp(k.SearchCursor, 0, len(idxs)-1)+1, len(idxs))
	}
	return fmt.Sprintf("%d/%d", activeIndex(k, d)+1, n)
}

// sidebarLines builds the inner rows for the normal or the filtered list.
func sidebarLines(k Key, d Data, pal uikit.Palette, w, rows int) []string {
	n := len(d.Rows)
	if n == 0 || rows < 1 {
		return nil
	}
	if k.Searching {
		return filteredLines(k, d, pal, w, rows)
	}
	start := startOffset(k, n, rows)
	out := make([]string, 0, rows)
	for i := start; i < n && len(out) < rows; i++ {
		out = append(out, rowStyle(d.Rows[i].Symbol == k.Active, d.Rows[i], pal).Render(rowText(d.Rows[i], w)))
	}
	return out
}

// filteredLines renders only the symbols matching the query, windowed by the
// search scroll and highlighting the candidate row (the search cursor).
func filteredLines(k Key, d Data, pal uikit.Palette, w, rows int) []string {
	idxs := filtered(k, d)
	if len(idxs) == 0 {
		return []string{uikit.StyDim.Render(i18n.T("no match"))}
	}
	start := clamp(k.SearchScroll, 0, max(0, len(idxs)-rows))
	out := make([]string, 0, rows)
	for p := start; p < len(idxs) && len(out) < rows; p++ {
		r := d.Rows[idxs[p]]
		out = append(out, rowStyle(p == k.SearchCursor, r, pal).Render(rowText(r, w)))
	}
	return out
}

// rowText formats one market row (symbol + 24h %change), padded to w. The symbol
// always shows; only the change is gated on a current ticker — while it is
// absent the change is a compact loading marker rather than a stale or blank
// value.
func rowText(r Row, w int) string {
	chg := "…"
	if r.Ready {
		chg = r.PriceChangePercent + "%"
	}
	sym := uikit.FmtSymbol(r.Symbol)
	gap := w - lipgloss.Width(sym) - lipgloss.Width(chg)
	if gap < 1 {
		gap = 1
	}
	return sym + strings.Repeat(" ", gap) + chg
}

// rowStyle picks exactly one style for a market row (so reverse video for a
// highlighted row never nests with a per-token color): reverse when highlighted,
// else tinted by 24h direction, else dim before a ticker arrives.
func rowStyle(highlighted bool, r Row, pal uikit.Palette) lipgloss.Style {
	switch {
	case highlighted:
		return uikit.StyActive
	case r.Ready && uikit.IsNegative(r.PriceChange):
		return pal.Down.Fg
	case r.Ready:
		return pal.Up.Fg
	default:
		return uikit.StyDim
	}
}

// startOffset is the first visible symbol index: the wheel scroll offset,
// clamped so a resize or a shrunk list can't leave the window past the end.
func startOffset(k Key, n, visible int) int {
	if n <= visible || visible <= 0 {
		return 0
	}
	return clamp(k.Scroll, 0, n-visible)
}

// activeIndex is the position of the active symbol in the (unfiltered) list, or 0
// when it is absent.
func activeIndex(k Key, d Data) int {
	for i, r := range d.Rows {
		if r.Symbol == k.Active {
			return i
		}
	}
	return 0
}

// filtered returns the indices into d.Rows whose symbol contains the query (all
// of them when the query is empty), preserving list order.
func filtered(k Key, d Data) []int {
	q := uikit.SymbolSearchKey(strings.TrimSpace(k.Query))
	out := make([]int, 0, len(d.Rows))
	for i, r := range d.Rows {
		if q == "" || strings.Contains(uikit.SymbolSearchKey(r.Symbol), q) {
			out = append(out, i)
		}
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
