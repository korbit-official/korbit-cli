// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// pathDoer dispatches on the request path so a test can serve /v2/time probes
// and the real endpoint differently, and records every request for assertion.
type pathDoer struct {
	onTime     func() (*http.Response, error)
	onEndpoint func(n int) (*http.Response, error) // n = 1-based endpoint call count
	endpoint   int
	reqs       []*http.Request
}

func (d *pathDoer) Do(r *http.Request) (*http.Response, error) {
	d.reqs = append(d.reqs, r)
	if strings.Contains(r.URL.Path, "/v2/time") {
		return d.onTime()
	}
	d.endpoint++
	return d.onEndpoint(d.endpoint)
}

// lastEndpointQuery returns the parsed query of the most recent non-time
// request (where the signed timestamp/recvWindow live for a GET).
func (d *pathDoer) lastEndpointQuery(t *testing.T) url.Values {
	t.Helper()
	for i := len(d.reqs) - 1; i >= 0; i-- {
		if !strings.Contains(d.reqs[i].URL.Path, "/v2/time") {
			return d.reqs[i].URL.Query()
		}
	}
	t.Fatal("no endpoint request recorded")
	return nil
}

func timeResp(ms int64) (*http.Response, error) {
	return resp(200, `{"success":true,"data":{"time":`+strconv.FormatInt(ms, 10)+`}}`, nil), nil
}

// TestTimeSyncProactivelyCorrectsTimestamp: with --time-sync on the signed
// timestamp is the server clock, not the (fixed) local clock. Local clock is
// pinned to 1700000000000; server is +8000ms. With rtt 0 the measured
// uncertainty is 0, so the widened recvWindow stays at/under the 5s default and
// is OMITTED (the unified omit-below-5s rule — the server applies its own 5s
// window). recvWindow only appears on the wire once a widening genuinely exceeds 5s.
func TestTimeSyncProactivelyCorrectsTimestamp(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	const serverMs = 1700000008000
	d := &pathDoer{
		onTime: func() (*http.Response, error) { return timeResp(serverMs) },
		onEndpoint: func(int) (*http.Response, error) {
			return resp(200, `{"success":true,"data":{"krw":{"available":"1"}}}`, nil), nil
		},
	}
	// The time-sync confirmation is Info-level telemetry (suppressed by default);
	// --debug surfaces it. See TestDiagnosticsAreDebugGated for the gating rule.
	_, stderr, code := runCLI(
		[]string{"balance", "--time-sync", "on", "--key", "bot", "--compact", "--debug"},
		map[string]string{"KORBIT_CLI_HOME": home}, d)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	q := d.lastEndpointQuery(t)
	if q.Get("timestamp") != "1700000008000" {
		t.Errorf("timestamp = %q, want the corrected server clock 1700000008000", q.Get("timestamp"))
	}
	// rtt is 0 under a fixed clock, so the widening is <= the 5s default and the
	// param is omitted (omit-below-5s); the server then applies its own 5s window.
	if q.Has("recvWindow") {
		t.Errorf("recvWindow = %q, want it omitted (widening <= 5s default)", q.Get("recvWindow"))
	}
	if !strings.Contains(stderr, "time-sync") {
		t.Errorf("expected a time-sync note on stderr, got %q", stderr)
	}
}

// TestTimeSyncOnViaEnv: KORBIT_CLI_TIME_SYNC=on enables proactive correction
// just like the flag (the env twin documented on the global flag).
func TestTimeSyncOnViaEnv(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	const serverMs = 1700000008000
	d := &pathDoer{
		onTime: func() (*http.Response, error) { return timeResp(serverMs) },
		onEndpoint: func(int) (*http.Response, error) {
			return resp(200, `{"success":true,"data":{"krw":{"available":"1"}}}`, nil), nil
		},
	}
	_, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home, "KORBIT_CLI_TIME_SYNC": "on"}, d)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if q := d.lastEndpointQuery(t); q.Get("timestamp") != "1700000008000" {
		t.Errorf("timestamp = %q, want the corrected server clock 1700000008000 (env on)", q.Get("timestamp"))
	}
}

// TestAutoRetryCorrectsOnExceedTimeWindow: a GET (idempotent) rejected with
// EXCEED_TIME_WINDOW is auto-corrected and retried to success, WITHOUT the
// --time-sync flag.
func TestAutoRetryCorrectsOnExceedTimeWindow(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	const serverMs = 1700000008000
	d := &pathDoer{
		onTime: func() (*http.Response, error) { return timeResp(serverMs) },
		onEndpoint: func(n int) (*http.Response, error) {
			if n == 1 {
				return resp(400, `{"success":false,"error":{"code":400,"message":"EXCEED_TIME_WINDOW"}}`, nil), nil
			}
			return resp(200, `{"success":true,"data":{"krw":{"available":"1"}}}`, nil), nil
		},
	}
	out, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, d)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s out=%s", code, stderr, out)
	}
	if d.endpoint != 2 {
		t.Errorf("endpoint calls = %d, want 2 (reject then corrected retry)", d.endpoint)
	}
	if q := d.lastEndpointQuery(t); q.Get("timestamp") != "1700000008000" {
		t.Errorf("retry timestamp = %q, want corrected 1700000008000", q.Get("timestamp"))
	}
}

// TestTimeSyncOffDisablesReactiveResync: with --time-sync off an idempotent GET
// rejected with EXCEED_TIME_WINDOW is NOT auto-corrected — it is sent once, never
// probes /v2/time, and fails. (Compare TestAutoRetryCorrectsOnExceedTimeWindow,
// where the auto default corrects and retries the same call.)
func TestTimeSyncOffDisablesReactiveResync(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	d := &pathDoer{
		onTime: func() (*http.Response, error) { return timeResp(1700000008000) },
		onEndpoint: func(int) (*http.Response, error) {
			return resp(400, `{"success":false,"error":{"code":400,"message":"EXCEED_TIME_WINDOW"}}`, nil), nil
		},
	}
	_, stderr, code := runCLI(
		[]string{"balance", "--time-sync", "off", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, d)
	if code == 0 {
		t.Fatalf("expected a non-zero exit for the uncorrected EXCEED_TIME_WINDOW; stderr=%s", stderr)
	}
	if d.endpoint != 1 {
		t.Errorf("endpoint calls = %d, want 1 (off = no corrective retry)", d.endpoint)
	}
	for _, r := range d.reqs {
		if strings.Contains(r.URL.Path, "/v2/time") {
			t.Error("off must not probe /v2/time (no clock correction at all)")
		}
	}
}

// TestTimeSyncInvalidValueIsUsageError: an unrecognized --time-sync word is a
// usage error (exit 2), not a silent fall-through to the auto default.
func TestTimeSyncInvalidValueIsUsageError(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	_, stderr, code := runCLI(
		[]string{"balance", "--time-sync", "sometimes", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d, want 2 for a bad --time-sync value; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "time-sync") {
		t.Errorf("expected the error to name --time-sync, got %q", stderr)
	}
}

// TestMoneyMoverNotAutoRetried: a non-idempotent POST that fails transiently
// (HTTP 503) is sent exactly once — never auto-resent.
func TestMoneyMoverNotAutoRetried(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	d := &countingDoer{status: 503, body: `{"success":false,"error":{"code":503,"message":"SERVICE_UNAVAILABLE"}}`}
	_, _, code := runCLI(
		[]string{"withdraw", "request", "btc", "--amount", "0.001",
			"--address", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, d)
	if code == 0 {
		t.Fatal("expected a non-zero exit for the 503")
	}
	if d.calls != 1 {
		t.Errorf("withdraw request was sent %d times; a money-mover must be single-shot", d.calls)
	}
}

// countingDoer returns a fresh response with the same status+body on every call
// and counts how many calls it received.
type countingDoer struct {
	status int
	body   string
	calls  int
}

func (c *countingDoer) Do(r *http.Request) (*http.Response, error) {
	c.calls++
	return resp(c.status, c.body, nil), nil
}

// placeETWDoer serves /v2/time probes, rejects the FIRST POST /v2/orders with
// EXCEED_TIME_WINDOW, accepts the second, and answers the verify lookup GET with
// the placed order. It records the form body of every POST so the test can
// assert the resend reused the same clientOrderId.
type placeETWDoer struct {
	serverMs   int64
	times      int
	postBodies []string
	gets       int
}

func (d *placeETWDoer) Do(r *http.Request) (*http.Response, error) {
	p := r.URL.Path
	switch {
	case strings.Contains(p, "/v2/time"):
		d.times++
		return timeResp(d.serverMs)
	case r.Method == http.MethodPost && strings.Contains(p, "/v2/orders"):
		body, _ := io.ReadAll(r.Body)
		d.postBodies = append(d.postBodies, string(body))
		if len(d.postBodies) == 1 {
			return resp(400, `{"success":false,"error":{"code":400,"message":"EXCEED_TIME_WINDOW"}}`, nil), nil
		}
		return resp(200, `{"success":true,"data":{"orderId":777}}`, nil), nil
	default: // GET /v2/orders — the verify-window lookup
		d.gets++
		return resp(200, `{"success":true,"data":{"orderId":777,"status":"open"}}`, nil), nil
	}
}

// TestOrderPlaceAutoResyncsOnExceedTimeWindow: a single-shot CLI `order place`
// rejected with EXCEED_TIME_WINDOW re-syncs the clock once and resends the SAME
// clientOrderId (the rejection is provably pre-execution, so the resend is safe).
// This exercises the place protocol's ops.Resync hook on the CLI, matching the
// monitor/mcp place behavior.
func TestOrderPlaceAutoResyncsOnExceedTimeWindow(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	d := &placeETWDoer{serverMs: 1700000008000}
	out, stderr, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, d)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s out=%s", code, stderr, out)
	}
	if len(d.postBodies) != 2 {
		t.Fatalf("POST /v2/orders sent %d times; want 2 (ETW reject, then resync + resend)", len(d.postBodies))
	}
	if d.times == 0 {
		t.Fatal("expected a /v2/time resync probe after EXCEED_TIME_WINDOW")
	}
	first := coidOf(t, d.postBodies[0])
	second := coidOf(t, d.postBodies[1])
	if first == "" {
		t.Fatal("first placement carried no clientOrderId")
	}
	if first != second {
		t.Fatalf("resend used a different clientOrderId: first=%q resend=%q (must reuse the idempotency key)", first, second)
	}
}

// TestSingleShotWriteNotResyncedOnExceedTimeWindow: a strictly single-shot money
// mover with no idempotency key (withdraw request) is NOT auto-corrected on
// EXCEED_TIME_WINDOW — it is sent exactly once and fails cleanly (pass
// --time-sync to correct proactively instead).
func TestSingleShotWriteNotResyncedOnExceedTimeWindow(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	cd := &etwCountingDoer{}
	_, _, code := runCLI(
		[]string{"withdraw", "request", "btc", "--amount", "0.001",
			"--address", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, cd)
	if code == 0 {
		t.Fatal("expected a non-zero exit for the EXCEED_TIME_WINDOW rejection")
	}
	if cd.writes != 1 {
		t.Errorf("withdraw sent %d times; a no-idempotency-key write must be single-shot even on ETW", cd.writes)
	}
	if cd.times != 0 {
		t.Errorf("withdraw probed /v2/time %d times; a single-shot write must not auto-resync on ETW", cd.times)
	}
}

// etwCountingDoer always rejects the write with EXCEED_TIME_WINDOW and counts
// write sends vs /v2/time probes.
type etwCountingDoer struct {
	writes int
	times  int
}

func (c *etwCountingDoer) Do(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/v2/time") {
		c.times++
		return timeResp(1700000000000)
	}
	c.writes++
	return resp(400, `{"success":false,"error":{"code":400,"message":"EXCEED_TIME_WINDOW"}}`, nil), nil
}

// coidOf extracts the clientOrderId from a urlencoded POST body.
func coidOf(t *testing.T, body string) string {
	t.Helper()
	v, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("bad POST body %q: %v", body, err)
	}
	return v.Get("clientOrderId")
}
