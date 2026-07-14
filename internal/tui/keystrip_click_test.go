// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/korbit-official/korbit-cli/internal/candles"
	"github.com/korbit-official/korbit-cli/internal/tui/components/keystrip"
)

// footerCapX returns the screen column of the footer key cap that presses keyStr.
func footerCapX(m model, keyStr string) (int, bool) {
	for _, h := range m.cFooter.Hits(m.footerKey()) {
		if h.Send.String() == keyStr {
			return h.X, true // footer line starts at screen column 0
		}
	}
	return 0, false
}

// Clicking a footer key cap behaves exactly like pressing that key.
func TestFooterCapClickDispatchesKey(t *testing.T) {
	cases := []struct {
		key  string
		want func(model) bool
		desc string
	}{
		{"X", func(m model) bool { return m.mode == modeCancelAll }, "cancel-all dialog"},
		{"?", func(m model) bool { return m.mode == modeHelp }, "help overlay"},
		{"n", func(m model) bool { return m.mode == modeNotices }, "notices overlay"},
		{"tab", func(m model) bool { return m.focus == focusOrders }, "focus advances"},
	}
	for _, c := range cases {
		m := testModel(t, true, &fakeTrader{})
		x, ok := footerCapX(m, c.key)
		if !ok {
			t.Fatalf("no footer cap for %q", c.key)
		}
		m = send(t, m, mclick(x, m.h-1))
		if !c.want(m) {
			t.Errorf("clicking the %q cap should open the %s", c.key, c.desc)
		}
	}
}

// A click strictly between two caps (the gap) presses nothing.
func TestFooterGapClickIsNoop(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	x, ok := footerCapX(m, "X")
	if !ok {
		t.Fatal("expected an X cap")
	}
	// One column left of the X cap is the separator before it — not a hit.
	m = send(t, m, mclick(x-1, m.h-1))
	if m.mode != modeNormal {
		t.Errorf("a click in the gap before a cap should do nothing, got mode %v", m.mode)
	}
}

// A paired cap (↑/↓) presses its own real key: clicking ↓ moves the active market
// exactly like pressing the Down arrow.
func TestFooterPairedCapClickEqualsKeypress(t *testing.T) {
	clicked := testModel(t, true, &fakeTrader{}) // markets focus by default
	x, ok := footerCapX(clicked, "down")
	if !ok {
		t.Fatal("markets focus should advertise a ↓ cap")
	}
	clicked = send(t, clicked, mclick(x, clicked.h-1))

	pressed := testModel(t, true, &fakeTrader{})
	pressed = send(t, pressed, special(tea.KeyDown))

	if clicked.symbol() != pressed.symbol() {
		t.Errorf("clicking ↓ (%q) ≠ pressing Down (%q)", clicked.symbol(), pressed.symbol())
	}
	if clicked.symbol() == testModel(t, true, &fakeTrader{}).symbol() {
		t.Error("Down should have moved the active market off its initial symbol")
	}
}

// Clicking the close cap in a modal overlay's hint dismisses it — proving the
// centered-overlay hit geometry lines up with what is drawn.
func TestOverlayHintCapClickCloses(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('X', "X")) // open the cancel-all dialog
	if m.mode != modeCancelAll {
		t.Fatalf("pressing X should open cancel-all, got mode %v", m.mode)
	}
	parts := m.cancelAllParts()
	x, y, ok := overlayCapScreen(m, parts, "esc")
	if !ok {
		t.Fatal("the cancel-all hint should expose an esc cap")
	}
	m = send(t, m, mclick(x, y))
	if m.mode != modeNormal {
		t.Errorf("clicking the dialog's esc cap should close it, got mode %v", m.mode)
	}
}

// Clicking the footer's esc cap while the order panel is open closes it — in
// order mode the footer strip stays live (it is not an overlay).
func TestOrderModeFooterCapClickCloses(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, k('b', "b"))
	if m.mode != modeOrder {
		t.Fatalf("pressing b should open order mode, got mode %v", m.mode)
	}
	x, ok := footerCapX(m, "esc")
	if !ok {
		t.Fatal("the order-mode footer should expose an esc cap")
	}
	m = send(t, m, mclick(x, m.h-1))
	if m.mode != modeNormal {
		t.Errorf("clicking the footer esc cap should close order mode, got mode %v", m.mode)
	}
}

// The footer advertises the command bar (`::cmd`) alongside the other
// order-entry caps, and clicking the ":" cap opens the bar.
func TestFooterCmdBarCapClickOpens(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	x, ok := footerCapX(m, ":")
	if !ok {
		t.Fatal("the footer should expose a : cap when order entry is enabled")
	}
	m = send(t, m, mclick(x, m.h-1))
	if !m.cmdBarVisible() {
		t.Error("clicking the : cap should open the command bar")
	}
}

// Clicking a cap on the chart overlay's hint dispatches its key: the esc cap
// closes the chart. Exercises the chart help-row geometry and a paired strip.
func TestChartHintCapClickCloses(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m.cfg.Candles = func(string, string, int, int64) ([]candles.Bar, error) { return nil, nil }
	m = send(t, m, k('g', "g")) // open the chart overlay
	if m.mode != modeChart {
		t.Fatalf("pressing g should open the chart, got mode %v", m.mode)
	}
	col, row := m.chartHelpScreen()
	_, hits := chartHelpLine()
	x, ok := capX(hits, "esc")
	if !ok {
		t.Fatal("chart hint should expose an esc cap")
	}
	m = send(t, m, mclick(col+x, row))
	if m.mode != modeNormal {
		t.Errorf("clicking the chart's esc cap should close it, got mode %v", m.mode)
	}
}

// TestCtrlCQuitsInEveryMode pins the one claim the overlay-mode footer makes:
// ctrl+c quits regardless of which surface owns the keyboard. handleKey applies
// it before any mode dispatch, so the "ctrl+c quits" reminder is verified, not
// asserted on faith.
func TestCtrlCQuitsInEveryMode(t *testing.T) {
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	if ctrlC.String() != "ctrl+c" {
		t.Fatalf("ctrl+c synthesizes to %q, want ctrl+c", ctrlC.String())
	}
	modes := []struct {
		name  string
		setup func(*testing.T) model
	}{
		{"normal", func(t *testing.T) model { return testModel(t, true, &fakeTrader{}) }},
		{"searching", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			return send(t, m, k('/', "/")) // quick search owns the keyboard
		}},
		{"order", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			return send(t, m, k('b', "b"))
		}},
		{"ladder", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			return send(t, m, k('t', "t"))
		}},
		{"confirm", func(t *testing.T) model {
			m := seedOpenOrder(t, testModel(t, true, &fakeTrader{}))
			m = send(t, m, special(tea.KeyTab)) // focus the open-orders pane
			return send(t, m, k('x', "x"))      // confirm-cancel the selected order
		}},
		{"cancelAll", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			return send(t, m, k('X', "X"))
		}},
		{"help", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			return send(t, m, k('?', "?"))
		}},
		{"notices", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			return send(t, m, k('n', "n"))
		}},
		{"chart", func(t *testing.T) model {
			m := chartTestModel(t, 120, 32)
			return send(t, m, k('g', "g"))
		}},
		{"quitConfirm", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			m.orderInFlight = true
			return send(t, m, k('q', "q"))
		}},
		{"accountSwitch", func(t *testing.T) model {
			return send(t, multiAccountModel(t, 1, 2, 3), k('@', "@"))
		}},
	}
	for _, mc := range modes {
		m := mc.setup(t)
		// Guard against a vacuous pass: ctrl+c quits from modeNormal too, so a setup
		// that silently failed to enter its mode would pass without testing anything.
		switch mc.name {
		case "normal":
		case "searching":
			if !m.searching {
				t.Fatalf("%s setup did not enter search", mc.name)
			}
		default:
			if m.mode == modeNormal {
				t.Fatalf("%s setup did not open its overlay", mc.name)
			}
		}
		_, cmd := m.Update(ctrlC)
		if cmd == nil || cmd() != (tea.QuitMsg{}) {
			t.Errorf("%s mode: ctrl+c must quit (cmd=%v)", mc.name, cmd)
		}
	}
}

// TestEscClosesEveryOverlay pins the other half of the overlay-footer reminder:
// esc backs out of every overlay (returns to the normal view). Keeps the
// "esc to close overlay" prose honest as overlays are added/changed.
func TestEscClosesEveryOverlay(t *testing.T) {
	overlays := []struct {
		name  string
		setup func(*testing.T) model
	}{
		{"order", func(t *testing.T) model {
			return send(t, testModel(t, true, &fakeTrader{}), k('b', "b"))
		}},
		{"ladder", func(t *testing.T) model {
			return send(t, testModel(t, true, &fakeTrader{}), k('t', "t"))
		}},
		{"confirm", func(t *testing.T) model {
			m := seedOpenOrder(t, testModel(t, true, &fakeTrader{}))
			m = send(t, m, special(tea.KeyTab)) // focus open orders
			return send(t, m, k('x', "x"))
		}},
		// The idle cancel-all dialog closes on esc. The one overlay state where esc
		// does NOT immediately return to normal is a *running* batch (esc aborts,
		// then closes once the in-flight cancel lands) — that path is pinned by
		// TestCancelAllAbort, and the footer's "esc to close overlay" prose covers it
		// as a graceful back-out (the in-box hint shows the precise "esc: stop").
		{"cancelAll", func(t *testing.T) model {
			return send(t, testModel(t, true, &fakeTrader{}), k('X', "X"))
		}},
		{"help", func(t *testing.T) model {
			return send(t, testModel(t, true, &fakeTrader{}), k('?', "?"))
		}},
		{"notices", func(t *testing.T) model {
			return send(t, testModel(t, true, &fakeTrader{}), k('n', "n"))
		}},
		{"chart", func(t *testing.T) model {
			return send(t, chartTestModel(t, 120, 32), k('g', "g"))
		}},
		{"quitConfirm", func(t *testing.T) model {
			m := testModel(t, true, &fakeTrader{})
			m.orderInFlight = true
			return send(t, m, k('q', "q"))
		}},
		{"accountSwitch", func(t *testing.T) model {
			return send(t, multiAccountModel(t, 1, 2, 3), k('@', "@"))
		}},
	}
	for _, o := range overlays {
		m := o.setup(t)
		if m.mode == modeNormal {
			t.Fatalf("%s setup did not open an overlay", o.name)
		}
		m = send(t, m, special(tea.KeyEscape))
		if m.mode != modeNormal {
			t.Errorf("%s: esc must return to the normal view, mode = %v", o.name, m.mode)
		}
	}
}

// capX returns the relative column of the hit pressing keyStr.
func capX(hits []keystrip.Hit, keyStr string) (int, bool) {
	for _, h := range hits {
		if h.Send.String() == keyStr {
			return h.X, true
		}
	}
	return 0, false
}

// overlayCapScreen returns the screen position of the cap pressing keyStr within a
// modal overlay's hint lines.
func overlayCapScreen(m model, parts overlayParts, keyStr string) (x, y int, ok bool) {
	for _, cl := range parts.clicks {
		col, row := m.overlayLineOrigin(parts.lines, cl.idx)
		if rx, found := capX(cl.hits, keyStr); found {
			return col + rx, row, true
		}
	}
	return 0, 0, false
}
