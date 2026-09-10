// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package transfers renders the funding screen's history pane: a windowed
// table of deposit or withdrawal records with a dim column header and the
// selected row reverse-highlighted. The column set comes in via [Data], so the
// one component serves crypto and KRW histories in both directions. It is a
// pure view component — [Model.View] is a function of its explicit [Key] and
// [Data], owns no shared state, and caches its render via [uikit.Memo] keyed
// on [Key]. The parent projects the rows and stamps Rev so the Key alone
// decides a cache hit.
package transfers

import (
	"fmt"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/i18n"
	"github.com/digitalx-official/digitalx-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. Rev is the
// parent's stamp for the projected rows; Title is the full panel title
// (direction, currency, count); Cursor/Scroll are the selection and window;
// Loading shows the loading state; Err (non-empty) replaces the rows with the
// fetch error; Count is the row total. Focused selects the border color. W and
// H are the OUTER panel size; Style is the palette identity.
type Key struct {
	Rev     uint64
	Title   string
	Cursor  int
	Scroll  int
	Loading bool
	Err     string
	Count   int
	Focused bool
	W, H    int
	Style   uikit.StyleID
}

// Data is what the component renders on a cache miss: the column titles,
// their proportional weights, and the pre-formatted rows (cells parallel to
// Cols).
type Data struct {
	Cols    []string
	Weights []int
	Rows    [][]string
}

// Model is the transfers component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a transfers component.
func New() *Model { return &Model{} }

// View renders the full bordered pane for k/d, reusing the cached render when
// k is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	w := k.W - 2
	rows := k.H - 3
	lines := tableLines(k, d, w, rows)
	return uikit.PanelWithFooter(k.Focused, false, "", k.Title, footer(k), lines, k.W, k.H)
}

func tableLines(k Key, d Data, w, rows int) []string {
	switch {
	case k.Err != "":
		return []string{uikit.StyErr.Render(uikit.Truncate(i18n.T("load failed: %s", k.Err), w))}
	case k.Loading:
		return []string{uikit.StyDim.Render(i18n.T("loading…"))}
	case len(d.Rows) == 0:
		return []string{uikit.StyDim.Render(i18n.T("no records"))}
	}
	widths := fitWidths(w-(len(d.Cols)-1), d.Weights, 3)
	cell := func(s string, i int) string { return uikit.PadRight(uikit.ClipTail(s, widths[i]), widths[i]) }
	rowText := func(cells []string) string {
		out := make([]string, len(d.Cols))
		for i := range d.Cols {
			out[i] = cell(cells[i], i)
		}
		return strings.Join(out, " ")
	}
	lines := []string{uikit.StyDim.Render(rowText(d.Cols))}
	dataRows := rows - 1
	if dataRows < 1 {
		dataRows = 1
	}
	start := clamp(k.Scroll, 0, max(0, len(d.Rows)-dataRows))
	for r := start; r < len(d.Rows) && len(lines) < rows; r++ {
		line := rowText(d.Rows[r])
		if r == k.Cursor {
			line = uikit.StyActive.Render(line)
		}
		lines = append(lines, line)
	}
	return lines
}

// footer is the position indicator (cursor/total), empty without rows.
func footer(k Key) string {
	if k.Count == 0 || k.Loading || k.Err != "" {
		return ""
	}
	return fmt.Sprintf("%d/%d", clamp(k.Cursor, 0, k.Count-1)+1, k.Count)
}

// fitWidths distributes total across columns in proportion to weights, each at
// least min, never summing past total (so the table can't overflow its panel).
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
