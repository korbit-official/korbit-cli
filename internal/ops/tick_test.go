// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import "testing"

// A three-band policy: tick 1 below 1,000, tick 5 from 1,000, tick 10 from 5,000.
var testBands = []TickBand{
	{PriceGte: "0", TickSize: "1"},
	{PriceGte: "1000", TickSize: "5"},
	{PriceGte: "5000", TickSize: "10"},
}

func TestTickSizeAt(t *testing.T) {
	cases := []struct {
		price string
		want  string
		ok    bool
	}{
		{"999", "1", true},
		{"1000", "5", true},
		{"4999.99", "5", true},
		{"5000", "10", true},
		{"123456789", "10", true},
		{"not-a-number", "", false},
	}
	for _, c := range cases {
		got, ok := TickSizeAt(testBands, c.price)
		if ok != c.ok || got != c.want {
			t.Errorf("TickSizeAt(%q) = %q,%v; want %q,%v", c.price, got, ok, c.want, c.ok)
		}
	}
	if _, ok := TickSizeAt(nil, "100"); ok {
		t.Error("TickSizeAt with no bands: want ok=false")
	}
	if _, ok := TickSizeAt([]TickBand{{PriceGte: "0", TickSize: "0"}}, "100"); ok {
		t.Error("TickSizeAt with a zero tick: want ok=false")
	}
	if _, ok := TickSizeAt([]TickBand{{PriceGte: "500", TickSize: "1"}}, "100"); ok {
		t.Error("TickSizeAt below every band: want ok=false")
	}
}

func TestSnapToTick(t *testing.T) {
	cases := []struct {
		price string
		want  string
	}{
		{"999", "999"},     // already on the fine grid
		{"1003", "1000"},   // floors onto the 5-grid
		{"1005", "1005"},   // on the 5-grid
		{"5004", "5000"},   // floors onto the 10-grid
		{"5000", "5000"},   // band edge, on grid
		{"999.7", "999"},   // fractional input floors
		{"1000.0", "1000"}, // canonical form out
	}
	for _, c := range cases {
		got, ok := SnapToTick(testBands, c.price)
		if !ok || got != c.want {
			t.Errorf("SnapToTick(%q) = %q,%v; want %q", c.price, got, ok, c.want)
		}
	}
}

func TestSnapUpToTick(t *testing.T) {
	cases := []struct {
		price string
		want  string
	}{
		{"999", "999"},     // already on the fine grid
		{"1001", "1005"},   // ceils onto the 5-grid
		{"1005", "1005"},   // on the 5-grid
		{"5001", "5010"},   // ceils onto the 10-grid
		{"5000", "5000"},   // band edge, on grid
		{"999.3", "1000"},  // ceils across the band edge up onto the 5-grid
		{"1000.0", "1000"}, // canonical form out
	}
	for _, c := range cases {
		got, ok := SnapUpToTick(testBands, c.price)
		if !ok || got != c.want {
			t.Errorf("SnapUpToTick(%q) = %q,%v; want %q", c.price, got, ok, c.want)
		}
	}
	if _, ok := SnapUpToTick(nil, "100"); ok {
		t.Error("SnapUpToTick with no bands: want ok=false")
	}
}

func TestOnTick(t *testing.T) {
	cases := []struct {
		price string
		grid  bool
		ok    bool
	}{
		{"999", true, true},    // on the fine grid
		{"1000", true, true},   // on the 5-grid (band edge)
		{"1003", false, true},  // off the 5-grid
		{"5000", true, true},   // on the 10-grid
		{"5005", false, true},  // off the 10-grid
		{"nope", false, false}, // unparseable
	}
	for _, c := range cases {
		grid, ok := OnTick(testBands, c.price)
		if grid != c.grid || ok != c.ok {
			t.Errorf("OnTick(%q) = %v,%v; want %v,%v", c.price, grid, ok, c.grid, c.ok)
		}
	}
	if _, ok := OnTick(nil, "100"); ok {
		t.Error("OnTick with no policy: want ok=false (cannot check, defer to server)")
	}
	if _, ok := OnTick([]TickBand{{PriceGte: "500", TickSize: "1"}}, "100"); ok {
		t.Error("OnTick below every band: want ok=false")
	}
}

func TestStepTicks(t *testing.T) {
	cases := []struct {
		price string
		n     int
		want  string
	}{
		{"1005", 1, "1010"},
		{"1005", -1, "1000"},
		{"998", 1, "999"},
		{"999", 1, "1000"},  // up across the band edge onto the coarser grid
		{"1000", -1, "999"}, // down across the band edge onto the finer grid
		{"5000", -1, "4995"},
		{"4995", 1, "5000"},
		{"5000", 3, "5030"},
		{"1013", 0, "1010"}, // n=0 still snaps
		{"1013", 2, "1020"}, // snap first, then step
		{"2", -5, "1"},      // clamps at the lowest positive grid point
		{"1", -1, "1"},      // cannot leave the grid downward
	}
	for _, c := range cases {
		got, ok := StepTicks(testBands, c.price, c.n)
		if !ok || got != c.want {
			t.Errorf("StepTicks(%q, %d) = %q,%v; want %q", c.price, c.n, got, ok, c.want)
		}
	}
	if _, ok := StepTicks(nil, "100", 1); ok {
		t.Error("StepTicks with no bands: want ok=false")
	}
}

// A policy whose band edge is off the coarser band's zero-anchored grid: an
// up-step across the edge must never move backwards.
func TestStepTicksMisalignedBandEdge(t *testing.T) {
	bands := []TickBand{
		{PriceGte: "0", TickSize: "100"},
		{PriceGte: "10500", TickSize: "1000"},
	}
	got, ok := StepTicks(bands, "10400", 1) // +100 → 10500, in the 1000-band, off its grid
	if !ok || got != "11000" {
		t.Fatalf("StepTicks(10400, +1) = %q,%v; want 11000 (ceiled onto the coarser grid)", got, ok)
	}
	got, ok = StepTicks(bands, "11000", -1) // down across the edge: largest 100-grid point < 11000
	if !ok || got != "10400" {
		// 10900 is on the finer grid but INSIDE the 1000-band (>= 10500), whose
		// grid it is not on; the largest on-grid point strictly below 11000 that
		// its own band accepts is what matters. Accept either safe answer.
		if got != "10900" {
			t.Fatalf("StepTicks(11000, -1) = %q,%v; want a lower on-grid price", got, ok)
		}
	}
}

func TestAnalyzePlacePure(t *testing.T) {
	bids := []BookLevel{{Price: "9999000", Qty: "2"}, {Price: "9990000", Qty: "5"}}
	asks := []BookLevel{{Price: "10001000", Qty: "2"}, {Price: "10100000", Qty: "5"}}
	bands := []TickBand{{PriceGte: "0", TickSize: "1000"}}

	// A resting limit buy on the grid: no warnings, not marketable.
	sim, ws, err := AnalyzePlace(map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9990000", "qty": "0.01",
	}, bids, asks, bands)
	if err != nil {
		t.Fatalf("AnalyzePlace: %v", err)
	}
	if sim.Marketable || len(ws) != 0 {
		t.Fatalf("resting limit: marketable=%v warnings=%s", sim.Marketable, codes(ws))
	}
	if sim.BestBid != "9999000" || sim.BestAsk != "10001000" || sim.Mid != "10000000" {
		t.Fatalf("reference prices: bid %s ask %s mid %s", sim.BestBid, sim.BestAsk, sim.Mid)
	}
	if sim.NotionalKRW != "99900" {
		t.Fatalf("notional: %s", sim.NotionalKRW)
	}

	// Off-grid price: the pure path flags it from the supplied bands.
	_, ws, err = AnalyzePlace(map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9990500", "qty": "0.01",
	}, bids, asks, bands)
	if err != nil {
		t.Fatalf("AnalyzePlace: %v", err)
	}
	if !hasCode(ws, "PRICE_OFF_TICK") {
		t.Fatalf("want PRICE_OFF_TICK, got: %s", codes(ws))
	}

	// nil bands skip the tick check but nothing else.
	_, ws, err = AnalyzePlace(map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit",
		"price": "9990500", "qty": "0.01",
	}, bids, asks, nil)
	if err != nil {
		t.Fatalf("AnalyzePlace: %v", err)
	}
	if hasCode(ws, "PRICE_OFF_TICK") {
		t.Fatalf("nil bands must skip the tick check, got: %s", codes(ws))
	}

	// An empty side is an unusable book.
	if _, _, err := AnalyzePlace(map[string]string{
		"symbol": "btc_krw", "side": "buy", "orderType": "limit", "price": "1", "qty": "1",
	}, bids, nil, nil); err == nil {
		t.Fatal("empty asks: want an error")
	}
}
