// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"fmt"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/stream"
)

// --- event constructors (raw stream events fed through Ingest) ----------------

func connectedPrivate() stream.Notice {
	return stream.Notice{Code: stream.Connected, Level: stream.LevelInfo, Message: "connected", Time: 1, Details: map[string]any{"endpoint": "private"}}
}

// myOrderFrame builds a live (non-backfill) myOrder WS frame for one order.
func myOrderFrame(orderID int64, status, price, filledQty string) stream.Data {
	payload := fmt.Sprintf(`{"order":{"orders":[{"orderId":%d,"status":%q,"price":%q,"qty":"1","filledQty":%q,"side":"buy","createdAt":1000}]}}`,
		orderID, status, price, filledQty)
	return stream.Data{Channel: stream.ChannelMyOrder, Symbol: "btc_krw", Origin: stream.OriginRealtime, ServerTime: 100, Payload: []byte(payload)}
}

func tickerData(close string) stream.Data {
	payload := fmt.Sprintf(`{"data":{"close":%q,"open":"90","high":"110","low":"80"}}`, close)
	return stream.Data{Channel: stream.ChannelTicker, Symbol: "btc_krw", Origin: stream.OriginRealtime, ServerTime: 100, Payload: []byte(payload)}
}

func myAssetData(currency, available string) stream.Data {
	payload := fmt.Sprintf(`{"asset":{"assets":[{"currency":%q,"balance":%q,"available":%q}]}}`, currency, available, available)
	return stream.Data{Channel: stream.ChannelMyAsset, Origin: stream.OriginRealtime, ServerTime: 100, Payload: []byte(payload)}
}

// balanceSnapshot builds a /v2/balance backfill snapshot for one sub-account —
// the frame that latches per-account balance readiness (a live myAsset delta
// does not).
func balanceSnapshot(accountSeq int, currency, available string) stream.Data {
	payload := fmt.Sprintf(`[{"currency":%q,"balance":%q,"available":%q,"tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`, currency, available, available)
	return stream.Data{Channel: stream.ChannelMyAsset, Origin: stream.OriginBackfill, Source: "/v2/balance", ServerTime: 100, AccountSeq: &accountSeq, Payload: []byte(payload)}
}

// openOrdersSnapshot builds a /v2/openOrders backfill snapshot for one
// sub-account+symbol — the frame that latches per-account open-order readiness.
func openOrdersSnapshot(accountSeq int, symbol string) stream.Data {
	payload := fmt.Sprintf(`[{"orderId":1,"status":"open","symbol":%q,"createdAt":1}]`, symbol)
	return stream.Data{Channel: stream.ChannelMyOrder, Symbol: symbol, Origin: stream.OriginBackfill, Source: "/v2/openOrders", ServerTime: 100, AccountSeq: &accountSeq, Payload: []byte(payload)}
}

// --- tests --------------------------------------------------------------------

// TestStateThrowsWithoutStateful: every state.* method throws a catchable Error
// when --stateful is off, and the Store is never built.
func TestStateThrowsWithoutStateful(t *testing.T) {
	err := runOn(t, Options{
		On: `try { state.openOrders(); throw new Error('expected a throw'); }
			catch (e) { if (!/requires --stateful/.test(e.message)) throw new Error('wrong gate: ' + e.message); }`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// TestStateUsableInWhere: state.* is a synchronous global usable inside --where
// (it is NOT behind the inWhere deny-gate that blocks korbit.*/db.*).
func TestStateUsableInWhere(t *testing.T) {
	r := newTestRuntime(t, Options{Stateful: true, Where: "state.openOrders().length === 0 && Array.isArray(state.fills())"})
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("state.* must be usable in --where: ok=%v err=%v", ok, err)
	}
}

// TestStateEmptyBeforeEvents: with --stateful but no events applied yet (the
// --init situation), reads return empty arrays and null, not an error.
func TestStateEmptyBeforeEvents(t *testing.T) {
	err := runOn(t, Options{
		Stateful: true,
		On: `if (state.openOrders().length !== 0) throw new Error('expected no open orders');
			if (state.fills().length !== 0) throw new Error('expected no fills');
			if (state.balances().length !== 0) throw new Error('expected no balances');
			if (state.order(1) !== null) throw new Error('expected null order');
			if (state.ticker('btc_krw') !== null) throw new Error('expected null ticker');
			if (state.orderbook('btc_krw') !== null) throw new Error('expected null orderbook');
			var h = state.health();
			if (h.private.up !== false || h.dataCount !== 0) throw new Error('expected an empty health');`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// TestStateMaterializesViaIngest: events fed through Ingest are visible to a
// later handler via state.*, with wire field names and decimal-string money.
func TestStateMaterializesViaIngest(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful: true,
		On: `var oo = state.openOrders();
			if (oo.length !== 1) throw new Error('openOrders count: ' + oo.length);
			if (oo[0].orderId !== 987) throw new Error('orderId: ' + oo[0].orderId);
			if (oo[0].status !== 'open') throw new Error('status (unfilled->open): ' + oo[0].status);
			if (oo[0].price !== '139000000') throw new Error('price must stay a string: ' + oo[0].price);
			if (typeof oo[0].epoch !== 'undefined') throw new Error('epoch must not be surfaced');
			var o = state.order(987);
			if (!o || o.side !== 'buy') throw new Error('order() lookup wrong: ' + JSON.stringify(o));
			var t = state.ticker('btc_krw');
			if (!t || t.close !== '141000000') throw new Error('ticker: ' + JSON.stringify(t));
			var b = state.balances();
			if (b.length !== 1 || b[0].currency !== 'btc' || b[0].available !== '0.5') throw new Error('balances: ' + JSON.stringify(b));`,
	})
	r.Ingest(connectedPrivate())
	r.Ingest(myOrderFrame(987, "unfilled", "139000000", "0"))
	r.Ingest(tickerData("141000000"))
	r.Ingest(myAssetData("btc", "0.5"))
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("handler saw the wrong state: %v", err)
	}
}

// TestLastDataAtUsesSystemClock pins the clock-frame split in the state store:
// Health.LastDataAt (a local receive time) is stamped from Options.Now (the system
// clock), NOT from Options.ServerNow (the server-clock estimate, which backs only
// korbit.now()). The two seams return distinct values so a regression that wires
// LastDataAt back to the server clock — or swaps the two — is caught.
func TestLastDataAtUsesSystemClock(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful:  true,
		Now:       func() int64 { return 777_000 },
		ServerNow: func() int64 { return 1_750_000_000_000 },
		On: `var h = state.health();
			if (h.lastDataAt !== 777000) throw new Error('lastDataAt must come from the system clock (Options.Now), got ' + h.lastDataAt);
			if (korbit.now() !== 1750000000000) throw new Error('korbit.now() must be the server clock (Options.ServerNow), got ' + korbit.now());`,
	})
	r.Ingest(tickerData("141000000"))
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestStateOrderAdvancesByProgress: a terminal frame supersedes the open one
// (the order's intrinsic progress, surfaced through state.order). It leaves the
// open set once terminal.
func TestStateOrderAdvancesByProgress(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful: true,
		On: `var o = state.order(987);
			if (!o) throw new Error('order missing');
			if (o.status !== 'filled') throw new Error('expected terminal filled, got ' + o.status);
			if (state.openOrders().length !== 0) throw new Error('a filled order must leave the open set');`,
	})
	r.Ingest(connectedPrivate())
	r.Ingest(myOrderFrame(987, "unfilled", "139000000", "0"))
	r.Ingest(myOrderFrame(987, "filled", "139000000", "1"))
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestStateReadyAndNotices: ready() reflects the freshness latches and notices()
// returns the recent notices newest-first.
func TestStateReadyAndNotices(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful: true,
		On: `var r = state.ready('btc_krw');
			if (r.ticker !== true) throw new Error('ticker should be ready after a frame');
			if (r.orderbook !== false) throw new Error('orderbook never arrived, should be not-ready');
			var n = state.notices();
			if (n.length < 1 || n[0].code !== 'DATA_GAP') throw new Error('expected DATA_GAP newest-first: ' + JSON.stringify(n));`,
	})
	r.Ingest(connectedPrivate())
	r.Ingest(tickerData("141000000"))
	r.Ingest(stream.Notice{Code: stream.DataGap, Level: stream.LevelWarn, Message: "gap", Time: 5, Details: map[string]any{"endpoint": "public"}})
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestStateReadyRequiresAllSubscribedAccounts: state.ready().balances rolls the
// store's PER-ACCOUNT readiness up over the subscribed sub-accounts — true only
// when EVERY subscribed account has snapshotted, so a partial private backfill
// (one account healed, another still failing its retry) never reads as complete.
func TestStateReadyRequiresAllSubscribedAccounts(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful:    true,
		AccountSeqs: []int{1, 2},
		Where:       "state.ready('btc_krw').balances",
	})
	r.Ingest(connectedPrivate())

	// Only account 1 snapshotted: the aggregate must stay false (account 2 pending).
	r.Ingest(balanceSnapshot(1, "krw", "1000000"))
	if ok, err := r.Match(dataEvent(`{}`)); err != nil || ok {
		t.Fatalf("balances must NOT be ready while account 2 is un-snapshotted: ok=%v err=%v", ok, err)
	}

	// Account 2's snapshot lands: now every subscribed account is ready.
	r.Ingest(balanceSnapshot(2, "krw", "2000000"))
	if ok, err := r.Match(dataEvent(`{}`)); err != nil || !ok {
		t.Fatalf("balances must be ready once every subscribed account snapshotted: ok=%v err=%v", ok, err)
	}
}

// TestStateReadyOpenOrdersRequiresAllAccounts: the open-order analogue of the
// balances rollup — ready().openOrders is true only when every subscribed
// account has snapshotted the symbol.
func TestStateReadyOpenOrdersRequiresAllAccounts(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful:    true,
		AccountSeqs: []int{1, 2},
		Where:       "state.ready('btc_krw').openOrders",
	})
	r.Ingest(connectedPrivate())

	r.Ingest(openOrdersSnapshot(1, "btc_krw"))
	if ok, err := r.Match(dataEvent(`{}`)); err != nil || ok {
		t.Fatalf("openOrders must NOT be ready while account 2 is un-snapshotted: ok=%v err=%v", ok, err)
	}

	r.Ingest(openOrdersSnapshot(2, "btc_krw"))
	if ok, err := r.Match(dataEvent(`{}`)); err != nil || !ok {
		t.Fatalf("openOrders must be ready once every subscribed account snapshotted: ok=%v err=%v", ok, err)
	}
}

// TestStateReadyFalseWithoutSubscribedAccounts: with no private accounts
// subscribed (a public-only session), the private-readiness flags stay false
// rather than reading vacuously true.
func TestStateReadyFalseWithoutSubscribedAccounts(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful: true, // no AccountSeqs
		Where:    "state.ready('btc_krw').balances === false && state.ready('btc_krw').openOrders === false",
	})
	r.Ingest(connectedPrivate())
	if ok, err := r.Match(dataEvent(`{}`)); err != nil || !ok {
		t.Fatalf("private readiness must be false when no accounts are subscribed: ok=%v err=%v", ok, err)
	}
}

// TestIngestNoopWithoutStore: Ingest is a no-op (no panic, no effect) when the
// Store was never built — the non-stateful monitor path calls it for every event.
func TestIngestNoopWithoutStore(t *testing.T) {
	r := newTestRuntime(t, Options{On: "1"})
	r.Ingest(connectedPrivate())
	r.Ingest(tickerData("100"))
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestStateOpenOrdersSymbolFilter: openOrders(symbol) scopes to one symbol;
// omitting it returns all.
func TestStateOpenOrdersSymbolFilter(t *testing.T) {
	r := newTestRuntime(t, Options{
		Stateful: true,
		On: `if (state.openOrders().length !== 1) throw new Error('all-symbols count: ' + state.openOrders().length);
			if (state.openOrders('eth_krw').length !== 0) throw new Error('eth_krw should have none');
			if (state.openOrders('btc_krw').length !== 1) throw new Error('btc_krw should have one');`,
	})
	r.Ingest(connectedPrivate())
	r.Ingest(myOrderFrame(987, "unfilled", "139000000", "0"))
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestStateOrderArgRequired is a guard: a bare numeric id reaches order().
func TestStateOrderArg(t *testing.T) {
	r := newTestRuntime(t, Options{Stateful: true, Where: "state.order(404) === null"})
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("order(unknown) must be null: ok=%v err=%v", ok, err)
	}
}
