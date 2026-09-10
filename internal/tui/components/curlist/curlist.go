// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package curlist renders the funding screen's currency list: a pinned column
// header (currency / available / est. KRW value) above the parent-projected
// rows, with the selected row reverse-highlighted and a divider row between
// the held and unheld sections. It is a pure view component — [Model.View] is
// a function of its explicit [Key] and [Data], owns no shared state, and
// caches its render via [uikit.Memo] keyed on [Key]. The parent builds the
// rows (ordering, filtering, value estimation) and stamps Rev/BalanceRev/
// TickerRev so the Key alone decides a cache hit.
package curlist

import (
	"fmt"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/i18n"
	"github.com/digitalx-official/digitalx-cli/internal/tui/uikit"
)

// Row is one projected list row. Display strings are parent-formatted; empty
// Value means no estimate is available (the pair's ticker is not subscribed).
type Row struct {
	Code      string // display code ("BTC"); "" for the divider row
	Avail     string // formatted available balance ("" for none)
	Value     string // formatted estimated KRW value ("" hidden)
	Held      bool
	Suspended bool
	Divider   bool
}

// Key is the comparable cache key: equal Keys render identically. Rev is the
// parent's stamp for the funding data the rows derive from; BalanceRev and
// TickerRev are the store revisions feeding the balance/value columns; Ready
// gates the catalog-loading hint; SelIdx is the selected row; Scroll is the
// window top. Searching/Query drive the filter prompt in the border. Focused
// selects the border color. W and H are the OUTER panel size; Style is the
// palette identity.
type Key struct {
	Rev        uint64
	BalanceRev uint64
	TickerRev  uint64
	Ready      bool
	SelIdx     int
	Scroll     int
	Searching  bool
	Query      string
	Focused    bool
	W, H       int
	Style      uikit.StyleID
}

// Data is what the component renders on a cache miss: the parent-projected
// rows, in display order. The revisions in the Key stand in for "did these
// change".
type Data struct {
	Rows []Row
}

// Model is the currency-list component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a currency-list component.
func New() *Model { return &Model{} }

// View renders the full bordered pane for k/d, reusing the cached render when
// k is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	w := k.W - 2
	rows := k.H - 3
	lines := listLines(k, d, w, rows)
	return uikit.PanelWithFooter(k.Focused, k.Searching, k.Query, i18n.T("currencies"), footer(k, d), lines, k.W, k.H)
}

// listLines builds the pinned column header plus the visible rows, windowed by
// Scroll with the selected row highlighted.
func listLines(k Key, d Data, w, rows int) []string {
	if len(d.Rows) == 0 {
		if !k.Ready {
			return []string{uikit.StyDim.Render(i18n.T("loading…"))}
		}
		return []string{uikit.StyDim.Render(i18n.T("no match"))}
	}
	// One space after the code and two between the numeric columns, so two
	// full-width numbers can never visually run together.
	codeW := clamp(w/4, 5, 9)
	numW := (w - codeW - 3) / 2
	if numW < 6 {
		numW = 6
	}
	header := uikit.PadRight(i18n.T("currency"), codeW) + " " + uikit.PadLeft(i18n.T("available"), numW) + "  " + uikit.PadLeft(i18n.T("value(krw)"), numW)
	lines := []string{uikit.StyDim.Render(header)}
	start := clamp(k.Scroll, 0, max(0, len(d.Rows)-(rows-1)))
	for i := start; i < len(d.Rows) && len(lines) < rows; i++ {
		lines = append(lines, rowLine(d.Rows[i], i == k.SelIdx, codeW, numW, w))
	}
	if !k.Ready && len(lines) < rows {
		lines = append(lines, uikit.StyDim.Render(i18n.T("loading currencies…")))
	}
	return lines
}

func rowLine(r Row, selected bool, codeW, numW, w int) string {
	if r.Divider {
		label := " no balance "
		dashes := w - len(label)
		if dashes < 2 {
			dashes = 2
		}
		return uikit.StyDim.Render(strings.Repeat("─", dashes/2) + label + strings.Repeat("─", dashes-dashes/2))
	}
	code := r.Code
	if r.Suspended {
		code = "⊘" + code
	}
	num := func(v string) string { return uikit.PadLeft(uikit.ClipTail(uikit.GroupThousands(v), numW), numW) }
	line := uikit.PadRight(uikit.ClipTail(code, codeW), codeW) + " " + num(r.Avail) + "  " + num(r.Value)
	switch {
	case selected:
		return uikit.StyActive.Render(line)
	case !r.Held:
		return uikit.StyDim.Render(line)
	}
	return line
}

// footer is the position indicator: selected/total over the projected rows
// (dividers excluded from the count).
func footer(k Key, d Data) string {
	total, pos, seen := 0, 0, 0
	for i, r := range d.Rows {
		if r.Divider {
			continue
		}
		total++
		seen++
		if i == k.SelIdx {
			pos = seen
		}
	}
	if total == 0 {
		return ""
	}
	if pos == 0 {
		pos = 1
	}
	return fmt.Sprintf("%d/%d", pos, total)
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
