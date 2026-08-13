// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
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
}

func TestBuildPreviewNotReady(t *testing.T) {
	d := newOrderDraft("btc_krw", "buy")
	p := buildPreview(d, state.Orderbook{}, state.StatusNotReady, nil, nil, ops.OrderValueBounds{}, nil)
	if p.OK || p.Err == "" {
		t.Fatalf("not-ready book: %+v", p)
	}
}

// An empty (live-but-orderless) book previews a limit order as a resting maker
// and refuses a market order for want of liquidity.
func TestBuildPreviewEmptyBook(t *testing.T) {
	fees := &FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw"}

	lim := newOrderDraft("btc_krw", "buy")
	lim.price, lim.qty = "9990000", "0.05"
	p := buildPreview(lim, state.Orderbook{}, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees)
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

	mkt := newOrderDraft("btc_krw", "buy")
	mkt.typ, mkt.amt = "market", "500000"
	if q := buildPreview(mkt, state.Orderbook{}, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees); q.OK || q.Err == "" {
		t.Fatalf("empty-book market should be refused: %+v", q)
	}

	// Only a tif that can REST previews as the first maker: an ioc/fok limit
	// would be accepted and immediately canceled unfilled, so it is refused
	// (and must never carry a maker-fee estimate); po rests like gtc.
	for i, tif := range tifOptions {
		d := newOrderDraft("btc_krw", "buy")
		d.price, d.qty, d.tifIdx = "9990000", "0.05", i
		p := buildPreview(d, state.Orderbook{}, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees)
		if wantOK := tif == "gtc" || tif == "po"; p.OK != wantOK {
			t.Errorf("empty-book %s limit: OK=%v want %v (err %q)", tif, p.OK, wantOK, p.Err)
		}
		if !p.OK && p.FeeEst != "" {
			t.Errorf("a refused %s limit must not estimate a fee: %q", tif, p.FeeEst)
		}
	}

	// The order-value bounds do not need a book, so the empty-book path raises
	// the same below-min warning placement would — through the same ops helper.
	// An empty book is where a too-small order is MOST likely (a first maker
	// testing a new listing), so this is the one check that must not go missing.
	small := newOrderDraft("btc_krw", "buy")
	small.price, small.qty = "9990000", "0.0001" // 999 KRW: below the 5,000 minimum
	bounds := ops.OrderValueBounds{QuoteCurrency: "krw", Min: "5000", Max: "1000000000"}
	sp := buildPreview(small, state.Orderbook{}, state.StatusEmpty, entryBals, entryBands, bounds, fees)
	if !sp.OK {
		t.Fatalf("a below-min limit still previews (the warning is the signal): %s", sp.Err)
	}
	if len(sp.Warnings) != 1 || sp.Warnings[0].Code != ops.WarnNotionalBelowMin {
		t.Fatalf("empty-book below-min must warn: %+v", sp.Warnings)
	}
	// A pair publishing no bound raises nothing rather than borrowing another's.
	if q := buildPreview(small, state.Orderbook{}, state.StatusEmpty, entryBals, entryBands, ops.OrderValueBounds{}, fees); len(q.Warnings) != 0 {
		t.Fatalf("no published bound must raise no bound warning: %+v", q.Warnings)
	}

	// The empty-book path values the order itself rather than going through
	// AnalyzePlace, so it must render the same way for a pair quoted in a
	// small-magnitude currency: a fractional notional kept, and a fee that whole
	// units would round away to "no fee at all".
	sub := newOrderDraft("eth_btc", "buy")
	sub.price, sub.qty = "0.0345", "2"
	q := buildPreview(sub, state.Orderbook{}, state.StatusEmpty, nil, nil, ops.OrderValueBounds{}, fees)
	if !q.OK {
		t.Fatalf("empty-book limit on a btc-quoted pair: %s", q.Err)
	}
	if q.Notional != "0.069" || q.FeeEst != "0.000069" {
		t.Fatalf("btc-quoted empty-book figures: notional=%q fee=%q", q.Notional, q.FeeEst)
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
