// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"testing"

	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

func TestBoundsForSymbol(t *testing.T) {
	pairs := []rawapi.Pair{
		{Symbol: "btc_krw", Status: "launched", BaseCurrency: "btc", QuoteCurrency: "krw",
			MinOrderValue: "5000", MaxOrderValue: "1000000000"},
		// A pair that publishes neither bound: the fields are absent on the wire,
		// so they arrive as empty strings.
		{Symbol: "eth_" + testQuoteCcy, Status: "launched", BaseCurrency: "eth", QuoteCurrency: testQuoteCcy},
	}

	got := BoundsForSymbol(pairs, "BTC_KRW") // symbols match case-insensitively
	if got.QuoteCurrency != "krw" || got.Min != "5000" || got.Max != "1000000000" {
		t.Fatalf("btc_krw: got %+v", got)
	}

	// A pair with no published bound must yield no bound — the quote currency is
	// still reported, because the unit is known even when the bounds are not.
	got = BoundsForSymbol(pairs, "eth_"+testQuoteCcy)
	if got.QuoteCurrency != testQuoteCcy || got.Min != "" || got.Max != "" {
		t.Fatalf("unbounded pair: got %+v", got)
	}

	// A symbol the listing does not carry yields nothing at all: no unit, and no
	// bounds. Falling back to another pair's figures is the failure this prevents.
	if got := BoundsForSymbol(pairs, "doge_krw"); got != (OrderValueBounds{}) {
		t.Fatalf("unlisted symbol: got %+v, want the zero value", got)
	}
	if got := BoundsForSymbol(nil, "btc_krw"); got != (OrderValueBounds{}) {
		t.Fatalf("empty listing: got %+v, want the zero value", got)
	}
}

// ResolveBoundsForSymbol adds one thing to the raw listing read: the KRW
// market's documented figures, and ONLY against a server that does not publish
// the pair identity fields at all. The four paths below are the whole contract.
func TestResolveBoundsFallsBackOnlyForAPrePublicationServer(t *testing.T) {
	newShape := []rawapi.Pair{
		{Symbol: "btc_krw", Status: "launched", BaseCurrency: "btc", QuoteCurrency: "krw",
			MinOrderValue: "7000", MaxOrderValue: "900000000"},
		// Same server, a KRW pair it says has no bounds. "No bound" is the
		// server's answer, not a gap to paper over.
		{Symbol: "doge_krw", Status: "launched", BaseCurrency: "doge", QuoteCurrency: "krw"},
	}
	// A server that predates the fields: symbol + status only, exactly what
	// production and an older sandbox bundle serve.
	oldShape := []rawapi.Pair{
		{Symbol: "btc_krw", Status: "launched"},
		{Symbol: "btc_" + testQuoteCcy, Status: "launched"},
	}

	// 1. New shape, bounds published -> verbatim. The published figures win over
	//    the constants, so a market that moves its bounds is followed.
	if got := ResolveBoundsForSymbol(newShape, "btc_krw"); got.Min != "7000" || got.Max != "900000000" {
		t.Errorf("published bounds must be used verbatim: got %+v", got)
	}
	// 2. New shape, bounds omitted -> no bounds. The fallback must NOT override a
	//    server that is capable of publishing and chose not to.
	if got := ResolveBoundsForSymbol(newShape, "doge_krw"); got.Min != "" || got.Max != "" {
		t.Errorf("an omitted bound on a publishing server means none: got %+v", got)
	}
	// 3. Old shape, KRW symbol -> the documented KRW figures, so the below-min /
	//    above-max warnings survive an upgrade that outruns the server.
	got := ResolveBoundsForSymbol(oldShape, "btc_krw")
	if got.QuoteCurrency != "krw" || got.Min != krwMinOrder || got.Max != krwMaxOrder {
		t.Errorf("pre-publication server, krw pair: got %+v", got)
	}
	// 4. Old shape, non-KRW symbol -> the unit only. Inventing a bound for a
	//    market whose figures this code has never known is the original bug.
	got = ResolveBoundsForSymbol(oldShape, "btc_"+testQuoteCcy)
	if got.QuoteCurrency != testQuoteCcy || got.Min != "" || got.Max != "" {
		t.Errorf("pre-publication server, non-krw pair must stay unbounded: got %+v", got)
	}

	// A listing that never arrived (a failed fetch passes nil) resolves like the
	// old shape: the KRW market keeps its warnings, everything else is unbounded.
	if got := ResolveBoundsForSymbol(nil, "btc_krw"); got.Min != krwMinOrder {
		t.Errorf("absent listing, krw pair: got %+v", got)
	}
	if got := ResolveBoundsForSymbol(nil, "btc_"+testQuoteCcy); got.Min != "" {
		t.Errorf("absent listing, non-krw pair: got %+v", got)
	}
	// A symbol a publishing server does not list is not a pre-publication server:
	// the pair simply does not exist there, so no figure is supplied for it.
	if got := ResolveBoundsForSymbol(newShape, "xrp_krw"); got != (OrderValueBounds{}) {
		t.Errorf("unlisted symbol on a publishing server: got %+v", got)
	}
}

// The resolver normalizes what a server sends: symbols and currency codes are
// compared case-folded and trimmed. Nothing upstream guarantees the server's
// casing, and the fallback keys on an exact "krw" — so a server answering "KRW"
// must not be read as a different currency, and an entry whose currency is
// blank-but-present must not read as published.
func TestResolveBoundsNormalizesCaseAndWhitespace(t *testing.T) {
	// An uppercase symbol reaching the fallback still resolves to the KRW market.
	if got := ResolveBoundsForSymbol(nil, "BTC_KRW"); got.Min != krwMinOrder || got.QuoteCurrency != "krw" {
		t.Errorf("uppercase symbol through the fallback: got %+v", got)
	}
	// An uppercase / padded currency from the server is the same currency.
	pairs := []rawapi.Pair{{Symbol: "btc_krw", QuoteCurrency: " KRW ", MinOrderValue: " 7000 "}}
	got := ResolveBoundsForSymbol(pairs, "btc_krw")
	if got.QuoteCurrency != "krw" || got.Min != "7000" {
		t.Errorf("padded/uppercase server values: got %+v", got)
	}
	// A currency field of only whitespace is not a published currency, so the
	// listing reads as pre-publication and the KRW fallback applies.
	blank := []rawapi.Pair{{Symbol: "btc_krw", QuoteCurrency: "   "}}
	if got := ResolveBoundsForSymbol(blank, "btc_krw"); got.Min != krwMinOrder {
		t.Errorf("whitespace-only currency must not count as published: got %+v", got)
	}
}

// A bound the entry publishes is never discarded in favour of a constant, even
// when that entry omits the currency the spec says is always there. Discarding
// it would silently replace a real per-pair figure with the KRW market's.
func TestResolveBoundsKeepsAPublishedBoundWithoutACurrency(t *testing.T) {
	pairs := []rawapi.Pair{{Symbol: "btc_" + testQuoteCcy, Status: "launched", MinOrderValue: "10"}}
	got := ResolveBoundsForSymbol(pairs, "btc_"+testQuoteCcy)
	if got.Min != "10" {
		t.Errorf("published minimum discarded: got %+v", got)
	}
	if got.QuoteCurrency != testQuoteCcy {
		t.Errorf("unit should fall back to the symbol segment: got %+v", got)
	}
	// Same shape on a KRW pair: the entry's figure wins over the constant.
	krw := []rawapi.Pair{{Symbol: "btc_krw", Status: "launched", MinOrderValue: "10"}}
	if got := ResolveBoundsForSymbol(krw, "btc_krw"); got.Min != "10" {
		t.Errorf("the KRW constant overrode a published figure: got %+v", got)
	}
}

// An unparseable bound cannot support a rejection claim, so it is skipped exactly
// as an absent one is — never coerced to zero, which would flag every order as
// above the maximum.
func TestBoundsIgnoreUnparseableFigures(t *testing.T) {
	b := OrderValueBounds{QuoteCurrency: "krw", Min: "not-a-number", Max: ""}
	if _, ok := b.min(); ok {
		t.Error("an unparseable minimum must not be usable")
	}
	if _, ok := b.max(); ok {
		t.Error("an absent maximum must not be usable")
	}
}

// The end-to-end path: the analysis behind `order place --dry-run` reads the
// bounds from the pair listing. A listing that publishes no bound for the symbol
// must produce no bound warning — proof that no figure is carried in the code.
func TestPrePlaceRaisesNoBoundWarningWhenTheListingPublishesNone(t *testing.T) {
	unbounded := `[{"symbol":"btc_krw","status":"launched","baseCurrency":"btc","quoteCurrency":"krw"}]`
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick, pairs: unbounded}

	// A 1 KRW notional: below every plausible minimum, and still unwarned,
	// because this pair publishes none.
	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9999000", "qty": "0.0000001",
	})
	if hasCode(ws, string(WarnNotionalBelowMin)) {
		t.Fatalf("claimed a minimum the listing does not publish: %s", codes(ws))
	}

	// The same order against a listing that DOES publish the minimum warns.
	ws = run(t, fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick, pairs: samplePairs}, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9999000", "qty": "0.0000001",
	})
	if !hasCode(ws, string(WarnNotionalBelowMin)) {
		t.Fatalf("want NOTIONAL_BELOW_MIN from the published bound, got: %s", codes(ws))
	}
}

// End to end against a server that predates the bound fields — production today,
// and any sandbox bundle older than the one that publishes them. The KRW market's
// below-min warning must still fire: this CLI may be upgraded ahead of the server
// it talks to, and a warning that quietly stops appearing reads exactly like an
// order that is fine to send.
func TestPrePlaceKeepsTheKrwWarningAgainstAPrePublicationServer(t *testing.T) {
	const oldShape = `[{"symbol":"btc_krw","status":"launched"}]`
	doer := fakeMarketDoer{orderbook: sampleBook, tickSize: sampleTick, pairs: oldShape}

	ws := run(t, doer, map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9999000", "qty": "0.0000001", // ~1 KRW, under the 5,000 minimum
	})
	if !hasCode(ws, string(WarnNotionalBelowMin)) {
		t.Fatalf("want NOTIONAL_BELOW_MIN from the KRW fallback, got: %s", codes(ws))
	}

	// The fallback is the KRW market's figure alone: the same server tells us
	// nothing about any other market, so nothing is claimed about one.
	ws = run(t, fakeMarketDoer{
		orderbook: sampleBook,
		tickSize:  `[{"symbol":"btc_` + testQuoteCcy + `","tickSizePolicy":[{"priceGte":"0","tickSize":"1000"}],"orderbookLevels":[]}]`,
		pairs:     `[{"symbol":"btc_` + testQuoteCcy + `","status":"launched"}]`,
	}, map[string]string{
		"symbol": "btc_" + testQuoteCcy, "side": "buy", "orderType": "limit",
		"price": "9999000", "qty": "0.0000001",
	})
	if hasCode(ws, string(WarnNotionalBelowMin)) {
		t.Fatalf("the KRW figure must not be applied to another market: %s", codes(ws))
	}
}
