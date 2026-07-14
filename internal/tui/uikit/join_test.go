// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// rect builds an h-line block whose every line is exactly w display cells of r,
// optionally wrapped in an ANSI style — a perfect rectangle, the contract HJoin
// and VJoin rely on.
func rect(w, h int, r rune, sty *lipgloss.Style) string {
	line := strings.Repeat(string(r), w)
	if sty != nil {
		line = sty.Render(line)
	}
	lines := make([]string, h)
	for i := range lines {
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// block repeats line rows times, joined by newlines — a rectangle whose display
// width is the caller's responsibility to declare (used for wide/combining
// content where display width != rune count).
func block(line string, rows int) string {
	lines := make([]string, rows)
	for i := range lines {
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// TestHJoinMatchesLipgloss: for rectangular column blocks, HJoin is byte-identical
// to lipgloss.JoinHorizontal(Top, …) — including the bottom-pad of a shorter block
// (the pad uses the block's own width, matching Top alignment). Dropping the pad
// would shift later columns left on the short rows and break this. The wide- and
// combining-character cases prove the join stays aligned with content whose
// display width differs from its rune count, because HJoin only ever joins whole
// lines (the renderer, not HJoin, owns within-line width).
func TestHJoinMatchesLipgloss(t *testing.T) {
	red := lipgloss.NewStyle().Foreground(lipgloss.Red)
	// hangul: U+D55C "한" is one rune of two display columns, so 3 of them are a
	// 6-column line. combining: "e" + U+0301 (combining acute) is one display
	// column from two runes, so 4 of them are a 4-column line.
	hangul := strings.Repeat(string(rune(0xD55C)), 3)
	combining := strings.Repeat("e"+string(rune(0x0301)), 4)
	cases := []struct {
		name string
		h    int
		cols []Col
	}{
		{"three equal-height plain", 4, []Col{{rect(6, 4, 'a', nil), 6}, {rect(3, 4, 'b', nil), 3}, {rect(5, 4, 'c', nil), 5}}},
		{"ansi content", 4, []Col{{rect(6, 4, 'a', &red), 6}, {rect(3, 4, 'b', nil), 3}}},
		{"two columns", 3, []Col{{rect(10, 3, 'x', nil), 10}, {rect(7, 3, 'y', nil), 7}}},
		{"short block bottom-padded", 4, []Col{{rect(6, 4, 'a', nil), 6}, {rect(3, 2, 'b', nil), 3}}},
		{"full-width east asian", 3, []Col{{block(hangul, 2), 6}, {rect(4, 3, 'b', nil), 4}}},
		{"combining-mark graphemes", 2, []Col{{block(combining, 2), 4}, {rect(3, 2, 'b', nil), 3}}},
	}
	for _, c := range cases {
		texts := make([]string, len(c.cols))
		for i, col := range c.cols {
			texts[i] = col.Text
		}
		want := lipgloss.JoinHorizontal(lipgloss.Top, texts...)
		if got := HJoin(c.h, c.cols...); got != want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, want)
		}
	}
}

// TestVJoinMatchesLipgloss: for equal-width blocks, VJoin equals
// lipgloss.JoinVertical(Left, …).
func TestVJoinMatchesLipgloss(t *testing.T) {
	want := lipgloss.JoinVertical(lipgloss.Left, rect(8, 2, 'a', nil), rect(8, 3, 'b', nil))
	if got := VJoin(rect(8, 2, 'a', nil), rect(8, 3, 'b', nil)); got != want {
		t.Errorf("VJoin != lipgloss\n got %q\nwant %q", got, want)
	}
}

// TestHJoinEdges covers the trivial arities.
func TestHJoinEdges(t *testing.T) {
	if got := HJoin(3); got != "" {
		t.Errorf("no cols: got %q, want empty", got)
	}
	if got := HJoin(3, Col{rect(4, 3, 'a', nil), 4}); got != rect(4, 3, 'a', nil) {
		t.Errorf("single col must pass through unchanged, got %q", got)
	}
}
