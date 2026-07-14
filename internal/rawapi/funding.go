// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

import (
	"context"
	"encoding/json"

	"github.com/korbit-official/korbit-cli/internal/korbit"
)

// CoinDeposit is one crypto deposit record.
type CoinDeposit struct {
	ID               int64  `json:"id"`
	Currency         string `json:"currency"`
	Network          string `json:"network"`
	Address          string `json:"address"`
	SecondaryAddress string `json:"secondaryAddress,omitempty"`
	Status           string `json:"status"`
	Quantity         string `json:"quantity"`
	TransactionHash  string `json:"transactionHash"`
	CreatedAt        int64  `json:"createdAt"`
}

// CoinWithdrawal is one crypto withdrawal record.
type CoinWithdrawal struct {
	ID               int64  `json:"id"`
	Currency         string `json:"currency"`
	Network          string `json:"network"`
	Address          string `json:"address"`
	SecondaryAddress string `json:"secondaryAddress,omitempty"`
	Quantity         string `json:"quantity"`
	Fee              string `json:"fee"`
	Status           string `json:"status"`
	TransactionHash  string `json:"transactionHash,omitempty"`
	CreatedAt        int64  `json:"createdAt"`
}

// DepositAddress is a crypto deposit address.
type DepositAddress struct {
	Currency         string `json:"currency"`
	Network          string `json:"network"`
	Address          string `json:"address"`
	SecondaryAddress string `json:"secondaryAddress,omitempty"`
}

// Funding accountSeq note: every deposit/withdrawal endpoint below accepts an
// optional accountSeq, but the API operates on the MAIN account only and accepts
// only 1. accountSeq is NOT special-cased for funding: we send whatever the
// caller resolved via the normal precedence (explicit → the key's configured
// default → main), so a key whose default is a sub-account must pass accountSeq 1
// for funding or the server rejects it with ACCOUNT_SEQ_NOT_ALLOWED. AccountSeq
// is appended last so the wire order matches the account/order requests.

// ---- deposit addresses: GET /v2/coin/depositAddresses (signed) ----

// DepositAddressesRequest lists deposit addresses. AccountSeq is main-only
// (the API accepts only 1); see the funding accountSeq note below.
type DepositAddressesRequest struct {
	AccountSeq *int
}

func (c *Client) DepositAddresses(ctx context.Context, req DepositAddressesRequest, pol korbit.Policy) ([]DepositAddress, json.RawMessage, korbit.Meta, error) {
	var p params
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]DepositAddress](c, ctx, "GET", "/v2/coin/depositAddresses", true, p, pol)
}

// ---- deposit address: GET /v2/coin/depositAddress (signed) ----

// DepositAddressRequest shows the deposit address for one asset.
type DepositAddressRequest struct {
	Currency   Currency
	Network    *string
	AccountSeq *int
}

func (c *Client) DepositAddress(ctx context.Context, req DepositAddressRequest, pol korbit.Policy) (DepositAddress, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("currency", string(req.Currency))
	p.strPtr("network", req.Network)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[DepositAddress](c, ctx, "GET", "/v2/coin/depositAddress", true, p, pol)
}

// ---- deposit generate: POST /v2/coin/depositAddress (signed) ----

// DepositGenerateRequest generates (or returns the existing) deposit address.
type DepositGenerateRequest struct {
	Currency   Currency
	Network    *string
	AccountSeq *int
}

func (c *Client) DepositGenerate(ctx context.Context, req DepositGenerateRequest, pol korbit.Policy) (DepositAddress, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("currency", string(req.Currency))
	p.strPtr("network", req.Network)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[DepositAddress](c, ctx, "POST", "/v2/coin/depositAddress", true, p, pol)
}

// ---- deposit history: GET /v2/coin/recentDeposits (signed) ----

// DepositHistoryRequest lists recent crypto deposits for an asset.
type DepositHistoryRequest struct {
	Currency   Currency
	Limit      *int
	AccountSeq *int
}

func (c *Client) DepositHistory(ctx context.Context, req DepositHistoryRequest, pol korbit.Policy) ([]CoinDeposit, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("currency", string(req.Currency))
	p.intPtr("limit", req.Limit)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]CoinDeposit](c, ctx, "GET", "/v2/coin/recentDeposits", true, p, pol)
}

// ---- deposit status: GET /v2/coin/deposit (signed) ----

// DepositStatusRequest fetches one crypto deposit by id.
type DepositStatusRequest struct {
	Currency      Currency
	CoinDepositID int
	AccountSeq    *int
}

func (c *Client) DepositStatus(ctx context.Context, req DepositStatusRequest, pol korbit.Policy) (CoinDeposit, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("currency", string(req.Currency))
	p.intVal("coinDepositId", req.CoinDepositID)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[CoinDeposit](c, ctx, "GET", "/v2/coin/deposit", true, p, pol)
}

// ---- withdraw addresses: GET /v2/coin/withdrawableAddresses (signed) ----

// WithdrawAddressesRequest lists registered withdrawal addresses. AccountSeq is
// main-only (see the funding accountSeq note above).
type WithdrawAddressesRequest struct {
	AccountSeq *int
}

// WithdrawableAddress is one address registered for API withdrawals.
type WithdrawableAddress struct {
	Network          string `json:"network"`
	Currency         string `json:"currency,omitempty"`
	Address          string `json:"address"`
	SecondaryAddress string `json:"secondaryAddress,omitempty"`
}

func (c *Client) WithdrawAddresses(ctx context.Context, req WithdrawAddressesRequest, pol korbit.Policy) ([]WithdrawableAddress, json.RawMessage, korbit.Meta, error) {
	var p params
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]WithdrawableAddress](c, ctx, "GET", "/v2/coin/withdrawableAddresses", true, p, pol)
}

// ---- withdraw amount: GET /v2/coin/withdrawableAmount (signed) ----

// WithdrawAmountRequest fetches withdrawable amounts. Currency is optional
// (omit for all).
type WithdrawAmountRequest struct {
	Currency   *string
	AccountSeq *int
}

// WithdrawableAmount is one asset's withdrawable amount.
type WithdrawableAmount struct {
	Currency              string `json:"currency"`
	WithdrawableAmount    string `json:"withdrawableAmount"`
	WithdrawalInUseAmount string `json:"withdrawalInUseAmount"`
}

func (c *Client) WithdrawAmount(ctx context.Context, req WithdrawAmountRequest, pol korbit.Policy) ([]WithdrawableAmount, json.RawMessage, korbit.Meta, error) {
	var p params
	p.strPtr("currency", req.Currency)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]WithdrawableAmount](c, ctx, "GET", "/v2/coin/withdrawableAmount", true, p, pol)
}

// ---- withdraw request: POST /v2/coin/withdrawal (signed) ----

// WithdrawRequestRequest requests a crypto withdrawal to a registered address.
// Amount and Address are required; Network and SecondaryAddress are optional and
// appended in declaration order when set.
type WithdrawRequestRequest struct {
	Currency         Currency
	Amount           string
	Address          string
	Network          *string
	SecondaryAddress *string
	AccountSeq       *int
}

// WithdrawRequestResponse is the withdrawal-request acknowledgement.
type WithdrawRequestResponse struct {
	Status           string `json:"status"`
	CoinWithdrawalID int64  `json:"coinWithdrawalId"`
}

func (c *Client) WithdrawRequest(ctx context.Context, req WithdrawRequestRequest, pol korbit.Policy) (WithdrawRequestResponse, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("currency", string(req.Currency))
	p.str("amount", req.Amount)
	p.str("address", req.Address)
	p.strPtr("network", req.Network)
	p.strPtr("secondaryAddress", req.SecondaryAddress)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[WithdrawRequestResponse](c, ctx, "POST", "/v2/coin/withdrawal", true, p, pol)
}

// ---- withdraw cancel: DELETE /v2/coin/withdrawal (signed) ----

// WithdrawCancelRequest cancels a crypto withdrawal by id.
type WithdrawCancelRequest struct {
	CoinWithdrawalID int
	AccountSeq       *int
}

// WithdrawCancelResponse is the cancel acknowledgement.
type WithdrawCancelResponse struct {
	Success bool `json:"success,omitempty"`
}

func (c *Client) WithdrawCancel(ctx context.Context, req WithdrawCancelRequest, pol korbit.Policy) (WithdrawCancelResponse, json.RawMessage, korbit.Meta, error) {
	var p params
	p.intVal("coinWithdrawalId", req.CoinWithdrawalID)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[WithdrawCancelResponse](c, ctx, "DELETE", "/v2/coin/withdrawal", true, p, pol)
}

// ---- withdraw history: GET /v2/coin/recentWithdrawals (signed) ----

// WithdrawHistoryRequest lists recent crypto withdrawals for an asset.
type WithdrawHistoryRequest struct {
	Currency   Currency
	Limit      *int
	AccountSeq *int
}

func (c *Client) WithdrawHistory(ctx context.Context, req WithdrawHistoryRequest, pol korbit.Policy) ([]CoinWithdrawal, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("currency", string(req.Currency))
	p.intPtr("limit", req.Limit)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]CoinWithdrawal](c, ctx, "GET", "/v2/coin/recentWithdrawals", true, p, pol)
}

// ---- withdraw status: GET /v2/coin/withdrawal (signed) ----

// WithdrawStatusRequest fetches one crypto withdrawal by id.
type WithdrawStatusRequest struct {
	Currency         Currency
	CoinWithdrawalID int
	AccountSeq       *int
}

func (c *Client) WithdrawStatus(ctx context.Context, req WithdrawStatusRequest, pol korbit.Policy) (CoinWithdrawal, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("currency", string(req.Currency))
	p.intVal("coinWithdrawalId", req.CoinWithdrawalID)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[CoinWithdrawal](c, ctx, "GET", "/v2/coin/withdrawal", true, p, pol)
}

// ---- krw deposit: POST /v2/krw/sendKrwDepositPush (signed) ----

// KRWDepositRequest sends a KRW-deposit push to the Korbit app.
type KRWDepositRequest struct {
	Amount     string
	AccountSeq *int
}

// KRWPushResponse is a KRW deposit/withdrawal push acknowledgement.
type KRWPushResponse struct {
	Success bool `json:"success,omitempty"`
}

func (c *Client) KRWDeposit(ctx context.Context, req KRWDepositRequest, pol korbit.Policy) (KRWPushResponse, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("amount", req.Amount)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[KRWPushResponse](c, ctx, "POST", "/v2/krw/sendKrwDepositPush", true, p, pol)
}

// ---- krw withdraw: POST /v2/krw/sendKrwWithdrawalPush (signed) ----

// KRWWithdrawRequest sends a KRW-withdrawal push to the Korbit app.
type KRWWithdrawRequest struct {
	Amount     string
	AccountSeq *int
}

func (c *Client) KRWWithdraw(ctx context.Context, req KRWWithdrawRequest, pol korbit.Policy) (KRWPushResponse, json.RawMessage, korbit.Meta, error) {
	var p params
	p.str("amount", req.Amount)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[KRWPushResponse](c, ctx, "POST", "/v2/krw/sendKrwWithdrawalPush", true, p, pol)
}

// ---- krw deposits: GET /v2/krw/recentDeposits (signed) ----

// KRWDepositsRequest lists recent KRW deposits.
type KRWDepositsRequest struct {
	Limit      *int
	AccountSeq *int
}

// KRWDeposit is one KRW deposit record.
type KRWDeposit struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	Quantity  string `json:"quantity"`
	CreatedAt int64  `json:"createdAt"`
}

func (c *Client) KRWDeposits(ctx context.Context, req KRWDepositsRequest, pol korbit.Policy) ([]KRWDeposit, json.RawMessage, korbit.Meta, error) {
	var p params
	p.intPtr("limit", req.Limit)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]KRWDeposit](c, ctx, "GET", "/v2/krw/recentDeposits", true, p, pol)
}

// ---- krw withdrawals: GET /v2/krw/recentWithdrawals (signed) ----

// KRWWithdrawalsRequest lists recent KRW withdrawals.
type KRWWithdrawalsRequest struct {
	Limit      *int
	AccountSeq *int
}

// KRWWithdrawal is one KRW withdrawal record.
type KRWWithdrawal struct {
	ID        int64  `json:"id"`
	Quantity  string `json:"quantity"`
	Fee       string `json:"fee"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
}

func (c *Client) KRWWithdrawals(ctx context.Context, req KRWWithdrawalsRequest, pol korbit.Policy) ([]KRWWithdrawal, json.RawMessage, korbit.Meta, error) {
	var p params
	p.intPtr("limit", req.Limit)
	p.intPtr("accountSeq", req.AccountSeq)
	return call[[]KRWWithdrawal](c, ctx, "GET", "/v2/krw/recentWithdrawals", true, p, pol)
}
