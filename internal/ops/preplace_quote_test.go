// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"strings"
	"testing"
)

// The quote currency here appears nowhere else in this module on purpose. A test
// written against a currency that any hardcoded list would plausibly contain
// (krw, usdt) passes whether or not such a list exists, and so proves nothing;
// this one fails the moment a currency list gates the quote-generic paths.
const testQuoteCcy = "xaut"

// The pre-place analysis reports the notional for ANY quote currency. It is
// load-bearing beyond the figure itself: the order panel's fee estimate is gated
// on a non-empty notional, so a blank notional blanks the fee too.
func TestAnalyzePlaceReportsNotionalForAnyQuoteCurrency(t *testing.T) {
	bids := []BookLevel{{Price: "93900.00", Qty: "2"}, {Price: "93800.00", Qty: "5"}}
	asks := []BookLevel{{Price: "94100.00", Qty: "2"}, {Price: "94200.00", Qty: "5"}}
	bands := []TickBand{{PriceGte: "0", TickSize: "0.01"}}
	symbol := "btc_" + testQuoteCcy

	sim, ws, err := AnalyzePlace(map[string]string{
		"symbol": symbol, "side": "buy", "orderType": "limit",
		"price": "93800.00", "qty": "0.001",
	}, bids, asks, bands, OrderValueBounds{})
	if err != nil {
		t.Fatalf("AnalyzePlace: %v", err)
	}
	if sim.QuoteCurrency != testQuoteCcy {
		t.Fatalf("quote currency: got %q, want %q", sim.QuoteCurrency, testQuoteCcy)
	}
	if sim.Notional != "93.8" { // 93800.00 × 0.001
		t.Fatalf("notional: got %q, want 93.8", sim.Notional)
	}
	// No bounds were supplied for this pair, so the analysis must claim none: a
	// notional this small is below some markets' minimum and fine on others, and
	// guessing which costs the caller a fill the exchange would have accepted.
	for _, w := range ws {
		if w.Code == WarnNotionalBelowMin || w.Code == WarnNotionalAboveMax {
			t.Fatalf("claimed a notional bound on a %s-quoted pair: %s", testQuoteCcy, w.Message)
		}
	}
	for _, w := range ws {
		if strings.Contains(w.Message, "KRW") {
			t.Fatalf("a %s-quoted pair drew a KRW-worded warning: %s", testQuoteCcy, w.Message)
		}
	}
}

// A market BUY is sized in the quote currency: `amt` IS the notional, whatever
// the quote currency is.
func TestAnalyzePlaceMarketBuyNotionalIsAmtForAnyQuoteCurrency(t *testing.T) {
	bids := []BookLevel{{Price: "93900.00", Qty: "2"}}
	asks := []BookLevel{{Price: "94100.00", Qty: "2"}}

	sim, _, err := AnalyzePlace(map[string]string{
		"symbol": "btc_" + testQuoteCcy, "side": "buy", "orderType": "market",
		"timeInForce": "ioc", "amt": "150.75",
	}, bids, asks, nil, OrderValueBounds{})
	if err != nil {
		t.Fatalf("AnalyzePlace: %v", err)
	}
	if sim.Notional != "150.75" || sim.QuoteCurrency != testQuoteCcy {
		t.Fatalf("market buy: notional %q in %q", sim.Notional, sim.QuoteCurrency)
	}
}

// The bound warnings come from the pair's OWN published bounds, on any market —
// the figures are read from the listing, so a pair quoted in anything raises the
// same warning the KRW market does.
func TestAnalyzePlaceWarnsFromThePairsPublishedBounds(t *testing.T) {
	bids := []BookLevel{{Price: "93900.00", Qty: "2"}}
	asks := []BookLevel{{Price: "94100.00", Qty: "2"}}
	bands := []TickBand{{PriceGte: "0", TickSize: "0.01"}}
	symbol := "btc_" + testQuoteCcy
	bounds := OrderValueBounds{QuoteCurrency: testQuoteCcy, Min: "100", Max: "1000"}

	for _, c := range []struct {
		name, qty string
		want      PlaceWarningCode
	}{
		{"below the published minimum", "0.0001", WarnNotionalBelowMin}, // 9.39
		{"above the published maximum", "0.5", WarnNotionalAboveMax},    // 46950
	} {
		_, ws, err := AnalyzePlace(map[string]string{
			"symbol": symbol, "side": "buy", "orderType": "limit",
			"price": "93900.00", "qty": c.qty,
		}, bids, asks, bands, bounds)
		if err != nil {
			t.Fatalf("%s: AnalyzePlace: %v", c.name, err)
		}
		var found bool
		for _, w := range ws {
			if w.Code != c.want {
				continue
			}
			found = true
			// The figure and its unit both come from the pair entry, so a warning
			// can never quote one market's bound in another's currency.
			if !strings.Contains(w.Message, strings.ToUpper(testQuoteCcy)) {
				t.Errorf("%s: warning names no %s unit: %s", c.name, testQuoteCcy, w.Message)
			}
			if strings.Contains(w.Message, "KRW") {
				t.Errorf("%s: warning is worded in KRW: %s", c.name, w.Message)
			}
		}
		if !found {
			t.Errorf("%s: want %s, got %s", c.name, c.want, codes(ws))
		}
	}
}

// A bound the pair does not publish is skipped, not read as zero: an order value
// no published minimum applies to must draw no minimum warning.
func TestAnalyzePlaceSkipsBoundsThePairDoesNotPublish(t *testing.T) {
	bids := []BookLevel{{Price: "9999000", Qty: "2"}}
	asks := []BookLevel{{Price: "10001000", Qty: "2"}}
	bands := []TickBand{{PriceGte: "0", TickSize: "1000"}}

	// A minimum, but no maximum: a huge order value must warn on neither end.
	_, ws, err := AnalyzePlace(map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9999000", "qty": "1000", // notional ~10 billion
	}, bids, asks, bands, OrderValueBounds{QuoteCurrency: "krw", Min: "5000"})
	if err != nil {
		t.Fatalf("AnalyzePlace: %v", err)
	}
	for _, w := range ws {
		if w.Code == WarnNotionalAboveMax {
			t.Fatalf("warned above a maximum the pair does not publish: %s", w.Message)
		}
	}
}

func TestSplitSymbol(t *testing.T) {
	cases := []struct {
		symbol, base, quote string
	}{
		{"btc_krw", "btc", "krw"},
		{"btc_" + testQuoteCcy, "btc", testQuoteCcy},
		{"btc", "btc", ""},   // no separator: no quote to report
		{"_krw", "_krw", ""}, // empty base is not a pair
		{"", "", ""},
	}
	for _, c := range cases {
		base, quote := SplitSymbol(c.symbol)
		if base != c.base || quote != c.quote {
			t.Errorf("SplitSymbol(%q) = %q,%q; want %q,%q", c.symbol, base, quote, c.base, c.quote)
		}
		if got := QuoteOf(c.symbol); got != c.quote {
			t.Errorf("QuoteOf(%q) = %q; want %q", c.symbol, got, c.quote)
		}
	}
}
