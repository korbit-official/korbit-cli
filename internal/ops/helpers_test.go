// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

// fakeClient is a scripted Doer (satisfying both ops.Doer and rawapi.Doer): it
// returns per-"METHOD path" steps (the last step repeats) and records every
// call with the policy it was handed, so a test can assert how many sends each
// operation made and under which policy.
type fakeClient struct {
	mu    sync.Mutex
	calls []recorded
	steps map[string][]step
}

type recorded struct {
	call apiclient.Call
	pol  apiclient.Policy
}

type step struct {
	data json.RawMessage
	err  error
}

func newFakeClient() *fakeClient { return &fakeClient{steps: map[string][]step{}} }

func (f *fakeClient) on(method, path string, s ...step) {
	f.steps[method+" "+path] = s
}

func (f *fakeClient) Do(_ context.Context, call apiclient.Call, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recorded{call, pol})
	key := call.Method + " " + call.Path
	queue := f.steps[key]
	if len(queue) == 0 {
		return nil, apiclient.Meta{Attempts: 1}, fmt.Errorf("unexpected call %s", key)
	}
	s := queue[0]
	if len(queue) > 1 {
		f.steps[key] = queue[1:]
	}
	return s.data, apiclient.Meta{Attempts: 1, RecordID: 0}, s.err
}

func (f *fakeClient) countOf(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.call.Method == method && c.call.Path == path {
			n++
		}
	}
	return n
}

func (f *fakeClient) callsOf(method, path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, c := range f.calls {
		if c.call.Method == method && c.call.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func apiOver(f *fakeClient, extra apiExtra) *API {
	return &API{
		Raw:           rawapi.New(f, nil),
		Resync:        extra.Resync,
		ServerNow:     func() int64 { return 1_750_000_000_000 },
		Sleep:         func(time.Duration) {},
		RetryBudgetMs: 5000,
		Journal:       journalFor(extra.OrderJournal),
	}
}

type apiExtra struct {
	OrderJournal orderJournalFunc
	Resync       func() error
}

// orderJournalFunc is the test seam for the place protocol's order-row hook: a
// factory that returns an OrderFinishFunc (mirroring the production OpHandle's
// StartOrder), so the tests can assert the journaled outcome without SQLite.
type orderJournalFunc func(params map[string]string, paramsJSON string) (OrderFinishFunc, error)

// journalFor wraps an orderJournalFunc as a minimal OpJournal/OpHandle so tests
// drive the operations layer through the real Journal seam. A nil factory makes
// StartOrder a no-op (operations with no order intent still Begin cleanly).
func journalFor(oj orderJournalFunc) OpJournal {
	return fakeOpJournal{oj: oj}
}

type fakeOpJournal struct{ oj orderJournalFunc }

func (f fakeOpJournal) Begin(OpStart) (OpHandle, error) { return &fakeOpHandle{oj: f.oj}, nil }

type fakeOpHandle struct{ oj orderJournalFunc }

func (h *fakeOpHandle) OperationID() int64                { return 0 }
func (h *fakeOpHandle) ForCall(string) apiclient.Recorder { return nil }
func (h *fakeOpHandle) StartOrder(in OrderIntent) (OrderFinishFunc, error) {
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

const placedOrderDoc = `{"orderId":987,"clientOrderId":"cid-1","status":"filled","price":"100","qty":"1"}`

func apiErr(status int, code string) error {
	return &output.ApiError{Message: code, HTTPStatus: status, Code: code}
}

func errStr(s string) error { return fmt.Errorf("%s", s) }

func m(pairs ...string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = pairs[i+1]
	}
	return out
}

func cidOf(params []apiclient.KV) string { return cidByKey(params, "clientOrderId") }

func cidByKey(params []apiclient.KV, key string) string {
	for _, kv := range params {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// candleRows builds a `count`-row array whose timestamp counts DOWN from hi.
func candleRows(hi int64, count int) json.RawMessage {
	rows := make([]json.RawMessage, count)
	for i := 0; i < count; i++ {
		ts := hi - int64(i)
		rows[i] = json.RawMessage(fmt.Sprintf(`{"timestamp":%d,"close":"1"}`, ts))
	}
	return joinRows(rows)
}

// finRec captures one OrderFinishFunc call so a test can assert the journaled
// outcome.
type finRec struct{ status, orderID, code string }

func captureFinishes(out *[]finRec) orderJournalFunc {
	return func(map[string]string, string) (OrderFinishFunc, error) {
		return func(status, orderID, code string, _ int) {
			*out = append(*out, finRec{status, orderID, code})
		}, nil
	}
}
