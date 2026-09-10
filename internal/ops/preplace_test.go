// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
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

func (d fakeMarketDoer) Do(_ context.Context, call apiclient.Call, _ apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
	if d.err != nil {
		return nil, apiclient.Meta{}, d.err
	}
	switch call.Path {
	case "/v2/orderbook":
		return json.RawMessage(d.orderbook), apiclient.Meta{}, nil
	case "/v2/tickSizePolicy":
		return json.RawMessage(d.tickSize), apiclient.Meta{}, nil
	case "/v2/currencyPairs":
		if d.pairs == "" {
			return json.RawMessage(`[]`), apiclient.Meta{}, nil
		}
		return json.RawMessage(d.pairs), apiclient.Meta{}, nil
	}
	return json.RawMessage(`{}`), apiclient.Meta{}, nil
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

// ---- empty and one-sided books ----
//
// A book with an empty side is analyzed, not refused. These tests pin both halves
// of that: the checks that do not need the missing price still run (losing them
// silently is what costs a caller money), and the ones that do are skipped rather
// than fed a fabricated zero — a zero best is a real, extremely favorable price,
// so every comparison against it answers confidently and wrongly.

// The two-sided fixtures the degradation cases take one side of: a mid of
// 10,000,000, a tick of 1000, and the KRW bounds.
var (
	degradeBids   = []BookLevel{{Price: "9999000", Qty: "2"}, {Price: "9990000", Qty: "5"}}
	degradeAsks   = []BookLevel{{Price: "10001000", Qty: "2"}, {Price: "10100000", Qty: "5"}}
	degradeBands  = []TickBand{{PriceGte: "0", TickSize: "1000"}}
	degradeBounds = OrderValueBounds{QuoteCurrency: "krw", Min: "5000", Max: "1000000000"}
)

// analyze runs the pure analysis over a caller-supplied book. AnalyzePlace has
// no failure mode and therefore no error return — whatever the book's shape,
// there is an answer.
func analyze(t *testing.T, values map[string]string, bids, asks []BookLevel) (PlaceSimulation, []PlaceWarning) {
	t.Helper()
	return AnalyzePlace(values, bids, asks, degradeBands, degradeBounds)
}

// orderKind is one cell of the order-type × tif matrix the degradation tests run.
//
// restsEmpty/restsOneSided say whether the order can actually rest as a maker
// when the whole book is empty / when only the side it takes from is: a post-only
// BEST order pegs to its own (queue) side, so it rests on a one-sided book that
// prices that side and cannot on an empty one — the two differ for it alone.
//
// warnsNoOpposing is whether an empty fill side warrants NO_OPPOSING_LIQUIDITY,
// which carries one meaning only: nothing would execute. So it is false for
// everything that can rest, and false for a post-only best order in BOTH books —
// it either rests, or is unpriceable and reports BEST_PEG_UNAVAILABLE instead.
type orderKind struct {
	name            string
	typ, tif        string
	restsEmpty      bool
	restsOneSided   bool
	warnsNoOpposing bool
	midCheckApplies bool // a mid-derived check would have run (every limit)
}

var orderKinds = []orderKind{
	{name: "market", typ: "market", warnsNoOpposing: true},
	{name: "limit-gtc", typ: "limit", tif: "gtc", restsEmpty: true, restsOneSided: true, midCheckApplies: true},
	{name: "limit-po", typ: "limit", tif: "po", restsEmpty: true, restsOneSided: true, midCheckApplies: true},
	{name: "limit-ioc", typ: "limit", tif: "ioc", warnsNoOpposing: true, midCheckApplies: true},
	{name: "limit-fok", typ: "limit", tif: "fok", warnsNoOpposing: true, midCheckApplies: true},
	{name: "best-gtc", typ: "best", tif: "gtc", warnsNoOpposing: true},
	{name: "best-po", typ: "best", tif: "po", restsOneSided: true},
}

// values builds the wire params for this kind on a side, sized so nothing but the
// book is in question: an on-grid price and a notional inside the bounds.
func (k orderKind) values(side string) map[string]string {
	v := map[string]string{"symbol": "btc_krw", "side": side, "orderType": k.typ, "accountSeq": "1"}
	if k.tif != "" {
		v["timeInForce"] = k.tif
	}
	switch {
	case k.typ == "limit":
		v["price"], v["qty"] = "10000000", "0.01"
	case side == "buy": // a market/best BUY is sized in the quote currency
		v["amt"] = "100000"
	default:
		v["qty"] = "0.01"
	}
	if k.typ == "best" {
		v["bestNth"] = "1"
	}
	return v
}

// spuriousCodes are the warnings that can only be true of a book with prices on
// the side in question. None of them may appear when that side is empty — each
// would be produced by comparing against a zero.
var spuriousCodes = []string{
	"PRICE_PROTECTION_CAPPED", "PRICE_FAR_ABOVE_MARKET", "PRICE_FAR_BELOW_MARKET",
	"POST_ONLY_WOULD_REJECT", "HIGH_SLIPPAGE", "INSUFFICIENT_LIQUIDITY", "BOOK_DEPTH_LIMITED",
}

// assertNoSpurious fails on any warning that needs a price the book did not have,
// and on any message that quotes a zero as a reference price.
func assertNoSpurious(t *testing.T, ws []PlaceWarning) {
	t.Helper()
	for _, code := range spuriousCodes {
		if hasCode(ws, code) {
			t.Errorf("%s cannot be concluded without a price on that side of the book: %s", code, codes(ws))
		}
	}
	for _, w := range ws {
		for _, zero := range []string{"best ask 0", "best bid 0", "mid 0", "of the mid 0"} {
			if strings.Contains(w.Message, zero) {
				t.Errorf("%s quotes a fabricated zero reference price: %q", w.Code, w.Message)
			}
		}
	}
}

// TestAnalyzePlaceEmptyBookDegradesGracefully: a newly listed pair with no
// resting orders at all. Every order type on both sides must produce a
// simulation, name the missing liquidity, and fabricate nothing.
func TestAnalyzePlaceEmptyBookDegradesGracefully(t *testing.T) {
	for _, k := range orderKinds {
		for _, side := range []string{"buy", "sell"} {
			t.Run(k.name+"/"+side, func(t *testing.T) {
				sim, ws := analyze(t, k.values(side), nil, nil)

				if sim.BestBid != "" || sim.BestAsk != "" || sim.Mid != "" {
					t.Fatalf("an empty book has no reference price to report, got bid %q ask %q mid %q", sim.BestBid, sim.BestAsk, sim.Mid)
				}
				if sim.Disclaimer == "" {
					t.Fatalf("the estimate-only disclaimer must survive every degraded state")
				}
				if got := hasCode(ws, "NO_OPPOSING_LIQUIDITY"); got != k.warnsNoOpposing {
					t.Fatalf("NO_OPPOSING_LIQUIDITY = %v, want %v (it means nothing would execute — never the ordinary first-maker outcome): %s", got, k.warnsNoOpposing, codes(ws))
				}
				if k.typ == "best" && k.tif == "po" && !hasCode(ws, "BEST_PEG_UNAVAILABLE") {
					// A po best order pegs to its own side, which is empty here: the peg
					// failure is the accurate reason and stands in for the liquidity one.
					t.Fatalf("an unpriceable post-only best order must report BEST_PEG_UNAVAILABLE: %s", codes(ws))
				}
				if got := hasCode(ws, "MID_PRICE_UNAVAILABLE"); got != k.midCheckApplies {
					t.Fatalf("MID_PRICE_UNAVAILABLE = %v, want %v (report a suppressed check, stay silent where the mid is unused): %s", got, k.midCheckApplies, codes(ws))
				}
				assertNoSpurious(t, ws)

				if sim.Marketable || sim.FullyFilled || sim.EstFilledQty != "" || sim.EstAvgFillPrice != "" {
					t.Fatalf("nothing can fill against an empty book: %+v", sim)
				}
				if sim.Disposition == "" {
					t.Fatalf("the caller must be told what becomes of the order: %+v", sim)
				}
				if strings.Contains(sim.Disposition, "exhausted") {
					t.Fatalf("a book that was never populated did not run out: %q", sim.Disposition)
				}
				if rests := strings.Contains(sim.Disposition, "rests"); rests != k.restsEmpty {
					t.Fatalf("disposition %q: rests=%v, want %v", sim.Disposition, rests, k.restsEmpty)
				}
			})
		}
	}
}

// TestAnalyzePlaceOneSidedBookFillSideEmpty: the side the order takes from is
// empty while the other side has depth. The mid is still unavailable, but the
// book does carry one real price — which must be reported as itself and never
// mirrored onto the missing side.
func TestAnalyzePlaceOneSidedBookFillSideEmpty(t *testing.T) {
	for _, k := range orderKinds {
		for _, side := range []string{"buy", "sell"} {
			t.Run(k.name+"/"+side, func(t *testing.T) {
				// A buy takes from the asks, a sell from the bids: keep the other side.
				bids, asks := degradeBids, []BookLevel(nil)
				if side == "sell" {
					bids, asks = nil, degradeAsks
				}
				sim, ws := analyze(t, k.values(side), bids, asks)

				if side == "buy" && (sim.BestBid != "9999000" || sim.BestAsk != "" || sim.Mid != "") {
					t.Fatalf("bids-only book: want the real bid and nothing else, got bid %q ask %q mid %q", sim.BestBid, sim.BestAsk, sim.Mid)
				}
				if side == "sell" && (sim.BestAsk != "10001000" || sim.BestBid != "" || sim.Mid != "") {
					t.Fatalf("asks-only book: want the real ask and nothing else, got bid %q ask %q mid %q", sim.BestBid, sim.BestAsk, sim.Mid)
				}
				if got := hasCode(ws, "NO_OPPOSING_LIQUIDITY"); got != k.warnsNoOpposing {
					t.Fatalf("NO_OPPOSING_LIQUIDITY = %v, want %v — an order that rests is not warned, one that cannot execute is: %s", got, k.warnsNoOpposing, codes(ws))
				}
				if got := hasCode(ws, "MID_PRICE_UNAVAILABLE"); got != k.midCheckApplies {
					t.Fatalf("MID_PRICE_UNAVAILABLE = %v, want %v: %s", got, k.midCheckApplies, codes(ws))
				}
				assertNoSpurious(t, ws)
				if sim.Marketable || sim.EstFilledQty != "" {
					t.Fatalf("an empty fill side fills nothing, whatever the other side holds: %+v", sim)
				}
				if rests := strings.Contains(sim.Disposition, "rests"); rests != k.restsOneSided {
					t.Fatalf("disposition %q: rests=%v, want %v", sim.Disposition, rests, k.restsOneSided)
				}
			})
		}
	}
}

// TestAnalyzePlaceOneSidedBookFillSidePopulated: a one-sided book is NOT
// uniformly degraded. When the side the order takes from is the populated one,
// the sweep is exactly as valid as on a two-sided book — only the mid-derived
// checks are lost, and only MID_PRICE_UNAVAILABLE is added.
func TestAnalyzePlaceOneSidedBookFillSidePopulated(t *testing.T) {
	cases := []struct {
		name        string
		bids, asks  []BookLevel
		values      map[string]string
		wantFill    string // EstFilledQty
		wantAvg     string // EstAvgFillPrice
		wantWarning string // the ONLY warning code allowed ("" = none at all)
	}{
		{
			name: "market sell into a bids-only book", bids: degradeBids,
			values:   map[string]string{"symbol": "btc_krw", "side": "sell", "orderType": "market", "qty": "0.01"},
			wantFill: "0.01", wantAvg: "9999000",
		},
		{
			name: "market buy into an asks-only book", asks: degradeAsks,
			values:   map[string]string{"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "100010"},
			wantFill: "0.01", wantAvg: "10001000",
		},
		{
			name: "crossing limit sell into a bids-only book", bids: degradeBids,
			values: map[string]string{"symbol": "btc_krw", "side": "sell", "orderType": "limit",
				"price": "9990000", "qty": "0.01", "timeInForce": "gtc"},
			wantFill: "0.01", wantAvg: "9999000", wantWarning: "MID_PRICE_UNAVAILABLE",
		},
		{
			name: "crossing limit buy into an asks-only book", asks: degradeAsks,
			values: map[string]string{"symbol": "btc_krw", "side": "buy", "orderType": "limit",
				"price": "10100000", "qty": "0.01", "timeInForce": "gtc"},
			wantFill: "0.01", wantAvg: "10001000", wantWarning: "MID_PRICE_UNAVAILABLE",
		},
		{
			// A price a mid WOULD have flagged as a fat finger (90% below the market).
			// With no mid the check cannot run, and must not run on a guess.
			name: "far-below limit sell into a bids-only book", bids: degradeBids,
			values: map[string]string{"symbol": "btc_krw", "side": "sell", "orderType": "limit",
				"price": "1000000", "qty": "1", "timeInForce": "gtc"},
			wantFill: "1", wantAvg: "9999000", wantWarning: "MID_PRICE_UNAVAILABLE",
		},
		{
			// The mirror image: an extra digit on a buy, with no mid to compare to.
			name: "far-above limit buy into an asks-only book", asks: degradeAsks,
			values: map[string]string{"symbol": "btc_krw", "side": "buy", "orderType": "limit",
				"price": "100000000", "qty": "0.001", "timeInForce": "gtc"},
			wantFill: "0.001", wantAvg: "10001000", wantWarning: "MID_PRICE_UNAVAILABLE",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.values["accountSeq"] = "1"
			sim, ws := analyze(t, c.values, c.bids, c.asks)

			if !sim.Marketable || !sim.FullyFilled || sim.EstFilledQty != c.wantFill || sim.EstAvgFillPrice != c.wantAvg {
				t.Fatalf("the sweep must run in full against the populated side: want fill %s at %s, got %+v", c.wantFill, c.wantAvg, sim)
			}
			if hasCode(ws, "NO_OPPOSING_LIQUIDITY") {
				t.Fatalf("the side this order takes from HAS liquidity: %s", codes(ws))
			}
			assertNoSpurious(t, ws)
			if c.wantWarning == "" {
				if len(ws) != 0 {
					t.Fatalf("a mid-independent order against a populated fill side needs no warning: %s", codes(ws))
				}
				return
			}
			if got := codes(ws); got != c.wantWarning {
				t.Fatalf("only the suppressed mid check may be added, got: %s", got)
			}
		})
	}
}

// TestAnalyzePlaceNoMidDoesNotEngagePriceProtection: price protection is a band
// around the mid, so with no mid it is not in effect. The bug this pins is worse
// than a missing check: a band around zero excludes every level, so the sweep
// broke on its first ask and reported a fully fillable buy as protection-canceled.
func TestAnalyzePlaceNoMidDoesNotEngagePriceProtection(t *testing.T) {
	values := map[string]string{"symbol": "btc_krw", "side": "buy", "orderType": "market",
		"amt": "100010", "pp": "true", "accountSeq": "1"}
	sim, ws := analyze(t, values, nil, degradeAsks)

	if hasCode(ws, "PRICE_PROTECTION_CAPPED") {
		t.Fatalf("protection cannot bind without a mid to measure it from: %s", codes(ws))
	}
	if !sim.Marketable || sim.EstFilledQty != "0.01" {
		t.Fatalf("the ask side has depth, so the sweep must fill against it: %+v", sim)
	}
	if strings.Contains(sim.Disposition, "price protection") {
		t.Fatalf("disposition must not blame price protection: %q", sim.Disposition)
	}
	// The caller ASKED for protection and did not get it — that has to be said.
	if !hasCode(ws, "MID_PRICE_UNAVAILABLE") {
		t.Fatalf("a requested --pp that could not be evaluated must be reported: %s", codes(ws))
	}
	// ...and it stays silent for the same order without --pp, where the mid is unused.
	delete(values, "pp")
	if _, plain := analyze(t, values, nil, degradeAsks); hasCode(plain, "MID_PRICE_UNAVAILABLE") {
		t.Fatalf("an order that never uses the mid must not be warned about it: %s", codes(plain))
	}
}

// TestAnalyzePlaceNoMidStaysSilentOnAPricelessLimitDraft: MID_PRICE_UNAVAILABLE
// reports a check the missing mid suppressed, so it takes a price that check would
// have examined. A limit draft carrying none yet — a UI panel on a pair the user
// has typed nothing into — had no price sanity check to suppress, and the mid is
// not what stopped it.
func TestAnalyzePlaceNoMidStaysSilentOnAPricelessLimitDraft(t *testing.T) {
	_, ws := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "timeInForce": "gtc", "accountSeq": "1",
	}, nil, nil)
	if hasCode(ws, "MID_PRICE_UNAVAILABLE") {
		t.Fatalf("no price to sanity-check means no suppressed check to report: %s", codes(ws))
	}
	// The same draft once a price is entered: now the check IS one the mid stopped.
	if _, ws := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10000000", "timeInForce": "gtc", "accountSeq": "1",
	}, nil, nil); !hasCode(ws, "MID_PRICE_UNAVAILABLE") {
		t.Fatalf("want MID_PRICE_UNAVAILABLE once there is a price to check: %s", codes(ws))
	}
}

// TestAnalyzePlaceNoMidWithProtectionOnEveryOrderType: the same gate in the
// market, best and limit branches — a requested --pp on a book with no mid must
// never produce a protection verdict anywhere.
func TestAnalyzePlaceNoMidWithProtectionOnEveryOrderType(t *testing.T) {
	for _, k := range orderKinds {
		for _, side := range []string{"buy", "sell"} {
			for _, book := range []string{"empty", "one-sided"} {
				t.Run(k.name+"/"+side+"/"+book, func(t *testing.T) {
					var bids, asks []BookLevel
					if book == "one-sided" {
						// The populated side is the one the order takes from, so a
						// zero-mid band would have something to wrongly exclude.
						if side == "buy" {
							asks = degradeAsks
						} else {
							bids = degradeBids
						}
					}
					values := k.values(side)
					values["pp"], values["ppPercent"] = "true", "2"
					sim, ws := analyze(t, values, bids, asks)
					if hasCode(ws, "PRICE_PROTECTION_CAPPED") {
						t.Fatalf("no mid, no protection verdict: %s", codes(ws))
					}
					if strings.Contains(sim.Disposition, "price protection") {
						t.Fatalf("no mid, no protection disposition: %q", sim.Disposition)
					}
					if !hasCode(ws, "MID_PRICE_UNAVAILABLE") {
						t.Fatalf("a requested --pp that could not be evaluated must be reported: %s", codes(ws))
					}
				})
			}
		}
	}
}

// TestAnalyzePlaceEmptyBookNoSpuriousNotionalOrRejection covers the two hazards
// that are specific to one order shape each: a market/best SELL is the only order
// whose value comes from the book, and a post-only order is the only one whose
// verdict comes from comparing against the opposing best.
func TestAnalyzePlaceEmptyBookNoSpuriousNotionalOrRejection(t *testing.T) {
	// A market SELL is valued at the best bid. With no bids it cannot be valued at
	// all — and an unvaluable order must not be reported as below the minimum.
	sim, ws := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "market", "qty": "0.01", "accountSeq": "1",
	}, nil, degradeAsks)
	if sim.Notional != "" {
		t.Fatalf("a market sell with no bid to value it against has no notional, got %q", sim.Notional)
	}
	if hasCode(ws, "NOTIONAL_BELOW_MIN") || hasCode(ws, "NOTIONAL_ABOVE_MAX") {
		t.Fatalf("a bound cannot be checked against a price we do not have: %s", codes(ws))
	}
	// A market BUY is sized in quote by --amt, so it IS valuable with no book at
	// all — and its bound checks must still run.
	sim, ws = analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "amt": "100", "accountSeq": "1",
	}, nil, nil)
	if sim.Notional != "100" || !hasCode(ws, "NOTIONAL_BELOW_MIN") {
		t.Fatalf("an amt-sized buy is valuable without a book, so its bounds still apply: %+v / %s", sim, codes(ws))
	}

	// A post-only order is rejected only if it CROSSES the opposing best. With no
	// opposing best nothing is crossed, on either the limit or the best path — and
	// resting is the ordinary outcome, so neither is warned about liquidity.
	for _, values := range []map[string]string{
		{"symbol": "btc_krw", "side": "buy", "orderType": "best", "timeInForce": "po", "bestNth": "1", "amt": "100000"},
		{"symbol": "btc_krw", "side": "buy", "orderType": "limit", "timeInForce": "po", "price": "10000000", "qty": "0.01"},
	} {
		values["accountSeq"] = "1"
		sim, ws := analyze(t, values, degradeBids, nil)
		if hasCode(ws, "POST_ONLY_WOULD_REJECT") {
			t.Fatalf("%s po: nothing to cross, so nothing to reject: %s", values["orderType"], codes(ws))
		}
		if hasCode(ws, "NO_OPPOSING_LIQUIDITY") {
			t.Fatalf("%s po: an order that rests as a maker must not be told nothing would execute: %s", values["orderType"], codes(ws))
		}
		if strings.Contains(sim.Disposition, "REJECTED") {
			t.Fatalf("%s po: %q", values["orderType"], sim.Disposition)
		}
		if !strings.Contains(sim.Disposition, "rests") {
			t.Fatalf("%s po: a post-only order with no opposing side rests as a maker: %q", values["orderType"], sim.Disposition)
		}
	}
}

// TestAnalyzePlaceBestOrderIsValuedAtItsPeg: a best (BBO) order rests at — or
// crosses to — its own peg, so the peg is the price its value is measured at, not
// the opposing best. Two things ride on that. On a two-sided book the figure is
// simply the right one. On a book with no opposing side the peg is the ONLY price
// there is, so valuing the order against the opposing best instead leaves the
// notional empty and silently skips BOTH order-value bound checks — on exactly the
// order a first maker sizing small on a new listing places, where the simulation
// otherwise reports a real peg and a "rests at …" disposition and the dry-run
// emits no warning at all.
func TestAnalyzePlaceBestOrderIsValuedAtItsPeg(t *testing.T) {
	poSell := func(qty string) map[string]string {
		return map[string]string{"symbol": "btc_krw", "side": "sell", "orderType": "best",
			"timeInForce": "po", "bestNth": "1", "qty": qty, "accountSeq": "1"}
	}
	// Asks only, no bids at all: a po SELL pegs to its own (ask) side and rests
	// there. 0.0001 valued at that peg is 1000.1 — under the 5000 minimum.
	sim, ws := analyze(t, poSell("0.0001"), nil, degradeAsks)
	if sim.EstPegPrice != "10001000" || !strings.Contains(sim.Disposition, "rests") {
		t.Fatalf("a po best sell pegs to its own (ask) side and rests: %+v", sim)
	}
	if sim.Notional != "1000.1" {
		t.Fatalf("notional must be the qty valued at the peg: %q, want 1000.1", sim.Notional)
	}
	if !hasCode(ws, "NOTIONAL_BELOW_MIN") {
		t.Fatalf("a peg-priced order under the minimum must be warned, got: %s", codes(ws))
	}
	// The other bound, same book: the missing bid side suppresses neither.
	if _, ws := analyze(t, poSell("1000"), nil, degradeAsks); !hasCode(ws, "NOTIONAL_ABOVE_MAX") {
		t.Fatalf("want NOTIONAL_ABOVE_MAX, got: %s", codes(ws))
	}
	// On a full book a taker best SELL pegs to the --best-nth level of the opposing
	// side, which is not the best bid: the value follows the peg it crosses to.
	sim, _ = analyze(t, map[string]string{"symbol": "btc_krw", "side": "sell", "orderType": "best",
		"timeInForce": "gtc", "bestNth": "2", "qty": "0.01", "accountSeq": "1"}, degradeBids, degradeAsks)
	if sim.EstPegPrice != "9990000" || sim.Notional != "99900" {
		t.Fatalf("a taker best sell is valued at its peg (the 2nd bid): peg %q notional %q", sim.EstPegPrice, sim.Notional)
	}
	// A best order whose --best-nth level is not visible has no peg at all, so a
	// base-sized SELL has nothing to be valued at: BEST_PEG_UNAVAILABLE is the
	// finding, and a value taken from a price this order would never get would read
	// as a checked one.
	sim, ws = analyze(t, map[string]string{"symbol": "btc_krw", "side": "sell", "orderType": "best",
		"timeInForce": "gtc", "bestNth": "9", "qty": "0.01", "accountSeq": "1"}, degradeBids, degradeAsks)
	if sim.Notional != "" || !hasCode(ws, "BEST_PEG_UNAVAILABLE") {
		t.Fatalf("an unpriceable best sell has no value to report: notional %q / %s", sim.Notional, codes(ws))
	}
	// A best BUY is sized by --amt, which is already quote-denominated, so it needs
	// no reference price: its bounds hold even where no peg can be derived.
	_, ws = analyze(t, map[string]string{"symbol": "btc_krw", "side": "buy", "orderType": "best",
		"timeInForce": "gtc", "bestNth": "1", "amt": "100", "accountSeq": "1"}, nil, nil)
	if !hasCode(ws, "NOTIONAL_BELOW_MIN") {
		t.Fatalf("an amt-sized best buy is valuable without any book, got: %s", codes(ws))
	}
}

// TestAnalyzePlacePostOnlyBestPegSideDecidesOnOneSidedBook: a best order takes its
// price from the OPPOSING side for a taker tif but from its OWN queue side for po,
// so a one-sided book treats the two po sides oppositely. On a bids-only book a po
// BUY pegs to the best bid and rests; a po SELL, whose queue side is the empty
// one, cannot be priced at all — and the peg failure, not the missing liquidity,
// is what it is told.
func TestAnalyzePlacePostOnlyBestPegSideDecidesOnOneSidedBook(t *testing.T) {
	buy, buyWs := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "best",
		"timeInForce": "po", "bestNth": "1", "amt": "100000", "accountSeq": "1",
	}, degradeBids, nil)
	if buy.EstPegPrice != "9999000" {
		t.Fatalf("a po best buy pegs to the best bid even with no asks: %+v", buy)
	}
	if !strings.Contains(buy.Disposition, "rests") || buy.Marketable {
		t.Fatalf("a po best buy priced off a populated bid side rests: %+v", buy)
	}
	if hasCode(buyWs, "NO_OPPOSING_LIQUIDITY") || hasCode(buyWs, "POST_ONLY_WOULD_REJECT") || hasCode(buyWs, "BEST_PEG_UNAVAILABLE") {
		t.Fatalf("a po best buy that rests at its peg needs none of these: %s", codes(buyWs))
	}

	sell, sellWs := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "best",
		"timeInForce": "po", "bestNth": "1", "qty": "0.01", "accountSeq": "1",
	}, degradeBids, nil)
	if !hasCode(sellWs, "BEST_PEG_UNAVAILABLE") {
		t.Fatalf("a po best sell whose queue (ask) side is empty cannot be priced: %s", codes(sellWs))
	}
	if hasCode(sellWs, "NO_OPPOSING_LIQUIDITY") {
		t.Fatalf("the peg failure is the accurate reason; do not stack a liquidity verdict on it: %s", codes(sellWs))
	}
	if sell.EstPegPrice != "" || sell.Marketable {
		t.Fatalf("an unpriceable po best sell must fabricate no peg: %+v", sell)
	}
}

// TestAnalyzePlaceEmptyBookCoEmitsCauseAndConsequence: NO_OPPOSING_LIQUIDITY is
// the cause, the tif verdict is the consequence, and the caller needs both. A --pp
// branch must not preempt the tif verdict either, so the pp variants are pinned too.
func TestAnalyzePlaceEmptyBookCoEmitsCauseAndConsequence(t *testing.T) {
	cases := []struct {
		tif, want string
	}{
		{"ioc", "IOC_WOULD_EXPIRE"},
		{"fok", "FOK_WOULD_KILL"},
	}
	for _, c := range cases {
		for _, side := range []string{"buy", "sell"} {
			for _, pp := range []string{"", "true"} {
				name := c.tif + "/" + side
				if pp != "" {
					name += "/pp"
				}
				t.Run(name, func(t *testing.T) {
					for _, book := range []string{"empty", "fill-side-empty"} {
						var bids, asks []BookLevel
						if book == "fill-side-empty" {
							if side == "buy" {
								bids = degradeBids
							} else {
								asks = degradeAsks
							}
						}
						values := map[string]string{"symbol": "btc_krw", "side": side, "orderType": "limit",
							"price": "10000000", "qty": "0.01", "timeInForce": c.tif, "accountSeq": "1"}
						if pp != "" {
							values["pp"] = pp
						}
						_, ws := analyze(t, values, bids, asks)
						if !hasCode(ws, "NO_OPPOSING_LIQUIDITY") {
							t.Fatalf("%s: want the cause NO_OPPOSING_LIQUIDITY, got: %s", book, codes(ws))
						}
						if !hasCode(ws, c.want) {
							t.Fatalf("%s: want the consequence %s alongside it, got: %s", book, c.want, codes(ws))
						}
						// The cause must name the TIF, not just the order type. An
						// otherwise identical gtc limit rests here, so a message saying
						// only "this limit <side>" reads as a claim about every limit
						// order and contradicts the resting case.
						for _, w := range ws {
							if w.Code != WarnNoOpposingLiquidity {
								continue
							}
							if !strings.Contains(w.Message, c.tif+" limit") {
								t.Errorf("%s: NO_OPPOSING_LIQUIDITY must name the tif that blocks resting, got: %s", book, w.Message)
							}
						}
						assertNoSpurious(t, ws)
					}
				})
			}
		}
	}
}

// TestAnalyzePlaceEmptyBookStillChecksNotionalAndTick is the costly-omission
// guard: a first maker sizing small to test a new listing gets the same
// order-value and tick-grid rejection warnings on an empty book as anywhere else.
// Neither check needs a book price. What such an order must NOT get is a
// nothing-would-execute verdict — it rests; the actionable signal is that no
// price-sanity check could run.
func TestAnalyzePlaceEmptyBookStillChecksNotionalAndTick(t *testing.T) {
	for _, tif := range []string{"gtc", "po"} {
		// Off the 1000 tick grid, and 1000.05 KRW is under the 5000 minimum.
		sim, ws := analyze(t, map[string]string{
			"symbol": "btc_krw", "side": "buy", "orderType": "limit",
			"price": "10000500", "qty": "0.0001", "timeInForce": tif, "accountSeq": "1",
		}, nil, nil)
		if sim.Notional != "1000.05" {
			t.Fatalf("%s: a limit order carries its own price, so it is always valuable: %+v", tif, sim)
		}
		if !hasCode(ws, "NOTIONAL_BELOW_MIN") {
			t.Fatalf("%s: want NOTIONAL_BELOW_MIN on an empty book, got: %s", tif, codes(ws))
		}
		if !hasCode(ws, "PRICE_OFF_TICK") {
			t.Fatalf("%s: want PRICE_OFF_TICK on an empty book, got: %s", tif, codes(ws))
		}
		if !hasCode(ws, "MID_PRICE_UNAVAILABLE") {
			t.Fatalf("%s: a resting limit's actionable signal is the unrun price check, got: %s", tif, codes(ws))
		}
		if hasCode(ws, "NO_OPPOSING_LIQUIDITY") {
			t.Fatalf("%s: a first maker executes nothing BY DESIGN; that is not a warning: %s", tif, codes(ws))
		}
		if sim.EstRemainingQty != "0.0001" || !strings.Contains(sim.Disposition, "rests") {
			t.Fatalf("%s: the whole quantity rests, and the simulation must say so: %+v", tif, sim)
		}
	}
	// The other bound too, on the sell side.
	_, ws := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "sell", "orderType": "limit",
		"price": "10000000", "qty": "1000", "timeInForce": "gtc", "accountSeq": "1",
	}, nil, nil)
	if !hasCode(ws, "NOTIONAL_ABOVE_MAX") {
		t.Fatalf("want NOTIONAL_ABOVE_MAX on an empty book, got: %s", codes(ws))
	}
}

// TestAnalyzePlaceUnavailablePricesMarshalEmpty pins the JSON shape an agent
// reads: the three reference-price keys are always present, and an unavailable
// price is the empty string — never "0", which would parse as a real price.
func TestAnalyzePlaceUnavailablePricesMarshalEmpty(t *testing.T) {
	sim, _ := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10000000", "qty": "0.01", "accountSeq": "1",
	}, nil, nil)
	b, err := json.Marshal(sim)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"bestBid":""`, `"bestAsk":""`, `"mid":""`} {
		if !strings.Contains(got, want) {
			t.Fatalf("want %s in the JSON document, got: %s", want, got)
		}
	}
	for _, bad := range []string{`"bestBid":"0"`, `"bestAsk":"0"`, `"mid":"0"`} {
		if strings.Contains(got, bad) {
			t.Fatalf("a fabricated zero price reached the JSON: %s", got)
		}
	}
}

// ---- the structural outcome token ----

// The book fixtures the outcome cases need beyond the two-sided degrade pair: a
// spread wider than a default 5% protection band (so protection excludes even the
// best opposing level), a book whose top levels are crossed (the only shape that
// makes a post-only BEST order marketable), and the partial-trim book.
var (
	wideBids    = []BookLevel{{Price: "9000000", Qty: "5"}}
	wideAsks    = []BookLevel{{Price: "11000000", Qty: "5"}}
	lockedBids  = []BookLevel{{Price: "10002000", Qty: "2"}}
	lockedAsks  = []BookLevel{{Price: "10001000", Qty: "2"}}
	trimmedBids = []BookLevel{{Price: "9999000", Qty: "2"}}
	trimmedAsks = []BookLevel{{Price: "10001000", Qty: "1"}, {Price: "11000000", Qty: "5"}}
)

// TestAnalyzePlaceOutcomeTokenPerBranch pins Outcome at every terminal branch of
// the analysis. The token is what a consumer BRANCHES on (a fee estimate, a UI
// label, an agent's decision) — Disposition is prose for a human and must never be
// pattern-matched, and Marketable answers a different question entirely — so a
// branch that sets the wrong token, or none, silently misstates the order.
func TestAnalyzePlaceOutcomeTokenPerBranch(t *testing.T) {
	cases := []struct {
		name       string
		bids, asks []BookLevel
		values     map[string]string
		want       PlaceOutcome
	}{
		// market
		{
			name: "market buy sweeps the asks", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "market", "amt": "100010"},
			want:   OutcomeFills,
		},
		{
			name: "market sell sweeps the bids", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "sell", "orderType": "market", "qty": "0.01"},
			want:   OutcomeFills,
		},
		{
			name:   "market buy with no asks at all",
			values: map[string]string{"side": "buy", "orderType": "market", "amt": "100010"},
			want:   OutcomeNothing,
		},
		{
			name: "market buy whose every level price protection excludes", bids: wideBids, asks: wideAsks,
			values: map[string]string{"side": "buy", "orderType": "market", "amt": "500000", "pp": "true"},
			want:   OutcomeNothing,
		},
		// limit
		{
			name: "non-crossing gtc limit", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "9000000", "qty": "0.01", "timeInForce": "gtc"},
			want:   OutcomeRests,
		},
		{
			name: "crossing gtc limit that fills part and rests the rest", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "10001000", "qty": "5", "timeInForce": "gtc"},
			want:   OutcomeFills,
		},
		{
			name: "gtc limit as the first maker on an empty book",
			values: map[string]string{"side": "buy", "orderType": "limit",
				"price": "10000000", "qty": "0.01", "timeInForce": "gtc"},
			want: OutcomeRests,
		},
		{
			name: "non-crossing post-only limit", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "9000000", "qty": "0.01", "timeInForce": "po"},
			want:   OutcomeRests,
		},
		{
			name: "crossing post-only limit (rejected)", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "10001000", "qty": "1", "timeInForce": "po"},
			want:   OutcomeNothing,
		},
		{
			name: "partially fillable fill-or-kill limit (killed)", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "10001000", "qty": "5", "timeInForce": "fok"},
			want:   OutcomeNothing,
		},
		{
			name: "fill-or-kill limit the crossing book covers", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "10001000", "qty": "2", "timeInForce": "fok"},
			want:   OutcomeFills,
		},
		{
			name: "non-crossing ioc limit (expired)", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "9000000", "qty": "0.01", "timeInForce": "ioc"},
			want:   OutcomeNothing,
		},
		{
			name: "crossing ioc limit that fills part and cancels the rest", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "10001000", "qty": "5", "timeInForce": "ioc"},
			want:   OutcomeFills,
		},
		{
			name: "ioc limit with no asks at all",
			values: map[string]string{"side": "buy", "orderType": "limit",
				"price": "10000000", "qty": "0.01", "timeInForce": "ioc"},
			want: OutcomeNothing,
		},
		{
			name: "crossing limit whose whole quantity price protection cancels", bids: wideBids, asks: wideAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "11000000", "qty": "1",
				"timeInForce": "gtc", "pp": "true"},
			want: OutcomeNothing,
		},
		{
			name: "crossing limit price protection only trims", bids: trimmedBids, asks: trimmedAsks,
			values: map[string]string{"side": "buy", "orderType": "limit", "price": "11000000", "qty": "3",
				"timeInForce": "gtc", "pp": "true"},
			want: OutcomeFills,
		},
		// best (BBO)
		{
			name: "post-only best resting at its queue peg", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "sell", "orderType": "best", "timeInForce": "po", "bestNth": "1", "qty": "0.01"},
			want:   OutcomeRests,
		},
		{
			name: "post-only best pegged across a crossed book (rejected)", bids: lockedBids, asks: lockedAsks,
			values: map[string]string{"side": "buy", "orderType": "best", "timeInForce": "po", "bestNth": "1", "amt": "100000"},
			want:   OutcomeNothing,
		},
		{
			name: "taker best filling at its peg", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "best", "timeInForce": "ioc", "bestNth": "1", "amt": "10000000"},
			want:   OutcomeFills,
		},
		{
			name: "fill-or-kill best over the peg depth (killed)", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "best", "timeInForce": "fok", "bestNth": "1", "amt": "30000000"},
			want:   OutcomeNothing,
		},
		{
			name: "best whose --best-nth level is not visible", bids: degradeBids, asks: degradeAsks,
			values: map[string]string{"side": "buy", "orderType": "best", "timeInForce": "ioc", "bestNth": "5", "amt": "10000000"},
			want:   OutcomeNothing,
		},
		{
			name: "post-only best whose queue side is empty", bids: degradeBids,
			values: map[string]string{"side": "sell", "orderType": "best", "timeInForce": "po", "bestNth": "1", "qty": "0.01"},
			want:   OutcomeNothing,
		},
		{
			name: "gtc best whose every level price protection excludes", bids: wideBids, asks: wideAsks,
			values: map[string]string{"side": "buy", "orderType": "best", "timeInForce": "gtc", "bestNth": "1",
				"amt": "500000", "pp": "true"},
			want: OutcomeNothing,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.values["symbol"], c.values["accountSeq"] = "btc_krw", "1"
			sim, _ := analyze(t, c.values, c.bids, c.asks)
			if sim.Outcome != c.want {
				t.Fatalf("outcome = %q, want %q: %+v", sim.Outcome, c.want, sim)
			}
			// The token is the disposition's structural twin, so an order that does not
			// execute in full must also say so in prose — a human reads that one.
			// (A fully filled order has no remainder to describe, hence no disposition.)
			if c.want != OutcomeFills && sim.Disposition == "" {
				t.Fatalf("an outcome without a disposition leaves a human with nothing to read: %+v", sim)
			}
		})
	}
}

// TestAnalyzePlaceOutcomeIsNotMarketable: the token exists because Marketable
// answers a different question — "would this take liquidity" — which is true of
// two orders that execute nothing at all. Keying a fee (or any decision) off
// Marketable charges a taker fee on an order the server never fills.
func TestAnalyzePlaceOutcomeIsNotMarketable(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
	}{
		{"crossing post-only", map[string]string{"side": "buy", "orderType": "limit",
			"price": "10001000", "qty": "1", "timeInForce": "po"}},
		{"partially fillable fill-or-kill", map[string]string{"side": "buy", "orderType": "limit",
			"price": "10001000", "qty": "5", "timeInForce": "fok"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.values["symbol"], c.values["accountSeq"] = "btc_krw", "1"
			sim, _ := analyze(t, c.values, degradeBids, degradeAsks)
			if !sim.Marketable {
				t.Fatalf("this order WOULD take liquidity — that is the trap the token exists for: %+v", sim)
			}
			if sim.Outcome != OutcomeNothing {
				t.Fatalf("outcome = %q, want %q — it executes nothing: %+v", sim.Outcome, OutcomeNothing, sim)
			}
		})
	}
}

// TestAnalyzePlaceOutcomeUnsetWithoutASize: the token is a verdict, so an order
// too incomplete to have one — a UI draft with no price or quantity typed yet —
// reports none rather than a default that reads as an answer.
func TestAnalyzePlaceOutcomeUnsetWithoutASize(t *testing.T) {
	sim, _ := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "timeInForce": "gtc", "accountSeq": "1",
	}, degradeBids, degradeAsks)
	if sim.Outcome != "" {
		t.Fatalf("outcome = %q, want empty: %+v", sim.Outcome, sim)
	}
	b, err := json.Marshal(sim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"outcome"`) {
		t.Fatalf("an absent verdict must be omitted from the JSON, not sent empty: %s", b)
	}
	// And it IS in the document once there is a verdict to report.
	sim, _ = analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9000000", "qty": "0.01", "timeInForce": "gtc", "accountSeq": "1",
	}, degradeBids, degradeAsks)
	b, err = json.Marshal(sim)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"outcome":"rests"`) {
		t.Fatalf("want the outcome token in the JSON document: %s", b)
	}
}

// TestAnalyzePlaceNoVerdictForASizelessDraftOnAnEmptyBook: the statement made from
// the BOOK's shape alone obeys the same rule as every other verdict — a request
// with no determinable size has none. An empty book is where this bites: it is the
// one path that answers before any sizing branch runs, so a draft mid-entry would
// otherwise be told it rests (or executes nothing) before it has a size at all.
func TestAnalyzePlaceNoVerdictForASizelessDraftOnAnEmptyBook(t *testing.T) {
	cases := []struct {
		name      string
		sizeless  map[string]string // the draft as a UI holds it mid-entry
		sizeField string            // the field that sizes THIS shape
		size      string
		want      PlaceOutcome // the verdict once it is sized
		wantRests bool         // and whether the disposition says it rests
	}{
		{
			name:      "gtc limit can rest",
			sizeless:  map[string]string{"side": "buy", "orderType": "limit", "timeInForce": "gtc"},
			sizeField: "qty", size: "0.01", want: OutcomeRests, wantRests: true,
		},
		{
			name:      "market buy cannot rest, and is sized by amt",
			sizeless:  map[string]string{"side": "buy", "orderType": "market"},
			sizeField: "amt", size: "100000", want: OutcomeNothing,
		},
		{
			name:      "market sell cannot rest, and is sized by qty",
			sizeless:  map[string]string{"side": "sell", "orderType": "market"},
			sizeField: "qty", size: "0.01", want: OutcomeNothing,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.sizeless["symbol"], c.sizeless["accountSeq"] = "btc_krw", "1"
			sim, ws := analyze(t, c.sizeless, nil, nil)
			if sim.Outcome != "" || sim.Disposition != "" {
				t.Fatalf("no size, no verdict: outcome=%q disposition=%q", sim.Outcome, sim.Disposition)
			}
			b, err := json.Marshal(sim)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{`"outcome"`, `"remainingDisposition"`} {
				if strings.Contains(string(b), key) {
					t.Fatalf("an absent verdict is omitted from the JSON, not sent empty: %s", b)
				}
			}
			// The book-shape ADVICE is not gated on the size — it is true of this book
			// and this tif while the size is still being typed.
			if got := hasCode(ws, "NO_OPPOSING_LIQUIDITY"); got != (c.want == OutcomeNothing) {
				t.Fatalf("NO_OPPOSING_LIQUIDITY = %v on a sizeless draft, want %v: %s", got, c.want == OutcomeNothing, codes(ws))
			}

			// The same draft with a size reports both — and keeps the empty-side wording
			// that this early statement exists for, never the sweep's "exhausted" (a book
			// with no resting orders did not run out).
			sized := map[string]string{c.sizeField: c.size}
			for k, v := range c.sizeless {
				sized[k] = v
			}
			sim, _ = analyze(t, sized, nil, nil)
			if sim.Outcome != c.want {
				t.Fatalf("outcome = %q, want %q: %+v", sim.Outcome, c.want, sim)
			}
			if !strings.Contains(sim.Disposition, "side of the book is empty") || strings.Contains(sim.Disposition, "exhausted") {
				t.Fatalf("the empty-side wording must win over the sweep-derived one: %q", sim.Disposition)
			}
			if rests := strings.Contains(sim.Disposition, "rests"); rests != c.wantRests {
				t.Fatalf("disposition %q: rests=%v, want %v", sim.Disposition, rests, c.wantRests)
			}
		})
	}
	// The gate must read the same field the sizing branch does: a market BUY spends a
	// quote amt, so a qty on one sizes nothing and leaves that draft without a verdict.
	sim, _ := analyze(t, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "market", "qty": "0.01", "accountSeq": "1",
	}, nil, nil)
	if sim.Outcome != "" || sim.Disposition != "" {
		t.Fatalf("a market buy is sized by amt, not qty: outcome=%q disposition=%q", sim.Outcome, sim.Disposition)
	}
}

// TestAnalyzePlaceMidUnavailableNamesOnlyTheChecksThatApplied: the warning reports
// checks the missing mid suppressed, so it must name the ones this order actually
// had. A plain limit never asked for price protection and a protected market order
// has no limit price to sanity-check; naming both there is true but reads as a lost
// check, which on a money path misleads as much as a hidden one.
func TestAnalyzePlaceMidUnavailableNamesOnlyTheChecksThatApplied(t *testing.T) {
	const limitCheck = "limit price sanity check"
	const ppCheck = "--pp price protection estimate"
	cases := []struct {
		name          string
		values        map[string]string
		want, notWant string
	}{
		{
			name: "a limit price, no protection requested",
			values: map[string]string{"side": "buy", "orderType": "limit",
				"price": "10000000", "qty": "0.01", "timeInForce": "gtc"},
			want: limitCheck, notWant: ppCheck,
		},
		{
			name:   "protection requested, no limit price to check",
			values: map[string]string{"side": "buy", "orderType": "market", "amt": "100000", "pp": "true"},
			want:   ppCheck, notWant: limitCheck,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.values["symbol"], c.values["accountSeq"] = "btc_krw", "1"
			_, ws := analyze(t, c.values, nil, nil)
			msg := warnMessage(ws, WarnMidPriceUnavailable)
			if msg == "" {
				t.Fatalf("want MID_PRICE_UNAVAILABLE: %s", codes(ws))
			}
			if !strings.Contains(msg, c.want) {
				t.Fatalf("want the applicable check named (%q): %q", c.want, msg)
			}
			if strings.Contains(msg, c.notWant) {
				t.Fatalf("a check this order never had must not be reported as lost (%q): %q", c.notWant, msg)
			}
			if !strings.Contains(msg, "not evidence") {
				t.Fatalf("every variant must keep the silence-is-not-a-pass clause: %q", msg)
			}
		})
	}

	// A protected limit order is the one shape both checks apply to, so both are
	// named — and that is the only shape where they are.
	_, ws := analyze(t, map[string]string{"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "10000000", "qty": "0.01", "timeInForce": "gtc", "pp": "true", "accountSeq": "1"}, nil, nil)
	msg := warnMessage(ws, WarnMidPriceUnavailable)
	if !strings.Contains(msg, limitCheck) || !strings.Contains(msg, ppCheck) || !strings.Contains(msg, "not evidence") {
		t.Fatalf("a protected limit lost both checks and must be told so: %q", msg)
	}
}

// warnMessage returns the rendered message of the first warning with code, or "".
func warnMessage(ws []PlaceWarning, code PlaceWarningCode) string {
	for _, w := range ws {
		if w.Code == code {
			return w.Message
		}
	}
	return ""
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
