// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Shared chrome styles. These deliberately use the 16 basic ANSI colors so they
// read on any terminal theme; the semantic up/down colors are NOT here (they
// depend on the color scheme + terminal profile — see [Palette]).
var (
	StyTitle  = lipgloss.NewStyle().Bold(true)
	StyDim    = lipgloss.NewStyle().Faint(true)
	StyWarn   = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	StyErr    = lipgloss.NewStyle().Foreground(lipgloss.Red).Bold(true)
	StyOK     = lipgloss.NewStyle().Foreground(lipgloss.Green)
	StyActive = lipgloss.NewStyle().Reverse(true).Bold(true)
	// StyCanceling marks an open-orders row whose cancel is in flight: gray
	// (basic-ANSI bright black) so it reads as "leaving". It replaces the old
	// text status column.
	StyCanceling     = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	StyBorder        = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.BrightBlack)
	StyBorderFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Cyan)
	StyFocusLbl      = lipgloss.NewStyle().Reverse(true)
	// StyKey styles a clickable key-cap glyph in a key-hint strip: underlined and
	// cyan (the focus accent) so it reads as a pressable/clickable affordance,
	// while the surrounding label stays dim. Basic ANSI cyan keeps it legible on
	// any terminal theme (see the note above).
	StyKey = lipgloss.NewStyle().Foreground(lipgloss.Cyan).Underline(true)
)

// BorderFor returns the focused border style when focused, else the normal one.
func BorderFor(focused bool) lipgloss.Style {
	if focused {
		return StyBorderFocused
	}
	return StyBorder
}

// Panel draws a bordered box with a title line and exactly h-3 content lines.
func Panel(title string, lines []string, w, h int) string {
	return PanelStyled(StyBorder, title, lines, w, h)
}

// PanelStyled is [Panel] with an explicit border style (so a focused pane can use
// a highlighted border). Note lipgloss v2 Width/Height include the border frame,
// so the style takes the full panel width while content fits w-2.
func PanelStyled(border lipgloss.Style, title string, lines []string, w, h int) string {
	return panelBody(border, StyTitle.Render(Truncate(title, w-2)), lines, w, h)
}

// panelBody assembles the bordered box from a title line the caller has already
// styled and width-fitted, plus the h-3 content lines. It is the shared core of
// the plain-title panels (which wrap the title in [StyTitle]) and the pre-styled-
// title ones (which style each segment themselves).
func panelBody(border lipgloss.Style, titleLine string, lines []string, w, h int) string {
	innerW, innerH := w-2, h-2
	if innerW < 1 || innerH < 1 {
		return ""
	}
	out := make([]string, 0, innerH)
	out = append(out, titleLine)
	for i := 0; i < innerH-1; i++ {
		if i < len(lines) {
			out = append(out, Truncate(lines[i], innerW))
		} else {
			out = append(out, "")
		}
	}
	return border.Width(w).Render(strings.Join(out, "\n"))
}

// PanelWithFooter is a focusable panel that splices an optional position
// indicator (right) and an optional live search query (left) into the bottom
// border. focused selects the border color; when searching is set the bottom-left
// renders as "/query" — including an empty query, so an active search with no
// text yet still shows a "/" prompt that is distinct from no search.
func PanelWithFooter(focused, searching bool, query, title, footer string, lines []string, w, h int) string {
	return panelWithFooter(focused, searching, query, StyTitle.Render(Truncate(title, w-2)), footer, lines, w, h)
}

// PanelWithFooterStyled is [PanelWithFooter] for a title the caller has already
// styled per-segment. The panel applies no title style of its own: a single
// wrapping style ends at the title's first color reset, so it would drop the
// styling of every segment after it (and the outer style would not resume). The
// caller therefore owns all of the title's styling, bold included; here the title
// is only width-clipped.
func PanelWithFooterStyled(focused, searching bool, query, styledTitle, footer string, lines []string, w, h int) string {
	return panelWithFooter(focused, searching, query, Truncate(styledTitle, w-2), footer, lines, w, h)
}

func panelWithFooter(focused, searching bool, query, titleLine, footer string, lines []string, w, h int) string {
	p := panelBody(BorderFor(focused), titleLine, lines, w, h)
	left := ""
	if searching {
		left = "/" + query
	}
	if footer == "" && left == "" {
		return p
	}
	parts := strings.Split(p, "\n")
	if len(parts) < 2 {
		return p
	}
	parts[len(parts)-1] = BottomBorderLabel(w, left, footer, focused)
	return strings.Join(parts, "\n")
}

// BottomBorderLabel builds a w-wide bottom border with an optional left label
// after the left corner and an optional right label before the right corner:
// "╰ /btc ──────── 12/352 ─╯". Corners/dashes take the border color (cyan when
// focused, else dim); the labels are plain so they stay readable. When the two
// can't both fit, the right label is dropped first, then the left.
func BottomBorderLabel(w int, left, right string, focused bool) string {
	inner := w - 2
	dash := lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	if focused {
		dash = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	}
	if inner < 2 {
		return dash.Render(strings.Repeat("─", max(0, w)))
	}
	l, r := "", ""
	if left != "" {
		l = " " + left + " "
	}
	if right != "" {
		r = " " + right + " "
	}
	if lipgloss.Width(l)+lipgloss.Width(r)+2 > inner {
		r = ""
		if lipgloss.Width(l)+2 > inner {
			l = ""
		}
	}
	mid := inner - 2 - lipgloss.Width(l) - lipgloss.Width(r)
	if mid < 0 {
		mid = 0
	}
	return dash.Render("╰─") + l + dash.Render(strings.Repeat("─", mid)) + r + dash.Render("─╯")
}

// Overlay centers a [StyBorder]-boxed surface in a w×h area.
func Overlay(w, h int, content string) string {
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, StyBorder.Render(content))
}

// Truncate cuts a (possibly styled) line to the given display width, marking
// the cut with a trailing "…" so a reader can always tell content was
// dropped — a silent cut reads as the whole story.
func Truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	if w == 1 {
		return "…"
	}
	return lipgloss.NewStyle().MaxWidth(w-1).Render(s) + "…"
}

// Wrap breaks long plain text (error messages) at word boundaries.
func Wrap(s string, w int) string {
	words := strings.Fields(s)
	var b strings.Builder
	line := 0
	for i, word := range words {
		ww := lipgloss.Width(word)
		if i > 0 {
			if line+1+ww > w {
				b.WriteByte('\n')
				line = 0
			} else {
				b.WriteByte(' ')
				line++
			}
		}
		b.WriteString(word)
		line += ww
	}
	return b.String()
}
