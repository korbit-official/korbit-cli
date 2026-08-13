// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

// fakeMarketDoer answers the public market-data calls PrePlaceCheck makes,
// keyed on path, with the verbatim DATA payload (the wire client already unwraps
// the envelope, so the typed layer sees the inner object/array).
type fakeMarketDoer struct {
	orderbook string
	tickSize  string
	// pairs is the /v2/currencyPairs payload the bound checks read the pair's
	// minOrderValue/maxOrderValue and quote currency from. Empty means the listing
	// carries no entry for the symbol, so both bound checks are skipped.
	pairs string
	err   error
}

func (d fakeMarketDoer) Do(_ context.Context, call korbit.Call, _ korbit.Policy) (json.RawMessage, korbit.Meta, error) {
	if d.err != nil {
		return nil, korbit.Meta{}, d.err
	}
	switch call.Path {
	case "/v2/orderbook":
		return json.RawMessage(d.orderbook), korbit.Meta{}, nil
	case "/v2/tickSizePolicy":
		return json.RawMessage(d.tickSize), korbit.Meta{}, nil
	case "/v2/currencyPairs":
		if d.pairs == "" {
			return json.RawMessage(`[]`), korbit.Meta{}, nil
		}
		return json.RawMessage(d.pairs), korbit.Meta{}, nil
	}
	return json.RawMessage(`{}`), korbit.Meta{}, nil
}

// a liquid book around a ~10,000,000 mid, tick 1000.
const sampleBook = `{"timestamp":1,
	"bids":[{"price":"9999000","qty":"2"},{"price":"9990000","qty":"5"}],
	"asks":[{"price":"10001000","qty":"2"},{"price":"10100000","qty":"5"}]}`

const sampleTick = `[{"symbol":"btc_krw","tickSizePolicy":[{"priceGte":"0","tickSize":"1000"}],"orderbookLevels":[]}]`

// the pair listing as the API serves it: the currencies and the order value
// bounds the analysis checks an order against.
const samplePairs = `[{"symbol":"btc_krw","status":"launched","baseCurrency":"btc","quoteCurrency":"krw",
	"minOrderValue":"5000","maxOrderValue":"1000000000"}]`

// a book whose second ask (11,000,000) sits outside a 5% band around the
// 10,000,000 mid (cap 10,500,000), so a price-protected taker buy trims there.
const ppBook = `{"timestamp":1,
	"bids":[{"price":"9999000","qty":"2"}],
	"asks":[{"price":"10001000","qty":"1"},{"price":"11000000","qty":"5"}]}`

func codes(ws []PlaceWarning) string {
	var cs []string
	for _, w := range ws {
		cs = append(cs, string(w.Code))
	}
	return strings.Join(cs, ",")
}

func hasCode(ws []PlaceWarning, code string) bool {
	for _, w := range ws {
		if string(w.Code) == code {
			return true
		}
	}
	return false
}

func run(t *testing.T, doer fakeMarketDoer, values map[string]string) []PlaceWarning {
	t.Helper()
	if _, ok := values["accountSeq"]; !ok {
		values["accountSeq"] = "1"
	}
	_, ws, err := PrePlaceCheck(context.Background(), rawapi.New(doer, nil), values)
	if err != nil {
		t.Fatalf("PrePlaceCheck error: %v", err)
	}
	return ws
}

func runSim(t *testing.T, doer fakeMarketDoer, values map[string]string) PlaceSimulation {
	t.Helper()
	if _, ok := values["accountSeq"]; !ok {
		values["accountSeq"] = "1"
	}
	sim, _, err := PrePlaceCheck(context.Background(), rawapi.New(doer, nil), values)
	if err != nil {
		t.Fatalf("PrePlaceCheck error: %v", err)
	}
	return sim
}

func TestPrePlaceMarketSlippageAndLiquidity(t *testing.T) {
	// A thin top with a big gap to the next level: best ask 10,000,000 holds only
	// 0.5, the rest of a market buy jumps to 11,000,000 (10% higher).
	gapped := `{"timestamp":1,
		"bids":[{"price":"9990000","qty":"5"}],
		"asks":[{"price":"10000000","qty":"0.5"},{"price":"11000000","qty":"5"}]}`
	doer := fakeMarketDoer{orderbook: gapped}
	// Market buy of 20,000,000 KRW sweeps mostly into the 11,000,000 level.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "20000000",
	})
	if !hasCode(ws, "HIGH_SLIPPAGE") {
		t.Fatalf("want HIGH_SLIPPAGE, got: %s", codes(ws))
	}

	// A buy larger than the whole ask side can fill: INSUFFICIENT_LIQUIDITY. And
	// because the sweep runs off the end of the returned book, the estimate is
	// depth-limited (the visible book may not be the full depth).
	ws = run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "999999999999",
	})
	if !hasCode(ws, "INSUFFICIENT_LIQUIDITY") {
		t.Fatalf("want INSUFFICIENT_LIQUIDITY, got: %s", codes(ws))
	}
	if !hasCode(ws, "BOOK_DEPTH_LIMITED") {
		t.Fatalf("want BOOK_DEPTH_LIMITED when the sweep exhausts the visible book, got: %s", codes(ws))
	}

	// A market buy that fills comfortably within the visible book does NOT flag a
	// depth limit — the estimate saw every level it touched.
	ws = run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "1000000",
	})
	if hasCode(ws, "BOOK_DEPTH_LIMITED") {
		t.Fatalf("a fully-filled order must not flag a depth limit: %s", codes(ws))
	}
}

func TestPrePlaceLimitBookDepthVsPriceCap(t *testing.T) {
	// Two asks, both below a deep limit price. A limit buy for more than both hold
	// crosses every visible level and still can't fill: the visible crossing book
	// (not the limit price) is the binding constraint, so BOOK_DEPTH_LIMITED fires.
	book := `{"timestamp":1,
		"bids":[{"price":"9990000","qty":"5"}],
		"asks":[{"price":"10000000","qty":"1"},{"price":"10001000","qty":"1"}]}`
	doer := fakeMarketDoer{orderbook: book, tickSize: sampleTick}
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "10005000", "qty": "5",
	})
	if !hasCode(ws, "BOOK_DEPTH_LIMITED") {
		t.Fatalf("a limit that exhausts the visible crossing book should flag a depth limit: %s", codes(ws))
	}

	// A limit priced between the two asks stops the sweep on its own price, not on
	// book depth — the remainder rests as a maker order, so no depth-limit flag.
	ws = run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "10000000", "qty": "5",
	})
	if hasCode(ws, "BOOK_DEPTH_LIMITED") {
		t.Fatalf("a limit stopped by its own price (not book depth) must not flag a depth limit: %s", codes(ws))
	}
}

func TestPrePlaceMarketableLimitIsNotWarned(t *testing.T) {
	// A limit buy priced at the top ask (holds qty 2) for qty 5: the crossing
	// portion fills 2 now, the remaining 3 rests (gtc) or cancels (ioc). A
	// crossing limit taking liquidity is ordinary behavior fully described by the
	// simulation (marketable + the est-fill/remainder fields), so it fires no
	// warning at all: the crossing itself is not flagged, and the sweep stops on
	// the limit price (not book depth) so no BOOK_DEPTH_LIMITED / slippage either.
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	for _, tif := range []string{"gtc", "ioc"} {
		values := map[string]string{
			"symbol": "btc_krw", "side": "buy", "orderType": "limit",
			"price": "10001000", "qty": "5", "timeInForce": tif,
		}
		ws := run(t, doer, values)
		if len(ws) != 0 {
			t.Fatalf("tif=%s: a crossing marketable limit must fire no warnings, got: %s", tif, codes(ws))
		}
		// A marketable IOC makes at least some fills, so it must NOT warn that it
		// would expire — that warning is only for a non-crossing IOC (below).
		if hasCode(ws, "IOC_WOULD_EXPIRE") {
			t.Fatalf("tif=%s: a marketable IOC that fills must not warn IOC_WOULD_EXPIRE: %s", tif, codes(ws))
		}

		// The taker crossing and the partial outcome are fully carried by the
		// simulation regardless — this is what makes the warning redundant.
		sim := runSim(t, doer, values)
		if !sim.Marketable || sim.FullyFilled || sim.EstFilledQty != "2" || sim.EstRemainingQty != "3" {
			t.Fatalf("tif=%s: the simulation must report the marketable 2-of-5 partial fill: %+v", tif, sim)
		}
		wantDisp := "rests"
		if tif == "ioc" {
			wantDisp = "canceled"
		}
		if !strings.Contains(sim.Disposition, wantDisp) {
			t.Fatalf("tif=%s: want disposition %q, got %q", tif, wantDisp, sim.Disposition)
		}
	}
}

func TestPrePlaceBookDepthExactFillAndSellSide(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}

	// A market buy whose amount consumes both ask levels EXACTLY: the sweep touches
	// the last level but the order fully fills, so the estimate saw every level it
	// paid for — no depth warning. (asks: 10001000*2 + 10100000*5 = 70502000 KRW.)
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "70502000",
	})
	if hasCode(ws, "BOOK_DEPTH_LIMITED") {
		t.Fatalf("an order that fully fills on the last level must not flag a depth limit: %s", codes(ws))
	}

	// A market SELL bigger than the whole bid side: sweeps every visible bid level
	// without filling, so the depth warning fires on the consumed (bid) side.
	ws = run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "market", "qty": "100",
	})
	if !hasCode(ws, "BOOK_DEPTH_LIMITED") {
		t.Fatalf("a sell that exhausts the visible bid side should flag a depth limit: %s", codes(ws))
	}
}

func TestPrePlaceLimitFatFinger(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// Limit buy with an extra digit (100,010,000 vs ~10,000,000 mid).
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "100010000", "qty": "1",
	})
	if !hasCode(ws, "PRICE_FAR_ABOVE_MARKET") {
		t.Fatalf("want PRICE_FAR_ABOVE_MARKET, got: %s", codes(ws))
	}

	// A normal resting limit buy below the market raises no price/slippage flags.
	ws = run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "9000000", "qty": "1",
	})
	for _, w := range ws {
		if w.Code != "" && strings.HasPrefix(string(w.Code), "PRICE_FAR") {
			t.Fatalf("a below-market resting buy should not warn on price: %s", codes(ws))
		}
	}
}

func TestPrePlacePostOnlyWouldReject(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// Post-only buy priced into the asks -> the server would reject it.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10001000", "qty": "1", "timeInForce": "po",
	})
	if !hasCode(ws, "POST_ONLY_WOULD_REJECT") {
		t.Fatalf("want POST_ONLY_WOULD_REJECT, got: %s", codes(ws))
	}
}

func TestPrePlaceTickAndNotional(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick, pairs: samplePairs}
	// Price off the 1000 tick grid + a sub-minimum notional.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "9000500", "qty": "0.0001",
	})
	if !hasCode(ws, "PRICE_OFF_TICK") {
		t.Fatalf("want PRICE_OFF_TICK, got: %s", codes(ws))
	}
	if !hasCode(ws, "NOTIONAL_BELOW_MIN") {
		t.Fatalf("want NOTIONAL_BELOW_MIN, got: %s", codes(ws))
	}
}

func TestPrePlaceCleanOrderNoWarnings(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// On-grid, below-market resting limit buy of healthy notional: no warnings.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "9000000", "qty": "1",
	})
	if len(ws) != 0 {
		t.Fatalf("expected no warnings, got: %s", codes(ws))
	}
}

func TestPrePlaceSimulationFields(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}

	// A resting (below-market) limit buy: not marketable, the whole qty rests.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "9000000", "qty": "2",
	})
	if sim.Disclaimer == "" {
		t.Fatalf("the simulation must always carry the estimate-only disclaimer")
	}
	if sim.Marketable {
		t.Fatalf("a below-market limit buy should not be marketable: %+v", sim)
	}
	if sim.EstRemainingQty != "2" || sim.Disposition == "" {
		t.Fatalf("a resting limit should report the full remaining qty + disposition: %+v", sim)
	}

	// A market sell that the top bid can't fully absorb: partial fill + remainder
	// canceled, with an average fill price reported.
	sim = runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "market", "qty": "10",
	})
	if !sim.Marketable || sim.EstAvgFillPrice == "" || sim.EstFilledQty == "" {
		t.Fatalf("a market sell should report a marketable fill estimate: %+v", sim)
	}
	if sim.FullyFilled {
		t.Fatalf("selling 10 into a 7-deep bid side should not fully fill: %+v", sim)
	}
	if sim.EstRemainingQty == "" || !strings.Contains(sim.Disposition, "canceled") {
		t.Fatalf("the unfilled market remainder should be reported as canceled: %+v", sim)
	}
}

func TestPrePlaceBestPostOnlyRestsAtQueuePeg(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// A post-only best SELL pegs to the own (queue = ask) side's Nth level and
	// rests as a maker — it never takes, so there is no immediate fill.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "best", "timeInForce": "po", "bestNth": "1", "qty": "2",
	})
	if sim.EstPegPrice != "10001000" {
		t.Fatalf("po sell should peg to the best ask (10001000): %+v", sim)
	}
	if sim.Marketable || sim.EstFilledQty != "" || !strings.Contains(sim.Disposition, "rests") {
		t.Fatalf("a post-only best order should rest, not fill: %+v", sim)
	}
	if sim.BestBid == "" || sim.Notional == "" {
		t.Fatalf("a best order should still report reference prices + notional: %+v", sim)
	}
}

func TestPrePlaceBestTakerPegsToOpponentLevel(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// A taker best BUY (ioc) pegs to the opponent (ask) side's Nth level and
	// crosses up to it like a limit at that price.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "ioc", "bestNth": "1", "amt": "10000000",
	})
	if sim.EstPegPrice != "10001000" {
		t.Fatalf("taker buy should peg to the best ask (10001000): %+v", sim)
	}
	if !sim.Marketable || sim.EstFilledQty == "" {
		t.Fatalf("a taker best buy within the peg should fill: %+v", sim)
	}
}

func TestPrePlaceBestFOKKilled(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// A fill-or-kill best BUY whose amount exceeds the liquidity at/under the peg
	// (best-nth 1 = best ask, only qty 2 available) is KILLED — no fill.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "fok", "bestNth": "1", "amt": "30000000",
	})
	if sim.EstFilledQty != "" || !strings.Contains(sim.Disposition, "KILLED") {
		t.Fatalf("an unfillable fill-or-kill best order should simulate as killed: %+v", sim)
	}
	if ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "fok", "bestNth": "1", "amt": "30000000",
	}); !hasCode(ws, "FOK_WOULD_KILL") {
		t.Fatalf("expected FOK_WOULD_KILL, got: %s", codes(ws))
	}
}

func TestPrePlaceBestPegLevelNotVisible(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// best-nth 5 with only two ask levels visible: the peg can't be derived, so no
	// peg and no fill are reported — only the reference prices remain.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "ioc", "bestNth": "5", "amt": "10000000",
	})
	if sim.EstPegPrice != "" || sim.Marketable || sim.EstFilledQty != "" {
		t.Fatalf("an out-of-range best-nth should not fabricate a peg or fill: %+v", sim)
	}
	if sim.BestBid == "" {
		t.Fatalf("reference prices should still be reported: %+v", sim)
	}
	// The user must still get a signal that the order would take no liquidity —
	// not an empty (reads-as-safe) warnings list.
	if ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "ioc", "bestNth": "5", "amt": "10000000",
	}); !hasCode(ws, "BEST_PEG_UNAVAILABLE") {
		t.Fatalf("an unpriceable best order should warn BEST_PEG_UNAVAILABLE, got: %s", codes(ws))
	}
}

func TestPrePlaceMarketPriceProtectionTrims(t *testing.T) {
	doer := fakeMarketDoer{orderbook: ppBook, tickSize: sampleTick}
	// A price-protected (--pp) market BUY: the second ask (11,000,000) is outside
	// the default 5% band (cap 10,500,000), so the fill is trimmed there and the
	// remainder is canceled by protection — NOT flagged as insufficient liquidity.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "20000000", "pp": "true",
	})
	if !hasCode(ws, "PRICE_PROTECTION_CAPPED") {
		t.Fatalf("expected PRICE_PROTECTION_CAPPED, got: %s", codes(ws))
	}
	if hasCode(ws, "INSUFFICIENT_LIQUIDITY") {
		t.Fatalf("a protection trim must not also report insufficient liquidity: %s", codes(ws))
	}
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "20000000", "pp": "true",
	})
	if !strings.Contains(sim.Disposition, "price protection") {
		t.Fatalf("disposition should attribute the cancel to price protection: %+v", sim)
	}
}

func TestPrePlaceMarketPriceProtectionWithinBand(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// Both asks (10,001,000 and 10,100,000) are inside the 5% band, so a pp buy
	// that fills within them is not trimmed.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "30000000", "pp": "true",
	})
	if hasCode(ws, "PRICE_PROTECTION_CAPPED") {
		t.Fatalf("a fill entirely within the pp band must not be trimmed: %s", codes(ws))
	}
}

func TestPrePlaceBestGtcBuyRestsRemainder(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// A gtc best BUY sized by --amt is converted to base at the peg (best-nth 1 =
	// best ask 10,001,000; qty = 30,000,000/10,001,000 ≈ 2.9997) but the peg level
	// holds only qty 2, so the crossing portion fills 2 and the rest rests as a
	// maker.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "gtc", "bestNth": "1", "amt": "30000000",
	})
	if sim.EstPegPrice != "10001000" || !sim.Marketable || sim.FullyFilled {
		t.Fatalf("a gtc best buy over the peg depth should partially fill: %+v", sim)
	}
	if sim.EstFilledQty != "2" || sim.EstRemainingQty == "" {
		t.Fatalf("a gtc best buy should fill the peg-level qty (2) and rest a base remainder: %+v", sim)
	}
	if !strings.Contains(sim.Disposition, "rests") {
		t.Fatalf("a gtc best buy remainder should rest as a maker: %+v", sim)
	}
}

func TestPrePlaceBestIocBuyPegLimitedDisposition(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// An ioc best BUY whose amt exceeds the peg-level depth: the peg (not the book)
	// stops the fill, so the canceled remainder must be attributed to the price
	// limit — NOT "book exhausted".
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "ioc", "bestNth": "1", "amt": "30000000",
	})
	if !sim.Marketable || sim.FullyFilled {
		t.Fatalf("an ioc best buy over the peg depth should partially fill: %+v", sim)
	}
	if !strings.Contains(sim.Disposition, "canceled") || strings.Contains(sim.Disposition, "book exhausted") {
		t.Fatalf("a peg-limited ioc remainder should be canceled by the price limit, not 'book exhausted': %+v", sim)
	}
}

func TestPrePlaceBestSellPegAndPostOnlyBuy(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// A taker best SELL pegs to the opponent (bid) side's Nth level (best-nth 2 =
	// the 2nd bid, 9,990,000).
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "best", "timeInForce": "ioc", "bestNth": "2", "qty": "1",
	})
	if sim.EstPegPrice != "9990000" || !sim.Marketable || sim.EstFilledQty == "" {
		t.Fatalf("a taker best sell should peg to the 2nd bid and fill: %+v", sim)
	}
	// A post-only best BUY pegs to the own (bid) side's Nth level and rests.
	poSim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "po", "bestNth": "1", "amt": "50000",
	})
	if poSim.EstPegPrice != "9999000" || poSim.Marketable || !strings.Contains(poSim.Disposition, "rests") {
		t.Fatalf("a post-only best buy should peg to the best bid and rest: %+v", poSim)
	}
}

func TestPrePlaceBestFOKFullyFillsNotKilled(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// A fill-or-kill best BUY pegged to the 2nd ask (best-nth 2) whose amt fits
	// within the crossing depth must NOT be killed — it fills and falls through.
	values := map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "fok", "bestNth": "2", "amt": "10000000",
	}
	if ws := run(t, doer, values); hasCode(ws, "FOK_WOULD_KILL") {
		t.Fatalf("a fillable fok best must not be killed: %s", codes(ws))
	}
	if sim := runSim(t, doer, values); !sim.Marketable || sim.EstFilledQty == "" || strings.Contains(sim.Disposition, "KILLED") {
		t.Fatalf("a fillable fok best should fill, not kill: %+v", sim)
	}
}

func TestPrePlaceBestWithPriceProtectionTrims(t *testing.T) {
	doer := fakeMarketDoer{orderbook: ppBook, tickSize: sampleTick}
	// A taker best BUY pegged to the 2nd ask (11,000,000) with --pp: the 5% band
	// (cap 10,500,000) binds tighter than the peg, so the fill is trimmed by price
	// protection and not reported as insufficient liquidity.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "ioc", "bestNth": "2", "amt": "20000000", "pp": "true",
	})
	if !hasCode(ws, "PRICE_PROTECTION_CAPPED") {
		t.Fatalf("a pp cap tighter than the best peg should trim: %s", codes(ws))
	}
	if hasCode(ws, "INSUFFICIENT_LIQUIDITY") {
		t.Fatalf("a protection trim on a best order must not report insufficient liquidity: %s", codes(ws))
	}
}

func TestPrePlaceLimitPriceProtectionTrims(t *testing.T) {
	// A crossing limit is a taker over its marketable portion, so --pp must trim it
	// the same way it trims market/best orders. On ppBook (mid 10,000,000, 5% cap
	// 10,500,000) a gtc limit BUY priced at 11,000,000 would, without protection,
	// sweep both asks (qty 1 + 5 = 6) and fully fill qty 3; with --pp the second ask
	// (11,000,000) is outside the band, so only 1 fills and the remainder is canceled
	// by protection — NOT flagged as insufficient liquidity.
	for _, tif := range []string{"gtc", "ioc"} {
		doer := fakeMarketDoer{orderbook: ppBook, tickSize: sampleTick}
		ws := run(t, doer, map[string]string{
			"symbol": "btc_krw", "side": "buy", "orderType": "limit",
			"price": "11000000", "qty": "3", "timeInForce": tif, "pp": "true",
		})
		if !hasCode(ws, "PRICE_PROTECTION_CAPPED") {
			t.Fatalf("%s: a crossing limit past the pp band should trim, got: %s", tif, codes(ws))
		}
		if hasCode(ws, "INSUFFICIENT_LIQUIDITY") {
			t.Fatalf("%s: a protection trim must not also report insufficient liquidity: %s", tif, codes(ws))
		}
		if msg := ppMessage(ws); !strings.Contains(msg, "up to") {
			t.Fatalf("%s: a buy-side pp bound is a ceiling and must render 'up to', got: %q", tif, msg)
		}
	}

	// The same on the sell side: a sell band is a floor, so it reads "down to".
	// sellBook mid 10,000,000, 5% floor 9,500,000; the 2nd bid (9,000,000) is
	// outside it, so a gtc limit SELL of qty 3 priced at 9,000,000 fills only 1.
	const sellBook = `{"timestamp":1,
		"bids":[{"price":"9999000","qty":"1"},{"price":"9000000","qty":"5"}],
		"asks":[{"price":"10001000","qty":"2"}]}`
	ws := run(t, fakeMarketDoer{orderbook: sellBook, tickSize: sampleTick}, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "limit",
		"price": "9000000", "qty": "3", "timeInForce": "gtc", "pp": "true",
	})
	if !hasCode(ws, "PRICE_PROTECTION_CAPPED") {
		t.Fatalf("a crossing limit sell past the pp floor should trim, got: %s", codes(ws))
	}
	if msg := ppMessage(ws); !strings.Contains(msg, "down to") || strings.Contains(msg, "up to") {
		t.Fatalf("a sell-side pp bound is a floor and must render 'down to', got: %q", msg)
	}
}

func TestPrePlaceLimitFOKPriceProtectionKills(t *testing.T) {
	doer := fakeMarketDoer{orderbook: ppBook, tickSize: sampleTick}
	// A fill-or-kill limit BUY of qty 3 at 11,000,000: the book alone (qty 1 + 5)
	// covers it, so WITHOUT protection it fills and is not killed. WITH --pp the 5%
	// band (cap 10,500,000) admits only the first ask's qty 1, so the FOK cannot
	// fill in full and is KILLED — the case a pp-blind simulation would wrongly
	// report as fillable.
	base := map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "11000000", "qty": "3", "timeInForce": "fok",
	}
	if ws := run(t, doer, base); hasCode(ws, "FOK_WOULD_KILL") {
		t.Fatalf("without pp the book fully covers this fok limit; it must not be killed: %s", codes(ws))
	}

	protected := map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "11000000", "qty": "3", "timeInForce": "fok", "pp": "true",
	}
	ws := run(t, doer, protected)
	if !hasCode(ws, "FOK_WOULD_KILL") {
		t.Fatalf("a pp band that cannot cover a fok limit in full must KILL it, got: %s", codes(ws))
	}
	var fokMsg string
	for _, w := range ws {
		if w.Code == WarnFOKWouldKill {
			fokMsg = w.Message
		}
	}
	if !strings.Contains(fokMsg, "price protection") {
		t.Fatalf("a pp-driven fok kill should attribute the shortfall to price protection, got: %q", fokMsg)
	}
	if sim := runSim(t, doer, protected); !strings.Contains(sim.Disposition, "KILLED") {
		t.Fatalf("a pp-driven fok kill should simulate as KILLED: %+v", sim)
	}
}

// wideBook has a >10.5% spread, so even the best opposing level sits outside a
// default 5% protection band around the 10,000,000 mid (buy cap 10,500,000, sell
// floor 9,500,000). A protected crossing limit therefore fills nothing at all.
const wideBook = `{"timestamp":1,
	"bids":[{"price":"9000000","qty":"5"}],
	"asks":[{"price":"11000000","qty":"5"}]}`

func TestPrePlaceLimitPriceProtectionBlocksAllFills(t *testing.T) {
	doer := fakeMarketDoer{orderbook: wideBook, tickSize: sampleTick}
	// A limit that WOULD cross (buy at/above the best ask, sell at/below the best
	// bid) but whose only takeable level lies outside the pp band: price protection
	// cancels the entire quantity. This must be attributed to pp for BOTH gtc and
	// ioc — not reported as a full resting order (gtc) or as "does not cross the
	// book" (ioc).
	cases := []struct {
		name        string
		side, price string
	}{
		{"buy", "buy", "11000000"},
		{"sell", "sell", "9000000"},
	}
	for _, c := range cases {
		for _, tif := range []string{"gtc", "ioc"} {
			values := map[string]string{
				"symbol": "btc_krw", "side": c.side, "orderType": "limit",
				"price": c.price, "qty": "1", "timeInForce": tif, "pp": "true",
			}
			ws := run(t, doer, values)
			if !hasCode(ws, "PRICE_PROTECTION_CAPPED") {
				t.Fatalf("%s/%s: pp blocking all fills must warn PRICE_PROTECTION_CAPPED, got: %s", c.side, tif, codes(ws))
			}
			if hasCode(ws, "IOC_WOULD_EXPIRE") {
				t.Fatalf("%s/%s: a pp-blocked crossing limit must not be reported as non-crossing: %s", c.side, tif, codes(ws))
			}
			sim := runSim(t, doer, values)
			if sim.Marketable || sim.EstFilledQty != "" {
				t.Fatalf("%s/%s: a pp-blocked limit fills nothing: %+v", c.side, tif, sim)
			}
			// The whole (base-sized) quantity is unfilled and must be reported.
			if sim.EstRemainingQty != "1" {
				t.Fatalf("%s/%s: a pp-blocked limit must report the full submitted qty as remaining, got %q: %+v", c.side, tif, sim.EstRemainingQty, sim)
			}
			if !strings.Contains(sim.Disposition, "price protection") {
				t.Fatalf("%s/%s: disposition must attribute the cancel to price protection: %+v", c.side, tif, sim)
			}
			if strings.Contains(sim.Disposition, "rests") || strings.Contains(sim.Disposition, "does not cross") {
				t.Fatalf("%s/%s: a pp-blocked limit neither rests nor is 'non-crossing': %+v", c.side, tif, sim)
			}
		}
	}
}

func TestPrePlaceLimitNonCrossingWithProtectionNotFlagged(t *testing.T) {
	doer := fakeMarketDoer{orderbook: wideBook, tickSize: sampleTick}
	// A buy limit priced INSIDE the spread — above the 5% cap (10,500,000) but below
	// the best ask (11,000,000) — genuinely does not cross. Protection is irrelevant
	// here (nothing would fill anyway), so it must NOT be blamed: a gtc rests and an
	// ioc expires as non-crossing, with no PRICE_PROTECTION_CAPPED.
	gtc := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10800000", "qty": "1", "timeInForce": "gtc", "pp": "true",
	})
	if !strings.Contains(gtc.Disposition, "rest") {
		t.Fatalf("a non-crossing gtc limit should rest, not be blamed on pp: %+v", gtc)
	}
	iocWs := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10800000", "qty": "1", "timeInForce": "ioc", "pp": "true",
	})
	if hasCode(iocWs, "PRICE_PROTECTION_CAPPED") {
		t.Fatalf("a non-crossing pp limit must not report PRICE_PROTECTION_CAPPED: %s", codes(iocWs))
	}
	if !hasCode(iocWs, "IOC_WOULD_EXPIRE") {
		t.Fatalf("a non-crossing ioc limit should warn IOC_WOULD_EXPIRE: %s", codes(iocWs))
	}
}

func TestPrePlaceBestFOKPriceProtectionKillAttribution(t *testing.T) {
	doer := fakeMarketDoer{orderbook: ppBook, tickSize: sampleTick}
	// A best BUY fok pegged to the 2nd ask (11,000,000) with --pp: the 5% band
	// (cap 10,500,000) binds below the peg and admits only the first ask's qty 1,
	// so the fok cannot fill in full and is KILLED. The message must attribute the
	// shortfall to price protection — not to the peg (the best-branch analogue of
	// the limit-fok case).
	values := map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best",
		"timeInForce": "fok", "bestNth": "2", "amt": "30000000", "pp": "true",
	}
	ws := run(t, doer, values)
	if !hasCode(ws, "FOK_WOULD_KILL") {
		t.Fatalf("a pp band that cannot cover a fok best in full must KILL it, got: %s", codes(ws))
	}
	var fokMsg string
	for _, w := range ws {
		if w.Code == WarnFOKWouldKill {
			fokMsg = w.Message
		}
	}
	if !strings.Contains(fokMsg, "price protection") {
		t.Fatalf("a pp-driven best fok kill should attribute the shortfall to price protection, got: %q", fokMsg)
	}
	if sim := runSim(t, doer, values); !strings.Contains(sim.Disposition, "KILLED") {
		t.Fatalf("a pp-driven best fok kill should simulate as KILLED: %+v", sim)
	}
}

func TestPrePlacePriceProtectionSellSideAndCustomPercent(t *testing.T) {
	// A book whose second bid (9,000,000) is outside a 5% floor (9,500,000) around
	// the 10,000,000 mid, so a price-protected market SELL trims on the floor side.
	sellBook := `{"timestamp":1,
		"bids":[{"price":"9999000","qty":"1"},{"price":"9000000","qty":"5"}],
		"asks":[{"price":"10001000","qty":"2"}]}`
	ws := run(t, fakeMarketDoer{orderbook: sellBook, tickSize: sampleTick}, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "market", "qty": "3", "pp": "true",
	})
	if !hasCode(ws, "PRICE_PROTECTION_CAPPED") {
		t.Fatalf("a pp market sell past the floor should trim: %s", codes(ws))
	}
	// The sell band is a floor, so the message must read "down to", not "up to".
	sellMsg := ppMessage(ws)
	if !strings.Contains(sellMsg, "down to") || strings.Contains(sellMsg, "up to") {
		t.Fatalf("a sell-side pp bound is a floor and must render 'down to', got: %q", sellMsg)
	}

	// A custom --pp-percent tightens the band and is echoed in the message: on
	// ppBook (mid 10,000,000) a 2% band caps a buy at 10,200,000. A buy band is a
	// ceiling, so it reads "up to".
	ws = run(t, fakeMarketDoer{orderbook: ppBook, tickSize: sampleTick}, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "20000000", "pp": "true", "ppPercent": "2",
	})
	buyMsg := ppMessage(ws)
	if !strings.Contains(buyMsg, "2%") {
		t.Fatalf("a custom --pp-percent 2 should render '2%%' in the message, got: %q", buyMsg)
	}
	if !strings.Contains(buyMsg, "up to") || strings.Contains(buyMsg, "down to") {
		t.Fatalf("a buy-side pp bound is a ceiling and must render 'up to', got: %q", buyMsg)
	}
}

// ppMessage returns the rendered message of the PRICE_PROTECTION_CAPPED warning,
// or "" if none is present.
func ppMessage(ws []PlaceWarning) string {
	for _, w := range ws {
		if w.Code == WarnPriceProtectionCapped {
			return w.Message
		}
	}
	return ""
}

func TestPrePlacePostOnlyAndFOKSimulationMatchesWarning(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}

	// Post-only that crosses: simulation must say REJECTED, not a partial fill.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10001000", "qty": "1", "timeInForce": "po",
	})
	if sim.EstFilledQty != "" || !strings.Contains(sim.Disposition, "REJECTED") {
		t.Fatalf("a crossing post-only must simulate as rejected with no fill: %+v", sim)
	}

	// Fill-or-kill larger than the crossing book: simulation must say KILLED.
	sim = runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10001000", "qty": "5", "timeInForce": "fok",
	})
	if sim.EstFilledQty != "" || !strings.Contains(sim.Disposition, "KILLED") {
		t.Fatalf("an unfillable fill-or-kill must simulate as killed with no fill: %+v", sim)
	}
}

func TestPrePlaceIOCLimitRemainderCanceled(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// A marketable IOC limit that can't fully fill: the remainder is CANCELED,
	// not rested.
	sim := runSim(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10001000", "qty": "5", "timeInForce": "ioc",
	})
	if sim.EstRemainingQty == "" || !strings.Contains(sim.Disposition, "canceled") {
		t.Fatalf("an IOC limit remainder should be canceled, not rested: %+v", sim)
	}
}

func TestPrePlaceIOCWouldExpire(t *testing.T) {
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick}
	// An IOC limit BUY priced below the best ask (10,001,000) does not cross the
	// book: it takes no liquidity and expires immediately with no fill.
	values := map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9000000", "qty": "1", "timeInForce": "ioc",
	}
	ws := run(t, doer, values)
	if !hasCode(ws, "IOC_WOULD_EXPIRE") {
		t.Fatalf("a non-crossing IOC limit should warn IOC_WOULD_EXPIRE, got: %s", codes(ws))
	}
	sim := runSim(t, doer, values)
	if sim.Marketable {
		t.Fatalf("a non-crossing IOC must not be marketable: %+v", sim)
	}
	if !strings.Contains(sim.Disposition, "EXPIRED") || sim.EstRemainingQty != "1" {
		t.Fatalf("a non-crossing IOC should report the full qty as EXPIRED: %+v", sim)
	}

	// The sell side too: an IOC limit SELL priced above the best bid (9,999,000)
	// does not cross, so it must also warn IOC_WOULD_EXPIRE and report EXPIRED.
	sellValues := map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "limit",
		"price": "10500000", "qty": "1", "timeInForce": "ioc",
	}
	ws = run(t, doer, sellValues)
	if !hasCode(ws, "IOC_WOULD_EXPIRE") {
		t.Fatalf("a non-crossing IOC limit SELL should warn IOC_WOULD_EXPIRE, got: %s", codes(ws))
	}
	if sim := runSim(t, doer, sellValues); sim.Marketable || !strings.Contains(sim.Disposition, "EXPIRED") {
		t.Fatalf("a non-crossing IOC SELL should be non-marketable and EXPIRED: %+v", sim)
	}

	// The same non-crossing price WITHOUT --tif ioc rests as a maker limit and
	// must NOT warn — the expiry warning is IOC-specific.
	ws = run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "9000000", "qty": "1",
	})
	if hasCode(ws, "IOC_WOULD_EXPIRE") {
		t.Fatalf("a non-crossing resting limit (no --tif ioc) must not warn IOC_WOULD_EXPIRE: %s", codes(ws))
	}
}

func TestPrePlaceSkipsWhenBookUnavailable(t *testing.T) {
	doer := fakeMarketDoer{err: context.DeadlineExceeded}
	_, _, err := PrePlaceCheck(context.Background(), rawapi.New(doer, nil), map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "50000", "accountSeq": "1",
	})
	if err == nil {
		t.Fatalf("expected an error so the caller can note the checks were skipped")
	}
}
