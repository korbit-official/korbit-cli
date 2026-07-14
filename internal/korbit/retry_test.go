// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/output"
)

// replayDoer returns a programmed outcome per Do call and records the requests
// it saw (so a test can assert how many attempts happened and inspect the
// signed timestamp on each).
type replayDoer struct {
	steps    []step
	call     int
	requests []*http.Request
	bodies   []string
}

type step struct {
	resp *http.Response
	err  error
}

func (d *replayDoer) Do(r *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, r)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		d.bodies = append(d.bodies, string(b))
	} else {
		d.bodies = append(d.bodies, "")
	}
	i := d.call
	d.call++
	if i >= len(d.steps) {
		return nil, errors.New("replayDoer: more calls than programmed")
	}
	return d.steps[i].resp, d.steps[i].err
}

func okResp() *http.Response { return mkResp(200, `{"success":true,"data":{"ok":1}}`, nil) }
func apiResp(status int, code string, h map[string]string) *http.Response {
	return mkResp(status, `{"success":false,"error":{"code":`+itoa(status)+`,"message":"`+code+`"}}`, h)
}
func itoa(n int) string { return strconv.Itoa(n) }

// fakeSleep records the durations it was asked to sleep (without sleeping), so a
// test can assert the backoff schedule. The budget is tracked as total sleep
// time, so no clock is needed.
type fakeSleep struct {
	slept []time.Duration
}

func (f *fakeSleep) sleep(d time.Duration) { f.slept = append(f.slept, d) }

func TestClassifyTryAgainIsTransient(t *testing.T) {
	// TRY_AGAIN is the cancel "order is being processed, retry shortly" gate. It
	// is a definitive 4xx (not a 5xx), so it must be classified by its symbolic
	// code rather than falling through to ClassFatal. The exact 4xx (400 vs 409)
	// is irrelevant — classification keys on the code, not the status.
	for _, status := range []int{400, 409} {
		err := &output.ApiError{Code: "TRY_AGAIN", HTTPStatus: status}
		if got := Classify(err); got != ClassTransient {
			t.Errorf("Classify(TRY_AGAIN @ %d) = %v, want ClassTransient", status, got)
		}
	}
}

func TestExecuteWithRetryTryAgainRetriesWhenIdempotent(t *testing.T) {
	// An idempotent cancel that first sees TRY_AGAIN (order mid-processing) then
	// succeeds must retry within budget, not surface the first rejection.
	d := &replayDoer{steps: []step{{resp: apiResp(409, "TRY_AGAIN", nil)}, {resp: okResp()}}}
	f := &fakeSleep{}
	_, err := ExecuteWithRetry(Request{Method: "DELETE", Path: "/v2/orders"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: 5000, Sleep: f.sleep})
	if err != nil {
		t.Fatalf("idempotent TRY_AGAIN should retry to success, got err=%v", err)
	}
	if d.call != 2 {
		t.Errorf("expected 2 attempts (retry after TRY_AGAIN), got %d", d.call)
	}
}

func TestExecuteWithRetryNonIdempotentIsSingleShot(t *testing.T) {
	d := &replayDoer{steps: []step{{err: errors.New("connection reset")}, {resp: okResp()}}}
	_, err := ExecuteWithRetry(Request{Method: "POST", Path: "/v2/coin/withdrawal"}, Options{Doer: d},
		RetryPolicy{Idempotent: false, BudgetMs: 5000})
	if err == nil {
		t.Fatal("expected the single attempt to surface the network error")
	}
	if d.call != 1 {
		t.Errorf("money-mover was sent %d times; must be exactly 1", d.call)
	}
}

func TestExecuteWithRetryBudgetZeroIsSingleShot(t *testing.T) {
	d := &replayDoer{steps: []step{{err: errors.New("net")}, {resp: okResp()}}}
	_, err := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: 0})
	if err == nil || d.call != 1 {
		t.Errorf("BudgetMs=0 must be single shot: err=%v calls=%d", err, d.call)
	}
}

func TestExecuteWithRetryNetworkThenSuccess(t *testing.T) {
	d := &replayDoer{steps: []step{{err: errors.New("net")}, {resp: okResp()}}}
	fs := &fakeSleep{}
	attempts := 0
	var observed []string
	_, err := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: 5000, Sleep: fs.sleep, Attempts: &attempts,
			Observe: func(m string) { observed = append(observed, m) }})
	if err != nil {
		t.Fatalf("expected success after one backoff retry: %v", err)
	}
	if d.call != 2 {
		t.Errorf("calls = %d, want 2", d.call)
	}
	if len(fs.slept) != 1 || fs.slept[0] != retryInitialBackoffMs*time.Millisecond {
		t.Errorf("slept = %v, want one %dms backoff", fs.slept, retryInitialBackoffMs)
	}
	if attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (the success isn't observed but is counted)", attempts)
	}
	// One retry happened, so Observe fired exactly once describing the backoff.
	if len(observed) != 1 || !strings.Contains(observed[0], "backing off") {
		t.Errorf("Observe = %v, want one backoff message", observed)
	}
}

func TestExecuteWithRetryBudgetExhausted(t *testing.T) {
	// All attempts fail with a network error; a tight budget must stop retrying.
	steps := make([]step, 20)
	for i := range steps {
		steps[i] = step{err: errors.New("net")}
	}
	d := &replayDoer{steps: steps}
	fs := &fakeSleep{}
	_, err := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: 250, Sleep: fs.sleep})
	if err == nil {
		t.Fatal("expected failure once the budget is exhausted")
	}
	// 100ms then 200ms would reach 300 > 250, so only the 100ms sleep fits:
	// attempt 1 (fail) -> sleep 100 -> attempt 2 (fail) -> 100+200>250 stop.
	if len(fs.slept) != 1 || fs.slept[0] != 100*time.Millisecond {
		t.Errorf("slept = %v, want a single 100ms backoff before giving up", fs.slept)
	}
	if d.call != 2 {
		t.Errorf("calls = %d, want 2", d.call)
	}
}

func TestExecuteWithRetryRateLimitHonorsRetryAfter(t *testing.T) {
	d := &replayDoer{steps: []step{
		{resp: apiResp(429, "TOO_MANY_REQUESTS", map[string]string{"Retry-After": "2"})},
		{resp: okResp()},
	}}
	fs := &fakeSleep{}
	_, err := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: 5000, Sleep: fs.sleep})
	if err != nil {
		t.Fatalf("expected success after honoring Retry-After: %v", err)
	}
	if len(fs.slept) != 1 || fs.slept[0] != 2*time.Second {
		t.Errorf("slept = %v, want a single 2s (Retry-After) wait", fs.slept)
	}
}

func TestExecuteWithRetryFatalNotRetried(t *testing.T) {
	d := &replayDoer{steps: []step{{resp: apiResp(422, "INSUFFICIENT_BALANCE", nil)}, {resp: okResp()}}}
	_, err := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: 5000})
	if err == nil || d.call != 1 {
		t.Errorf("a non-transient API error must not be retried: err=%v calls=%d", err, d.call)
	}
}

func TestExecuteWithRetryTimeWindowCorrectsAndRetries(t *testing.T) {
	d := &replayDoer{steps: []step{
		{resp: apiResp(400, "EXCEED_TIME_WINDOW", nil)},
		{resp: okResp()},
	}}
	const offset = int64(5000)
	corrected := 0
	creds := testKey(t)
	_, err := ExecuteWithRetry(
		Request{Method: "GET", Path: "/v2/balances", Auth: true},
		Options{Doer: d, Now: func() int64 { return 1_000_000 }, Creds: &creds},
		RetryPolicy{
			Idempotent: true, BudgetMs: 5000,
			Correct: func() (Correction, error) {
				corrected++
				return Correction{Now: func() int64 { return 1_000_000 + offset }}, nil
			},
		})
	if err != nil {
		t.Fatalf("expected success after a corrective retry: %v", err)
	}
	if corrected != 1 || d.call != 2 {
		t.Fatalf("corrected=%d calls=%d, want 1 and 2", corrected, d.call)
	}
	// The first attempt signs with the raw clock, the second with +offset.
	ts0 := timestampOf(t, d.requests[0])
	ts1 := timestampOf(t, d.requests[1])
	if ts0 != 1_000_000 {
		t.Errorf("first timestamp = %d, want 1000000", ts0)
	}
	if ts1 != 1_000_000+offset {
		t.Errorf("corrected timestamp = %d, want %d", ts1, 1_000_000+offset)
	}
}

func TestExecuteWithRetryTimeWindowNoCorrectorSurfaces(t *testing.T) {
	d := &replayDoer{steps: []step{{resp: apiResp(400, "EXCEED_TIME_WINDOW", nil)}}}
	_, err := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: 5000, Correct: nil})
	if err == nil || d.call != 1 {
		t.Errorf("without a corrector EXCEED_TIME_WINDOW must surface, not loop: err=%v calls=%d", err, d.call)
	}
}

func TestExecuteWithRetryTimeWindowCorrectedOnceThenSurfaces(t *testing.T) {
	d := &replayDoer{steps: []step{
		{resp: apiResp(400, "EXCEED_TIME_WINDOW", nil)},
		{resp: apiResp(400, "EXCEED_TIME_WINDOW", nil)},
	}}
	creds := testKey(t)
	_, err := ExecuteWithRetry(
		Request{Method: "GET", Path: "/v2/balances", Auth: true},
		Options{Doer: d, Now: func() int64 { return 1 }, Creds: &creds},
		RetryPolicy{Idempotent: true, BudgetMs: 5000, Correct: func() (Correction, error) { return Correction{Now: func() int64 { return 8 }}, nil }})
	if err == nil {
		t.Fatal("expected the second EXCEED_TIME_WINDOW to surface")
	}
	if d.call != 2 {
		t.Errorf("calls = %d, want 2 (correct once, no further loop)", d.call)
	}
}

// TestExecuteWithRetryNonIdempotentTimeWindowIsSingleShot is the safety pin: a
// money-moving / non-idempotent endpoint rejected with EXCEED_TIME_WINDOW must
// NOT be auto-corrected and resent — it is sent exactly once and Correct is
// never invoked. (Proactive --time-sync corrects such a request on its single
// send instead; that path doesn't go through this retry loop.)
func TestExecuteWithRetryNonIdempotentTimeWindowIsSingleShot(t *testing.T) {
	d := &replayDoer{steps: []step{
		{resp: apiResp(400, "EXCEED_TIME_WINDOW", nil)},
		{resp: okResp()},
	}}
	correctCalls := 0
	_, err := ExecuteWithRetry(
		Request{Method: "POST", Path: "/v2/coin/withdrawal"}, Options{Doer: d},
		RetryPolicy{
			Idempotent: false, BudgetMs: 5000,
			Correct: func() (Correction, error) { correctCalls++; return Correction{}, nil },
		})
	if err == nil {
		t.Fatal("expected EXCEED_TIME_WINDOW to surface for a non-idempotent endpoint")
	}
	if d.call != 1 {
		t.Errorf("endpoint was sent %d times; a non-idempotent endpoint must be single-shot", d.call)
	}
	if correctCalls != 0 {
		t.Errorf("Correct was invoked %d times; it must never fire for a non-idempotent endpoint", correctCalls)
	}
}

func TestRetryGovernorClassDecisions(t *testing.T) {
	// Fatal is never retried.
	g := NewRetryGovernor(true, true, true, 5000)
	if d := g.Next(ClassFatal, 0); d.Action != RetryGiveUp {
		t.Errorf("ClassFatal: action %v, want GiveUp", d.Action)
	}

	// EXCEED_TIME_WINDOW corrects exactly once, then gives up.
	g = NewRetryGovernor(true, true, true, 5000)
	if d := g.Next(ClassTimeWindow, 0); d.Action != RetryResync {
		t.Fatalf("first ETW: action %v, want Resync", d.Action)
	}
	if d := g.Next(ClassTimeWindow, 0); d.Action != RetryGiveUp {
		t.Errorf("second ETW: action %v, want GiveUp (resync-once)", d.Action)
	}

	// No resync hook: ETW is surfaced, not corrected.
	g = NewRetryGovernor(true, true, false, 5000)
	if d := g.Next(ClassTimeWindow, 0); d.Action != RetryGiveUp {
		t.Errorf("ETW without resync hook: action %v, want GiveUp", d.Action)
	}

	// Neither gate set: even the pre-exec classes are not retried.
	g = NewRetryGovernor(false, false, true, 5000)
	if d := g.Next(ClassTimeWindow, 0); d.Action != RetryGiveUp {
		t.Errorf("ETW with no gate: action %v, want GiveUp", d.Action)
	}
	if d := g.Next(ClassRateLimited, 0); d.Action != RetryGiveUp {
		t.Errorf("429 with no gate: action %v, want GiveUp", d.Action)
	}

	// Transient is retried only when idempotent.
	g = NewRetryGovernor(false, true, true, 5000)
	if d := g.Next(ClassTransient, 0); d.Action != RetryGiveUp {
		t.Errorf("transient with retryPre-only: action %v, want GiveUp", d.Action)
	}
}

func TestRetryGovernorBackoffAndBudget(t *testing.T) {
	// Transient backoff doubles (100, 200, 400, ...) within budget, then gives up
	// once the next backoff would exceed it.
	g := NewRetryGovernor(true, true, true, 700)
	var waits []time.Duration
	for {
		d := g.Next(ClassTransient, 0)
		if d.Action == RetryGiveUp {
			break
		}
		if d.Action != RetryWait {
			t.Fatalf("unexpected action %v", d.Action)
		}
		waits = append(waits, d.Wait)
	}
	// 100 + 200 + 400 = 700 fits; the next (800) would push to 1500 > 700 budget.
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait[%d] = %v, want %v", i, waits[i], want[i])
		}
	}

	// 429 honors Retry-After over the backoff.
	g = NewRetryGovernor(true, true, true, 5000)
	if d := g.Next(ClassRateLimited, 2000); d.Action != RetryWait || d.Wait != 2*time.Second {
		t.Errorf("429 Retry-After: action %v wait %v, want Wait 2s", d.Action, d.Wait)
	}
}

func TestRetryGovernorAttemptCeiling(t *testing.T) {
	g := NewRetryGovernor(true, true, true, 0) // ceiling = retryFreeIterations
	n := 0
	for g.Attempt() {
		n++
		if n > retryFreeIterations+5 {
			t.Fatal("Attempt never returned false — ceiling not enforced")
		}
	}
	if n != retryFreeIterations {
		t.Errorf("ran %d attempts, want the ceiling %d", n, retryFreeIterations)
	}
	if g.Attempts() != retryFreeIterations+1 {
		t.Errorf("Attempts() = %d, want %d (the final false-returning call counts)", g.Attempts(), retryFreeIterations+1)
	}
}

// TestRetryEngineCeilingFailsSafe exercises the engine's hard-ceiling fail-safe
// exit, which is unreachable in normal operation (the budget and resync-once
// bounds always terminate the loop first — see MaxRetryIterations). The
// maxIters seam forces the ceiling below the budget so gov.Attempt(), not the
// budget, stops the loop; the loop must then surface the LAST error (never a
// nil/nil "success") rather than spin. A relentless transient send with a large
// budget keeps the budget from being the terminating bound.
func TestRetryEngineCeilingFailsSafe(t *testing.T) {
	n := 0
	eng := retryEngine{
		idempotent: true,
		retryPre:   true,
		budgetMs:   1_000_000,              // huge: the budget must NOT be what stops the loop
		maxIters:   3,                      // force the ceiling instead
		sleep:      func(time.Duration) {}, // don't burn real time on the backoffs
	}
	data, err := eng.run(func(Options) (json.RawMessage, error) {
		n++
		return nil, errors.New("net") // always transient → RetryWait every time
	}, Options{})
	if n != 3 {
		t.Fatalf("ran %d attempts, want maxIters=3 (the ceiling, not the budget, must stop it)", n)
	}
	if data != nil {
		t.Errorf("ceiling fail-safe returned data %q, want nil", data)
	}
	if err == nil || err.Error() != "net" {
		t.Fatalf("ceiling fail-safe must surface the last error, got %v", err)
	}
}

func TestMaxRetryIterations(t *testing.T) {
	cases := []struct {
		budget int64
		want   int
	}{
		{0, retryFreeIterations},              // no budget: only the free slack
		{-1, retryFreeIterations},             // negative clamps to 0
		{250, retryFreeIterations + 2},        // 250/100 = 2 sleeping iters
		{5000, retryFreeIterations + 50},      // the default --retry-timeout
		{600_000, retryFreeIterations + 6000}, // scales with a large budget
	}
	for _, c := range cases {
		if got := MaxRetryIterations(c.budget); got != c.want {
			t.Errorf("MaxRetryIterations(%d) = %d, want %d", c.budget, got, c.want)
		}
	}
}

// TestRetryLoopTerminatesAndStaysUnderCeiling pins that the engine ALWAYS
// terminates under relentless failure and never exceeds the hard iteration
// ceiling. The Doer is programmed with far more failing steps than any bound
// allows, so if both the budget bound and the ceiling were ever broken the test
// would error (replayDoer: "more calls than programmed") rather than hang — but
// it must instead stop well under the ceiling, driven by the budget.
func TestRetryLoopTerminatesAndStaysUnderCeiling(t *testing.T) {
	steps := make([]step, 1000)
	for i := range steps {
		steps[i] = step{err: errors.New("net")}
	}
	d := &replayDoer{steps: steps}
	fs := &fakeSleep{}
	attempts := 0
	const budget = 5000
	_, err := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d},
		RetryPolicy{Idempotent: true, BudgetMs: budget, Sleep: fs.sleep, Attempts: &attempts})
	if err == nil {
		t.Fatal("expected failure under relentless transient errors")
	}
	if ceiling := MaxRetryIterations(budget); attempts > ceiling {
		t.Errorf("attempts %d exceeded the hard iteration ceiling %d", attempts, ceiling)
	}
	if attempts < 2 {
		t.Errorf("attempts %d, expected at least one retry before giving up", attempts)
	}
}

// timestampOf extracts the signed `timestamp` param from a built request's
// query string (GET) for assertion.
func timestampOf(t *testing.T, r *http.Request) int64 {
	t.Helper()
	u, err := url.Parse(r.URL.String())
	if err != nil {
		t.Fatalf("bad url: %v", err)
	}
	v := u.Query().Get("timestamp")
	if v == "" {
		t.Fatalf("no timestamp param in %s", r.URL.String())
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("timestamp %q not an int: %v", v, err)
	}
	return n
}
