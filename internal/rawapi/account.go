// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

import (
	"context"
	"encoding/json"

	"github.com/korbit-official/korbit-cli/internal/korbit"
)

// ---- balance: GET /v2/balance (signed) ----

// BalanceRequest fetches account balances. Currencies is the optional
// comma-separated asset filter.
type BalanceRequest struct {
	Currencies *string
	AccountSeq *int
}

// Balance is one asset's balance.
type Balance struct {
	Currency        string `json:"currency"`
	Balance         string `json:"balance"`
	Available       string `json:"available"`
	TradeInUse      string `json:"tradeInUse"`
	WithdrawalInUse string `json:"withdrawalInUse"`
	AvgPrice        string `json:"avgPrice"`
}

func (c *Client) Balance(ctx context.Context, req BalanceRequest, pol korbit.Policy) ([]Balance, json.RawMessage, korbit.Meta, error) {
	var p params
	p.strPtr("currencies", req.Currencies)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]Balance](c, ctx, "GET", "/v2/balance", true, p, pol)
}

// ---- fees: GET /v2/tradingFeePolicy (signed) ----

// FeesRequest fetches the trading fee policy. Symbol is the optional
// comma-separated trading-pair filter.
type FeesRequest struct {
	Symbol     *string
	AccountSeq *int
}

// Fee is one symbol's trading fee policy.
type Fee struct {
	Symbol          string `json:"symbol"`
	BuyFeeCurrency  string `json:"buyFeeCurrency"`
	SellFeeCurrency string `json:"sellFeeCurrency"`
	MaxFeeRate      string `json:"maxFeeRate"`
	TakerFeeRate    string `json:"takerFeeRate"`
	MakerFeeRate    string `json:"makerFeeRate"`
}

func (c *Client) Fees(ctx context.Context, req FeesRequest, pol korbit.Policy) ([]Fee, json.RawMessage, korbit.Meta, error) {
	var p params
	p.strPtr("symbol", req.Symbol)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]Fee](c, ctx, "GET", "/v2/tradingFeePolicy", true, p, pol)
}

// ---- whoami: GET /v2/currentKeyInfo (signed) ----

// WhoamiRequest has no parameters.
type WhoamiRequest struct{}

// KeyInfo is the current API key's metadata.
type KeyInfo struct {
	APIKey             string   `json:"apiKey"`
	UserUUID           string   `json:"userUuid,omitempty"`
	Type               string   `json:"type"`
	PublicKey          string   `json:"publicKey,omitempty"`
	Permissions        []string `json:"permissions"`
	Whitelist          string   `json:"whitelist"`
	Expiration         int64    `json:"expiration"`
	Status             string   `json:"status"`
	Label              string   `json:"label"`
	AllowedAccountSeqs []int    `json:"allowedAccountSeqs"`
	CreatedAt          int64    `json:"createdAt"`
}

func (c *Client) Whoami(ctx context.Context, _ WhoamiRequest, pol korbit.Policy) (KeyInfo, json.RawMessage, korbit.Meta, error) {
	var p params
	return call[KeyInfo](c, ctx, "GET", "/v2/currentKeyInfo", true, p, pol)
}
