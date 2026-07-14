// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Logger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "korbit-cli.db"), false, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestOpenEnablesWAL(t *testing.T) {
	l := openTemp(t)
	var mode string
	if err := l.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var busy int
	if err := l.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busy)
	}
}

func TestOpenNoFsync(t *testing.T) {
	// noFsync=false keeps the SQLite default (FULL=2); noFsync=true sets OFF=0.
	for _, tc := range []struct {
		noFsync bool
		want    int
	}{{false, 2}, {true, 0}} {
		l, err := Open(filepath.Join(t.TempDir(), "korbit-cli.db"), tc.noFsync, nil)
		if err != nil {
			t.Fatalf("Open(noFsync=%v): %v", tc.noFsync, err)
		}
		var sync int
		if err := l.db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatalf("query synchronous: %v", err)
		}
		l.Close()
		if sync != tc.want {
			t.Fatalf("noFsync=%v: synchronous = %d, want %d", tc.noFsync, sync, tc.want)
		}
	}
}

func TestFreshDBStampsCurrentVersion(t *testing.T) {
	l := openTemp(t)
	var v int
	if err := l.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("query user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

// TestRecreateOnVersionMismatch: an old-stamped DB with an old-shaped table is
// reset (dropped + recreated) on open rather than failing — the journal is a
// recreatable log, not a system of record.
func TestRecreateOnVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "korbit-cli.db")
	// Hand-build a v1-shaped database: a stale api_calls table with the old
	// command_key column, stamped user_version=1.
	old, err := Open(path, false, nil) // creates v2; we then forcibly downgrade it below
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := old.db.Exec(`DROP TABLE orders; DROP TABLE api_calls; DROP TABLE operations;
CREATE TABLE api_calls (id INTEGER PRIMARY KEY, command_key TEXT);
PRAGMA user_version=1;`); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	old.Close()

	l, err := Open(path, false, nil)
	if err != nil {
		t.Fatalf("reopen stale DB should reset, not fail: %v", err)
	}
	defer l.Close()
	var v int
	if err := l.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("query user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version after reset = %d, want %d", v, schemaVersion)
	}
	// The new-shaped api_calls (with operation_id) must be usable.
	if _, err := l.LogCall(CallRecord{StartedAtMs: 1, Method: "GET", Path: "/v2/tickers", BaseURL: "x", Success: true}); err != nil {
		t.Fatalf("write after reset: %v", err)
	}
}

func TestStartFinishOperation(t *testing.T) {
	l := openTemp(t)
	id, err := l.StartOperation(OperationStart{
		StartedAtMs: 1000, OpID: "order.place", Surface: "cli", Safety: "non-idempotent",
		KeyName: "bot", APIKeyID: "KEYID-1", CLIVersion: "9.9.9",
	})
	if err != nil || id == 0 {
		t.Fatalf("StartOperation: id=%d err=%v", id, err)
	}
	ops, err := l.RecentOperations(10)
	if err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	if len(ops) != 1 || ops[0].Outcome != "running" || ops[0].OpID != "order.place" {
		t.Fatalf("running operation row: %+v", ops)
	}
	if err := l.FinishOperation(id, OperationFinish{FinishedAtMs: 1200, Outcome: "ok", Attempts: 2}); err != nil {
		t.Fatalf("FinishOperation: %v", err)
	}
	ops, _ = l.RecentOperations(10)
	o := ops[0]
	if o.Outcome != "ok" || o.Attempts != 2 || o.FinishedAtMs == nil || *o.FinishedAtMs != 1200 {
		t.Fatalf("finished operation row: %+v", o)
	}
	if o.Surface != "cli" || o.Safety != "non-idempotent" {
		t.Fatalf("operation metadata: %+v", o)
	}
}

func TestLogCallRoundTrip(t *testing.T) {
	l := openTemp(t)
	opID, err := l.StartOperation(OperationStart{StartedAtMs: 900, OpID: "order.place", Surface: "cli"})
	if err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	status := 422
	id, err := l.LogCall(CallRecord{
		OperationID: opID, Seq: 1,
		StartedAtMs: 1000, FinishedAtMs: 1200,
		Method: "POST", Path: "/v2/orders", BaseURL: "https://api.korbit.co.kr",
		Auth: true, KeyName: "bot", APIKeyID: "KEYID-1", ParamsJSON: `{"symbol":"btc_krw"}`,
		HTTPStatus: &status, Success: false, ErrorCode: "DUPLICATE_CLIENT_ORDER_ID",
		ErrorMessage: "already placed", RetryCount: 2, DurationMs: 200, CLIVersion: "9.9.9",
	})
	if err != nil || id == 0 {
		t.Fatalf("LogCall: id=%d err=%v", id, err)
	}
	rows, err := l.RecentCalls(10)
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Method != "POST" || r.Path != "/v2/orders" || !r.Auth || r.Success {
		t.Fatalf("unexpected row: %+v", r)
	}
	if r.OperationID == nil || *r.OperationID != opID || r.Seq != 1 {
		t.Fatalf("operation link/seq not recorded: %+v", r)
	}
	if r.ErrorCode != "DUPLICATE_CLIENT_ORDER_ID" || r.RetryCount != 2 {
		t.Fatalf("error/retry not recorded: %+v", r)
	}
	if r.HTTPStatus == nil || *r.HTTPStatus != 422 {
		t.Fatalf("http status not recorded: %+v", r)
	}
	if string(r.Params) != `{"symbol":"btc_krw"}` {
		t.Fatalf("params not recorded: %s", r.Params)
	}
}

func TestStartFinishOrder(t *testing.T) {
	l := openTemp(t)
	opID, err := l.StartOperation(OperationStart{StartedAtMs: 400, OpID: "order.place", Surface: "cli"})
	if err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	id, err := l.StartOrder(OrderStart{
		CreatedAtMs: 500, OperationID: opID, ClientOrderID: "coid-1",
		Symbol: "btc_krw", Side: "buy", OrderType: "limit", Price: "100", Qty: "0.001",
		ParamsJSON: `{"symbol":"btc_krw"}`, KeyName: "bot", APIKeyID: "KEYID-1",
	})
	if err != nil || id == 0 {
		t.Fatalf("StartOrder: id=%d err=%v", id, err)
	}
	if _, err := l.LogCall(CallRecord{OperationID: opID, Seq: 1, StartedAtMs: 600, Method: "POST", Path: "/v2/orders", BaseURL: "x", Success: true}); err != nil {
		t.Fatalf("LogCall: %v", err)
	}
	if err := l.FinishOrder(id, OrderFinish{
		FinishedAtMs: 700, Status: "accepted", OrderID: "9876543210", RetryCount: 1,
	}); err != nil {
		t.Fatalf("FinishOrder: %v", err)
	}
	orders, err := l.RecentOrders(10)
	if err != nil {
		t.Fatalf("RecentOrders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	o := orders[0]
	if o.Status != "accepted" || o.OrderID != "9876543210" || o.ClientOrderID != "coid-1" {
		t.Fatalf("unexpected order: %+v", o)
	}
	if o.AttemptCount != 1 || o.RetryCount != 1 {
		t.Fatalf("counts: attempt=%d retry=%d", o.AttemptCount, o.RetryCount)
	}
	if o.OperationID == nil || *o.OperationID != opID {
		t.Fatalf("operation not linked: %+v", o)
	}
}

// TestStartOrderUniqueClientOrderID is the safety pin for the UNIQUE index plus
// the idempotent-retry flow: reusing a clientOrderId upserts onto the same row
// (bumping attempt_count) instead of inserting a duplicate or erroring.
func TestStartOrderUniqueClientOrderID(t *testing.T) {
	l := openTemp(t)
	id1, err := l.StartOrder(OrderStart{CreatedAtMs: 100, ClientOrderID: "dup", Symbol: "btc_krw"})
	if err != nil {
		t.Fatalf("first StartOrder: %v", err)
	}
	id2, err := l.StartOrder(OrderStart{CreatedAtMs: 200, ClientOrderID: "dup", Symbol: "btc_krw"})
	if err != nil {
		t.Fatalf("retry StartOrder: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("reused clientOrderId should map to the same row: %d vs %d", id1, id2)
	}
	orders, err := l.RecentOrders(10)
	if err != nil {
		t.Fatalf("RecentOrders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("want a single row for the reused clientOrderId, got %d", len(orders))
	}
	if orders[0].AttemptCount != 2 {
		t.Fatalf("attempt_count should bump to 2 on reuse, got %d", orders[0].AttemptCount)
	}
}

// TestFinishOrderDoesNotDowngradeAccepted covers the reuse-after-success case:
// a clientOrderId retried after it already went through must not flip the row
// from accepted to failed, and must keep the original orderId.
func TestFinishOrderDoesNotDowngradeAccepted(t *testing.T) {
	l := openTemp(t)
	id, err := l.StartOrder(OrderStart{CreatedAtMs: 1, ClientOrderID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.FinishOrder(id, OrderFinish{FinishedAtMs: 3, Status: "accepted", OrderID: "555"}); err != nil {
		t.Fatal(err)
	}
	// Mirror the real CLI reuse path: a second StartOrder (upsert) precedes the
	// retry's FinishOrder, and must not erase the accepted state either.
	if _, err := l.StartOrder(OrderStart{CreatedAtMs: 4, ClientOrderID: "c"}); err != nil {
		t.Fatal(err)
	}
	// A later duplicate-retry comes back failed with no orderId.
	if err := l.FinishOrder(id, OrderFinish{FinishedAtMs: 5, Status: "failed", ErrorCode: "DUPLICATE_CLIENT_ORDER_ID"}); err != nil {
		t.Fatal(err)
	}
	orders, _ := l.RecentOrders(1)
	if orders[0].Status != "accepted" {
		t.Fatalf("accepted must not be downgraded, got %q", orders[0].Status)
	}
	if orders[0].OrderID != "555" {
		t.Fatalf("orderId must be preserved, got %q", orders[0].OrderID)
	}
}

// TestFinishOrderDoesNotDowngradeAcceptedToUnknown: an accepted order whose
// clientOrderId is later retried in an attempt that can only be resolved as
// UNKNOWN (ambiguous send + unreadable lookup) must keep its accepted status and
// orderId — accepted is positive proof and outranks a can't-confirm result.
func TestFinishOrderDoesNotDowngradeAcceptedToUnknown(t *testing.T) {
	l := openTemp(t)
	id, err := l.StartOrder(OrderStart{CreatedAtMs: 1, ClientOrderID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.FinishOrder(id, OrderFinish{FinishedAtMs: 3, Status: "accepted", OrderID: "555"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.StartOrder(OrderStart{CreatedAtMs: 4, ClientOrderID: "c"}); err != nil {
		t.Fatal(err)
	}
	// A later ambiguous retry can't confirm and resolves UNKNOWN.
	if err := l.FinishOrder(id, OrderFinish{FinishedAtMs: 5, Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	orders, _ := l.RecentOrders(1)
	if orders[0].Status != "accepted" {
		t.Fatalf("accepted must not be downgraded to unknown, got %q", orders[0].Status)
	}
	if orders[0].OrderID != "555" {
		t.Fatalf("orderId must be preserved, got %q", orders[0].OrderID)
	}
}

// TestFinishOrderUpgradesFailed is the converse: a failed-then-succeeded retry
// (e.g. fixed a balance problem) does move to accepted.
func TestFinishOrderUpgradesFailed(t *testing.T) {
	l := openTemp(t)
	id, _ := l.StartOrder(OrderStart{CreatedAtMs: 1, ClientOrderID: "c"})
	_ = l.FinishOrder(id, OrderFinish{FinishedAtMs: 3, Status: "failed", ErrorCode: "NO_BALANCE"})
	_ = l.FinishOrder(id, OrderFinish{FinishedAtMs: 4, Status: "accepted", OrderID: "777"})
	orders, _ := l.RecentOrders(1)
	if orders[0].Status != "accepted" || orders[0].OrderID != "777" {
		t.Fatalf("failed should upgrade to accepted: %+v", orders[0])
	}
	if orders[0].ErrorCode != "" {
		t.Fatalf("stale error_code should clear on upgrade, got %q", orders[0].ErrorCode)
	}
}

func TestRecentNewestFirst(t *testing.T) {
	l := openTemp(t)
	for i := 0; i < 3; i++ {
		if _, err := l.LogCall(CallRecord{StartedAtMs: int64(i), Method: "GET", Path: "/v2/ticker", BaseURL: "x", Success: true}); err != nil {
			t.Fatalf("LogCall %d: %v", i, err)
		}
	}
	rows, err := l.RecentCalls(2)
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit not honored: got %d", len(rows))
	}
	if rows[0].ID <= rows[1].ID {
		t.Fatalf("rows should be newest-first: %d then %d", rows[0].ID, rows[1].ID)
	}
}

func TestDisabled(t *testing.T) {
	get := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if Disabled(get(map[string]string{})) {
		t.Fatal("should be enabled by default")
	}
	if !Disabled(get(map[string]string{"KORBIT_CLI_NO_JOURNAL": "1"})) {
		t.Fatal("=1 should disable")
	}
	if !Disabled(get(map[string]string{"KORBIT_CLI_NO_JOURNAL": "true"})) {
		t.Fatal("=true should disable")
	}
}
