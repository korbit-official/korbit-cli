// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package callrec

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
)

// TestDefaultPolicyTable pins the per-surface policy across the read/write class
// and debug — the single behavioral contract this package centralizes.
func TestDefaultPolicyTable(t *testing.T) {
	type want struct {
		record bool
		mode   FailMode
	}
	cases := []struct {
		surface string
		write   bool
		debug   bool
		want    want
	}{
		// cli: writes always record; reads only in debug; post-failure Fail.
		{"cli", true, false, want{true, Fail}},
		{"cli", true, true, want{true, Fail}},
		{"cli", false, false, want{false, Fail}},
		{"cli", false, true, want{true, Fail}},
		// doctor: never records, in any combination.
		{"doctor", true, false, want{false, Fail}},
		{"doctor", true, true, want{false, Fail}},
		{"doctor", false, false, want{false, Fail}},
		{"doctor", false, true, want{false, Fail}},
		// stream-backfill: recovery reads are never recorded, in any combination
		// (auth account snapshots and public reads alike, even under --debug).
		{"stream-backfill", true, false, want{false, Fail}},
		{"stream-backfill", true, true, want{false, Fail}},
		{"stream-backfill", false, false, want{false, Fail}},
		{"stream-backfill", false, true, want{false, Fail}},
		// other surfaces (e.g. monitor): same record rule as cli, but Warn.
		{"monitor", true, false, want{true, Warn}},
		{"monitor", false, false, want{false, Warn}},
		{"monitor", false, true, want{true, Warn}},
		{"", true, false, want{true, Warn}},
	}
	for _, c := range cases {
		pol := DefaultPolicy(c.debug)
		info := apiclient.CallInfo{Origin: apiclient.Origin{Surface: c.surface}, Auth: true}
		if c.write {
			info.Safety = cmdmeta.SafetyNonIdempotent
		} else {
			info.Safety = cmdmeta.SafetyReadOnly
		}
		d := pol(info)
		if d.Record != c.want.record {
			t.Errorf("surface=%q write=%v debug=%v: Record=%v, want %v", c.surface, c.write, c.debug, d.Record, c.want.record)
		}
		// PostFailure is only meaningful when recording, but assert it anyway so the
		// doctor "never" row stays explicit.
		if d.Record && d.PostFailure != c.want.mode {
			t.Errorf("surface=%q write=%v debug=%v: PostFailure=%v, want %v", c.surface, c.write, c.debug, d.PostFailure, c.want.mode)
		}
	}
}

// authInfo is an authenticated write call — recorded by default (POST classifies
// it as a write for the ad-hoc path; operation tests carry Safety via OpStart).
func authInfo(surface string) apiclient.CallInfo {
	return apiclient.CallInfo{Origin: apiclient.Origin{Surface: surface}, Method: "POST", Path: "/v2/orders", Auth: true}
}

// TestLazyOpenPublicNoDebugLeavesNoDB: a call the policy declines to record must
// not even create the DB file (preserving "pure public use never touches a
// read-only home").
func TestLazyOpenPublicNoDebugLeavesNoDB(t *testing.T) {
	home := t.TempDir()
	path := journal.DefaultPath(home)
	r := New(path, false, false, DefaultPolicy(false), nil, nil)
	defer r.Close()

	cr := r.ForCall("")
	info := apiclient.CallInfo{Origin: apiclient.Origin{Surface: "cli"}, Method: "GET", Path: "/v2/tickers", Auth: false}
	if err := cr.Ready(info); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	cr.Record(info, apiclient.Outcome{HTTPStatus: 200})
	if r.Opened() {
		t.Fatal("recorder opened the journal for a non-recorded call")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("non-recorded call must not create the DB (err=%v)", err)
	}
}

// TestReadyFailsOnlyWhenRecording: a read-only home makes Ready fail for a
// recorded (auth) call — the pre-send hard guarantee — but is silent for a
// non-recorded one.
func TestReadyFailsOnlyWhenRecording(t *testing.T) {
	home := t.TempDir()
	// Occupy the DB path with a directory so journal.Open fails.
	if err := os.Mkdir(journal.DefaultPath(home), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New(journal.DefaultPath(home), false, false, DefaultPolicy(false), nil, nil)
	defer r.Close()

	// A non-recorded public call: Ready opens nothing, so no error.
	pub := apiclient.CallInfo{Origin: apiclient.Origin{Surface: "cli"}, Method: "GET", Path: "/v2/tickers", Auth: false}
	if err := r.ForCall("").Ready(pub); err != nil {
		t.Fatalf("public Ready should not open or fail: %v", err)
	}
	// A recorded auth call: Ready must fail with the config-error open text.
	err := r.ForCall("").Ready(authInfo("cli"))
	if err == nil {
		t.Fatal("recorded call Ready should fail on a broken journal")
	}
	var ce *output.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("Ready error should be a ConfigError, got %T", err)
	}
	if got := err.Error(); !contains(got, "cannot open the action journal") || !contains(got, "DIGITALX_CLI_NO_JOURNAL=1") {
		t.Fatalf("open error text changed: %q", got)
	}
}

// TestNoJournalShortCircuits: DIGITALX_CLI_NO_JOURNAL (disabled=true) declines to
// record even an auth call, and never opens the DB — even on a broken path.
func TestNoJournalShortCircuits(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(journal.DefaultPath(home), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New(journal.DefaultPath(home), true /*disabled*/, false, DefaultPolicy(true), nil, nil)
	defer r.Close()

	info := authInfo("cli")
	cr := r.ForCall("")
	if err := cr.Ready(info); err != nil {
		t.Fatalf("disabled Ready must not open or fail: %v", err)
	}
	if id := cr.Record(info, apiclient.Outcome{Err: nil, HTTPStatus: 200}); id != 0 {
		t.Fatalf("disabled Record = %d, want 0", id)
	}
	if r.Opened() {
		t.Fatal("disabled recorder must never open the DB")
	}
	// The order seam short-circuits the same way: no row, no open, and the
	// matching FinishOrder no-ops (the path is a directory, so any open attempt
	// would error loudly instead).
	rowID, err := r.StartOrder(journal.OrderStart{ClientOrderID: "x"})
	if err != nil || rowID != 0 {
		t.Fatalf("disabled StartOrder = (%d,%v), want (0,nil)", rowID, err)
	}
	if err := r.FinishOrder(rowID, journal.OrderFinish{Status: "accepted"}); err != nil {
		t.Fatalf("disabled FinishOrder must no-op: %v", err)
	}
	if r.Opened() {
		t.Fatal("disabled StartOrder/FinishOrder must never open the DB")
	}
}

// TestPostFailureModes: a post-call Record write failure is never returned — it
// is delivered to the onPostFailure sink with the call's FailMode and the raw
// journal error, for BOTH modes. (The cli's sink, not this layer, decides
// warn-vs-fatal from the mode.)
func TestPostFailureModes(t *testing.T) {
	apiErr := &output.ApiError{HTTPStatus: 500, Code: "X"}
	out := apiclient.Outcome{Err: apiErr, HTTPStatus: 500, Code: "X", Message: "boom", Attempts: 1}

	for _, mode := range []FailMode{Fail, Warn} {
		home := t.TempDir()
		var gotModes []FailMode
		var gotErrs []error
		r := New(journal.DefaultPath(home), false, false, func(apiclient.CallInfo) Decision {
			return Decision{Record: true, PostFailure: mode}
		}, nil, captureFailures(&gotModes, &gotErrs))
		// Open the DB normally so Ready succeeds, then close the underlying handle
		// so the LogCall write fails — simulating a post-call journal break.
		info := authInfo("cli")
		cr := r.ForCall("")
		if err := cr.Ready(info); err != nil {
			t.Fatalf("Ready: %v", err)
		}
		_ = r.jl.Close() // force the next write to fail

		cr.Record(info, out) // never returns an error; delivers to the sink
		if len(gotModes) != 1 || gotModes[0] != mode {
			t.Fatalf("sink modes = %v, want one %v", gotModes, mode)
		}
		if len(gotErrs) != 1 || gotErrs[0] == nil {
			t.Fatalf("sink should receive the raw journal error once, got %v", gotErrs)
		}
		r.Close()
	}
}

// TestRecordParityFields checks the api_calls row field mapping: success leaves
// http_status NULL, an API error stores it, a transport failure leaves it NULL
// with the message, and retries = attempts-1.
func TestRecordParityFields(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		// out carries the client's (real-clock) times; they must NOT be stored —
		// the row's timing comes from the recorder's injectable clock (see below).
		row := recordOne(t, apiclient.Outcome{Err: nil, HTTPStatus: 200, Attempts: 1, StartedAtMs: 999, FinishedAtMs: 999, DurationMs: 999})
		if !row.Success || row.HTTPStatus != nil {
			t.Fatalf("success row: success=%v httpStatus=%v (want true, nil)", row.Success, row.HTTPStatus)
		}
		// recordOne's clock steps 1000 -> 1100 across Ready -> Record, so the row
		// must read started=1000, finished=1100, duration=100 — proving the
		// injectable clock (not the client's Outcome times) drives these columns.
		if row.StartedAtMs != 1000 || row.FinishedAtMs == nil || *row.FinishedAtMs != 1100 {
			t.Fatalf("success row timing not from injectable clock: %+v", row)
		}
		if row.RetryCount != 0 || row.DurationMs == nil || *row.DurationMs != 100 {
			t.Fatalf("success row duration wrong: %+v", row)
		}
	})
	t.Run("apiError", func(t *testing.T) {
		ae := &output.ApiError{HTTPStatus: 422, Code: "DUPLICATE", Message: "dup"}
		row := recordOne(t, apiclient.Outcome{Err: ae, HTTPStatus: 422, Code: "DUPLICATE", Message: "dup", Attempts: 2})
		if row.Success || row.HTTPStatus == nil || *row.HTTPStatus != 422 {
			t.Fatalf("apiError row: %+v", row)
		}
		if row.ErrorCode != "DUPLICATE" || row.RetryCount != 1 {
			t.Fatalf("apiError fields: %+v", row)
		}
	})
	t.Run("transport", func(t *testing.T) {
		row := recordOne(t, apiclient.Outcome{Err: errors.New("dial tcp: refused"), Message: "dial tcp: refused", Attempts: 1})
		if row.Success || row.HTTPStatus != nil {
			t.Fatalf("transport row should have NULL http_status: %+v", row)
		}
		if row.ErrorMessage != "dial tcp: refused" || row.ErrorCode != "" {
			t.Fatalf("transport fields: %+v", row)
		}
	})
}

// TestForCallOverridesParamsJSON: the CLI's spec/insertion-ordered params JSON,
// passed via ForCall, wins over the client's map-sorted CallInfo.ParamsJSON.
func TestForCallOverridesParamsJSON(t *testing.T) {
	home := t.TempDir()
	r := New(journal.DefaultPath(home), false, false, DefaultPolicy(false), nil, nil)
	defer r.Close()
	info := authInfo("cli")
	info.ParamsJSON = []byte(`{"clientOrderId":"x","side":"buy","symbol":"btc_krw"}`) // sorted
	ordered := `{"symbol":"btc_krw","side":"buy","clientOrderId":"x"}`
	cr := r.ForCall(ordered)
	if err := cr.Ready(info); err != nil {
		t.Fatal(err)
	}
	cr.Record(info, apiclient.Outcome{HTTPStatus: 200, Attempts: 1})
	rows, _ := r.jl.RecentCalls(1)
	if len(rows) != 1 || string(rows[0].Params) != ordered {
		t.Fatalf("params_json = %s, want %s", rows[0].Params, ordered)
	}
}

// TestConcurrentSameCommandIsolated: two concurrent recorded calls of the SAME
// command through ONE shared parent recorder must not clobber each other's
// per-call state (start time, ordered params). This is the monitor surface's
// concurrent api.order.place scenario; ForCall gives each call its own state.
func TestConcurrentSameCommandIsolated(t *testing.T) {
	home := t.TempDir()
	r := New(journal.DefaultPath(home), false, false, DefaultPolicy(false), nil, nil)
	defer r.Close()

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info := authInfo("cli")
			ordered := `{"clientOrderId":"coid-` + itoa(i) + `"}`
			cr := r.ForCall(ordered)
			if err := cr.Ready(info); err != nil {
				t.Errorf("Ready: %v", err)
				return
			}
			cr.Record(info, apiclient.Outcome{HTTPStatus: 200, Attempts: 1})
		}(i)
	}
	wg.Wait()

	rows, err := r.jl.RecentCalls(n)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("want %d rows, got %d", n, len(rows))
	}
	// Every per-call ordered params JSON must appear exactly once — no clobber.
	seen := map[string]bool{}
	for _, row := range rows {
		p := string(row.Params)
		if seen[p] {
			t.Fatalf("duplicate/clobbered params_json: %s", p)
		}
		seen[p] = true
		// Each row's timing must be self-consistent (finished >= started).
		if row.FinishedAtMs == nil || *row.FinishedAtMs < row.StartedAtMs {
			t.Fatalf("inconsistent timing on a row: %+v", row)
		}
	}
	if len(seen) != n {
		t.Fatalf("want %d distinct params_json, got %d", n, len(seen))
	}
}

// TestBeginGroupsCallsUnderOperation: a recorded operation opens one operations
// row, each ForCall lands an api_calls row carrying that operation_id and a
// 1-based seq, and Finish folds the outcome into the operations row.
func TestBeginGroupsCallsUnderOperation(t *testing.T) {
	home := t.TempDir()
	r := New(journal.DefaultPath(home), false, false, DefaultPolicy(false), nil, nil)
	defer r.Close()

	h, err := r.Begin(ops.OpStart{OpID: "order.place", Surface: "cli", Safety: cmdmeta.SafetyNonIdempotent, Auth: true, KeyName: "bot", APIKeyID: "KEYID-1"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	opID := h.OperationID()
	if opID == 0 {
		t.Fatal("recording operation should have a non-zero id")
	}
	info := authInfo("cli")
	for i := 0; i < 3; i++ {
		cr := h.ForCall("")
		if err := cr.Ready(info); err != nil {
			t.Fatalf("Ready %d: %v", i, err)
		}
		cr.Record(info, apiclient.Outcome{HTTPStatus: 200, Attempts: 1})
	}
	if err := h.Finish("ok", ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	calls, err := r.jl.RecentCalls(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("want 3 api_calls, got %d", len(calls))
	}
	seqs := map[int]bool{}
	for _, c := range calls {
		if c.OperationID == nil || *c.OperationID != opID {
			t.Fatalf("call not linked to the operation: %+v", c)
		}
		seqs[c.Seq] = true
	}
	for _, want := range []int{1, 2, 3} {
		if !seqs[want] {
			t.Fatalf("missing seq %d (got %v)", want, seqs)
		}
	}
	operations, err := r.jl.RecentOperations(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Outcome != "ok" || operations[0].Attempts != 3 {
		t.Fatalf("operation row not finished as expected: %+v", operations)
	}
	if operations[0].OpID != "order.place" || operations[0].Surface != "cli" {
		t.Fatalf("operation metadata: %+v", operations[0])
	}
}

// TestJournalRowsStampedFromInjectableClock pins that EVERY journal time column —
// operations started/finished AND orders created/finished — is stamped from the
// recorder's own injectable (system) clock, not from any caller-supplied value or
// server-clock estimate. ops passes no timestamp (the OpStart/OrderIntent/Finish
// seams carry none), and callrec stamps each column at its own moment, so a clock
// that steps 1000ms per read yields four distinct, ordered ticks in row order
// Begin(1000) -> StartOrder(2000) -> order-finish(3000) -> Finish(4000).
func TestJournalRowsStampedFromInjectableClock(t *testing.T) {
	home := t.TempDir()
	r := New(journal.DefaultPath(home), false, false, DefaultPolicy(false), steppedClock(1000, 1000), nil)
	defer r.Close()

	h, err := r.Begin(ops.OpStart{OpID: "order.place", Surface: "cli", Safety: cmdmeta.SafetyNonIdempotent, Auth: true})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	finish, err := h.StartOrder(ops.OrderIntent{ClientOrderID: "coid-1", Symbol: "btc_krw", Side: "buy"})
	if err != nil {
		t.Fatalf("StartOrder: %v", err)
	}
	finish("accepted", "9999", "", 1)
	if err := h.Finish("ok", ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	opRows, err := r.jl.RecentOperations(1)
	if err != nil || len(opRows) != 1 {
		t.Fatalf("RecentOperations: %v (%d rows)", err, len(opRows))
	}
	if op := opRows[0]; op.StartedAtMs != 1000 || op.FinishedAtMs == nil || *op.FinishedAtMs != 4000 {
		t.Fatalf("operation timing not from the injectable clock: started=%d finished=%v (want 1000, 4000)", op.StartedAtMs, op.FinishedAtMs)
	}
	ordRows, err := r.jl.RecentOrders(1)
	if err != nil || len(ordRows) != 1 {
		t.Fatalf("RecentOrders: %v (%d rows)", err, len(ordRows))
	}
	if ord := ordRows[0]; ord.CreatedAtMs != 2000 || ord.FinishedAtMs == nil || *ord.FinishedAtMs != 3000 {
		t.Fatalf("order timing not from the injectable clock: created=%d finished=%v (want 2000, 3000)", ord.CreatedAtMs, ord.FinishedAtMs)
	}
}

// TestBeginNonRecordingOpensNothing: a read (no-debug) operation declines to
// record, so Begin returns a no-op handle that opens no DB and whose ForCall
// recorder lets the send proceed un-journaled.
func TestBeginNonRecordingOpensNothing(t *testing.T) {
	home := t.TempDir()
	path := journal.DefaultPath(home)
	r := New(path, false, false, DefaultPolicy(false), nil, nil)
	defer r.Close()

	h, err := r.Begin(ops.OpStart{OpID: "ticker", Surface: "cli", Safety: cmdmeta.SafetyReadOnly, Auth: false})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if h.OperationID() != 0 {
		t.Fatal("non-recording operation must have id 0")
	}
	// A non-recording handle hands out no recorder (nil), the wire client's
	// "do not record" sentinel, so the send proceeds un-journaled.
	if rec := h.ForCall(""); rec != nil {
		t.Fatalf("a non-recording handle must hand out no recorder, got %T", rec)
	}
	if err := h.Finish("ok", ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if r.Opened() {
		t.Fatal("a non-recording operation must not open the DB")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("non-recording operation must not create the DB (err=%v)", err)
	}
}

// TestBeginHardGateFailsPreSend: a broken journal makes Begin fail for a
// recorded operation — the operation's pre-send hard guarantee — with the
// config-error open text.
func TestBeginHardGateFailsPreSend(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(journal.DefaultPath(home), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New(journal.DefaultPath(home), false, false, DefaultPolicy(false), nil, nil)
	defer r.Close()

	_, err := r.Begin(ops.OpStart{OpID: "order.place", Surface: "cli", Safety: cmdmeta.SafetyNonIdempotent, Auth: true})
	if err == nil {
		t.Fatal("Begin should fail on a broken journal for a recorded operation")
	}
	var ce *output.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("Begin error should be a ConfigError, got %T", err)
	}
	if got := err.Error(); !contains(got, "cannot open the action journal") || !contains(got, "DIGITALX_CLI_NO_JOURNAL=1") {
		t.Fatalf("open error text changed: %q", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// recordOne opens a fresh recorder, records one auth call, and returns the row.
// The injectable clock steps 1000 -> 1100 across the Ready -> Record pair, so
// timing-column assertions are deterministic.
// steppedClock returns a clock that yields start on its first read and advances
// by step on every subsequent read, so a test can pin which moment stamped each
// row (distinct, ordered ticks).
func steppedClock(start, step int64) func() int64 {
	n := start - step
	return func() int64 { n += step; return n }
}

func recordOne(t *testing.T, out apiclient.Outcome) journal.CallRow {
	t.Helper()
	home := t.TempDir()
	ticks := []int64{1000, 1100}
	i := 0
	clock := func() int64 {
		v := ticks[i]
		if i < len(ticks)-1 {
			i++
		}
		return v
	}
	r := New(journal.DefaultPath(home), false, false, DefaultPolicy(false), clock, nil)
	t.Cleanup(func() { r.Close() })
	info := authInfo("cli")
	info.KeyName, info.APIKeyID = "bot", "KEYID-1"
	cr := r.ForCall("")
	if err := cr.Ready(info); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	cr.Record(info, out)
	rows, err := r.jl.RecentCalls(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("RecentCalls: %v (%d rows)", err, len(rows))
	}
	return rows[0]
}

// captureFailures returns an onPostFailure sink that appends each call's mode and
// error to the given slices (either may be nil).
func captureFailures(modes *[]FailMode, errs *[]error) func(FailMode, error) {
	return func(m FailMode, e error) {
		if modes != nil {
			*modes = append(*modes, m)
		}
		if errs != nil {
			*errs = append(*errs, e)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestRecorderCloseRaceWithLateCalls drives the TUI-shutdown window: a recorder
// touched by a still-running command goroutine while Close() runs on another. It
// covers the three jl reads that must be race-safe — CallRecorder.Record, an
// operation's per-call Record, and the order start/finish path — concurrent with
// Close(). Under `go test -race` it asserts no data race on the jl handle, no
// nil-pointer panic, and that each late call either lands or no-ops cleanly.
func TestRecorderCloseRaceWithLateCalls(t *testing.T) {
	info := authInfo("tui") // a write => always recording
	for i := 0; i < 50; i++ {
		r := New(journal.DefaultPath(t.TempDir()), false, false, DefaultPolicy(false), nil, nil)
		// Open the journal up front so Close has a live handle to race against.
		if err := r.ForCall("").Ready(info); err != nil {
			t.Fatalf("Ready: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(4)
		// Ad-hoc per-call write.
		go func() {
			defer wg.Done()
			cr := r.ForCall("")
			_ = cr.Ready(info)
			cr.Record(info, apiclient.Outcome{HTTPStatus: 200})
		}()
		// Order intent + finish (pre-send hard-guarantee path, may fail cleanly).
		go func() {
			defer wg.Done()
			h, err := r.Begin(ops.OpStart{OpID: "order.place", Surface: "tui", Safety: cmdmeta.SafetyNonIdempotent, Auth: true})
			if err != nil || h == nil {
				return
			}
			fin, err := h.StartOrder(ops.OrderIntent{ClientOrderID: "x", Symbol: "btc_krw", Side: "buy", OrderType: "limit"})
			if err != nil {
				return // journal raced shut before the row; refused, as designed
			}
			fin("filled", "1", "", 1)
			_ = h.Finish("ok", "")
		}()
		// Operation per-call write (the candle path).
		go func() {
			defer wg.Done()
			h, err := r.Begin(ops.OpStart{OpID: "candles", Surface: "tui", Safety: cmdmeta.SafetyIdempotent, Auth: true})
			if err != nil || h == nil {
				return
			}
			cr := h.ForCall("")
			if cr == nil {
				return
			}
			_ = cr.Ready(info)
			cr.Record(info, apiclient.Outcome{HTTPStatus: 200})
		}()
		// The shutdown close.
		go func() {
			defer wg.Done()
			_ = r.Close()
		}()
		wg.Wait()
		// No trailing Close() cleanup: the terminal closed flag means a late call
		// can't have reopened the journal, so there is no second handle to release
		// (TestRecorderNoReopenAfterClose pins that directly).
	}
}

// TestRecorderNoReopenAfterClose pins the terminal-closed contract: once Close
// has run, no late open-then-write call (a leaked TUI command goroutine) reopens
// the journal. Crucially, the behavior splits by guarantee — pre-send
// would-record writes (Begin/StartOrder) REFUSE so the operation aborts before
// any send (the hard guarantee), while post-send diagnostic writes (Record)
// cleanly no-op. None reopens a fresh, unclosed handle.
func TestRecorderNoReopenAfterClose(t *testing.T) {
	info := authInfo("tui")
	r := New(journal.DefaultPath(t.TempDir()), false, false, DefaultPolicy(false), nil, nil)
	cr := r.ForCall("")
	if err := cr.Ready(info); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	cr.Record(info, apiclient.Outcome{HTTPStatus: 200})
	if !r.Opened() {
		t.Fatal("expected the journal open after a recorded call")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.Opened() {
		t.Fatal("expected the journal closed after Close")
	}

	// Post-send Record must no-op, not reopen.
	late := r.ForCall("")
	_ = late.Ready(info)
	if id := late.Record(info, apiclient.Outcome{HTTPStatus: 200}); id != 0 {
		t.Fatalf("late Record after Close should no-op, got id %d", id)
	}
	if r.Opened() {
		t.Fatal("Record after Close must not reopen the journal")
	}

	// Pre-send would-record Begin (a money op) must REFUSE, not return a
	// non-recording handle — else the placement would send with no ledger row.
	h, err := r.Begin(ops.OpStart{OpID: "order.place", Surface: "tui", Safety: cmdmeta.SafetyNonIdempotent, Auth: true})
	if err == nil {
		t.Fatalf("Begin after Close for a would-record op must refuse, got handle %+v", h)
	}
	if r.Opened() {
		t.Fatal("Begin after Close must not reopen the journal")
	}

	// Pre-send StartOrder (the standalone intent write) must REFUSE too.
	if _, err := r.StartOrder(journal.OrderStart{ClientOrderID: "x"}); err == nil {
		t.Fatal("StartOrder after Close must refuse, not silently skip the row")
	}
	if r.Opened() {
		t.Fatal("StartOrder after Close must not reopen the journal")
	}
}
