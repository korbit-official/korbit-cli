// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// The trade ladder ('t', modeLadder): open/close, the size-preset → cursor →
// arm → confirm → place loop, market orders, cancel-at-row, tick nudges, and
// the gates — all driven through the model like a user would.

// testOrderValueBounds stands in for the pair listing: it publishes the bounds
// the below-min / above-max warnings check an order value against, in the quote
// currency the symbol names.
func testOrderValueBounds(symbol string) (ops.OrderValueBounds, error) {
	return ops.OrderValueBounds{
		QuoteCurrency: ops.QuoteOf(symbol),
		Min:           "5000",
		Max:           "1000000000",
	}, nil
}

// ladderTestModel is a private-mode model with the tick policy wired and the
// market seeded, sitting in ladder mode with the metadata fetch landed.
func ladderTestModel(t *testing.T, tr Trader) model {
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
		OrderValueBounds: testOrderValueBounds,
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m2, cmd := sendC(t, m, k('t', "t"))
	m = drainCmds(t, m2, cmd) // lands the tick-policy fetch
	if m.mode != modeLadder {
		t.Fatal("'t' must open the trade ladder")
	}
	return m
}

// A failing pair-listing read must not cost the KRW market its bound warnings.
// The seam is best-effort: it hands back what a listing-less resolution yields
// (the documented KRW figures) ALONGSIDE the error, and applyBounds keeps them
// while clearing the retry guard. Before this, an error dropped the bounds
// entirely and the ladder/order panel showed no ⚠ <min for an order that
// `order place --dry-run` warns about against the very same failure.
func TestBoundsSurviveAFailedListingFetch(t *testing.T) {
	failing := func(symbol string) (ops.OrderValueBounds, error) {
		return ops.ResolveBoundsForSymbol(nil, symbol), errors.New("network down")
	}
	o := newOrderModel(nil, 1, nil, failing, nil, nil)
	b, err := failing("btc_krw")
	o = o.applyBounds(orderBoundsMsg{symbol: "btc_krw", bounds: b, err: err})

	if got := o.boundsFor("btc_krw"); got.Min != "5000" || got.QuoteCurrency != "krw" {
		t.Fatalf("a failed fetch must keep the fallback bounds, got %+v", got)
	}
	if o.boundsReq["btc_krw"] {
		t.Error("the retry guard must be cleared so the next trigger refetches")
	}
	// A later successful fetch replaces the fallback with the pair's own figures.
	o = o.applyBounds(orderBoundsMsg{symbol: "btc_krw", bounds: ops.OrderValueBounds{
		QuoteCurrency: "krw", Min: "7000", Max: "900000000",
	}})
	if got := o.boundsFor("btc_krw"); got.Min != "7000" {
		t.Fatalf("a landed fetch must win over the fallback, got %+v", got)
	}
	// And a later failure must not overwrite what already landed.
	b2, err2 := failing("btc_krw")
	o = o.applyBounds(orderBoundsMsg{symbol: "btc_krw", bounds: b2, err: err2})
	if got := o.boundsFor("btc_krw"); got.Min != "7000" {
		t.Fatalf("a failed refetch must not clobber landed bounds, got %+v", got)
	}
}

// TestLadderGates: without a Trader (public mode), 't' only toasts and the
// ladder never opens. With a Trader wired it opens (no experimental opt-in).
func TestLadderGates(t *testing.T) {
	m := testModel(t, false, nil) // public
	m, _ = press(t, m, k('t', "t"))
	if m.mode != modeNormal {
		t.Fatalf("'t' must not open the ladder in public mode, mode=%v", m.mode)
	}
	if !strings.Contains(plain(m.render()), "public mode") {
		t.Fatal("a public-mode 't' should explain via a toast")
	}
}

// TestLadderOpenSeedsCursorAndRenders: opening lands the cursor on the best
// bid and the ladder renders the book's levels with the mid rule.
func TestLadderOpenSeedsCursorAndRenders(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	if m.ladder.cursorPrice != "100000000" {
		t.Fatalf("the cursor should seed at the best bid, got %q", m.ladder.cursorPrice)
	}
	out := plain(m.render())
	for _, want := range []string{"trade ladder — BTC/KRW", "100,010,000", "100,000,000", "press 1-4"} {
		if !strings.Contains(out, want) {
			t.Errorf("ladder render should contain %q", want)
		}
	}
}

// TestLadderCursorReconcilesOnBookChurn: the cursor pins to a PRICE, so a level
// churning in or out elsewhere leaves it put — but when its OWN price leaves the
// book (a live-mirror pair moves the whole book between key presses) it must
// snap to the nearest surviving row, never strand on a nonexistent level (no
// marker rendered, and an arm/estimate resolving against a stale price).
// Regression for the live-book ladder cursor vanishing.
func TestLadderCursorReconcilesOnBookChurn(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	if m.ladder.cursorPrice != "100000000" {
		t.Fatalf("precondition: cursor at best bid, got %q", m.ladder.cursorPrice)
	}

	// A book update that still carries 100000000 leaves the pin exactly where it
	// is — a new level appearing above must not drag the cursor.
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 200, "", `{
		"data":{"timestamp":200,
		"asks":[{"price":"100010000","qty":"1"},{"price":"100020000","qty":"2"}],
		"bids":[{"price":"100005000","qty":"1"},{"price":"100000000","qty":"2"},{"price":"99990000","qty":"1"}]}}`))
	if m.ladder.cursorPrice != "100000000" {
		t.Fatalf("cursor must stay on its price while it still has a row, got %q", m.ladder.cursorPrice)
	}

	// Now the book shifts and 100000000 is gone: the cursor must snap to the
	// nearest surviving row (the next price at or below it), staying visible.
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 300, "", `{
		"data":{"timestamp":300,
		"asks":[{"price":"100010000","qty":"1"},{"price":"100020000","qty":"2"}],
		"bids":[{"price":"100002000","qty":"1"},{"price":"99998000","qty":"2"},{"price":"99990000","qty":"1"}]}}`))
	if m.ladder.cursorPrice != "99998000" {
		t.Fatalf("cursor must snap to the nearest live row when its price leaves the book, got %q", m.ladder.cursorPrice)
	}
	if !strings.Contains(plain(m.render()), "▸") {
		t.Fatal("the cursor marker must render after reconciling onto a live row")
	}
}

// TestLadderPlaceLoop is the golden path: 2 (25% size) → j (one row down) →
// b (arm a limit at the cursor) → enter (place). The trader sees the resolved
// wire form; the accept disarms back to browsing with the flash set.
func TestLadderPlaceLoop(t *testing.T) {
	tr := &fakeTrader{result: PlaceResult{OrderID: "91442", ClientOrderID: "cid-1"}}
	m := ladderTestModel(t, tr)

	m, _ = press(t, m, k('2', "2")) // size 25%
	if m.ladder.sizePct() != 25 {
		t.Fatalf("2 should arm the 25%% preset, got %d", m.ladder.sizePct())
	}
	m, _ = press(t, m, k('j', "j")) // one row down: 99,990,000
	if m.ladder.cursorPrice != "99990000" {
		t.Fatalf("j should walk one row down, got %q", m.ladder.cursorPrice)
	}
	m, _ = press(t, m, k('b', "b")) // arm a buy at the cursor
	if m.ladder.view != ladderConfirm || m.ladder.armed.kind != armPlace {
		t.Fatalf("b should arm a place, view=%v err=%q", m.ladder.view, m.ladder.stripErr)
	}
	d := m.ladder.armed.draft
	if d.side != "buy" || d.typ != "limit" || d.price != "99990000" {
		t.Fatalf("armed draft should be a limit buy at the cursor, got %+v", d)
	}
	// 25% of 1,000,000 KRW at 99,990,000 (no fees wired: no headroom).
	if d.qty != "0.00250025" {
		t.Fatalf("the preset should size the qty at the cursor price, got %q", d.qty)
	}
	out := plain(m.render())
	if !strings.Contains(out, "CONFIRM") || !strings.Contains(out, "99,990,000") {
		t.Errorf("the confirm strip should echo the resolved order: %q", out)
	}

	m, cmd := press(t, m, special(tea.KeyEnter))
	if cmd == nil || !m.orderInFlight || m.ladder.view != ladderBusy {
		t.Fatal("enter must dispatch under the in-flight gate and show busy")
	}
	msg := cmd()
	if len(tr.tickets) != 1 {
		t.Fatalf("the trader should see exactly one order, got %d", len(tr.tickets))
	}
	form := tr.tickets[0]
	if form.Symbol != "btc_krw" || form.Side != "buy" || form.Type != "limit" ||
		form.Price != "99990000" || form.Qty != "0.00250025" {
		t.Fatalf("wire form mismatch: %+v", form)
	}
	mm, _ := m.Update(msg)
	m = mm.(model)
	if m.ladder.view != ladderBrowse || m.ladder.armed.kind != armNone {
		t.Fatalf("an accept should disarm back to browsing, view=%v", m.ladder.view)
	}
	if m.orderInFlight || m.flashOrderID != 91442 {
		t.Fatalf("the accept should clear the gate and flash the new order, flash=%d", m.flashOrderID)
	}
}

// TestConfirmRendersExactWireAmount: the armed review is the final confirmation
// of what is sent, so the size/price WIRE values render exact (GroupExact) — a
// large-magnitude fractional amount is shown to the last digit, not capped the
// way the live echo groups it for reading. Locks the ladder strip and the
// command-bar echo to the order panel's byte-exact confirm rule; the advisory
// notional stays grouped.
func TestConfirmRendersExactWireAmount(t *testing.T) {
	// >8 integer digits + a 2-digit fraction trips capFraction: GroupThousands
	// renders "123,456,789.5…", only GroupExact keeps every digit.
	const amt = "123456789.55"
	const want = "123,456,789.55"
	d := orderDraft{symbol: "btc_krw", side: "buy", typ: "market", amt: amt, pp: true}

	// Ladder confirm strip.
	lm := ladderTestModel(t, &fakeTrader{})
	lm.ladder.armed = ladderArmed{kind: armPlace, draft: d}
	lm.ladder.view = ladderConfirm
	strip := plain(strings.Join(lm.ladder.confirmStripLines(200, "", lm.ladderBands(), lm.ladderBounds(), lm.ladderFees()), "\n"))
	if !strings.Contains(strip, want) {
		t.Errorf("ladder confirm must show the exact wire amount %q:\n%q", want, strip)
	}

	// Command bar: exact in the armed review, grouped in the live echo.
	cm := cmdBarModelWithBands(t, &fakeTrader{})
	if armed := plain(cm.cmdEcho(d, "", true, 200)); !strings.Contains(armed, want) {
		t.Errorf("the armed review must show the exact wire amount %q: %q", want, armed)
	}
	if live := plain(cm.cmdEcho(d, "", false, 200)); strings.Contains(live, want) {
		t.Errorf("the live echo groups for reading, should not show the exact %q: %q", want, live)
	}
}

// TestConfirmMarketSellNotionalLabeledEstimate: a market sell's KRW notional is
// valued against the live book (qty×bestBid), so the confirm marks it an
// estimate (~), not a committed figure (=). A limit order and a market buy have
// a notional fixed by their frozen fields, so they stay "=".
func TestConfirmMarketSellNotionalLabeledEstimate(t *testing.T) {
	sell := orderDraft{symbol: "btc_krw", side: "sell", typ: "market", qty: "0.5", pp: true}
	if !sell.notionalIsEstimate() {
		t.Error("a market sell notional is a live estimate")
	}
	for _, d := range []orderDraft{
		{symbol: "btc_krw", side: "buy", typ: "market", amt: "250000", pp: true},
		{symbol: "btc_krw", side: "sell", typ: "limit", price: "100000000", qty: "0.5"},
	} {
		if d.notionalIsEstimate() {
			t.Errorf("a %s %s notional is fixed by its frozen fields, not an estimate", d.typ, d.side)
		}
	}

	lm := ladderTestModel(t, &fakeTrader{})
	lm.ladder.view = ladderConfirm
	lm.ladder.armed = ladderArmed{kind: armPlace, draft: sell}
	// bestBid 100,000,000 × 0.5 = 50,000,000 KRW, marked "~ " (the tilde-space is
	// unique to the notional lead; est-fill/fee use "~<digit>").
	strip := plain(strings.Join(lm.ladder.confirmStripLines(200, "", lm.ladderBands(), lm.ladderBounds(), lm.ladderFees()), "\n"))
	if !strings.Contains(strip, "~ 50,000,000") {
		t.Errorf("a market-sell confirm must mark the notional an estimate:\n%q", strip)
	}
}

// TestLadderMarketOrders: B arms a market buy sized in KRW, S a market sell
// sized in base — both with price protection on by default.
func TestLadderMarketOrders(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	m, _ = press(t, m, k('2', "2"), k('B', "B"))
	if m.ladder.view != ladderConfirm {
		t.Fatalf("B should arm a market buy, err=%q", m.ladder.stripErr)
	}
	d := m.ladder.armed.draft
	if d.typ != "market" || d.side != "buy" || d.amt != "250000" || !d.pp {
		t.Fatalf("market buy should size 25%% of KRW with pp on, got %+v", d)
	}
	m, _ = press(t, m, special(tea.KeyEscape), k('S', "S"))
	d = m.ladder.armed.draft
	if d.typ != "market" || d.side != "sell" || d.qty != "0.125" {
		t.Fatalf("market sell should size 25%% of the base balance, got %+v", d)
	}
}

// TestLadderNudgeAndTif: while armed, [ and ] step the frozen price along the
// tick grid ({ } by ten), and t cycles the time-in-force.
func TestLadderNudgeAndTif(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	m, _ = press(t, m, k('1', "1"), k('b', "b"))
	if m.ladder.view != ladderConfirm {
		t.Fatalf("arm failed: %q", m.ladder.stripErr)
	}
	m, _ = press(t, m, k(']', "]"))
	if got := m.ladder.armed.draft.price; got != "100001000" {
		t.Fatalf("] must nudge +1 tick, got %q", got)
	}
	m, _ = press(t, m, k('{', "{"))
	if got := m.ladder.armed.draft.price; got != "99991000" {
		t.Fatalf("{ must nudge -10 ticks, got %q", got)
	}
	m, _ = press(t, m, k('t', "t"))
	if got := m.ladder.armed.draft.tif(); got != "ioc" {
		t.Fatalf("t must cycle the tif off the gtc default to ioc, got %q", got)
	}
}

// TestLadderDefaultTif: t while browsing cycles the ladder's session-wide
// default tif (shown in the title), every limit arm inherits it, and a per-order
// tif change in the confirm strip never moves that default.
func TestLadderDefaultTif(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})

	// Default is gtc, surfaced in the title chip.
	if m.ladder.defaultTifIdx != 0 {
		t.Fatalf("the ladder default should start at gtc, idx=%d", m.ladder.defaultTifIdx)
	}
	if out := plain(m.render()); !strings.Contains(out, "tif") || !strings.Contains(out, "gtc") {
		t.Errorf("the title should show the default tif chip: %q", out)
	}

	// t while browsing cycles the default (gtc → ioc), no order armed.
	m, _ = press(t, m, k('t', "t"))
	if m.ladder.defaultTifIdx != 1 || m.ladder.view != ladderBrowse {
		t.Fatalf("browse t should cycle the default to ioc without arming, idx=%d view=%v",
			m.ladder.defaultTifIdx, m.ladder.view)
	}
	if !strings.Contains(plain(m.render()), "ioc") {
		t.Error("the title chip should reflect the new default tif")
	}

	// A limit arm inherits the default.
	m, _ = press(t, m, k('1', "1"), k('b', "b"))
	if m.ladder.view != ladderConfirm {
		t.Fatalf("arm failed: %q", m.ladder.stripErr)
	}
	if got := m.ladder.armed.draft.tif(); got != "ioc" {
		t.Fatalf("the arm should inherit the ioc default, got %q", got)
	}

	// A confirm-state t change is per-order (ioc → fok) and leaves the default.
	m, _ = press(t, m, k('t', "t"))
	if got := m.ladder.armed.draft.tif(); got != "fok" {
		t.Fatalf("confirm t should cycle this order's tif to fok, got %q", got)
	}
	if m.ladder.defaultTifIdx != 1 {
		t.Fatalf("a confirm tif change must not move the ladder default, idx=%d", m.ladder.defaultTifIdx)
	}

	// Re-arming proves the default is still ioc (the per-order change was local).
	m, _ = press(t, m, special(tea.KeyEscape), k('b', "b"))
	if got := m.ladder.armed.draft.tif(); got != "ioc" {
		t.Fatalf("the next arm should still inherit the ioc default, got %q", got)
	}
}

// TestLadderDefaultTifSurvivesSymbolSwitch: the default tif is session-wide —
// unlike the per-symbol size preset, it carries across a symbol change.
func TestLadderDefaultTifSurvivesSymbolSwitch(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	m, _ = press(t, m, k('t', "t"), k('t', "t")) // gtc → ioc → fok
	if m.ladder.defaultTifIdx != 2 {
		t.Fatalf("two t presses should reach fok, idx=%d", m.ladder.defaultTifIdx)
	}
	reopened := m.ladder.open("eth_krw") // re-entering for another symbol
	if reopened.defaultTifIdx != 2 {
		t.Fatalf("the default tif should survive a symbol switch, idx=%d", reopened.defaultTifIdx)
	}
}

// TestLadderMarketArmIgnoresDefaultTif: the default tif is a limit-order
// preference — a market arm is ioc-only regardless of it, and confirm t cannot
// cycle a market order's tif.
func TestLadderMarketArmIgnoresDefaultTif(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	m, _ = press(t, m, k('t', "t"), k('t', "t")) // default gtc → ioc → fok
	if m.ladder.defaultTifIdx != 2 {
		t.Fatalf("two t presses should reach fok, idx=%d", m.ladder.defaultTifIdx)
	}
	m, _ = press(t, m, k('2', "2"), k('B', "B")) // market buy under a fok default
	if m.ladder.view != ladderConfirm || m.ladder.armed.draft.typ != "market" {
		t.Fatalf("B should arm a market buy, err=%q", m.ladder.stripErr)
	}
	if got := m.ladder.armed.draft.tif(); got != "ioc" {
		t.Fatalf("a market arm must be ioc regardless of the fok default, got %q", got)
	}
	m, _ = press(t, m, k('t', "t")) // confirm t is a no-op on a market (ioc-only)
	if got := m.ladder.armed.draft.tif(); got != "ioc" {
		t.Fatalf("confirm t must not cycle a market order's tif, got %q", got)
	}
}

// TestLadderTitleChipFocusAndArrows: the direct keys (a number, t) set focus to
// the chip they change, left/right steps the focused chip (wrapping/clamping),
// tab does not touch the chips, and vertical arrows stay the ladder cursor.
func TestLadderTitleChipFocusAndArrows(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	if m.ladder.titleFocus != chipSize {
		t.Fatalf("focus should start on the size chip, got %v", m.ladder.titleFocus)
	}
	// right steps the (unset) size onto the first preset; left clamps there
	// (the size selector clamps like the order panel, it does not wrap).
	m, _ = press(t, m, special(tea.KeyRight))
	if m.ladder.sizePct() != 10 {
		t.Fatalf("right on the size chip should land on the first preset (10%%), got %d", m.ladder.sizePct())
	}
	m, _ = press(t, m, special(tea.KeyLeft))
	if m.ladder.sizePct() != 10 {
		t.Fatalf("left should clamp at the first preset (no wrap), got %d", m.ladder.sizePct())
	}
	// right clamps at the last preset (max) instead of wrapping to the first.
	m, _ = press(t, m, special(tea.KeyRight), special(tea.KeyRight), special(tea.KeyRight), special(tea.KeyRight))
	if m.ladder.sizePct() != 100 {
		t.Fatalf("right should climb to and clamp at the last preset (max), got %d", m.ladder.sizePct())
	}
	// t focuses the tif chip (and cycles it once, gtc→ioc); left/right then cycle
	// the default tif and leave the size untouched.
	m, _ = press(t, m, k('t', "t"))
	if m.ladder.titleFocus != chipTif || m.ladder.defaultTifIdx != 1 {
		t.Fatalf("t should focus the tif chip and cycle gtc→ioc, focus=%v idx=%d",
			m.ladder.titleFocus, m.ladder.defaultTifIdx)
	}
	m, _ = press(t, m, special(tea.KeyLeft)) // ioc → gtc
	if m.ladder.defaultTifIdx != 0 || m.ladder.sizePct() != 100 {
		t.Fatalf("left should cycle ioc→gtc without touching size, idx=%d pct=%d",
			m.ladder.defaultTifIdx, m.ladder.sizePct())
	}
	// A number re-focuses the size chip (and sets it).
	m, _ = press(t, m, k('2', "2"))
	if m.ladder.titleFocus != chipSize || m.ladder.sizePct() != 25 {
		t.Fatalf("a number should focus + set the size chip, focus=%v pct=%d",
			m.ladder.titleFocus, m.ladder.sizePct())
	}
	// tab does not move the chip focus (it is free for pane focus); focus stays put.
	m, _ = press(t, m, special(tea.KeyTab))
	if m.ladder.titleFocus != chipSize {
		t.Fatalf("tab must not move the chip focus, got %v", m.ladder.titleFocus)
	}
	// Vertical arrows walk the cursor, not the chips.
	before := m.ladder.sizePct()
	m, _ = press(t, m, special(tea.KeyDown))
	if m.ladder.sizePct() != before {
		t.Fatal("the down arrow must move the cursor, not the size chip")
	}
}

// TestLadderTitleChipClick: clicking a chip's › steps and focuses it; clicking
// its value focuses without stepping. Coordinates come from titleLayout, the
// same source the render draws from.
func TestLadderTitleChipClick(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	left, _ := m.ladderGeom()
	y := m.bodyTop() + 1
	click := func(col int) {
		mm, _ := m.Update(tea.MouseClickMsg{X: left + 1 + col, Y: y, Button: tea.MouseLeft})
		m = mm.(model)
	}

	_, _, tifSpan := m.ladder.titleLayout()
	click(tifSpan.nextX) // › on the tif chip
	if m.ladder.defaultTifIdx != 1 || m.ladder.titleFocus != chipTif {
		t.Fatalf("clicking › on the tif chip should cycle + focus it, idx=%d focus=%v",
			m.ladder.defaultTifIdx, m.ladder.titleFocus)
	}

	_, sizeSpan, _ := m.ladder.titleLayout()
	click(sizeSpan.valX0) // the size value: focus only
	if m.ladder.titleFocus != chipSize || m.ladder.sizePct() != 0 {
		t.Fatalf("clicking the size value should focus without stepping, focus=%v pct=%d",
			m.ladder.titleFocus, m.ladder.sizePct())
	}
	click(sizeSpan.nextX) // › on the size chip: step to the first preset
	if m.ladder.sizePct() != 10 {
		t.Fatalf("clicking › on the size chip should step to the first preset, pct=%d", m.ladder.sizePct())
	}
}

// TestLadderTitleChipSpansMatchGlyphs pins the click-span columns against the
// actually-rendered title glyphs — an independent oracle, so an off-by-one in
// titleLayout's offsets (which render, hit-test, and the click test all share)
// can't slip through. All runes in the title are single width, so a title-column
// index equals a rune index into the ANSI-stripped string.
func TestLadderTitleChipSpansMatchGlyphs(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	m, _ = press(t, m, k('2', "2")) // size 25%, tif stays gtc
	styled, sizeSpan, tifSpan := m.ladder.titleLayout()
	runes := []rune(plain(styled))

	check := func(name string, sp ladderChipSpan, want string) {
		t.Helper()
		if runes[sp.prevX] != '‹' {
			t.Errorf("%s: prevX should sit on ‹, got %q", name, runes[sp.prevX])
		}
		if runes[sp.nextX] != '›' {
			t.Errorf("%s: nextX should sit on ›, got %q", name, runes[sp.nextX])
		}
		if got := string(runes[sp.valX0 : sp.valX1+1]); got != want {
			t.Errorf("%s: value span should cover %q, got %q", name, want, got)
		}
	}
	check("size", sizeSpan, "25%")
	check("tif", tifSpan, "gtc")
}

// TestLadderArmRefusals: arming refuses — with the reason on the strip —
// without a size preset, and a cancel-arm refuses off an own-order row.
func TestLadderArmRefusals(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	m, _ = press(t, m, k('b', "b"))
	if m.ladder.view != ladderBrowse || !strings.Contains(m.ladder.stripErr, "size") {
		t.Fatalf("arming without a size must refuse with the reason, err=%q", m.ladder.stripErr)
	}
	if !strings.Contains(plain(m.render()), "arm a size first") {
		t.Error("the refusal should show on the strip")
	}
	m, _ = press(t, m, k('x', "x"))
	if m.ladder.view != ladderBrowse || !strings.Contains(m.ladder.stripErr, "no resting order") {
		t.Fatalf("x off an own-order row must refuse, err=%q", m.ladder.stripErr)
	}
}

// TestLadderPlaceGateBlocked: with the book not yet live, arming refuses with
// the freshness gate's reason.
func TestLadderPlaceGateBlocked(t *testing.T) {
	m := testModel(t, true, &fakeTrader{}) // no book seeded
	m, _ = press(t, m, k('t', "t"))
	if m.mode != modeLadder {
		t.Fatal("'t' should open the ladder even while the book loads")
	}
	m, _ = press(t, m, k('1', "1"), k('b', "b"))
	if m.ladder.view != ladderBrowse || !strings.Contains(m.ladder.stripErr, "orderbook") {
		t.Fatalf("arming against a non-live book must refuse, err=%q", m.ladder.stripErr)
	}
}

// TestLadderRejectionLandsInline: a rejected placement returns to the confirm
// strip with the error inline and the armed order kept for a nudge-and-retry.
func TestLadderRejectionLandsInline(t *testing.T) {
	tr := &fakeTrader{placeErr: &output.ApiError{Message: "not enough", Code: "NO_BALANCE", HTTPStatus: 400}}
	m := ladderTestModel(t, tr)
	m, _ = press(t, m, k('1', "1"), k('b', "b"))
	m, cmd := press(t, m, special(tea.KeyEnter))
	mm, _ := m.Update(cmd())
	m = mm.(model)
	if m.ladder.view != ladderConfirm || !strings.Contains(m.ladder.stripErr, "NO_BALANCE") {
		t.Fatalf("a rejection should land inline on the armed strip, view=%v err=%q", m.ladder.view, m.ladder.stripErr)
	}
	if m.orderInFlight {
		t.Fatal("the in-flight gate must clear on a rejection")
	}
	if !strings.Contains(plain(m.render()), "NO_BALANCE") {
		t.Error("the inline error should render on the strip")
	}
}

// TestLadderRejectionAfterBusyDismissed: esc hides the busy strip (disarming);
// a rejection landing afterwards shows on the browse foot — never a re-armed
// review of a zero-value draft.
func TestLadderRejectionAfterBusyDismissed(t *testing.T) {
	tr := &fakeTrader{placeErr: &output.ApiError{Message: "not enough", Code: "NO_BALANCE", HTTPStatus: 400}}
	m := ladderTestModel(t, tr)
	m, _ = press(t, m, k('1', "1"), k('b', "b"))
	m, cmd := press(t, m, special(tea.KeyEnter))
	m, _ = press(t, m, special(tea.KeyEscape)) // hide the busy strip
	if m.ladder.view != ladderBrowse {
		t.Fatal("esc should hide the busy strip")
	}
	mm, _ := m.Update(cmd())
	m = mm.(model)
	if m.ladder.view != ladderBrowse || !strings.Contains(m.ladder.stripErr, "NO_BALANCE") {
		t.Fatalf("the late rejection should land on the browse foot, view=%v err=%q", m.ladder.view, m.ladder.stripErr)
	}
}

// TestLadderCancelAtRow: with an own order resting on a level, x at its row
// arms a cancel, enter dispatches it through the Trader, and the result
// returns the strip to browsing.
func TestLadderCancelAtRow(t *testing.T) {
	tr := &fakeTrader{}
	m := ladderTestModel(t, tr)
	m = feed(t, m, dataEvent("myOrder", "btc_krw", stream.OriginBackfill, 100, "/v2/openOrders",
		`[{"orderId":555,"status":"open","side":"buy","orderType":"limit","price":"99990000","qty":"0.2","filledQty":"0","createdAt":900,"clientOrderId":"c5"}]`))

	// The MINE column shows the resting order on its row.
	if out := plain(m.render()); !strings.Contains(out, "0.2 ►") {
		t.Errorf("the resting buy should render in the MINE column: %q", out)
	}

	m, _ = press(t, m, k('j', "j")) // cursor to 99,990,000 (the order's row)
	if m.ladder.cursorPrice != "99990000" {
		t.Fatalf("cursor should be on the order's row, got %q", m.ladder.cursorPrice)
	}
	m, _ = press(t, m, k('x', "x"))
	if m.ladder.view != ladderConfirm || m.ladder.armed.kind != armCancel || m.ladder.armed.cancelID != 555 {
		t.Fatalf("x should arm a cancel of the resting order, got %+v err=%q", m.ladder.armed, m.ladder.stripErr)
	}
	if !strings.Contains(plain(m.render()), "CANCEL") {
		t.Error("the cancel review should show on the strip")
	}

	m, cmd := press(t, m, special(tea.KeyEnter))
	if cmd == nil || !m.orderInFlight || !m.cancelsInFlight[555] {
		t.Fatal("enter must dispatch the cancel under the in-flight gate")
	}
	mm, _ := m.Update(cmd())
	m = mm.(model)
	if len(tr.cancels) != 1 || tr.cancels[0] != [3]string{"btc_krw", "555", "1"} {
		t.Fatalf("the trader should see the cancel, got %v", tr.cancels)
	}
	if m.ladder.view != ladderBrowse || m.orderInFlight {
		t.Fatalf("the result should return the strip to browsing, view=%v", m.ladder.view)
	}
}

// TestLadderGroupedMineAndCancel: with a grouping level active, a resting
// order renders (and cancels) at its BUCKET row — a buy truncates down onto
// the level grid — and the cancel strip still discloses its exact price.
func TestLadderGroupedMineAndCancel(t *testing.T) {
	tr := &fakeTrader{}
	m := ladderTestModel(t, tr)
	m.bookGrp["btc_krw"] = "10000" // as if +/- had regrouped the subscription
	// A resting buy at 99,991,000 — on no book level; its 10,000-bucket is
	// 99,990,000, which IS one.
	m = feed(t, m, dataEvent("myOrder", "btc_krw", stream.OriginBackfill, 100, "/v2/openOrders",
		`[{"orderId":777,"status":"open","side":"buy","orderType":"limit","price":"99991000","qty":"0.2","filledQty":"0","createdAt":900,"clientOrderId":"c7"}]`))

	rows := m.ladderRows()
	found := false
	for _, r := range rows {
		if r.Price == "99990000" && r.MineBuy == "0.2" {
			found = true
		}
		if r.Price == "99991000" {
			t.Fatal("a grouped ladder must not grow a synthetic row for the exact price")
		}
	}
	if !found {
		t.Fatalf("the resting buy should aggregate into its bucket row, rows=%+v", rows)
	}

	m, _ = press(t, m, k('j', "j")) // cursor to 99,990,000 (the bucket row)
	if m.ladder.cursorPrice != "99990000" {
		t.Fatalf("cursor should be on the bucket row, got %q", m.ladder.cursorPrice)
	}
	m, _ = press(t, m, k('x', "x"))
	if m.ladder.view != ladderConfirm || m.ladder.armed.cancelID != 777 {
		t.Fatalf("x at the bucket row should arm the order's cancel, got %+v err=%q", m.ladder.armed, m.ladder.stripErr)
	}
	if out := plain(m.render()); !strings.Contains(out, "99,991,000") {
		t.Error("the cancel strip must disclose the order's exact price")
	}
}

// TestLadderPresetEstimates: while browsing with a preset armed, the foot
// strip resolves it live at the cursor price, per side.
func TestLadderPresetEstimates(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{}) // KRW 1,000,000 · BTC 0.5 · cursor 100,000,000
	m, _ = press(t, m, k('2', "2"))        // 25%
	out := plain(m.render())
	if !strings.Contains(out, "size 25% ≈ buy 0.0025 BTC · sell 0.125 BTC") {
		t.Fatalf("the browse strip should resolve the preset per side at the cursor price: %q", out)
	}
}

// TestLadderPresetBreachWarnsBeforeArm: a preset whose resolved notional the
// server would reject carries its ⚠ on the browse strip — before arming.
func TestLadderPresetBreachWarnsBeforeArm(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	// A whale's KRW (25% ⇒ 2.25B notional > the 1B max) and dust BTC
	// (25% ⇒ 250 KRW notional < the 5,000 min).
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 101, "/v2/balance",
		`[{"currency":"krw","balance":"9000000000","available":"9000000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"0.00001","available":"0.00001","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	m, _ = press(t, m, k('2', "2"))
	out := plain(m.render())
	if !strings.Contains(out, "buy 22.5 BTC ⚠ >max") {
		t.Fatalf("an over-max buy preset must be flagged before arming: %q", out)
	}
	if !strings.Contains(out, "⚠ <min") {
		t.Fatalf("an under-min sell preset must be flagged before arming: %q", out)
	}
}

// TestLadderFootLineDimContinuity: the browse foot line's action-hints tail stays
// dim even past a warn ⚠ pop. Regression guard: wrapping the whole line in one
// StyDim let the warn segment's ANSI reset turn the outer faint off, leaving
// everything after the first ⚠ bright. Each fragment must carry its own style.
func TestLadderFootLineDimContinuity(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	// A whale's KRW so the buy preset breaches the max — the warn is mid-line,
	// with the dim action-hints tail after it.
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 101, "/v2/balance",
		`[{"currency":"krw","balance":"9000000000","available":"9000000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"0.5","available":"0.5","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	m, _ = press(t, m, k('2', "2")) // 25% → over-max buy
	styled := m.ladder.browseFootLine(400, m.ladderBands(), m.ladderBounds(), m.ladderFees(), m.pal)
	if !strings.Contains(styled, uikit.StyWarn.Render(" ⚠ >max")) {
		t.Fatalf("the over-max warn should render as its own warn fragment: %q", styled)
	}
	// The action-hints tail (after the " │ " group boundary) must be emitted as
	// its own dim fragment, not left bright by the preceding warn's reset.
	tail := uikit.StyDim.Render(" │ b/s:limit at cursor · B/S:market · x:cancel at row · t:tif")
	if !strings.Contains(styled, tail) {
		t.Fatalf("the foot-line tail must stay dim past the warn (regression): %q", styled)
	}
}

// TestLadderFootLineSideColors: the per-side estimates render in the book's
// bid/ask colors (buy = Up, sell = Down), so buy vs sell reads by color and the
// live estimates stand out from the dim action hints.
func TestLadderFootLineSideColors(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{}) // KRW 1,000,000 · BTC 0.5 · cursor 100,000,000
	// Force TrueColor so Up/Down carry the explicit RGB foregrounds a real
	// terminal shows (the buy≠sell guard below then has teeth).
	mm, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m = mm.(model)
	m, _ = press(t, m, k('2', "2")) // 25%: neither side breaches, so clean segments
	styled := m.ladder.browseFootLine(400, m.ladderBands(), m.ladderBounds(), m.ladderFees(), m.pal)

	if m.pal.Up.Fg.Render("x") == m.pal.Down.Fg.Render("x") {
		t.Fatal("Up and Down should differ under truecolor — the side test would be vacuous")
	}
	if !strings.Contains(styled, m.pal.Up.Fg.Render("buy 0.0025 BTC")) {
		t.Errorf("the buy estimate should render in the bid (Up) color: %q", styled)
	}
	if !strings.Contains(styled, m.pal.Down.Fg.Render("sell 0.125 BTC")) {
		t.Errorf("the sell estimate should render in the ask (Down) color: %q", styled)
	}

	// The buy=Up / sell=Down mapping holds under the other scheme too (red-blue),
	// where PaletteFor swaps the tints but not the roles.
	m.colorScheme = uikit.ColorSchemeRedBlue
	m.pal = uikit.PaletteFor(m.colorScheme, m.profile)
	styled = m.ladder.browseFootLine(400, m.ladderBands(), m.ladderBounds(), m.ladderFees(), m.pal)
	if m.pal.Up.Fg.Render("x") == m.pal.Down.Fg.Render("x") {
		t.Fatal("Up and Down should differ under red-blue too")
	}
	if !strings.Contains(styled, m.pal.Up.Fg.Render("buy 0.0025 BTC")) {
		t.Errorf("buy should use the Up color under red-blue: %q", styled)
	}
	if !strings.Contains(styled, m.pal.Down.Fg.Render("sell 0.125 BTC")) {
		t.Errorf("sell should use the Down color under red-blue: %q", styled)
	}
}

// TestLadderConfirmWarningsOwnLines: an armed order's preplace warnings each
// get their own confirm-strip line — never the tail of the truncated facts
// line, where a narrow ladder silently clips the safety disclosure.
func TestLadderConfirmWarningsOwnLines(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	// A whale's KRW: the max preset arms a market buy whose ~9B KRW notional
	// breaches the 1B cap and sweeps far past the seeded book.
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 101, "/v2/balance",
		`[{"currency":"krw","balance":"9000000000","available":"9000000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	m, _ = press(t, m, k('4', "4"), k('B', "B"))
	if m.ladder.view != ladderConfirm {
		t.Fatalf("B should arm a market buy, err=%q", m.ladder.stripErr)
	}
	// A width where the facts line alone overflows and truncates.
	var found bool
	for _, ln := range m.ladderStrip(40) {
		p := plain(ln)
		if strings.HasPrefix(p, "⚠ NOTIONAL_ABOVE_MAX") {
			found = true
		}
		if strings.Contains(p, "fee ") && strings.Contains(p, "NOTIONAL_ABOVE_MAX") {
			t.Errorf("warnings must not ride the truncatable facts line: %q", p)
		}
	}
	if !found {
		t.Fatalf("the armed breach must hold its own strip line at narrow widths, lines=%q", m.ladderStrip(40))
	}
	if out := plain(m.render()); !strings.Contains(out, "INSUFFICIENT_LIQUIDITY") {
		t.Errorf("every warning should render, each on its own line: %q", out)
	}
}

// TestLadderSizePresetSticksPerSymbol: the armed size survives leaving and
// re-entering ladder mode for the same symbol (session-sticky, per symbol).
func TestLadderSizePresetSticksPerSymbol(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	m, _ = press(t, m, k('3', "3"))
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal {
		t.Fatal("esc should leave ladder mode")
	}
	m, _ = press(t, m, k('t', "t"))
	if m.ladder.sizePct() != 50 {
		t.Fatalf("the size preset should stick for the session, got %d", m.ladder.sizePct())
	}
}

// TestLadderMouse: a click on a price row moves the cursor; the wheel walks it.
func TestLadderMouse(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	prices := m.ladderPrices()
	row := -1
	for i, p := range prices {
		if p == "100010000" { // best ask's row
			row = i
		}
	}
	if row < 0 {
		t.Fatalf("best ask should be on the ladder: %v", prices)
	}
	left, _ := m.ladderGeom()
	mm, _ := m.Update(tea.MouseClickMsg{X: left + 4, Y: m.bodyTop() + 2 + row, Button: tea.MouseLeft})
	m = mm.(model)
	if m.ladder.cursorPrice != "100010000" {
		t.Fatalf("a click on a price row should move the cursor, got %q", m.ladder.cursorPrice)
	}
	mm, _ = m.Update(tea.MouseWheelMsg{X: left + 4, Y: m.bodyTop() + 2 + row, Button: tea.MouseWheelDown})
	m = mm.(model)
	if m.ladder.cursorPrice != "100000000" {
		t.Fatalf("a wheel notch should walk the cursor one row, got %q", m.ladder.cursorPrice)
	}
}

// TestLadderFooterHints: the footer follows the ladder's views with live hints
// (the docked ladder is not an overlay — its footer keys stay clickable).
func TestLadderFooterHints(t *testing.T) {
	m := ladderTestModel(t, &fakeTrader{})
	if out := plain(m.renderFooter()); !strings.Contains(out, "[ladder]") {
		t.Errorf("browse footer should show the ladder hints: %q", out)
	}
	m, _ = press(t, m, k('1', "1"), k('b', "b"))
	if out := plain(m.renderFooter()); !strings.Contains(out, "PLACE") {
		t.Errorf("confirm footer should show the place hint: %q", out)
	}
}
