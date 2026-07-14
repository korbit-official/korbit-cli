// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// ---------------------------------------------------------------------------
// Fakes: a scriptable Conn + Dialer (no network) and a stub REST Doer.
// ---------------------------------------------------------------------------

type fakeConn struct {
	in        chan []byte
	writes    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	// pingFunc overrides Ping; nil means instant success.
	pingFunc func(ctx context.Context) error
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		in:     make(chan []byte, 64),
		writes: make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (c *fakeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case p := <-c.in:
		return p, nil
	case <-c.closed:
		return nil, errors.New("connection dropped")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *fakeConn) Write(ctx context.Context, p []byte) error {
	select {
	case <-c.closed:
		return errors.New("connection dropped")
	case c.writes <- append([]byte(nil), p...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeConn) Ping(ctx context.Context) error {
	if c.pingFunc != nil {
		return c.pingFunc(ctx)
	}
	return nil
}

func (c *fakeConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// serverPush feeds a frame as if the server sent it.
func (c *fakeConn) serverPush(t *testing.T, frame string) {
	t.Helper()
	select {
	case c.in <- []byte(frame):
	case <-time.After(2 * time.Second):
		t.Fatalf("serverPush stalled: %s", frame)
	}
}

// serverDrop kills the connection from the server side.
func (c *fakeConn) serverDrop() { _ = c.Close() }

// awaitWrite returns the next client->server message.
func (c *fakeConn) awaitWrite(t *testing.T) []byte {
	t.Helper()
	select {
	case p := <-c.writes:
		return p
	case <-time.After(2 * time.Second):
		t.Fatalf("no client write arrived")
		return nil
	}
}

type dialRecord struct {
	url    string
	header http.Header
}

type fakeDialer struct {
	mu     sync.Mutex
	script []func() (Conn, error)
	dials  []dialRecord
}

func (d *fakeDialer) dial(ctx context.Context, wsURL string, header http.Header) (Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, dialRecord{url: wsURL, header: header.Clone()})
	var next func() (Conn, error)
	if len(d.script) > 0 {
		next = d.script[0]
		d.script = d.script[1:]
	}
	d.mu.Unlock()
	if next == nil {
		// Script exhausted: park until the session shuts down.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return next()
}

func (d *fakeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dials)
}

func (d *fakeDialer) dialAt(i int) dialRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials[i]
}

// instantDropConn accepts the subscribe write but its Read fails immediately,
// so serve() returns at once — modeling a server that accepts the upgrade then
// instantly drops the connection. The reconnect loop must still back off before
// re-dialing; without that it would spin hot.
type instantDropConn struct{}

func (instantDropConn) Read(ctx context.Context) ([]byte, error) {
	return nil, errors.New("connection dropped")
}
func (instantDropConn) Write(ctx context.Context, p []byte) error { return nil }
func (instantDropConn) Ping(ctx context.Context) error            { return nil }
func (instantDropConn) Close() error                              { return nil }

func instantDropScript(n int) []func() (Conn, error) {
	var s []func() (Conn, error)
	for i := 0; i < n; i++ {
		s = append(s, func() (Conn, error) { return instantDropConn{}, nil })
	}
	return s
}

func connScript(conns ...*fakeConn) []func() (Conn, error) {
	var script []func() (Conn, error)
	for _, c := range conns {
		c := c
		script = append(script, func() (Conn, error) { return c, nil })
	}
	return script
}

// stubDoer answers REST calls. handle gets the path and query/form params.
type stubDoer struct {
	mu     sync.Mutex
	calls  []string
	handle func(path string, params url.Values) (status int, body string)
}

func (d *stubDoer) Do(req *http.Request) (*http.Response, error) {
	params := req.URL.Query()
	d.mu.Lock()
	d.calls = append(d.calls, req.Method+" "+req.URL.Path)
	handle := d.handle
	d.mu.Unlock()
	status, body := 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
	if handle != nil {
		status, body = handle(req.URL.Path, params)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (d *stubDoer) called(pathSuffix string) bool {
	return d.countCalls(pathSuffix) > 0
}

func (d *stubDoer) countCalls(pathSuffix string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if strings.HasSuffix(c, pathSuffix) {
			n++
		}
	}
	return n
}

func ok(dataJSON string) (int, string) {
	return 200, `{"success":true,"data":` + dataJSON + `}`
}

// ---------------------------------------------------------------------------
// Harness: run a session and tap its events.
// ---------------------------------------------------------------------------

// orderScopes builds tracking scopes for symbols under the default account
// (the tests' myOrder subscriptions pin no AccountSeqs).
func orderScopes(symbols ...string) []OrderScope {
	out := make([]OrderScope, 0, len(symbols))
	for _, sym := range symbols {
		out = append(out, OrderScope{Symbol: sym})
	}
	return out
}

func testTunables() Tunables {
	return Tunables{
		DialTimeoutMs:  2000,
		WriteTimeoutMs: 2000,
		ReconnectMinMs: 1, ReconnectMaxMs: 5, StableAfterMs: 60_000,
		PingIntervalMs: 60_000, PongTimeoutMs: 1000, // pings effectively off unless a test opts in
		KeepaliveAfterMs:      60_000 * 5, // keepalive effectively off unless a test opts in
		DelayWarnMs:           10_000,
		UnreliableRTTMs:       2_000,
		UnreliableWindowMs:    300_000,
		NoticeMinIntervalMs:   1,
		UnreliableDisconnects: 1000, // off unless a test opts in
		BackfillRetryMinMs:    1, BackfillRetryMaxMs: 5,
	}
}

type tap struct {
	t       *testing.T
	s       *Session
	seen    []Event // everything received, for failure messages
	pending []Event // received but not yet matched by a wait*
	done    chan error
	result  *error
}

func startSession(t *testing.T, cfg Config) (*tap, context.CancelFunc) {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	tp := &tap{t: t, s: s, done: done}
	t.Cleanup(func() {
		cancel()
		if _, ok := tp.runResult(5 * time.Second); !ok {
			t.Errorf("Run did not return after cancel")
		}
	})
	return tp, cancel
}

// runResult waits for Run to return, caching the result so it can be asked
// for more than once (the test body and the cleanup both do).
func (tp *tap) runResult(timeout time.Duration) (error, bool) {
	if tp.result != nil {
		return *tp.result, true
	}
	select {
	case err := <-tp.done:
		tp.result = &err
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (tp *tap) next(timeout time.Duration) (Event, bool) {
	select {
	case ev, ok := <-tp.s.Events():
		if ok {
			tp.seen = append(tp.seen, ev)
		}
		return ev, ok
	case <-time.After(timeout):
		return nil, false
	}
}

// wait scans events — first the unmatched backlog, then live arrivals — until
// pred matches; unmatched events stay queued for later wait calls.
func (tp *tap) wait(desc string, pred func(Event) bool) Event {
	tp.t.Helper()
	for i, ev := range tp.pending {
		if pred(ev) {
			tp.pending = append(tp.pending[:i:i], tp.pending[i+1:]...)
			return ev
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ev, ok := tp.next(time.Until(deadline))
		if !ok {
			break
		}
		if pred(ev) {
			return ev
		}
		tp.pending = append(tp.pending, ev)
	}
	tp.t.Fatalf("%s never arrived; saw: %s", desc, tp.describeSeen())
	return nil
}

func (tp *tap) waitNotice(code NoticeCode) Notice {
	tp.t.Helper()
	ev := tp.wait("notice "+string(code), func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == code
	})
	return ev.(Notice)
}

func (tp *tap) waitData(desc string, pred func(Data) bool) Data {
	tp.t.Helper()
	ev := tp.wait("data ("+desc+")", func(ev Event) bool {
		d, isData := ev.(Data)
		return isData && pred(d)
	})
	return ev.(Data)
}

func (tp *tap) describeSeen() string {
	var parts []string
	for _, ev := range tp.seen {
		switch v := ev.(type) {
		case Notice:
			parts = append(parts, string(v.Code))
		case Data:
			parts = append(parts, fmt.Sprintf("data:%s/%s/%s", v.Channel, v.Symbol, v.Origin))
		}
	}
	return strings.Join(parts, ", ")
}

func parseSubscribeItems(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("subscribe message was not a JSON array: %v (%s)", err, raw)
	}
	return items
}

func testAuth(t *testing.T) (*korbit.Credentials, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &korbit.Credentials{APIKeyID: "test-key-id", Signer: korbit.NewEd25519Signer(priv)}, pub
}

// testClient builds the stream's single Korbit API handle for tests: a
// korbit.Client over the given REST doer with a real shared clock + Syncer
// (measuring /v2/time through the doer, so the proactive-measure and
// EXCEED_TIME_WINDOW-resync paths run the real machinery), optional creds for
// the signed upgrade, and the stream-backfill Origin.
func testClient(baseURL string, doer korbit.Doer, creds *korbit.Credentials) *korbit.Client {
	now := func() int64 { return time.Now().UnixMilli() }
	clk := clock.New(now)
	measure := func() (int64, int64, error) {
		off, err := korbit.MeasureClockOffset(korbit.Options{BaseURL: baseURL, Doer: doer, Now: now}, 0, nil)
		if err != nil {
			return 0, 0, err
		}
		return off.OffsetMs, off.UncertaintyMs(), nil
	}
	syncer := clock.NewSyncer(clk, measure, now, clock.DefaultCoolDownMs)
	c := &korbit.Client{
		BaseURL: baseURL,
		Doer:    doer,
		Creds:   creds,
		Clock:   clk,
		Resync:  syncer.Sync,
		Origin:  korbit.Origin{Surface: "stream-backfill"},
	}
	if creds != nil {
		c.APIKeyID = creds.APIKeyID // recorder metadata (CallInfo); the upgrade header reads Creds.APIKeyID
	}
	return c
}

// ---------------------------------------------------------------------------
// Config validation.
// ---------------------------------------------------------------------------

func TestNewValidation(t *testing.T) {
	auth, _ := testAuth(t)
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"no subscriptions", Config{}, "at least one subscription"},
		{"unknown channel", Config{Subscriptions: []Subscription{{Channel: "bogus", Symbols: []string{"btc_krw"}}}}, "unknown channel"},
		{"missing symbols", Config{Subscriptions: []Subscription{{Channel: ChannelTicker}}}, "requires at least one symbol"},
		{"level on ticker", Config{Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}, Level: "1000"}}}, "Level applies"},
		{"accountSeqs on public", Config{Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}, AccountSeqs: []int{1}}}}, "AccountSeqs applies"},
		{"tradeHistory on non-trade", Config{Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}, TradeHistory: 50}}}, "TradeHistory applies to the trade channel"},
		{"negative tradeHistory", Config{Subscriptions: []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}, TradeHistory: -1}}}, "TradeHistory must be non-negative"},
		// No client at all (it carries the clock + REST recovery).
		{"missing client", Config{Subscriptions: []Subscription{{Channel: ChannelMyAsset}}}, "a Client with a Clock is required"},
		// A client without credentials cannot sign the private upgrade.
		{"private without creds", Config{Subscriptions: []Subscription{{Channel: ChannelMyAsset}}, Client: testClient("http://x", nil, nil)}, "require a Client with credentials"},
		// Creds present but malformed: a nil Signer would panic at sign time and an
		// empty APIKeyID would send an empty X-KAPI-KEY — both rejected up front.
		{"private creds nil signer", Config{Subscriptions: []Subscription{{Channel: ChannelMyAsset}}, Client: testClient("http://x", nil, &korbit.Credentials{APIKeyID: "k"})}, "require a Client with credentials"},
		{"private creds empty apiKeyID", Config{Subscriptions: []Subscription{{Channel: ChannelMyAsset}}, Client: testClient("http://x", nil, &korbit.Credentials{Signer: auth.Signer})}, "require a Client with credentials"},
		// A credentialed client with no BaseURL cannot back the private state.
		{"private without baseURL", Config{Subscriptions: []Subscription{{Channel: ChannelMyAsset}}, Client: testClient("", nil, auth)}, "the Client's BaseURL"},
		// A trade channel with backfill on needs the client's BaseURL.
		{"trade without baseURL", Config{Subscriptions: []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}}, Client: testClient("", nil, nil)}, "the trade channel with backfill enabled requires the Client's BaseURL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}

	// trade without a REST BaseURL is fine when backfill is off (but the client,
	// which carries the clock, is still required).
	_, err := New(Config{
		Subscriptions:   []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}},
		DisableBackfill: true,
		Client:          testClient("", nil, nil),
	})
	if err != nil {
		t.Fatalf("trade + DisableBackfill should not need a REST BaseURL: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Public streaming: subscribe protocol, data delivery, reconnect.
// ---------------------------------------------------------------------------

func TestPublicSubscribeAndStream(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tp, _ := startSession(t, Config{
		Client:    testClient("", nil, nil),
		PublicURL: "ws://example.test/v2/public",
		Subscriptions: []Subscription{
			{Channel: ChannelTicker, Symbols: []string{"btc_krw", "eth_krw"}},
			{Channel: ChannelOrderbook, Symbols: []string{"btc_krw"}, Level: "1000"},
		},
		Dial:     dialer.dial,
		Tunables: testTunables(),
	})

	n := tp.waitNotice(Connected)
	if n.Details["endpoint"] != "public" || n.Details["attempt"] != 1 {
		t.Fatalf("unexpected Connected details: %v", n.Details)
	}

	items := parseSubscribeItems(t, conn.awaitWrite(t))
	if len(items) != 2 {
		t.Fatalf("want 2 subscribe items, got %v", items)
	}
	if items[0]["type"] != "ticker" || items[0]["method"] != "subscribe" || items[0]["requestId"] == nil {
		t.Fatalf("bad ticker item: %v", items[0])
	}
	if items[1]["type"] != "orderbook" || items[1]["level"] != "1000" {
		t.Fatalf("bad orderbook item: %v", items[1])
	}

	conn.serverPush(t, `{"type":"ticker","timestamp":1700000000000,"symbol":"btc_krw","snapshot":true,"data":{"close":"99027000"}}`)
	d := tp.waitData("ticker snapshot", func(d Data) bool { return d.Channel == ChannelTicker })
	if d.Origin != OriginSnapshot || d.Symbol != "btc_krw" || d.ServerTime != 1700000000000 {
		t.Fatalf("unexpected snapshot Data: %+v", d)
	}

	conn.serverPush(t, `{"type":"ticker","timestamp":1700000001000,"symbol":"btc_krw","data":{"close":"99028000"}}`)
	d = tp.waitData("ticker realtime", func(d Data) bool { return d.Channel == ChannelTicker && d.Origin == OriginRealtime })
	if !strings.Contains(string(d.Payload), "99028000") {
		t.Fatalf("payload not verbatim: %s", d.Payload)
	}
}

func TestReconnectResubscribes(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverDrop()

	tp.waitNotice(Disconnected)
	n := tp.waitNotice(Connected)
	if n.Details["attempt"] != 2 {
		t.Fatalf("want attempt 2, got %v", n.Details)
	}
	items := parseSubscribeItems(t, conn2.awaitWrite(t))
	if len(items) != 1 || items[0]["type"] != "ticker" {
		t.Fatalf("resubscribe missing: %v", items)
	}
}

// TestConnectedLevelReflectsRecovery pins the context-dependent CONNECTED level:
// a clean first connect is info (routine), but the CONNECTED that follows a drop
// is warn — it resolves the DISCONNECTED, so the same level threshold shows both
// the drop and the recovery.
func TestConnectedLevelReflectsRecovery(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	if n := tp.waitNotice(Connected); n.Level != LevelInfo {
		t.Fatalf("first CONNECTED level = %q, want info (clean first connect)", n.Level)
	}
	conn1.awaitWrite(t)
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	if n := tp.waitNotice(Connected); n.Level != LevelWarn {
		t.Fatalf("reconnect CONNECTED level = %q, want warn (recovery of the drop)", n.Level)
	}
}

// TestNoReconnectExitsOnDrop pins the NoReconnect contract for a drop after a
// good connection: the usual DISCONNECTED notice fires, a terminal FATAL notice
// follows, Run returns the underlying error, and the session does NOT re-dial.
func TestNoReconnectExitsOnDrop(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		NoReconnect:   true,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn.awaitWrite(t)
	conn.serverDrop()

	tp.waitNotice(Disconnected)
	tp.waitNotice(Fatal)
	err, returned := tp.runResult(5 * time.Second)
	if !returned {
		t.Fatal("Run did not return after a drop under NoReconnect")
	}
	if err == nil {
		t.Fatal("want a fatal error from Run under NoReconnect, got nil")
	}
	if n := dialer.dialCount(); n != 1 {
		t.Fatalf("want exactly 1 dial (no reconnect), got %d", n)
	}
}

// TestNoReconnectExitsOnFailedConnect pins the NoReconnect contract for a
// failed initial connect: CONNECT_FAILED then a terminal FATAL, Run returns the
// dial error, and there is no retry.
func TestNoReconnectExitsOnFailedConnect(t *testing.T) {
	dialer := &fakeDialer{script: []func() (Conn, error){
		func() (Conn, error) { return nil, errors.New("dial refused") },
	}}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		NoReconnect:   true,
		Tunables:      testTunables(),
	})

	tp.waitNotice(ConnectFailed)
	tp.waitNotice(Fatal)
	err, returned := tp.runResult(5 * time.Second)
	if !returned {
		t.Fatal("Run did not return after a failed connect under NoReconnect")
	}
	if err == nil {
		t.Fatal("want an error from Run under NoReconnect on a failed connect, got nil")
	}
	if n := dialer.dialCount(); n != 1 {
		t.Fatalf("want exactly 1 dial (no retry), got %d", n)
	}
}

// TestReconnectAlwaysBacksOffNoHotLoop pins the loop-safety invariant for the
// reconnect loop: a connection that is accepted and then instantly dropped
// (serve returns ~0ms) must still be followed by a backoff before the next
// dial. Without the top-of-loop backoff this path re-dials with no delay and
// spins hot. The injected Sleep records every backoff; we require one before
// each reconnect.
func TestReconnectAlwaysBacksOffNoHotLoop(t *testing.T) {
	var mu sync.Mutex
	sleeps := 0
	slept := make(chan struct{}, 256)
	sleep := func(time.Duration) {
		mu.Lock()
		sleeps++
		mu.Unlock()
		select {
		case slept <- struct{}{}:
		default:
		}
	}
	dialer := &fakeDialer{script: instantDropScript(8)}
	tp, cancel := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Sleep:         sleep,
		Tunables:      testTunables(),
	})

	// Three reconnects, each gated by a backoff — proves the serve-disconnect
	// path does not re-dial without pausing. A hot loop would record zero sleeps
	// and time out here.
	for i := 0; i < 3; i++ {
		select {
		case <-slept:
		case <-time.After(5 * time.Second):
			t.Fatalf("no backoff before reconnect %d — the reconnect loop is hot", i+1)
		}
	}

	cancel()
	if _, ok := tp.runResult(5 * time.Second); !ok {
		t.Fatal("Run did not return after cancel")
	}

	mu.Lock()
	s := sleeps
	mu.Unlock()
	dials := dialer.dialCount()
	// Every dial after the first is preceded by exactly one backoff, so sleeps
	// must keep pace with reconnects (the only un-slept dial is the first).
	if s < dials-1 {
		t.Fatalf("hot reconnect loop: %d dials but only %d backoff sleeps", dials, s)
	}
}

func TestSubscribeRejectionDropsSubscription(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tp, _ := startSession(t, Config{
		Client: testClient("", nil, nil),
		Subscriptions: []Subscription{
			{Channel: ChannelTicker, Symbols: []string{"btc_krw"}},
			{Channel: ChannelOrderbook, Symbols: []string{"bogus_krw"}},
		},
		Dial:     dialer.dial,
		Tunables: testTunables(),
	})

	tp.waitNotice(Connected)
	items := parseSubscribeItems(t, conn1.awaitWrite(t))
	// Reject the orderbook item.
	var orderbookReq float64
	for _, item := range items {
		if item["type"] == "orderbook" {
			orderbookReq = item["requestId"].(float64)
		}
	}
	conn1.serverPush(t, fmt.Sprintf(`{"status":"fail","requestId":%d,"code":"INVALID_SYMBOL","message":"The symbol does not exist (bogus_krw)"}`, int(orderbookReq)))

	n := tp.waitNotice(SubscribeFailed)
	if n.Details["channel"] != ChannelOrderbook || n.Details["code"] != "INVALID_SYMBOL" {
		t.Fatalf("unexpected SubscribeFailed details: %v", n.Details)
	}

	// The dropped subscription must not come back on reconnect.
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	items = parseSubscribeItems(t, conn2.awaitWrite(t))
	if len(items) != 1 || items[0]["type"] != "ticker" {
		t.Fatalf("rejected subscription was resubscribed: %v", items)
	}
}

// goLive consumes the initial subscribeAll frame and then proves the serve
// loop is running (so the connection is "live" and applyChange will write
// dynamic frames) by pushing a ticker snapshot and waiting for its data event.
func goLive(t *testing.T, tp *tap, conn *fakeConn, symbol string) {
	t.Helper()
	tp.waitNotice(Connected)
	parseSubscribeItems(t, conn.awaitWrite(t)) // initial subscribeAll
	conn.serverPush(t, fmt.Sprintf(`{"type":"ticker","timestamp":1700000000000,"symbol":"%s","snapshot":true,"data":{"close":"1"}}`, symbol))
	tp.waitData("live ticker", func(d Data) bool { return d.Channel == ChannelTicker && d.Symbol == symbol })
}

// TestDynamicSubscribeWritesFrames: Session.Subscribe/Unsubscribe put the right
// subscribe/unsubscribe frames on the wire for the public orderbook/trade
// channels.
func TestDynamicSubscribeWritesFrames(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})
	goLive(t, tp, conn, "btc_krw")

	if err := tp.s.Subscribe(Subscription{Channel: ChannelTrade, Symbols: []string{"eth_krw"}, TradeHistory: 50}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	items := parseSubscribeItems(t, conn.awaitWrite(t))
	if len(items) != 1 || items[0]["method"] != "subscribe" || items[0]["type"] != "trade" {
		t.Fatalf("unexpected subscribe frame: %v", items)
	}
	if syms, _ := items[0]["symbols"].([]any); len(syms) != 1 || syms[0] != "eth_krw" {
		t.Fatalf("unexpected subscribe symbols: %v", items[0])
	}
	// The seed depth is recorded against the live registry so the first
	// snapshot of the dynamically-added symbol triggers the REST seed.
	if got := tp.s.tradeHistoryFor("eth_krw"); got != 50 {
		t.Fatalf("dynamic trade seed depth = %d, want 50", got)
	}

	if err := tp.s.Unsubscribe(ChannelTrade, []string{"eth_krw"}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	items = parseSubscribeItems(t, conn.awaitWrite(t))
	if len(items) != 1 || items[0]["method"] != "unsubscribe" || items[0]["type"] != "trade" {
		t.Fatalf("unexpected unsubscribe frame: %v", items)
	}
	if got := tp.s.tradeHistoryFor("eth_krw"); got != 0 {
		t.Fatalf("seed depth after unsubscribe = %d, want 0", got)
	}

	// Non-public channels are refused.
	if err := tp.s.Subscribe(Subscription{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}}); err == nil {
		t.Fatalf("Subscribe must refuse a private channel")
	}
}

// TestDynamicSubscribeSurvivesReconnect: a dynamically-added subscription is in
// the registry, so a reconnect re-subscribes the CURRENT set (ticker + the
// added orderbook), not just the original config.
func TestDynamicSubscribeSurvivesReconnect(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})
	goLive(t, tp, conn1, "btc_krw")

	if err := tp.s.Subscribe(Subscription{Channel: ChannelOrderbook, Symbols: []string{"eth_krw"}}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	parseSubscribeItems(t, conn1.awaitWrite(t)) // the dynamic subscribe frame

	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	items := parseSubscribeItems(t, conn2.awaitWrite(t))
	types := map[string]bool{}
	for _, it := range items {
		types[it["type"].(string)] = true
	}
	if !types["ticker"] || !types["orderbook"] {
		t.Fatalf("reconnect did not resubscribe the current set: %v", items)
	}
}

// TestDynamicUnsubscribeRejectionIsBenign: a rejected unsubscribe is a Warn
// notice (we already removed it locally) and never drops a subscription or goes
// fatal — the connection keeps streaming.
func TestDynamicUnsubscribeRejectionIsBenign(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})
	goLive(t, tp, conn, "btc_krw")

	if err := tp.s.Unsubscribe(ChannelOrderbook, []string{"eth_krw"}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	items := parseSubscribeItems(t, conn.awaitWrite(t))
	reqID := int(items[0]["requestId"].(float64))
	conn.serverPush(t, fmt.Sprintf(`{"status":"fail","requestId":%d,"code":"NOT_SUBSCRIBED","message":"x"}`, reqID))

	n := tp.waitNotice(SubscribeFailed)
	if n.Level != LevelWarn || n.Details["method"] != "unsubscribe" {
		t.Fatalf("unsubscribe-fail should be a benign Warn: %+v", n)
	}
	conn.serverPush(t, `{"type":"ticker","timestamp":1700000002000,"symbol":"btc_krw","data":{"close":"2"}}`)
	tp.waitData("ticker after unsub-fail", func(d Data) bool {
		return d.Channel == ChannelTicker && strings.Contains(string(d.Payload), `"close":"2"`)
	})
}

func TestAllSubscriptionsRejectedIsFatal(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"bogus_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	items := parseSubscribeItems(t, conn.awaitWrite(t))
	conn.serverPush(t, fmt.Sprintf(`{"status":"fail","requestId":%d,"code":"INVALID_SYMBOL","message":"nope"}`, int(items[0]["requestId"].(float64))))

	tp.waitNotice(SubscribeFailed)
	tp.waitNotice(Fatal)
	err, returned := tp.runResult(5 * time.Second)
	if !returned {
		t.Fatal("Run did not return after fatal")
	}
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want fatal error from Run, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Private endpoint: signed upgrade, auth errors, clock resync.
// ---------------------------------------------------------------------------

func TestPrivateUpgradeIsSigned(t *testing.T) {
	auth, pub := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/balance" {
			return ok(`[{"currency":"krw","available":"1000"}]`)
		}
		return 404, `{}`
	}}
	tp, _ := startSession(t, Config{
		PrivateURL:    "ws://example.test/v2/private",
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	rec := dialer.dialAt(0)
	if rec.header.Get("X-KAPI-KEY") != "test-key-id" {
		t.Fatalf("X-KAPI-KEY missing: %v", rec.header)
	}
	u, err := url.Parse(rec.url)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	ts, sig := q.Get("timestamp"), q.Get("signature")
	if ts == "" || sig == "" {
		t.Fatalf("unsigned upgrade URL: %s", rec.url)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	if !ed25519.Verify(pub, []byte("timestamp="+ts), sigBytes) {
		t.Fatal("upgrade signature does not verify over the signed bytes")
	}
	// And the signed query must come before signature in the raw URL
	// (signature appended last).
	if !strings.Contains(rec.url, "?timestamp=") || !strings.Contains(rec.url, "&signature=") {
		t.Fatalf("query ordering wrong: %s", rec.url)
	}
}

// TestPrivateUpgradeIncludesRecvWindowWhenNeeded: with a measured clock whose
// uncertainty exceeds the server's default 5s window, the signed upgrade
// query must carry recvWindow inside the signed bytes (timestamp, recvWindow,
// signature appended last).
func TestPrivateUpgradeIncludesRecvWindowWhenNeeded(t *testing.T) {
	auth, pub := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) { return ok(`[]`) }}
	// A high-RTT measurement: lean = RTTMin/2 = 2000ms -> recvWindow 12000. Install
	// it on the client's clock BEFORE Run so the first signed upgrade carries it.
	clk := clock.New(func() int64 { return time.Now().UnixMilli() })
	off := korbit.ClockOffset{OffsetMs: 0, RTTMinMs: 4000, Samples: 1}
	clk.Install(off.OffsetMs, off.UncertaintyMs())
	client := &korbit.Client{
		BaseURL: "http://example.test", Doer: doer, Creds: auth, Clock: clk,
		Origin: korbit.Origin{Surface: "stream-backfill"},
	}
	s, err := New(Config{
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        client,
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for dialer.dialCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if dialer.dialCount() == 0 {
		t.Fatal("no dial happened")
	}
	u, err := url.Parse(dialer.dialAt(0).url)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("recvWindow") != "12000" {
		t.Fatalf("recvWindow missing/wrong: %s", dialer.dialAt(0).url)
	}
	signed := "timestamp=" + q.Get("timestamp") + "&recvWindow=12000"
	if !strings.Contains(dialer.dialAt(0).url, signed+"&signature=") {
		t.Fatalf("signed segment must precede signature: %s", dialer.dialAt(0).url)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(q.Get("signature"))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(signed), sigBytes) {
		t.Fatal("signature does not verify over timestamp&recvWindow")
	}
}

// TestProactiveMeasureGatedOnProactiveTimeSync: the server clock is measured at
// Run start only when Config.ProactiveTimeSync is set. Without it the estimate is
// corrected only reactively (on an EXCEED_TIME_WINDOW rejection), so /v2/time is
// not probed at startup. A fakeDialer accepts the upgrade regardless of the
// signed clock, so the connect succeeds either way.
func TestProactiveMeasureGatedOnProactiveTimeSync(t *testing.T) {
	mkDoer := func() *stubDoer {
		return &stubDoer{handle: func(path string, _ url.Values) (int, string) {
			switch path {
			case "/v2/time":
				return ok(`{"time":1700000000000}`)
			case "/v2/balance":
				return ok(`[]`)
			}
			return 404, `{}`
		}}
	}

	t.Run("enabled probes at Run start", func(t *testing.T) {
		auth, _ := testAuth(t)
		doer := mkDoer()
		dialer := &fakeDialer{script: connScript(newFakeConn())}
		tp, _ := startSession(t, Config{
			Subscriptions:     []Subscription{{Channel: ChannelMyAsset}},
			Client:            testClient("http://example.test", doer, auth),
			Dial:              dialer.dial,
			ProactiveTimeSync: true,
			Tunables:          testTunables(),
		})
		tp.waitNotice(Connected)
		deadline := time.Now().Add(5 * time.Second)
		for !doer.called("/v2/time") && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !doer.called("/v2/time") {
			t.Fatal("ProactiveTimeSync=true: expected a proactive /v2/time probe at Run start")
		}
	})

	t.Run("disabled does not probe at Run start", func(t *testing.T) {
		auth, _ := testAuth(t)
		doer := mkDoer()
		dialer := &fakeDialer{script: connScript(newFakeConn())}
		tp, _ := startSession(t, Config{
			Subscriptions:     []Subscription{{Channel: ChannelMyAsset}},
			Client:            testClient("http://example.test", doer, auth),
			Dial:              dialer.dial,
			ProactiveTimeSync: false,
			Tunables:          testTunables(),
		})
		// The proactive measure (if enabled) is launched before any dial in Run,
		// so by the time connect + the private backfill (balances) complete it
		// would already have probed /v2/time. It has not, because it is gated off.
		tp.waitNotice(Connected)
		tp.waitNotice(BackfillDone)
		if doer.called("/v2/time") {
			t.Fatal("ProactiveTimeSync=false: /v2/time must not be probed proactively (reactive correction only)")
		}
	})
}

func TestPrivateAuthRejectionIsFatal(t *testing.T) {
	auth, _ := testAuth(t)
	dialer := &fakeDialer{script: []func() (Conn, error){
		func() (Conn, error) { return nil, &UpgradeError{Status: 401, Code: "KEY_NOT_FOUND"} },
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        testClient("http://example.test", &stubDoer{}, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	n := tp.waitNotice(Fatal)
	if n.Details["code"] != "KEY_NOT_FOUND" {
		t.Fatalf("unexpected Fatal details: %v", n.Details)
	}
	err, returned := tp.runResult(5 * time.Second)
	if !returned {
		t.Fatal("Run did not return")
	}
	var ue *UpgradeError
	if !errors.As(err, &ue) || ue.Code != "KEY_NOT_FOUND" {
		t.Fatalf("want UpgradeError from Run, got %v", err)
	}
}

func TestPrivateTimeWindowResyncsClockAndRetries(t *testing.T) {
	auth, pub := testAuth(t)
	const serverAhead = 100_000
	conn := newFakeConn()
	dialer := &fakeDialer{script: []func() (Conn, error){
		func() (Conn, error) { return nil, &UpgradeError{Status: 401, Code: "EXCEED_TIME_WINDOW"} },
		func() (Conn, error) { return conn, nil },
	}}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		switch path {
		case "/v2/time":
			return ok(fmt.Sprintf(`{"time":%d}`, time.Now().UnixMilli()+serverAhead))
		case "/v2/balance":
			return ok(`[]`)
		}
		return 404, `{}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	if !doer.called("/v2/time") {
		t.Fatal("clock was never re-measured")
	}
	if dialer.dialCount() != 2 {
		t.Fatalf("want 2 dial attempts, got %d", dialer.dialCount())
	}
	// The second attempt must be signed with the corrected (server) clock.
	u, _ := url.Parse(dialer.dialAt(1).url)
	ts, _ := strconv.ParseInt(u.Query().Get("timestamp"), 10, 64)
	driftFromServer := ts - (time.Now().UnixMilli() + serverAhead)
	if driftFromServer < -30_000 || driftFromServer > 5_000 {
		t.Fatalf("second dial not signed with corrected clock: drift %dms", driftFromServer)
	}
	sigBytes, _ := base64.StdEncoding.DecodeString(u.Query().Get("signature"))
	if !ed25519.Verify(pub, []byte("timestamp="+u.Query().Get("timestamp")), sigBytes) {
		t.Fatal("corrected-clock signature does not verify")
	}
}

// ---------------------------------------------------------------------------
// Private backfill and gap recovery.
// ---------------------------------------------------------------------------

func privateStubDoer() *stubDoer {
	return &stubDoer{handle: func(path string, params url.Values) (int, string) {
		switch path {
		case "/v2/balance":
			return ok(`[{"currency":"krw","available":"1000000"}]`)
		case "/v2/openOrders":
			return ok(`[{"orderId":11,"status":"open","symbol":"` + params.Get("symbol") + `"}]`)
		case "/v2/allOrders":
			return ok(`[{"orderId":11,"status":"filled"}]`)
		case "/v2/myTrades":
			return ok(`[{"tradeId":7,"orderId":11},{"tradeId":8,"orderId":11}]`)
		}
		return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
	}}
}

func TestPrivateInitialBackfill(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := privateStubDoer()
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{
			{Channel: ChannelMyAsset},
			{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}},
			{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}},
		},
		Client:   testClient("http://example.test", doer, auth),
		Dial:     dialer.dial,
		Tunables: testTunables(),
	})

	n := tp.waitNotice(BackfillStart)
	if n.Details["reason"] != "initial" {
		t.Fatalf("want initial backfill, got %v", n.Details)
	}
	d := tp.waitData("balances", func(d Data) bool { return d.Channel == ChannelMyAsset && d.Origin == OriginBackfill })
	if d.Source != "/v2/balance" || !strings.Contains(string(d.Payload), "1000000") {
		t.Fatalf("unexpected balance backfill: %+v", d)
	}
	d = tp.waitData("open orders", func(d Data) bool { return d.Channel == ChannelMyOrder })
	if d.Source != "/v2/openOrders" || d.Symbol != "btc_krw" {
		t.Fatalf("unexpected openOrders backfill: %+v", d)
	}
	tp.waitNotice(BackfillDone)

	// The initial backfill must not run the gap queries.
	if doer.called("/v2/allOrders") || doer.called("/v2/myTrades") {
		t.Fatalf("initial backfill ran gap queries: %v", doer.calls)
	}
}

// TestLazyOpenOrdersScopesSnapshots: with Config.LazyOpenOrders the open-order
// snapshot backfill follows the dynamic tracked set, not the subscribed symbols.
// The initial connect (empty tracked set) snapshots no open orders; tracking a
// symbol then snapshots exactly that one, never the other subscribed pair.
func TestLazyOpenOrdersScopesSnapshots(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var snapMu sync.Mutex
	snapped := map[string]int{}
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		switch path {
		case "/v2/balance":
			return ok(`[{"currency":"krw","available":"1000000"}]`)
		case "/v2/openOrders":
			sym := params.Get("symbol")
			snapMu.Lock()
			snapped[sym]++
			snapMu.Unlock()
			return ok(`[{"orderId":11,"status":"open","symbol":"` + sym + `"}]`)
		}
		return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{
			{Channel: ChannelMyAsset},
			{Channel: ChannelMyOrder, Symbols: []string{"btc_krw", "eth_krw"}},
		},
		Client:         testClient("http://example.test", doer, auth),
		Dial:           dialer.dial,
		Tunables:       testTunables(),
		LazyOpenOrders: true,
	})

	// Initial backfill: lazy + empty tracked set → balances only, no openOrders.
	tp.waitData("balances", func(d Data) bool { return d.Channel == ChannelMyAsset })
	tp.waitNotice(BackfillDone)
	snapMu.Lock()
	initial := len(snapped)
	snapMu.Unlock()
	if initial != 0 {
		t.Fatalf("lazy backfill snapshotted before tracking was set: %v", snapped)
	}

	// Track btc only: exactly btc_krw is snapshotted on demand, never eth_krw.
	tp.s.SetTrackedOrderScopes(orderScopes("btc_krw"))
	d := tp.waitData("btc open orders", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "btc_krw"
	})
	if d.Source != "/v2/openOrders" {
		t.Fatalf("unexpected open-order source: %+v", d)
	}
	snapMu.Lock()
	defer snapMu.Unlock()
	if snapped["btc_krw"] == 0 || snapped["eth_krw"] != 0 {
		t.Fatalf("lazy tracking must snapshot only btc_krw: %v", snapped)
	}
}

// TestBackfillOpenOrdersRefreshesTrackedOnly: direct refreshes are scoped to the
// tracked set. An untracked symbol's stored rows are stale context for a future
// re-track, not current state to refresh.
func TestBackfillOpenOrdersRefreshesTrackedOnly(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var snapMu sync.Mutex
	snapped := map[string]int{}
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		sym := params.Get("symbol")
		snapMu.Lock()
		snapped[sym]++
		snapMu.Unlock()
		return ok(`[]`)
	}}
	s, err := New(Config{
		Subscriptions:  []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw", "eth_krw"}}},
		Client:         testClient("http://example.test", doer, auth),
		Dial:           dialer.dial,
		Tunables:       testTunables(),
		LazyOpenOrders: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetTrackedOrderScopes(orderScopes("eth_krw")) // seeds the set before Run; no fetch yet
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	tp := &tap{t: t, s: s, done: done}
	t.Cleanup(func() {
		cancel()
		if _, ok := tp.runResult(5 * time.Second); !ok {
			t.Errorf("Run did not return after cancel")
		}
	})

	tp.waitData("initial tracked snapshot", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "eth_krw"
	})
	tp.waitNotice(BackfillDone)
	s.BackfillOpenOrders(OrderScope{Symbol: "btc_krw"})
	time.Sleep(20 * time.Millisecond)
	snapMu.Lock()
	if snapped["btc_krw"] != 0 {
		t.Fatalf("untracked direct refresh must not fetch: %v", snapped)
	}
	snapMu.Unlock()

	s.BackfillOpenOrders(OrderScope{Symbol: "eth_krw"})
	tp.waitData("tracked refresh", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "eth_krw"
	})
	snapMu.Lock()
	defer snapMu.Unlock()
	if snapped["eth_krw"] != 2 {
		t.Fatalf("tracked refresh must fetch once after the initial snapshot: %v", snapped)
	}
}

// TestLazyOpenOrdersBatchSnapshotsAll: tracking many symbols at once (the
// "all pairs" toggle) snapshots every newly-tracked symbol via the bounded
// on-demand batch — exactly once each (the in-flight dedup holds).
func TestLazyOpenOrdersBatchSnapshotsAll(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var snapMu sync.Mutex
	snapped := map[string]int{}
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		switch path {
		case "/v2/balance":
			return ok(`[{"currency":"krw","available":"1000000"}]`)
		case "/v2/openOrders":
			sym := params.Get("symbol")
			snapMu.Lock()
			snapped[sym]++
			snapMu.Unlock()
			return ok(`[]`)
		}
		return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
	}}
	all := []string{"btc_krw", "eth_krw", "xrp_krw", "sol_krw"}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{
			{Channel: ChannelMyAsset},
			{Channel: ChannelMyOrder, Symbols: all},
		},
		Client:         testClient("http://example.test", doer, auth),
		Dial:           dialer.dial,
		Tunables:       testTunables(),
		LazyOpenOrders: true,
	})
	tp.waitNotice(BackfillDone)

	tp.s.SetTrackedOrderScopes(orderScopes(all...))
	for _, sym := range all {
		want := sym
		tp.waitData("snapshot "+want, func(d Data) bool {
			return d.Channel == ChannelMyOrder && d.Symbol == want
		})
	}
	// A duplicate track of the same set while snapshots may still be in flight
	// must not double-fetch (in-flight dedup).
	tp.s.SetTrackedOrderScopes(orderScopes(all...))

	snapMu.Lock()
	defer snapMu.Unlock()
	for _, sym := range all {
		if snapped[sym] != 1 {
			t.Fatalf("symbol %s want exactly one snapshot, got %d: %v", sym, snapped[sym], snapped)
		}
	}
}

// TestOnDemandBackfillAfterRunReturnsIsInert: the consumer-facing on-demand
// calls (SetTrackedOrderScopes / BackfillOpenOrders) are documented as safe
// from any goroutine at any time — including after the session has stopped
// (the TUI can race a key press against a session dying underneath it). Once
// Run has returned they must be inert: before the startBg gate they spawned a
// goroutine whose failed fetch emitted on the CLOSED events channel (a panic).
func TestOnDemandBackfillAfterRunReturnsIsInert(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := privateStubDoer()
	tp, cancel := startSession(t, Config{
		Subscriptions:  []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}}},
		Client:         testClient("http://example.test", doer, auth),
		Dial:           dialer.dial,
		Tunables:       testTunables(),
		LazyOpenOrders: true,
	})
	tp.waitNotice(Connected)

	cancel()
	if _, ok := tp.runResult(5 * time.Second); !ok {
		t.Fatal("Run did not return after cancel")
	}
	// The events channel is closed now. These must be no-ops, not panics.
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	tp.s.BackfillOpenOrders(OrderScope{Symbol: "btc_krw"})
	// A wrongly-spawned goroutine would panic the test binary; give it a beat.
	time.Sleep(20 * time.Millisecond)
}

// TestReconnectGapWindowUsesServerClock: the gap walk's startTime is a filter
// the SERVER evaluates against its own row timestamps, so the window must be
// anchored on the server-clock estimate, not the bare local clock — a local
// clock running further ahead of the server than the 60s overlap would
// otherwise silently start the walk late and miss gap rows.
func TestReconnectGapWindowUsesServerClock(t *testing.T) {
	const offsetMs = int64(3_600_000) // server 1h ahead of the local clock
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}

	var mu sync.Mutex
	var starts []int64
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		if path == "/v2/myTrades" {
			if v := params.Get("startTime"); v != "" {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					return 400, `{"success":false,"error":{"code":400,"message":"BAD_START_TIME"}}`
				}
				mu.Lock()
				starts = append(starts, n)
				mu.Unlock()
			}
			return ok(`[]`)
		}
		return ok(`[]`)
	}}

	client := testClient("http://example.test", doer, auth)
	client.Clock.(*clock.State).Install(offsetMs, 0)

	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}}},
		Client:        client,
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})
	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDone) // initial: no myTrades walk

	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDone) // reconnect: the gap walk ran

	mu.Lock()
	defer mu.Unlock()
	if len(starts) == 0 {
		t.Fatal("the reconnect gap walk never sent a startTime")
	}
	// The window start is serverNow - downtime - overlap. With a 1h offset and a
	// sub-second downtime that is ~now + 1h - 60s; anchored on the local clock it
	// would be ~now - 60s. Split the difference to discriminate robustly.
	nowMs := time.Now().UnixMilli()
	if got := starts[0]; got < nowMs+offsetMs/2 {
		t.Fatalf("gap startTime %d is anchored on the local clock (now=%d, offset=%d): the walk starts %dms late in server time",
			got, nowMs, offsetMs, offsetMs-60_000)
	}
}

// TestPrivateSnapshotOnlyBackfill: with Config.BackfillSnapshotOnly the private
// backfill fetches balances + the open-order snapshot on every connect but never
// the per-symbol allOrders/myTrades gap walks, even across a reconnect — and it
// announces the reduced recovery (BACKFILL_SNAPSHOT_ONLY: Info on initial, Warn
// on reconnect).
func TestPrivateSnapshotOnlyBackfill(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := privateStubDoer()
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{
			{Channel: ChannelMyAsset},
			{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}},
			{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}},
		},
		BackfillSnapshotOnly: true,
		Client:               testClient("http://example.test", doer, auth),
		Dial:                 dialer.dial,
		Tunables:             testTunables(),
	})

	if n := tp.waitNotice(BackfillSnapshotOnly); n.Level != LevelInfo {
		t.Fatalf("initial snapshot-only notice should be Info, got %v", n.Level)
	}
	tp.waitData("open orders", func(d Data) bool { return d.Channel == ChannelMyOrder && d.Source == "/v2/openOrders" })
	tp.waitNotice(BackfillDone)

	// Force a reconnect: the gap walks must still be skipped, and the notice
	// escalates to Warn (a real, accepted recovery loss).
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	if n := tp.waitNotice(BackfillSnapshotOnly); n.Level != LevelWarn {
		t.Fatalf("reconnect snapshot-only notice should be Warn, got %v", n.Level)
	}
	tp.waitData("open orders (reconnect)", func(d Data) bool { return d.Channel == ChannelMyOrder && d.Source == "/v2/openOrders" })
	tp.waitNotice(BackfillDone)

	if doer.called("/v2/allOrders") || doer.called("/v2/myTrades") {
		t.Fatalf("snapshot-only ran gap walks: %v", doer.calls)
	}
	if !doer.called("/v2/balance") || !doer.called("/v2/openOrders") {
		t.Fatalf("snapshot-only skipped a snapshot fetch: %v", doer.calls)
	}
}

func TestPrivateReconnectBackfillsGapAndDedupes(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := privateStubDoer()
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{
			{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}},
			{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}},
		},
		Client:   testClient("http://example.test", doer, auth),
		Dial:     dialer.dial,
		Tunables: testTunables(),
	})

	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDone)

	// A fill arrives live: tradeId 7.
	conn1.serverPush(t, `{"channelType":"myTrade","timestamp":1700000000000,"symbol":"btc_krw","trade":{"trades":[{"tradeId":7,"orderId":11}]}}`)
	tp.waitData("live fill", func(d Data) bool { return d.Channel == ChannelMyTrade && d.Origin == OriginRealtime })

	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)

	// Reconnect backfill: gap queries run; the REST rows {7, 8} must be deduped
	// down to {8} because 7 was already delivered live.
	d := tp.waitData("gap fills", func(d Data) bool { return d.Channel == ChannelMyTrade && d.Origin == OriginBackfill })
	if d.Source != "/v2/myTrades" {
		t.Fatalf("unexpected source: %+v", d)
	}
	if strings.Contains(string(d.Payload), `"tradeId":7`) || !strings.Contains(string(d.Payload), `"tradeId":8`) {
		t.Fatalf("dedupe failed: %s", d.Payload)
	}
	d = tp.waitData("gap orders", func(d Data) bool { return d.Channel == ChannelMyOrder && d.Source == "/v2/allOrders" })
	if !strings.Contains(string(d.Payload), `"orderId":11`) {
		t.Fatalf("unexpected allOrders payload: %s", d.Payload)
	}

	// A live frame replaying tradeId 8 after the backfill must be suppressed:
	// push it, then a fresh fill, and verify only the fresh one comes out.
	conn2.serverPush(t, `{"channelType":"myTrade","timestamp":1700000002000,"symbol":"btc_krw","trade":{"trades":[{"tradeId":8,"orderId":11},{"tradeId":9,"orderId":11}]}}`)
	d = tp.waitData("post-dedupe fill", func(d Data) bool { return d.Channel == ChannelMyTrade && d.Origin == OriginRealtime })
	if strings.Contains(string(d.Payload), `"tradeId":8`) || !strings.Contains(string(d.Payload), `"tradeId":9`) {
		t.Fatalf("live replay not deduped: %s", d.Payload)
	}
}

// TestPrivateGapHistoryPaginates: a reconnect gap with more fills than one
// page (newest-first) must be paged backward via endTime until a short page,
// with every row delivered exactly once and no DATA_GAP.
func TestPrivateGapHistoryPaginates(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tsBase := time.Now().UnixMilli() - 30_000 // tradedAt = tsBase + tradeId, inside the gap window

	fillRows := func(fromID, toID int) string { // descending ids, newest-first
		var rows []string
		for id := fromID; id >= toID; id-- {
			rows = append(rows, fmt.Sprintf(`{"tradeId":%d,"tradedAt":%d}`, id, tsBase+int64(id)))
		}
		return `[` + strings.Join(rows, ",") + `]`
	}
	var myTradesCalls []url.Values
	var mu sync.Mutex
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		switch path {
		case "/v2/myTrades":
			mu.Lock()
			myTradesCalls = append(myTradesCalls, params)
			mu.Unlock()
			if params.Get("endTime") == "" {
				return ok(fillRows(2000, 1001)) // 1000 rows: saturated
			}
			return ok(fillRows(1001, 950)) // 52 rows incl. the boundary overlap: short
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)

	got := map[int]int{}
	for len(got) < 1051 { // ids 950..2000
		d := tp.waitData("paged fills", func(d Data) bool { return d.Channel == ChannelMyTrade && d.Origin == OriginBackfill })
		if d.ServerTime <= 0 {
			t.Fatalf("backfill event without ServerTime: %+v", d)
		}
		for _, id := range payloadTradeIDs(t, d.Payload) {
			got[id]++
		}
	}
	for id, count := range got {
		if count != 1 {
			t.Fatalf("tradeId %d delivered %d times", id, count)
		}
	}
	tp.waitNotice(BackfillDone)
	mu.Lock()
	calls := myTradesCalls
	mu.Unlock()
	if len(calls) != 2 || calls[1].Get("endTime") == "" {
		t.Fatalf("expected 2 myTrades calls with endTime on the second, got %v", calls)
	}
	for _, ev := range append(tp.seen, tp.pending...) {
		if n, isNotice := ev.(Notice); isNotice && n.Code == DataGap {
			t.Fatalf("unexpected DATA_GAP on a fully paged gap: %v", n)
		}
	}
}

// TestPrivateGapHistoryPaginatesAscending: the server's sort order is
// observed, not contractual — if a saturated page comes back OLDEST-FIRST,
// pagination must advance startTime forward instead of declaring the window
// covered (which would silently drop the newest rows).
func TestPrivateGapHistoryPaginatesAscending(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tsBase := time.Now().UnixMilli() - 30_000

	fillRowsAsc := func(fromID, toID int) string { // ascending ids, oldest-first
		var rows []string
		for id := fromID; id <= toID; id++ {
			rows = append(rows, fmt.Sprintf(`{"tradeId":%d,"tradedAt":%d}`, id, tsBase+int64(id)))
		}
		return `[` + strings.Join(rows, ",") + `]`
	}
	calls := 0
	var mu sync.Mutex
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		if path == "/v2/myTrades" {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return ok(fillRowsAsc(1001, 2000)) // 1000 rows, oldest-first: saturated
			}
			return ok(fillRowsAsc(2000, 2050)) // short page with the boundary overlap
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.serverDrop()
	tp.waitNotice(Connected)

	got := map[int]int{}
	for len(got) < 1050 { // ids 1001..2050
		d := tp.waitData("asc paged fills", func(d Data) bool { return d.Channel == ChannelMyTrade && d.Origin == OriginBackfill })
		for _, id := range payloadTradeIDs(t, d.Payload) {
			got[id]++
		}
	}
	for id, count := range got {
		if count != 1 {
			t.Fatalf("tradeId %d delivered %d times", id, count)
		}
	}
	tp.waitNotice(BackfillDone)
	for _, ev := range append(tp.seen, tp.pending...) {
		if n, isNotice := ev.(Notice); isNotice && n.Code == DataGap {
			t.Fatalf("unexpected DATA_GAP on a fully paged ascending gap: %v", n)
		}
	}
}

// TestPrivateGapHistoryIncompleteRaisesDataGap: pagination that cannot make
// progress (every page saturated with the same oldest timestamp) must stop
// and TELL the consumer via DATA_GAP instead of silently under-delivering.
func TestPrivateGapHistoryIncompleteRaisesDataGap(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tsSame := time.Now().UnixMilli() - 10_000

	var rows []string
	for id := 3000; id > 2000; id-- { // 1000 rows, ALL the same timestamp
		rows = append(rows, fmt.Sprintf(`{"tradeId":%d,"tradedAt":%d}`, id, tsSame))
	}
	page := `[` + strings.Join(rows, ",") + `]`
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/myTrades" {
			return ok(page)
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.serverDrop()
	tp.waitNotice(Connected)
	n := tp.waitNotice(DataGap)
	if n.Details["source"] != "/v2/myTrades" || n.Details["channel"] != ChannelMyTrade {
		t.Fatalf("unexpected DataGap details: %v", n.Details)
	}
	tp.waitNotice(BackfillDone)
}

func TestUpgrade4xxWithoutEnvelopeRetries(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: []func() (Conn, error){
		func() (Conn, error) { return nil, &UpgradeError{Status: 403, Body: "<html>proxy denied</html>"} },
		func() (Conn, error) { return conn, nil },
	}}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) { return ok(`[]`) }}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(ConnectFailed) // not Fatal: no Korbit error envelope
	tp.waitNotice(Connected)
}

func TestBackfillDisabled(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := &stubDoer{}
	tp, _ := startSession(t, Config{
		Subscriptions:   []Subscription{{Channel: ChannelMyAsset}},
		Client:          testClient("http://example.test", doer, auth),
		DisableBackfill: true,
		Dial:            dialer.dial,
		Tunables:        testTunables(),
	})

	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDisabled)
	doer.mu.Lock()
	defer doer.mu.Unlock()
	for _, call := range doer.calls {
		// The proactive clock measurement (/v2/time) is not backfill and is
		// allowed; any data fetch is a violation.
		if !strings.HasSuffix(call, "/v2/time") {
			t.Fatalf("backfill disabled but REST data was fetched: %v", doer.calls)
		}
	}
}

func TestBackfillFailureIsReportedAndStreamContinues(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		return 400, `{"success":false,"error":{"code":400,"message":"SOMETHING_BROKE"}}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	n := tp.waitNotice(BackfillFailed)
	if n.Details["source"] != "/v2/balance" {
		t.Fatalf("unexpected BackfillFailed details: %v", n.Details)
	}
	tp.waitNotice(BackfillDone)

	// Live frames still flow after the failed backfill.
	conn.serverPush(t, `{"channelType":"myAsset","timestamp":1700000000000,"asset":{"assets":[{"currency":"btc","available":"1"}]}}`)
	tp.waitData("live asset delta", func(d Data) bool { return d.Channel == ChannelMyAsset && d.Origin == OriginRealtime })
}

// TestFailedBackfillSelfHealsWhileConnected: a transiently-failed backfill
// call (network/5xx after the in-call ladder) is re-run with backoff while
// the SAME connection stays up — a fresh BACKFILL_START/DONE pass with
// reason="retry" scoped to the failed calls only — so the stream heals
// without waiting for a reconnect.
func TestFailedBackfillSelfHealsWhileConnected(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var mu sync.Mutex
	balCalls := 0
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		switch path {
		case "/v2/balance":
			mu.Lock()
			balCalls++
			n := balCalls
			mu.Unlock()
			if n == 1 {
				return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
			}
			return ok(`[{"currency":"krw","available":"1000000"}]`)
		case "/v2/openOrders":
			return ok(`[]`)
		}
		return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{
			{Channel: ChannelMyAsset},
			{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}},
		},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		BackfillRetryBudgetMs: 1, // fail the in-call ladder fast; the pass-level retry is under test
	})

	n := tp.waitNotice(BackfillFailed)
	if n.Details["source"] != "/v2/balance" {
		t.Fatalf("unexpected BackfillFailed details: %v", n.Details)
	}
	tp.wait("failing pass done", func(ev Event) bool {
		d, isNotice := ev.(Notice)
		return isNotice && d.Code == BackfillDone && d.Details["failures"] == 1
	})

	// The self-heal pass: reason "retry", scoped to the failed call only (the
	// channels detail derives from the re-run units).
	retry := tp.wait("retry pass start", func(ev Event) bool {
		d, isNotice := ev.(Notice)
		return isNotice && d.Code == BackfillStart && d.Details["reason"] == "retry"
	}).(Notice)
	if retry.Details["attempt"] != 1 {
		t.Fatalf("first retry must carry attempt=1: %v", retry.Details)
	}
	if chs, _ := retry.Details["channels"].([]string); len(chs) != 1 || chs[0] != ChannelMyAsset {
		t.Fatalf("retry must be scoped to the failed call's channel: %v", retry.Details)
	}
	d := tp.waitData("healed balances", func(d Data) bool {
		return d.Channel == ChannelMyAsset && d.Origin == OriginBackfill
	})
	if !strings.Contains(string(d.Payload), "1000000") {
		t.Fatalf("unexpected healed balance payload: %s", d.Payload)
	}
	tp.wait("retry pass done clean", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "retry" && n.Details["failures"] == 0
	})

	// Healed on the same connection, and the succeeded channel was not re-run.
	if got := dialer.dialCount(); got != 1 {
		t.Fatalf("self-heal must not reconnect (dials: %d)", got)
	}
	if got := doer.countCalls("/v2/openOrders"); got != 1 {
		t.Fatalf("the succeeded openOrders channel must not be retried (calls: %d)", got)
	}
}

// TestBackfillRetryScopedToFailedCalls: the self-heal retry re-runs only the
// leaf calls that actually failed — within one channel, a symbol whose
// snapshot succeeded is never replayed while a sibling heals, so a broad
// fan-out cannot feed the rate limit (or outage) that caused the failure.
func TestBackfillRetryScopedToFailedCalls(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var mu sync.Mutex
	snapCalls := map[string]int{}
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		sym := params.Get("symbol")
		mu.Lock()
		snapCalls[sym]++
		n := snapCalls[sym]
		mu.Unlock()
		if sym == "eth_krw" && n == 1 {
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw", "eth_krw"}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		BackfillRetryBudgetMs: 1,
	})

	tp.waitNotice(BackfillFailed)
	// The retry pass names exactly the one failed call.
	retry := tp.wait("retry pass start", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillStart && n.Details["reason"] == "retry"
	}).(Notice)
	units, _ := retry.Details["units"].([]map[string]any)
	if len(units) != 1 || units[0]["source"] != "/v2/openOrders" || units[0]["symbol"] != "eth_krw" {
		t.Fatalf("retry must carry exactly the failed unit: %v", retry.Details)
	}
	tp.wait("retry pass done clean", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "retry" && n.Details["failures"] == 0
	})

	mu.Lock()
	defer mu.Unlock()
	if snapCalls["btc_krw"] != 1 {
		t.Fatalf("the succeeded symbol's snapshot must not be replayed (calls: %d)", snapCalls["btc_krw"])
	}
	if snapCalls["eth_krw"] != 2 {
		t.Fatalf("the failed symbol must be re-run exactly once (calls: %d)", snapCalls["eth_krw"])
	}
}

// TestBackfillRetryUnitDropsWhenUntracked: under LazyOpenOrders, a failed
// snapshot's retry unit self-drops once its symbol leaves the tracked set —
// the self-heal loop must not keep re-snapshotting a symbol nobody wants.
func TestBackfillRetryUnitDropsWhenUntracked(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		switch path {
		case "/v2/openOrders":
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		case "/v2/allOrders":
			return ok(`[]`) // the reconnect gap walk over the static symbol succeeds
		}
		return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		LazyOpenOrders:        true,
		BackfillRetryBudgetMs: 1,
	})

	// Initial pass: nothing tracked, nothing to snapshot.
	tp.wait("clean initial pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "initial" && n.Details["failures"] == 0
	})

	// Track a symbol (the on-demand snapshot fails and starts its own
	// self-heal loop, which the reconnect's generation bump kills), then
	// reconnect so the per-connect pass snapshots the tracked set and leaves a
	// failed unit behind for its self-heal loop.
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	tp.waitNotice(BackfillFailed)
	conn1.serverDrop()
	tp.wait("failing reconnect pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "reconnect" && n.Details["failures"] == 1
	})
	tp.wait("a retry pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillStart && n.Details["reason"] == "retry"
	})

	// Untrack: the unit is stale, so the retry loop drops it and exits. The
	// call count stabilizes (a surviving 1-5ms loop would add dozens of calls
	// over 50ms).
	tp.s.SetTrackedOrderScopes(nil)
	time.Sleep(50 * time.Millisecond)
	before := doer.countCalls("/v2/openOrders")
	time.Sleep(50 * time.Millisecond)
	if after := doer.countCalls("/v2/openOrders"); after != before {
		t.Fatalf("a stale snapshot unit kept retrying: %d -> %d openOrders calls", before, after)
	}
}

// TestBackfillRetryScopedToFailedAccountSeq: retry units carry their
// accountSeq, so with multiple pinned accounts only the failed account's call
// is re-run — the sibling account that succeeded is never replayed — and the
// retry notice names the account.
func TestBackfillRetryScopedToFailedAccountSeq(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var mu sync.Mutex
	balCalls := map[string]int{} // per-accountSeq /v2/balance calls
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		if path != "/v2/balance" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		seq := params.Get("accountSeq")
		mu.Lock()
		balCalls[seq]++
		n := balCalls[seq]
		mu.Unlock()
		if seq == "8" && n == 1 {
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		}
		return ok(`[{"currency":"krw","available":"1000000"}]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyAsset, AccountSeqs: []int{7, 8}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		BackfillRetryBudgetMs: 1,
	})

	tp.waitNotice(BackfillFailed)
	retry := tp.wait("retry pass start", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillStart && n.Details["reason"] == "retry"
	}).(Notice)
	units, _ := retry.Details["units"].([]map[string]any)
	if len(units) != 1 || units[0]["source"] != "/v2/balance" || units[0]["accountSeq"] != 8 {
		t.Fatalf("retry must carry exactly the failed account's unit: %v", retry.Details)
	}
	tp.wait("retry pass done clean", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "retry" && n.Details["failures"] == 0
	})

	mu.Lock()
	defer mu.Unlock()
	if balCalls["7"] != 1 {
		t.Fatalf("the succeeded account's balance must not be replayed (calls: %d)", balCalls["7"])
	}
	if balCalls["8"] != 2 {
		t.Fatalf("the failed account must be re-run exactly once (calls: %d)", balCalls["8"])
	}
}

// TestBackfillRetryLeavesFatalSiblingAlone: a fatal and a transient failure in
// the SAME channel are separated by the unit granularity — the retry re-runs
// only the transient call, while the definitively-rejected sibling waits for
// the next (re)connect. The fatal sibling is not part of the retry unit set.
func TestBackfillRetryLeavesFatalSiblingAlone(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var mu sync.Mutex
	snapCalls := map[string]int{}
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		sym := params.Get("symbol")
		mu.Lock()
		snapCalls[sym]++
		n := snapCalls[sym]
		mu.Unlock()
		switch {
		case sym == "btc_krw": // definitive rejection: never retried in-connection
			return 403, `{"success":false,"error":{"code":403,"message":"NOT_AUTHORIZED"}}`
		case sym == "eth_krw" && n == 1: // transient: heals on the retry
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw", "eth_krw"}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		BackfillRetryBudgetMs: 1,
	})

	// The initial pass counts both failures but enqueues only the transient one.
	tp.wait("failing initial pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "initial" && n.Details["failures"] == 2
	})
	retry := tp.wait("retry pass start", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillStart && n.Details["reason"] == "retry"
	}).(Notice)
	units, _ := retry.Details["units"].([]map[string]any)
	if len(units) != 1 || units[0]["symbol"] != "eth_krw" {
		t.Fatalf("only the transient failure may be enqueued: %v", retry.Details)
	}
	tp.wait("retry pass done clean", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "retry" && n.Details["failures"] == 0
	})

	// The retry loop is done (eth healed); the fatal sibling was never replayed.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if snapCalls["btc_krw"] != 1 {
		t.Fatalf("the fatally-failed symbol must not be replayed (calls: %d)", snapCalls["btc_krw"])
	}
	if snapCalls["eth_krw"] != 2 {
		t.Fatalf("the transient symbol must be re-run exactly once (calls: %d)", snapCalls["eth_krw"])
	}
}

// TestBackfillRetryEndsQuietlyWhenAllUnitsStale: when every pending unit goes
// stale before the next retry pass (its symbol untracked), the pass runs
// nothing and emits nothing — no BACKFILL_START/DONE for work outside the
// tracked set — and the loop ends.
func TestBackfillRetryEndsQuietlyWhenAllUnitsStale(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	var mu sync.Mutex
	snapCalls := 0
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		switch path {
		case "/v2/openOrders":
			// The on-demand snapshot (call 1) succeeds — so no on-demand
			// self-heal loop exists to race the assertions below — and the
			// reconnect pass's snapshot (call 2 on) fails, leaving the failed
			// unit for the per-connect self-heal loop under test.
			mu.Lock()
			snapCalls++
			n := snapCalls
			mu.Unlock()
			if n == 1 {
				return ok(`[]`)
			}
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		case "/v2/allOrders":
			return ok(`[]`)
		}
		return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
	}}
	// A slow retry backoff (240-360ms after jitter) leaves a wide window to
	// untrack the symbol BEFORE the first retry pass fires.
	tun := testTunables()
	tun.BackfillRetryMinMs, tun.BackfillRetryMaxMs = 300, 600
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              tun,
		LazyOpenOrders:        true,
		BackfillRetryBudgetMs: 1,
	})

	// Track a symbol (its on-demand snapshot succeeds), then reconnect so the
	// per-connect pass re-snapshots the tracked set and leaves a failed unit
	// pending; untrack it before the first retry fires.
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	tp.waitData("on-demand snapshot", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "eth_krw" && d.Origin == OriginBackfill
	})
	conn1.serverDrop()
	tp.wait("failing reconnect pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "reconnect" && n.Details["failures"] == 1
	})
	tp.s.SetTrackedOrderScopes(nil)

	// Drain events past the would-be first and second retry slots: no retry
	// pass may announce itself, and no further snapshot call may fire.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		ev, ok := tp.next(time.Until(deadline))
		if !ok {
			break
		}
		if n, isNotice := ev.(Notice); isNotice && n.Code == BackfillStart && n.Details["reason"] == "retry" {
			t.Fatalf("an all-stale retry pass must emit no notices: %v", n.Details)
		}
	}
	if got := doer.countCalls("/v2/openOrders"); got != 2 {
		t.Fatalf("stale units must not be re-run (openOrders calls: %d, want 2: on-demand + reconnect)", got)
	}
}

// TestOnDemandBackfillSelfHeals: an on-demand open-order snapshot (a symbol
// tracked mid-session under LazyOpenOrders) that fails transiently self-heals
// with backoff on the SAME connection, exactly like a per-connect pass's
// failure — a symbol tracked mid-session must also become ready eventually.
func TestOnDemandBackfillSelfHeals(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var mu sync.Mutex
	snapCalls := 0
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		mu.Lock()
		snapCalls++
		n := snapCalls
		mu.Unlock()
		if n == 1 {
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		LazyOpenOrders:        true,
		BackfillRetryBudgetMs: 1,
	})

	tp.wait("clean initial pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "initial" && n.Details["failures"] == 0
	})
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	tp.waitNotice(BackfillFailed)

	// The self-heal pass names the failed on-demand call, and the snapshot lands.
	retry := tp.wait("retry pass start", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillStart && n.Details["reason"] == "retry"
	}).(Notice)
	units, _ := retry.Details["units"].([]map[string]any)
	if len(units) != 1 || units[0]["source"] != "/v2/openOrders" || units[0]["symbol"] != "eth_krw" {
		t.Fatalf("retry must carry the failed on-demand unit: %v", retry.Details)
	}
	tp.waitData("healed snapshot", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "eth_krw" && d.Origin == OriginBackfill
	})
	tp.wait("retry pass done clean", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "retry" && n.Details["failures"] == 0
	})
	if got := dialer.dialCount(); got != 1 {
		t.Fatalf("on-demand self-heal must not need a reconnect (dials: %d)", got)
	}
}

// TestRetrackDuringFailingSnapshotHeals: a re-track that lands while the same
// symbol's earlier on-demand fetch is still in flight is deduped — and the
// dedupe is CORRECT, not lossy, because the failing fetch's retry unit already
// guarantees the snapshot. The symbol still becomes ready, with no duplicate
// call from the re-track.
func TestRetrackDuringFailingSnapshotHeals(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	firstIn := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	snapCalls := 0
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		mu.Lock()
		snapCalls++
		n := snapCalls
		mu.Unlock()
		if n == 1 {
			close(firstIn)
			<-release // hold the first fetch in flight while the test re-tracks
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		LazyOpenOrders:        true,
		BackfillRetryBudgetMs: 1,
	})

	// Let the initial per-connect pass finish over the empty tracked set first,
	// so the only snapshot calls in play are the on-demand ones under test.
	tp.wait("clean initial pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "initial" && n.Details["failures"] == 0
	})
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	<-firstIn // the fetch is now in flight
	tp.s.SetTrackedOrderScopes(nil)
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw")) // deduped against the in-flight fetch
	close(release)                                     // the fetch now fails

	// The retry unit heals the symbol; the deduped re-track fired no extra call.
	tp.waitNotice(BackfillFailed)
	tp.waitData("healed snapshot", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "eth_krw" && d.Origin == OriginBackfill
	})
	tp.wait("retry pass done clean", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "retry" && n.Details["failures"] == 0
	})
	mu.Lock()
	defer mu.Unlock()
	if snapCalls != 2 {
		t.Fatalf("want exactly 2 openOrders calls (failed + healed), got %d", snapCalls)
	}
}

// TestRetrackAfterStaleDropFiresFreshSnapshot: when an on-demand self-heal
// loop ends by dropping its last unit as stale (symbol untracked), the
// symbol's in-flight mark is released — so a later re-track fires a FRESH
// snapshot instead of being deduped against an exited loop.
func TestRetrackAfterStaleDropFiresFreshSnapshot(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var mu sync.Mutex
	heal := false
	snapCalls := 0
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		mu.Lock()
		snapCalls++
		healed := heal
		mu.Unlock()
		if !healed {
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		}
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		LazyOpenOrders:        true,
		BackfillRetryBudgetMs: 1,
	})

	// Track: the on-demand fetch fails and its self-heal loop starts churning.
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	tp.waitNotice(BackfillFailed)

	// Untrack: the loop drops its unit as stale and exits, releasing the
	// symbol's in-flight mark. Wait for the churn to stop.
	tp.s.SetTrackedOrderScopes(nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		before := snapCalls
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		after := snapCalls
		heal = after == before // stop failing once the loop is proven dead
		mu.Unlock()
		if after == before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the on-demand retry loop never stopped after untrack")
		}
	}

	// Re-track: the released mark lets a fresh fetch through, and it lands.
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	tp.waitData("fresh snapshot after re-track", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "eth_krw" && d.Origin == OriginBackfill
	})
}

// TestSameEpochRetrackSkipsSnapshot: a scope re-tracked while the SAME private
// connection is up is NOT re-snapshotted — its recorded baseline plus the
// lossless account-wide live feed already make it current, so flipping focus
// back and forth costs no REST calls. An explicit BackfillOpenOrders still
// forces a fetch, and a reconnect re-baselines the tracked set (new generation
// → the memo no longer matches), after which same-epoch re-tracks skip again.
func TestSameEpochRetrackSkipsSnapshot(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	var mu sync.Mutex
	snapCalls := 0
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		mu.Lock()
		snapCalls++
		mu.Unlock()
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:  []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw", "eth_krw"}}},
		Client:         testClient("http://example.test", doer, auth),
		Dial:           dialer.dial,
		Tunables:       testTunables(),
		LazyOpenOrders: true,
	})
	calls := func() int { mu.Lock(); defer mu.Unlock(); return snapCalls }
	ethSnapshot := func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "eth_krw" && d.Origin == OriginBackfill
	}

	// Let the initial per-connect pass finish first: the CONNECTED notice
	// precedes onUp, so only BACKFILL_DONE proves the generation is stored and
	// the pass has read the (empty) tracked set — otherwise the on-demand fetch
	// below races the pass and the call counts are timing-dependent.
	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDone)

	// Track eth: the snapshot lands and records the scope's baseline.
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	tp.waitData("tracked snapshot", ethSnapshot)
	if got := calls(); got != 1 {
		t.Fatalf("want 1 snapshot call after tracking, got %d", got)
	}

	// Untrack, then re-track within the same connection: no new fetch.
	tp.s.SetTrackedOrderScopes(nil)
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	time.Sleep(30 * time.Millisecond)
	if got := calls(); got != 1 {
		t.Fatalf("same-epoch re-track must not re-fetch, got %d calls", got)
	}

	// An explicit refresh bypasses the memo.
	tp.s.BackfillOpenOrders(OrderScope{Symbol: "eth_krw"})
	tp.waitData("explicit refresh", ethSnapshot)
	if got := calls(); got != 2 {
		t.Fatalf("explicit refresh must fetch, got %d calls", got)
	}

	// Reconnect: the per-connect pass re-baselines the tracked scope under the
	// new generation; a later same-epoch re-track skips again.
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	tp.waitData("reconnect re-baseline", ethSnapshot)
	if got := calls(); got != 3 {
		t.Fatalf("reconnect must re-baseline the tracked scope, got %d calls", got)
	}
	tp.s.SetTrackedOrderScopes(nil)
	tp.s.SetTrackedOrderScopes(orderScopes("eth_krw"))
	time.Sleep(30 * time.Millisecond)
	if got := calls(); got != 3 {
		t.Fatalf("re-track in the new epoch must not re-fetch, got %d calls", got)
	}
}

// TestTrackingScopedToAccount: with several sub-accounts subscribed, a tracked
// scope snapshots exactly its own {account, symbol} — never a cross-product
// over every subscribed account — and a scope whose account the myOrder
// subscription doesn't cover is ignored.
func TestTrackingScopedToAccount(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	var mu sync.Mutex
	var calls []string // "symbol/accountSeq"
	doer := &stubDoer{handle: func(path string, params url.Values) (int, string) {
		if path != "/v2/openOrders" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		mu.Lock()
		calls = append(calls, params.Get("symbol")+"/"+params.Get("accountSeq"))
		mu.Unlock()
		return ok(`[]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:  []Subscription{{Channel: ChannelMyOrder, Symbols: []string{"btc_krw", "eth_krw"}, AccountSeqs: []int{1, 2}}},
		Client:         testClient("http://example.test", doer, auth),
		Dial:           dialer.dial,
		Tunables:       testTunables(),
		LazyOpenOrders: true,
	})

	// Let the initial (empty-tracked-set) pass finish so the on-demand fetch
	// below can't race it (see TestSameEpochRetrackSkipsSnapshot).
	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDone)

	// Track btc under account 2 only, plus a scope under an unsubscribed
	// account (7) that must be dropped.
	tp.s.SetTrackedOrderScopes([]OrderScope{
		{AccountSeq: 2, Symbol: "btc_krw"},
		{AccountSeq: 7, Symbol: "eth_krw"},
	})
	d := tp.waitData("scoped snapshot", func(d Data) bool {
		return d.Channel == ChannelMyOrder && d.Symbol == "btc_krw" && d.Origin == OriginBackfill
	})
	if d.AccountSeq == nil || *d.AccountSeq != 2 {
		t.Fatalf("snapshot must carry its scope's account, got %+v", d.AccountSeq)
	}
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "btc_krw/2" {
		t.Fatalf("want exactly one snapshot for btc_krw under account 2, got %v", calls)
	}
}

// TestFatalBackfillFailureIsNotRetried: a definitively-rejected recovery call
// (4xx — auth/permission/config class, the in-call ladder already refused to
// retry it) is not self-healed; it waits for the next (re)connect. Retrying a
// provably non-transient call forever would be noise, not healing.
func TestFatalBackfillFailureIsNotRetried(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		return 403, `{"success":false,"error":{"code":403,"message":"NOT_AUTHORIZED"}}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        testClient("http://example.test", doer, auth),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(BackfillFailed)
	tp.waitNotice(BackfillDone)
	// With the 1-5ms retry ladder, dozens of retries would have fired by now if
	// fatal failures were re-run.
	time.Sleep(50 * time.Millisecond)
	if got := doer.countCalls("/v2/balance"); got != 1 {
		t.Fatalf("a fatal backfill failure must not be retried (calls: %d)", got)
	}
}

// TestBackfillRetryAbandonedOnReconnect: a standing self-heal loop stops once
// its connection is superseded — the new connect's own full pass owns
// recovery, so no stale-generation retry keeps hitting REST afterwards.
func TestBackfillRetryAbandonedOnReconnect(t *testing.T) {
	auth, _ := testAuth(t)
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	var mu sync.Mutex
	heal := false
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path != "/v2/balance" {
			return 404, `{"success":false,"error":{"code":404,"message":"NOT_FOUND"}}`
		}
		mu.Lock()
		healed := heal
		mu.Unlock()
		if !healed {
			return 500, `{"success":false,"error":{"code":500,"message":"INTERNAL_SERVER_ERROR"}}`
		}
		return ok(`[{"currency":"krw","available":"1000000"}]`)
	}}
	tp, _ := startSession(t, Config{
		Subscriptions:         []Subscription{{Channel: ChannelMyAsset}},
		Client:                testClient("http://example.test", doer, auth),
		Dial:                  dialer.dial,
		Tunables:              testTunables(),
		BackfillRetryBudgetMs: 1,
	})

	// The initial pass fails and the self-heal loop starts churning.
	tp.waitNotice(BackfillFailed)
	tp.wait("a retry pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillStart && n.Details["reason"] == "retry"
	})

	// Drop the connection; the server recovers; the reconnect pass succeeds.
	conn1.serverDrop()
	mu.Lock()
	heal = true
	mu.Unlock()
	tp.wait("clean reconnect pass", func(ev Event) bool {
		n, isNotice := ev.(Notice)
		return isNotice && n.Code == BackfillDone && n.Details["reason"] == "reconnect" && n.Details["failures"] == 0
	})

	// The old generation's loop must be gone: the call count stabilizes (a
	// surviving 1-5ms loop would add dozens of calls over 50ms).
	time.Sleep(50 * time.Millisecond)
	before := doer.countCalls("/v2/balance")
	time.Sleep(50 * time.Millisecond)
	if after := doer.countCalls("/v2/balance"); after != before {
		t.Fatalf("a superseded retry loop kept running: %d -> %d balance calls", before, after)
	}
}

// ---------------------------------------------------------------------------
// Public trade continuity: dedupe, gap patch, DATA_GAP.
// ---------------------------------------------------------------------------

func tradeFrame(snapshot bool, ids ...int) string {
	rows := make([]string, len(ids))
	for i, id := range ids {
		rows[i] = fmt.Sprintf(`{"timestamp":1700000000000,"price":"100","qty":"1","isBuyerTaker":true,"tradeId":%d}`, id)
	}
	snap := ""
	if snapshot {
		snap = `"snapshot":true,`
	}
	return fmt.Sprintf(`{"type":"trade","timestamp":1700000000000,"symbol":"btc_krw",%s"data":[%s]}`, snap, strings.Join(rows, ","))
}

func payloadTradeIDs(t *testing.T, payload json.RawMessage) []int {
	t.Helper()
	var frame struct {
		Data []struct {
			TradeID int `json:"tradeId"`
		} `json:"data"`
	}
	// Backfill payloads are a bare row array; frames nest under "data".
	var rows []struct {
		TradeID int `json:"tradeId"`
	}
	if err := json.Unmarshal(payload, &rows); err == nil {
		ids := make([]int, len(rows))
		for i, r := range rows {
			ids[i] = r.TradeID
		}
		return ids
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("unparseable trade payload: %s", payload)
	}
	ids := make([]int, len(frame.Data))
	for i, r := range frame.Data {
		ids[i] = r.TradeID
	}
	return ids
}

func runTradeGapScenario(t *testing.T, restTradeIDs []int) (*tap, *stubDoer) {
	t.Helper()
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/trades" {
			rows := make([]string, len(restTradeIDs))
			for i, id := range restTradeIDs {
				rows[i] = fmt.Sprintf(`{"tradeId":%d,"price":"100"}`, id)
			}
			return ok(`[` + strings.Join(rows, ",") + `]`)
		}
		return 404, `{}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}},
		Client:        testClient("http://example.test", doer, nil),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverPush(t, tradeFrame(true, 1, 2, 3))
	d := tp.waitData("initial snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 3 {
		t.Fatalf("initial snapshot filtered: %v", got)
	}

	// A realtime frame overlapping the snapshot: row 3 is a duplicate.
	conn1.serverPush(t, tradeFrame(false, 3, 4))
	d = tp.waitData("realtime", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginRealtime })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 1 || got[0] != 4 {
		t.Fatalf("overlap not deduped: %v", got)
	}

	// Drop; the resubscribe snapshot starts at 10 — a hole above 4.
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	conn2.awaitWrite(t)
	conn2.serverPush(t, tradeFrame(true, 10, 11))
	d = tp.waitData("resubscribe snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 2 || got[0] != 10 {
		t.Fatalf("resubscribe snapshot mangled: %v", got)
	}
	return tp, doer
}

// TestTradeResubscribeAllDuplicatesEmitsEmptySnapshot: a resubscribe snapshot
// whose trades were all already delivered is no longer suppressed — it emits an
// empty snapshot frame so a consumer gating freshness on snapshot receipt (the
// TUI trade latch) is told the channel is current again, instead of waiting
// forever for a frame that, with no new trades, never arrives. A LIVE all-dup
// frame is still suppressed.
func TestTradeResubscribeAllDuplicatesEmitsEmptySnapshot(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := &stubDoer{handle: func(string, url.Values) (int, string) { return 404, `{}` }}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}},
		Client:        testClient("http://example.test", doer, nil),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})
	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverPush(t, tradeFrame(true, 1, 2, 3))
	tp.waitData("initial snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })

	// A LIVE frame of only already-delivered rows stays suppressed: push it then a
	// fresh row and verify only the fresh one surfaces.
	conn1.serverPush(t, tradeFrame(false, 2, 3))
	conn1.serverPush(t, tradeFrame(false, 4))
	d := tp.waitData("post-dup realtime", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginRealtime })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 1 || got[0] != 4 {
		t.Fatalf("a live all-duplicate frame must be suppressed, got %v", got)
	}

	// Reconnect; the resubscribe snapshot replays only already-seen ids → it must
	// still emit, as an empty snapshot (the "TOLD you it's current" signal).
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	conn2.awaitWrite(t)
	conn2.serverPush(t, tradeFrame(true, 1, 2, 3, 4))
	d = tp.waitData("empty resubscribe snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 0 {
		t.Fatalf("a fully-deduped resubscribe snapshot must emit empty, got %v", got)
	}
}

func TestTradeGapPatchedFromREST(t *testing.T) {
	// REST still has everything back to id 3: the hole 5..9 is recoverable.
	tp, _ := runTradeGapScenario(t, []int{3, 4, 5, 6, 7, 8, 9, 10, 11})
	d := tp.waitData("gap patch", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginBackfill })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 5 || got[0] != 5 || got[4] != 9 {
		t.Fatalf("gap patch wrong rows: %v", got)
	}
	// Fully recovered: no DATA_GAP may be raised.
	for _, ev := range tp.seen {
		if n, isNotice := ev.(Notice); isNotice && n.Code == DataGap {
			t.Fatalf("unexpected DATA_GAP: %v", n)
		}
	}
}

func TestTradeGapBackfillNoticesBracketSnapshot(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/trades" {
			return ok(`[{"tradeId":3,"price":"100"},{"tradeId":4,"price":"100"},{"tradeId":5,"price":"100"},{"tradeId":6,"price":"100"},{"tradeId":7,"price":"100"},{"tradeId":8,"price":"100"},{"tradeId":9,"price":"100"},{"tradeId":10,"price":"100"},{"tradeId":11,"price":"100"}]`)
		}
		return 404, `{}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}},
		Client:        testClient("http://example.test", doer, nil),
		Dial:          dialer.dial,
		Now:           func() int64 { return 1700000000000 },
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverPush(t, tradeFrame(true, 1, 2, 3))
	tp.waitData("initial snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	conn1.serverPush(t, tradeFrame(false, 4))
	tp.waitData("realtime", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginRealtime })

	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	conn2.awaitWrite(t)
	conn2.serverPush(t, tradeFrame(true, 10, 11))

	ev, ok := tp.next(5 * time.Second)
	if !ok {
		t.Fatal("no event after gap snapshot")
	}
	start, ok := ev.(Notice)
	if !ok || start.Code != BackfillStart {
		t.Fatalf("first gap event = %#v, want BACKFILL_START", ev)
	}
	if start.Level != LevelInfo || start.Details["endpoint"] != "public" ||
		start.Details["channel"] != ChannelTrade || start.Details["reason"] != "gap" ||
		start.Details["symbol"] != "btc_krw" {
		t.Fatalf("unexpected BACKFILL_START: %+v", start)
	}

	ev, ok = tp.next(5 * time.Second)
	if !ok {
		t.Fatal("no snapshot after BACKFILL_START")
	}
	snap, ok := ev.(Data)
	if !ok || snap.Channel != ChannelTrade || snap.Origin != OriginSnapshot {
		t.Fatalf("second gap event = %#v, want trade snapshot Data", ev)
	}

	tp.waitData("gap patch", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginBackfill })
	done := tp.waitNotice(BackfillDone)
	if done.Level != start.Level {
		t.Fatalf("BACKFILL_DONE level = %q, want same as start %q", done.Level, start.Level)
	}
	if complete, _ := done.Details["complete"].(bool); !complete {
		t.Fatalf("complete BACKFILL_DONE missing complete=true: %v", done.Details)
	}
	if done.Details["endpoint"] != "public" || done.Details["channel"] != ChannelTrade ||
		done.Details["reason"] != "gap" || done.Details["symbol"] != "btc_krw" {
		t.Fatalf("unexpected BACKFILL_DONE: %+v", done)
	}
}

func TestTradeGapBackfillFailureCarriesGapDetails(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/trades" {
			return 400, `{"success":false,"error":{"code":400,"message":"BAD_REQUEST"}}`
		}
		return 404, `{}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}},
		Client:        testClient("http://example.test", doer, nil),
		Dial:          dialer.dial,
		Now:           func() int64 { return 1700000000000 },
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverPush(t, tradeFrame(true, 1, 2, 3))
	tp.waitData("initial snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	conn1.serverPush(t, tradeFrame(false, 4))
	tp.waitData("realtime", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginRealtime })

	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	conn2.awaitWrite(t)
	conn2.serverPush(t, tradeFrame(true, 10, 11))

	tp.waitNotice(BackfillStart)
	tp.waitData("resubscribe snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	failed := tp.waitNotice(BackfillFailed)
	if failed.Details["endpoint"] != "public" ||
		failed.Details["channel"] != ChannelTrade ||
		failed.Details["reason"] != "gap" ||
		failed.Details["symbol"] != "btc_krw" ||
		failed.Details["source"] != "/v2/trades" ||
		failed.Details["afterTradeId"] != int64(4) ||
		failed.Details["beforeTradeId"] != int64(10) ||
		failed.Details["error"] == "" {
		t.Fatalf("unexpected public trade gap BACKFILL_FAILED details: %+v", failed)
	}
}

func TestTradeGapBackfillDisabledNoticesBeforeSnapshot(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tp, _ := startSession(t, Config{
		Subscriptions:   []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}},
		Client:          testClient("", nil, nil),
		DisableBackfill: true,
		Dial:            dialer.dial,
		Now:             func() int64 { return 1700000000000 },
		Tunables:        testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverPush(t, tradeFrame(true, 1, 2, 3))
	tp.waitData("initial snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	conn1.serverPush(t, tradeFrame(false, 4))
	tp.waitData("realtime", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginRealtime })

	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected)
	conn2.awaitWrite(t)
	conn2.serverPush(t, tradeFrame(true, 10, 11))

	ev, ok := tp.next(5 * time.Second)
	if !ok {
		t.Fatal("no event after disabled gap snapshot")
	}
	disabled, ok := ev.(Notice)
	if !ok || disabled.Code != BackfillDisabled {
		t.Fatalf("first disabled-gap event = %#v, want BACKFILL_DISABLED", ev)
	}
	if disabled.Level != LevelWarn || disabled.Details["endpoint"] != "public" ||
		disabled.Details["channel"] != ChannelTrade || disabled.Details["reason"] != "gap" ||
		disabled.Details["symbol"] != "btc_krw" {
		t.Fatalf("unexpected BACKFILL_DISABLED: %+v", disabled)
	}

	ev, ok = tp.next(5 * time.Second)
	if !ok {
		t.Fatal("no DATA_GAP after BACKFILL_DISABLED")
	}
	gap, ok := ev.(Notice)
	if !ok || gap.Code != DataGap {
		t.Fatalf("second disabled-gap event = %#v, want DATA_GAP", ev)
	}
	if gap.Level != LevelWarn || gap.Details["backfill"] != "disabled" {
		t.Fatalf("unexpected disabled DATA_GAP: %+v", gap)
	}

	ev, ok = tp.next(5 * time.Second)
	if !ok {
		t.Fatal("no snapshot after disabled gap notices")
	}
	snap, ok := ev.(Data)
	if !ok || snap.Channel != ChannelTrade || snap.Origin != OriginSnapshot {
		t.Fatalf("third disabled-gap event = %#v, want trade snapshot Data", ev)
	}
}

func TestTradeGapBeyondBufferRaisesDataGap(t *testing.T) {
	// REST's oldest id is 8 with a SHORT page: ids 5..7 either never existed
	// or aged out of the server's buffer — unprovable, so the consumer must
	// be warned (saturated=false says the loss is possible, not certain).
	tp, _ := runTradeGapScenario(t, []int{8, 9, 10, 11})
	d := tp.waitData("partial gap patch", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginBackfill })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 2 || got[0] != 8 || got[1] != 9 {
		t.Fatalf("partial patch wrong rows: %v", got)
	}
	n := tp.waitNotice(DataGap)
	if n.Details["symbol"] != "btc_krw" || n.Details["saturated"] != false {
		t.Fatalf("unexpected DataGap details: %v", n.Details)
	}
}

func TestTradeGapSaturatedRaisesDataGap(t *testing.T) {
	// REST returns a SATURATED page (500 rows) whose oldest id is 8: the
	// fetch was truncated, the hole below 8 is certainly unreachable.
	ids := make([]int, 500)
	for i := range ids {
		ids[i] = 8 + i
	}
	tp, _ := runTradeGapScenario(t, ids)
	n := tp.waitNotice(DataGap)
	if n.Details["saturated"] != true {
		t.Fatalf("unexpected DataGap details: %v", n.Details)
	}
}

// TestTradeHistorySeedFromREST: a trade subscription that requests initial
// history is seeded from /v2/trades on the first snapshot, so a consumer starts
// with depth even when the WebSocket snapshot carries a single trade.
func TestTradeHistorySeedFromREST(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/trades" {
			// Server returns recent trades newest-first; id 100 overlaps the
			// snapshot's single row and must be excluded from the seed.
			return ok(`[{"tradeId":100,"price":"100"},{"tradeId":99,"price":"100"},{"tradeId":98,"price":"100"},{"tradeId":97,"price":"100"},{"tradeId":96,"price":"100"},{"tradeId":95,"price":"100"}]`)
		}
		return 404, `{}`
	}}
	tp, _ := startSession(t, Config{
		Subscriptions: []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}, TradeHistory: 3}},
		Client:        testClient("http://example.test", doer, nil),
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverPush(t, tradeFrame(true, 100)) // server-defined single-row snapshot
	d := tp.waitData("snapshot", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginSnapshot })
	if got := payloadTradeIDs(t, d.Payload); len(got) != 1 || got[0] != 100 {
		t.Fatalf("snapshot rows: %v (want [100])", got)
	}
	d = tp.waitData("seed", func(d Data) bool { return d.Channel == ChannelTrade && d.Origin == OriginBackfill })
	if d.Source != "/v2/trades" {
		t.Fatalf("seed source: %q", d.Source)
	}
	// The 3 most recent trades BELOW the snapshot (99,98,97), newest-first; id
	// 100 is excluded as already delivered and 96/95 are trimmed to the depth.
	if got := payloadTradeIDs(t, d.Payload); len(got) != 3 || got[0] != 99 || got[2] != 97 {
		t.Fatalf("seed rows: %v (want 99,98,97)", got)
	}
	// A history seed is best-effort depth, not gap recovery: it raises no DATA_GAP.
	for _, ev := range tp.seen {
		if n, isNotice := ev.(Notice); isNotice && n.Code == DataGap {
			t.Fatalf("unexpected DATA_GAP from a history seed: %v", n)
		}
	}
}

func TestTradeGapWithBackfillDisabled(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tp, _ := startSession(t, Config{
		Client:          testClient("", nil, nil),
		Subscriptions:   []Subscription{{Channel: ChannelTrade, Symbols: []string{"btc_krw"}}},
		DisableBackfill: true,
		Dial:            dialer.dial,
		Tunables:        testTunables(),
	})

	tp.waitNotice(Connected)
	conn1.serverPush(t, tradeFrame(true, 1, 2, 3))
	tp.waitData("snapshot", func(d Data) bool { return d.Channel == ChannelTrade })
	conn1.serverDrop()
	tp.waitNotice(Connected)
	conn2.serverPush(t, tradeFrame(true, 10, 11))

	n := tp.waitNotice(DataGap)
	if n.Details["backfill"] != "disabled" {
		t.Fatalf("DataGap should say backfill is disabled: %v", n.Details)
	}
}

// ---------------------------------------------------------------------------
// Health: keepalive, half-open ping, disconnect storm, delayed data.
// ---------------------------------------------------------------------------

func TestKeepaliveFiresOnSilence(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tun := testTunables()
	tun.KeepaliveAfterMs = 80
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      tun,
	})

	tp.waitNotice(Connected)
	n := tp.waitNotice(Keepalive)
	if n.Details["connectionsUp"] != 1 || n.Details["connectionsTotal"] != 1 {
		t.Fatalf("unexpected keepalive details: %v", n.Details)
	}
}

func TestPingTimeoutForcesReconnect(t *testing.T) {
	conn1, conn2 := newFakeConn(), newFakeConn()
	conn1.pingFunc = func(ctx context.Context) error {
		<-ctx.Done() // never pong
		return ctx.Err()
	}
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tun := testTunables()
	tun.PingIntervalMs = 20
	tun.PongTimeoutMs = 20
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      tun,
	})

	tp.waitNotice(Connected)
	tp.waitNotice(Disconnected) // the half-open connection was torn down
	n := tp.waitNotice(Connected)
	if n.Details["attempt"] != 2 {
		t.Fatalf("want reconnect, got %v", n.Details)
	}
}

func TestHighRTTRaisesUnreliable(t *testing.T) {
	conn := newFakeConn()
	conn.pingFunc = func(ctx context.Context) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	}
	dialer := &fakeDialer{script: connScript(conn)}
	tun := testTunables()
	tun.PingIntervalMs = 10
	tun.UnreliableRTTMs = 1
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      tun,
	})

	tp.waitNotice(Connected)
	n := tp.waitNotice(ConnectionUnreliable)
	if _, hasRTT := n.Details["rttMs"]; !hasRTT {
		t.Fatalf("unreliable notice without rttMs: %v", n.Details)
	}
}

func TestDisconnectStormRaisesUnreliable(t *testing.T) {
	conn1, conn2, conn3 := newFakeConn(), newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2, conn3)}
	tun := testTunables()
	tun.UnreliableDisconnects = 2
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      tun,
	})

	tp.waitNotice(Connected)
	conn1.serverDrop()
	tp.waitNotice(Connected)
	conn2.serverDrop()
	n := tp.waitNotice(ConnectionUnreliable)
	if n.Details["disconnects"] != 2 {
		t.Fatalf("unexpected disconnect count: %v", n.Details)
	}
}

func TestDelayedDataRaisesNotice(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	staleTs := time.Now().UnixMilli() - 60_000
	conn.serverPush(t, fmt.Sprintf(`{"type":"ticker","timestamp":%d,"symbol":"btc_krw","data":{}}`, staleTs))
	n := tp.waitNotice(DataDelayed)
	if delay, isInt := n.Details["delayMs"].(int64); !isInt || delay < 10_000 {
		t.Fatalf("unexpected delay details: %v", n.Details)
	}
}

// TestConnectFailureNoticed: failed initial dials are reported, then a later
// success connects normally.
func TestConnectFailureNoticed(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: []func() (Conn, error){
		func() (Conn, error) { return nil, errors.New("connection refused") },
		func() (Conn, error) { return conn, nil },
	}}
	tp, _ := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	n := tp.waitNotice(ConnectFailed)
	if !strings.Contains(n.Details["error"].(string), "connection refused") {
		t.Fatalf("unexpected ConnectFailed details: %v", n.Details)
	}
	tp.waitNotice(Connected)
}

// ---------------------------------------------------------------------------
// Session lifecycle.
// ---------------------------------------------------------------------------

func TestCleanShutdownClosesEvents(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	tp, cancel := startSession(t, Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	cancel()
	err, returned := tp.runResult(5 * time.Second)
	if !returned {
		t.Fatal("Run did not return after cancel")
	}
	if err != nil {
		t.Fatalf("clean shutdown should return nil, got %v", err)
	}
	// The events channel must drain and close.
	for {
		_, open := <-tp.s.Events()
		if !open {
			break
		}
	}
}

// lockedBuf is a concurrency-safe sink for the operational logger, which logs
// from several stream goroutines. logging.New serializes writes under its own
// mutex, but the test also reads the buffer, so guard both.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestDebugLogsMechanicsNotNotices drives a public reconnect with a Debug logger
// wired onto Config.Log and asserts the operational logs show the connection
// MECHANICS behind the notices ("ws dial", "subscribe", "reconnect backoff") and
// that a notice (DISCONNECTED) is NOT duplicated into the log — notices are
// program output, the logs are the low-level mechanics that complement them.
func TestDebugLogsMechanicsNotNotices(t *testing.T) {
	var buf lockedBuf
	conn1, conn2 := newFakeConn(), newFakeConn()
	dialer := &fakeDialer{script: connScript(conn1, conn2)}
	tp, cancel := startSession(t, Config{
		Client:        testClient("", nil, nil),
		PublicURL:     "ws://example.test/v2/public",
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          dialer.dial,
		Tunables:      testTunables(),
		Log:           logging.New(&buf, slog.LevelDebug),
	})

	tp.waitNotice(Connected)
	conn1.awaitWrite(t)
	conn1.serverDrop()
	tp.waitNotice(Disconnected)
	tp.waitNotice(Connected) // the reconnect
	conn2.awaitWrite(t)

	// Stop and wait for Run to return, so every goroutine has finished logging
	// before the buffer is read.
	cancel()
	if _, ok := tp.runResult(5 * time.Second); !ok {
		t.Fatal("Run did not return after cancel")
	}

	out := buf.String()
	for _, want := range []string{
		"ws dial", // the resolved dial target + signed flag
		"url=ws://example.test/v2/public",
		`endpoint=public`,   // per-endpoint mechanic tag
		"subscribe",         // the subscribe frame
		"reconnect backoff", // the backoff sleep before the re-dial
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected mechanic log %q; got:\n%s", want, out)
		}
	}
	// A notice must NOT be duplicated as a log line: the stream layer's reliability
	// story is the notice channel, not the operational logger.
	if strings.Contains(out, "websocket disconnected") {
		t.Fatalf("DISCONNECTED notice text was duplicated into the operational log:\n%s", out)
	}
	if strings.Contains(out, "websocket connected") || strings.Contains(out, "websocket reconnected") {
		t.Fatalf("CONNECTED notice text was duplicated into the operational log:\n%s", out)
	}
}

func TestRunTwiceFails(t *testing.T) {
	s, err := New(Config{
		Client:        testClient("", nil, nil),
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Dial:          (&fakeDialer{}).dial,
		Tunables:      testTunables(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("second Run must fail")
	}
	cancel()
	<-done
}
