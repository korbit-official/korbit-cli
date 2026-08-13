// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"github.com/dop251/goja"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
)

// installState installs the synchronous `state` global: a read-only projection
// of the materialized stream/state.Store (open orders, fills, balances, tickers,
// orderbooks, public trades, connection health). It is a sibling of `ta` — a
// plain synchronous global usable everywhere (--init, --where, --on), NOT behind
// the inWhere deny-gate that blocks the async korbit.*/db.* surface, because a
// state read is a local memory lookup, not I/O.
//
// The Store is built (and fed) only when --stateful is set; the methods are
// always installed so the disabled path is a clean, catchable throw rather than
// a missing global. Every method first checks r.stateful and throws
// "state.* requires --stateful" when off.
//
// The Store is applied (Runtime.Ingest) and read here on the SAME event-loop
// goroutine, so no lock is needed — identical to how the TUI confines its store
// to one update-loop goroutine. Returned values are plain JS objects with
// lowerCamelCase wire field names; money/quantity stay decimal strings, and the
// internal Order.epoch is never surfaced.
func installState(r *Runtime, vm *goja.Runtime) error {
	st := vm.NewObject()

	// store returns the live Store or throws the disabled-state error. The throw
	// is a real Error (catchable) raised the goja way — panic with a JS value.
	store := func() *state.Store {
		if !r.stateful || r.store == nil {
			panic(newJSError(vm, "state.* requires --stateful"))
		}
		return r.store
	}
	// symbolArg reads an optional leading symbol argument ("" when omitted/null).
	symbolArg := func(call goja.FunctionCall) string {
		v := call.Argument(0)
		if goja.IsUndefined(v) || goja.IsNull(v) {
			return ""
		}
		return v.String()
	}

	must(st.Set("openOrders", func(call goja.FunctionCall) goja.Value {
		orders := store().OpenOrders(symbolArg(call))
		out := make([]any, len(orders))
		for i, o := range orders {
			out[i] = orderToJS(o)
		}
		return vm.ToValue(out)
	}))

	must(st.Set("order", func(call goja.FunctionCall) goja.Value {
		s := store()
		o, ok := s.Order(call.Argument(0).ToInteger())
		if !ok {
			return goja.Null()
		}
		return vm.ToValue(orderToJS(o))
	}))

	must(st.Set("fills", func(goja.FunctionCall) goja.Value {
		fills := store().Fills()
		out := make([]any, len(fills))
		for i, f := range fills {
			out[i] = fillToJS(f)
		}
		return vm.ToValue(out)
	}))

	must(st.Set("balances", func(goja.FunctionCall) goja.Value {
		bals := store().Balances()
		out := make([]any, len(bals))
		for i, b := range bals {
			out[i] = balanceToJS(b)
		}
		return vm.ToValue(out)
	}))

	must(st.Set("ticker", func(call goja.FunctionCall) goja.Value {
		t, ok := store().Ticker(symbolArg(call))
		if !ok {
			return goja.Null()
		}
		return vm.ToValue(tickerToJS(t))
	}))

	must(st.Set("orderbook", func(call goja.FunctionCall) goja.Value {
		b, ok := store().Orderbook(symbolArg(call))
		if !ok {
			return goja.Null()
		}
		return vm.ToValue(orderbookToJS(b))
	}))

	must(st.Set("trades", func(call goja.FunctionCall) goja.Value {
		trades := store().Trades(symbolArg(call))
		out := make([]any, len(trades))
		for i, t := range trades {
			out[i] = tradeToJS(t)
		}
		return vm.ToValue(out)
	}))

	must(st.Set("health", func(goja.FunctionCall) goja.Value {
		return vm.ToValue(healthToJS(store().Health()))
	}))

	// allAccounts rolls the store's per-account readiness up across every
	// subscribed sub-account: true only when ready holds for all of them (so a
	// partial private snapshot never reads as complete), and false when no
	// private accounts are subscribed (the balances/openOrders flags stay false,
	// as they did before per-account tracking).
	allAccounts := func(ready func(seq int) bool) bool {
		if len(r.opts.AccountSeqs) == 0 {
			return false
		}
		for _, seq := range r.opts.AccountSeqs {
			if !ready(seq) {
				return false
			}
		}
		return true
	}

	must(st.Set("ready", func(call goja.FunctionCall) goja.Value {
		s := store()
		sym := symbolArg(call)
		return vm.ToValue(map[string]any{
			"ticker":     s.TickerReady(sym),
			"orderbook":  s.OrderbookReady(sym),
			"trades":     s.TradesReady(sym),
			"openOrders": allAccounts(func(seq int) bool { return s.OpenOrdersReady(seq, sym) }),
			"balances":   allAccounts(s.BalancesReady),
		})
	}))

	must(st.Set("notices", func(call goja.FunctionCall) goja.Value {
		n := 0
		if v := call.Argument(0); !goja.IsUndefined(v) && !goja.IsNull(v) {
			n = int(v.ToInteger())
		}
		notices := store().Notices(n)
		out := make([]any, len(notices))
		for i, nt := range notices {
			out[i] = noticeToJS(nt)
		}
		return vm.ToValue(out)
	}))

	return vm.Set("state", st)
}

// --- Store struct -> JS object converters (lowerCamelCase wire names) ---------

// orderToJS renders one order. The internal Order.epoch is deliberately omitted.
// status may be the synthetic "closed" (vanished from an open-orders snapshot
// during a gap, real terminal status not yet known) — treat it as terminal.
func orderToJS(o state.Order) map[string]any {
	return map[string]any{
		"orderId":       o.OrderID,
		"accountSeq":    o.AccountSeq,
		"clientOrderId": o.ClientOrderID,
		"symbol":        o.Symbol,
		"side":          o.Side,
		"orderType":     o.OrderType,
		"price":         o.Price,
		"qty":           o.Qty,
		"amt":           o.Amt,
		"filledQty":     o.FilledQty,
		"filledAmt":     o.FilledAmt,
		"avgPrice":      o.AvgPrice,
		"status":        o.Status,
		"createdAt":     o.CreatedAt,
		"lastFilledAt":  o.LastFilledAt,
	}
}

func balanceToJS(b state.Balance) map[string]any {
	return map[string]any{
		"accountSeq":      b.AccountSeq,
		"currency":        b.Currency,
		"balance":         b.Balance,
		"available":       b.Available,
		"tradeInUse":      b.TradeInUse,
		"withdrawalInUse": b.WithdrawalInUse,
		"avgPrice":        b.AvgPrice,
		"updatedAt":       b.UpdatedAt,
	}
}

func tickerToJS(t state.Ticker) map[string]any {
	return map[string]any{
		"symbol":             t.Symbol,
		"open":               t.Open,
		"high":               t.High,
		"low":                t.Low,
		"close":              t.Close,
		"prevClose":          t.PrevClose,
		"priceChange":        t.PriceChange,
		"priceChangePercent": t.PriceChangePercent,
		"volume":             t.Volume,
		"quoteVolume":        t.QuoteVolume,
		"bestBidPrice":       t.BestBidPrice,
		"bestAskPrice":       t.BestAskPrice,
		"lastTradedAt":       t.LastTradedAt,
	}
}

func orderbookToJS(b state.Orderbook) map[string]any {
	return map[string]any{
		"symbol":    b.Symbol,
		"timestamp": b.Timestamp,
		"bids":      levelsToJS(b.Bids),
		"asks":      levelsToJS(b.Asks),
	}
}

func levelsToJS(levels []state.PriceLevel) []any {
	out := make([]any, len(levels))
	for i, l := range levels {
		out[i] = map[string]any{"price": l.Price, "qty": l.Qty}
	}
	return out
}

func tradeToJS(t state.Trade) map[string]any {
	return map[string]any{
		"tradeId":      t.TradeID,
		"timestamp":    t.Timestamp,
		"price":        t.Price,
		"qty":          t.Qty,
		"isBuyerTaker": t.IsBuyerTaker,
	}
}

func fillToJS(f state.Fill) map[string]any {
	return map[string]any{
		"tradeId":     f.TradeID,
		"orderId":     f.OrderID,
		"accountSeq":  f.AccountSeq,
		"symbol":      f.Symbol,
		"side":        f.Side,
		"price":       f.Price,
		"qty":         f.Qty,
		"fee":         f.Fee,
		"feeCurrency": f.FeeCurrency,
		"isTaker":     f.IsTaker,
		"time":        f.Time,
	}
}

func healthToJS(h state.Health) map[string]any {
	return map[string]any{
		"public":      endpointHealthToJS(h.Public),
		"private":     endpointHealthToJS(h.Private),
		"backfilling": h.Backfilling,
		"gapCount":    h.GapCount,
		"lastDataAt":  h.LastDataAt,
		"dataCount":   h.DataCount,
	}
}

func endpointHealthToJS(e state.EndpointHealth) map[string]any {
	return map[string]any{
		"known":        e.Known,
		"up":           e.Up,
		"lastChangeAt": e.LastChangeAt,
		"lastError":    e.LastError,
	}
}

func noticeToJS(n stream.Notice) map[string]any {
	return map[string]any{
		"code":    string(n.Code),
		"level":   string(n.Level),
		"message": n.Message,
		"details": n.Details,
		"time":    n.Time,
	}
}
