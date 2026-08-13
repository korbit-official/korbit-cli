// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"slices"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
)

// A liquid book around a 10,000,000 mid (tick 1000), and balances funding both
// sides.
var (
	entryBook = state.Orderbook{
		Symbol: "btc_krw",
		Bids:   []state.PriceLevel{{Price: "9999000", Qty: "2"}, {Price: "9990000", Qty: "5"}},
		Asks:   []state.PriceLevel{{Price: "10001000", Qty: "2"}, {Price: "10100000", Qty: "5"}},
	}
	entryBands = []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}
	entryBals  = []state.Balance{
		{Currency: "krw", Available: "1000000"},
		{Currency: "btc", Available: "0.5"},
	}
)

func TestDraftValuesFollowSizingMatrix(t *testing.T) {
	d := newOrderDraft("btc_krw", "buy")
	d.price, d.qty, d.amt = "9990000", "0.01", "500000"

	v := d.values()
	if v["price"] != "9990000" || v["qty"] != "0.01" {
		t.Fatalf("limit values: %v", v)
	}
	if _, ok := v["amt"]; ok {
		t.Fatal("limit draft must not emit amt")
	}
	if _, ok := v["pp"]; ok {
		t.Fatal("pp applies to market orders only")
	}

	d.typ = "market" // market buy: amt only, pp rides along
	v = d.values()
	if _, ok := v["price"]; ok {
		t.Fatal("market draft must not emit price")
	}
	if _, ok := v["qty"]; ok {
		t.Fatal("market buy must not emit qty")
	}
	if v["amt"] != "500000" || v["pp"] != "true" {
		t.Fatalf("market buy values: %v", v)
	}

	d.side = "sell" // market sell: qty only
	v = d.values()
	if v["qty"] != "0.01" {
		t.Fatalf("market sell values: %v", v)
	}
	if _, ok := v["amt"]; ok {
		t.Fatal("market sell must not emit amt")
	}

	f := d.form()
	if f.Qty != "0.01" || f.Amt != "" || f.Price != "" || !f.PP {
		t.Fatalf("market sell form: %+v", f)
	}
}

func TestBuildPreviewRestingLimit(t *testing.T) {
	d := newOrderDraft("btc_krw", "buy")
	d.price, d.qty = "9990000", "0.05"
	p := buildPreview(d, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, nil)
	if !p.OK {
		t.Fatalf("preview not OK: %s", p.Err)
	}
	if p.Sim.Marketable || len(p.Warnings) != 0 {
		t.Fatalf("resting limit: marketable=%v warnings=%v", p.Sim.Marketable, p.Warnings)
	}
	if p.Notional != "499500" {
		t.Fatalf("notional: %s", p.Notional)
	}
	if p.TickSize != "1000" {
		t.Fatalf("tick size: %s", p.TickSize)
	}
	if p.PctFromMid != "-0.1%" {
		t.Fatalf("pct from mid: %s", p.PctFromMid)
	}
	if p.AvailQuote != "1000000" || p.AvailBase != "0.5" || p.BaseCcy != "btc" || p.QuoteCcy != "krw" {
		t.Fatalf("balances: %+v", p)
	}
	if p.FeeEst != "" {
		t.Fatal("no fee rates supplied: FeeEst must be empty")
	}
}

func TestBuildPreviewFeeUsesTakerWhenMarketable(t *testing.T) {
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw"}

	d := newOrderDraft("btc_krw", "buy")
	d.price, d.qty = "9990000", "0.05" // rests → maker
	p := buildPreview(d, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if p.FeeKind != "maker" || p.FeeEst != "500" { // 499500 × 0.001 ≈ 500
		t.Fatalf("maker fee: kind=%s est=%s", p.FeeKind, p.FeeEst)
	}

	d.price = "10001000" // crosses → taker
	p = buildPreview(d, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if !p.Sim.Marketable || p.FeeKind != "taker" {
		t.Fatalf("marketable limit: marketable=%v kind=%s", p.Sim.Marketable, p.FeeKind)
	}
	// The third outcome, on this same two-sided book: a non-crossing ioc/fok limit
	// takes nothing and cannot rest either — it is canceled unfilled and charged
	// nothing, so it gets NO estimate rather than a maker rate it would never pay.
	for _, tif := range []string{"ioc", "fok"} {
		d := newOrderDraft("btc_krw", "buy")
		d.price, d.qty, d.tifIdx = "9990000", "0.05", slices.Index(tifOptions, tif)
		p := buildPreview(d, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
		if p.Sim.Marketable {
			t.Fatalf("%s: a below-market buy takes nothing: %+v", tif, p.Sim)
		}
		if p.FeeEst != "" || p.FeeKind != "" {
			t.Fatalf("%s: an order that expires unfilled pays no fee: kind=%q est=%q", tif, p.FeeKind, p.FeeEst)
		}
	}
}

func TestBuildPreviewNotReady(t *testing.T) {
	d := newOrderDraft("btc_krw", "buy")
	p := buildPreview(d, state.Orderbook{}, state.StatusNotReady, nil, nil, ops.OrderValueBounds{}, nil)
	if p.OK || p.Err == "" {
		t.Fatalf("not-ready book: %+v", p)
	}
}

// hasWarn reports whether the preview raised a given warning code.
func hasWarn(ws []ops.PlaceWarning, code ops.PlaceWarningCode) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

// An empty (live-but-orderless) book previews a limit order as a resting maker,
// through the SAME ops.AnalyzePlace path a two-sided book takes. An order that
// cannot fill there previews too — its notional and balances are still worth
// showing — and says so as an error-styled NO_OPPOSING_LIQUIDITY warning; the
// REFUSAL is the gate's (fillSideRefusal), not the preview's.
func TestBuildPreviewEmptyBook(t *testing.T) {
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw"}
	empty := state.Orderbook{Symbol: "btc_krw"}

	lim := newOrderDraft("btc_krw", "buy")
	lim.price, lim.qty = "9990000", "0.05"
	p := buildPreview(lim, empty, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if !p.OK {
		t.Fatalf("empty-book limit should preview OK: %s", p.Err)
	}
	if p.Notional != "499500" { // 9990000 × 0.05
		t.Fatalf("empty-book limit notional: %s", p.Notional)
	}
	if p.FeeKind != "maker" || p.FeeEst != "500" {
		t.Fatalf("empty-book limit fee: kind=%s est=%s", p.FeeKind, p.FeeEst)
	}
	if p.PctFromMid != "" {
		t.Fatalf("empty book has no mid: PctFromMid=%s", p.PctFromMid)
	}
	if p.TickSize != "1000" { // the tick check needs no book
		t.Fatalf("empty-book limit tick size: %s", p.TickSize)
	}
	if hasWarn(p.Warnings, ops.WarnNoOpposingLiquidity) {
		t.Fatalf("a gtc limit rests as the first maker — not a would-not-execute case: %+v", p.Warnings)
	}
	if g := fillSideRefusal(lim, empty); g != "" {
		t.Fatalf("a resting limit must be placeable on an orderless book, got %q", g)
	}

	// A market order on the same book: the preview runs and states the outcome,
	// the gate is what refuses it, and the warning renders as an error.
	mkt := newOrderDraft("btc_krw", "buy")
	mkt.typ, mkt.amt = "market", "500000"
	q := buildPreview(mkt, empty, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if !q.OK || q.Err != "" {
		t.Fatalf("every live book previews through one path: %+v", q)
	}
	if !hasWarn(q.Warnings, ops.WarnNoOpposingLiquidity) {
		t.Fatalf("empty-book market must warn it would not execute: %+v", q.Warnings)
	}
	// An order that cannot execute is charged nothing — and a market order is never
	// a maker under any book shape, so there is no rate to fall back on either.
	if q.FeeEst != "" || q.FeeKind != "" {
		t.Fatalf("a market order with nothing to fill against must carry no fee estimate: kind=%q est=%q", q.FeeKind, q.FeeEst)
	}
	if !fatalWarnCodes[ops.WarnNoOpposingLiquidity] {
		t.Fatal("NO_OPPOSING_LIQUIDITY must render as an error — the order would not execute")
	}
	if fatalWarnCodes[ops.WarnMidPriceUnavailable] {
		t.Fatal("MID_PRICE_UNAVAILABLE reports an unrun check, not a failed order — it must not render as an error")
	}
	if g := fillSideRefusal(mkt, empty); g == "" {
		t.Fatal("the gate must refuse a market order with no fill side")
	}

	// Only a tif that can REST is placeable: an ioc/fok limit would be accepted
	// and immediately canceled unfilled, so the gate refuses it and the analysis
	// flags it as would-not-execute; po rests like gtc.
	for i, tif := range tifOptions {
		d := newOrderDraft("btc_krw", "buy")
		d.price, d.qty, d.tifIdx = "9990000", "0.05", i
		p := buildPreview(d, empty, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees)
		if !p.OK {
			t.Errorf("empty-book %s limit must still preview: %s", tif, p.Err)
		}
		rests := tif == "gtc" || tif == "po"
		if warned := hasWarn(p.Warnings, ops.WarnNoOpposingLiquidity); warned == rests {
			t.Errorf("empty-book %s limit: NO_OPPOSING_LIQUIDITY=%v, want %v", tif, warned, !rests)
		}
		if allowed := fillSideRefusal(d, empty) == ""; allowed != rests {
			t.Errorf("empty-book %s limit: gate allows=%v, want %v", tif, allowed, rests)
		}
		// The fee follows the same rest test: the first maker is quoted a maker fee,
		// while an ioc/fok limit that can neither fill nor rest is quoted none at all
		// (the notional is known either way, so nothing else suppresses the estimate).
		switch {
		case rests && (p.FeeKind != "maker" || p.FeeEst == ""):
			t.Errorf("empty-book %s limit rests as the first maker: kind=%q est=%q", tif, p.FeeKind, p.FeeEst)
		case !rests && (p.FeeEst != "" || p.FeeKind != ""):
			t.Errorf("a refused %s limit must not estimate a fee: kind=%q est=%q", tif, p.FeeKind, p.FeeEst)
		}
	}

	// The order-value bounds do not need a book, so an orderless book raises the
	// same below-min warning placement would — through the same ops helper. An
	// empty book is where a too-small order is MOST likely (a first maker testing
	// a new listing), so this is the one check that must not go missing.
	small := newOrderDraft("btc_krw", "buy")
	small.price, small.qty = "9990000", "0.0001" // 999 KRW: below the 5,000 minimum
	bounds := ops.OrderValueBounds{QuoteCurrency: "krw", Min: "5000", Max: "1000000000"}
	sp := buildPreview(small, empty, state.StatusEmpty, entryBals, entryBands, bounds, fees)
	if !sp.OK {
		t.Fatalf("a below-min limit still previews (the warning is the signal): %s", sp.Err)
	}
	if !hasWarn(sp.Warnings, ops.WarnNotionalBelowMin) {
		t.Fatalf("empty-book below-min must warn: %+v", sp.Warnings)
	}
	// A pair publishing no bound raises nothing rather than borrowing another's.
	for _, w := range buildPreview(small, empty, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees).Warnings {
		if w.Code == ops.WarnNotionalBelowMin || w.Code == ops.WarnNotionalAboveMax {
			t.Fatalf("no published bound must raise no bound warning: %s", w.Message)
		}
	}

	// The figures must read the same for a pair quoted in a small-magnitude
	// currency: a fractional notional kept, and a fee that whole units would
	// round away to "no fee at all".
	sub := newOrderDraft("eth_btc", "buy")
	sub.price, sub.qty = "0.0345", "2"
	r := buildPreview(sub, state.Orderbook{Symbol: "eth_btc"}, state.StatusEmpty, nil, nil, ops.OrderValueBounds{}, fees)
	if !r.OK {
		t.Fatalf("empty-book limit on a btc-quoted pair: %s", r.Err)
	}
	if r.Notional != "0.069" || r.FeeEst != "0.000069" {
		t.Fatalf("btc-quoted empty-book figures: notional=%q fee=%q", r.Notional, r.FeeEst)
	}
}

// A ONE-SIDED book is not uniformly degraded: the side an order takes from is
// what decides. On a bids-only book a market SELL sweeps normally — real fill
// estimates, a taker fee — while the market BUY beside it has nothing to fill
// against; a best order follows its PEG side, which post-only takes from its own
// queue instead of the opposing one.
func TestBuildPreviewOneSidedBook(t *testing.T) {
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw"}
	bidsOnly := state.Orderbook{Symbol: "btc_krw", Bids: entryBook.Bids} // no asks at all

	// The fill side is populated for a sell: a full sweep with real figures.
	sell := newOrderDraft("btc_krw", "sell")
	sell.typ, sell.qty = "market", "0.05"
	s := buildPreview(sell, bidsOnly, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if !s.OK {
		t.Fatalf("market sell into a bids-only book: %s", s.Err)
	}
	if !s.Sim.Marketable || !s.Sim.FullyFilled {
		t.Fatalf("market sell must sweep the bids: marketable=%v fullyFilled=%v", s.Sim.Marketable, s.Sim.FullyFilled)
	}
	if s.Sim.EstFilledQty != "0.05" || s.Sim.EstAvgFillPrice != "9999000" {
		t.Fatalf("fill estimate: qty=%q avg=%q", s.Sim.EstFilledQty, s.Sim.EstAvgFillPrice)
	}
	if s.Notional != "499950" { // 0.05 × the best bid
		t.Fatalf("market sell notional: %q, want 499950", s.Notional)
	}
	if s.FeeKind != "taker" || s.FeeEst != "1000" { // 499950 × 0.002, to 3 sig figs
		t.Fatalf("market sell fee: kind=%s est=%q", s.FeeKind, s.FeeEst)
	}
	if hasWarn(s.Warnings, ops.WarnNoOpposingLiquidity) {
		t.Fatalf("the sell's fill side is populated — nothing missing: %+v", s.Warnings)
	}
	if s.Sim.BestAsk != "" || s.Sim.Mid != "" {
		t.Fatalf("an absent side reports EMPTY, never a price: ask=%q mid=%q", s.Sim.BestAsk, s.Sim.Mid)
	}
	if g := fillSideRefusal(sell, bidsOnly); g != "" {
		t.Fatalf("a sell into a bids-only book must be placeable, got %q", g)
	}

	// The same book, the other side: nothing to buy from.
	buy := newOrderDraft("btc_krw", "buy")
	buy.typ, buy.amt = "market", "500000"
	b := buildPreview(buy, bidsOnly, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if !b.OK || b.Sim.Marketable {
		t.Fatalf("market buy against no asks: OK=%v marketable=%v err=%q", b.OK, b.Sim.Marketable, b.Err)
	}
	if !hasWarn(b.Warnings, ops.WarnNoOpposingLiquidity) {
		t.Fatalf("market buy against no asks must warn it would not execute: %+v", b.Warnings)
	}

	// The refusal decision table over the same one-sided book. A best order is
	// classified by its PEG side, never by tif alone.
	best := func(side, tif string) orderDraft {
		d := newOrderDraft("btc_krw", side)
		d.typ, d.tifIdx, d.qty, d.amt = "best", slices.Index(tifOptions, tif), "0.05", "500000"
		return d
	}
	lim := func(side, tif string) orderDraft {
		d := newOrderDraft("btc_krw", side)
		d.price, d.qty, d.tifIdx = "9990000", "0.05", slices.Index(tifOptions, tif)
		return d
	}
	mkt := func(side string) orderDraft {
		d := newOrderDraft("btc_krw", side)
		d.typ, d.qty, d.amt = "market", "0.05", "500000"
		return d
	}
	for _, c := range []struct {
		name    string
		d       orderDraft
		allowed bool
	}{
		{"market buy — takes from the empty asks", mkt("buy"), false},
		{"market sell — takes from the populated bids", mkt("sell"), true},
		{"gtc limit buy — rests", lim("buy", "gtc"), true},
		{"po limit buy — rests", lim("buy", "po"), true},
		{"ioc limit buy — cannot rest, cannot fill", lim("buy", "ioc"), false},
		{"fok limit buy — cannot rest, cannot fill", lim("buy", "fok"), false},
		{"ioc limit sell — fills against the bids", lim("sell", "ioc"), true},
		{"fok limit sell — fills against the bids", lim("sell", "fok"), true},
		{"po best buy — pegs to its own queue side, the bids", best("buy", "po"), true},
		{"po best sell — its queue side, the asks, is empty", best("sell", "po"), false},
		{"gtc best buy — pegs to the opposing asks", best("buy", "gtc"), false},
		{"gtc best sell — pegs to the opposing bids", best("sell", "gtc"), true},
	} {
		if allowed := fillSideRefusal(c.d, bidsOnly) == ""; allowed != c.allowed {
			t.Errorf("%s: gate allows=%v, want %v (%q)", c.name, allowed, c.allowed, fillSideRefusal(c.d, bidsOnly))
		}
	}
}

// The fee estimate's whole decision table, keyed on the simulated OUTCOME. The
// three no-fee rows are orders the book makes marketable — or whose tif says they
// would rest — and that the server still executes nothing of, so an estimate keyed
// on either signal alone quotes a charge the account never pays.
func TestBuildPreviewFeeFollowsTheSimulatedOutcome(t *testing.T) {
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw"}
	lim := func(tif, price, qty string) orderDraft {
		d := newOrderDraft("btc_krw", "buy")
		d.price, d.qty, d.tifIdx = price, qty, slices.Index(tifOptions, tif)
		return d
	}
	cases := []struct {
		name     string
		d        orderDraft
		wantKind string // "" = no estimate at all
	}{
		{"a resting gtc limit pays maker", lim("gtc", "9990000", "0.05"), "maker"},
		{"a crossing gtc limit pays taker", lim("gtc", "10001000", "0.05"), "taker"},
		// The crossing portion (qty 2 of 5) is what the server would take, so the
		// order executes and pays taker — the resting remainder does not change that.
		{"a partly filled crossing limit pays taker", lim("gtc", "10001000", "5"), "taker"},
		{"a crossing post-only is rejected and pays nothing", lim("po", "10001000", "1"), ""},
		{"a partially fillable fill-or-kill is killed and pays nothing", lim("fok", "10001000", "5"), ""},
		{"a non-crossing ioc expires and pays nothing", lim("ioc", "9990000", "0.05"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := buildPreview(c.d, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
			if !p.OK || p.Notional == "" {
				t.Fatalf("the notional must be known, so nothing but the outcome suppresses the fee: %+v", p)
			}
			if p.FeeKind != c.wantKind {
				t.Fatalf("fee kind = %q, want %q (outcome %q, marketable=%v): est=%q",
					p.FeeKind, c.wantKind, p.Sim.Outcome, p.Sim.Marketable, p.FeeEst)
			}
			if (p.FeeEst == "") != (c.wantKind == "") || (p.FeeRate == "") != (c.wantKind == "") {
				t.Fatalf("the three fee fields must stand or fall together: kind=%q rate=%q est=%q", p.FeeKind, p.FeeRate, p.FeeEst)
			}
		})
	}
}

// An order whose whole quantity price protection cancels neither fills nor rests,
// so it pays nothing either — the case the draft's tif gets wrong on its own (a
// gtc limit's tif says it would rest, and it never reaches the book).
func TestPreviewFeeSkipsAWhollyProtectionCappedOrder(t *testing.T) {
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw"}
	// A spread wider than the default 5% protection band, so the band excludes even
	// the best ask: mid 10,000,000, buy cap 10,500,000, best ask 11,000,000.
	wide := state.Orderbook{
		Symbol: "btc_krw",
		Bids:   []state.PriceLevel{{Price: "9000000", Qty: "5"}},
		Asks:   []state.PriceLevel{{Price: "11000000", Qty: "5"}},
	}

	// A protected market buy is the shape this panel can compose (price protection
	// rides on market orders here), and it goes through buildPreview end to end.
	mkt := newOrderDraft("btc_krw", "buy")
	mkt.typ, mkt.amt = "market", "500000"
	if !mkt.pp {
		t.Fatal("a market draft carries price protection by default — the case under test")
	}
	q := buildPreview(mkt, wide, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if q.Sim.Outcome != ops.OutcomeNothing {
		t.Fatalf("protection excludes every ask, so nothing executes: outcome=%q %+v", q.Sim.Outcome, q.Sim)
	}
	if q.Notional == "" || q.FeeEst != "" || q.FeeKind != "" {
		t.Fatalf("a wholly capped order pays no fee: notional=%q kind=%q est=%q", q.Notional, q.FeeKind, q.FeeEst)
	}

	// The same outcome on a gtc LIMIT is what the tif alone gets wrong: it says the
	// order would rest, while protection cancels the whole quantity before it can.
	// The panel's own draft never puts --pp on a limit, so drive the analysis
	// directly and hand its verdict to the same fee rule the preview uses.
	sim, _ := ops.AnalyzePlace(map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "11000000",
		"qty": "0.05", "timeInForce": "gtc", "pp": "true", "accountSeq": "1",
	}, opsLevels(wide.Bids), opsLevels(wide.Asks), entryBands, ops.OrderValueBounds{})
	if sim.Outcome != ops.OutcomeNothing {
		t.Fatalf("a wholly capped limit executes nothing: outcome=%q %+v", sim.Outcome, sim)
	}
	gtc := newOrderDraft("btc_krw", "buy")
	gtc.price, gtc.qty = "11000000", "0.05"
	if !gtc.canRest() {
		t.Fatal("the tif says this order would rest — the signal that is wrong here")
	}
	if est, rate, kind := feeEstimate(sim, "550000", fees); est != "" || rate != "" || kind != "" {
		t.Fatalf("a wholly capped limit pays no fee: est=%q rate=%q kind=%q", est, rate, kind)
	}
}

// A taker fee is charged on what the order EXECUTES, so a partial fill must be
// quoted on the executed quote value and never on the full request — the two
// shapes below differ only in what becomes of the remainder (canceled outright,
// or resting to be charged later, if ever). The two cases the rule leaves alone
// are pinned in the same test: a full fill, where the executed value IS the
// notional, and a resting limit, which is quoted maker on the whole order.
func TestPreviewFeeChargesTheExecutedValueOnAPartialFill(t *testing.T) {
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw"}

	// Remainder CANCELED: one ask inside the default 5% protection band (mid
	// 10,000,000 → buy cap 10,500,000) and the next outside it, so a protected
	// market buy takes the first level and price protection cancels the rest.
	trimmed := state.Orderbook{
		Symbol: "btc_krw",
		Bids:   []state.PriceLevel{{Price: "9999000", Qty: "2"}},
		Asks:   []state.PriceLevel{{Price: "10001000", Qty: "1"}, {Price: "11000000", Qty: "5"}},
	}
	mkt := newOrderDraft("btc_krw", "buy")
	mkt.typ, mkt.amt = "market", "20000000"
	q := buildPreview(mkt, trimmed, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if q.Sim.Outcome != ops.OutcomeFills || q.Sim.EstFilledQuote != "10001000" || q.Notional != "20000000" {
		t.Fatalf("want a partial fill of 10,001,000 against a 20,000,000 request: outcome=%q filled=%q notional=%q",
			q.Sim.Outcome, q.Sim.EstFilledQuote, q.Notional)
	}
	// 10,001,000 × 0.002 to 3 sig figs. The full notional would read "40000" — twice
	// a fee the account never pays on the canceled half.
	if q.FeeKind != "taker" || q.FeeEst != "20000" {
		t.Fatalf("a canceled remainder pays nothing: kind=%q est=%q, want taker 20000", q.FeeKind, q.FeeEst)
	}

	// Remainder RESTS: a crossing gtc limit takes the one ask level at its price and
	// rests the rest. The resting part pays a maker fee only if it later fills — a
	// future this simulation does not predict — so it stays out of the figure.
	lim := newOrderDraft("btc_krw", "buy")
	lim.price, lim.qty = "10001000", "5"
	p := buildPreview(lim, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if p.Sim.Outcome != ops.OutcomeFills || p.Sim.EstFilledQuote != "20002000" || p.Sim.EstRemainingQty != "3" || p.Notional != "50005000" {
		t.Fatalf("want 2 of 5 filled with 3 resting: outcome=%q filled=%q remaining=%q notional=%q",
			p.Sim.Outcome, p.Sim.EstFilledQuote, p.Sim.EstRemainingQty, p.Notional)
	}
	// 20,002,000 × 0.002 to 3 sig figs; the full notional would read "100000".
	if p.FeeKind != "taker" || p.FeeEst != "40000" {
		t.Fatalf("only the crossing portion is taken now: kind=%q est=%q, want taker 40000", p.FeeKind, p.FeeEst)
	}

	// Unchanged: a FULL fill, where the executed value is the notional itself.
	sell := newOrderDraft("btc_krw", "sell")
	sell.typ, sell.qty = "market", "0.05"
	s := buildPreview(sell, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if !s.Sim.FullyFilled || s.Sim.EstFilledQuote != s.Notional || s.Notional != "499950" {
		t.Fatalf("a full fill executes its whole notional: fullyFilled=%v filled=%q notional=%q",
			s.Sim.FullyFilled, s.Sim.EstFilledQuote, s.Notional)
	}
	if s.FeeKind != "taker" || s.FeeEst != "1000" { // 499,950 × 0.002
		t.Fatalf("full-fill fee: kind=%q est=%q, want taker 1000", s.FeeKind, s.FeeEst)
	}

	// Unchanged: a non-crossing limit rests in full, executes nothing, and is quoted
	// the maker rate on the whole notional — there is no executed value to charge.
	rest := newOrderDraft("btc_krw", "buy")
	rest.price, rest.qty = "9990000", "0.05"
	r := buildPreview(rest, entryBook, state.StatusPresent, entryBals, entryBands, ops.OrderValueBounds{}, fees)
	if r.Sim.Outcome != ops.OutcomeRests || r.Sim.EstFilledQuote != "" || r.Notional != "499500" {
		t.Fatalf("want a wholly resting limit: outcome=%q filled=%q notional=%q", r.Sim.Outcome, r.Sim.EstFilledQuote, r.Notional)
	}
	if r.FeeKind != "maker" || r.FeeEst != "500" { // 499,500 × 0.001
		t.Fatalf("resting fee: kind=%q est=%q, want maker 500", r.FeeKind, r.FeeEst)
	}
}

func TestApplyPreset(t *testing.T) {
	// Limit buy: 50% of 1,000,000 KRW at 10,000,000 → 0.05 BTC.
	d := newOrderDraft("btc_krw", "buy")
	d.price = "10000000"
	got, reason := applyPreset(d, entryBals, true, nil, 50)
	if reason != "" || got.qty != "0.05" {
		t.Fatalf("limit buy preset: %q reason=%q", got.qty, reason)
	}

	// With a KRW buy fee, the spend reserves the fee headroom.
	fees := &FeeRates{MaxRate: "0.25", BuyFeeCurrency: "krw"} // absurd rate to make the math visible
	got, reason = applyPreset(d, entryBals, true, fees, 50)
	if reason != "" || got.qty != "0.04" { // 500000/1.25/10000000
		t.Fatalf("fee-reserved preset: %q reason=%q", got.qty, reason)
	}
	// A base-currency buy fee reserves nothing.
	fees = &FeeRates{MaxRate: "0.25", BuyFeeCurrency: "btc"}
	got, _ = applyPreset(d, entryBals, true, fees, 50)
	if got.qty != "0.05" {
		t.Fatalf("base-fee preset: %q", got.qty)
	}

	// Market buy sizes the amt directly, truncated to whole KRW.
	d.typ = "market"
	got, reason = applyPreset(d, entryBals, true, nil, 25)
	if reason != "" || got.amt != "250000" {
		t.Fatalf("market buy preset: %q reason=%q", got.amt, reason)
	}

	// Sell: 25% of 0.5 BTC.
	d = newOrderDraft("btc_krw", "sell")
	got, reason = applyPreset(d, entryBals, true, nil, 25)
	if reason != "" || got.qty != "0.125" {
		t.Fatalf("sell preset: %q reason=%q", got.qty, reason)
	}

	// A limit buy without a price cannot be sized.
	d = newOrderDraft("btc_krw", "buy")
	if _, reason := applyPreset(d, entryBals, true, nil, 10); reason != "waiting for a limit price" {
		t.Fatalf("limit buy without a price: reason=%q", reason)
	}

	// Balances not loaded yet → a transient wait, whatever the currency.
	if _, reason := applyPreset(newOrderDraft("btc_krw", "sell"), nil, false, nil, 10); reason != "waiting for balances" {
		t.Fatalf("balances not ready: reason=%q", reason)
	}

	// Balances loaded, but nothing in the sold currency (a zero balance is not
	// a stored row, so the currency is simply absent): a real zero, not a wait.
	if _, reason := applyPreset(newOrderDraft("eth_krw", "sell"), entryBals, true, nil, 10); reason != "no ETH available" {
		t.Fatalf("absent currency, balances ready: reason=%q", reason)
	}
}

// divFloorQty must truncate toward zero at the request precision, never round
// the last kept digit up — the property the balance-sizing invariant leans on.
func TestDivFloorQtyTruncates(t *testing.T) {
	// 2/3 = 0.6666…7: a round-to-nearest would carry the 16th digit up, but the
	// floored 8-dp result must keep the trailing 6s.
	if got := divFloorQty(decimal.NewFromInt(2), decimal.NewFromInt(3)).String(); got != "0.66666666" {
		t.Fatalf("2/3 floored to %d dp = %q, want 0.66666666", reqDecimalPlaces, got)
	}
	if got := divFloorQty(decimal.NewFromInt(1), decimal.NewFromInt(3)).String(); got != "0.33333333" {
		t.Fatalf("1/3 floored to %d dp = %q, want 0.33333333", reqDecimalPlaces, got)
	}
}

// A fee-reserving buy preset must floor hard enough that the exchange's
// notional*(1+maxFeeRate) reservation still fits the available balance — an ugly
// price makes the exact affordable qty non-terminating, so a round-up would spill
// past the balance into a NO_BALANCE rejection.
func TestApplyPresetBuyReservationNeverExceedsBalance(t *testing.T) {
	const availKrw = "1000000"
	bals := []state.Balance{{Currency: "krw", Available: availKrw}}
	fees := &FeeRates{MaxRate: "0.0015", BuyFeeCurrency: "krw"}
	d := newOrderDraft("btc_krw", "buy")
	d.price = "9999999" // non-terminating quotient

	got, reason := applyPreset(d, bals, true, fees, 100)
	if reason != "" {
		t.Fatalf("100%% buy: reason=%q", reason)
	}
	qty, _ := parseDec(got.qty)
	price, _ := parseDec(d.price)
	rate, _ := parseDec(fees.MaxRate)
	avail, _ := parseDec(availKrw)
	reserve := qty.Mul(price).Mul(decimal.NewFromInt(1).Add(rate))
	if reserve.GreaterThan(avail) {
		t.Fatalf("reservation %s exceeds available %s (qty=%s)", reserve, avail, got.qty)
	}
}

func TestDraftValidateAcceptsSupportedShapes(t *testing.T) {
	limit := func(side string) orderDraft { d := newOrderDraft("btc_krw", side); d.price = "10000000"; return d }
	market := func(side string) orderDraft { d := newOrderDraft("btc_krw", side); d.typ = "market"; return d }

	limitBuyQty := limit("buy")
	limitBuyQty.qty = "0.05"
	limitSellQty := limit("sell")
	limitSellQty.qty = "0.05"
	limitBuyAmt := limit("buy")
	limitBuyAmt.sizeInAmt, limitBuyAmt.amt = true, "500000"
	limitSellAmt := limit("sell")
	limitSellAmt.sizeInAmt, limitSellAmt.amt = true, "500000"
	marketBuy := market("buy")
	marketBuy.amt = "500000"
	marketSell := market("sell")
	marketSell.qty = "0.05"

	for _, d := range []orderDraft{limitBuyQty, limitSellQty, limitBuyAmt, limitSellAmt, marketBuy, marketSell} {
		if err := d.validate(); err != nil {
			t.Errorf("supported shape rejected: side=%s typ=%s amtMode=%v: %v", d.side, d.typ, d.sizeInAmt, err)
		}
	}

	// Wire-shape invariants: exactly one size field, price only on a limit.
	if v := limitBuyQty.values(); v["price"] != "10000000" || v["qty"] != "0.05" || v["amt"] != "" {
		t.Errorf("limit qty must wire price+qty only: %v", v)
	}
	if v := limitBuyAmt.values(); v["qty"] != "0.05" || v["amt"] != "" { // 500000/10000000 derived
		t.Errorf("limit-amount must wire a derived qty and no amt: %v", v)
	}
	if v := marketBuy.values(); v["amt"] != "500000" || v["qty"] != "" || v["price"] != "" {
		t.Errorf("market buy must wire amt only: %v", v)
	}
	if v := marketSell.values(); v["qty"] != "0.05" || v["amt"] != "" || v["price"] != "" {
		t.Errorf("market sell must wire qty only: %v", v)
	}
}

func TestDraftValidateRejectsBadShapes(t *testing.T) {
	limitNoSize := newOrderDraft("btc_krw", "buy")
	limitNoSize.price = "10000000" // price but no size
	limitNoPrice := newOrderDraft("btc_krw", "buy")
	limitNoPrice.qty = "0.05" // size but no price

	bad := []struct {
		name string
		d    orderDraft
	}{
		{"no size", limitNoSize},
		{"limit without price", limitNoPrice},
		{"bad side", orderDraft{symbol: "btc_krw", side: "hodl", typ: "limit", price: "1", qty: "1"}},
		{"bad type", orderDraft{symbol: "btc_krw", side: "buy", typ: "stop", qty: "1"}},
	}
	for _, c := range bad {
		if err := c.d.validate(); err == nil {
			t.Errorf("%s: validate must reject, got nil", c.name)
		}
	}
}

// TestDraftIgnoresIrrelevantFields: stray fields left on the draft (a leftover
// limit price, a stray qty, sizeInAmt on a market order) never reach the wire —
// the matrix filters them and validate still accepts the order.
func TestDraftIgnoresIrrelevantFields(t *testing.T) {
	d := newOrderDraft("btc_krw", "buy")
	d.typ = "market"
	d.price = "10000000" // leftover from a prior limit
	d.qty = "0.05"       // stray
	d.amt = "500000"
	d.sizeInAmt = true // meaningless on a market order

	v := d.values()
	if v["amt"] != "500000" || v["price"] != "" || v["qty"] != "" {
		t.Fatalf("market buy must filter irrelevant fields: %v", v)
	}
	if err := d.validate(); err != nil {
		t.Fatalf("validate must accept a market buy despite stray fields: %v", err)
	}
}

func TestApplyPresetAmountMode(t *testing.T) {
	// Limit buy in amount mode: 50% of 1,000,000 KRW → amt 500,000, no qty; the
	// wire derives qty = 500000/10000000 = 0.05.
	d := newOrderDraft("btc_krw", "buy")
	d.price, d.sizeInAmt = "10000000", true
	got, reason := applyPreset(d, entryBals, true, nil, 50)
	if reason != "" || got.amt != "500000" || got.qty != "" {
		t.Fatalf("limit-amount buy preset: amt=%q qty=%q reason=%q", got.amt, got.qty, reason)
	}
	if q := got.wireQty(); q != "0.05" {
		t.Fatalf("limit-amount buy wire qty: %q", q)
	}

	// Limit sell in amount mode: 25% of 0.5 BTC = 0.125 → amt 0.125×10,000,000.
	d = newOrderDraft("btc_krw", "sell")
	d.price, d.sizeInAmt = "10000000", true
	got, reason = applyPreset(d, entryBals, true, nil, 25)
	if reason != "" || got.amt != "1250000" {
		t.Fatalf("limit-amount sell preset: amt=%q reason=%q", got.amt, reason)
	}
}

func TestAnchorPrice(t *testing.T) {
	tick := state.Ticker{Close: "9995000"}
	cases := []struct {
		anchor string
		want   string
	}{
		{"bid", "9999000"},
		{"ask", "10001000"},
		{"mid", "10000000"}, // (9999000+10001000)/2, already on the 1000 grid
		{"last", "9995000"},
	}
	for _, c := range cases {
		got, ok := anchorPrice(c.anchor, "buy", entryBook, true, tick, true, entryBands)
		if !ok || got != c.want {
			t.Errorf("anchorPrice(%q) = %q,%v; want %q", c.anchor, got, ok, c.want)
		}
	}

	// A half-tick mid snaps in the side's conservative direction: down for a
	// buy (never bid above), up for a sell (never ask below).
	book := entryBook
	book.Asks = []state.PriceLevel{{Price: "10000000", Qty: "1"}} // mid 9999500
	if got, ok := anchorPrice("mid", "buy", book, true, tick, true, entryBands); !ok || got != "9999000" {
		t.Fatalf("buy mid must floor: %q,%v", got, ok)
	}
	if got, ok := anchorPrice("mid", "sell", book, true, tick, true, entryBands); !ok || got != "10000000" {
		t.Fatalf("sell mid must ceil: %q,%v", got, ok)
	}

	if _, ok := anchorPrice("bid", "buy", state.Orderbook{}, false, tick, true, nil); ok {
		t.Fatal("no book: want ok=false")
	}
	if _, ok := anchorPrice("last", "buy", entryBook, true, state.Ticker{}, false, nil); ok {
		t.Fatal("no ticker: want ok=false")
	}
}

func TestPlaceGateReason(t *testing.T) {
	if got := placeGateReason(false, true, true); got != "" {
		t.Fatalf("open gate: %q", got)
	}
	if got := placeGateReason(true, true, true); got == "" {
		t.Fatal("in-flight must gate")
	}
	if got := placeGateReason(false, false, true); got == "" {
		t.Fatal("a not-live (loading/unsettled) book must gate")
	}
	if got := placeGateReason(false, true, false); got == "" {
		t.Fatal("unknown fee policy must gate")
	}
}

// A pair quoted in something other than KRW must show the same panel figures.
// The quote currency here is named nowhere else in the module, so a currency
// list gating any of these paths fails this test instead of passing it.
func TestBuildPreviewOnNonKRWQuotedPair(t *testing.T) {
	const quote = "xaut"
	book := state.Orderbook{
		Symbol: "btc_" + quote,
		Bids:   []state.PriceLevel{{Price: "93900.00", Qty: "2"}, {Price: "93800.00", Qty: "5"}},
		Asks:   []state.PriceLevel{{Price: "94100.00", Qty: "2"}, {Price: "94200.00", Qty: "5"}},
	}
	bands := []ops.TickBand{{PriceGte: "0", TickSize: "0.01"}}
	bals := []state.Balance{{Currency: quote, Available: "1000"}, {Currency: "btc", Available: "0.5"}}
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.0015", MaxRate: "0.002", BuyFeeCurrency: quote}

	d := newOrderDraft("btc_"+quote, "buy")
	d.price, d.qty = "93800.00", "0.001"
	p := buildPreview(d, book, state.StatusPresent, bals, bands, ops.OrderValueBounds{}, fees)
	if !p.OK {
		t.Fatalf("preview not OK: %s", p.Err)
	}
	if p.BaseCcy != "btc" || p.QuoteCcy != quote {
		t.Fatalf("currencies: base=%s quote=%s", p.BaseCcy, p.QuoteCcy)
	}
	if p.AvailQuote != "1000" {
		t.Fatalf("available quote: %s", p.AvailQuote)
	}
	// The notional is what the fee estimate is gated on — both must be present.
	if p.Notional != "93.8" {
		t.Fatalf("notional: %q, want 93.8", p.Notional)
	}
	// 93.8 × 0.001 = 0.0938, displayed to 3 significant figures. A fee rounded to
	// whole units would read "0" — no fee at all.
	if p.FeeKind != "maker" || p.FeeEst != "0.0938" {
		t.Fatalf("fee: kind=%s est=%q", p.FeeKind, p.FeeEst)
	}
	// No notional-bound warning: those figures belong to the KRW market.
	for _, w := range p.Warnings {
		if w.Code == ops.WarnNotionalBelowMin || w.Code == ops.WarnNotionalAboveMax {
			t.Fatalf("claimed a notional bound on a %s pair: %s", quote, w.Message)
		}
	}
}

// The fee is DISPLAYED to 3 significant figures, so it reads correctly whether
// the quote currency puts it in the thousands or in a small fraction.
func TestRoundSigFigs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0", "0"},
		{"499.5", "500"},
		{"149.99985", "150"},
		{"0.1409685", "0.141"},
		{"0.00012345", "0.000123"},
		{"1234567", "1230000"},
		{"-149.99985", "-150"},
	}
	for _, c := range cases {
		in, err := decimal.NewFromString(c.in)
		if err != nil {
			t.Fatalf("parse %q: %v", c.in, err)
		}
		if got := roundSigFigs(in, feeSigFigs).String(); got != c.want {
			t.Errorf("roundSigFigs(%s) = %s; want %s", c.in, got, c.want)
		}
	}
}
