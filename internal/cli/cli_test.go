// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/cli"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/version"
)

type stubDoer struct {
	resp *http.Response
	err  error
	last *http.Request
	// paths records every request path in order, for a command that makes more
	// than one call (the place dry-run's market-data preflight reads the
	// orderbook, the tick-size policy, and the pair listing).
	paths  []string
	signed bool // any request carried an api key
	body   string
}

func (s *stubDoer) Do(r *http.Request) (*http.Response, error) {
	s.last = r
	s.paths = append(s.paths, r.URL.Path)
	s.signed = s.signed || r.Header.Get("x-kapi-key") != ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		s.body = string(b)
	}
	return s.resp, s.err
}

// jsonTail returns stderr from its first '{' — skipping any diagnostic note
// lines that precede the structured error object.
func jsonTail(s string) string {
	if i := strings.IndexByte(s, '{'); i >= 0 {
		return s[i:]
	}
	return s
}

func resp(status int, body string, header map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range header {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h}
}

// sharedTestHome is a process-wide temp CLI home, used to keep tests that pass a
// nil env (mostly public endpoints) from journaling into the developer's real
// ~/.korbit-cli. Tests that need their own keys override KORBIT_CLI_HOME.
var (
	sharedTestHomeOnce sync.Once
	sharedTestHomeDir  string
)

func sharedTestHome() string {
	sharedTestHomeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "korbit-cli-test-home-")
		if err != nil {
			panic(err)
		}
		sharedTestHomeDir = dir
	})
	return sharedTestHomeDir
}

func runCLI(args []string, env map[string]string, doer korbit.Doer) (string, string, int) {
	// Always provide a temp KORBIT_CLI_HOME so journaling never touches the real
	// user home; any explicit env entry from the caller still overrides it.
	merged := map[string]string{"KORBIT_CLI_HOME": sharedTestHome()}
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
		Sleep:  func(time.Duration) {}, // retries run instantly under test
	})
	return out.String(), errb.String(), code
}

// runCLIConfirm is runCLI with an injected `self uninstall` confirmer, so the
// interactive uninstall flow is exercisable in-process (it forces the
// interactive path on, bypassing the TTY gate, and never reads real stdin).
func runCLIConfirm(args []string, env map[string]string, doer korbit.Doer, confirm func(string, bool) (bool, error)) (string, string, int) {
	merged := map[string]string{"KORBIT_CLI_HOME": sharedTestHome()}
	for k, v := range env {
		merged[k] = v
	}
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:               func(k string) string { return merged[k] },
		Stdout:               &out,
		Stderr:               &errb,
		Doer:                 doer,
		Now:                  func() int64 { return 1700000000000 },
		Sleep:                func(time.Duration) {},
		SelfUninstallConfirm: confirm,
	})
	return out.String(), errb.String(), code
}

// runCLIInstallConfirm is runCLI with an injected `self install` PATH-wiring
// confirmer, so the install flow is exercisable in-process without reaching a
// real /dev/tty (which, run interactively, would prompt the developer).
func runCLIInstallConfirm(args []string, env map[string]string, doer korbit.Doer, confirm func(string, bool) (bool, error)) (string, string, int) {
	merged := map[string]string{"KORBIT_CLI_HOME": sharedTestHome()}
	for k, v := range env {
		merged[k] = v
	}
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:             func(k string) string { return merged[k] },
		Stdout:             &out,
		Stderr:             &errb,
		Doer:               doer,
		Now:                func() int64 { return 1700000000000 },
		Sleep:              func(time.Duration) {},
		SelfInstallConfirm: confirm,
	})
	return out.String(), errb.String(), code
}

// seedBoundKey creates a bound key "bot" in home and returns its public key.
func seedBoundKey(t *testing.T, home string) ed25519.PublicKey {
	t.Helper()
	m := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	nk, err := m.Add("bot", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("bot", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(nk.PublicKey))
	pub, _ := x509.ParsePKIXPublicKey(block.Bytes)
	return pub.(ed25519.PublicKey)
}

func TestPublicCommandUnwrapsEnvelope(t *testing.T) {
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"last":"100","symbol":"btc_krw"}}`, nil)}
	out, _, code := runCLI([]string{"ticker", "btc_krw", "--compact"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(out) != `{"last":"100","symbol":"btc_krw"}` {
		t.Fatalf("stdout = %q", out)
	}
	if doer.last.Method != "GET" || doer.last.URL.Path != "/v2/tickers" {
		t.Fatalf("request = %s %s", doer.last.Method, doer.last.URL.Path)
	}
	if doer.last.Header.Get("x-kapi-key") != "" {
		t.Fatalf("public request must not be signed")
	}
}

// TestDiagnosticsAreDebugGated pins the logging contract: operational logs below
// Warn (the Debug request-target line here) are suppressed in a normal
// invocation and surface only under --debug, while the result is on stdout and
// the always-shown "signing as key" safety disclosure is never level-gated.
func TestDiagnosticsAreDebugGated(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	newDoer := func() *stubDoer {
		return &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}
	}

	// Default invocation: the result and disclosure show; the Debug log is hidden.
	stdout, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, newDoer())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"available":"1000"`) {
		t.Fatalf("the result is program output and must appear on stdout: %q", stdout)
	}
	if !strings.Contains(stderr, `signing as key "bot"`) {
		t.Fatalf("the signing disclosure must show without --debug: %q", stderr)
	}
	if strings.Contains(stderr, "korbit-cli: debug:") {
		t.Fatalf("a Debug log must be suppressed without --debug, got stderr=%q", stderr)
	}

	// --debug lowers the threshold so the Debug request-target log appears.
	_, stderr, code = runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--debug"},
		map[string]string{"KORBIT_CLI_HOME": home}, newDoer())
	if code != 0 {
		t.Fatalf("debug exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "korbit-cli: debug: GET /v2/balance") {
		t.Fatalf("expected the Debug request-target log under --debug, got stderr=%q", stderr)
	}
}

// TestLogLevelControlsLevelSeparately pins that --log-level (and its env var)
// set the operational-log threshold independently of --debug: it can raise
// verbosity without --debug, and it overrides the --debug-derived level when
// both are set. The always-shown "signing as key" disclosure is never gated.
func TestLogLevelControlsLevelSeparately(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	newDoer := func() *stubDoer {
		return &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}
	}
	const debugLine = "korbit-cli: debug: GET /v2/balance"

	// --log-level debug raises verbosity WITHOUT --debug: the Debug log appears.
	_, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--log-level", "debug"},
		map[string]string{"KORBIT_CLI_HOME": home}, newDoer())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, debugLine) {
		t.Fatalf("--log-level debug should show the Debug log without --debug, got %q", stderr)
	}

	// --debug --log-level off: log-level wins, so the Debug log is suppressed,
	// but the signing disclosure still shows.
	_, stderr, code = runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--debug", "--log-level", "off"},
		map[string]string{"KORBIT_CLI_HOME": home}, newDoer())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stderr, "korbit-cli: debug:") {
		t.Fatalf("--log-level off must override --debug and suppress logs, got %q", stderr)
	}
	if !strings.Contains(stderr, `signing as key "bot"`) {
		t.Fatalf("the signing disclosure must show regardless of log level, got %q", stderr)
	}

	// The env var resolves the same way as the flag (and the flag wins over it).
	_, stderr, code = runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--debug"},
		map[string]string{"KORBIT_CLI_HOME": home, "KORBIT_CLI_LOG_LEVEL": "off"}, newDoer())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stderr, "korbit-cli: debug:") {
		t.Fatalf("KORBIT_CLI_LOG_LEVEL=off must override --debug, got %q", stderr)
	}

	// An unrecognized level is a clean usage error (exit 2) before anything runs.
	_, stderr, code = runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--log-level", "loud"},
		map[string]string{"KORBIT_CLI_HOME": home}, newDoer())
	if code != 2 {
		t.Fatalf("bad --log-level should exit 2, got %d (stderr=%q)", code, stderr)
	}
}

// TestLogFileRedirectsOperationalLogs pins that --log-file diverts operational
// logs to a file while the signing disclosure stays on stderr.
func TestLogFileRedirectsOperationalLogs(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	logPath := filepath.Join(t.TempDir(), "korbit.log")
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}

	_, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--debug", "--log-file", logPath},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stderr, "korbit-cli: debug:") {
		t.Fatalf("operational logs should go to the file, not stderr, got stderr=%q", stderr)
	}
	if !strings.Contains(stderr, `signing as key "bot"`) {
		t.Fatalf("the signing disclosure must stay on stderr, got %q", stderr)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	// In the default text format the file trail drops the "korbit-cli: " tag and
	// stamps each line with a local RFC3339 timestamp, level, then the message.
	if strings.Contains(string(b), "korbit-cli:") {
		t.Fatalf("the --log-file text trail must drop the korbit-cli: tag, got %q", string(b))
	}
	fileLine := regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2} debug GET /v2/balance`)
	if !fileLine.MatchString(string(b)) {
		t.Fatalf("the Debug log should be in the log file, timestamped and untagged, got %q", string(b))
	}

	// An unwritable --log-file path is a clean usage error (exit 2).
	_, stderr, code = runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--log-file", filepath.Join(logPath, "nope")},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{resp: resp(200, `{"success":true,"data":{}}`, nil)})
	if code != 2 {
		t.Fatalf("an unopenable --log-file should exit 2, got %d (stderr=%q)", code, stderr)
	}
}

// TestLogFormatJSON pins that --log-format json emits one JSON object per
// operational record (time/level/msg, then attributes) with the level rendered
// in this CLI's vocabulary, on whichever sink logs go to, and that a bad format
// word is a clean usage error.
func TestLogFormatJSON(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	newDoer := func() *stubDoer {
		return &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}
	}

	// JSON logs on stderr: each operational record is a JSON object. (The
	// "signing as key" disclosure is a side-channel notification, not a log, and keeps its
	// own line — so only lines beginning with `{` are log records.)
	_, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--log-level", "debug", "--log-format", "json"},
		map[string]string{"KORBIT_CLI_HOME": home}, newDoer())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue // the signing disclosure, not a log record
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not valid JSON: %v (line=%q)", err, line)
		}
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "GET /v2/balance") {
			if rec["level"] != "debug" {
				t.Fatalf("level should render in this CLI's vocabulary, got %v", rec["level"])
			}
			if _, ok := rec["time"]; !ok {
				t.Fatalf("JSON record should carry a time field, got %q", line)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a JSON Debug record for GET /v2/balance, got %q", stderr)
	}

	// The env var resolves the same way as the flag.
	_, stderr, code = runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--log-level", "debug"},
		map[string]string{"KORBIT_CLI_HOME": home, "KORBIT_CLI_LOG_FORMAT": "json"}, newDoer())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !regexp.MustCompile(`(?m)^\{.*"msg":"GET /v2/balance`).MatchString(stderr) {
		t.Fatalf("KORBIT_CLI_LOG_FORMAT=json should emit JSON logs, got %q", stderr)
	}

	// An unrecognized format is a clean usage error (exit 2) before anything runs.
	_, stderr, code = runCLI(
		[]string{"balance", "--key", "bot", "--compact", "--log-format", "yaml"},
		map[string]string{"KORBIT_CLI_HOME": home}, newDoer())
	if code != 2 {
		t.Fatalf("bad --log-format should exit 2, got %d (stderr=%q)", code, stderr)
	}
}

func TestPrivateRequestSignsAndVerifies(t *testing.T) {
	home := t.TempDir()
	pub := seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}
	_, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home},
		doer,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if doer.last.Header.Get("x-kapi-key") != "KEYID-1" {
		t.Fatalf("missing/incorrect api key header")
	}
	if !strings.Contains(stderr, `signing as key "bot"`) {
		t.Fatalf("missing signing note: %q", stderr)
	}
	// Verify the signature over the exact sent query string (signature stripped).
	rawQuery := doer.last.URL.RawQuery
	idx := strings.LastIndex(rawQuery, "&signature=")
	if idx < 0 {
		t.Fatalf("no signature param in %q", rawQuery)
	}
	signed := rawQuery[:idx]
	sigEsc := rawQuery[idx+len("&signature="):]
	sigStr, _ := url.QueryUnescape(sigEsc)
	sig, _ := base64.StdEncoding.DecodeString(sigStr)
	if !ed25519.Verify(pub, []byte(signed), sig) {
		t.Fatalf("signature did not verify over sent bytes %q", signed)
	}
	if !strings.Contains(signed, "timestamp=1700000000000") {
		t.Fatalf("timestamp missing from signed string: %q", signed)
	}
	if !strings.Contains(signed, "accountSeq=1") {
		t.Fatalf("default accountSeq missing from signed string: %q", signed)
	}
}

func TestPrivateRequestHonorsExplicitAccountSeq(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}
	_, stderr, code := runCLI(
		[]string{"balance", "--account-seq", "2", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home},
		doer,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	rawQuery := doer.last.URL.RawQuery
	idx := strings.LastIndex(rawQuery, "&signature=")
	if idx < 0 {
		t.Fatalf("no signature param in %q", rawQuery)
	}
	if signed := rawQuery[:idx]; !strings.Contains(signed, "accountSeq=2") {
		t.Fatalf("explicit accountSeq missing from signed string: %q", signed)
	}
}

func TestOrderPlaceEchoesClientOrderID(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"orderId":123,"status":"open"}}`, nil)}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home},
		doer,
	)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("bad json: %v (%s)", err, out)
	}
	coid, ok := result["clientOrderId"].(string)
	if !ok || coid == "" {
		t.Fatalf("clientOrderId not echoed: %s", out)
	}
	// The same id must have been sent in the request body.
	if !strings.Contains(doer.body, "clientOrderId="+coid) {
		t.Fatalf("clientOrderId not sent in body: %s", doer.body)
	}
	if result["orderId"] == nil {
		t.Fatalf("API data fields should be preserved: %s", out)
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	doer := &stubDoer{resp: resp(422,
		`{"success":false,"error":{"code":422,"message":"DUPLICATE_CLIENT_ORDER_ID","description":"already placed"}}`, nil)}
	home := t.TempDir()
	seedBoundKey(t, home)
	_, stderr, code := runCLI(
		[]string{"order", "get", "--symbol", "btc_krw", "--order-id", "1", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 3 {
		t.Fatalf("exit=%d", code)
	}
	var doc struct {
		Error struct {
			Type       string `json:"type"`
			Code       string `json:"code"`
			HTTPStatus int    `json:"httpStatus"`
			Message    string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal([]byte(jsonTail(stderr)), &doc)
	if doc.Error.Type != "api" || doc.Error.Code != "DUPLICATE_CLIENT_ORDER_ID" || doc.Error.HTTPStatus != 422 {
		t.Fatalf("error envelope wrong: %s", stderr)
	}
	if doc.Error.Message != "already placed" {
		t.Fatalf("message should prefer description: %q", doc.Error.Message)
	}
}

func TestRetryAfterSurfaced(t *testing.T) {
	doer := &stubDoer{resp: resp(429,
		`{"success":false,"error":{"code":429,"message":"TOO_MANY_REQUESTS"}}`,
		map[string]string{"Retry-After": "30"})}
	_, stderr, code := runCLI([]string{"ticker", "btc_krw", "--compact"}, nil, doer)
	if code != 3 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stderr, `"retryAfterSec":30`) {
		t.Fatalf("retryAfterSec missing: %s", stderr)
	}
}

// TestDryRunDoesNotSendOrder pins that --dry-run never SENDS the order (no signed
// POST) and works without credentials. For order place it now makes a best-effort
// PUBLIC market-data call for the customer-protection checks; when that data is
// unreachable the plan is still emitted with a skip note (graceful degradation).
func TestDryRunDoesNotSendOrder(t *testing.T) {
	doer := &stubDoer{err: io.ErrUnexpectedEOF} // market-data fetch fails -> checks skipped
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "sell", "--type", "market",
			"--qty", "0.001", "--dry-run", "--compact"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	// The only call allowed is the public orderbook probe; it must never be the
	// signed order POST.
	if doer.last != nil {
		if doer.last.Method == "POST" || doer.last.Header.Get("x-kapi-key") != "" {
			t.Fatalf("dry-run must not sign or send the order: %s %s", doer.last.Method, doer.last.URL.Path)
		}
	}
	if !strings.Contains(out, `"dryRun":true`) {
		t.Fatalf("dry-run output wrong: %s", out)
	}
	// A dry-run does NOT mint a clientOrderId (a real send mints a fresh one, so
	// echoing one here would only mislead).
	if strings.Contains(out, `clientOrderId`) {
		t.Fatalf("dry-run must not auto-mint a clientOrderId: %s", out)
	}
	if !strings.Contains(out, `"accountSeq":"1"`) {
		t.Fatalf("dry-run must show the default accountSeq: %s", out)
	}
	if !strings.Contains(out, `"checksSkipped"`) {
		t.Fatalf("dry-run with unreachable market data should note the skipped checks: %s", out)
	}
}

// TestDryRunKeepsExplicitClientOrderID pins that an explicitly supplied
// --client-order-id still appears in the dry-run plan (only the auto-minted one
// is omitted).
func TestDryRunKeepsExplicitClientOrderID(t *testing.T) {
	doer := &stubDoer{err: io.ErrUnexpectedEOF}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "sell", "--type", "market",
			"--qty", "0.001", "--client-order-id", "019eabcf-7f2e-7587-979c-d67bde2b8967",
			"--dry-run", "--compact"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "019eabcf-7f2e-7587-979c-d67bde2b8967") {
		t.Fatalf("an explicit --client-order-id must show in the dry-run: %s", out)
	}
}

// TestNonPlaceDryRunStaysOffline pins that --dry-run on a non-place command makes
// no network call at all (the market-data preflight is order-place-only).
func TestNonPlaceDryRunStaysOffline(t *testing.T) {
	doer := &stubDoer{err: io.ErrUnexpectedEOF} // would fail if called
	out, _, code := runCLI([]string{"ticker", "btc_krw", "--dry-run", "--compact"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if doer.last != nil {
		t.Fatalf("a non-place dry-run must not send any request")
	}
	if !strings.Contains(out, `"dryRun":true`) {
		t.Fatalf("dry-run output wrong: %s", out)
	}
}

// TestPlaceDryRunWarnsOnSlippage pins the customer-protection preflight: a market
// buy that sweeps a thin book to a far-worse average price produces a
// HIGH_SLIPPAGE warning, fetched from the resolved (public) baseURL.
func TestPlaceDryRunWarnsOnSlippage(t *testing.T) {
	book := `{"success":true,"data":{"timestamp":1,"bids":[{"price":"9900","qty":"5"}],` +
		`"asks":[{"price":"10000","qty":"1"},{"price":"20000","qty":"5"}]}}`
	doer := &stubDoer{resp: resp(200, book, nil)}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "market",
			"--amt", "15000", "--dry-run", "--compact"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !slices.Contains(doer.paths, "/v2/orderbook") {
		t.Fatalf("expected an orderbook fetch, got: %v", doer.paths)
	}
	if doer.signed {
		t.Fatalf("every market-data fetch in the preflight must be public (unsigned): %v", doer.paths)
	}
	if !strings.Contains(out, "HIGH_SLIPPAGE") {
		t.Fatalf("expected a HIGH_SLIPPAGE warning: %s", out)
	}
}

// TestPlaceDryRunHumanRender pins the human-mode dry-run: the plan, the
// estimate-only SIMULATION block, and the warnings all render as text (never
// JSON).
func TestPlaceDryRunHumanRender(t *testing.T) {
	book := `{"success":true,"data":{"timestamp":1,"bids":[{"price":"9900","qty":"5"}],` +
		`"asks":[{"price":"10000","qty":"1"},{"price":"20000","qty":"5"}]}}`
	doer := &stubDoer{resp: resp(200, book, nil)}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "market",
			"--amt", "15000", "--dry-run"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(out, `"dryRun"`) || strings.Contains(out, `"simulation"`) {
		t.Fatalf("human mode must not emit JSON:\n%s", out)
	}
	// "outcome fills" rides beside "marketable": that this order EXECUTES is the
	// fact a reader acts on, and marketable alone does not say it.
	for _, want := range []string{"DRY RUN", "SIMULATION (estimate only", "est. avg fill price",
		"marketable", "outcome", "fills", "warning(s)", "HIGH_SLIPPAGE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("human dry-run missing %q:\n%s", want, out)
		}
	}
}

// TestPlaceDryRunJournalsPreflightOnlyInDebug pins that the preflight's public
// market-data reads follow the normal journaling policy: a normal dry-run opens
// no journal (so it works on a read-only home / before key setup), while a
// --debug dry-run journals them so they're available for troubleshooting via
// `korbit logs`.
func TestPlaceDryRunJournalsPreflightOnlyInDebug(t *testing.T) {
	book := `{"success":true,"data":{"timestamp":1,"bids":[{"price":"99000000","qty":"1"}],` +
		`"asks":[{"price":"100000000","qty":"1"}]}}`
	args := []string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
		"--price", "90000000", "--qty", "0.001", "--dry-run", "--compact"}

	// Normal dry-run: no journal DB is created.
	plain := t.TempDir()
	if _, _, code := runCLI(args, map[string]string{"KORBIT_CLI_HOME": plain}, &stubDoer{resp: resp(200, book, nil)}); code != 0 {
		t.Fatalf("plain dry-run exit=%d", code)
	}
	if _, err := os.Stat(journal.DefaultPath(plain)); !os.IsNotExist(err) {
		t.Fatalf("a normal dry-run must not open the journal: err=%v", err)
	}

	// --debug dry-run: the preflight orderbook read is journaled.
	dbg := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": dbg}
	if _, _, code := runCLI(append(append([]string{}, args...), "--debug"), env, &stubDoer{resp: resp(200, book, nil)}); code != 0 {
		t.Fatalf("debug dry-run exit=%d", code)
	}
	out, _, code := runCLI([]string{"logs", "--json"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("logs exit=%d", code)
	}
	if !strings.Contains(out, "/v2/orderbook") {
		t.Fatalf("a --debug dry-run should journal the preflight orderbook read: %s", out)
	}
}

func TestCatalogSurface(t *testing.T) {
	out, _, code := runCLI([]string{"commands", "--json"}, nil, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var cat struct {
		CatalogVersion int `json:"catalogVersion"`
		Commands       []struct {
			Command        string `json:"command"`
			ResponseFields []struct {
				Name string `json:"name"`
				Type string `json:"type"`
				Desc string `json:"desc"`
			} `json:"responseFields"`
		} `json:"commands"`
		GlobalFlags []struct {
			Flag string `json:"flag"`
		} `json:"globalFlags"`
	}
	if err := json.Unmarshal([]byte(out), &cat); err != nil {
		t.Fatalf("catalog json: %v", err)
	}
	if cat.CatalogVersion != 3 {
		t.Fatalf("catalogVersion: want 3 (order place returns the full reconciled order), got %d", cat.CatalogVersion)
	}
	// 65 endpoint+builtin commands, plus the 3 visible `self` builtins (update,
	// uninstall, doctor); `self install` is hidden and never in the catalog.
	if len(cat.Commands) != 68 {
		t.Fatalf("expected 68 commands, got %d", len(cat.Commands))
	}
	byName := map[string][]string{}
	found := false
	for _, c := range cat.Commands {
		if c.Command == "order place" {
			found = true
		}
		var fs []string
		for _, rf := range c.ResponseFields {
			fs = append(fs, rf.Name)
		}
		byName[c.Command] = fs
	}
	if !found {
		t.Fatalf("catalog missing `order place`")
	}
	// Endpoint commands publish response-shape hints; assert a couple of known
	// fields surface, and that a builtin has none.
	if got := byName["order get"]; !contains(got, "orderId") || !contains(got, "status") || !contains(got, "avgPrice") {
		t.Fatalf("`order get` responseFields missing expected fields: %v", got)
	}
	if got := byName["ticker"]; !contains(got, "symbol") || !contains(got, "close") {
		t.Fatalf("`ticker` responseFields missing expected fields: %v", got)
	}
	if got := byName["commands"]; len(got) != 0 {
		t.Fatalf("builtin `commands` must have no responseFields, got: %v", got)
	}
}

// TestCatalogExamplesAlwaysArray pins that every command's `examples` is a JSON
// array (never null) so machine consumers see a consistent type, and that
// `key set-base-url` carries examples for both the set and the --clear forms.
func TestCatalogExamplesAlwaysArray(t *testing.T) {
	out, _, code := runCLI([]string{"commands", "--json"}, nil, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var cat struct {
		Commands []struct {
			Command  string          `json:"command"`
			Examples json.RawMessage `json:"examples"`
		} `json:"commands"`
	}
	if err := json.Unmarshal([]byte(out), &cat); err != nil {
		t.Fatalf("catalog json: %v", err)
	}
	for _, c := range cat.Commands {
		if len(c.Examples) == 0 || string(c.Examples) == "null" {
			t.Fatalf("command %q has non-array examples: %q", c.Command, string(c.Examples))
		}
		if c.Examples[0] != '[' {
			t.Fatalf("command %q examples is not a JSON array: %q", c.Command, string(c.Examples))
		}
	}
	for _, c := range cat.Commands {
		if c.Command == "key set-base-url" {
			if !strings.Contains(string(c.Examples), "--clear") || !strings.Contains(string(c.Examples), "set-base-url") {
				t.Fatalf("`key set-base-url` examples missing set/--clear forms: %s", c.Examples)
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestOrderPlacePOSTBodyIsSigned(t *testing.T) {
	home := t.TempDir()
	pub := seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"orderId":1}}`, nil)}
	_, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "sell", "--type", "market",
			"--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	// POST signs over the form BODY. Verify the way the server does.
	body := doer.body
	idx := strings.LastIndex(body, "&signature=")
	if idx < 0 {
		t.Fatalf("no signature in POST body: %s", body)
	}
	signed := body[:idx]
	sig, _ := base64.StdEncoding.DecodeString(mustUnescape(body[idx+len("&signature="):]))
	if !ed25519.Verify(pub, []byte(signed), sig) {
		t.Fatalf("POST body signature did not verify over sent bytes")
	}
}

func mustUnescape(s string) string {
	u, _ := url.QueryUnescape(s)
	return u
}

func TestBaseURLResolution(t *testing.T) {
	dry := func(args []string, env map[string]string) (string, int) {
		out, _, code := runCLI(append([]string{"ticker", "btc_krw", "--dry-run", "--compact"}, args...), env, &stubDoer{})
		return out, code
	}
	// --base-url to a loopback host (the local mock-server pattern)
	if out, code := dry([]string{"--base-url", "http://127.0.0.1:9999"}, nil); code != 0 || !strings.Contains(out, `"baseUrl":"http://127.0.0.1:9999"`) {
		t.Fatalf("base-url loopback: %s (%d)", out, code)
	}
	// env
	if out, code := dry(nil, map[string]string{"KORBIT_CLI_BASE_URL": "https://env.example"}); code != 0 || !strings.Contains(out, `"baseUrl":"https://env.example"`) {
		t.Fatalf("env: %s (%d)", out, code)
	}
	// --base-url overrides env, and trailing slash trimmed
	if out, code := dry([]string{"--base-url", "https://flag.example/"}, map[string]string{"KORBIT_CLI_BASE_URL": "https://env.example"}); code != 0 || !strings.Contains(out, `"baseUrl":"https://flag.example"`) {
		t.Fatalf("flag override: %s (%d)", out, code)
	}
}

func TestPlaintextBaseURLRefusedWhenSigning(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{err: io.ErrUnexpectedEOF} // must not be called
	_, stderr, code := runCLI(
		[]string{"balance", "--key", "bot", "--base-url", "http://evil.example", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 2 {
		t.Fatalf("plaintext signed request should be refused (exit 2), got %d", code)
	}
	if doer.last != nil {
		t.Fatalf("must not send before refusing")
	}
	if !strings.Contains(stderr, "plaintext http") {
		t.Fatalf("unexpected error: %s", stderr)
	}
}

func TestTimeoutBounds(t *testing.T) {
	base := []string{"ticker", "btc_krw", "--dry-run", "--compact"}
	for _, tc := range []struct {
		flag, val string
	}{{"--timeout", "abc"}, {"--timeout", "0"}} {
		_, _, code := runCLI(append(append([]string{}, base...), tc.flag, tc.val), nil, &stubDoer{})
		if code != 2 {
			t.Errorf("%s %s should be exit 2, got %d", tc.flag, tc.val, code)
		}
	}
}

// TestNetBindingValidationAndInertness pins both halves of the --bind/--family
// wiring: a malformed value fails fast as a usage error (exit 2) before any
// request, and a valid value leaves an injected (test) Doer in place — the bind
// rebinds only the real-network defaults, so it is inert under a stub.
func TestNetBindingValidationAndInertness(t *testing.T) {
	// Malformed values map to exit 2.
	for _, tc := range [][]string{
		{"--family", "bogus"},
		{"--bind", "192.0.2.10", "--family", "ipv6"}, // family/literal conflict
		{"--bind", "if!nosuchif0"},                   // unknown interface
	} {
		args := append(append([]string{}, tc...), "ticker", "btc_krw", "--compact")
		if _, _, code := runCLI(args, nil, &stubDoer{}); code != 2 {
			t.Errorf("%v should be exit 2, got %d", tc, code)
		}
	}

	// A valid bind does not override the injected Doer: the stub still serves the
	// request and records it.
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"last":"100"}}`, nil)}
	_, stderr, code := runCLI([]string{"--bind", "127.0.0.1", "ticker", "btc_krw", "--compact"}, nil, doer)
	if code != 0 {
		t.Fatalf("valid --bind exit=%d stderr=%s", code, stderr)
	}
	if doer.last == nil || doer.last.URL.Path != "/v2/tickers" {
		t.Fatalf("injected Doer was bypassed by --bind; last=%v", doer.last)
	}
}

func TestUnknownCommandStructuredError(t *testing.T) {
	out, stderr, code := runCLI([]string{"tickr", "btc_krw"}, nil, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("stdout must stay clean on unknown command, got: %s", out)
	}
	// Default (no --json): a plain human error line, not the JSON envelope.
	if strings.Contains(stderr, "{") {
		t.Fatalf("human error must not be JSON: %s", stderr)
	}
	if !strings.Contains(stderr, "did you mean") || !strings.Contains(stderr, "ticker") {
		t.Fatalf("expected did-you-mean suggestion, got: %s", stderr)
	}
}

// TestVersionVerb pins that the bare `version` verb prints the version on stdout
// and exits 0, matching `--version` instead of being rejected as an unknown
// command.
func TestVersionVerb(t *testing.T) {
	out, stderr, code := runCLI([]string{"version"}, nil, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(out) != version.Version {
		t.Fatalf("expected version %q on stdout, got: %q", version.Version, out)
	}
}

// TestMistypedSubcommandWithFlags pins that a mistyped subcommand followed by a
// flag (e.g. `order plac --symbol btc_krw`) is reported as a did-you-mean usage
// error (exit 2), not as "unknown flag: --symbol" — the flag must not be parsed
// against the bare group.
func TestMistypedSubcommandWithFlags(t *testing.T) {
	out, stderr, code := runCLI([]string{"order", "plac", "--symbol", "btc_krw"}, nil, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("stdout must stay clean, got: %s", out)
	}
	if strings.Contains(stderr, "unknown flag") {
		t.Fatalf("must not report unknown flag for a mistyped subcommand: %s", stderr)
	}
	if !strings.Contains(stderr, "did you mean") || !strings.Contains(stderr, "order place") {
		t.Fatalf("expected did-you-mean to `order place`, got: %s", stderr)
	}
}

// TestCorrectSubcommandUnknownFlagStillReported pins that a correctly-spelled
// subcommand with a bogus flag still reports the leaf's unknown-flag usage error
// (exit 2) — the group's disabled flag parsing must not swallow real leaf flag
// errors.
func TestCorrectSubcommandUnknownFlagStillReported(t *testing.T) {
	_, stderr, code := runCLI([]string{"order", "place", "--bogus"}, nil, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Fatalf("expected leaf unknown-flag usage error, got: %s", stderr)
	}
}

// TestBareGroupPrintsHelp pins that a bare group (no subcommand) prints the
// focused group help on stdout and exits 2 (an incomplete command, shown
// helpfully — mirrors the bare-root behavior). A mistyped subcommand still
// errors (TestUnknownSubcommandSuggests / TestGroupAcceptsJSONFlag).
func TestBareGroupPrintsHelp(t *testing.T) {
	stdout, stderr, code := runCLI([]string{"order"}, nil, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("bare group must not emit an error, got stderr: %s", stderr)
	}
	if !strings.Contains(stdout, "order") || !strings.Contains(stdout, "Usage:") {
		t.Fatalf("bare group should print group help on stdout, got: %s", stdout)
	}
}

// End-to-end: an error is a plain "error: <message>" line on stderr by default,
// and the structured {"error":...} envelope under --json — same exit code, only
// the rendering differs. (stdout stays clean either way.) Uses a leaf-command
// usage error (`order place` with required flags missing), the path agents hit.
func TestErrorRenderingHonorsMode(t *testing.T) {
	// Default (human): a plain, untagged error line — not JSON.
	out, stderr, code := runCLI([]string{"order", "place"}, nil, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("stdout must stay clean, got: %s", out)
	}
	h := strings.TrimSpace(stderr)
	if !strings.HasPrefix(h, "error: ") || strings.Contains(h, "{") || strings.Contains(h, "korbit-cli") {
		t.Fatalf("default error must be a plain untagged line, got: %q", h)
	}

	// --json: the structured envelope, same exit code.
	_, stderrJSON, codeJSON := runCLI([]string{"order", "place", "--json"}, nil, &stubDoer{})
	if codeJSON != 2 {
		t.Fatalf("exit=%d under --json", codeJSON)
	}
	var doc struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(jsonTail(stderrJSON)), &doc); err != nil {
		t.Fatalf("--json error must be the JSON envelope: %s", stderrJSON)
	}
	if doc.Error.Type != "usage" {
		t.Fatalf("envelope type = %q, want usage", doc.Error.Type)
	}
}

// A group's error path (a mistyped subcommand) honors --json/--compact — its
// cobra flag parsing is disabled (for did-you-mean handling), so the group RunE
// reads the global output flags itself. `key bogus` emits the plain line; `key
// bogus --json`/`--compact` emit the structured envelope.
func TestGroupAcceptsJSONFlag(t *testing.T) {
	_, stderr, code := runCLI([]string{"key", "bogus"}, nil, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(stderr), "error: ") || strings.Contains(stderr, "{") {
		t.Fatalf("group error (human) must be a plain error line: %q", stderr)
	}

	_, sj, cj := runCLI([]string{"key", "bogus", "--json"}, nil, &stubDoer{})
	if cj != 2 {
		t.Fatalf("exit=%d", cj)
	}
	var doc struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(jsonTail(sj)), &doc); err != nil {
		t.Fatalf("`key bogus --json` must emit the JSON envelope: %s", sj)
	}
	if doc.Error.Type != "usage" || !strings.Contains(doc.Error.Message, "unknown subcommand") {
		t.Fatalf("unexpected envelope: %s", sj)
	}

	if _, sc, _ := runCLI([]string{"key", "bogus", "--compact"}, nil, &stubDoer{}); !strings.Contains(sc, `{"error"`) {
		t.Fatalf("`key bogus --compact` must emit JSON: %s", sc)
	}
}

// A global value-taking flag in `--flag value` (space) form before a bare group
// must not have its value misread as an attempted subcommand. The group disables
// cobra flag parsing, so its RunE scans args by hand and has to skip the value;
// otherwise `--key sandbox deposit` reported `unknown subcommand "deposit sandbox"`.
func TestGroupSkipsGlobalFlagValue(t *testing.T) {
	// Bare group reached past `--key sandbox`: focused group help, exit 2, and
	// crucially NOT a bogus "unknown subcommand" naming the flag value.
	out, _, code := runCLI([]string{"--key", "sandbox", "deposit"}, nil, &stubDoer{})
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if strings.Contains(out, "unknown subcommand") {
		t.Fatalf("flag value misread as subcommand: %q", out)
	}
	if !strings.Contains(out, "deposit") || !strings.Contains(out, "generate") {
		t.Fatalf("expected deposit group help: %q", out)
	}

	// The flag value is skipped, not swallowed — a genuine bad subcommand after
	// it is still reported (and names the real token, not the flag value).
	_, stderr, code := runCLI([]string{"--key", "sandbox", "deposit", "bogus"}, nil, &stubDoer{})
	if code != 2 || !strings.Contains(stderr, `unknown subcommand "deposit bogus"`) {
		t.Fatalf("genuine bad subcommand should still error: %q (%d)", stderr, code)
	}

	// Boundary cases that must not panic or misfire:
	//   - a value flag as the LAST token (the value-skip runs past the end);
	//   - the `--flag=value` single-token form (skipped as a dash-prefixed arg);
	//   - a flag value that itself looks like a flag (consumed as the value).
	for _, args := range [][]string{
		{"deposit", "--key"},                                // value flag is the final arg
		{"--key=sandbox", "deposit"},                        // =form
		{"--key", "--json", "deposit"},                      // value looks like a flag
		{"--base-url", "http://x", "--key", "k", "deposit"}, // two value flags
	} {
		out, _, code := runCLI(args, nil, &stubDoer{})
		if code != 2 || strings.Contains(out, "unknown subcommand") || !strings.Contains(out, "generate") {
			t.Fatalf("args %v: expected deposit group help, got %q (%d)", args, out, code)
		}
	}
}

func TestNetworkFailureIsExit1(t *testing.T) {
	doer := &stubDoer{err: io.ErrUnexpectedEOF}
	_, stderr, code := runCLI([]string{"ticker", "btc_krw", "--compact"}, nil, doer)
	if code != 1 {
		t.Fatalf("network failure must be exit 1 (retryable), got %d", code)
	}
	if !strings.Contains(stderr, `"type": "internal"`) && !strings.Contains(stderr, `"type":"internal"`) {
		t.Fatalf("expected internal error type: %s", stderr)
	}
}

func TestKeyUseRemoveThroughMain(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	runCLI([]string{"key", "add", "a", "--compact"}, env, &stubDoer{})
	runCLI([]string{"key", "add", "b", "--compact"}, env, &stubDoer{})

	if out, _, code := runCLI([]string{"key", "use", "b", "--compact"}, env, &stubDoer{}); code != 0 || !strings.Contains(out, `"defaultKey":"b"`) {
		t.Fatalf("key use: %s (%d)", out, code)
	}
	// Removing the default unsets it (no silent fallback) and carries the
	// follow-up in stdout.
	out, stderr, code := runCLI([]string{"key", "remove", "b", "--compact"}, env, &stubDoer{})
	if code != 0 || !strings.Contains(out, `"defaultKey":null`) {
		t.Fatalf("key remove: %s (%d)", out, code)
	}
	if !strings.Contains(out, "key use <name>") {
		t.Fatalf("expected default-removed next step in stdout: %s", out)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("key remove must not duplicate its result on stderr: %s", stderr)
	}
}

// TestKeyRemoveForceStrandedKeyThroughMain drives `key remove --force` end to end
// against a key whose record names a backend this build doesn't know (the
// tolerant-load case). Without --force it must error and point at --force; with
// --force the record is removed, the JSON reports secretRemoved:false with a
// warning, and the warning is carried in stdout.
func TestKeyRemoveForceStrandedKeyThroughMain(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	// Plant a stranded record by hand — `key add` would reject an unknown backend.
	stranded := `{"version":1,"defaultKey":"stranded","keys":{"stranded":{"type":"ed25519","keystore":"hardware","apiKeyId":"KEYID-1","publicKey":"p","createdAt":1}}}`
	if err := os.WriteFile(filepath.Join(home, "keys.json"), []byte(stranded), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without --force: a usable error that names the escape hatch, nonzero exit,
	// and the record must survive.
	_, stderr, code := runCLI([]string{"key", "remove", "stranded", "--compact"}, env, &stubDoer{})
	if code == 0 || !strings.Contains(stderr, "--force") {
		t.Fatalf("no-force removal of a stranded key must fail pointing at --force: code=%d stderr=%s", code, stderr)
	}
	if out, _, _ := runCLI([]string{"key", "list", "--compact"}, env, &stubDoer{}); !strings.Contains(out, `"name":"stranded"`) {
		t.Fatalf("failed no-force removal must leave the record: %s", out)
	}

	// With --force: removed, default unset, secretRemoved:false + warning in JSON.
	out, stderr, code := runCLI([]string{"key", "remove", "stranded", "--force", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("forced removal must succeed: code=%d out=%s stderr=%s", code, out, stderr)
	}
	if !strings.Contains(out, `"removed":"stranded"`) || !strings.Contains(out, `"secretRemoved":false`) {
		t.Fatalf("forced-removal JSON shape wrong: %s", out)
	}
	if !strings.Contains(out, `"defaultKey":null`) {
		t.Fatalf("forced removal of the default must unset it: %s", out)
	}
	if !strings.Contains(out, `"warning":`) || !strings.Contains(out, "hardware") {
		t.Fatalf("forced removal must carry a warning naming the backend: %s", out)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("forced-removal warning must not be duplicated on stderr: %s", stderr)
	}
	if listOut, _, _ := runCLI([]string{"key", "list", "--compact"}, env, &stubDoer{}); strings.Contains(listOut, `"name":"stranded"`) {
		t.Fatalf("forced removal must drop the record: %s", listOut)
	}
}

func TestKeyLifecycleThroughMain(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	out, _, code := runCLI([]string{"key", "add", "bot", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("add exit=%d", code)
	}
	if !strings.Contains(out, `"isDefault":true`) {
		t.Fatalf("first key should be default: %s", out)
	}
	out, _, code = runCLI([]string{"key", "list", "--compact"}, env, &stubDoer{})
	if code != 0 || !strings.Contains(out, `"name":"bot"`) {
		t.Fatalf("list: %s (exit %d)", out, code)
	}
}

// TestKeyAddSandboxIDRequiresSandboxName verifies that `key add <name> --api-key
// SANDBOX_…` is refused up front when the name lacks the sandbox token, and that
// it does NOT leave an orphaned unbound key behind (the add-then-bind path checks
// before creating anything).
func TestKeyAddSandboxIDRequiresSandboxName(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	_, errOut, code := runCLI([]string{"key", "add", "prod", "--api-key", keys.SandboxAPIKeyPrefix + "K1", "--compact"}, env, &stubDoer{})
	if code == 0 || !strings.Contains(errOut, "sandbox") {
		t.Fatalf("expected a sandbox-naming refusal, got exit=%d err=%q", code, errOut)
	}
	// No key should have been created.
	out, _, code := runCLI([]string{"key", "list", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("list exit=%d", code)
	}
	if strings.Contains(out, `"name":"prod"`) {
		t.Fatalf("a refused add must not leave an orphaned key: %s", out)
	}
	// A conforming name is accepted and bound.
	out, _, code = runCLI([]string{"key", "add", "sandbox", "--api-key", keys.SandboxAPIKeyPrefix + "K1", "--compact"}, env, &stubDoer{})
	if code != 0 || !strings.Contains(out, `"name":"sandbox"`) {
		t.Fatalf("a conforming sandbox name should be accepted: %s (exit %d)", out, code)
	}
}

func setKeyBaseURL(t *testing.T, home, name, u string) {
	t.Helper()
	m := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	// Empty WS companion: the monitor derives it from this REST URL at use time.
	if err := m.SetBaseURL(name, u, ""); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPerKeyBaseURLPrecedence pins the per-key tier: it beats prod and the
// global config.json baseUrl, but the per-invocation overrides (env, --base-url)
// still win, and config.json is used when the key has no override.
func TestPerKeyBaseURLPrecedence(t *testing.T) {
	hostFor := func(t *testing.T, args []string, env map[string]string, setup func(home string)) *url.URL {
		t.Helper()
		home := t.TempDir()
		seedBoundKey(t, home)
		if setup != nil {
			setup(home)
		}
		e := map[string]string{"KORBIT_CLI_HOME": home}
		for k, v := range env {
			e[k] = v
		}
		doer := &stubDoer{resp: resp(200, `{"success":true,"data":{}}`, nil)}
		_, stderr, code := runCLI(append([]string{"balance", "--key", "bot", "--compact"}, args...), e, doer)
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr)
		}
		if doer.last == nil {
			t.Fatalf("no request sent")
		}
		return doer.last.URL
	}

	// per-key beats the prod default
	if got := hostFor(t, nil, nil, func(home string) {
		setKeyBaseURL(t, home, "bot", "https://perkey.example.test")
	}); got.Host != "perkey.example.test" {
		t.Fatalf("per-key not used: %s", got)
	}
	// per-key beats global config.json baseUrl
	if got := hostFor(t, nil, nil, func(home string) {
		writeConfig(t, home, `{"baseUrl":"https://config.example.test"}`)
		setKeyBaseURL(t, home, "bot", "https://perkey.example.test")
	}); got.Host != "perkey.example.test" {
		t.Fatalf("per-key should beat config: %s", got)
	}
	// config.json used when the key has no override
	if got := hostFor(t, nil, nil, func(home string) {
		writeConfig(t, home, `{"baseUrl":"https://config.example.test"}`)
	}); got.Host != "config.example.test" {
		t.Fatalf("config not used: %s", got)
	}
	// env beats per-key
	if got := hostFor(t, nil, map[string]string{"KORBIT_CLI_BASE_URL": "https://env.example.test"}, func(home string) {
		setKeyBaseURL(t, home, "bot", "https://perkey.example.test")
	}); got.Host != "env.example.test" {
		t.Fatalf("env should beat per-key: %s", got)
	}
	// --base-url beats per-key (and env)
	if got := hostFor(t, []string{"--base-url", "https://flag.example.test"},
		map[string]string{"KORBIT_CLI_BASE_URL": "https://env.example.test"}, func(home string) {
			setKeyBaseURL(t, home, "bot", "https://perkey.example.test")
		}); got.Host != "flag.example.test" {
		t.Fatalf("flag should beat per-key: %s", got)
	}
}

// TestPublicCommandHonorsPerKeyBaseURL pins that a public (unsigned) command
// follows the in-play key's host instead of silently hitting prod: explicitly
// via --key, and implicitly via the default key. Public market data on a key
// pinned to a sandbox must not leak to production.
func TestPublicCommandHonorsPerKeyBaseURL(t *testing.T) {
	hostFor := func(t *testing.T, args []string, setup func(home string)) *url.URL {
		t.Helper()
		home := t.TempDir()
		seedBoundKey(t, home)
		setup(home)
		doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"last":"100"}}`, nil)}
		_, stderr, code := runCLI(append([]string{"ticker", "btc_krw", "--compact"}, args...),
			map[string]string{"KORBIT_CLI_HOME": home}, doer)
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr)
		}
		if doer.last == nil {
			t.Fatalf("no request sent")
		}
		// The public request must still be unsigned even though it adopts the key's host.
		if doer.last.Header.Get("x-kapi-key") != "" {
			t.Fatalf("public request must not be signed")
		}
		return doer.last.URL
	}

	// explicit --key: the public call targets that key's host
	if got := hostFor(t, []string{"--key", "bot"}, func(home string) {
		setKeyBaseURL(t, home, "bot", "https://perkey.example.test")
	}); got.Host != "perkey.example.test" {
		t.Fatalf("explicit --key per-key host not honored: %s", got)
	}
	// no --key: the default key's host is used
	if got := hostFor(t, nil, func(home string) {
		setKeyBaseURL(t, home, "bot", "https://perkey.example.test")
	}); got.Host != "perkey.example.test" {
		t.Fatalf("default-key per-key host not honored: %s", got)
	}
	// no per-key override: stays on the prod default
	if got := hostFor(t, []string{"--key", "bot"}, func(home string) {}); got.Host != "api.korbit.co.kr" {
		t.Fatalf("expected prod default, got: %s", got)
	}
}

func TestDryRunReflectsPerKeyBaseURL(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	setKeyBaseURL(t, home, "bot", "https://perkey.example.test")
	out, _, code := runCLI([]string{"balance", "--key", "bot", "--dry-run", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, `"baseUrl":"https://perkey.example.test"`) {
		t.Fatalf("dry-run baseUrl wrong: %s", out)
	}
}

func TestSetBaseURLCommand(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}

	// set (trailing slash trimmed); the WS companion is derived from the host.
	// --no-verify keeps the test offline (the smoke test is covered separately).
	out, _, code := runCLI([]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr/", "--no-verify", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("set exit=%d", code)
	}
	if !strings.Contains(out, `"baseUrl":"https://api-test.korbit.co.kr"`) ||
		!strings.Contains(out, `"wsBaseUrl":"wss://ws-api-test.korbit.co.kr"`) {
		t.Fatalf("set output: %s", out)
	}
	// key show reflects both
	out, _, _ = runCLI([]string{"key", "show", "bot", "--compact"}, env, &stubDoer{})
	if !strings.Contains(out, `"baseUrl":"https://api-test.korbit.co.kr"`) ||
		!strings.Contains(out, `"wsBaseUrl":"wss://ws-api-test.korbit.co.kr"`) {
		t.Fatalf("show output: %s", out)
	}
	// an explicit --ws-base-url is stored verbatim instead of being derived
	out, _, code = runCLI([]string{"key", "set-base-url", "bot", "https://api-test.korbit.co.kr",
		"--ws-base-url", "wss://stream.example.test/", "--no-verify", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("set --ws-base-url exit=%d", code)
	}
	if !strings.Contains(out, `"wsBaseUrl":"wss://stream.example.test"`) {
		t.Fatalf("explicit ws output: %s", out)
	}
	// clear removes both
	out, _, code = runCLI([]string{"key", "set-base-url", "bot", "--clear", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("clear exit=%d", code)
	}
	if !strings.Contains(out, `"baseUrl":""`) || !strings.Contains(out, `"wsBaseUrl":""`) {
		t.Fatalf("clear output: %s", out)
	}

	// validation paths all exit 2
	for _, args := range [][]string{
		{"key", "set-base-url", "bot"},                                                           // neither url nor --clear
		{"key", "set-base-url", "bot", "https://x.test", "--clear"},                              // both
		{"key", "set-base-url"},                                                                  // no name
		{"key", "set-base-url", "bot", "ftp://x"},                                                // bad url
		{"key", "set-base-url", "bot", "https://x.test", "--ws-base-url", "https://not-ws.test"}, // bad ws url
	} {
		if _, _, code := runCLI(append(args, "--compact"), env, &stubDoer{}); code != 2 {
			t.Errorf("%v should be exit 2, got %d", args, code)
		}
	}
}

func TestSetDefaultAccountSeqCommand(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}

	// Set a valid accountSeq.
	out, _, code := runCLI([]string{"key", "set-default-account-seq", "bot", "3", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("set exit=%d: %s", code, out)
	}
	if !strings.Contains(out, `"defaultAccountSeq":3`) {
		t.Fatalf("set output missing defaultAccountSeq: %s", out)
	}

	// key show reflects the new value.
	out, _, _ = runCLI([]string{"key", "show", "bot", "--compact"}, env, &stubDoer{})
	if !strings.Contains(out, `"defaultAccountSeq":3`) {
		t.Fatalf("show after set missing defaultAccountSeq: %s", out)
	}

	// key list reflects the new value.
	out, _, _ = runCLI([]string{"key", "list", "--compact"}, env, &stubDoer{})
	if !strings.Contains(out, `"defaultAccountSeq":3`) {
		t.Fatalf("list after set missing defaultAccountSeq: %s", out)
	}

	// Clear removes it.
	out, _, code = runCLI([]string{"key", "set-default-account-seq", "bot", "--clear", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("clear exit=%d: %s", code, out)
	}
	if strings.Contains(out, `"defaultAccountSeq"`) {
		t.Fatalf("clear should omit defaultAccountSeq (omitempty): %s", out)
	}

	// key show no longer shows it after clear.
	out, _, _ = runCLI([]string{"key", "show", "bot", "--compact"}, env, &stubDoer{})
	if strings.Contains(out, `"defaultAccountSeq"`) {
		t.Fatalf("show after clear should omit defaultAccountSeq: %s", out)
	}
}

func TestSetDefaultAccountSeqValidation(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}

	cases := []struct {
		name string
		args []string
	}{
		{"value and clear", []string{"key", "set-default-account-seq", "bot", "2", "--clear"}},
		{"no value no clear", []string{"key", "set-default-account-seq", "bot"}},
		{"zero", []string{"key", "set-default-account-seq", "bot", "0"}},
		{"negative", []string{"key", "set-default-account-seq", "bot", "-1"}},
		{"non-integer", []string{"key", "set-default-account-seq", "bot", "abc"}},
		{"unknown key", []string{"key", "set-default-account-seq", "nonexistent", "2"}},
		{"no name", []string{"key", "set-default-account-seq"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, code := runCLI(append(tc.args, "--compact"), env, &stubDoer{})
			if code != 2 {
				t.Errorf("args %v: want exit 2, got %d", tc.args, code)
			}
		})
	}
}

func TestPerKeyDefaultAccountSeqInSignedRequest(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}

	// Set per-key default to 5.
	if _, _, code := runCLI([]string{"key", "set-default-account-seq", "bot", "5", "--compact"}, env, &stubDoer{}); code != 0 {
		t.Fatal("setup failed")
	}

	// A signed request without explicit --account-seq uses the per-key default.
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}
	_, _, code := runCLI([]string{"balance", "--key", "bot", "--compact"}, env, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	rawQuery := doer.last.URL.RawQuery
	idx := strings.LastIndex(rawQuery, "&signature=")
	if idx < 0 {
		t.Fatalf("no signature in %q", rawQuery)
	}
	if signed := rawQuery[:idx]; !strings.Contains(signed, "accountSeq=5") {
		t.Fatalf("per-key default accountSeq=5 missing from signed string: %q", signed)
	}

	// An explicit --account-seq overrides the per-key default.
	doer2 := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1000"}}}`, nil)}
	_, _, code = runCLI([]string{"balance", "--account-seq", "2", "--key", "bot", "--compact"}, env, doer2)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	rawQuery2 := doer2.last.URL.RawQuery
	idx2 := strings.LastIndex(rawQuery2, "&signature=")
	if idx2 < 0 {
		t.Fatalf("no signature in %q", rawQuery2)
	}
	if signed := rawQuery2[:idx2]; !strings.Contains(signed, "accountSeq=2") {
		t.Fatalf("explicit --account-seq=2 should override per-key default: %q", signed)
	}
}

func TestPerKeyDefaultAccountSeqInDryRun(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}

	// Set per-key default to 4.
	if _, _, code := runCLI([]string{"key", "set-default-account-seq", "bot", "4", "--compact"}, env, &stubDoer{}); code != 0 {
		t.Fatal("setup failed")
	}

	// --dry-run should reflect the per-key default.
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)}
	out, _, code := runCLI([]string{"order", "place", "--symbol", "btc_krw", "--side", "buy",
		"--type", "limit", "--price", "100000000", "--qty", "0.001",
		"--key", "bot", "--dry-run", "--compact"}, env, doer)
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, out)
	}
	if !strings.Contains(out, `"accountSeq":"4"`) {
		t.Fatalf("dry-run must show per-key default accountSeq=4: %s", out)
	}
}
