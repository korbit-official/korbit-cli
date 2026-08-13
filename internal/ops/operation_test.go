// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// apiWithRaw builds an API over the scripted fake with the typed rawapi layer
// wired to the same fake.
func apiWithRaw(f *fakeClient, extra apiExtra) *API {
	return apiOver(f, extra)
}

func findOp(t *testing.T, id ...string) Operation {
	t.Helper()
	op := Find(id...)
	if op == nil {
		t.Fatalf("operation %v not in catalog", id)
	}
	return op
}

func runInput(values map[string]string) RunInput {
	return RunInput{Values: values}
}

// ---- place operation: reconcile protocol ----

func TestOpPlaceSuccessMintsAndFetches(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders", step{data: json.RawMessage(`{"orderId":987}`)})
	f.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "order", "place")

	res, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "accountSeq", "1")))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if f.countOf("POST", "/v2/orders") != 1 {
		t.Fatalf("expected 1 POST, got %d", f.countOf("POST", "/v2/orders"))
	}
	posts := f.callsOf("POST", "/v2/orders")
	if posts[0].pol.Idempotent {
		t.Fatalf("place must be single-shot (non-idempotent policy)")
	}
	if len(cidOf(posts[0].call.Params)) != 36 {
		t.Fatalf("expected a minted UUIDv7 clientOrderId, got %q", cidOf(posts[0].call.Params))
	}
	if f.countOf("GET", "/v2/orders") != 1 {
		t.Fatalf("expected one follow-up fetch")
	}
	if jsonNumberField(res.Data, "orderId") != "987" {
		t.Fatalf("expected the full fetched order, got %s", res.Data)
	}
}

// TestOpAccountSeqRequiredPanicsOnAbsence documents the ops-layer contract:
// accountSeq is optional at the user surface (CLI/MCP/bot users may omit it),
// but REQUIRED in RunInput.Values by the time ops.Run is called. The frontend
// must call accountseq.Ensure before dispatching to ops. If a frontend path
// forgets, the ops layer panics immediately to catch the bug in tests.
func TestOpAccountSeqRequiredPanicsOnAbsence(t *testing.T) {
	f := newFakeClient()
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "balance")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when accountSeq is missing from values")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "BUG") {
			t.Fatalf("unexpected panic value: %v", r)
		}
	}()
	// Omit accountSeq — simulates a frontend that forgot accountseq.Ensure.
	op.Run(context.Background(), a, runInput(m()))
}

func TestOpPlaceAmbiguousThenDuplicateResolves(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders",
		step{err: errStr("connection reset")},
		step{err: apiErr(409, "DUPLICATE_CLIENT_ORDER_ID")},
	)
	f.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	var finishes []string
	a := apiWithRaw(f, apiExtra{OrderJournal: func(p map[string]string, _ string) (OrderFinishFunc, error) {
		if p["clientOrderId"] != "cid-1" {
			t.Errorf("journal saw clientOrderId %q", p["clientOrderId"])
		}
		return func(status, orderID, _ string, _ int) {
			finishes = append(finishes, status+"/"+orderID)
		}, nil
	}})
	op := findOp(t, "order", "place")
	res, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	posts := f.callsOf("POST", "/v2/orders")
	if len(posts) != 2 {
		t.Fatalf("expected 2 sends (resend on ambiguous), got %d", len(posts))
	}
	for i, p := range posts {
		if cidOf(p.call.Params) != "cid-1" {
			t.Fatalf("send %d used a different clientOrderId %q", i, cidOf(p.call.Params))
		}
	}
	if len(finishes) != 1 || finishes[0] != "accepted/987" {
		t.Fatalf("expected one accepted/987 finish, got %v", finishes)
	}
	if jsonNumberField(res.Data, "orderId") != "987" {
		t.Fatalf("expected the full order")
	}
}

func TestOpPlaceTimeWindowResyncsOnce(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders",
		step{err: apiErr(400, "EXCEED_TIME_WINDOW")},
		step{data: json.RawMessage(`{"orderId":987}`)},
	)
	f.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	resyncs := 0
	a := apiWithRaw(f, apiExtra{Resync: func() error { resyncs++; return nil }})
	op := findOp(t, "order", "place")
	_, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
	if err != nil {
		t.Fatalf("place after resync: %v", err)
	}
	if resyncs != 1 {
		t.Fatalf("expected exactly one resync, got %d", resyncs)
	}
	if f.countOf("POST", "/v2/orders") != 2 {
		t.Fatalf("expected the corrected resend")
	}
}

func TestOpPlaceCleanRejectionNoResend(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders", step{err: apiErr(400, "INSUFFICIENT_BALANCE")})
	var fins []finRec
	a := apiWithRaw(f, apiExtra{OrderJournal: captureFinishes(&fins)})
	op := findOp(t, "order", "place")
	_, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
	if apiErrOf(err) == nil || apiErrOf(err).Code != "INSUFFICIENT_BALANCE" {
		t.Fatalf("expected the clean rejection to surface, got %v", err)
	}
	if f.countOf("POST", "/v2/orders") != 1 {
		t.Fatalf("a clean rejection must not be resent")
	}
	if len(fins) != 1 || fins[0].status != "failed" || fins[0].code != "INSUFFICIENT_BALANCE" {
		t.Fatalf("expected failed/INSUFFICIENT_BALANCE, got %+v", fins)
	}
}

func TestOpPlaceEndgameNotVisibleIsUnknown(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders", step{err: errStr("timeout")})
	f.on("GET", "/v2/orders", step{err: apiErr(404, "ORDER_NOT_FOUND")})
	var fins []finRec
	a := apiWithRaw(f, apiExtra{OrderJournal: captureFinishes(&fins)})
	a.RetryBudgetMs = 0
	op := findOp(t, "order", "place")
	_, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("expected state-unknown, got %v", err)
	}
	if len(fins) != 1 || fins[0].status != "unknown" {
		t.Fatalf("expected the order row journaled unknown, got %+v", fins)
	}
}

// TestOpPlaceSkipReconcile pins that Controls.SkipReconcile takes the single-shot
// ack path: exactly one send, NO follow-up lookup, the raw accept returned.
func TestOpPlaceSkipReconcile(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders", step{data: json.RawMessage(`{"orderId":987}`)})
	var fins []finRec
	a := apiWithRaw(f, apiExtra{OrderJournal: captureFinishes(&fins)})
	op := findOp(t, "order", "place")
	in := RunInput{Values: m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1"), Controls: Controls{SkipReconcile: true}}
	res, err := op.Run(context.Background(), a, in)
	if err != nil {
		t.Fatalf("ack place: %v", err)
	}
	if jsonNumberField(res.Data, "orderId") != "987" {
		t.Fatalf("expected the raw accept ack, got %s", res.Data)
	}
	if f.countOf("POST", "/v2/orders") != 1 || f.countOf("GET", "/v2/orders") != 0 {
		t.Fatalf("skip-reconcile must be a single shot with no lookup: posts=%d gets=%d",
			f.countOf("POST", "/v2/orders"), f.countOf("GET", "/v2/orders"))
	}
	if len(fins) != 1 || fins[0].status != "accepted" || fins[0].orderID != "987" {
		t.Fatalf("expected accepted/987, got %+v", fins)
	}
}

func TestOpPlaceSkipReconcileAmbiguousIsUnknown(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders", step{err: errStr("connection reset")})
	var fins []finRec
	a := apiWithRaw(f, apiExtra{OrderJournal: captureFinishes(&fins)})
	op := findOp(t, "order", "place")
	in := RunInput{Values: m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1"), Controls: Controls{SkipReconcile: true}}
	_, err := op.Run(context.Background(), a, in)
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("expected UNKNOWN on an ambiguous single-shot, got %v", err)
	}
	if f.countOf("GET", "/v2/orders") != 0 {
		t.Fatalf("skip-reconcile must never look up")
	}
	if len(fins) != 1 || fins[0].status != "unknown" {
		t.Fatalf("expected journal status unknown, got %+v", fins)
	}
}

func TestOpPlaceJournalFailureBlocksSend(t *testing.T) {
	f := newFakeClient()
	a := apiWithRaw(f, apiExtra{OrderJournal: func(map[string]string, string) (OrderFinishFunc, error) {
		return nil, errStr("journal: disk full")
	}})
	op := findOp(t, "order", "place")
	_, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("expected the journal failure, got %v", err)
	}
	if f.countOf("POST", "/v2/orders") != 0 {
		t.Fatalf("the hard guarantee: nothing may be sent when the journal fails")
	}
}

// ---- history / fills paging ----

func TestOpHistoryDedupesAndDefaultsWindow(t *testing.T) {
	f := newFakeClient()
	full := make([]json.RawMessage, 1000)
	for i := 0; i < 1000; i++ {
		full[i] = json.RawMessage(fmt.Sprintf(`{"orderId":%d,"createdAt":%d}`, 2000-i, 2000-i))
	}
	f.on("GET", "/v2/allOrders",
		step{data: joinRows(full)},
		step{data: json.RawMessage(`[{"orderId":1001,"createdAt":1001},{"orderId":1000,"createdAt":1000}]`)},
	)
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "order", "history")
	res, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "startTime", "1", "accountSeq", "1")))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(res.Data, &rows); err != nil {
		t.Fatalf("result not an array: %v", err)
	}
	if len(rows) != 1001 {
		t.Fatalf("expected 1001 deduped rows, got %d", len(rows))
	}
	if res.Truncated {
		t.Fatalf("a fully covered window must not be truncated")
	}
}

func TestOpHistoryLimitCapsRows(t *testing.T) {
	f := newFakeClient()
	full := make([]json.RawMessage, 1000)
	for i := 0; i < 1000; i++ {
		full[i] = json.RawMessage(fmt.Sprintf(`{"orderId":%d,"createdAt":%d}`, 2000-i, 2000-i))
	}
	// A saturated first page walks on; the second short page completes the
	// window (1001 deduped rows). --limit 10 trims to the newest 10, and a
	// satisfied cap is reported complete, never truncated.
	f.on("GET", "/v2/allOrders",
		step{data: joinRows(full)},
		step{data: json.RawMessage(`[{"orderId":1001,"createdAt":1001}]`)},
	)
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "order", "history")
	res, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "startTime", "1", "limit", "10", "accountSeq", "1")))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(res.Data, &rows); err != nil {
		t.Fatalf("result not an array: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("expected --limit 10 to cap at 10 rows, got %d", len(rows))
	}
	if res.Truncated {
		t.Fatalf("a satisfied --limit cap must not be reported truncated")
	}
	if id := jsonNumberField(rows[0], "orderId"); id != "2000" {
		t.Fatalf("expected the newest order (2000) first, got %s", id)
	}
}

func TestOpHistoryPagePolicyIsIdempotent(t *testing.T) {
	f := newFakeClient()
	f.on("GET", "/v2/allOrders", step{data: json.RawMessage(`[]`)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "order", "history")
	if _, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "startTime", "1", "accountSeq", "1"))); err != nil {
		t.Fatalf("history: %v", err)
	}
	calls := f.callsOf("GET", "/v2/allOrders")
	if len(calls) == 0 || !calls[0].pol.Idempotent {
		t.Fatalf("a read page must use an idempotent policy, got %+v", calls)
	}
}

func TestOpFillsPagePassesParams(t *testing.T) {
	f := newFakeClient()
	f.on("GET", "/v2/myTrades", step{data: json.RawMessage(`[]`)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "fills")
	if _, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "startTime", "1", "endTime", "2000", "accountSeq", "1"))); err != nil {
		t.Fatalf("fills: %v", err)
	}
	calls := f.callsOf("GET", "/v2/myTrades")
	if len(calls) == 0 {
		t.Fatalf("expected a fills page request")
	}
	if cidByKey(calls[0].call.Params, "symbol") != "btc_krw" {
		t.Fatalf("expected symbol on the wire, got %q", cidByKey(calls[0].call.Params, "symbol"))
	}
	if cidByKey(calls[0].call.Params, "startTime") != "1" {
		t.Fatalf("expected startTime=1, got %q", cidByKey(calls[0].call.Params, "startTime"))
	}
}

// ---- candles auto-paging ----

func TestOpCandlesAutoPages(t *testing.T) {
	f := newFakeClient()
	f.on("GET", "/v2/candles",
		step{data: candleRows(1000, 200)},
		step{data: candleRows(800, 100)},
	)
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "candles")
	res, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "interval", "1", "limit", "300")))
	if err != nil {
		t.Fatalf("candles: %v", err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(res.Data, &rows); err != nil {
		t.Fatalf("not an array: %v", err)
	}
	if len(rows) != 300 {
		t.Fatalf("expected 300 candles, got %d", len(rows))
	}
	if f.countOf("GET", "/v2/candles") != 2 {
		t.Fatalf("expected 2 pages")
	}
	if jsonInt64Field(rows[0], "timestamp") != 701 {
		t.Fatalf("expected ascending order starting at 701, got %d", jsonInt64Field(rows[0], "timestamp"))
	}
}

func TestOpCandlesSinglePageNoPaging(t *testing.T) {
	f := newFakeClient()
	f.on("GET", "/v2/candles", step{data: candleRows(1000, 100)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "candles")
	if _, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "interval", "1", "limit", "100"))); err != nil {
		t.Fatalf("candles: %v", err)
	}
	if f.countOf("GET", "/v2/candles") != 1 {
		t.Fatalf("a limit within the server cap must be a single GET")
	}
}

// ---- funding cap ----

func TestOpFundingHistoryTruncationNote(t *testing.T) {
	f := newFakeClient()
	full := make([]json.RawMessage, 100)
	for i := range full {
		full[i] = json.RawMessage(`{"id":1}`)
	}
	f.on("GET", "/v2/coin/recentDeposits", step{data: joinRows(full)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "deposit", "history")
	res, err := op.Run(context.Background(), a, runInput(m("currency", "btc", "accountSeq", "1")))
	if err != nil {
		t.Fatalf("funding: %v", err)
	}
	if !res.Truncated || !strings.Contains(res.Note, "cannot be reached") {
		t.Fatalf("expected a saturation note, got truncated=%v note=%q", res.Truncated, res.Note)
	}
	calls := f.callsOf("GET", "/v2/coin/recentDeposits")
	if cidByKey(calls[0].call.Params, "limit") != "100" {
		t.Fatalf("expected limit=100 on the wire, got %q", cidByKey(calls[0].call.Params, "limit"))
	}
}

// ---- passthrough policy derivation ----

func TestOpPassthroughReadPolicy(t *testing.T) {
	f := newFakeClient()
	f.on("GET", "/v2/tickers", step{data: json.RawMessage(`[{"symbol":"btc_krw"}]`)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "ticker")
	if _, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw"))); err != nil {
		t.Fatalf("ticker: %v", err)
	}
	calls := f.callsOf("GET", "/v2/tickers")
	if len(calls) != 1 || !calls[0].pol.Idempotent || calls[0].pol.BudgetMs != 5000 {
		t.Fatalf("a GET must derive an idempotent budgeted policy, got %+v", calls)
	}
}

func TestOpMoneyMoverSingleShot(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/coin/withdrawal", step{data: json.RawMessage(`{"status":"pending"}`)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "withdraw", "request")
	if _, err := op.Run(context.Background(), a, runInput(m("currency", "btc", "amount", "0.1", "address", "addr", "accountSeq", "1"))); err != nil {
		t.Fatalf("withdraw request: %v", err)
	}
	calls := f.callsOf("POST", "/v2/coin/withdrawal")
	if len(calls) != 1 || calls[0].pol.Idempotent {
		t.Fatalf("a money-mover must be single-shot (non-idempotent), got %+v", calls)
	}
}

func TestOpIdempotentWriteRetryable(t *testing.T) {
	f := newFakeClient()
	f.on("DELETE", "/v2/orders", step{data: json.RawMessage(`{"success":true}`)})
	a := apiWithRaw(f, apiExtra{})
	op := findOp(t, "order", "cancel")
	if _, err := op.Run(context.Background(), a, runInput(m("symbol", "btc_krw", "orderId", "123", "accountSeq", "1"))); err != nil {
		t.Fatalf("order cancel: %v", err)
	}
	calls := f.callsOf("DELETE", "/v2/orders")
	if len(calls) != 1 || !calls[0].pol.Idempotent {
		t.Fatalf("an idempotent write must opt into the retry ladder, got %+v", calls)
	}
}

// ---- cross-field validation via OpMeta.CrossValidate ----

func TestOpCrossValidatePlaceMatrix(t *testing.T) {
	op := findOp(t, "order", "place")
	cv := op.Meta().CrossValidate
	if cv == nil {
		t.Fatal("order place must carry a CrossValidate hook")
	}
	cases := []struct {
		name    string
		values  map[string]string
		wantErr string
	}{
		{"limit ok", m("orderType", "limit", "side", "buy", "price", "100", "qty", "1"), ""},
		{"limit missing qty", m("orderType", "limit", "side", "buy", "price", "100"), "requires both --price and --qty"},
		{"market buy with qty", m("orderType", "market", "side", "buy", "qty", "1"), "size it with --amt"},
		{"market sell ok", m("orderType", "market", "side", "sell", "qty", "1"), ""},
		{"best missing tif", m("orderType", "best", "side", "buy", "amt", "5000", "bestNth", "1"), "require --tif"},
		{"pp-percent without pp", m("orderType", "limit", "side", "buy", "price", "100", "qty", "1", "ppPercent", "5"), "requires --pp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cv(tc.values)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestOpCrossValidateIDXor(t *testing.T) {
	for _, key := range [][]string{{"order", "get"}, {"order", "cancel"}} {
		op := findOp(t, key...)
		cv := op.Meta().CrossValidate
		if cv == nil {
			t.Fatalf("%v must carry a CrossValidate hook", key)
		}
		if err := cv(m("orderId", "1")); err != nil {
			t.Errorf("%v: one id should be ok, got %v", key, err)
		}
		if err := cv(m("orderId", "1", "clientOrderId", "c")); err == nil {
			t.Errorf("%v: both ids must error", key)
		}
		if err := cv(m()); err == nil {
			t.Errorf("%v: neither id must error", key)
		}
	}
}

func TestOpCrossValidateTimeWindow(t *testing.T) {
	for _, key := range [][]string{{"candles"}, {"order", "history"}, {"fills"}} {
		op := findOp(t, key...)
		cv := op.Meta().CrossValidate
		if cv == nil {
			t.Fatalf("%v must carry a CrossValidate hook", key)
		}
		if err := cv(m("startTime", "100", "endTime", "200")); err != nil {
			t.Errorf("%v: end after start should be ok, got %v", key, err)
		}
		if err := cv(m("startTime", "200", "endTime", "100")); err == nil || !strings.Contains(err.Error(), "--end must be after --start") {
			t.Errorf("%v: end before start must error, got %v", key, err)
		}
	}
}
