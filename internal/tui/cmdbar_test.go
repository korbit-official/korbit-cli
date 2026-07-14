// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

func TestParseOrderCmd(t *testing.T) {
	cases := []struct {
		in   string
		sym  string // "" defaults to btc_krw
		want cmdParse
		err  string // substring of the expected error; "" = ok
	}{
		{in: "b 0.05 @ 163480000", want: cmdParse{side: "buy", qty: "0.05", price: "163480000"}},
		{in: "buy 0.05 @163480000 gtc", want: cmdParse{side: "buy", qty: "0.05", price: "163480000", tif: "gtc"}},
		{in: "b 0.05 btc @ 163480000", want: cmdParse{side: "buy", qty: "0.05", price: "163480000"}}, // explicit base unit
		{in: "s 25% @ a", want: cmdParse{side: "sell", pct: 25, anchor: "ask"}},
		{in: "b 0.1 @ b-2", want: cmdParse{side: "buy", qty: "0.1", anchor: "bid", anchorTicks: -2}},
		{in: "s 0.05 @ m+5 po", want: cmdParse{side: "sell", qty: "0.05", anchor: "mid", anchorTicks: 5, tif: "po"}},
		{in: "b 0.05 @ 50m", want: cmdParse{side: "buy", qty: "0.05", price: "50000000"}},              // k/m multiplies the price too
		{in: "b 500k krw @ mkt", want: cmdParse{side: "buy", market: true, amt: "500000"}},             // quote-ccy unit → amount
		{in: "b 1.5m krw @ mkt", want: cmdParse{side: "buy", market: true, amt: "1500000"}},            // k/m on an amount
		{in: "b 10000 krw @ 163480000", want: cmdParse{side: "buy", amt: "10000", price: "163480000"}}, // limit amount (qty derived at resolve)
		{in: "b 0.5 krw @ mkt", want: cmdParse{side: "buy", market: true, amt: "0.5"}},                 // fractional KRW is valid — no per-currency precision assumption
		{in: "b 0.123456789 btc @ 1", want: cmdParse{side: "buy", qty: "0.12345678", price: "1"}},      // literal qty floored to the 8dp request precision
		{in: "s 0.05 @ mkt ioc", want: cmdParse{side: "sell", market: true, qty: "0.05", tif: "ioc"}},
		{in: "s 500k @ mkt", want: cmdParse{side: "sell", market: true, qty: "500000"}}, // bare number is a quantity, valid market sell
		{in: "b 10% @ last", want: cmdParse{side: "buy", pct: 10, anchor: "last"}},

		{in: "", err: "empty"},
		{in: "x 1 @ 1", err: "start with"},
		{in: "b", err: "missing size"},
		{in: "b nope @ 1", err: "bad size"},
		{in: "b 150% @ 1", err: "percent"},
		{in: "b 0.05", err: "missing @"},
		{in: "b 0.05 @", err: "missing the price"},
		{in: "b 0.05 @ zzz", err: "bad price"},
		{in: "b 0.05 @ 1 gtc extra", err: "unexpected"},
		{in: "b 0.05 eth @ 1", err: "after the size"},           // eth is neither base nor quote of btc_krw
		{in: "b 0.000000001 krw @ mkt", err: "too small"},       // amount below the 8dp request precision rounds to zero
		{in: "b 0.05 @ mkt", err: "market buy is sized in KRW"}, // bare number is a qty; a market buy needs an amount
		{in: "s 500k krw @ mkt", err: "market sell is sized"},   // a KRW amount can't size a market sell
		{in: "b 500k krw @ mkt gtc", err: "only ioc"},
	}
	for _, c := range cases {
		sym := c.sym
		if sym == "" {
			sym = "btc_krw"
		}
		got, err := parseOrderCmd(c.in, sym)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("parse(%q): err = %v, want substring %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parse(%q):\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
}

// cmdBarModelWithBands is a private-mode test model with the tick policy
// wired, the market seeded, and the bar's metadata fetch already landed.
func cmdBarModelWithBands(t *testing.T, tr Trader) model {
	t.Helper()
	m := newModel(Config{
		Symbols:     []string{"btc_krw", "eth_krw"},
		Private:     true,
		Trader:      tr,
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{Bands: []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m2, cmd := sendC(t, m, k(':', ":"))
	m = drainCmds(t, m2, cmd) // lands the tick-policy fetch
	if !m.cmdbar.active {
		t.Fatal("':' must open the command bar")
	}
	return m
}

// TestCmdBarEchoNarrowKeepsSafetyFacts: on a narrow bar the echo drops whole
// derived facts (fee first) and discloses the drops — the warning code and
// the book state must survive at ANY width, never sit on a clipped tail.
func TestCmdBarEchoNarrowKeepsSafetyFacts(t *testing.T) {
	m := cmdBarModelWithBands(t, &fakeTrader{})
	// A whale's KRW: "b 100% @ mkt" resolves to a market buy whose notional
	// breaches the 1B cap and sweeps far past the seeded book.
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 101, "/v2/balance",
		`[{"currency":"krw","balance":"9000000000","available":"9000000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	m = typeText(t, m, "b 100% @ mkt")

	_, wide := m.renderCmdBarLines(200)
	for _, want := range []string{"⚠ INSUFFICIENT_LIQUIDITY", "book ●", "= 9,000,000,000 KRW", "pp"} {
		if !strings.Contains(plain(wide), want) {
			t.Fatalf("a roomy echo should carry every fact, missing %q: %q", want, plain(wide))
		}
	}
	_, narrow := m.renderCmdBarLines(80)
	got := plain(narrow)
	if lipgloss.Width(narrow) > 80 {
		t.Fatalf("the echo must fit its width, got %d: %q", lipgloss.Width(narrow), got)
	}
	for _, want := range []string{"⚠ INSUFFICIENT_LIQUIDITY", "book ●", "BUY 9,000,000,000 KRW"} {
		if !strings.Contains(got, want) {
			t.Errorf("a narrow echo must keep the safety facts, missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "= 9,000,000,000") {
		t.Errorf("a narrow echo should drop the derived notional before any safety fact: %q", got)
	}
	if !strings.Contains(got, "+") {
		t.Errorf("dropped facts must be disclosed with a +n tail: %q", got)
	}
}

// TestCmdBarPlacesAnchoredOrder: the golden path — type, review (the anchored
// price frozen on the tick grid), place; the trader sees the resolved wire
// form and the bar stays open with the line in history.
func TestCmdBarPlacesAnchoredOrder(t *testing.T) {
	tr := &fakeTrader{result: PlaceResult{OrderID: "31337", ClientOrderID: "cid-9"}}
	m := cmdBarModelWithBands(t, tr)

	m = typeText(t, m, "b 0.05 @ b-2")
	m, _ = press(t, m, special(tea.KeyEnter)) // arm
	if !m.cmdbar.armed {
		t.Fatalf("enter must arm, err=%q", m.cmdbar.errText)
	}
	if m.cmdbar.armedDraft.price != "99998000" { // best bid 100,000,000 − 2×1000
		t.Fatalf("anchored price must resolve on the grid, got %q", m.cmdbar.armedDraft.price)
	}
	m, cmd := press(t, m, special(tea.KeyEnter)) // place
	if cmd == nil || !m.orderInFlight {
		t.Fatal("the confirmed review must dispatch under the in-flight gate")
	}
	msg := cmd()
	want := OrderForm{Symbol: "btc_krw", Side: "buy", Type: "limit", Price: "99998000", Qty: "0.05", TIF: "gtc", AccountSeq: 1}
	assertPlacedForm(t, tr.tickets[0], want)
	mm, _ := m.Update(msg)
	m = mm.(model)
	if !m.cmdbar.active || m.cmdbar.input.Value() != "" {
		t.Fatal("after placing, the bar stays open with a cleared line")
	}
	if len(m.cmdbar.history) != 1 || m.cmdbar.history[0] != "b 0.05 @ b-2" {
		t.Fatalf("the placed line must land in history: %v", m.cmdbar.history)
	}
	if m.flashOrderID != 31337 {
		t.Fatalf("the accepted order should flash, got %d", m.flashOrderID)
	}
	if m.order.formErr != "" {
		t.Fatalf("a bar-placed order must not touch the panel's inline state: %q", m.order.formErr)
	}
}

// TestCmdBarLimitAmountDerivesQty: a limit order sized by a KRW amount places
// the quantity derived at the resolved price (the API has no limit-amount
// field), so `b 10m krw @ 100000000` buys 0.1 BTC.
func TestCmdBarLimitAmountDerivesQty(t *testing.T) {
	tr := &fakeTrader{}
	m := cmdBarModelWithBands(t, tr)

	m = typeText(t, m, "b 10m krw @ 100000000")
	m, _ = press(t, m, special(tea.KeyEnter))
	if !m.cmdbar.armed {
		t.Fatalf("a limit amount must arm, err=%q", m.cmdbar.errText)
	}
	m, cmd := press(t, m, special(tea.KeyEnter))
	cmd()
	// 10,000,000 KRW ÷ 100,000,000 = 0.1 BTC; a limit order carries qty, not amt.
	want := OrderForm{Symbol: "btc_krw", Side: "buy", Type: "limit", Price: "100000000", Qty: "0.1", TIF: "gtc", AccountSeq: 1}
	assertPlacedForm(t, tr.tickets[0], want)
}

// TestCmdBarMarketBuyAndPercent: KRW-unit market buys and percent sizing
// resolve through the same engine as the panel presets.
func TestCmdBarMarketBuyAndPercent(t *testing.T) {
	tr := &fakeTrader{}
	m := cmdBarModelWithBands(t, tr)

	m = typeText(t, m, "b 500k krw @ mkt")
	m, _ = press(t, m, special(tea.KeyEnter))
	m, cmd := press(t, m, special(tea.KeyEnter))
	cmd()
	want := OrderForm{Symbol: "btc_krw", Side: "buy", Type: "market", Amt: "500000", PP: true, TIF: "ioc", AccountSeq: 1}
	assertPlacedForm(t, tr.tickets[0], want)

	// Percent sell: 25% of 0.5 BTC.
	mm, _ := m.Update(placeDoneMsg{res: PlaceResult{OrderID: "1", ClientOrderID: "c"}})
	m = mm.(model)
	m = typeText(t, m, "s 25% @ a")
	m, _ = press(t, m, special(tea.KeyEnter))
	m, cmd = press(t, m, special(tea.KeyEnter))
	cmd()
	want = OrderForm{Symbol: "btc_krw", Side: "sell", Type: "limit", Price: "100010000", Qty: "0.125", TIF: "gtc", AccountSeq: 1}
	assertPlacedForm(t, tr.tickets[1], want)
}

// TestCmdBarOffGridPriceRejected: a typed price off the tick grid is rejected
// (checked, never silently snapped); an on-grid price arms.
func TestCmdBarOffGridPriceRejected(t *testing.T) {
	m := cmdBarModelWithBands(t, &fakeTrader{}) // tick 1000
	m = typeText(t, m, "b 0.05 @ 100000500")    // 100,000,500 is off the 1000 grid
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.cmdbar.armed {
		t.Fatal("an off-grid typed price must not arm")
	}
	if !strings.Contains(m.cmdbar.errText, "off the tick grid") {
		t.Fatalf("the off-grid price must be reported: %q", m.cmdbar.errText)
	}

	on := cmdBarModelWithBands(t, &fakeTrader{})
	on = typeText(t, on, "b 0.05 @ 100000000") // on the 1000 grid
	on, _ = press(t, on, special(tea.KeyEnter))
	if !on.cmdbar.armed {
		t.Fatalf("an on-grid price must arm, err=%q", on.cmdbar.errText)
	}
}

// TestCmdBarFreezeAndDrift: an explicitly typed price is frozen through book
// moves; an anchored price disarms when its anchor drifts past the tolerance.
func TestCmdBarFreezeAndDrift(t *testing.T) {
	tr := &fakeTrader{}
	m := cmdBarModelWithBands(t, tr)

	// Explicit price: the book moving between arm and place changes nothing.
	m = typeText(t, m, "b 0.01 @ 99000000")
	m, _ = press(t, m, special(tea.KeyEnter))
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 102, "", `{
		"data":{"timestamp":100,
		"asks":[{"price":"120010000","qty":"1"}],
		"bids":[{"price":"120000000","qty":"1"}]}}`))
	m, cmd := press(t, m, special(tea.KeyEnter))
	if cmd == nil {
		t.Fatalf("an explicit price must place through a book move, err=%q", m.cmdbar.errText)
	}
	cmd()
	if tr.tickets[0].Price != "99000000" {
		t.Fatalf("the frozen price must go on the wire, got %q", tr.tickets[0].Price)
	}
	mm, _ := m.Update(placeDoneMsg{res: PlaceResult{OrderID: "1", ClientOrderID: "c"}})
	m = mm.(model)

	// Anchored price: arm at the (new) best bid, then move the book far away —
	// placing must refuse and disarm.
	m = typeText(t, m, "b 0.01 @ b")
	m, _ = press(t, m, special(tea.KeyEnter))
	if !m.cmdbar.armed || m.cmdbar.armedDraft.price != "120000000" {
		t.Fatalf("arm at the live bid, got %q (err %q)", m.cmdbar.armedDraft.price, m.cmdbar.errText)
	}
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 103, "", `{
		"data":{"timestamp":101,
		"asks":[{"price":"120510000","qty":"1"}],
		"bids":[{"price":"120500000","qty":"1"}]}}`))
	m, cmd = press(t, m, special(tea.KeyEnter))
	if cmd != nil || m.cmdbar.armed {
		t.Fatal("a drifted anchor must refuse to place and disarm")
	}
	if !strings.Contains(m.cmdbar.errText, "market moved") {
		t.Fatalf("the drift must be explained: %q", m.cmdbar.errText)
	}
	if len(tr.tickets) != 1 {
		t.Fatalf("no second order may have been dispatched: %v", tr.tickets)
	}
}

// TestCmdBarGates: opening respects the public-mode gate; arming
// respects the freshness gate; esc backs out of the review then the bar.
func TestCmdBarGates(t *testing.T) {
	// Public mode: ':' toasts and stays closed.
	m := testModel(t, false, nil)
	m, _ = press(t, m, k(':', ":"))
	if m.cmdbar.active {
		t.Fatal("public mode must not open the command bar")
	}
	if !strings.Contains(m.toast.text, "public mode") {
		t.Fatalf("the user must be told why: %q", m.toast.text)
	}

	// No live book: arming is refused with the gate reason.
	m = testModel(t, true, &fakeTrader{})
	m, _ = press(t, m, k(':', ":"))
	m = typeText(t, m, "b 0.01 @ 99000000")
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.cmdbar.armed {
		t.Fatal("arming against a not-ready book must be refused")
	}
	if !strings.Contains(m.cmdbar.errText, "orderbook is not live") {
		t.Fatalf("the gate reason must show on the echo line: %q", m.cmdbar.errText)
	}

	// esc: review → edit → closed.
	m = seedOrderMarket(t, m)
	m = typeText(t, m, "") // no-op; the line still holds the order
	m, _ = press(t, m, special(tea.KeyEnter))
	if !m.cmdbar.armed {
		t.Fatalf("expected to arm once the book is live, err=%q", m.cmdbar.errText)
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if !m.cmdbar.active || m.cmdbar.armed {
		t.Fatal("esc from the review must return to editing")
	}
	if m.cmdbar.input.Value() != "b 0.01 @ 99000000" {
		t.Fatalf("backing out must keep the line, got %q", m.cmdbar.input.Value())
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.cmdbar.active {
		t.Fatal("esc from editing must close the bar")
	}
}

// TestCmdBarHistoryRecall: ↑ recalls, ↓ returns to the fresh line; recalled
// lines re-resolve (anchors are live until armed).
func TestCmdBarHistoryRecall(t *testing.T) {
	tr := &fakeTrader{}
	m := cmdBarModelWithBands(t, tr)
	m.cmdbar.history = []string{"b 0.01 @ b", "s 0.02 @ a"}

	m, _ = press(t, m, special(tea.KeyUp))
	if m.cmdbar.input.Value() != "s 0.02 @ a" {
		t.Fatalf("↑ must recall the latest line, got %q", m.cmdbar.input.Value())
	}
	m, _ = press(t, m, special(tea.KeyUp))
	if m.cmdbar.input.Value() != "b 0.01 @ b" {
		t.Fatalf("↑↑ must recall the older line, got %q", m.cmdbar.input.Value())
	}
	m, _ = press(t, m, special(tea.KeyDown), special(tea.KeyDown))
	if m.cmdbar.input.Value() != "" {
		t.Fatalf("↓ past the newest line must return to a fresh one, got %q", m.cmdbar.input.Value())
	}
}

// TestCmdBarBackspaceOnEmptyCloses: backspace on an empty line closes the bar
// (the shared "delete past the start to back out" gesture); with text left it
// just edits.
func TestCmdBarBackspaceOnEmptyCloses(t *testing.T) {
	m := cmdBarModelWithBands(t, &fakeTrader{})
	m = typeText(t, m, "b")
	m, _ = press(t, m, special(tea.KeyBackspace)) // deletes the "b"
	if !m.cmdbar.active || m.cmdbar.input.Value() != "" {
		t.Fatalf("backspace with text must edit, active=%v value=%q", m.cmdbar.active, m.cmdbar.input.Value())
	}
	m, _ = press(t, m, special(tea.KeyBackspace)) // empty → close
	if m.cmdbar.active {
		t.Fatal("backspace on an empty bar must close it")
	}
}

// TestCmdBarRejectionToasts: a bar-placed rejection reports via toast (there
// is no panel to land on) and the in-flight gate clears.
func TestCmdBarRejectionToasts(t *testing.T) {
	tr := &fakeTrader{placeErr: &output.ApiError{Message: "insufficient balance", Code: "NOT_ENOUGH_BALANCE", HTTPStatus: 400}}
	m := cmdBarModelWithBands(t, tr)
	m = typeText(t, m, "b 0.01 @ b")
	m, _ = press(t, m, special(tea.KeyEnter))
	m, cmd := press(t, m, special(tea.KeyEnter))
	mm, _ := m.Update(cmd())
	m = mm.(model)
	if !m.toast.isError || !strings.Contains(m.toast.text, "NOT_ENOUGH_BALANCE") {
		t.Fatalf("the rejection must toast: %+v", m.toast)
	}
	if m.orderInFlight {
		t.Fatal("the in-flight gate must clear")
	}
}

// TestCmdBarShrinksBody: while the bar is open the body yields two rows and
// the frame still fills the terminal exactly.
func TestCmdBarShrinksBody(t *testing.T) {
	m := cmdBarModelWithBands(t, &fakeTrader{})
	if got := strings.Count(m.render(), "\n") + 1; got != 32 {
		t.Fatalf("frame height with the bar open = %d lines, want 32", got)
	}
	if m.bodyHeight() != 32-4-2 {
		t.Fatalf("bodyHeight with the bar open = %d, want %d", m.bodyHeight(), 32-4-2)
	}
	out := plain(m.render())
	if !strings.Contains(out, "enter:review") && !strings.Contains(out, "b|s <qty") {
		t.Fatal("the echo/hint line should render")
	}
}
