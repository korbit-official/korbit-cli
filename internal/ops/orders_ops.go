// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

func init() {
	// order place: the reconcile protocol (special).
	register(placeOp{meta: OpMeta{
		ID: []string{"order", "place"}, Section: cmdmeta.SectionOrders,
		Method: "POST", Path: "/v2/orders",
		Summary: "place an order",
		Params: []cmdmeta.Param{
			symbolFlag,
			{Flag: "side", API: "side", Kind: cmdmeta.KindEnum, EnumValues: []string{"buy", "sell"}, Required: true, Desc: "order side"},
			{
				Flag: "type", API: "orderType", Kind: cmdmeta.KindEnum,
				EnumValues: []string{"limit", "market", "best"}, Required: true,
				Desc: "order type (best = best-bid/offer order)",
			},
			{Flag: "price", API: "price", Kind: cmdmeta.KindDecimal, Desc: "limit price in the quote currency (limit orders only)"},
			{Flag: "qty", API: "qty", Kind: cmdmeta.KindDecimal, Desc: "base-asset quantity (limit orders, and market/best SELL)"},
			{Flag: "amt", API: "amt", Kind: cmdmeta.KindDecimal, Desc: "quote-currency amount to spend, e.g. KRW (market/best BUY only)"},
			{
				Flag: "tif", API: "timeInForce", Kind: cmdmeta.KindEnum,
				EnumValues: []string{"gtc", "ioc", "fok", "po"},
				Desc:       "time in force (server default: gtc for limit, ioc for market; REQUIRED for best)",
			},
			{Flag: "best-nth", API: "bestNth", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(5), Desc: "BBO price level 1-5 (best orders only; required)"},
			{
				Flag: "client-order-id", API: "clientOrderId", Kind: cmdmeta.KindString,
				Pattern: clientOrderIDPattern, PatternDesc: clientOrderIDPatternDesc,
				Desc: "idempotency id — when omitted, a UUIDv7 is minted and echoed in the output; persist it and reuse it to retry THIS order",
			},
			{Flag: "pp", API: "pp", Kind: cmdmeta.KindFlag, Desc: "enable price protection for taker fills"},
			{Flag: "pp-percent", API: "ppPercent", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(100), Desc: "price-protection threshold percent (server default 5; requires --pp)"},
			accountSeq,
		},
		Notes: []string{
			"Sizing: limit -> --price + --qty; market/best BUY -> --amt only (KRW to spend); market/best SELL -> --qty only.",
			"The output is the FULL order (status, fills), not just the accept ack: order place reconciles by clientOrderId and returns the fetched order. The clientOrderId is always echoed — persist it and reuse it via --client-order-id to retry THIS order; never mint a new one for a retry.",
			"On a network/5xx failure the placement is reconciled by clientOrderId, not blindly resent: it resolves to the existing order if it landed, else reports UNKNOWN (verify with `{prog} order get --client-order-id ...`) — a placed order is never reported failed.",
			"Pass --no-reconcile to send once and return the raw accept acknowledgement with no follow-up fetch (an ambiguous failure is reported UNKNOWN; verify before retrying).",
			"Notional bounds: 5,000 KRW <= price*qty (or amt) <= 1,000,000,000 KRW.",
			"Preview with --dry-run: it prints the unsigned request AND runs a customer-protection check against live public market data (orderbook + tick size, fetched from the same base URL the order uses — no credentials touched). The output's `warnings` flag risky orders: high market-order slippage / insufficient liquidity, a limit price far from market (fat-finger), a post-only that would be rejected for crossing, a fill-or-kill that would be killed, a tick-misaligned price, an out-of-bounds notional, or an order that sweeps past the visible book depth (its fill/slippage estimate is only a lower bound). Advisory only — never blocks the order.",
		},
		Response: orderResponseFields,
		Examples: []string{
			"{prog} order place --symbol btc_krw --side buy --type limit --price 100000000 --qty 0.001",
			"{prog} order place --symbol btc_krw --side buy --type market --amt 50000",
			"{prog} order place --symbol btc_krw --side sell --type market --qty 0.001",
			"{prog} order place --symbol btc_krw --side sell --type best --tif po --best-nth 1 --qty 0.001",
		},
		Auth:          signedAuth("writeOrders"),
		Safety:        cmdmeta.SafetyNonIdempotent,
		Destructive:   true,
		CrossValidate: crossValidatePlace,
	}})

	// order get / cancel: passthrough, with the id-XOR cross-field rule.
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"order", "get"}, Section: cmdmeta.SectionOrders,
			Method: "GET", Path: "/v2/orders",
			Summary: "fetch one order (with fill state) by order id or clientOrderId",
			Params:  []cmdmeta.Param{symbolFlag, orderIDFlag, clientOrderIDLookup, accountSeq},
			Notes: []string{
				"avgPrice is absent until something fills — never read it unguarded.",
				"Orders in expired/canceled status stop being queryable ~3 days after they close.",
			},
			Response: orderResponseFields,
			Examples: []string{
				"{prog} order get --symbol btc_krw --order-id 123456",
				"{prog} order get --symbol btc_krw --client-order-id 019eabcf-7f2e-7587-979c-d67bde2b8967",
			},
			Auth:          signedAuth("readOrders"),
			Safety:        cmdmeta.SafetyReadOnly,
			CrossValidate: crossValidateIDXor,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.OrderGet(ctx, rawapi.OrderGetRequest{
				Symbol:        rawapi.Symbol(reqStr(in.Values, "symbol")),
				OrderID:       optInt(in.Values, "orderId"),
				ClientOrderID: optStr(in.Values, "clientOrderId"),
				AccountSeq:    &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"order", "cancel"}, Section: cmdmeta.SectionOrders,
			Method: "DELETE", Path: "/v2/orders",
			Summary: "cancel an open order by order id or clientOrderId",
			Params:  []cmdmeta.Param{symbolFlag, orderIDFlag, clientOrderIDLookup, accountSeq},
			Notes: []string{
				"Cancellation is accepted asynchronously — confirm with `{prog} order get` before treating the funds as free.",
				"TRY_AGAIN means the order is mid-processing; it is retried automatically within --retry-timeout, so a surfaced TRY_AGAIN means it stayed busy the whole window — run the cancel again shortly.",
			},
			Examples:      []string{"{prog} order cancel --symbol btc_krw --order-id 123456"},
			Auth:          signedAuth("writeOrders"),
			Safety:        cmdmeta.SafetyIdempotent,
			Destructive:   true,
			CrossValidate: crossValidateIDXor,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.OrderCancel(ctx, rawapi.OrderCancelRequest{
				Symbol:        rawapi.Symbol(reqStr(in.Values, "symbol")),
				OrderID:       optInt(in.Values, "orderId"),
				ClientOrderID: optStr(in.Values, "clientOrderId"),
				AccountSeq:    &seq,
			}, pol)
			return b, meta, err
		},
	})

	// order open: plain passthrough.
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"order", "open"}, Section: cmdmeta.SectionOrders,
			Method: "GET", Path: "/v2/openOrders",
			Summary: "list open orders (status open or partiallyFilled) for a symbol",
			Params: []cmdmeta.Param{
				symbolFlag,
				{Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(1000), Desc: "max rows (server default 500)"},
				accountSeq,
			},
			Response: orderResponseFields,
			Examples: []string{"{prog} order open --symbol btc_krw"},
			Auth:     signedAuth("readOrders"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.OrderOpen(ctx, rawapi.OrderOpenRequest{
				Symbol:     rawapi.Symbol(reqStr(in.Values, "symbol")),
				Limit:      optInt(in.Values, "limit"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})

	// order history / fills: the cursorless window walk (special).
	register(historyOp{
		meta: OpMeta{
			ID: []string{"order", "history"}, Section: cmdmeta.SectionOrders,
			Method: "GET", Path: "/v2/allOrders",
			Summary: "list recent orders for a symbol (any status, last 36 hours)",
			Params: []cmdmeta.Param{
				symbolFlag,
				{Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(HistoryMaxRows), Desc: "max rows to return — auto-paged and deduped across the window (newest first), up to 20000"},
				startMs, endMs, accountSeq,
			},
			Notes: []string{
				"This listing may lag a few seconds — to decide whether a just-placed order landed, use `{prog} order get` or `{prog} order open` instead.",
			},
			Response:      orderResponseFields,
			Examples:      []string{"{prog} order history --symbol btc_krw --limit 100"},
			Auth:          signedAuth("readOrders"),
			Safety:        cmdmeta.SafetyReadOnly,
			CrossValidate: crossValidateTimeWindow,
		},
		tsField: "createdAt", idField: "orderId",
		page: func(ctx context.Context, raw *rawapi.Client, args historyArgs, p pageParams, pol korbit.Policy) (json.RawMessage, error) {
			_, b, _, err := raw.OrderHistory(ctx, rawapi.OrderHistoryRequest{
				Symbol:     rawapi.Symbol(args.symbol),
				Limit:      &p.limit,
				StartTime:  &p.startTime,
				EndTime:    p.endTime,
				AccountSeq: &args.accountSeq,
			}, pol)
			return b, err
		},
	})
	register(historyOp{
		meta: OpMeta{
			ID: []string{"fills"}, Section: cmdmeta.SectionOrders,
			Method: "GET", Path: "/v2/myTrades",
			Summary: "your executed trades for a symbol (last 36 hours)",
			Params: []cmdmeta.Param{
				symbolFlag,
				{Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: metaPtr(1), Max: metaPtr(HistoryMaxRows), Desc: "max rows to return — auto-paged and deduped across the window (newest first), up to 20000"},
				startMs, endMs, accountSeq,
			},
			Response: []cmdmeta.ResponseField{
				{Name: "symbol", Type: "string", Desc: "trading pair"},
				{Name: "tradeId", Type: "number", Desc: "per-symbol trade id"},
				{Name: "orderId", Type: "number", Desc: "order id this fill belongs to"},
				{Name: "side", Type: "string", Desc: "buy | sell"},
				{Name: "price", Type: "string", Desc: "fill price"},
				{Name: "qty", Type: "string", Desc: "fill quantity (base)"},
				{Name: "amt", Type: "string", Desc: "fill amount (quote)"},
				{Name: "tradedAt", Type: "number", Desc: "fill timestamp (ms)"},
				{Name: "isTaker", Type: "boolean", Desc: "true = taker fill, false = maker"},
				{Name: "feeCurrency", Type: "string", Desc: "asset the fee was paid in"},
				{Name: "feeQty", Type: "string", Desc: "fee quantity"},
			},
			Examples:      []string{"{prog} fills --symbol btc_krw --limit 100"},
			Auth:          signedAuth("readOrders"),
			Safety:        cmdmeta.SafetyReadOnly,
			CrossValidate: crossValidateTimeWindow,
		},
		tsField: "tradedAt", idField: "tradeId",
		page: func(ctx context.Context, raw *rawapi.Client, args historyArgs, p pageParams, pol korbit.Policy) (json.RawMessage, error) {
			_, b, _, err := raw.Fills(ctx, rawapi.FillsRequest{
				Symbol:     rawapi.Symbol(args.symbol),
				Limit:      &p.limit,
				StartTime:  &p.startTime,
				EndTime:    p.endTime,
				AccountSeq: &args.accountSeq,
			}, pol)
			return b, err
		},
	})
}
