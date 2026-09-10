// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package apiclient

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// recRecorder is a programmable Recorder for the Do tests.
type recRecorder struct {
	readyErr error // if non-nil, Ready returns it (abort-before-send)
	recordID int64

	mu          sync.Mutex
	readyCalls  int
	recordCalls int
	lastInfo    CallInfo
	lastOutcome Outcome
}

func (r *recRecorder) Ready(info CallInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readyCalls++
	r.lastInfo = info
	return r.readyErr
}

func (r *recRecorder) Record(info CallInfo, out Outcome) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordCalls++
	r.lastInfo = info
	r.lastOutcome = out
	return r.recordID
}

// recFor wires a fixed Recorder as a Client.NewRecorder (the same instance for
// every Do, which is fine for these single-call tests).
func recFor(r Recorder) func(context.Context, Call) Recorder {
	return func(context.Context, Call) Recorder { return r }
}

// fakeClock is a programmable apiclient.Clock for the Do tests. now (when set) is
// read fresh on each sign, so a test can mutate the signed timestamp from a
// Resync hook.
type fakeClock struct {
	now        func() int64
	recvWindow int
	offset     int64
}

func (f fakeClock) SignNow() int64 {
	if f.now != nil {
		return f.now()
	}
	return 0
}
func (f fakeClock) RecvWindowMs() int { return f.recvWindow }
func (f fakeClock) Offset() int64     { return f.offset }
func (f fakeClock) Measured() bool    { return f.offset != 0 }
func (f fakeClock) ServerNowMs() int64 {
	if f.now != nil {
		return f.now() + f.offset
	}
	return f.offset
}

func TestDoZeroPolicyIsSingleShot(t *testing.T) {
	// A transient (network) failure under the zero Policy must NOT be retried.
	d := &replayDoer{steps: []step{{err: errors.New("connection reset")}, {resp: okResp()}}}
	c := &Client{BaseURL: "https://api.example", Doer: d}
	_, meta, err := c.Do(context.Background(), Call{Method: "POST", Path: "/v2/orders"}, Policy{})
	if err == nil {
		t.Fatal("zero Policy must surface the first error (single shot)")
	}
	if d.call != 1 {
		t.Errorf("sent %d times; zero Policy must be exactly 1", d.call)
	}
	if meta.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", meta.Attempts)
	}
}

func TestDoIdempotentRetryParityWithExecuteWithRetry(t *testing.T) {
	// network -> success, with a budget: both paths must take 2 attempts and
	// one InitialBackoff sleep.
	mkDoer := func() *replayDoer {
		return &replayDoer{steps: []step{{err: errors.New("net")}, {resp: okResp()}}}
	}

	// Baseline: ExecuteWithRetry.
	d1 := mkDoer()
	fs1 := &fakeSleep{}
	at1 := 0
	_, err1 := ExecuteWithRetry(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d1},
		RetryPolicy{Idempotent: true, BudgetMs: 5000, Sleep: fs1.sleep, Attempts: &at1})
	if err1 != nil || d1.call != 2 || at1 != 2 || len(fs1.slept) != 1 || fs1.slept[0] != InitialBackoffMs*time.Millisecond {
		t.Fatalf("baseline ExecuteWithRetry: err=%v calls=%d attempts=%d slept=%v", err1, d1.call, at1, fs1.slept)
	}

	// Do with an idempotent policy must match.
	d2 := mkDoer()
	fs2 := &fakeSleep{}
	c := &Client{BaseURL: "https://api.example", Doer: d2, Sleep: fs2.sleep}
	_, meta, err2 := c.Do(context.Background(), Call{Method: "GET", Path: "/v2/x"},
		Policy{Idempotent: true, BudgetMs: 5000})
	if err2 != nil {
		t.Fatalf("Do idempotent: %v", err2)
	}
	if d2.call != d1.call || meta.Attempts != at1 {
		t.Errorf("attempt parity: Do calls=%d attempts=%d, want calls=%d attempts=%d", d2.call, meta.Attempts, d1.call, at1)
	}
	if len(fs2.slept) != len(fs1.slept) || fs2.slept[0] != fs1.slept[0] {
		t.Errorf("backoff parity: Do slept=%v, want %v", fs2.slept, fs1.slept)
	}
}

func TestDoRetryPreExecRetries429AndTimeWindowButNotTransient(t *testing.T) {
	// !Idempotent + RetryPreExec.

	// 429 -> success: retried.
	t.Run("429 retried", func(t *testing.T) {
		d := &replayDoer{steps: []step{
			{resp: apiResp(429, "TOO_MANY_REQUESTS", map[string]string{"Retry-After": "1"})},
			{resp: okResp()},
		}}
		fs := &fakeSleep{}
		c := &Client{BaseURL: "https://api.example", Doer: d, Sleep: fs.sleep}
		_, _, err := c.Do(context.Background(), Call{Method: "POST", Path: "/v2/orders"},
			Policy{Idempotent: false, RetryPreExec: true, BudgetMs: 5000})
		if err != nil {
			t.Fatalf("RetryPreExec must retry 429: %v", err)
		}
		if d.call != 2 || len(fs.slept) != 1 || fs.slept[0] != 1*time.Second {
			t.Errorf("calls=%d slept=%v, want 2 calls and one 1s Retry-After wait", d.call, fs.slept)
		}
	})

	// EXCEED_TIME_WINDOW -> corrective resync -> success: retried.
	t.Run("EXCEED_TIME_WINDOW corrected", func(t *testing.T) {
		d := &replayDoer{steps: []step{
			{resp: apiResp(400, "EXCEED_TIME_WINDOW", nil)},
			{resp: okResp()},
		}}
		creds := testKey(t)
		corrected := 0
		signNow := int64(1000)
		c := &Client{
			BaseURL: "https://api.example", Doer: d, Creds: &creds,
			Clock: fakeClock{now: func() int64 { return signNow }},
			Resync: func() error {
				corrected++
				signNow = 9000 // the re-sign reads the now-corrected clock
				return nil
			},
		}
		_, _, err := c.Do(context.Background(), Call{Method: "POST", Path: "/v2/orders", Auth: true},
			Policy{Idempotent: false, RetryPreExec: true, BudgetMs: 5000})
		if err != nil {
			t.Fatalf("RetryPreExec must correct+retry EXCEED_TIME_WINDOW: %v", err)
		}
		if corrected != 1 || d.call != 2 {
			t.Errorf("corrected=%d calls=%d, want 1 and 2", corrected, d.call)
		}
	})

	// network (transient/ambiguous) must NOT be retried under RetryPreExec.
	t.Run("network not retried", func(t *testing.T) {
		d := &replayDoer{steps: []step{{err: errors.New("net")}, {resp: okResp()}}}
		c := &Client{BaseURL: "https://api.example", Doer: d}
		_, _, err := c.Do(context.Background(), Call{Method: "POST", Path: "/v2/orders"},
			Policy{Idempotent: false, RetryPreExec: true, BudgetMs: 5000})
		if err == nil {
			t.Fatal("RetryPreExec must NOT retry an ambiguous network failure")
		}
		if d.call != 1 {
			t.Errorf("network sent %d times; must be single shot under RetryPreExec", d.call)
		}
	})

	// 5xx (transient/ambiguous) must NOT be retried under RetryPreExec.
	t.Run("5xx not retried", func(t *testing.T) {
		d := &replayDoer{steps: []step{{resp: apiResp(503, "SERVICE_UNAVAILABLE", nil)}, {resp: okResp()}}}
		c := &Client{BaseURL: "https://api.example", Doer: d}
		_, _, err := c.Do(context.Background(), Call{Method: "POST", Path: "/v2/orders"},
			Policy{Idempotent: false, RetryPreExec: true, BudgetMs: 5000})
		if err == nil {
			t.Fatal("RetryPreExec must NOT retry an ambiguous 5xx")
		}
		if d.call != 1 {
			t.Errorf("5xx sent %d times; must be single shot under RetryPreExec", d.call)
		}
	})
}

func TestDoRecorderReadyAbortSendsNothing(t *testing.T) {
	d := &replayDoer{steps: []step{{resp: okResp()}}}
	rec := &recRecorder{readyErr: errors.New("read-only home")}
	c := &Client{BaseURL: "https://api.example", Doer: d, NewRecorder: recFor(rec)}
	_, _, err := c.Do(context.Background(), Call{Method: "POST", Path: "/v2/orders"}, Policy{})
	if err == nil || !strings.Contains(err.Error(), "read-only home") {
		t.Fatalf("Ready error must abort the call: %v", err)
	}
	if d.call != 0 {
		t.Errorf("nothing must reach the wire after a Ready abort; sent %d", d.call)
	}
	if rec.recordCalls != 0 {
		t.Errorf("Record must not run after a Ready abort; ran %d", rec.recordCalls)
	}
}

func TestDoRecordOnceWithAttempts(t *testing.T) {
	// Two attempts (network then success); Record must fire once with Attempts=2.
	d := &replayDoer{steps: []step{{err: errors.New("net")}, {resp: okResp()}}}
	fs := &fakeSleep{}
	rec := &recRecorder{recordID: 42}
	creds := testKey(t)
	c := &Client{BaseURL: "https://api.example", Doer: d, Creds: &creds, Sleep: fs.sleep, NewRecorder: recFor(rec)}
	_, meta, err := c.Do(context.Background(), Call{Method: "GET", Path: "/v2/balance", Auth: true},
		Policy{Idempotent: true, BudgetMs: 5000})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if rec.readyCalls != 1 {
		t.Errorf("Ready calls = %d, want 1", rec.readyCalls)
	}
	if rec.recordCalls != 1 {
		t.Errorf("Record calls = %d, want exactly 1 (one logical Do == one record)", rec.recordCalls)
	}
	if rec.lastOutcome.Attempts != 2 {
		t.Errorf("recorded Attempts = %d, want 2", rec.lastOutcome.Attempts)
	}
	if rec.lastOutcome.Err != nil {
		t.Errorf("recorded a success outcome with Err = %v", rec.lastOutcome.Err)
	}
	if meta.RecordID != 42 {
		t.Errorf("Meta.RecordID = %d, want 42", meta.RecordID)
	}
	if rec.lastInfo.Method != "GET" || rec.lastInfo.Path != "/v2/balance" {
		t.Errorf("CallInfo = %q %q, want GET /v2/balance", rec.lastInfo.Method, rec.lastInfo.Path)
	}
}

func TestDoCtxCancellationDuringBackoffSleep(t *testing.T) {
	// Idempotent retry, all attempts fail; cancel the context during the first
	// backoff sleep. Do must abort promptly and surface the context error.
	steps := make([]step, 10)
	for i := range steps {
		steps[i] = step{err: errors.New("net")}
	}
	d := &replayDoer{steps: steps}
	ctx, cancel := context.WithCancel(context.Background())

	// Sleep that cancels the context the moment it is first asked to wait, then
	// blocks (so the cancellation, not the sleep completing, is what unblocks).
	first := true
	blockingSleep := func(time.Duration) {
		if first {
			first = false
			cancel()
		}
		// Block long enough that the ctx path must win the select.
		time.Sleep(50 * time.Millisecond)
	}
	c := &Client{BaseURL: "https://api.example", Doer: d, Sleep: blockingSleep}
	start := time.Now()
	_, meta, err := c.Do(ctx, Call{Method: "GET", Path: "/v2/x"}, Policy{Idempotent: true, BudgetMs: 60000})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation mid-retry must surface context.Canceled, got %v", err)
	}
	// One attempt, then the canceled sleep aborts before a second send.
	if meta.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no send after the canceled sleep)", meta.Attempts)
	}
	if d.call != 1 {
		t.Errorf("sent %d times; the canceled sleep must prevent a resend", d.call)
	}
	if elapsed > 40*time.Millisecond {
		t.Errorf("abort took %v; cancellation should be prompt (not wait out the sleep)", elapsed)
	}
}

// TestDoCallInfoParamsJSONIsPreSigningOnly pins the load-bearing journal-safety
// invariant: the CallInfo handed to a Recorder carries the PRE-SIGNING business
// params and never the signing artifacts (timestamp/recvWindow/signature) that
// Build appends after the CallInfo is captured. A regression here would leak
// signing material into the journal seam.
func TestDoCallInfoParamsJSONIsPreSigningOnly(t *testing.T) {
	d := &replayDoer{steps: []step{{resp: okResp()}}}
	rec := &recRecorder{}
	creds := testKey(t)
	c := &Client{
		BaseURL: "https://api.example", Doer: d, Creds: &creds, NewRecorder: recFor(rec),
		// A non-zero recvWindow on the wire (from the clock's widened window) so the
		// assertion below proves it is excluded from the journal's pre-signing params.
		Clock: fakeClock{now: func() int64 { return 1700000000000 }, recvWindow: 5000},
	}
	_, _, err := c.Do(context.Background(),
		Call{Method: "POST", Path: "/v2/orders", Auth: true, Params: []KV{{"symbol", "btc_krw"}, {"price", "1000.5"}}},
		Policy{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pj := string(rec.lastInfo.ParamsJSON)
	for _, want := range []string{"symbol", "btc_krw", "price", "1000.5"} {
		if !strings.Contains(pj, want) {
			t.Errorf("ParamsJSON %q is missing the business param %q", pj, want)
		}
	}
	for _, banned := range []string{"timestamp", "recvWindow", "signature"} {
		if strings.Contains(pj, banned) {
			t.Errorf("ParamsJSON %q leaked the signing artifact %q (must be pre-signing only)", pj, banned)
		}
	}
	// And money strings stay verbatim strings (never parsed to a number).
	if !strings.Contains(pj, `"1000.5"`) {
		t.Errorf("money value must stay a decimal string in ParamsJSON, got %q", pj)
	}
}

func TestDoSignedCallWithoutCredsFailsFast(t *testing.T) {
	d := &replayDoer{steps: []step{{resp: okResp()}}}
	rec := &recRecorder{}
	c := &Client{BaseURL: "https://api.example", Doer: d, NewRecorder: recFor(rec)} // public-only
	_, _, err := c.Do(context.Background(), Call{Method: "GET", Path: "/v2/balance", Auth: true}, Policy{})
	if !errors.Is(err, errNoCreds) {
		t.Fatalf("signed call on a public-only client must fail fast, got %v", err)
	}
	if d.call != 0 {
		t.Errorf("nothing must reach the wire; sent %d", d.call)
	}
	if rec.readyCalls != 0 || rec.recordCalls != 0 {
		t.Errorf("recorder must not be touched on the creds guard: ready=%d record=%d", rec.readyCalls, rec.recordCalls)
	}
}

func TestDoUserAgentDefaultVsCustom(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		d := &replayDoer{steps: []step{{resp: okResp()}}}
		c := &Client{BaseURL: "https://api.example", Doer: d}
		if _, _, err := c.Do(context.Background(), Call{Method: "GET", Path: "/v2/x"}, Policy{}); err != nil {
			t.Fatal(err)
		}
		if got := d.requests[0].Header.Get("user-agent"); got != defaultUserAgent() {
			t.Errorf("default UA = %q, want %q", got, defaultUserAgent())
		}
	})
	t.Run("custom", func(t *testing.T) {
		d := &replayDoer{steps: []step{{resp: okResp()}}}
		c := &Client{BaseURL: "https://api.example", Doer: d, UserAgent: "my-bot/2.0 (linux)"}
		if _, _, err := c.Do(context.Background(), Call{Method: "GET", Path: "/v2/x"}, Policy{}); err != nil {
			t.Fatal(err)
		}
		if got := d.requests[0].Header.Get("user-agent"); got != "my-bot/2.0 (linux)" {
			t.Errorf("custom UA = %q, want my-bot/2.0 (linux)", got)
		}
	})
}

// TestBuildUserAgentDefaultUnchanged pins that the low-level Build emits the
// default UA when Options.UserAgent is empty.
func TestBuildUserAgentDefaultUnchanged(t *testing.T) {
	b := Build(Request{Method: "GET", Path: "/v2/x"}, Options{BaseURL: "https://api.example"})
	if b.Headers["user-agent"] != defaultUserAgent() {
		t.Errorf("Build UA = %q, want default %q", b.Headers["user-agent"], defaultUserAgent())
	}
}
