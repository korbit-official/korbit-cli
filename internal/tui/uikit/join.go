// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import "strings"

// Col is one fixed-width column block for [HJoin]. The contract HJoin trusts:
// every line of Text is exactly W DISPLAY COLUMNS wide — terminal cells, the
// East-Asian-width- and grapheme-aware measure, not bytes or runes — and W is
// that width. HJoin never re-measures, so a line that is not exactly W columns
// shifts every column to its right, silently, on that row. The TUI panes satisfy
// this because they are rendered through a width-padded lipgloss border, which
// pads every line to an exact column width (full-width and multi-codepoint
// content included); any new caller must guarantee the same.
type Col struct {
	Text string
	W    int
}

// HJoin lays fixed-size column blocks side by side into an h-row result: row r
// is Col0[r] + Col1[r] + … . It does NO display-width measurement — it trusts
// each block to be an exact rectangle per the [Col] contract — so rows
// concatenate directly. Because it only ever joins whole lines and never splits
// or measures inside one, full-width East Asian characters and multi-codepoint
// grapheme clusters within a line pass through untouched and stay aligned
// exactly as the renderer left them. A block with fewer than h rows has its
// missing rows filled with W spaces (bottom-pad, matching lipgloss Top
// alignment); a taller block is truncated to h. For inputs that honor the [Col]
// contract and are tab/CR-free, the output is byte-identical to
// lipgloss.JoinHorizontal(lipgloss.Top, …), at a fraction of the cost because
// the per-line width scan is skipped.
func HJoin(h int, cols ...Col) string {
	switch len(cols) {
	case 0:
		return ""
	case 1:
		return cols[0].Text
	}
	lines := make([][]string, len(cols))
	for i, c := range cols {
		lines[i] = strings.Split(c.Text, "\n")
	}
	var b strings.Builder
	for r := 0; r < h; r++ {
		if r > 0 {
			b.WriteByte('\n')
		}
		for i, c := range cols {
			if r < len(lines[i]) {
				b.WriteString(lines[i][r])
			} else {
				b.WriteString(strings.Repeat(" ", c.W)) // pad a short block's missing row
			}
		}
	}
	return b.String()
}

// VJoin stacks blocks vertically (a "\n" between each). It is pure
// concatenation, so it is content-agnostic — but callers must pass blocks of one
// shared display width, since a VJoin result is itself fed to [HJoin] as a [Col]
// (a ragged stack would then misalign). Under that contract, and tab/CR-free,
// the output is byte-identical to lipgloss.JoinVertical(lipgloss.Left, …)
// without the max-width scan.
func VJoin(blocks ...string) string { return strings.Join(blocks, "\n") }
