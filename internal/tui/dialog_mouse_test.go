// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// overlayLineScreen returns the screen origin (first content column, row) of the
// first overlay content line containing substr — the anchor a body-region click
// test offsets into.
func overlayLineScreen(m model, parts overlayParts, substr string) (col, row int, ok bool) {
	for i, ln := range parts.lines {
		if strings.Contains(plain(ln), substr) {
			c, r := m.overlayLineOrigin(parts.lines, i)
			return c, r, true
		}
	}
	return 0, 0, false
}

// glyphCol returns the display column of the first occurrence of glyph within the
// plain (ANSI-stripped) line, or -1.
func glyphCol(line, glyph string) int {
	p := plain(line)
	i := strings.Index(p, glyph)
	if i < 0 {
		return -1
	}
	return lipgloss.Width(p[:i])
}

// orderPanelLineScreen returns the screen origin of the first order-panel
// content line containing substr, plus the line's text — the anchor a panel
// click test offsets into.
func orderPanelLineScreen(m model, substr string) (col, row int, line string, ok bool) {
	left, _, rightW := m.orderColumnGeom()
	for i, l := range m.order.panelLines(rightW-2, m.orderGate()) {
		if strings.Contains(plain(l.text), substr) {
			return left + 1, m.bodyTop() + 2 + i, l.text, true
		}
	}
	return 0, 0, "", false
}

// Clicking an order-panel field row moves the field cursor onto it (so ←/→ and
// typing then act on it), exactly like arrowing to it.
func TestOrderPanelFieldClickMovesCursor(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('b', "b")) // open the order panel
	if m.mode != modeOrder {
		t.Fatalf("b should open order mode, got mode %v", m.mode)
	}
	col, row, _, ok := orderPanelLineScreen(m, "tif")
	if !ok {
		t.Fatal("the panel should render a tif row")
	}
	m = send(t, m, mclick(col+2, row))
	if m.order.curField() != ofTIF {
		t.Errorf("clicking the tif row should move the cursor onto it, current = %v", m.order.curField())
	}
}

// Clicking the ‹ / › arrows on a selector row changes its value, identical to
// moving the cursor there and pressing left/right.
func TestOrderPanelToggleArrowClickChangesValue(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('b', "b"))
	if m.order.draft.side != "buy" {
		t.Fatalf("a fresh panel defaults to buy, got %q", m.order.draft.side)
	}
	col, row, line, ok := orderPanelLineScreen(m, "side")
	if !ok {
		t.Fatal("the panel should render a side row")
	}
	m = send(t, m, mclick(col+glyphCol(line, "›"), row))
	if m.order.draft.side != "sell" {
		t.Errorf("clicking › on the side row should flip buy→sell, got %q", m.order.draft.side)
	}
	_, _, line, _ = orderPanelLineScreen(m, "side")
	m = send(t, m, mclick(col+glyphCol(line, "‹"), row))
	if m.order.draft.side != "buy" {
		t.Errorf("clicking ‹ on the side row should flip sell→buy, got %q", m.order.draft.side)
	}
}

// Clicking a %-of-balance chip sizes the draft from the available balance.
func TestOrderPanelPresetChipClick(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m = send(t, m, k('b', "b")) // price seeds from the best bid (100,000,000)
	col, row, line, ok := orderPanelLineScreen(m, "25%")
	if !ok {
		t.Fatal("the panel should render the preset chips")
	}
	m = send(t, m, mclick(col+glyphCol(line, "25%"), row))
	if m.order.draft.qty != "0.0025" { // 25% × 1,000,000 KRW / 100,000,000
		t.Errorf("clicking the 25%% chip should size the qty from balance, got %q", m.order.draft.qty)
	}
}

// Clicking a price level in the orderbook writes that price into the draft —
// the price picker gesture.
func TestOrderBookClickSetsPrice(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m = send(t, m, k('b', "b"))
	// Find the screen row of the second bid level (99,990,000).
	top, h := m.bookPaneGeom()
	rows := m.orderLadderRows()
	if len(rows) == 0 {
		t.Fatal("the ladder should have rows once the book is live")
	}
	target := -1
	for i, p := range rows {
		if p == "99990000" {
			target = i
			break
		}
	}
	if target < 0 || target >= h-3 {
		t.Fatalf("bid level not on a visible row: %d of %d", target, h-3)
	}
	sideW, _, _, _ := m.colWidths()
	m = send(t, m, mclick(sideW+2, top+2+target))
	if m.order.draft.price != "99990000" {
		t.Errorf("clicking a book level should set the draft price, got %q", m.order.draft.price)
	}
	if m.order.cursorPrice != "99990000" {
		t.Errorf("the ladder cursor should follow the click, got %q", m.order.cursorPrice)
	}
}

// A click outside the order panel (e.g. the sidebar) does NOT close order mode —
// leaving it is a deliberate esc, never a mis-click.
func TestOrderModeOutsideClickKeepsOpen(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('b', "b"))
	m = send(t, m, mclick(0, m.bodyTop()+1))
	if m.mode != modeOrder {
		t.Errorf("a click outside the panel must not close order mode, mode = %v", m.mode)
	}
}

// Clicking a cancel-all scope chip selects that scope directly (not just toggles).
func TestCancelAllScopeChipClick(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('X', "X")) // open cancel-all (defaults to the this-pair scope)
	if m.mode != modeCancelAll {
		t.Fatalf("X should open cancel-all, got mode %v", m.mode)
	}
	if m.cancelAll.allPairs {
		t.Fatalf("cancel-all should default to the this-pair scope")
	}
	col, row, ok := overlayLineScreen(m, m.cancelAllParts(), "scope:")
	if !ok {
		t.Fatal("cancel-all should render a scope row")
	}
	line, _ := findOverlayLine(m.cancelAllParts(), "scope:")

	// Click the "all pairs" chip → all-pairs scope.
	m = send(t, m, mclick(col+glyphCol(line, "all pairs"), row))
	if !m.cancelAll.allPairs {
		t.Errorf("clicking the all-pairs chip should select the all-pairs scope")
	}
	// Click the this-pair chip → back to this-pair scope.
	line, _ = findOverlayLine(m.cancelAllParts(), "scope:")
	m = send(t, m, mclick(col+glyphCol(line, "this pair"), row))
	if m.cancelAll.allPairs {
		t.Errorf("clicking the this-pair chip should select the this-pair scope")
	}
}

// findOverlayLine returns the first overlay line containing substr.
func findOverlayLine(parts overlayParts, substr string) (string, bool) {
	for _, ln := range parts.lines {
		if strings.Contains(plain(ln), substr) {
			return ln, true
		}
	}
	return "", false
}

// A left click outside the box dismisses each modal dialog, the same reversible
// gesture as the chart/help/notices overlays.
func TestModalClickOutsideDismisses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T) model
	}{
		{"confirm", func(t *testing.T) model {
			m := seedOpenOrder(t, testModel(t, true, &fakeTrader{}))
			m = send(t, m, special(tea.KeyTab)) // focus open orders
			return send(t, m, k('x', "x"))
		}},
		{"cancelAll", func(t *testing.T) model {
			return send(t, testModel(t, true, &fakeTrader{}), k('X', "X"))
		}},
	}
	for _, c := range cases {
		m := c.setup(t)
		if m.mode == modeNormal {
			t.Fatalf("%s: setup did not open the dialog", c.name)
		}
		m = send(t, m, mclick(0, 0)) // top-left corner: outside the centered box
		if m.mode != modeNormal {
			t.Errorf("%s: a click outside the box should dismiss it, mode = %v", c.name, m.mode)
		}
	}
}

// A click inside the box but not on an interactive region keeps the dialog open
// (the click-outside gesture must not fire from within the frame).
func TestModalClickInsideKeepsOpen(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('X', "X"))
	col, row, ok := overlayLineScreen(m, m.cancelAllParts(), "cancel all open orders")
	if !ok {
		t.Fatal("cancel-all should render a title line")
	}
	m = send(t, m, mclick(col, row)) // click the title (inside, non-interactive)
	if m.mode != modeCancelAll {
		t.Errorf("a click on the dialog's title should keep it open, mode = %v", m.mode)
	}
}

// A running cancel-all batch is NOT aborted by a click outside the box — only its
// explicit esc/stop hint aborts it, so a stray click can't silently kill it.
func TestRunningCancelAllIgnoresOutsideClick(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('X', "X"))
	m.cancelAll.running = true // simulate a batch in flight
	m = send(t, m, mclick(0, 0))
	if m.mode != modeCancelAll {
		t.Errorf("an outside click during a running batch must not close it, mode = %v", m.mode)
	}
}
