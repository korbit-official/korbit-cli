// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/cli"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

// doerFunc adapts a function to korbit.Doer, producing a fresh response per
// call (the stream layer probes REST more than once).
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// failingDoer fails every REST call; the monitor's startup clock measurement
// is best-effort, so this exercises the fallback path.
func failingDoer() korbit.Doer {
	return doerFunc(func(*http.Request) (*http.Response, error) {
		return resp(500, `{}`, nil), nil
	})
}

// fakeWSConn serves scripted frames, then blocks until canceled or closed.
type fakeWSConn struct {
	frames chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFakeWSConn(frames ...string) *fakeWSConn {
	c := &fakeWSConn{frames: make(chan []byte, len(frames)), closed: make(chan struct{})}
	for _, f := range frames {
		c.frames <- []byte(f)
	}
	return c
}

func (c *fakeWSConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case f := <-c.frames:
		return f, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, errors.New("connection closed")
	}
}

func (c *fakeWSConn) Write(context.Context, []byte) error { return nil }
func (c *fakeWSConn) Ping(context.Context) error          { return nil }
func (c *fakeWSConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// runMonitorCLI is runCLI plus the WebSocket dial seam.
func runMonitorCLI(args []string, env map[string]string, doer korbit.Doer, dial stream.Dialer) (string, string, int) {
	// The JavaScript bot runtime is gated behind --enable-experimental; enable it
	// by default here so the many scripting tests exercise the runtime directly.
	// The gate's own default-off behavior is covered by TestMonitorExperimentalGate.
	merged := map[string]string{"KORBIT_CLI_HOME": sharedTestHome(), "KORBIT_CLI_ENABLE_EXPERIMENTAL": "1"}
	for k, v := range env {
		merged[k] = v
	}
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv: func(k string) string { return merged[k] },
		Stdout: &out,
		Stderr: &errb,
		Doer:   doer,
		Now:    func() int64 { return 1700000000000 },
		Sleep:  func(time.Duration) {},
		WSDial: dial,
	})
	return out.String(), errb.String(), code
}

// TestSetBaseURLVerifies covers the post-set smoke test: a reachable pair is
// reported reachable, and an unreachable WebSocket host is non-fatal (exit 0)
// and surfaces the fix command.
func TestSetBaseURLVerifies(t *testing.T) {
	okDoer := doerFunc(func(*http.Request) (*http.Response, error) {
		return resp(200, `{"success":true,"data":{"serverTime":1700000000000}}`, nil), nil
	})

	t.Run("both reachable", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		out, _, code := runMonitorCLI(
			[]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, okDoer, dialFrames())
		if code != 0 {
			t.Fatalf("exit=%d out=%q", code, out)
		}
		var doc struct {
			Verification struct {
				REST endpointCheckJSON `json:"rest"`
				WS   endpointCheckJSON `json:"ws"`
			} `json:"verification"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("parse: %v (%s)", err, out)
		}
		if !doc.Verification.REST.Reachable || !doc.Verification.WS.Reachable {
			t.Fatalf("both should be reachable: %s", out)
		}
		if doc.Verification.WS.URL != "wss://ws-api-test.korbit.co.kr" {
			t.Fatalf("ws url wrong: %s", out)
		}
	})

	t.Run("ws unreachable is non-fatal and shows the fix", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		failDial := func(context.Context, string, http.Header) (stream.Conn, error) {
			return nil, errors.New("dial tcp: connection refused")
		}
		out, errb, code := runMonitorCLI(
			[]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, okDoer, failDial)
		if code != 0 {
			t.Fatalf("an unreachable endpoint must not fail the command: exit=%d", code)
		}
		var doc struct {
			BaseURL      string `json:"baseUrl"`
			Verification struct {
				WS endpointCheckJSON `json:"ws"`
			} `json:"verification"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("parse: %v (%s)", err, out)
		}
		if doc.BaseURL != "https://api-test.korbit.co.kr" {
			t.Fatalf("URL must still be stored: %s", out)
		}
		if doc.Verification.WS.Reachable {
			t.Fatalf("ws should be reported unreachable: %s", out)
		}
		// The verification (and its fix) is part of the result: structured here under
		// --compact, and rendered on stdout in human mode. Nothing on stderr.
		if strings.TrimSpace(errb) != "" {
			t.Fatalf("set-base-url --compact must write nothing to stderr: %s", errb)
		}
		humanOut, humanErr, _ := runMonitorCLI(
			[]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr"},
			map[string]string{"KORBIT_CLI_HOME": home}, okDoer, failDial)
		if !strings.Contains(humanOut, "--ws-base-url") || !strings.Contains(humanOut, "key set-base-url bot") {
			t.Fatalf("fix command not shown on stdout: %s (stderr=%s)", humanOut, humanErr)
		}
	})

	t.Run("rest unreachable is non-fatal and shows the fix", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		failDoer := doerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: connection refused")
		})
		out, errb, code := runMonitorCLI(
			[]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, failDoer, dialFrames())
		if code != 0 {
			t.Fatalf("an unreachable REST endpoint must not fail the command: exit=%d", code)
		}
		var doc struct {
			Verification struct {
				REST endpointCheckJSON `json:"rest"`
			} `json:"verification"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("parse: %v (%s)", err, out)
		}
		if doc.Verification.REST.Reachable {
			t.Fatalf("rest should be reported unreachable: %s", out)
		}
		if strings.TrimSpace(errb) != "" {
			t.Fatalf("set-base-url --compact must write nothing to stderr: %s", errb)
		}
		humanOut, humanErr, _ := runMonitorCLI(
			[]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr"},
			map[string]string{"KORBIT_CLI_HOME": home}, failDoer, dialFrames())
		if !strings.Contains(humanOut, "double-check the base URL") {
			t.Fatalf("REST fix guidance not shown on stdout: %s (stderr=%s)", humanOut, humanErr)
		}
	})

	t.Run("--no-verify skips the probe", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		// A dialer that fails the test if called proves the probe is skipped.
		neverDial := func(context.Context, string, http.Header) (stream.Conn, error) {
			t.Error("dialer must not be called with --no-verify")
			return nil, errors.New("unexpected")
		}
		out, _, code := runMonitorCLI(
			[]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr", "--no-verify", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, okDoer, neverDial)
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if strings.Contains(out, `"verification"`) {
			t.Fatalf("--no-verify must omit verification: %s", out)
		}
	})
}

// endpointCheckJSON mirrors the JSON shape of an endpoint smoke-test result.
type endpointCheckJSON struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail"`
}

func tickerFrame(close string) string {
	return `{"type":"ticker","timestamp":1700000000000,"symbol":"btc_krw","snapshot":false,"data":{"close":"` + close + `"}}`
}

func dialFrames(frames ...string) stream.Dialer {
	return func(context.Context, string, http.Header) (stream.Conn, error) {
		return newFakeWSConn(frames...), nil
	}
}

// monitorLine is the parsed JSON-mode output line.
type monitorLine struct {
	Type       string          `json:"type"`
	Channel    string          `json:"channel"`
	Symbol     string          `json:"symbol"`
	Origin     string          `json:"origin"`
	ServerTime int64           `json:"serverTime"`
	Code       string          `json:"code"`
	Payload    json.RawMessage `json:"payload"`
}

func parseMonitorLines(t *testing.T, out string) []monitorLine {
	t.Helper()
	var lines []monitorLine
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		if raw == "" {
			continue
		}
		var l monitorLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("line is not one JSON object: %q: %v", raw, err)
		}
		lines = append(lines, l)
	}
	return lines
}

func TestMonitorStreamsNDJSON(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "2"},
		nil, failingDoer(), dialFrames(tickerFrame("100"), tickerFrame("200"), tickerFrame("300")))
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	lines := parseMonitorLines(t, out)
	if len(lines) < 3 {
		t.Fatalf("expected a notice + 2 data lines, got %d: %q", len(lines), out)
	}
	if lines[0].Type != "notice" || lines[0].Code != "CONNECTED" {
		t.Errorf("first line should be the CONNECTED notice, got %+v", lines[0])
	}
	var data []monitorLine
	for _, l := range lines {
		if l.Type == "data" {
			data = append(data, l)
		}
	}
	if len(data) != 2 {
		t.Fatalf("expected exactly 2 data lines (max-events), got %d", len(data))
	}
	if data[0].Channel != "ticker" || data[0].Symbol != "btc_krw" || data[0].Origin != "realtime" {
		t.Errorf("data line metadata wrong: %+v", data[0])
	}
	if !strings.Contains(string(data[0].Payload), `"close":"100"`) {
		t.Errorf("payload not verbatim: %s", data[0].Payload)
	}
}

func TestMonitorWhereFilters(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1",
			"--where", `Number(payload.data.close) > 150`},
		nil, failingDoer(), dialFrames(tickerFrame("100"), tickerFrame("200"), tickerFrame("300")))
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var data []monitorLine
	for _, l := range parseMonitorLines(t, out) {
		if l.Type == "data" {
			data = append(data, l)
		}
	}
	if len(data) != 1 {
		t.Fatalf("expected exactly the first matching event, got %d data lines", len(data))
	}
	if !strings.Contains(string(data[0].Payload), `"close":"200"`) {
		t.Errorf("wrong event passed the filter: %s", data[0].Payload)
	}
}

func TestMonitorWhereExceptionSkipsEvent(t *testing.T) {
	// The first frame's payload lacks the path the predicate reads through
	// (data.deep is undefined -> TypeError); the event is skipped and the stream
	// continues. The skip is logged at Error — a predicate silently dropping
	// events is a bug the operator must see, and the run otherwise exits 0 with no
	// program-output equivalent — so it shows at the DEFAULT level (no flag).
	bad := `{"type":"ticker","timestamp":1700000000000,"symbol":"btc_krw","data":{"close":"1"}}`
	good := `{"type":"ticker","timestamp":1700000000000,"symbol":"btc_krw","data":{"close":"2","deep":{"v":5}}}`
	out, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1",
			"--where", `payload.data.deep.v === 5`},
		nil, failingDoer(), dialFrames(bad, good))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var data []monitorLine
	for _, l := range parseMonitorLines(t, out) {
		if l.Type == "data" {
			data = append(data, l)
		}
	}
	if len(data) != 1 || !strings.Contains(string(data[0].Payload), `"close":"2"`) {
		t.Fatalf("expected only the second event, got %v", data)
	}
	if !strings.Contains(errb, "--where threw") {
		t.Errorf("expected a predicate warning on stderr, got %q", errb)
	}
}

func TestMonitorJqFilters(t *testing.T) {
	// select(...) keeps only matching events; a passed event is re-emitted
	// verbatim (exact money string preserved). --jq needs no --enable-experimental.
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--max-events", "1",
			"--jq", `select(.type=="data" and (.payload.data.close|tonumber) > 150)`},
		nil, failingDoer(), dialFrames(tickerFrame("100"), tickerFrame("200"), tickerFrame("300")))
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var data []monitorLine
	for _, l := range parseMonitorLines(t, out) {
		if l.Type == "data" {
			data = append(data, l)
		}
	}
	if len(data) != 1 {
		t.Fatalf("expected exactly the first matching event, got %d data lines: %q", len(data), out)
	}
	if !strings.Contains(string(data[0].Payload), `"close":"200"`) {
		t.Errorf("wrong event passed the filter / payload not verbatim: %s", data[0].Payload)
	}
}

func TestMonitorJqFilterDropsNotices(t *testing.T) {
	// Notices flow through --jq (unlike the experimental --where), so a
	// data-only filter must NOT emit the CONNECTED (or any) notice line.
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--max-events", "1",
			"--jq", `select(.type=="data" and .channel=="ticker")`},
		nil, failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var data int
	for _, l := range parseMonitorLines(t, out) {
		if l.Type == "notice" {
			t.Errorf("a notice leaked through the data-only --jq filter: %+v", l)
		}
		if l.Type == "data" {
			data++
		}
	}
	if data != 1 {
		t.Fatalf("expected exactly 1 ticker data line, got %d: %q", data, out)
	}
}

func TestMonitorJqTransformAndImpliesJSON(t *testing.T) {
	// A reshaping program replaces the emitted line; --jq implies JSON output
	// (no --json given here). The money string is carried through exactly. The
	// select guards against the transform also running on the CONNECTED notice.
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--max-events", "1",
			"--jq", `select(.type=="data") | {px: .payload.data.close}`},
		nil, failingDoer(), dialFrames(tickerFrame("139000000.12345678")))
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var transformed map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(raw, `"px"`) {
			if err := json.Unmarshal([]byte(raw), &transformed); err != nil {
				t.Fatalf("transformed line is not JSON: %q: %v", raw, err)
			}
		}
	}
	if got := transformed["px"]; got != "139000000.12345678" {
		t.Errorf("transformed px = %v (%T), want exact string", got, got)
	}
}

func TestMonitorJqMutualExclusion(t *testing.T) {
	// --jq cannot be combined with the JavaScript runtime flags.
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--enable-experimental",
			"--jq", `.`, "--where", `true`},
		nil, failingDoer(), dialFrames(tickerFrame("100")))
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (usage error); stderr=%q", code, errb)
	}
	if !strings.Contains(errb, "cannot be combined") {
		t.Errorf("expected a mutual-exclusion usage error, got %q", errb)
	}
}

func TestMonitorJqCompileError(t *testing.T) {
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--jq", `select(`},
		nil, failingDoer(), dialFrames(tickerFrame("100")))
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (usage error); stderr=%q", code, errb)
	}
	if !strings.Contains(errb, "not a valid jq program") {
		t.Errorf("expected a jq compile error, got %q", errb)
	}
}

func TestMonitorDurationStops(t *testing.T) {
	start := time.Now()
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--duration", "150ms"},
		nil, failingDoer(), dialFrames())
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("duration did not stop the stream promptly: %v", elapsed)
	}
	for _, l := range parseMonitorLines(t, out) {
		if l.Type == "data" {
			t.Fatalf("no data lines expected, got %q", out)
		}
	}
}

func TestMonitorHumanLines(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--max-events", "1"},
		nil, failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "CONNECTED") {
		t.Errorf("human output missing the CONNECTED notice: %q", out)
	}
	if !strings.Contains(out, "ticker") || !strings.Contains(out, `"close":"100"`) {
		t.Errorf("human output missing the data line: %q", out)
	}
	if strings.Contains(out, `"type":"data"`) {
		t.Errorf("human mode should not emit the JSON envelope: %q", out)
	}
}

func TestMonitorDryRunPlan(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw,eth_krw", "--ticker", "--trades",
			"--base-url", "http://127.0.0.1:9999", "--dry-run", "--compact"},
		nil, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var plan struct {
		DryRun        bool   `json:"dryRun"`
		PublicURL     string `json:"wsPublicUrl"`
		PrivateURL    string `json:"wsPrivateUrl"`
		RESTBaseURL   string `json:"restBaseUrl"`
		Auth          bool   `json:"auth"`
		Backfill      bool   `json:"backfill"`
		Subscriptions []struct {
			Channel string   `json:"channel"`
			Symbols []string `json:"symbols"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan json: %v (%q)", err, out)
	}
	if !plan.DryRun || plan.Auth || !plan.Backfill {
		t.Errorf("plan flags wrong: %+v", plan)
	}
	if plan.PublicURL != "ws://127.0.0.1:9999/v2/public" {
		t.Errorf("sandbox ws derivation wrong: %q", plan.PublicURL)
	}
	if plan.PrivateURL != "" {
		t.Errorf("public-only plan should omit the private URL, got %q", plan.PrivateURL)
	}
	if plan.RESTBaseURL != "http://127.0.0.1:9999" {
		t.Errorf("rest base wrong: %q", plan.RESTBaseURL)
	}
	if len(plan.Subscriptions) != 2 || plan.Subscriptions[0].Channel != "ticker" ||
		len(plan.Subscriptions[0].Symbols) != 2 || plan.Subscriptions[1].Channel != "trade" {
		t.Errorf("subscriptions wrong: %+v", plan.Subscriptions)
	}
}

// TestMonitorAccountSeqsPlan asserts --account-seq attaches to every private
// subscription (orders/trades/assets) and leaves public ones (ticker) untouched.
func TestMonitorAccountSeqsPlan(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--my-orders", "--my-assets",
			"--account-seq", "1,2", "--base-url", "http://127.0.0.1:9999", "--dry-run", "--compact"},
		nil, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var plan struct {
		Subscriptions []struct {
			Channel     string   `json:"channel"`
			Symbols     []string `json:"symbols"`
			AccountSeqs []int    `json:"accountSeqs"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan json: %v (%q)", err, out)
	}
	for _, s := range plan.Subscriptions {
		switch s.Channel {
		case "ticker":
			if len(s.AccountSeqs) != 0 {
				t.Errorf("public ticker must not carry accountSeqs: %+v", s)
			}
		case "myOrder", "myAsset":
			if len(s.AccountSeqs) != 2 || s.AccountSeqs[0] != 1 || s.AccountSeqs[1] != 2 {
				t.Errorf("private %s must carry accountSeqs [1 2]: %+v", s.Channel, s)
			}
		}
	}
}

func TestMonitorDefaultAccountSeqsPlan(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--my-orders", "--my-assets",
			"--base-url", "http://127.0.0.1:9999", "--dry-run", "--compact"},
		nil, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var plan struct {
		Subscriptions []struct {
			Channel     string `json:"channel"`
			AccountSeqs []int  `json:"accountSeqs"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan json: %v (%q)", err, out)
	}
	for _, s := range plan.Subscriptions {
		if s.Channel != "myOrder" && s.Channel != "myAsset" {
			continue
		}
		if len(s.AccountSeqs) != 1 || s.AccountSeqs[0] != 1 {
			t.Fatalf("private %s must default to AccountSeqs [1]: %+v", s.Channel, s)
		}
	}
}

func TestMonitorDryRunProductionWSDefaults(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--dry-run", "--compact"},
		nil, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, `"wsPublicUrl":"wss://ws-api.korbit.co.kr/v2/public"`) {
		t.Errorf("production should use the default WS host: %q", out)
	}
}

// TestMonitorWSBaseURLResolution covers the WebSocket-URL precedence and the
// derive-from-REST fallback for the layers that don't need a signing key.
func TestMonitorWSBaseURLResolution(t *testing.T) {
	const wantDerived = `"wsPublicUrl":"wss://ws-api-test.korbit.co.kr/v2/public"`
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "derived from --base-url",
			args: []string{"--base-url", "https://api-test.korbit.co.kr"},
			want: wantDerived,
		},
		{
			name: "--ws-base-url overrides the derivation",
			args: []string{"--base-url", "https://api-test.korbit.co.kr", "--ws-base-url", "wss://stream.example.test"},
			want: `"wsPublicUrl":"wss://stream.example.test/v2/public"`,
		},
		{
			name: "KORBIT_CLI_WS_BASE_URL env",
			args: []string{"--base-url", "https://api-test.korbit.co.kr"},
			env:  map[string]string{"KORBIT_CLI_WS_BASE_URL": "wss://env-stream.example.test"},
			want: `"wsPublicUrl":"wss://env-stream.example.test/v2/public"`,
		},
		{
			name: "derived from KORBIT_CLI_BASE_URL env",
			env:  map[string]string{"KORBIT_CLI_BASE_URL": "https://apiz.korbit.com"},
			want: `"wsPublicUrl":"wss://ws-api.korbit.com/v2/public"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"monitor", "--symbols", "btc_krw", "--ticker", "--dry-run", "--compact"}, tc.args...)
			out, _, code := runMonitorCLI(args, tc.env, nil, nil)
			if code != 0 {
				t.Fatalf("exit=%d out=%q", code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("want %s in: %s", tc.want, out)
			}
		})
	}
}

// TestMonitorWSBaseURLFromConfig pins that config.json's wsBaseUrl is honored
// when the REST base also comes from config (no higher override).
func TestMonitorWSBaseURLFromConfig(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"baseUrl":"https://api-test.korbit.co.kr","wsBaseUrl":"wss://cfg-stream.example.test"}`)
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--dry-run", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if !strings.Contains(out, `"wsPublicUrl":"wss://cfg-stream.example.test/v2/public"`) {
		t.Errorf("config wsBaseUrl not used: %s", out)
	}
}

// TestMonitorWSBaseURLFromKey covers the per-key tier: a key's stored REST
// baseUrl drives a derived WS URL, and an explicitly stored WS URL is used
// verbatim. The per-key tier is consulted because scripting (--where) is active.
func TestMonitorWSBaseURLFromKey(t *testing.T) {
	t.Run("derived from per-key baseUrl", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		setKeyBaseURL(t, home, "bot", "https://api-test.korbit.co.kr") // empty ws → derived
		out, _, code := runMonitorCLI(
			[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--where", "true", "--key", "bot", "--dry-run", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, nil, nil)
		if code != 0 {
			t.Fatalf("exit=%d out=%q", code, out)
		}
		if !strings.Contains(out, `"wsPublicUrl":"wss://ws-api-test.korbit.co.kr/v2/public"`) {
			t.Errorf("per-key derivation wrong: %s", out)
		}
	})
	t.Run("explicit per-key wsBaseUrl", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		m := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
		if err := m.SetBaseURL("bot", "https://api-test.korbit.co.kr", "wss://key-stream.example.test"); err != nil {
			t.Fatal(err)
		}
		out, _, code := runMonitorCLI(
			[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--where", "true", "--key", "bot", "--dry-run", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, nil, nil)
		if code != 0 {
			t.Fatalf("exit=%d out=%q", code, out)
		}
		if !strings.Contains(out, `"wsPublicUrl":"wss://key-stream.example.test/v2/public"`) {
			t.Errorf("explicit per-key wsBaseUrl not used: %s", out)
		}
	})
}

func TestMonitorUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no channels", []string{"monitor"}, "at least one channel"},
		{"symbols missing", []string{"monitor", "--ticker"}, "--symbols is required"},
		{"symbols useless with only my-assets", []string{"monitor", "--symbols", "btc_krw", "--my-assets"}, "no effect"},
		{"init without where", []string{"monitor", "--symbols", "btc_krw", "--ticker", "--init", "var x=1"}, "--init has no effect"},
		{"stateful without script", []string{"monitor", "--symbols", "btc_krw", "--ticker", "--stateful"}, "--stateful needs a script"},
		{"stateful with jq", []string{"monitor", "--symbols", "btc_krw", "--ticker", "--jq", ".", "--stateful"}, "cannot be combined"},
		{"where syntax error", []string{"monitor", "--symbols", "btc_krw", "--ticker", "--where", "1 +"}, "--where"},
		{"bad symbols", []string{"monitor", "--symbols", "BTC KRW", "--ticker"}, "--symbols"},
		{"positional arg", []string{"monitor", "btc_krw"}, "unexpected argument"},
		{"account-seq without private channel", []string{"monitor", "--symbols", "btc_krw", "--ticker", "--account-seq", "1,2"}, "applies to the private channels"},
		{"account-seq not positive", []string{"monitor", "--symbols", "btc_krw", "--my-orders", "--account-seq", "0"}, "positive sub-account number"},
		{"account-seq duplicate", []string{"monitor", "--symbols", "btc_krw", "--my-orders", "--account-seq", "1,1"}, "listed more than once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, errb, code := runMonitorCLI(tc.args, nil, nil, nil)
			if code != 2 {
				t.Fatalf("exit=%d, want 2 (stderr %q)", code, errb)
			}
			if !strings.Contains(errb, tc.want) {
				t.Errorf("stderr %q missing %q", errb, tc.want)
			}
		})
	}
}

// TestMonitorExperimentalGate asserts the JavaScript bot runtime is refused
// (usage error, exit 2) unless experimental features are enabled. Each gated
// flag must trip the gate on its own; plain streaming flags must not.
func TestMonitorExperimentalGate(t *testing.T) {
	gated := [][]string{
		{"monitor", "--symbols", "btc_krw", "--ticker", "--where", "true"},
		{"monitor", "--symbols", "btc_krw", "--ticker", "--on", "1"},
		{"monitor", "--symbols", "btc_krw", "--ticker", "--init", "var x=1"},
		{"monitor", "--symbols", "btc_krw", "--ticker", "--db", "/tmp/x.db"},
		{"monitor", "--symbols", "btc_krw", "--ticker", "--max-concurrency", "4"},
		{"monitor", "--symbols", "btc_krw", "--ticker", "--stateful"},
	}
	for _, args := range gated {
		// Disable the helper's default opt-in so the gate is in force.
		_, errb, code := runMonitorCLI(args, map[string]string{"KORBIT_CLI_ENABLE_EXPERIMENTAL": ""}, nil, nil)
		if code != 2 {
			t.Fatalf("%v: exit=%d, want 2 (stderr %q)", args, code, errb)
		}
		if !strings.Contains(errb, "experimental") || !strings.Contains(errb, "--enable-experimental") {
			t.Errorf("%v: stderr %q should name the experimental gate and how to enable it", args, errb)
		}
	}

	// Plain streaming must NOT trip the gate even with experimental off — this
	// pins the "only the JS flags are gated" property so a plain flag can't be
	// added to the gate list by accident. --dry-run keeps it from dialing.
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--max-events", "1", "--dry-run", "--compact"},
		map[string]string{"KORBIT_CLI_ENABLE_EXPERIMENTAL": ""}, nil, nil)
	if code != 0 || strings.Contains(errb, "experimental") {
		t.Fatalf("plain streaming must not require --enable-experimental, got exit=%d stderr=%q", code, errb)
	}

	// The same scripting flag is accepted once experimental is enabled (here it
	// reaches the --where compile step rather than the gate); a syntax error
	// proves we got past the gate into the runtime.
	_, errb, code = runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--where", "1 +"},
		map[string]string{"KORBIT_CLI_ENABLE_EXPERIMENTAL": "1"}, nil, dialFrames())
	if code != 2 || strings.Contains(errb, "experimental") {
		t.Fatalf("with the gate open, --where should reach compilation, got exit=%d stderr=%q", code, errb)
	}
}

func TestMonitorUpgradeRejectionExits3(t *testing.T) {
	// A 4xx handshake rejection carrying a Korbit error envelope is fatal:
	// the session emits a FATAL notice and the command exits 3 with the
	// symbolic code preserved in the structured error.
	dial := func(context.Context, string, http.Header) (stream.Conn, error) {
		return nil, &stream.UpgradeError{Status: 403, Code: "FORBIDDEN", Body: `{"error":{"message":"FORBIDDEN"}}`}
	}
	out, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json"},
		nil, failingDoer(), dial)
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (stderr %q)", code, errb)
	}
	if !strings.Contains(out, `"code":"FATAL"`) {
		t.Errorf("expected an in-band FATAL notice, got %q", out)
	}
	var envelope struct {
		Error struct {
			Type       string `json:"type"`
			HTTPStatus int    `json:"httpStatus"`
			Code       string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(jsonTail(errb)), &envelope); err != nil {
		t.Fatalf("stderr error envelope: %v (%q)", err, errb)
	}
	if envelope.Error.Type != "api" || envelope.Error.HTTPStatus != 403 || envelope.Error.Code != "FORBIDDEN" {
		t.Errorf("structured error should carry the symbolic code, got %+v", envelope.Error)
	}
}

func TestMonitorDurationStopsRunawayPredicate(t *testing.T) {
	// With REST/DB I/O off the event loop, only a pure-CPU runaway can stall it,
	// and --duration (like Ctrl-C) interrupts the VM. A runaway --where broken by
	// --duration is a deliberate stop -> exit 0, with no data emitted.
	start := time.Now()
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--duration", "150ms",
			"--where", `(function () { for (;;) {} })()`},
		nil, failingDoer(), dialFrames(tickerFrame("100"), tickerFrame("200")))
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (deliberate stop)", code)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("--duration did not interrupt the runaway predicate promptly: %v", elapsed)
	}
	for _, l := range parseMonitorLines(t, out) {
		if l.Type == "data" {
			t.Fatalf("no data lines expected (the predicate never returns), got %q", out)
		}
	}
}

func TestMonitorPrivateRequiresKey(t *testing.T) {
	home := t.TempDir()
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--my-assets"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, nil)
	if code != 4 {
		t.Fatalf("exit=%d, want 4 (stderr %q)", code, errb)
	}
}

// routingDoer answers REST calls by method+path, so monitor --on/--init can
// drive the bot API. /v2/time always succeeds (the startup clock measure).
type routingDoer struct {
	mu     sync.Mutex
	routes map[string]func() *http.Response
	seen   []string
}

func newRoutingDoer() *routingDoer {
	return &routingDoer{routes: map[string]func() *http.Response{
		"GET /v2/time": func() *http.Response { return resp(200, `{"success":true,"data":{"serverTime":1700000000000}}`, nil) },
	}}
}

func (d *routingDoer) on(method, path string, fn func() *http.Response) {
	d.routes[method+" "+path] = fn
}

func (d *routingDoer) Do(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	d.seen = append(d.seen, key)
	if fn, ok := d.routes[key]; ok {
		return fn(), nil
	}
	return resp(500, `{"success":false,"error":{"code":500,"message":"NO_ROUTE"}}`, nil), nil
}

func (d *routingDoer) count(method, path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.seen {
		if s == method+" "+path {
			n++
		}
	}
	return n
}

// TestMonitorOnPlacesOrder drives the flagship path: a public ticker stream, a
// --where gate, and a --on handler that places an order through the bot API.
// The order POST and its follow-up GET both fire, and the command exits 0
// after the single matching event.
func TestMonitorOnPlacesOrder(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := newRoutingDoer()
	doer.on("POST", "/v2/orders", func() *http.Response {
		return resp(200, `{"success":true,"data":{"orderId":987}}`, nil)
	})
	doer.on("GET", "/v2/orders", func() *http.Response {
		return resp(200, `{"success":true,"data":{"orderId":987,"clientOrderId":"x","symbol":"btc_krw","status":"open","price":"139000000","qty":"0.001"}}`, nil)
	})

	out, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--key", "bot", "--json",
			"--base-url", "https://api.example.test", "--max-events", "1",
			"--where", `Number(payload.data.close) < 140000000`,
			"--on", `if (ev.type === "data") { var o = await korbit.order.place({symbol:"btc_krw", side:"buy", orderType:"limit", price:"139000000", qty:"0.001"}); console.log("placed " + o.orderId) }`},
		map[string]string{"KORBIT_CLI_HOME": home},
		doer, dialFrames(tickerFrame("100"), tickerFrame("200")))
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if doer.count("POST", "/v2/orders") != 1 {
		t.Fatalf("expected exactly one order placement, got %d (seen %v)", doer.count("POST", "/v2/orders"), doer.seen)
	}
	if doer.count("GET", "/v2/orders") != 1 {
		t.Fatalf("expected the follow-up order fetch, got %d", doer.count("GET", "/v2/orders"))
	}
	if !strings.Contains(errb, "placed 987") {
		t.Errorf("console.log should reach stderr: %q", errb)
	}
	if !strings.Contains(errb, `signing as key "bot"`) {
		t.Errorf("eager creds: expected the signing note even for a public-only subscription: %q", errb)
	}
	// The journal recorded the order placement (a write).
	logsOut, _, logsCode := runCLI([]string{"logs", "--orders", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, newRoutingDoer())
	if logsCode != 0 {
		t.Fatalf("logs exit=%d", logsCode)
	}
	if !strings.Contains(logsOut, `"orderId":"987"`) || !strings.Contains(logsOut, `"status":"accepted"`) {
		t.Errorf("the placed order should be journaled as accepted: %q", logsOut)
	}
	_ = out
}

// TestMonitorOnUnhandledApiErrorExits3 — an unhandled API rejection in --on is
// fatal and maps to exit 3 with the symbolic code preserved.
func TestMonitorOnUnhandledApiErrorExits3(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := newRoutingDoer()
	doer.on("GET", "/v2/balance", func() *http.Response {
		return resp(403, `{"success":false,"error":{"code":403,"message":"FORBIDDEN"}}`, nil)
	})
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--key", "bot", "--json",
			"--base-url", "https://api.example.test",
			"--on", `await korbit.balance()`},
		map[string]string{"KORBIT_CLI_HOME": home},
		doer, dialFrames(tickerFrame("100")))
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (stderr %q)", code, errb)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(jsonTail(errb)), &envelope); err != nil {
		t.Fatalf("stderr error envelope: %v (%q)", err, errb)
	}
	if envelope.Error.Code != "FORBIDDEN" {
		t.Errorf("expected the symbolic code preserved, got %q", envelope.Error.Code)
	}
}

// TestMonitorOnGenericErrorExits1 — a non-API unhandled --on error exits 1.
func TestMonitorOnGenericErrorExits1(t *testing.T) {
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--on", `throw new Error("boom")`},
		nil, failingDoer(), dialFrames(tickerFrame("100")))
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr %q)", code, errb)
	}
}

// TestMonitorOnSeesNotices — the handler runs for notices too, with ev.type
// 'notice'; --where never sees them.
func TestMonitorOnSeesNotices(t *testing.T) {
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--duration", "300ms",
			"--on", `if (ev.type === "notice" && payload.code === "CONNECTED") console.error("got-connected")`},
		nil, failingDoer(), dialFrames())
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if !strings.Contains(errb, "got-connected") {
		t.Errorf("the handler should have seen the CONNECTED notice: %q", errb)
	}
}

// TestMonitorStatefulExposesState — --stateful feeds the materialized state.*
// read-model from the same stream (no second connection). By the time --on runs
// for a data event, Ingest has already applied it, so state.ticker reflects it.
func TestMonitorStatefulExposesState(t *testing.T) {
	out, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1", "--stateful",
			"--on", `if (ev.type === "data") {
				var t = state.ticker("btc_krw");
				if (!t || t.close !== "100") throw new Error("state.ticker wrong: " + JSON.stringify(t));
				if (!state.ready("btc_krw").ticker) throw new Error("ticker should be ready");
				console.error("state-close=" + t.close);
			}`},
		nil, failingDoer(), dialFrames(tickerFrame("100"), tickerFrame("200")))
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if !strings.Contains(errb, "state-close=100") {
		t.Errorf("the handler should have read the materialized ticker: %q", errb)
	}
	_ = out
}

// TestMonitorStatefulThrowsWithoutFlag — without --stateful, state.* throws, and
// the unhandled --on error is fatal (exit 1 for a generic error).
func TestMonitorStatefulThrowsWithoutFlag(t *testing.T) {
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--max-events", "1",
			"--on", `if (ev.type === "data") state.openOrders()`},
		nil, failingDoer(), dialFrames(tickerFrame("100")))
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr %q)", code, errb)
	}
	if !strings.Contains(errb, "requires --stateful") {
		t.Errorf("expected the disabled-state throw, got %q", errb)
	}
}

// TestMonitorDBPersistsAcrossEvents — the script's db.* surface round-trips,
// using a custom --db path.
func TestMonitorDBPersistsAcrossEvents(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "mybot.db")
	out, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--db", dbPath, "--json", "--max-events", "2",
			"--init", `await db.exec("CREATE TABLE IF NOT EXISTS t (n INTEGER)")`,
			"--on", `if (ev.type === "data") { await db.exec("INSERT INTO t (n) VALUES (?)", Number(payload.data.close)); var r = await db.get("SELECT COUNT(*) AS c FROM t"); console.error("count=" + r.c) }`},
		map[string]string{"KORBIT_CLI_HOME": home},
		failingDoer(), dialFrames(tickerFrame("100"), tickerFrame("200")))
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if !strings.Contains(errb, "count=1") || !strings.Contains(errb, "count=2") {
		t.Errorf("db inserts should accumulate across events: %q", errb)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("the --db file should exist at %s: %v", dbPath, err)
	}
	_ = out
}

// --- --candles: the synthesized candle channel ---

// candleDoer answers /v2/candles with one authoritative row (the still-open
// 1m bucket containing the fake clock's now, 1700000000000) and fails every
// other REST call (the clock measurement is best-effort).
func candleDoer() korbit.Doer {
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/v2/candles") {
			return resp(200, `{"success":true,"data":[
				{"timestamp":1699999980000,"open":"100","high":"100","low":"100","close":"100","volume":"1"}
			]}`, nil), nil
		}
		return resp(500, `{}`, nil), nil
	})
}

func tradeSnapshotFrame() string {
	return `{"type":"trade","timestamp":1700000000000,"symbol":"btc_krw","snapshot":true,"data":[{"tradeId":1,"timestamp":1699999990000,"price":"100","qty":"0.5","isBuyerTaker":true}]}`
}

func tradeLiveFrame() string {
	return `{"type":"trade","timestamp":1700000001000,"symbol":"btc_krw","snapshot":false,"data":[{"tradeId":2,"timestamp":1700000000000,"price":"105","qty":"0.25","isBuyerTaker":true}]}`
}

// TestMonitorCandlesUsageErrors covers the --candles flag surface validation.
func TestMonitorCandlesUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"symbols required", []string{"monitor", "--candles", "1"}, "--symbols is required"},
		{"alias rejected", []string{"monitor", "--symbols", "btc_krw", "--candles", "1m"}, "not a candle interval"},
		{"unknown interval", []string{"monitor", "--symbols", "btc_krw", "--candles", "2"}, "not a candle interval"},
		{"duplicate interval", []string{"monitor", "--symbols", "btc_krw", "--candles", "1,1"}, "listed more than once"},
		{"history without candles", []string{"monitor", "--symbols", "btc_krw", "--ticker", "--candle-history", "10"}, "applies to --candles"},
		{"no-backfill conflict", []string{"monitor", "--symbols", "btc_krw", "--candles", "1", "--no-backfill"}, "cannot be combined with --no-backfill"},
		{"history out of range", []string{"monitor", "--symbols", "btc_krw", "--candles", "1", "--candle-history", "9999"}, "--candle-history"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, errb, code := runMonitorCLI(tc.args, nil, nil, nil)
			if code != 2 {
				t.Fatalf("exit=%d, want 2 (stderr %q)", code, errb)
			}
			if !strings.Contains(errb, tc.want) {
				t.Errorf("stderr %q missing %q", errb, tc.want)
			}
		})
	}
}

// TestMonitorCandlesDryRunPlan asserts the plan lists the synthesized candle
// channel with its intervals and history, plus the implicit trade subscription
// that feeds it (the wire truth).
func TestMonitorCandlesDryRunPlan(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--candles", "1,60", "--candle-history", "100", "--dry-run", "--json"},
		nil, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	var plan struct {
		DryRun        bool `json:"dryRun"`
		Subscriptions []struct {
			Channel   string   `json:"channel"`
			Symbols   []string `json:"symbols"`
			Intervals []string `json:"intervals"`
			History   int      `json:"history"`
			Implicit  bool     `json:"implicit"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan is not JSON: %v\n%s", err, out)
	}
	var haveTrade, haveCandle bool
	for _, s := range plan.Subscriptions {
		switch s.Channel {
		case "trade":
			haveTrade = true
			if !s.Implicit {
				t.Errorf("candle-feeding trade sub should be marked implicit: %+v", s)
			}
		case "candle":
			haveCandle = true
			if len(s.Intervals) != 2 || s.Intervals[0] != "1" || s.Intervals[1] != "60" || s.History != 100 {
				t.Errorf("candle plan entry = %+v", s)
			}
		}
	}
	if !haveTrade || !haveCandle {
		t.Errorf("plan should list the implicit trade sub and the candle channel: %s", out)
	}
}

// TestMonitorCandlesStreams runs the full path: the trade snapshot kicks the
// REST seed, the seed emits the authoritative live bucket as a candle line,
// and the implicit trade subscription's own lines are suppressed. Only the
// snapshot frame is served — a live frame's arrival order vs the seed result
// is nondeterministic here (buffered-replay vs post-apply fold, both correct
// but different first lines); the replay semantics are pinned by the candles
// package's unit tests.
func TestMonitorCandlesStreams(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--candles", "1", "--json", "--max-events", "1"},
		nil, candleDoer(), dialFrames(tradeSnapshotFrame()))
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	lines := parseMonitorLines(t, out)
	var data []monitorLine
	for _, l := range lines {
		if l.Type == "data" {
			data = append(data, l)
		}
		if l.Type == "data" && l.Channel == "trade" {
			t.Errorf("implicit trade subscription leaked a trade line: %+v", l)
		}
	}
	sawCandleBackfill := false
	for _, raw := range strings.Split(out, "\n") {
		if strings.Contains(raw, `"code":"BACKFILL_START"`) && strings.Contains(raw, `"channel":"candle"`) {
			sawCandleBackfill = true
		}
	}
	if len(data) != 1 {
		t.Fatalf("want exactly 1 data line (max-events), got %d: %s", len(data), out)
	}
	c := data[0]
	if c.Channel != "candle" || c.Origin != "derived" || c.Symbol != "btc_krw" {
		t.Errorf("candle line metadata wrong: %+v", c)
	}
	var payload struct {
		Interval  string `json:"interval"`
		Timestamp int64  `json:"timestamp"`
		Close     string `json:"close"`
		Volume    string `json:"volume"`
		Final     bool   `json:"final"`
	}
	if err := json.Unmarshal(c.Payload, &payload); err != nil {
		t.Fatalf("candle payload: %v", err)
	}
	if payload.Interval != "1" || payload.Timestamp != 1699999980000 || payload.Close != "100" || payload.Volume != "1" || payload.Final {
		t.Errorf("candle payload = %+v, want the authoritative live bucket (final:false)", payload)
	}
	if !sawCandleBackfill {
		t.Errorf("no BACKFILL_START notice for the candle seed in: %s", out)
	}
}

// TestMonitorCandlesWithExplicitTrades asserts --trades alongside --candles
// keeps the raw trade lines flowing.
func TestMonitorCandlesWithExplicitTrades(t *testing.T) {
	out, _, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--candles", "1", "--trades", "--json", "--max-events", "3"},
		nil, candleDoer(), dialFrames(tradeSnapshotFrame(), tradeLiveFrame()))
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	haveTrade, haveCandle := false, false
	for _, l := range parseMonitorLines(t, out) {
		if l.Type != "data" {
			continue
		}
		switch l.Channel {
		case "trade":
			haveTrade = true
		case "candle":
			haveCandle = true
		}
	}
	if !haveTrade || !haveCandle {
		t.Errorf("want both trade and candle lines with --trades --candles, got trade=%v candle=%v: %s", haveTrade, haveCandle, out)
	}
}
