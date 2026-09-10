// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/journal"
)

func openJournal(t *testing.T, home string) *journal.Logger {
	t.Helper()
	jl, err := journal.Open(journal.DefaultPath(home), false, nil)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	t.Cleanup(func() { jl.Close() })
	return jl
}

// placeDoer answers an order-place POST and the reconcile protocol's follow-up
// GET separately, building a FRESH response per call (stubDoer reuses one body,
// which the follow-up fetch would then read as empty).
type placeDoer struct {
	postStatus  int
	postBody    string
	getStatus   int
	getBody     string
	posts, gets int
}

func (d *placeDoer) Do(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost {
		d.posts++
		return resp(d.postStatus, d.postBody, nil), nil
	}
	d.gets++
	return resp(d.getStatus, d.getBody, nil), nil
}

// placeCallRow returns the single place api_call among calls (POST /v2/orders);
// the reconcile protocol also records its follow-up GET /v2/orders lookups.
func placeCallRow(t *testing.T, calls []journal.CallRow) journal.CallRow {
	t.Helper()
	for _, c := range calls {
		if c.Method == "POST" && c.Path == "/v2/orders" {
			return c
		}
	}
	t.Fatalf("no place api_call (POST /v2/orders) among %d calls", len(calls))
	return journal.CallRow{}
}

func TestOrderPlaceRecordsToJournal(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// Default behavior: place reconciles by clientOrderId and returns the FULL
	// fetched order. The bare accept ack (no status) and the fetched order (with
	// status) are distinct so we can prove the full order — not the ack — is
	// returned.
	doer := &placeDoer{
		postStatus: 200, postBody: `{"success":true,"data":{"orderId":123}}`,
		getStatus: 200, getBody: `{"success":true,"data":{"orderId":123,"symbol":"btc_krw","side":"buy","orderType":"limit","status":"open"}}`,
	}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	coid := result["clientOrderId"].(string)
	// The result is the fetched order (carries status), not the bare accept ack.
	if result["status"] != "open" {
		t.Fatalf("expected the full fetched order (status), got %s", out)
	}

	jl := openJournal(t, home)
	calls, err := jl.RecentCalls(10)
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	// The reconcile records the place POST plus its follow-up lookup GET.
	if len(calls) != 2 {
		t.Fatalf("want 2 api_calls (place + reconcile lookup), got %d", len(calls))
	}
	pc := placeCallRow(t, calls)
	if !pc.Success || !pc.Auth {
		t.Fatalf("place api_call wrong: %+v", pc)
	}
	if pc.KeyName != "bot" || pc.APIKeyID != "KEYID-1" {
		t.Fatalf("key identity not recorded: %+v", pc)
	}
	// The recorded params must never carry the signature or a private key.
	if strings.Contains(string(pc.Params), "signature") || strings.Contains(string(pc.Params), "PRIVATE") {
		t.Fatalf("params leaked secret material: %s", pc.Params)
	}

	orders, err := jl.RecentOrders(10)
	if err != nil {
		t.Fatalf("RecentOrders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	o := orders[0]
	if o.Status != "accepted" || o.OrderID != "123" || o.ClientOrderID != coid {
		t.Fatalf("order row wrong: %+v", o)
	}
	if o.Symbol != "btc_krw" || o.Side != "buy" || o.OrderType != "limit" {
		t.Fatalf("order fields not captured: %+v", o)
	}
	// The order links to the operation that placed it — the same operation the
	// place POST (and its follow-up lookup) are grouped under.
	if o.OperationID == nil || pc.OperationID == nil || *o.OperationID != *pc.OperationID {
		t.Fatalf("order not linked to the place operation: order=%+v place=%+v", o, pc)
	}
}

// A clean business rejection is not reconciled: one send, journaled failed.
func TestOrderPlaceFailureRecordsError(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &placeDoer{postStatus: 400, postBody: `{"success":false,"error":{"code":400,"message":"INSUFFICIENT_BALANCE","description":"not enough"}}`}
	_, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 3 {
		t.Fatalf("want exit 3, got %d", code)
	}
	if doer.gets != 0 {
		t.Fatalf("a clean rejection must not trigger a reconcile lookup, got %d GETs", doer.gets)
	}
	jl := openJournal(t, home)
	calls, _ := jl.RecentCalls(10)
	if len(calls) != 1 || calls[0].Success || calls[0].ErrorCode != "INSUFFICIENT_BALANCE" {
		t.Fatalf("api_call error not recorded: %+v", calls)
	}
	if calls[0].HTTPStatus == nil || *calls[0].HTTPStatus != 400 {
		t.Fatalf("http status not recorded: %+v", calls[0])
	}
	orders, _ := jl.RecentOrders(10)
	if len(orders) != 1 || orders[0].Status != "failed" || orders[0].ErrorCode != "INSUFFICIENT_BALANCE" {
		t.Fatalf("order failure not recorded: %+v", orders)
	}
	if orders[0].OrderID != "" {
		t.Fatalf("failed order should have no orderId: %+v", orders[0])
	}
}

// A DUPLICATE_CLIENT_ORDER_ID is not a failure: the reconcile resolves it
// by fetching the existing order and returns it (exit 0, journaled accepted).
func TestOrderPlaceDuplicateResolvesToOrder(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &placeDoer{
		postStatus: 409, postBody: `{"success":false,"error":{"code":409,"message":"DUPLICATE_CLIENT_ORDER_ID","description":"already placed"}}`,
		getStatus: 200, getBody: `{"success":true,"data":{"orderId":777,"status":"open"}}`,
	}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("a duplicate that resolves must succeed, exit=%d out=%s", code, out)
	}
	if doer.gets == 0 {
		t.Fatalf("expected a reconcile lookup after DUPLICATE")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if result["orderId"] != float64(777) {
		t.Fatalf("expected the fetched existing order (777), got %s", out)
	}
	orders, _ := openJournal(t, home).RecentOrders(10)
	if len(orders) != 1 || orders[0].Status != "accepted" || orders[0].OrderID != "777" {
		t.Fatalf("duplicate resolution must journal accepted/777, got %+v", orders)
	}
}

// When a DUPLICATE order can't be read back, the order IS placed — the protocol's
// do-not-re-place guidance must reach the agent (otherwise a retry with a new id
// double-orders). It rides the error envelope's guidance field; the exact-shape
// contract lives in TestOrderPlaceDuplicateGuidanceInEnvelope.
func TestOrderPlaceDuplicateUnreadableSurfacesGuidance(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &placeDoer{
		postStatus: 409, postBody: `{"success":false,"error":{"code":409,"message":"DUPLICATE_CLIENT_ORDER_ID","description":"already placed"}}`,
		getStatus: 200, getBody: `{}`, // never visible
	}
	_, stderr, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 3 {
		t.Fatalf("want exit 3, got %d", code)
	}
	if !strings.Contains(stderr, "do NOT re-place") {
		t.Fatalf("duplicate guidance must reach stderr, got: %s", stderr)
	}
}

// When an ambiguous (5xx) place can't be confirmed, the envelope shows the inner
// 5xx ApiError; the UNKNOWN verify-before-retrying guidance must still surface.
func TestOrderPlaceUnknownSurfacesGuidance(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &placeDoer{
		postStatus: 500, postBody: `{"success":false,"error":{"code":500,"message":"INTERNAL"}}`,
		getStatus: 200, getBody: `{}`, // never visible
	}
	_, stderr, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--retry-timeout", "0", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 3 {
		t.Fatalf("want exit 3 (5xx ApiError), got %d", code)
	}
	if !strings.Contains(stderr, "UNKNOWN") || !strings.Contains(stderr, "verify") {
		t.Fatalf("UNKNOWN guidance must reach stderr, got: %s", stderr)
	}
}

// --no-reconcile is the opt-out: a single send returning the raw accept ack, no
// follow-up fetch.
func TestOrderPlaceNoReconcileIsSingleShot(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &placeDoer{postStatus: 200, postBody: `{"success":true,"data":{"orderId":55}}`}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--no-reconcile", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if doer.posts != 1 || doer.gets != 0 {
		t.Fatalf("--no-reconcile must be a single shot with no fetch, got posts=%d gets=%d", doer.posts, doer.gets)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if result["orderId"] != float64(55) || result["clientOrderId"] == nil {
		t.Fatalf("expected the accept ack with the echoed clientOrderId, got %s", out)
	}
	orders, _ := openJournal(t, home).RecentOrders(10)
	if len(orders) != 1 || orders[0].Status != "accepted" || orders[0].OrderID != "55" {
		t.Fatalf("--no-reconcile must still journal accepted/55, got %+v", orders)
	}
}

// TestOrderPlaceParamsJSONInsertionOrder: the api_calls params_json for an order
// place must stay in spec/insertion order (including the auto-minted
// clientOrderId, appended last) — NOT the map-sorted order the L1 client encodes
// into CallInfo.ParamsJSON. A byte-level parity guard for the journal row.
func TestOrderPlaceParamsJSONInsertionOrder(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &placeDoer{
		postStatus: 200, postBody: `{"success":true,"data":{"orderId":1}}`,
		getStatus: 200, getBody: `{"success":true,"data":{"orderId":1,"status":"open"}}`,
	}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	jl := openJournal(t, home)
	calls, _ := jl.RecentCalls(10)
	pc := placeCallRow(t, calls)
	params := string(pc.Params)
	// Spec order is symbol, side, orderType, price, qty, then the appended
	// clientOrderId — assert the keys appear in that order, and clientOrderId last.
	wantOrder := []string{"symbol", "side", "orderType", "price", "qty", "clientOrderId"}
	last := -1
	for _, k := range wantOrder {
		idx := strings.Index(params, `"`+k+`"`)
		if idx < 0 {
			t.Fatalf("params_json missing %q: %s", k, params)
		}
		if idx < last {
			t.Fatalf("params_json not in insertion order at %q: %s", k, params)
		}
		last = idx
	}
}

// TestSignedGetParamsJSONInsertionOrder: a multi-param signed GET (order open)
// also stores params_json in spec/insertion order, not the client's map-sorted
// order — the same override path order place relies on, on a non-order command.
// A signed GET is a read, journaled only under --debug.
func TestSignedGetParamsJSONInsertionOrder(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)}
	if _, _, code := runCLI(
		[]string{"order", "open", "--symbol", "btc_krw", "--limit", "10", "--key", "bot", "--compact", "--debug"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer); code != 0 {
		t.Fatalf("order open exit=%d", code)
	}
	jl := openJournal(t, home)
	calls, _ := jl.RecentCalls(1)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	params := string(calls[0].Params)
	// Spec order: symbol, then limit (accountSeq's default is appended after).
	si, li := strings.Index(params, `"symbol"`), strings.Index(params, `"limit"`)
	if si < 0 || li < 0 || si > li {
		t.Fatalf("params_json not in insertion order (symbol before limit): %s", params)
	}
}

// orderRowAssertingDoer asserts, at the moment the placement request arrives,
// that the order's mint-time row already exists in the journal — proving
// StartOrder ran (and was committed) BEFORE the send.
type orderRowAssertingDoer struct {
	t    *testing.T
	home string
	resp *http.Response
}

func (d *orderRowAssertingDoer) Do(r *http.Request) (*http.Response, error) {
	jl, err := journal.Open(journal.DefaultPath(d.home), false, nil)
	if err != nil {
		d.t.Fatalf("open journal during send: %v", err)
	}
	defer jl.Close()
	orders, err := jl.RecentOrders(10)
	if err != nil {
		d.t.Fatalf("RecentOrders during send: %v", err)
	}
	if len(orders) != 1 || orders[0].Status != "attempting" {
		d.t.Fatalf("order intent row must exist (status 'attempting') before the send, got %+v", orders)
	}
	return d.resp, nil
}

// TestOrderPlaceStartOrderBeforeSend: the order intent row is written before the
// placement request is sent (the pre-send hard guarantee).
func TestOrderPlaceStartOrderBeforeSend(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &orderRowAssertingDoer{t: t, home: home,
		resp: resp(200, `{"success":true,"data":{"orderId":7,"status":"open"}}`, nil)}
	_, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
}

// TestJournalTimingUsesInjectableClock: the api_calls timing columns come from
// the injected clock (runCLI pins it to 1700000000000), not the real wall clock
// apiclient.Do brackets a call with — the row is stamped with rt.deps.Now().
func TestJournalTimingUsesInjectableClock(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)}
	if _, _, code := runCLI([]string{"balance", "--key", "bot", "--debug"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer); code != 0 {
		t.Fatalf("balance exit=%d", code)
	}
	jl := openJournal(t, home)
	calls, _ := jl.RecentCalls(1)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	const fixed = 1700000000000
	if calls[0].StartedAtMs != fixed {
		t.Fatalf("started_at_ms = %d, want the injected clock %d", calls[0].StartedAtMs, fixed)
	}
	if calls[0].FinishedAtMs == nil || *calls[0].FinishedAtMs != fixed {
		t.Fatalf("finished_at_ms = %v, want %d", calls[0].FinishedAtMs, fixed)
	}
	if calls[0].DurationMs == nil || *calls[0].DurationMs != 0 {
		t.Fatalf("duration_ms = %v, want 0 under the fixed clock", calls[0].DurationMs)
	}
}

// TestDoctorDoesNotJournal: doctor's signed diagnostic calls are never journaled
// (the doctor surface's policy declines to record), so the DB is never created.
func TestDoctorDoesNotJournal(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200,
		`{"success":true,"data":{"type":"trading","status":"activated","permissions":["writeOrders"]}}`, nil)}
	_, _, code := runCLI([]string{"doctor", "--key", "bot"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	// doctor exits 0 (healthy) or 1 (network) — never mind the code; what matters
	// is the journal stayed untouched.
	_ = code
	if _, err := os.Stat(journal.DefaultPath(home)); !os.IsNotExist(err) {
		t.Fatalf("doctor must not journal (or create the db): err=%v", err)
	}
}

func TestLogsCommand(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"DIGITALX_CLI_HOME": home}
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)}
	if _, _, code := runCLI([]string{"balance", "--key", "bot", "--debug"}, env, doer); code != 0 {
		t.Fatalf("balance exit=%d", code)
	}
	out, _, code := runCLI([]string{"logs", "--json"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("logs exit=%d", code)
	}
	var doc struct {
		Calls []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"calls"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("logs json: %v (%s)", err, out)
	}
	if len(doc.Calls) != 1 || doc.Calls[0].Method != "GET" || doc.Calls[0].Path != "/v2/balance" {
		t.Fatalf("logs did not show the balance call: %s", out)
	}

	// Human mode renders a table, not JSON.
	human, _, _ := runCLI([]string{"logs"}, env, &stubDoer{})
	if strings.HasPrefix(strings.TrimSpace(human), "{") {
		t.Fatalf("human logs should not be JSON: %s", human)
	}
	if !strings.Contains(human, "balance") {
		t.Fatalf("human logs missing command: %s", human)
	}
}

// TestPublicCallNotJournaledByDefault: an unauthenticated call is excluded from
// the journal (and doesn't even create the db) unless debug mode is on.
func TestPublicCallNotJournaledByDefault(t *testing.T) {
	home := t.TempDir()
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"timestamp":1}}`, nil)}
	if _, _, code := runCLI([]string{"time"}, map[string]string{"DIGITALX_CLI_HOME": home}, doer); code != 0 {
		t.Fatalf("time exit=%d", code)
	}
	if _, err := os.Stat(journal.DefaultPath(home)); !os.IsNotExist(err) {
		t.Fatalf("public call must not journal (or create the db) by default (err=%v)", err)
	}
}

// TestAuthReadNotJournaledByDefault: an authenticated read (balance) is excluded
// from the journal by default (and doesn't even create the db) — only writes are
// journaled unless --debug is on.
func TestAuthReadNotJournaledByDefault(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)}
	if _, _, code := runCLI([]string{"balance", "--key", "bot"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer); code != 0 {
		t.Fatalf("balance exit=%d", code)
	}
	if _, err := os.Stat(journal.DefaultPath(home)); !os.IsNotExist(err) {
		t.Fatalf("an auth read must not journal (or create the db) by default (err=%v)", err)
	}
}

// TestDebugModeJournalsPublicCalls: --debug (and DIGITALX_CLI_DEBUG) journal
// public calls and emit verbose stderr diagnostics.
func TestDebugModeJournalsPublicCalls(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"flag", []string{"time", "--debug"}, nil},
		{"env", []string{"time"}, map[string]string{"DIGITALX_CLI_DEBUG": "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := map[string]string{"DIGITALX_CLI_HOME": home}
			for k, v := range tc.env {
				env[k] = v
			}
			doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"timestamp":1}}`, nil)}
			_, stderr, code := runCLI(tc.args, env, doer)
			if code != 0 {
				t.Fatalf("exit=%d", code)
			}
			if !strings.Contains(stderr, "debug:") {
				t.Fatalf("debug mode should emit verbose stderr, got: %q", stderr)
			}
			jl := openJournal(t, home)
			calls, _ := jl.RecentCalls(10)
			if len(calls) != 1 || calls[0].Path != "/v2/time" || calls[0].Auth {
				t.Fatalf("public call should be journaled (auth=false) in debug mode: %+v", calls)
			}
		})
	}
}

func TestDebugBundleNoSecrets(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"DIGITALX_CLI_HOME": home}
	// Make one signed call so the bundle has activity (--debug journals the read).
	runCLI([]string{"balance", "--key", "bot", "--debug"}, env,
		&stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)})

	outFile := filepath.Join(home, "bundle.json")
	out, _, code := runCLI([]string{"debug", "bundle", "--out", outFile, "--json"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("debug bundle exit=%d out=%s", code, out)
	}
	var summary struct {
		Path     string `json:"path"`
		APICalls int    `json:"apiCalls"`
		Keys     int    `json:"keys"`
	}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("summary json: %v (%s)", err, out)
	}
	if summary.Path != outFile || summary.APICalls < 1 || summary.Keys < 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "KEYID-1") || !strings.Contains(body, `"cliVersion"`) {
		t.Fatalf("bundle missing expected non-secret fields: %s", body)
	}
	for _, banned := range []string{"PRIVATE KEY", "BEGIN", "signature"} {
		if strings.Contains(body, banned) {
			t.Fatalf("bundle leaked %q: %s", banned, body)
		}
	}
}

// TestJournalOpenFailureFailsCommand: a journal that cannot be opened fails the
// command BEFORE any network call (exit 4), so journaling is a hard guarantee.
func TestJournalOpenFailureFailsCommand(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// Occupy the db path with a directory so opening it as a SQLite file fails,
	// while config load (which only needs home to be a readable dir) still works.
	if err := os.Mkdir(journal.DefaultPath(home), 0o755); err != nil {
		t.Fatal(err)
	}
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)}
	// A recorded call opens the journal before sending; --debug records this read,
	// so the open is attempted (and fails) before anything is sent.
	_, stderr, code := runCLI([]string{"balance", "--key", "bot", "--debug"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 4 {
		t.Fatalf("want exit 4 (config), got %d", code)
	}
	if doer.last != nil {
		t.Fatal("network must not be called when the journal can't be opened")
	}
	if !strings.Contains(stderr, "journal") {
		t.Fatalf("error should mention the journal: %s", stderr)
	}
}

func TestJournalOptOut(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// --debug would normally journal this read; NO_JOURNAL suppresses it.
	env := map[string]string{"DIGITALX_CLI_HOME": home, "DIGITALX_CLI_NO_JOURNAL": "1"}
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[]}`, nil)}
	if _, _, code := runCLI([]string{"balance", "--key", "bot", "--debug"}, env, doer); code != 0 {
		t.Fatalf("balance exit=%d", code)
	}
	if _, err := os.Stat(journal.DefaultPath(home)); !os.IsNotExist(err) {
		t.Fatalf("journal db should not exist when opted out (err=%v)", err)
	}
}
