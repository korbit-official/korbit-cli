// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package keystrip

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// pressString round-trips a key string through Press and back via String, the
// contract the strips rely on (a built button dispatches the key its handler
// matches).
func TestPressRoundTrips(t *testing.T) {
	for _, s := range []string{"x", "X", "esc", "tab", "shift+tab", "enter", "left", "right", "up", "down", "pgup", "pgdown", "home", "end", "[", "]", "+", "-", "/", "=", "?", "n", "g", "G", "a", "o", "v", "i", "C", "y"} {
		if got := Press(s).String(); got != s {
			t.Errorf("Press(%q).String() = %q, want %q", s, got, s)
		}
	}
}

func TestSingleKeyForm(t *testing.T) {
	line, hits := Layout([]Item{One("x", "cancel")}, 80)
	if w := lipgloss.Width(line); w != lipgloss.Width("x:cancel") {
		t.Errorf("single-key token width = %d, want %d (%q)", w, lipgloss.Width("x:cancel"), line)
	}
	if len(hits) != 1 || hits[0].X != 0 || hits[0].W != 1 {
		t.Fatalf("want one hit at X=0 W=1, got %+v", hits)
	}
	if got := hits[0].Send.String(); got != "x" {
		t.Errorf("hit sends %q, want x", got)
	}
}

func TestProseTokenHasNoHit(t *testing.T) {
	_, hits := Layout([]Item{Prose("[orders]")}, 80)
	if len(hits) != 0 {
		t.Errorf("a prose token must produce no hit, got %+v", hits)
	}
}

// A paired command renders as two adjacent, independently-clickable caps that
// each press their own (real) key — the load-bearing design point.
func TestPairedKeysAreTwoButtons(t *testing.T) {
	line, hits := Layout([]Item{Multi("interval", Btn("[", "["), Btn("]", "]"))}, 80)
	if len(hits) != 2 {
		t.Fatalf("paired token wants 2 hits, got %d (%q)", len(hits), line)
	}
	// The label follows the cap group with the same ":" as a single-key token, so
	// the strip reads uniformly (caps, then ":", then label).
	if got := ansi.Strip(line); got != "[ ]:interval" {
		t.Errorf("paired token = %q, want %q", got, "[ ]:interval")
	}
	if hits[0].Send.String() != "[" || hits[1].Send.String() != "]" {
		t.Errorf("paired caps send %q,%q; want [,]", hits[0].Send.String(), hits[1].Send.String())
	}
	// The two caps are adjacent, separated by one space: [ at col 0, ] at col 2.
	if hits[0].X != 0 || hits[1].X != 2 {
		t.Errorf("cap columns = %d,%d; want 0,2", hits[0].X, hits[1].X)
	}
	if _, ok := At(hits, 1); ok {
		t.Error("the space between caps should not be a hit")
	}
}

// Arrow glyphs differ from the keys they press.
func TestArrowGlyphPressesArrowKey(t *testing.T) {
	_, hits := Layout([]Item{Multi("select", Btn("←", "left"), Btn("→", "right"))}, 80)
	if len(hits) != 2 || hits[0].Send.String() != "left" || hits[1].Send.String() != "right" {
		t.Fatalf("arrow caps should press left/right, got %+v", hits)
	}
}

// A second token starts after the previous label plus the two-space separator.
func TestHitOffsetsAcrossTokens(t *testing.T) {
	hitsLine, hits := Layout([]Item{One("x", "cancel"), One("g", "chart")}, 80)
	// "x:cancel" is 8 cells, "  " is 2 ⇒ g cap at col 10.
	if len(hits) != 2 || hits[1].X != 10 {
		t.Fatalf("second token cap col = wrong: %+v (%q)", hits, hitsLine)
	}
	send, ok := At(hits, 10)
	if !ok || send.String() != "g" {
		t.Errorf("click at col 10 should press g, got %q ok=%v", send.String(), ok)
	}
}

// A cap past the width budget is dropped from the hitmap so a click can't land on
// a clipped/absent cell.
func TestTruncationDropsOffscreenHits(t *testing.T) {
	items := []Item{One("x", "cancel"), One("g", "chart")}
	// Budget fits only the first token ("x:cancel" = 8).
	line, hits := Layout(items, 8)
	if w := lipgloss.Width(line); w > 8 {
		t.Errorf("line width %d exceeds budget 8: %q", w, line)
	}
	for _, h := range hits {
		if h.X+h.W > 8 {
			t.Errorf("hit %+v extends past the width budget", h)
		}
	}
	if _, ok := At(hits, 10); ok {
		t.Error("a clipped cap must not be clickable")
	}
}

// Sanity: a synthesized press routes the same as a typed key would (String match
// is what every handler switches on).
func TestPressMatchesTypedKey(t *testing.T) {
	typed := tea.KeyPressMsg{Code: 'x', Text: "x"}
	if Press("x").String() != typed.String() {
		t.Error("synthetic and typed x should compare equal by String")
	}
}
