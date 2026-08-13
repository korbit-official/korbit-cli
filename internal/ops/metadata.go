// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/ids"
)

// metaPtr returns a pointer to n, for the optional *int bounds on a Param.
func metaPtr(n int) *int { return &n }

// Shared param/positional/response definitions, referenced across the inline
// OpMeta literals in the domain registration files. These mirror the public
// command surface field-for-field so the catalog stays terse and DRY.
var (
	clientOrderIDPattern     = ids.ClientOrderIDPattern
	clientOrderIDPatternDesc = "must match [0-9a-zA-Z.:_-]{1,36}"

	// accountSeq is optional at the user surface (Required=false): users may
	// omit it and the frontend fills it via accountseq.Ensure. However, by the
	// time ops.Run is called, values["accountSeq"] MUST be present — see
	// reqAccountSeq in passthrough.go for the ops-layer contract.
	accountSeq       = accountseq.Param()
	symbolPositional = cmdmeta.Positional{
		Name: "symbol", API: "symbol", Kind: cmdmeta.KindSymbol, Required: true,
		Desc: `trading pair, e.g. "btc_krw"`,
	}
	symbolFlag = cmdmeta.Param{
		Flag: "symbol", API: "symbol", Kind: cmdmeta.KindSymbol, Required: true,
		Desc: `trading pair, e.g. "btc_krw"`,
	}
	orderIDFlag = cmdmeta.Param{
		Flag: "order-id", API: "orderId", Kind: cmdmeta.KindInt, Min: metaPtr(1),
		Desc: "server-assigned order id",
	}
	clientOrderIDLookup = cmdmeta.Param{
		Flag: "client-order-id", API: "clientOrderId", Kind: cmdmeta.KindString,
		Pattern: clientOrderIDPattern, PatternDesc: clientOrderIDPatternDesc,
		Desc: "the clientOrderId you supplied (or the CLI minted) at placement",
	}
	startMs = cmdmeta.Param{Flag: "start", API: "startTime", Kind: cmdmeta.KindMs, Desc: "start timestamp (unix ms)"}
	endMs   = cmdmeta.Param{Flag: "end", API: "endTime", Kind: cmdmeta.KindMs, Desc: "end timestamp (unix ms)"}

	// Shared by the funding (deposit/withdrawal) commands.
	currencyPositional = cmdmeta.Positional{
		Name: "currency", API: "currency", Kind: cmdmeta.KindCurrency, Required: true,
		Desc: `asset symbol, e.g. "btc"`,
	}
	networkFlag = cmdmeta.Param{
		Flag: "network", API: "network", Kind: cmdmeta.KindString,
		Desc: `blockchain network symbol, e.g. "ETH" (default: the asset's default network; always set it — defaults can change)`,
	}
	fundingLimit = cmdmeta.Param{
		Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(100),
		Desc: "max rows to return (range 1-100)",
	}
)

// coinDepositFields are the per-deposit response hints shared by `deposit history`
// and `deposit status`.
var coinDepositFields = []cmdmeta.ResponseField{
	{Name: "id", Type: "number", Desc: "deposit id"},
	{Name: "currency", Type: "string", Desc: "asset symbol"},
	{Name: "network", Type: "string", Desc: "blockchain network symbol"},
	{Name: "address", Type: "string", Desc: "deposit address"},
	{Name: "secondaryAddress", Type: "string", Desc: "destination tag / memo (absent if none)"},
	{Name: "status", Type: "string", Desc: "pending | actionRequired | reviewing | done | refunded | failed"},
	{Name: "quantity", Type: "string", Desc: "deposited quantity"},
	{Name: "transactionHash", Type: "string", Desc: "on-chain transaction hash"},
	{Name: "createdAt", Type: "number", Desc: "deposit timestamp (ms)"},
}

// coinWithdrawalFields are the per-withdrawal response hints shared by
// `withdraw history` and `withdraw status`.
var coinWithdrawalFields = []cmdmeta.ResponseField{
	{Name: "id", Type: "number", Desc: "withdrawal id"},
	{Name: "currency", Type: "string", Desc: "asset symbol"},
	{Name: "network", Type: "string", Desc: "blockchain network symbol"},
	{Name: "address", Type: "string", Desc: "withdrawal address"},
	{Name: "secondaryAddress", Type: "string", Desc: "destination tag / memo (absent if none)"},
	{Name: "quantity", Type: "string", Desc: "withdrawn quantity (excludes fee)"},
	{Name: "fee", Type: "string", Desc: "withdrawal fee"},
	{Name: "status", Type: "string", Desc: "pending | actionRequired | reviewing | processing | done | canceled | failed"},
	{Name: "transactionHash", Type: "string", Desc: "on-chain transaction hash (absent until sent)"},
	{Name: "createdAt", Type: "number", Desc: "withdrawal request timestamp (ms)"},
}

// orderResponseFields are the success-response field hints shared by the order
// commands whose payload is the full order object (`order get`, `order open`,
// `order history`). `order place` returns only orderId, so it has its own list.
var orderResponseFields = []cmdmeta.ResponseField{
	{Name: "orderId", Type: "number", Desc: "server-assigned order id"},
	{Name: "clientOrderId", Type: "string", Desc: "the clientOrderId supplied at placement (absent if none)"},
	{Name: "symbol", Type: "string", Desc: "trading pair"},
	{Name: "orderType", Type: "string", Desc: "limit | market | best"},
	{Name: "side", Type: "string", Desc: "buy | sell"},
	{Name: "timeInForce", Type: "string", Desc: "gtc | ioc | fok | po"},
	{Name: "price", Type: "string", Desc: "order price (absent for market orders)"},
	{Name: "qty", Type: "string", Desc: "order quantity (base)"},
	{Name: "amt", Type: "string", Desc: "buy-side market/best spend amount (quote)"},
	{Name: "filledQty", Type: "string", Desc: "filled quantity (base)"},
	{Name: "filledAmt", Type: "string", Desc: "filled amount (quote)"},
	{Name: "avgPrice", Type: "string", Desc: "average execution price (absent until a fill)"},
	{Name: "createdAt", Type: "number", Desc: "order timestamp (ms)"},
	{Name: "lastFilledAt", Type: "number", Desc: "last execution timestamp (ms)"},
	{Name: "status", Type: "string", Desc: "pending | open | filled | canceled | partiallyFilled | partiallyFilledCanceled | expired"},
}

// signedAuth is the *cmdmeta.Auth helper for a permission-scoped endpoint.
func signedAuth(permission string) *cmdmeta.Auth { return &cmdmeta.Auth{Permission: permission} }
