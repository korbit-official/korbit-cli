// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

import (
	"context"
	"encoding/json"

	"github.com/korbit-official/korbit-cli/internal/korbit"
)

// Order is the full order document returned by the order query endpoints.
type Order struct {
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId,omitempty"`
	Symbol        string `json:"symbol"`
	OrderType     string `json:"orderType"`
	Side          string `json:"side"`
	TimeInForce   string `json:"timeInForce"`
	Price         string `json:"price,omitempty"`
	Qty           string `json:"qty"`
	Amt           string `json:"amt,omitempty"`
	FilledQty     string `json:"filledQty"`
	FilledAmt     string `json:"filledAmt"`
	AvgPrice      string `json:"avgPrice,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	LastFilledAt  int64  `json:"lastFilledAt"`
	Status        string `json:"status"`
}

// ---- order place: POST /v2/orders (signed) ----

// OrderPlaceRequest is a place-order request. Symbol, Side, and OrderType are
// required; the remaining fields are optional and appended in wire declaration
// order only when set.
type OrderPlaceRequest struct {
	Symbol        Symbol
	Side          Side
	OrderType     OrderType
	Price         *string
	Qty           *string
	Amt           *string
	TimeInForce   *TimeInForce
	BestNth       *int
	ClientOrderID *string
	PP            *bool
	PPPercent     *int
	AccountSeq    *int
}

// OrderPlaceResponse is the place-order acknowledgement.
type OrderPlaceResponse struct {
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId,omitempty"`
}

func (c *Client) OrderPlace(ctx context.Context, req OrderPlaceRequest, pol korbit.Policy) (OrderPlaceResponse, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.str("side", string(req.Side))
	p.str("orderType", string(req.OrderType))
	p.strPtr("price", req.Price)
	p.strPtr("qty", req.Qty)
	p.strPtr("amt", req.Amt)
	if req.TimeInForce != nil {
		v := string(*req.TimeInForce)
		p.strPtr("timeInForce", &v)
	}
	p.intPtr("bestNth", req.BestNth)
	p.strPtr("clientOrderId", req.ClientOrderID)
	p.boolFlag("pp", req.PP)
	p.intPtr("ppPercent", req.PPPercent)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[OrderPlaceResponse](c, ctx, "POST", "/v2/orders", true, p, pol)
}

// ---- order get: GET /v2/orders (signed) ----

// OrderGetRequest fetches one order by order id or clientOrderId.
type OrderGetRequest struct {
	Symbol        Symbol
	OrderID       *int
	ClientOrderID *string
	AccountSeq    *int
}

func (c *Client) OrderGet(ctx context.Context, req OrderGetRequest, pol korbit.Policy) (Order, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.intPtr("orderId", req.OrderID)
	p.strPtr("clientOrderId", req.ClientOrderID)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[Order](c, ctx, "GET", "/v2/orders", true, p, pol)
}

// ---- order cancel: DELETE /v2/orders (signed) ----

// OrderCancelRequest cancels one open order by order id or clientOrderId.
type OrderCancelRequest struct {
	Symbol        Symbol
	OrderID       *int
	ClientOrderID *string
	AccountSeq    *int
}

// OrderCancelResponse is the cancel acknowledgement (a bare ack normalizes to
// success only).
type OrderCancelResponse struct {
	Success bool `json:"success,omitempty"`
}

func (c *Client) OrderCancel(ctx context.Context, req OrderCancelRequest, pol korbit.Policy) (OrderCancelResponse, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.intPtr("orderId", req.OrderID)
	p.strPtr("clientOrderId", req.ClientOrderID)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[OrderCancelResponse](c, ctx, "DELETE", "/v2/orders", true, p, pol)
}

// ---- order open: GET /v2/openOrders (signed) ----

// OrderOpenRequest lists open orders for a symbol.
type OrderOpenRequest struct {
	Symbol     Symbol
	Limit      *int
	AccountSeq *int
}

func (c *Client) OrderOpen(ctx context.Context, req OrderOpenRequest, pol korbit.Policy) ([]Order, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.intPtr("limit", req.Limit)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]Order](c, ctx, "GET", "/v2/openOrders", true, p, pol)
}

// ---- order history: GET /v2/allOrders (signed) ----

// OrderHistoryRequest lists recent orders for a symbol.
type OrderHistoryRequest struct {
	Symbol     Symbol
	Limit      *int
	StartTime  *int
	EndTime    *int
	AccountSeq *int
}

func (c *Client) OrderHistory(ctx context.Context, req OrderHistoryRequest, pol korbit.Policy) ([]Order, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.intPtr("limit", req.Limit)
	p.intPtr("startTime", req.StartTime)
	p.intPtr("endTime", req.EndTime)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]Order](c, ctx, "GET", "/v2/allOrders", true, p, pol)
}

// ---- fills: GET /v2/myTrades (signed) ----

// FillsRequest lists your executed trades for a symbol.
type FillsRequest struct {
	Symbol     Symbol
	Limit      *int
	StartTime  *int
	EndTime    *int
	AccountSeq *int
}

// Fill is one executed trade.
type Fill struct {
	Symbol      string `json:"symbol"`
	TradeID     int64  `json:"tradeId"`
	OrderID     int64  `json:"orderId"`
	Side        string `json:"side"`
	Price       string `json:"price"`
	Qty         string `json:"qty"`
	Amt         string `json:"amt"`
	TradedAt    int64  `json:"tradedAt"`
	IsTaker     bool   `json:"isTaker"`
	FeeCurrency string `json:"feeCurrency"`
	FeeQty      string `json:"feeQty"`
}

func (c *Client) Fills(ctx context.Context, req FillsRequest, pol korbit.Policy) ([]Fill, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("symbol", string(req.Symbol))
	p.intPtr("limit", req.Limit)
	p.intPtr("startTime", req.StartTime)
	p.intPtr("endTime", req.EndTime)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]Fill](c, ctx, "GET", "/v2/myTrades", true, p, pol)
}
