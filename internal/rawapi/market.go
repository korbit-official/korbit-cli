// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

import (
	"context"
	"encoding/json"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
)

// ---- ticker: GET /v2/tickers (public) ----

// TickerRequest selects the symbols to fetch. Symbol is the comma-separated
// wire parameter; an empty value omits it (all symbols).
type TickerRequest struct {
	Symbol *string
}

// Ticker is one symbol's latest price and 24h stats.
type Ticker struct {
	Symbol             string `json:"symbol"`
	Open               string `json:"open"`
	High               string `json:"high"`
	Low                string `json:"low"`
	Close              string `json:"close"`
	PrevClose          string `json:"prevClose"`
	PriceChange        string `json:"priceChange"`
	PriceChangePercent string `json:"priceChangePercent"`
	Volume             string `json:"volume"`
	QuoteVolume        string `json:"quoteVolume"`
	BestBidPrice       string `json:"bestBidPrice"`
	BestAskPrice       string `json:"bestAskPrice"`
	LastTradedAt       int64  `json:"lastTradedAt"`
}

func (c *Client) Ticker(ctx context.Context, req TickerRequest, pol apiclient.Policy) ([]Ticker, json.RawMessage, apiclient.Meta, error) {
	var p params
	p.strPtr("symbol", req.Symbol)
	return call[[]Ticker](c, ctx, "GET", "/v2/tickers", false, p, pol)
}

// ---- orderbook: GET /v2/orderbook (public) ----

// OrderbookRequest is one symbol's orderbook request. Level is the optional
// price-grouping level.
type OrderbookRequest struct {
	Symbol Symbol
	Level  *string
}

// OrderbookLevel is one price level in the orderbook. Amt is present only on
// some grouping levels.
type OrderbookLevel struct {
	Price string `json:"price"`
	Qty   string `json:"qty"`
	Amt   string `json:"amt,omitempty"`
}

// Orderbook is an orderbook snapshot.
type Orderbook struct {
	Timestamp int64            `json:"timestamp"`
	Bids      []OrderbookLevel `json:"bids"`
	Asks      []OrderbookLevel `json:"asks"`
}

func (c *Client) Orderbook(ctx context.Context, req OrderbookRequest, pol apiclient.Policy) (Orderbook, json.RawMessage, apiclient.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.strPtr("level", req.Level)
	return call[Orderbook](c, ctx, "GET", "/v2/orderbook", false, p, pol)
}

// ---- trades: GET /v2/trades (public) ----

// TradesRequest is one symbol's recent-trades request.
type TradesRequest struct {
	Symbol Symbol
	Limit  *int
}

// Trade is one public trade.
type Trade struct {
	Timestamp    int64  `json:"timestamp"`
	Price        string `json:"price"`
	Qty          string `json:"qty"`
	IsBuyerTaker bool   `json:"isBuyerTaker"`
	TradeID      int64  `json:"tradeId"`
}

func (c *Client) Trades(ctx context.Context, req TradesRequest, pol apiclient.Policy) ([]Trade, json.RawMessage, apiclient.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.intPtr("limit", req.Limit)
	return call[[]Trade](c, ctx, "GET", "/v2/trades", false, p, pol)
}

// ---- candles: GET /v2/candles (public) ----

// CandlesRequest is one symbol's candle request. Interval is required; the time
// bounds are optional.
type CandlesRequest struct {
	Symbol   Symbol
	Interval string
	// Limit is required: the endpoint rejects a missing or out-of-range limit
	// (1..200) with BAD_REQUEST "limit out of range", so it is a value, not a
	// pointer — every call must carry one.
	Limit     int
	StartTime *int
	EndTime   *int
}

// Candle is one candlestick.
type Candle struct {
	Timestamp int64  `json:"timestamp"`
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	Volume    string `json:"volume"`
}

func (c *Client) Candles(ctx context.Context, req CandlesRequest, pol apiclient.Policy) ([]Candle, json.RawMessage, apiclient.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.str("interval", req.Interval)
	p.intVal("limit", req.Limit)
	// The candles endpoint names its time bounds start/end (unlike the history
	// endpoints' startTime/endTime); sending the wrong names makes the server
	// ignore the bound and always return the newest page — which silently caps
	// auto-paging and backfill at one page.
	p.intPtr("start", req.StartTime)
	p.intPtr("end", req.EndTime)
	return call[[]Candle](c, ctx, "GET", "/v2/candles", false, p, pol)
}

// ---- pairs: GET /v2/currencyPairs (public) ----

// PairsRequest has no parameters.
type PairsRequest struct{}

// Pair is one trading pair: its status, its currencies, and its order value
// bounds. MinOrderValue/MaxOrderValue are decimal strings denominated in
// QuoteCurrency, and are empty for a pair that publishes no such bound — an
// empty bound is a figure the pair does not publish, not a guarantee of none: the
// check is skipped and the server decides. Never read as zero, and never filled
// in from another pair.
type Pair struct {
	Symbol        string `json:"symbol"`
	Status        string `json:"status"`
	BaseCurrency  string `json:"baseCurrency,omitempty"`
	QuoteCurrency string `json:"quoteCurrency,omitempty"`
	MinOrderValue string `json:"minOrderValue,omitempty"`
	MaxOrderValue string `json:"maxOrderValue,omitempty"`
}

func (c *Client) Pairs(ctx context.Context, _ PairsRequest, pol apiclient.Policy) ([]Pair, json.RawMessage, apiclient.Meta, error) {
	var p params
	return call[[]Pair](c, ctx, "GET", "/v2/currencyPairs", false, p, pol)
}

// ---- ticksize: GET /v2/tickSizePolicy (public) ----

// TickSizeRequest is one symbol's tick-size request.
type TickSizeRequest struct {
	Symbol Symbol
}

// TickSizeBand is one price band of the tick-size policy.
type TickSizeBand struct {
	PriceGte string `json:"priceGte"`
	TickSize string `json:"tickSize"`
}

// TickSizePolicy is a symbol's tick-size policy and orderbook grouping levels.
type TickSizePolicy struct {
	Symbol          string         `json:"symbol"`
	TickSizePolicy  []TickSizeBand `json:"tickSizePolicy"`
	OrderbookLevels []string       `json:"orderbookLevels"`
}

// TickSize returns the queried symbol's tick-size policy. The endpoint's data
// payload is an array (one element per symbol), so the typed value is a slice;
// a single-symbol query yields a one-element slice.
func (c *Client) TickSize(ctx context.Context, req TickSizeRequest, pol apiclient.Policy) ([]TickSizePolicy, json.RawMessage, apiclient.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	return call[[]TickSizePolicy](c, ctx, "GET", "/v2/tickSizePolicy", false, p, pol)
}

// ---- currencies: GET /v2/currencies (public) ----

// CurrenciesRequest has no parameters.
type CurrenciesRequest struct{}

// CurrencyNetwork is one supported network for a currency. The network detail
// fields beyond name vary by asset, so the verbatim object is preserved in Raw
// alongside the common fields for the typed surface.
type CurrencyNetwork struct {
	Name             string          `json:"name"`
	WithdrawalStatus string          `json:"withdrawalStatus,omitempty"`
	DepositStatus    string          `json:"depositStatus,omitempty"`
	Raw              json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the verbatim network object in Raw while decoding the
// common fields, since the per-network detail set varies by asset.
func (n *CurrencyNetwork) UnmarshalJSON(b []byte) error {
	type alias CurrencyNetwork
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*n = CurrencyNetwork(a)
	n.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// CurrencyInfo is one supported cryptocurrency. NetworkList is absent for fiat.
type CurrencyInfo struct {
	Name                          string            `json:"name"`
	FullName                      string            `json:"fullName"`
	WithdrawalMaxAmountPerRequest string            `json:"withdrawalMaxAmountPerRequest"`
	WithdrawalMinAmount           string            `json:"withdrawalMinAmount"`
	DefaultNetwork                string            `json:"defaultNetwork"`
	NetworkList                   []CurrencyNetwork `json:"networkList"`
}

func (c *Client) Currencies(ctx context.Context, _ CurrenciesRequest, pol apiclient.Policy) ([]CurrencyInfo, json.RawMessage, apiclient.Meta, error) {
	var p params
	return call[[]CurrencyInfo](c, ctx, "GET", "/v2/currencies", false, p, pol)
}

// ---- time: GET /v2/time (public) ----

// TimeRequest has no parameters.
type TimeRequest struct{}

// ServerTime is the Digital X server time.
type ServerTime struct {
	Time int64 `json:"time"`
}

func (c *Client) Time(ctx context.Context, _ TimeRequest, pol apiclient.Policy) (ServerTime, json.RawMessage, apiclient.Meta, error) {
	var p params
	return call[ServerTime](c, ctx, "GET", "/v2/time", false, p, pol)
}
