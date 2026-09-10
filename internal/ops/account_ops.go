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
			ID: []string{"balance"}, Section: cmdmeta.SectionAccount,
			Method: "GET", Path: "/v2/balance",
			Summary: "account balances per asset",
			Params: []cmdmeta.Param{
				{Flag: "currencies", API: "currencies", Kind: cmdmeta.KindCSV, Desc: `comma-separated assets, e.g. "btc,eth" (default: all held)`},
				accountSeq,
			},
			Response: []cmdmeta.ResponseField{
				{Name: "currency", Type: "string", Desc: "asset symbol"},
				{Name: "balance", Type: "string", Desc: "total = available + tradeInUse + withdrawalInUse"},
				{Name: "available", Type: "string", Desc: "free quantity (size orders against this)"},
				{Name: "tradeInUse", Type: "string", Desc: "quantity locked in open orders"},
				{Name: "withdrawalInUse", Type: "string", Desc: "quantity locked in withdrawals"},
				{Name: "avgPrice", Type: "string", Desc: "average purchase price"},
			},
			Notes:    []string{"Size new orders against `available`, not `balance` — the difference is already locked in open orders/withdrawals."},
			Examples: []string{"{prog} balance", "{prog} balance --currencies krw,btc"},
			Auth:     signedAuth("readBalances"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.Balance(ctx, rawapi.BalanceRequest{
				Currencies: optStr(in.Values, "currencies"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"fees"}, Section: cmdmeta.SectionAccount,
			Method: "GET", Path: "/v2/tradingFeePolicy",
			Summary: "trading fee policy for your account",
			Params: []cmdmeta.Param{
				{Flag: "symbol", API: "symbol", Kind: cmdmeta.KindSymbols, Desc: "comma-separated trading pairs (default: all)"},
				accountSeq,
			},
			Notes: []string{
				"Check buyFeeCurrency before sizing a buy: if it is the pair's quote currency (the symbol's second segment), reserve price*qty*(1+maxFeeRate) of it; if it is the base coin, the fee comes out of the coin you receive.",
			},
			Response: []cmdmeta.ResponseField{
				{Name: "symbol", Type: "string", Desc: "trading pair"},
				{Name: "buyFeeCurrency", Type: "string", Desc: "fee currency for buy orders"},
				{Name: "sellFeeCurrency", Type: "string", Desc: "fee currency for sell orders"},
				{Name: "maxFeeRate", Type: "string", Desc: "max fee rate (reserve this when the buy fee is charged in the quote currency)"},
				{Name: "takerFeeRate", Type: "string", Desc: "taker fee rate"},
				{Name: "makerFeeRate", Type: "string", Desc: "maker fee rate"},
			},
			Examples: []string{"{prog} fees --symbol btc_krw"},
			Auth:     signedAuth("readOrders"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.Fees(ctx, rawapi.FeesRequest{
				Symbol:     optStr(in.Values, "symbol"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"whoami"}, Section: cmdmeta.SectionAccount,
			Method: "GET", Path: "/v2/currentKeyInfo",
			Summary: "current API key info: type, permissions, IP allowlist, expiry",
			Response: []cmdmeta.ResponseField{
				{Name: "apiKey", Type: "string", Desc: "API key id"},
				{Name: "userUuid", Type: "string", Desc: "UUID of the user who owns this key (may be absent)"},
				{Name: "type", Type: "string", Desc: "key type: hmac-sha256 | ed25519"},
				{Name: "publicKey", Type: "string", Desc: "ED25519 public key (ed25519 keys only)"},
				{Name: "permissions", Type: "array", Desc: "granted permissions, e.g. readOrders, writeOrders"},
				{Name: "whitelist", Type: "string", Desc: "comma-separated allowlisted IPs"},
				{Name: "expiration", Type: "number", Desc: "scheduled expiry timestamp (ms)"},
				{Name: "status", Type: "string", Desc: "activated | deactivated"},
				{Name: "label", Type: "string", Desc: "custom key label"},
				{Name: "allowedAccountSeqs", Type: "array", Desc: "account sequence numbers this key may access"},
				{Name: "createdAt", Type: "number", Desc: "key creation timestamp (ms)"},
			},
			Notes:    []string{"Use this right after `{prog} key bind` to verify the key works and has the permissions you expect."},
			Examples: []string{"{prog} whoami", "{prog} whoami --key trading-bot"},
			Auth:     signedAuth(""),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
			_, b, meta, err := raw.Whoami(ctx, rawapi.WhoamiRequest{}, pol)
			return b, meta, err
		},
	})
}
