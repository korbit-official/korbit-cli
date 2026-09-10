// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/cmdmeta"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
)

func init() {
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"ticker"}, Section: cmdmeta.SectionMarket,
			Method: "GET", Path: "/v2/tickers",
			Summary: "latest price and 24h stats for one or more symbols (default: all)",
			Positionals: []cmdmeta.Positional{
				{Name: "symbols", API: "symbol", Kind: cmdmeta.KindSymbols, Variadic: true, Desc: "trading pairs; omit for all"},
			},
			Params: []cmdmeta.Param{
				{Flag: "symbol", API: "symbol", Kind: cmdmeta.KindSymbols, Desc: "comma-separated trading pairs (alternative to positionals)"},
			},
			Response: []cmdmeta.ResponseField{
				{Name: "symbol", Type: "string", Desc: "trading pair"},
				{Name: "open", Type: "string", Desc: "open price (24h)"},
				{Name: "high", Type: "string", Desc: "high price (24h)"},
				{Name: "low", Type: "string", Desc: "low price (24h)"},
				{Name: "close", Type: "string", Desc: "last price (24h)"},
				{Name: "prevClose", Type: "string", Desc: "previous close price (24h)"},
				{Name: "priceChange", Type: "string", Desc: "close - prevClose"},
				{Name: "priceChangePercent", Type: "string", Desc: "percent change vs prevClose"},
				{Name: "volume", Type: "string", Desc: "trading volume (base, 24h)"},
				{Name: "quoteVolume", Type: "string", Desc: "trading volume (quote, 24h)"},
				{Name: "bestBidPrice", Type: "string", Desc: "best bid price"},
				{Name: "bestAskPrice", Type: "string", Desc: "best ask price"},
				{Name: "lastTradedAt", Type: "number", Desc: "last traded timestamp (ms)"},
			},
			Examples: []string{"{prog} ticker btc_krw", "{prog} ticker btc_krw eth_krw", "{prog} ticker"},
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.Ticker(ctx, rawapi.TickerRequest{
				Symbol: optStr(in.Values, "symbol"),
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"orderbook"}, Section: cmdmeta.SectionMarket,
			Method: "GET", Path: "/v2/orderbook",
			Summary:     "orderbook snapshot for a symbol",
			Positionals: []cmdmeta.Positional{symbolPositional},
			Params: []cmdmeta.Param{
				{Flag: "level", API: "level", Kind: cmdmeta.KindString, Desc: "price grouping level (valid levels: `{prog} ticksize <symbol>`)"},
			},
			Response: []cmdmeta.ResponseField{
				{Name: "timestamp", Type: "number", Desc: "snapshot timestamp (ms)"},
				{Name: "bids", Type: "array", Desc: "bid levels, each {price, qty, amt?}"},
				{Name: "asks", Type: "array", Desc: "ask levels, each {price, qty, amt?}"},
			},
			Examples: []string{"{prog} orderbook btc_krw", "{prog} orderbook btc_krw --level 4"},
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.Orderbook(ctx, rawapi.OrderbookRequest{
				Symbol: rawapi.Symbol(reqStr(in.Values, "symbol")),
				Level:  optStr(in.Values, "level"),
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"trades"}, Section: cmdmeta.SectionMarket,
			Method: "GET", Path: "/v2/trades",
			Summary:     "recent public trades for a symbol",
			Positionals: []cmdmeta.Positional{symbolPositional},
			Params: []cmdmeta.Param{
				{Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(500), Desc: "number of trades to return"},
			},
			Response: []cmdmeta.ResponseField{
				{Name: "timestamp", Type: "number", Desc: "trade timestamp (ms)"},
				{Name: "price", Type: "string", Desc: "trade price"},
				{Name: "qty", Type: "string", Desc: "trade quantity (base)"},
				{Name: "isBuyerTaker", Type: "boolean", Desc: "whether the taker was the buyer"},
				{Name: "tradeId", Type: "number", Desc: "per-symbol trade id"},
			},
			Examples: []string{"{prog} trades btc_krw --limit 50"},
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.Trades(ctx, rawapi.TradesRequest{
				Symbol: rawapi.Symbol(reqStr(in.Values, "symbol")),
				Limit:  optInt(in.Values, "limit"),
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"pairs"}, Section: cmdmeta.SectionMarket,
			Method: "GET", Path: "/v2/currencyPairs",
			Summary: "list trading pairs with their status, currencies, and order value bounds",
			Response: []cmdmeta.ResponseField{
				{Name: "symbol", Type: "string", Desc: "trading pair"},
				{Name: "status", Type: "string", Desc: "launched | stopped"},
				{Name: "baseCurrency", Type: "string", Desc: "the asset being traded"},
				{Name: "quoteCurrency", Type: "string", Desc: "the currency the pair is priced in; the unit of the two bounds below"},
				{Name: "minOrderValue", Type: "string", Desc: "minimum order value, in quoteCurrency (absent: this pair publishes no minimum — skip that check, the server still decides)"},
				{Name: "maxOrderValue", Type: "string", Desc: "maximum order value, in quoteCurrency (absent: this pair publishes no maximum — skip that check, the server still decides)"},
			},
			Notes: []string{
				"Order value bounds are per pair: check qty*price (or amt for a market buy) against this pair's minOrderValue/maxOrderValue before placing, or the order is rejected with ORDER_VALUE_TOO_SMALL/ORDER_VALUE_TOO_LARGE. A bound this pair omits is a figure it does not publish, not a guarantee of none: skip that check and let the server decide, and never apply another pair's figure.",
			},
			Examples: []string{"{prog} pairs"},
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.Pairs(ctx, rawapi.PairsRequest{}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"ticksize"}, Section: cmdmeta.SectionMarket,
			Method: "GET", Path: "/v2/tickSizePolicy",
			Summary:     "tick-size policy and orderbook grouping levels for a symbol",
			Positionals: []cmdmeta.Positional{symbolPositional},
			Response: []cmdmeta.ResponseField{
				{Name: "symbol", Type: "string", Desc: "trading pair"},
				{Name: "tickSizePolicy", Type: "array", Desc: "price bands, each {priceGte, tickSize}"},
				{Name: "orderbookLevels", Type: "array", Desc: "valid orderbook grouping levels (strings)"},
			},
			Notes: []string{
				"To snap a price: pick the entry with the largest priceGte that is <= your order price, and floor the price to a multiple of its tickSize (decimal math, never floats).",
			},
			Examples: []string{"{prog} ticksize btc_krw"},
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.TickSize(ctx, rawapi.TickSizeRequest{
				Symbol: rawapi.Symbol(reqStr(in.Values, "symbol")),
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"currencies"}, Section: cmdmeta.SectionMarket,
			Method: "GET", Path: "/v2/currencies",
			Summary: "supported cryptocurrencies and per-network deposit/withdrawal info",
			Response: []cmdmeta.ResponseField{
				{Name: "name", Type: "string", Desc: "currency symbol, e.g. btc"},
				{Name: "fullName", Type: "string", Desc: "currency name, e.g. Bitcoin"},
				{Name: "withdrawalMaxAmountPerRequest", Type: "string", Desc: "max withdrawal amount per request"},
				{Name: "withdrawalMinAmount", Type: "string", Desc: "minimum withdrawal amount"},
				{Name: "defaultNetwork", Type: "string", Desc: "default blockchain network symbol"},
				{Name: "networkList", Type: "array", Desc: "supported networks (absent for fiat), each {name, withdrawalStatus, depositStatus, ...}"},
			},
			Examples: []string{"{prog} currencies"},
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.Currencies(ctx, rawapi.CurrenciesRequest{}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"time"}, Section: cmdmeta.SectionMarket,
			Method: "GET", Path: "/v2/time",
			Summary: "Digital X server time (unix ms), plus the local-clock skew (offsetMs/rttMs) — use to check clock drift",
			Response: []cmdmeta.ResponseField{
				{Name: "time", Type: "number", Desc: "server time (unix ms)"},
			},
			Examples: []string{"{prog} time"},
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.Time(ctx, rawapi.TimeRequest{}, pol)
			if err != nil {
				return b, meta, err
			}
			return withTimeSkew(b, meta), meta, nil
		},
	})

	// candles is special: it auto-pages past the server's 200-candle cap.
	register(candlesOp{meta: OpMeta{
		ID: []string{"candles"}, Section: cmdmeta.SectionMarket,
		Method: "GET", Path: "/v2/candles",
		Summary:     "historical candlesticks (klines) for a symbol",
		Positionals: []cmdmeta.Positional{symbolPositional},
		Params: []cmdmeta.Param{
			{
				Flag: "interval", API: "interval", Kind: cmdmeta.KindEnum,
				EnumValues: []string{"1", "5", "15", "30", "60", "240", "1D", "1W"}, Required: true,
				Desc: "candle interval — minutes (1, 5, 15, 30, 60, 240) or 1D / 1W",
			},
			{Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(CandlesMaxLimit), Default: "100", Desc: "number of candles to return — auto-paged and merged above the 200-per-request server cap, up to 5000"},
			startMs, endMs,
		},
		Response: []cmdmeta.ResponseField{
			{Name: "timestamp", Type: "number", Desc: "candle start timestamp (ms)"},
			{Name: "open", Type: "string", Desc: "open price"},
			{Name: "high", Type: "string", Desc: "high price"},
			{Name: "low", Type: "string", Desc: "low price"},
			{Name: "close", Type: "string", Desc: "close price"},
			{Name: "volume", Type: "string", Desc: "volume (base)"},
		},
		Notes:         []string{"--end must be after --start when both are given."},
		Examples:      []string{"{prog} candles btc_krw --interval 60 --limit 100"},
		Safety:        cmdmeta.SafetyReadOnly,
		CrossValidate: crossValidateTimeWindow,
	}})
}

// withTimeSkew adds the local-clock skew to a /v2/time result, additively: the
// server `time` field is preserved and localTime/offsetMs/rttMs/uncertaintyMs
// are appended. meta.StartedAtMs/FinishedAtMs are the local system-clock readings
// the wire layer stamps around the call, so their midpoint pairs with the server
// reading (charging round-trip latency to rtt, not to the clock). offsetMs is
// serverClock - localClock: a positive offset means the local clock runs BEHIND
// the server. The sample is trustworthy only when a single attempt bracketed the
// one request whose time is in the body, with a sane (non-negative) round trip;
// a retry (meta.Attempts > 1), an unbracketed meta (either reading zero), or a
// wall clock that stepped backward between the two readings (finish < start) all
// make the offset meaningless, so the server time is returned without skew
// fields. Both frontends (cli and mcp serve) dispatch through this op, so both
// surface the skew.
func withTimeSkew(body json.RawMessage, meta apiclient.Meta) json.RawMessage {
	if meta.Attempts > 1 || meta.StartedAtMs == 0 || meta.FinishedAtMs == 0 || meta.FinishedAtMs < meta.StartedAtMs {
		return body
	}
	serverMs := jsonInt64Field(body, "time")
	if serverMs == 0 {
		return body
	}
	localMid := (meta.StartedAtMs + meta.FinishedAtMs) / 2
	rtt := meta.FinishedAtMs - meta.StartedAtMs
	return withFields(body,
		numField{"localTime", localMid},
		numField{"offsetMs", serverMs - localMid},
		numField{"rttMs", rtt},
		numField{"uncertaintyMs", rtt / 2},
	)
}
