// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package keystrip renders the TUI's key-hint strips — the global footer hint
// line and the chart/overlay hint lines — from a single ordered list of [Item]s,
// and returns BOTH the styled line and a click hitmap so the same tokens that
// document a key are also clickable buttons that dispatch that key.
//
// Each clickable key is a [Button]: its Glyph is what is drawn (the literal key,
// so the strip doubles as accurate documentation — `[`, `]`, `←`, `x`, `tab`,
// `esc`), and its Send is the synthetic [tea.KeyPressMsg] dispatched when the cap
// is clicked. The glyph and the dispatched key differ only in presentation (an
// arrow glyph for the Left/Right keys); for everything else the glyph IS the key.
//
// A token renders uniformly as caps, then `:`, then label: one cap is `x:cancel`,
// several are each independently clickable and space-joined before the colon —
// `[ ]:interval`, `← →:select` — so a paired command is two real, separately-
// clickable keys, never one ambiguous control. A token with no buttons is plain
// prose ([Item.Label] only, no hit) — used for mode tags like `[orders]` and
// notes like `(public mode …)`.
//
// [Layout] is the one entry point: it is a pure function of the items and the
// width, returning the styled line and the hits in display cells RELATIVE to the
// strip's first column. The caller adds the strip's screen origin before matching
// a mouse position. Because the layout depends only on the item list and width,
// the caller can render once (memoized) and recompute the hitmap on demand from
// the same inputs.
package keystrip

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/digitalx-official/digitalx-cli/internal/tui/uikit"
)

// Button is one clickable key cap. Glyph is the drawn key text (e.g. "x", "X",
// "[", "←", "tab", "esc"); Send is the key dispatched when the cap is clicked.
type Button struct {
	Glyph string
	Send  tea.KeyPressMsg
}

// Item is one hint token: zero or more key caps and the dim label they act on.
// Zero buttons means plain prose (the label is drawn dim with no clickable cap).
type Item struct {
	Buttons []Button
	Label   string
}

// Hit is a clickable region on the rendered strip, in display cells RELATIVE to
// the strip's first column: a click whose strip-relative X is in [X, X+W) presses
// Send. The caller adds the strip's screen origin before testing a mouse X.
type Hit struct {
	X, W int
	Send tea.KeyPressMsg
}

// Press builds the synthetic key press for a key string ("x", "X", "esc", "tab",
// "left", "pgup", "[", "+", …) — the inverse of [tea.KeyPressMsg.String] for the
// keys the strips use, so a built Item dispatches exactly the key its handler
// matches on.
func Press(s string) tea.KeyPressMsg {
	switch s {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}
	default:
		if r := []rune(s); len(r) == 1 {
			k := tea.KeyPressMsg{Code: unicode.ToLower(r[0]), Text: s}
			if unicode.IsUpper(r[0]) {
				k.Mod = tea.ModShift
			}
			return k
		}
		return tea.KeyPressMsg{Text: s}
	}
}

// Btn is a key cap whose drawn glyph differs from the key string it dispatches —
// the only case being the arrow keys (glyph "←", key "left"). For a cap whose
// glyph IS its key, build the Button inline or use [One].
func Btn(glyph, key string) Button { return Button{Glyph: glyph, Send: Press(key)} }

// One is a single-key token where the glyph is the key itself (the common case):
// One("x", "cancel") renders "x:cancel" and clicking the "x" presses x.
func One(key, label string) Item {
	return Item{Buttons: []Button{{Glyph: key, Send: Press(key)}}, Label: label}
}

// Multi is a token whose label is acted on by several independently-clickable
// caps: Multi("interval", Btn("[", "["), Btn("]", "]")) renders "[ ]:interval".
func Multi(label string, btns ...Button) Item { return Item{Buttons: btns, Label: label} }

// Prose is a non-clickable token (a mode tag or a note).
func Prose(text string) Item { return Item{Label: text} }

// itemSep separates adjacent tokens on the strip.
const itemSep = "  "

// Layout renders items into a single styled line clipped to w display cells, and
// returns the click hitmap (relative to column 0). A button clipped by the width
// budget is dropped from the hitmap, so a click never lands on a half-rendered or
// off-screen cap.
func Layout(items []Item, w int) (line string, hits []Hit) {
	var b strings.Builder
	col := 0
	emit := func(s string) { b.WriteString(s); col += lipgloss.Width(s) }

	for i, it := range items {
		if i > 0 {
			emit(uikit.StyDim.Render(itemSep))
		}
		for j, btn := range it.Buttons {
			if j > 0 {
				emit(" ")
			}
			gw := lipgloss.Width(btn.Glyph)
			// Drop a cap (and any later one) that would spill past the width budget,
			// so its hit can't point at a clipped cell.
			if col+gw <= w {
				hits = append(hits, Hit{X: col, W: gw, Send: btn.Send})
			}
			emit(uikit.StyKey.Render(btn.Glyph))
		}
		if it.Label != "" {
			// One uniform rule: caps, then ":", then the label — "x:cancel",
			// "[ ]:interval", "← →:select". A token with no caps is plain prose, so
			// it gets no separator.
			sep := ":"
			if len(it.Buttons) == 0 {
				sep = ""
			}
			emit(uikit.StyDim.Render(sep + it.Label))
		}
	}

	// hits already excludes any cap whose end (col+gw) would pass w — done at emit
	// time, when col is the cap's start — so the clipped line and the hitmap agree
	// with no second pass.
	return uikit.Truncate(b.String(), w), hits
}

// At returns the press for the hit covering strip-relative column x (the strip's
// screen origin already removed by the caller), and whether one was found.
func At(hits []Hit, x int) (tea.KeyPressMsg, bool) {
	for _, h := range hits {
		if x >= h.X && x < h.X+h.W {
			return h.Send, true
		}
	}
	return tea.KeyPressMsg{}, false
}
