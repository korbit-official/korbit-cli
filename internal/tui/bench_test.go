// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/tui/components/balances"
	"github.com/korbit-official/korbit-cli/internal/tui/components/fills"
	"github.com/korbit-official/korbit-cli/internal/tui/components/footer"
	"github.com/korbit-official/korbit-cli/internal/tui/components/header"
	"github.com/korbit-official/korbit-cli/internal/tui/components/notices"
	"github.com/korbit-official/korbit-cli/internal/tui/components/orderbook"
	"github.com/korbit-official/korbit-cli/internal/tui/components/orders"
	"github.com/korbit-official/korbit-cli/internal/tui/components/sidebar"
	"github.com/korbit-official/korbit-cli/internal/tui/components/trades"
)

// benchModel builds a fully populated private model: ticker/book/trades for both
// symbols, an open order, balances, a fill, and a notice — so every body pane has
// real content to render. The inline chart is left off here; the chart's own
// frame cache is measured in the candlechart package (BenchmarkView).
func benchModel() model { return benchModelN([]string{"btc_krw", "eth_krw"}) }

func benchModelN(symbols []string) model {
	m := newModel(Config{
		Symbols:     symbols,
		Private:     true,
		Trader:      &fakeTrader{},
		KeyName:     "benchkey",
		BaseURL:     "http://127.0.0.1:9999",
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 48})
	m = mm.(model)
	m.chartInline = false // isolate the panes; the chart is benchmarked separately

	apply := func(ev stream.Event) { mm, _ := m.Update(streamEventMsg{ev: ev}); m = mm.(model) }
	for _, sym := range symbols { // a ticker for every row, so the sidebar Data is fully built
		apply(dataEvent("ticker", sym, stream.OriginSnapshot, 100, "", `{
			"type":"ticker","timestamp":100,"symbol":"`+sym+`","data":{
			"open":"1","high":"2","low":"3","close":"99027000","prevClose":"1","priceChange":"4348000",
			"priceChangePercent":"4.59","volume":"147.9","quoteVolume":"1","bestAskPrice":"99027000",
			"bestBidPrice":"99026000","lastTradedAt":90}}`))
	}
	active := symbols[0]
	apply(dataEvent("orderbook", active, stream.OriginSnapshot, 101, "", `{
		"data":{"timestamp":99,"asks":[{"price":"99131000","qty":"0.004"},{"price":"99140000","qty":"0.01"}],
		"bids":[{"price":"99120000","qty":"0.003"},{"price":"99110000","qty":"0.02"}]}}`))
	apply(dataEvent("trade", active, stream.OriginSnapshot, 102, "",
		`{"data":[{"timestamp":95,"price":"98909000","qty":"0.001","isBuyerTaker":true,"tradeId":5}]}`))
	apply(dataEvent("myOrder", "btc_krw", stream.OriginBackfill, 100, "/v2/openOrders", "["+openOrderRow+"]"))
	apply(dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"900000","tradeInUse":"100000","withdrawalInUse":"0","avgPrice":"0"}]`))
	apply(dataEvent("myTrade", "btc_krw", stream.OriginRealtime, 100, "", `{
		"symbol":"btc_krw","timestamp":100,"channelType":"myTrade","trade":{"trades":[
		{"tradeId":9,"orderId":777,"side":"buy","price":"99000000","qty":"0.1","fee":"10","feeCurrency":"krw","filledAt":95,"isTaker":true}]}}`))
	apply(stream.Notice{Code: stream.Connected, Level: stream.LevelInfo, Message: "connected",
		Details: map[string]any{"endpoint": "public"}, Time: 1})
	_ = m.render() // warm every pane's memo
	return m
}

// BenchmarkRender measures a full-body redraw three ways:
//   - idle: nothing changed, so every pane memo hits (the steady state).
//   - ticker: one ticker tick per frame — only the ticker-keyed panes re-render.
//   - cold: fresh pane components each frame, so every pane re-renders (the cost
//     a redraw would pay with no memoization).
func BenchmarkRender(b *testing.B) {
	b.Run("idle", func(b *testing.B) {
		m := benchModel()
		b.ReportAllocs()
		for b.Loop() {
			_ = m.render()
		}
	})

	// idle300 is the idle steady state with a realistic ~300-row market list. The
	// panes still hit their memo; this exposes the Data the parent rebuilds every
	// frame regardless of a hit — mainly the sidebar's per-row slice.
	b.Run("idle300", func(b *testing.B) {
		syms := make([]string, 300)
		for i := range syms {
			syms[i] = "sym" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + "_krw"
		}
		m := benchModelN(syms)
		b.ReportAllocs()
		for b.Loop() {
			_ = m.render()
		}
	})

	b.Run("ticker", func(b *testing.B) {
		m := benchModel()
		ev := dataEvent("ticker", "btc_krw", stream.OriginRealtime, 200, "", `{
			"type":"ticker","timestamp":200,"symbol":"btc_krw","data":{
			"open":"1","high":"2","low":"3","close":"99030000","prevClose":"1","priceChange":"4350000",
			"priceChangePercent":"4.60","volume":"148.0","quoteVolume":"1","bestAskPrice":"99030000",
			"bestBidPrice":"99029000","lastTradedAt":190}}`)
		b.ReportAllocs()
		for b.Loop() {
			mm, _ := m.Update(streamEventMsg{ev: ev})
			m = mm.(model)
			_ = m.render()
		}
	})

	b.Run("cold", func(b *testing.B) {
		m := benchModel()
		b.ReportAllocs()
		for b.Loop() {
			m.cHeader, m.cFooter, m.cSidebar = header.New(), footer.New(), sidebar.New()
			m.cOrderbook, m.cTrades = orderbook.New(), trades.New()
			m.cOrders, m.cFills, m.cBalances, m.cNotices = orders.New(), fills.New(), balances.New(), notices.New()
			_ = m.render()
		}
	})
}

// BenchmarkPane measures each body pane's cold render in isolation — a fresh
// component (empty memo) every frame, so the per-frame cost is the render, not a
// cache hit. It breaks the cold total of BenchmarkRender down per pane, so the
// expensive surfaces are visible. The sizes are the model's own column/row
// geometry at 160x48.
func BenchmarkPane(b *testing.B) {
	m := benchModel()
	sideW, bookW, tradesW, rightW := m.colWidths()
	bodyH := m.bodyHeight()
	oh, fh, bh := m.rightHeights(bodyH)

	cases := []struct {
		name string
		run  func(*model)
	}{
		{"header", func(m *model) { m.cHeader = header.New(); _ = m.renderHeader() }},
		{"footer", func(m *model) { m.cFooter = footer.New(); _ = m.renderFooter() }},
		{"sidebar", func(m *model) { m.cSidebar = sidebar.New(); _ = m.viewSidebar(sideW, bodyH) }},
		{"orderbook", func(m *model) { m.cOrderbook = orderbook.New(); _ = m.viewOrderbook(bookW, bodyH) }},
		{"trades", func(m *model) { m.cTrades = trades.New(); _ = m.viewTrades(tradesW, bodyH) }},
		{"orders", func(m *model) { m.cOrders = orders.New(); _ = m.viewOrders(rightW, oh) }},
		{"fills", func(m *model) { m.cFills = fills.New(); _ = m.viewFills(rightW, fh) }},
		{"balances", func(m *model) { m.cBalances = balances.New(); _ = m.viewBalances(rightW, bh) }},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tc.run(&m)
			}
		})
	}
}
