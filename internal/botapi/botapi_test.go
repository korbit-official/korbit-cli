// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

// Req is the recorded shape of one logical call the fake ops.Doer saw. It keeps
// the field names the assertions read (Method/Path/Auth/Idempotent/Params) so
// the behavior oracle is unchanged after the L2 ops layer landed: Idempotent is
// the apiclient.Policy.Idempotent ops derived (the spec retry gate), Auth is the
// apiclient.Call.Auth.
type Req struct {
	Method     string
	Path       string
	Params     []apiclient.KV
	Auth       bool
	Idempotent bool
}

// fakeTransport scripts responses per "METHOD path" and records every request.
// It is the fake L1 client (ops.Doer): the real ops.API runs over it, so the
// place/history/candles/funding protocols and the spec-derived policy are all
// exercised exactly as in production.
type fakeTransport struct {
	mu    sync.Mutex
	reqs  []Req
	steps map[string][]step // consumed front-to-back; the last step repeats
	gate  chan struct{}     // when set, every call blocks until the gate closes
}

type step struct {
	data json.RawMessage
	err  error
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{steps: map[string][]step{}}
}

func (f *fakeTransport) on(method, path string, s ...step) {
	f.steps[method+" "+path] = s
}

// Do implements ops.Doer: it records the call (translating the policy's
// idempotency back into the Req.Idempotent the assertions read) and returns the
// scripted step. Attempts is always 1 (the fake does no retry of its own; the
// ops protocols decide every resend).
func (f *fakeTransport) Do(_ context.Context, call apiclient.Call, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, Req{
		Method: call.Method, Path: call.Path, Params: call.Params,
		Auth: call.Auth, Idempotent: pol.Idempotent,
	})
	key := call.Method + " " + call.Path
	queue := f.steps[key]
	if len(queue) == 0 {
		f.mu.Unlock()
		return nil, apiclient.Meta{Attempts: 1}, fmt.Errorf("unexpected call %s", key)
	}
	s := queue[0]
	if len(queue) > 1 {
		f.steps[key] = queue[1:]
	}
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return s.data, apiclient.Meta{Attempts: 1}, s.err
}

func (f *fakeTransport) calls(method, path string) []Req {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Req
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func noSleep(time.Duration) {}

// apiExtra carries the per-test ops.API seams (the place protocol's journal and
// clock-resync hooks); zero values are fine for tests that don't place orders.
type apiExtra struct {
	OrderJournal orderJournalFunc
	Resync       func() error
	Sleep        func(time.Duration)
	ServerNow    func() int64
}

// orderJournalFunc is the test seam for the place protocol's order-row hook: a
// factory returning an OrderFinishFunc, mirroring the production OpHandle's
// StartOrder, so tests assert the journaled outcome without SQLite.
type orderJournalFunc func(params map[string]string, paramsJSON string) (ops.OrderFinishFunc, error)

// fakeOpJournal wraps an orderJournalFunc as a minimal ops.OpJournal/OpHandle so
// the bot tests drive the operations layer through the real Journal seam.
type fakeOpJournal struct{ oj orderJournalFunc }

func (f fakeOpJournal) Begin(ops.OpStart) (ops.OpHandle, error) {
	return &fakeOpHandle{oj: f.oj}, nil
}

type fakeOpHandle struct{ oj orderJournalFunc }

func (h *fakeOpHandle) OperationID() int64                { return 0 }
func (h *fakeOpHandle) ForCall(string) apiclient.Recorder { return nil }
func (h *fakeOpHandle) StartOrder(in ops.OrderIntent) (ops.OrderFinishFunc, error) {
	if h.oj == nil {
		return func(string, string, string, int) {}, nil
	}
	params := map[string]string{
		"clientOrderId": in.ClientOrderID, "symbol": in.Symbol, "side": in.Side,
		"orderType": in.OrderType, "price": in.Price, "qty": in.Qty, "amt": in.Amt,
		"timeInForce": in.Tif,
	}
	return h.oj(params, in.ParamsJSON)
}
func (h *fakeOpHandle) Finish(string, string) error { return nil }

// api builds the real ops.API over the fake L1 client, with the same defaults
// the production wiring uses (5000ms retry budget, the test's fixed clock).
func (f *fakeTransport) api(extra apiExtra) *ops.API {
	sleep := extra.Sleep
	if sleep == nil {
		sleep = noSleep
	}
	serverNow := extra.ServerNow
	if serverNow == nil {
		serverNow = func() int64 { return 1_750_000_000_000 }
	}
	return &ops.API{
		Raw:           rawapi.New(f, nil),
		Resync:        extra.Resync,
		ServerNow:     serverNow,
		Sleep:         sleep,
		RetryBudgetMs: 5000,
		Journal:       fakeOpJournal{oj: extra.OrderJournal},
	}
}

func newTestRuntime(t *testing.T, opts Options) *Runtime {
	t.Helper()
	if opts.Sleep == nil {
		opts.Sleep = noSleep
	}
	if opts.ServerNow == nil {
		opts.ServerNow = func() int64 { return 1_750_000_000_000 }
	}
	r, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

func dataEvent(payload string) Event {
	return Event{Type: "data", Channel: "ticker", Symbol: "btc_krw", Origin: "realtime", ServerTime: 1, Payload: []byte(payload)}
}

// runOn runs a one-shot --on handler against a single event and returns its error.
func runOn(t *testing.T, opts Options, ev Event) error {
	t.Helper()
	r := newTestRuntime(t, opts)
	return r.RunHandler(ev)
}

func TestWhereMatchAndInitGlobals(t *testing.T) {
	r := newTestRuntime(t, Options{
		Init:  "var threshold = 100",
		Where: "Number(payload.data.close) > threshold",
	})
	ok, err := r.Match(dataEvent(`{"data":{"close":"150"}}`))
	if err != nil || !ok {
		t.Fatalf("expected match, got ok=%v err=%v", ok, err)
	}
	ok, err = r.Match(dataEvent(`{"data":{"close":"50"}}`))
	if err != nil || ok {
		t.Fatalf("expected no match, got ok=%v err=%v", ok, err)
	}
}

func TestWhereExceptionIsPerEvent(t *testing.T) {
	r := newTestRuntime(t, Options{Where: "payload.no.such.field"})
	_, err := r.Match(dataEvent(`{"data":{}}`))
	if err == nil || !strings.Contains(err.Error(), "--where threw") {
		t.Fatalf("expected a --where exception, got %v", err)
	}
	// The runtime survives for the next event.
	if _, err := r.Match(dataEvent(`{"no":1}`)); err == nil {
		t.Fatalf("expected another exception")
	}
}

func TestKorbitDeniedInWhere(t *testing.T) {
	ft := newFakeTransport()
	r := newTestRuntime(t, Options{Where: "api.ticker('btc_krw')", API: ft.api(apiExtra{})})
	_, err := r.Match(dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "not available inside --where") {
		t.Fatalf("expected the --where gate, got %v", err)
	}
	if len(ft.calls("GET", "/v2/tickers")) != 0 {
		t.Fatalf("transport must not have been called")
	}
}

func TestDBDeniedInWhere(t *testing.T) {
	r := newTestRuntime(t, Options{Where: "db.query('SELECT 1')", DBPath: filepath.Join(t.TempDir(), "bot.db")})
	_, err := r.Match(dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "not available inside --where") {
		t.Fatalf("expected the --where gate, got %v", err)
	}
}

func TestOnAwaitsAPICall(t *testing.T) {
	ft := newFakeTransport()
	ft.on("GET", "/v2/tickers", step{data: json.RawMessage(`{"btc_krw":{"close":"100"}}`)})
	var got string
	err := runOn(t, Options{
		API:    ft.api(apiExtra{}),
		On:     "var tk = await api.ticker('btc_krw'); report(tk.btc_krw.close)",
		Stderr: writerFunc(func(p []byte) { got += string(p) }),
		Init:   "function report(v){ console.log('close=' + v) }",
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}
	if !strings.Contains(got, "close=100") {
		t.Fatalf("console output missing, got %q", got)
	}
	calls := ft.calls("GET", "/v2/tickers")
	if len(calls) != 1 || calls[0].Auth || !calls[0].Idempotent {
		t.Fatalf("unexpected ticker call shape: %+v", calls)
	}
	if len(calls[0].Params) != 1 || calls[0].Params[0].Key != "symbol" || calls[0].Params[0].Value != "btc_krw" {
		t.Fatalf("unexpected params: %+v", calls[0].Params)
	}
}

type writerFunc func(p []byte)

func (w writerFunc) Write(p []byte) (int, error) { w(p); return len(p), nil }

func TestPromiseAllRunsConcurrently(t *testing.T) {
	ft := newFakeTransport()
	gate := make(chan struct{})
	ft.gate = gate
	ft.on("GET", "/v2/tickers", step{data: json.RawMessage(`{}`)})
	ft.on("GET", "/v2/orderbook", step{data: json.RawMessage(`{}`)})

	r := newTestRuntime(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await Promise.all([api.ticker('btc_krw'), api.orderbook('btc_krw')])",
	})
	done := make(chan error, 1)
	go func() { done <- r.RunHandler(dataEvent(`{}`)) }()

	// Both calls must be IN FLIGHT at once (parked on the gate) — that is the
	// worker-pool parallelism Promise.all relies on.
	deadline := time.After(5 * time.Second)
	for {
		ft.mu.Lock()
		n := len(ft.reqs)
		ft.mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 2 concurrent calls, have %d", n)
		case <-time.After(time.Millisecond):
		}
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("handler failed: %v", err)
	}
}

func TestUnhandledRejectionIsFatal(t *testing.T) {
	ft := newFakeTransport()
	ft.on("GET", "/v2/tickers", step{err: &output.ApiError{Message: "nope", HTTPStatus: 400, Code: "BAD"}})
	r := newTestRuntime(t, Options{
		API: ft.api(apiExtra{}),
		On:  "api.ticker('btc_krw'); 1", // fire-and-forget, nobody catches
	})
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("the handler itself should succeed, got %v", err)
	}
	select {
	case err := <-r.Fatal():
		if !strings.Contains(err.Error(), "unhandled promise rejection") {
			t.Fatalf("unexpected fatal: %v", err)
		}
		var ae *output.ApiError
		if !errors.As(err, &ae) || ae.Code != "BAD" {
			t.Fatalf("the API error classification must survive: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("expected a fatal unhandled rejection")
	}
}

func TestHandlerRejectionCarriesApiError(t *testing.T) {
	ft := newFakeTransport()
	ft.on("GET", "/v2/balance", step{err: &output.ApiError{Message: "denied", HTTPStatus: 403, Code: "FORBIDDEN"}})
	err := runOn(t, Options{API: ft.api(apiExtra{}), On: "await api.balance()"}, dataEvent(`{}`))
	var ae *output.ApiError
	if !errors.As(err, &ae) || ae.Code != "FORBIDDEN" || ae.HTTPStatus != 403 {
		t.Fatalf("expected the ApiError to survive the JS boundary, got %v", err)
	}
}

func TestCatchSeesStructuredError(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders", step{err: &output.ApiError{Message: "no funds", HTTPStatus: 400, Code: "INSUFFICIENT_BALANCE"}})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `try {
			await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1'});
			throw new Error('should not get here');
		} catch (e) {
			if (e.code !== 'INSUFFICIENT_BALANCE' || e.httpStatus !== 400) throw new Error('bad shape: ' + e.code + '/' + e.httpStatus);
		}`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("catch should have handled it: %v", err)
	}
}

func TestMoneyMustBeStrings(t *testing.T) {
	ft := newFakeTransport()
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price: 100, qty: '1'})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "decimal STRING") {
		t.Fatalf("expected the money-string guardrail, got %v", err)
	}
	if len(ft.calls("POST", "/v2/orders")) != 0 {
		t.Fatalf("nothing may be sent for a malformed call")
	}
}

func TestSizingMatrixEnforcedBeforeSend(t *testing.T) {
	ft := newFakeTransport()
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', qty:'1'})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "--price") {
		t.Fatalf("expected the sizing matrix to fire, got %v", err)
	}
}

func TestUnknownOptionRejected(t *testing.T) {
	ft := newFakeTransport()
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await api.ticker({symbl:'btc_krw'})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), `unknown option "symbl"`) {
		t.Fatalf("expected an unknown-option error, got %v", err)
	}
}

func TestCredsMissingThrowsClearly(t *testing.T) {
	ft := newFakeTransport()
	err := runOn(t, Options{
		API:      ft.api(apiExtra{}),
		CredsErr: `no key configured; add one with "korbit key add"`,
		On:       "await api.balance()",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "needs a signing key") || !strings.Contains(err.Error(), "no key configured") {
		t.Fatalf("expected the creds guidance, got %v", err)
	}
	// Public methods still work without creds.
	ft.on("GET", "/v2/tickers", step{data: json.RawMessage(`{"symbol":"btc_krw"}`)})
	err = runOn(t, Options{API: ft.api(apiExtra{}), CredsErr: "no key", On: "await api.ticker('btc_krw')"}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("public method should work credless: %v", err)
	}
}

const placedOrderDoc = `{"orderId":987,"clientOrderId":"cid-1","symbol":"btc_krw","status":"filled","price":"100","qty":"1"}`

func TestPlaceMintsClientOrderIdAndFetchesFullOrder(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders", step{data: json.RawMessage(`{"orderId":987}`)})
	ft.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `var o = await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1'});
			if (o.orderId !== 987) throw new Error('not the full order: ' + JSON.stringify(o));
			if (o.price !== '100') throw new Error('money must stay strings');`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("place failed: %v", err)
	}
	posts := ft.calls("POST", "/v2/orders")
	if len(posts) != 1 {
		t.Fatalf("expected exactly one send, got %d", len(posts))
	}
	if posts[0].Idempotent {
		t.Fatalf("order place must go through the transport as non-idempotent")
	}
	var minted string
	for _, kv := range posts[0].Params {
		if kv.Key == "clientOrderId" {
			minted = kv.Value
		}
	}
	if len(minted) != 36 {
		t.Fatalf("expected a minted UUIDv7 clientOrderId, got %q", minted)
	}
	if got := paramValue(posts[0].Params, "accountSeq"); got != "1" {
		t.Fatalf("order place must send the default accountSeq, got %q", got)
	}
	gets := ft.calls("GET", "/v2/orders")
	if len(gets) != 1 {
		t.Fatalf("expected the follow-up fetch, got %d", len(gets))
	}
	if got := paramValue(gets[0].Params, "accountSeq"); got != "1" {
		t.Fatalf("order lookup must use the same default accountSeq, got %q", got)
	}
}

// TestPlaceRegistersLocalHold: on a stateful runtime, api.order.place
// registers the order's local balance hold — sized with the account's
// quote-fee headroom, fetched once and cached per {account, symbol} — before
// the order is sent; state.balances() shows Available net of the in-flight
// order once the promise settles, the hold survives the accept, releases
// when the order is observed on myOrder, and a rejected place releases it by
// the time the rejection is catchable.
func TestPlaceRegistersLocalHold(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders",
		step{data: json.RawMessage(`{"orderId":987}`)},
		step{err: &output.ApiError{Message: "no funds", HTTPStatus: 422, Code: "NO_BALANCE"}})
	ft.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	ft.on("GET", "/v2/tradingFeePolicy", step{data: json.RawMessage(
		`[{"symbol":"btc_krw","buyFeeCurrency":"krw","sellFeeCurrency":"btc","maxFeeRate":"0.002","takerFeeRate":"0.002","makerFeeRate":"0.001"}]`)})
	r := newTestRuntime(t, Options{
		Stateful: true,
		API:      ft.api(apiExtra{}),
		On: `function krw() {
				var rows = state.balances().filter(function (b) { return b.currency === 'krw'; });
				return rows.length ? rows[0].available : 'none';
			}
			if (payload.step === 'place') {
				// 100000 x 3 = 300000, x1.002 quote-fee headroom = 300600.
				await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100000', qty:'3', clientOrderId:'cid-1'});
				if (krw() !== '699400') throw new Error('hold (with fee headroom) must survive the accept: ' + krw());
			} else if (payload.step === 'released') {
				if (krw() !== '1000000') throw new Error('myOrder observation must release the hold: ' + krw());
			} else if (payload.step === 'reject') {
				try {
					await api.order.place({symbol:'btc_krw', side:'buy', orderType:'market', amt:'200000', clientOrderId:'cid-2'});
					throw new Error('place should have been rejected');
				} catch (e) {
					if (e.code !== 'NO_BALANCE') throw e;
				}
				if (krw() !== '1000000') throw new Error('a rejected place must release its hold: ' + krw());
			}`,
	})
	r.Ingest(myAssetData("krw", "1000000"))
	if err := r.RunHandler(dataEvent(`{"step":"place"}`)); err != nil {
		t.Fatalf("place step: %v", err)
	}
	r.Ingest(stream.Data{Channel: stream.ChannelMyOrder, Symbol: "btc_krw", Origin: stream.OriginRealtime, ServerTime: 100,
		Payload: []byte(`{"order":{"orders":[{"orderId":987,"clientOrderId":"cid-1","status":"unfilled","price":"100000","qty":"3","side":"buy","createdAt":1000}]}}`)})
	if err := r.RunHandler(dataEvent(`{"step":"released"}`)); err != nil {
		t.Fatalf("released step: %v", err)
	}
	if err := r.RunHandler(dataEvent(`{"step":"reject"}`)); err != nil {
		t.Fatalf("reject step: %v", err)
	}
	// The fee policy is fetched once and cached per {account, symbol}: two
	// buys on the same key must not fetch twice.
	if n := len(ft.calls("GET", "/v2/tradingFeePolicy")); n != 1 {
		t.Fatalf("fee policy should be fetched exactly once, got %d", n)
	}
}

func paramValue(kvs []apiclient.KV, key string) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

func TestPlaceReconcileNetworkFailThenDuplicate(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders",
		step{err: errors.New("connection reset")}, // ambiguous: may have landed
		step{err: &output.ApiError{Message: "dup", HTTPStatus: 409, Code: "DUPLICATE_CLIENT_ORDER_ID"}},
	)
	ft.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	var finishes []string
	err := runOn(t, Options{
		API: ft.api(apiExtra{
			OrderJournal: func(params map[string]string, paramsJSON string) (ops.OrderFinishFunc, error) {
				if params["clientOrderId"] != "cid-1" {
					t.Errorf("journal saw clientOrderId %q", params["clientOrderId"])
				}
				return func(status, orderID, errCode string, attempts int) {
					finishes = append(finishes, status+"/"+orderID)
				}, nil
			},
		}),
		On: `var o = await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1', clientOrderId:'cid-1'});
			if (o.orderId !== 987) throw new Error('expected the reconciled order');`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if got := len(ft.calls("POST", "/v2/orders")); got != 2 {
		t.Fatalf("expected resend with the same id, got %d sends", got)
	}
	// Both sends must carry the SAME clientOrderId.
	for _, req := range ft.calls("POST", "/v2/orders") {
		found := false
		for _, kv := range req.Params {
			if kv.Key == "clientOrderId" && kv.Value == "cid-1" {
				found = true
			}
		}
		if !found {
			t.Fatalf("a resend changed or dropped the clientOrderId: %+v", req.Params)
		}
	}
	if len(finishes) != 1 || finishes[0] != "accepted/987" {
		t.Fatalf("journal finish mismatch: %v", finishes)
	}
}

func TestPlaceCleanRejectionDoesNotResend(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders", step{err: &output.ApiError{Message: "no funds", HTTPStatus: 400, Code: "INSUFFICIENT_BALANCE"}})
	var finishes []string
	err := runOn(t, Options{
		API: ft.api(apiExtra{
			OrderJournal: func(map[string]string, string) (ops.OrderFinishFunc, error) {
				return func(status, orderID, errCode string, attempts int) {
					finishes = append(finishes, status+"/"+errCode)
				}, nil
			},
		}),
		On: "await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1'})",
	}, dataEvent(`{}`))
	var ae *output.ApiError
	if !errors.As(err, &ae) || ae.Code != "INSUFFICIENT_BALANCE" {
		t.Fatalf("expected the clean rejection to surface, got %v", err)
	}
	if got := len(ft.calls("POST", "/v2/orders")); got != 1 {
		t.Fatalf("a clean rejection must not be resent, got %d sends", got)
	}
	if len(finishes) != 1 || finishes[0] != "failed/INSUFFICIENT_BALANCE" {
		t.Fatalf("journal finish mismatch: %v", finishes)
	}
}

func TestPlaceBudgetExhaustedFinalReconcileFindsOrder(t *testing.T) {
	ft := newFakeTransport()
	// Every send fails ambiguously; the final lookup proves it landed.
	ft.on("POST", "/v2/orders", step{err: errors.New("timeout")})
	ft.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `var o = await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1', clientOrderId:'cid-1'});
			if (o.orderId !== 987) throw new Error('expected the recovered order');`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("final reconcile failed: %v", err)
	}
}

// An ambiguous send whose order is not visible after the verify window is
// UNKNOWN — under eventual consistency we never assert "not placed".
func TestPlaceBudgetExhaustedIsUnknownNotFailed(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders", step{err: errors.New("timeout")})
	ft.on("GET", "/v2/orders", step{err: &output.ApiError{Message: "no such order", HTTPStatus: 404, Code: "ORDER_NOT_FOUND"}})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1', clientOrderId:'cid-1'})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("expected the state-unknown failure, got %v", err)
	}
}

// A confirmed 2xx accept whose full order can't be read back resolves as a
// SUCCESS, and the eventual-consistency note must reach the JS object (not only
// stderr): the resolved order carries the orderId AND a machine-visible
// acknowledgmentOnly flag + note so a bot knows fill state is absent.
func TestPlaceAcceptedButUnreadableSurfacesNoteToJS(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders", step{data: json.RawMessage(`{"orderId":987,"clientOrderId":"cid-1"}`)})
	ft.on("GET", "/v2/orders", step{data: json.RawMessage(`{}`)}) // never visible
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `var o = await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1', clientOrderId:'cid-1'});
			if (o.orderId !== 987) throw new Error('expected the accept ack orderId, got '+JSON.stringify(o));
			if (o.acknowledgmentOnly !== true) throw new Error('expected acknowledgmentOnly flag, got '+JSON.stringify(o));
			if (!o.note) throw new Error('expected a note on the ack');`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("accepted-but-unreadable must resolve as success with a note: %v", err)
	}
}

func TestPlaceStateUnknownWhenLookupAlsoFails(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders", step{err: errors.New("timeout")})
	ft.on("GET", "/v2/orders", step{err: errors.New("still down")})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1', clientOrderId:'cid-1'})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") || !strings.Contains(err.Error(), "cid-1") {
		t.Fatalf("expected the state-unknown failure with instructions, got %v", err)
	}
}

func TestPlaceJournalFailureBlocksSend(t *testing.T) {
	ft := newFakeTransport()
	err := runOn(t, Options{
		API: ft.api(apiExtra{
			OrderJournal: func(map[string]string, string) (ops.OrderFinishFunc, error) {
				return nil, errors.New("journal: disk full")
			},
		}),
		On: "await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1'})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("expected the journal failure, got %v", err)
	}
	if len(ft.calls("POST", "/v2/orders")) != 0 {
		t.Fatalf("the hard guarantee: nothing may be sent when the journal write fails")
	}
}

func TestSingleShotWritesNeverResend(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   string
		path string
	}{
		{"withdraw.request", "await api.withdraw.request('btc', {amount:'0.1', address:'addr1'})", "/v2/coin/withdrawal"},
		{"krw.deposit.request", "await api.krw.deposit.request('50000')", "/v2/krw/sendKrwDepositPush"},
		{"krw.withdraw.request", "await api.krw.withdraw.request('50000')", "/v2/krw/sendKrwWithdrawalPush"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ft := newFakeTransport()
			ft.on("POST", tc.path, step{err: errors.New("connection reset")})
			err := runOn(t, Options{API: ft.api(apiExtra{}), On: tc.on}, dataEvent(`{}`))
			if err == nil || !strings.Contains(err.Error(), "connection reset") {
				t.Fatalf("expected the transport error to surface, got %v", err)
			}
			calls := ft.calls("POST", tc.path)
			if len(calls) != 1 {
				t.Fatalf("single-shot means ONE send, got %d", len(calls))
			}
			if calls[0].Idempotent {
				t.Fatalf("single-shot writes must not opt into transport retry")
			}
		})
	}
}

func TestIdempotentCancelOptsIntoRetry(t *testing.T) {
	ft := newFakeTransport()
	ft.on("DELETE", "/v2/orders", step{data: json.RawMessage(`{"success":true}`)})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await api.order.cancel({symbol:'btc_krw', orderId: 987})",
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	calls := ft.calls("DELETE", "/v2/orders")
	if len(calls) != 1 || !calls[0].Idempotent {
		t.Fatalf("cancel must ride the idempotent retry ladder: %+v", calls)
	}
}

func TestHistoryWalkPagesAndDedupes(t *testing.T) {
	// Page 1: a full 1000 rows (ts 2000..1001, ids 3000..2001) -> saturated.
	// Page 2: a short page proving coverage, overlapping one row.
	page1 := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		page1[i] = fmt.Sprintf(`{"orderId":%d,"createdAt":%d}`, 3000-i, 2000-i)
	}
	page2 := []string{
		`{"orderId":2001,"createdAt":1001}`, // overlap duplicate
		`{"orderId":2000,"createdAt":1000}`,
	}
	ft := newFakeTransport()
	ft.on("GET", "/v2/allOrders",
		step{data: json.RawMessage("[" + strings.Join(page1, ",") + "]")},
		step{data: json.RawMessage("[" + strings.Join(page2, ",") + "]")},
	)
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `var rows = await api.order.history({symbol:'btc_krw', startTime: 500});
			if (rows.length !== 1001) throw new Error('expected 1001 deduped rows, got ' + rows.length);
			if (rows.truncated) throw new Error('must not be marked truncated');`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("history walk failed: %v", err)
	}
	calls := ft.calls("GET", "/v2/allOrders")
	if len(calls) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(calls))
	}
}

func TestHistoryLimitCapsRows(t *testing.T) {
	rows := make([]string, 50)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"tradeId":%d,"tradedAt":%d}`, i+1, 1000+i)
	}
	ft := newFakeTransport()
	ft.on("GET", "/v2/myTrades", step{data: json.RawMessage("[" + strings.Join(rows, ",") + "]")})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `var r = await api.fills({symbol:'btc_krw', limit: 10});
			if (r.length !== 10) throw new Error('limit must cap the rows, got ' + r.length);`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("fills with a --limit total-row cap failed: %v", err)
	}
}

func TestFundingHistoryCapNote(t *testing.T) {
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":%d,"status":"done"}`, i+1)
	}
	ft := newFakeTransport()
	ft.on("GET", "/v2/coin/recentDeposits", step{data: json.RawMessage("[" + strings.Join(rows, ",") + "]")})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `var rows = await api.deposit.history('btc');
			if (rows.length !== 100) throw new Error('rows: ' + rows.length);
			if (rows.truncated !== true) throw new Error('expected the truncation note');
			if (!/cannot be reached/.test(rows.note)) throw new Error('note: ' + rows.note);`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("funding cap test failed: %v", err)
	}
	calls := ft.calls("GET", "/v2/coin/recentDeposits")
	if len(calls) != 1 {
		t.Fatalf("expected one call, got %d", len(calls))
	}
	limit := ""
	for _, kv := range calls[0].Params {
		if kv.Key == "limit" {
			limit = kv.Value
		}
	}
	if limit != "100" {
		t.Fatalf("the binding must default limit to the 100 ceiling, got %q", limit)
	}
}

func TestCandlesAutoPages(t *testing.T) {
	// 300 candles requested: page 1 = 200 (ts 1000..801), page 2 = 100 (ts 800..701).
	mk := func(from, n int) json.RawMessage {
		rows := make([]string, n)
		for i := 0; i < n; i++ {
			rows[i] = fmt.Sprintf(`{"timestamp":%d,"close":"%d"}`, from-i, from-i)
		}
		return json.RawMessage("[" + strings.Join(rows, ",") + "]")
	}
	ft := newFakeTransport()
	ft.on("GET", "/v2/candles", step{data: mk(1000, 200)}, step{data: mk(800, 100)})
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On: `var c = await api.candles('btc_krw', {interval:'1', limit: 300});
			if (c.length !== 300) throw new Error('candles: ' + c.length);
			if (c[0].timestamp !== 701) throw new Error('must be ascending, first=' + c[0].timestamp);
			if (c[299].timestamp !== 1000) throw new Error('last=' + c[299].timestamp);`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("candles paging failed: %v", err)
	}
	calls := ft.calls("GET", "/v2/candles")
	if len(calls) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(calls))
	}
	// The second page must be bounded by end = oldest-1 (the candles endpoint
	// names its time bound "end", not "endTime").
	end := ""
	for _, kv := range calls[1].Params {
		if kv.Key == "end" {
			end = kv.Value
		}
	}
	if end != "800" {
		t.Fatalf("expected end 800 on page 2, got %q", end)
	}
}

func TestCandlesLimitCeiling(t *testing.T) {
	ft := newFakeTransport()
	err := runOn(t, Options{
		API: ft.api(apiExtra{}),
		On:  "await api.candles('btc_krw', {interval:'1', limit: 100000})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "5000") {
		t.Fatalf("expected the candles ceiling, got %v", err)
	}
}

func TestDBRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	err := runOn(t, Options{
		DBPath: dbPath,
		On: `await db.exec('CREATE TABLE IF NOT EXISTS fills (id INTEGER PRIMARY KEY, price TEXT, qty TEXT)');
			var r = await db.exec('INSERT INTO fills (price, qty) VALUES (?, ?)', '100.5', '0.01');
			if (r.lastInsertId !== 1) throw new Error('lastInsertId: ' + r.lastInsertId);
			await db.exec('INSERT INTO fills (price, qty) VALUES (?, ?)', '101.5', '0.02');
			var rows = await db.query('SELECT id, price, qty FROM fills ORDER BY id');
			if (rows.length !== 2) throw new Error('rows: ' + rows.length);
			if (rows[0].price !== '100.5') throw new Error('money must stay TEXT: ' + rows[0].price);
			var one = await db.get('SELECT COUNT(*) AS n FROM fills WHERE price > ?', '101');
			if (one.n !== 1) throw new Error('aggregate: ' + one.n);
			var none = await db.get('SELECT * FROM fills WHERE id = ?', 99);
			if (none !== null) throw new Error('expected null for no row');`,
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("db round-trip failed: %v", err)
	}
}

func TestDBRejectsObjectBinds(t *testing.T) {
	err := runOn(t, Options{
		DBPath: filepath.Join(t.TempDir(), "bot.db"),
		On:     "await db.exec('SELECT ?', {a: 1})",
	}, dataEvent(`{}`))
	if err == nil || !strings.Contains(err.Error(), "JSON.stringify") {
		t.Fatalf("expected the bind guardrail, got %v", err)
	}
}

func TestNoticeEventReachesHandler(t *testing.T) {
	notice := Event{
		Type: "notice", Channel: "notice", ServerTime: 42,
		Payload: []byte(`{"code":"DATA_GAP","level":"warn","message":"gap"}`),
	}
	var got string
	err := runOn(t, Options{
		On: `if (ev.type === 'notice' && payload.code === 'DATA_GAP') console.log('saw-gap')`,
		Stderr: writerFunc(func(p []byte) {
			got += string(p)
		}),
	}, notice)
	if err != nil {
		t.Fatalf("notice handler failed: %v", err)
	}
	if !strings.Contains(got, "saw-gap") {
		t.Fatalf("the handler did not see the notice: %q", got)
	}
}

func TestInitTopLevelAwait(t *testing.T) {
	ft := newFakeTransport()
	ft.on("GET", "/v2/candles", step{data: json.RawMessage(`[{"timestamp":1,"close":"5"}]`)})
	r := newTestRuntime(t, Options{
		API: ft.api(apiExtra{}),
		Init: `var warm = await api.candles('btc_krw', {interval:'1', limit:1});
			globalThis.last = Number(warm[0].close)`,
		Where: "last === 5",
	})
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("init await state not visible: ok=%v err=%v", ok, err)
	}
}

func TestInitFailureIsFatal(t *testing.T) {
	_, err := New(Options{Init: "throw new Error('boom')", Where: "true", Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "--init failed") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the init failure, got %v", err)
	}
}

func TestOnSyntaxErrorReported(t *testing.T) {
	_, err := New(Options{On: "if (", Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "--on is not valid JavaScript") {
		t.Fatalf("expected the syntax error, got %v", err)
	}
}

func TestKorbitGetIsGone(t *testing.T) {
	err := runOn(t, Options{
		API: newFakeTransport().api(apiExtra{}),
		On:  "if (typeof api.get !== 'undefined') throw new Error('api.get must be removed')",
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("%v", err)
	}
}

func TestHandlerSerializationStateVisible(t *testing.T) {
	// Two sequential handler runs share state; the second sees the first's write.
	r := newTestRuntime(t, Options{
		Init: "var count = 0",
		On:   "count++; if (channel === 'check' && count !== 2) throw new Error('count=' + count)",
	})
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("first: %v", err)
	}
	ev := dataEvent(`{}`)
	ev.Channel = "check"
	if err := r.RunHandler(ev); err != nil {
		t.Fatalf("second: %v", err)
	}
}

func TestTimeWindowResyncOnPlace(t *testing.T) {
	ft := newFakeTransport()
	ft.on("POST", "/v2/orders",
		step{err: &output.ApiError{Message: "clock", HTTPStatus: 400, Code: "EXCEED_TIME_WINDOW"}},
		step{data: json.RawMessage(`{"orderId":987}`)},
	)
	ft.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	resyncs := 0
	err := runOn(t, Options{
		API: ft.api(apiExtra{Resync: func() error { resyncs++; return nil }}),
		On:  "await api.order.place({symbol:'btc_krw', side:'buy', orderType:'limit', price:'100', qty:'1', clientOrderId:'cid-1'})",
	}, dataEvent(`{}`))
	if err != nil {
		t.Fatalf("place after resync failed: %v", err)
	}
	if resyncs != 1 {
		t.Fatalf("expected exactly one resync, got %d", resyncs)
	}
	if got := len(ft.calls("POST", "/v2/orders")); got != 2 {
		t.Fatalf("expected the corrected resend, got %d sends", got)
	}
}

// Timers (setTimeout/setInterval/clearTimeout/clearInterval) are supported
// surface: goja_nodejs/eventloop registers them and we keep them deliberately
// (heartbeats, periodic repricing). Callbacks run on the loop between jobs.

func TestSetTimeoutInInitMutatesSharedState(t *testing.T) {
	// Timers are supported surface in --init. The deterministic guarantee is
	// narrow: New runs --init to completion, and an async --init only returns
	// once its awaited promise settles. So state written *inside the callback of
	// a timer the init awaits* is reliably visible to a later --where. A bare,
	// un-awaited setTimeout in --init is NOT guaranteed to have fired before the
	// first event — each timer is a separate Go timer racing onto the loop's job
	// channel, and the awaited promise can settle (and init return) first. Await
	// the timer if you need its effect before streaming.
	r := newTestRuntime(t, Options{
		Init: `globalThis.armed = false;
			await new Promise(function (res) {
				setTimeout(function () { armed = true; res(); }, 0);
			});`,
		Where: "armed === true",
	})
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("awaited timer's state not visible to --where: ok=%v err=%v", ok, err)
	}
}

func TestSetIntervalDuringStreamingClosesCleanly(t *testing.T) {
	// A setInterval running during streaming must not break handler execution,
	// and the runtime must still tear down cleanly (no leaked goroutine, no
	// panic — run under -race). The interval is cleared by the handler.
	r := newTestRuntime(t, Options{
		Init: "globalThis.ticks = 0; globalThis.h = setInterval(function () { ticks++ }, 1)",
		On:   "clearInterval(h); 1",
	})
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("handler failed alongside a running interval: %v", err)
	}
	// A second handler still runs after the interval was cleared.
	if err := r.RunHandler(dataEvent(`{}`)); err != nil {
		t.Fatalf("second handler failed: %v", err)
	}
	// t.Cleanup runs r.Close — the test passing under -race is the leak check.
}
