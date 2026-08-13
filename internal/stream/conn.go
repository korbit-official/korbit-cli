// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// Subscription is one channel subscription. Channel must be one of the
// Channel* constants; Symbols is required for every channel except myAsset.
type Subscription struct {
	Channel string
	Symbols []string
	// Level is the optional orderbook grouping level (orderbook only).
	Level string
	// AccountSeqs is the optional account-sequence list (private channels
	// only); the server defaults to [1] (the main account) when omitted.
	AccountSeqs []int
	// TradeHistory seeds the trade channel with recent history on the initial
	// subscription (trade channel only). When > 0 the session fetches up to this
	// many trades preceding the live snapshot from GET /v2/trades and emits them
	// with Origin OriginBackfill, so a consumer starts with depth instead of only
	// whatever the WebSocket snapshot happened to carry (the public snapshot's
	// row count is server-defined and may be as small as one). Capped at the
	// server's 500-row page; ignored when backfill is disabled. The seed lands
	// asynchronously and may interleave with the first live frames — order by
	// tradeId, never by arrival.
	TradeHistory int
}

// Conn is the minimal WebSocket surface the manager needs. The default
// implementation wraps coder/websocket; tests inject fakes.
type Conn interface {
	// Read returns the next text message. It must also service protocol
	// control frames (answer server pings, complete client pings).
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, p []byte) error
	// Ping sends a protocol ping and returns when the pong arrives (requires a
	// concurrent Read to process it) or ctx expires.
	Ping(ctx context.Context) error
	// Close tears the connection down immediately. Safe to call more than once
	// and from any goroutine; it must unblock a concurrent Read.
	Close() error
}

// Dialer opens a WebSocket connection. A refused HTTP upgrade is reported as
// an *UpgradeError so the reconnect loop can classify it.
type Dialer func(ctx context.Context, url string, header http.Header) (Conn, error)

// UpgradeError is a refused WebSocket handshake (non-101 response).
type UpgradeError struct {
	Status int
	// Code is the symbolic error code from the response's JSON error envelope
	// (error.message, e.g. EXCEED_TIME_WINDOW, KEY_NOT_FOUND), "" if none.
	Code string
	Body string
}

func (e *UpgradeError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("websocket handshake rejected: HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("websocket handshake rejected: HTTP %d", e.Status)
}

// maxFrameBytes bounds a single inbound message. The largest routine frame is
// a depth-30 orderbook or a trade snapshot — well under 64 KiB — but the limit
// is generous so a legitimately fat frame is never mistaken for an error.
const maxFrameBytes = 4 << 20

// DefaultDialer dials with coder/websocket.
func DefaultDialer(ctx context.Context, url string, header http.Header) (Conn, error) {
	return dial(ctx, url, header, nil)
}

// DialerWithClient is DefaultDialer with the handshake performed through hc, so
// the WebSocket connection honors the same outbound binding (--bind/--family) as
// the REST client. hc.Transport must speak HTTP/1.1 for the Upgrade.
func DialerWithClient(hc *http.Client) Dialer {
	return func(ctx context.Context, url string, header http.Header) (Conn, error) {
		return dial(ctx, url, header, hc)
	}
}

func dial(ctx context.Context, url string, header http.Header, hc *http.Client) (Conn, error) {
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header, HTTPClient: hc})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, upgradeError(resp, err)
		}
		return nil, err
	}
	c.SetReadLimit(maxFrameBytes)
	return &wsConn{c: c}, nil
}

// upgradeError shapes a non-101 handshake response into an *UpgradeError,
// extracting the symbolic code from the standard error envelope when present.
func upgradeError(resp *http.Response, dialErr error) error {
	var body []byte
	if resp.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 2048))
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	if envelope.Error.Message == "" && dialErr != nil && len(body) == 0 {
		return &UpgradeError{Status: resp.StatusCode, Body: dialErr.Error()}
	}
	return &UpgradeError{Status: resp.StatusCode, Code: envelope.Error.Message, Body: string(body)}
}

type wsConn struct{ c *websocket.Conn }

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	_, p, err := w.c.Read(ctx)
	return p, err
}

func (w *wsConn) Write(ctx context.Context, p []byte) error {
	return w.c.Write(ctx, websocket.MessageText, p)
}

func (w *wsConn) Ping(ctx context.Context) error { return w.c.Ping(ctx) }

func (w *wsConn) Close() error { return w.c.CloseNow() }

// frameEnvelope is the routing skim of an inbound message: enough to tell a
// control message from data and to route data to its channel, leaving the
// payload untouched.
type frameEnvelope struct {
	// Control fields — a message with Status set is a control message.
	Status    string `json:"status"`
	RequestID *int   `json:"requestId"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	// Data fields. Public frames carry Type; private frames carry ChannelType.
	Type        string `json:"type"`
	ChannelType string `json:"channelType"`
	Symbol      string `json:"symbol"`
	Timestamp   int64  `json:"timestamp"`
	Snapshot    bool   `json:"snapshot"`
}

// channel returns the logical channel of a data frame, whichever field family
// it uses.
func (f frameEnvelope) channel() string {
	if f.ChannelType != "" {
		return f.ChannelType
	}
	return f.Type
}

// connAuth produces the signed upgrade credentials for the private endpoint.
type connAuth struct {
	// header returns the x-kapi-key header.
	header func() http.Header
	// query returns the signed query string to append to the URL
	// ("timestamp=...&signature=..."), signed with the session's current
	// clock estimate.
	query func() (string, error)
}

// errAllSubscriptionsRejected makes run stop reconnecting when the server has
// rejected every subscription — each reconnect would just be rejected again.
var errAllSubscriptionsRejected = errors.New("the server rejected every subscription")

// subOp is a dynamic subscription change requested after the session is live.
type subOp string

const (
	opSubscribe   subOp = "subscribe"
	opUnsubscribe subOp = "unsubscribe"
)

// subCommand is a dynamic change handed to the live connection's writer
// goroutine to put on the wire now.
type subCommand struct {
	op  subOp
	sub Subscription
}

// pendingReq is a subscribe/unsubscribe request awaiting its server ack. The op
// is tracked so a rejected UNsubscribe (benign — the subscription is already
// being removed) is not treated like a rejected subscribe (which drops the sub
// and can go fatal).
type pendingReq struct {
	op  subOp
	sub Subscription
}

// connManager owns one endpoint's connection lifecycle: dial (signed for
// private), subscribe, read, ping, reconnect with jittered exponential
// backoff, resubscribe. It reports through three seams: emit (notices), onUp
// (a connection is live and subscribed — the session backfills), and
// onFrame (a data frame arrived).
type connManager struct {
	endpoint string // "public" | "private", for notices
	url      string
	lg       *slog.Logger // resolved from the session (never nil); see log()
	dial     Dialer
	auth     *connAuth // nil for the public endpoint
	emit     func(Event)
	onUp     func(reconnect bool, downtimeMs int64, gen int)
	onFrame  func(raw []byte, env frameEnvelope)
	// resyncClock re-measures the server clock offset after an
	// EXCEED_TIME_WINDOW handshake rejection; nil when no REST config is
	// available to measure against.
	resyncClock func() error
	// setUp reports connection up/down transitions (for the session's
	// keepalive status).
	setUp func(up bool)
	now   func() int64
	sleep func(time.Duration)
	tun   Tunables
	// noReconnect ends the loop the first time a connection cannot be kept up
	// (failed connect, rejected subscribe write, or a drop after serving),
	// returning the underlying error instead of looping. See Config.NoReconnect.
	noReconnect bool
	health      *connHealth
	// delay is the same delayCheck the frame router feeds; the ping loop sweeps
	// it so a delay warning still clears when the feed goes quiet.
	delay *delayCheck

	mu      sync.Mutex
	subs    []Subscription
	pending map[int]pendingReq // requestId -> the (sub|unsub) request awaiting its ack
	nextReq int
	live    bool // a connection is currently serving (its writer drains cmds)
	// fatalSubs is set by the reader when the last subscription is rejected.
	fatalSubs bool
	// cmds carries dynamic subscribe/unsubscribe requests to the live
	// connection's writer goroutine. Buffered with a non-blocking send: a full
	// channel drops the command, which is safe because applyChange already
	// updated subs, so the next reconnect's subscribeAll reflects the change.
	cmds chan subCommand
}

// run dials and serves until ctx is canceled (returns ctx.Err()) or a fatal,
// non-retryable condition is hit (emits a Fatal notice and returns the error).
// Transient failures reconnect forever — but never in a hot loop. EVERY
// reconnect attempt is preceded by a jittered backoff applied at the top of the
// loop; the sole exception is a single EXCEED_TIME_WINDOW corrective resync,
// which retries immediately (the `immediate` flag) and is itself capped by
// `resynced`. Centralizing the backoff before the dial is what guarantees no
// failure mode — a refused dial, a failed subscribe, OR a connection that is
// accepted and then instantly dropped — can spin this loop without pausing
// (that last case otherwise re-dials with no delay). The backoff ladder is the
// rate bound that keeps an intentionally-unbounded reconnect loop safe; see the
// "Loop-safety invariants" section of internal/korbit/doc.go.
//
// This loop deliberately does NOT use korbit.RetryGovernor (which the budgeted
// request-retry loops share): it is unbounded by design, has no sleep budget,
// and classifies handshake rejections rather than Classify errors — a different
// shape, where the right safety property is a rate bound, not a count.
func (m *connManager) run(ctx context.Context) error {
	backoffMs := m.tun.ReconnectMinMs
	connects := 0
	var downSince int64
	resynced := false
	immediate := false // set only for the one-shot ETW resync: skip the next backoff
	// recovering tracks whether a degradation (CONNECT_FAILED or DISCONNECTED)
	// has been emitted since the last successful connect, so the CONNECTED that
	// follows is graded as a recovery (warn, matching the onset it clears) rather
	// than a routine first connect (info). Reset once that CONNECTED is emitted.
	recovering := false

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Mandatory backoff before every reconnect — never the first attempt, and
		// never the one-shot corrective resync. No re-dial path bypasses this, so
		// no server behavior can turn the loop hot. The notice emitted at the end
		// of the previous iteration printed this same (pre-doubling) value.
		if attempt > 0 && !immediate {
			sleepFor := jitterMs(backoffMs)
			m.log().Debug("reconnect backoff", "attempt", attempt, "backoffMs", backoffMs, "sleepMs", sleepFor.Milliseconds())
			m.sleep(sleepFor)
			backoffMs = min(backoffMs*2, m.tun.ReconnectMaxMs)
		}
		immediate = false

		m.log().Debug("ws dial", "url", redactURL(m.url), "signed", m.auth != nil, "attempt", attempt+1)
		conn, err := m.dialOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.log().Debug("ws dial failed", "error", err.Error())
			var ue *UpgradeError
			if errors.As(err, &ue) {
				// HTTP status separates "wrong host / intermediary" (no envelope
				// code) from a server-side auth/clock rejection (envelope code) —
				// the mechanic the CONNECT_FAILED notice summarizes.
				m.log().Debug("ws upgrade rejected", "httpStatus", ue.Status, "code", ue.Code)
				if ue.Code == "EXCEED_TIME_WINDOW" && !resynced && m.resyncClock != nil {
					// The server's clock gate refused the signed timestamp.
					// Re-measure once and retry immediately (no backoff); if the
					// retry is refused again `resynced` is set, so the normal
					// backoff classification applies from there on.
					resynced = true
					m.log().Debug("ws upgrade clock resync", "trigger", "EXCEED_TIME_WINDOW")
					if m.resyncClock() == nil {
						immediate = true
						continue
					}
				}
				if ue.Status >= 400 && ue.Status < 500 && ue.Status != 429 &&
					ue.Code != "" && ue.Code != "EXCEED_TIME_WINDOW" {
					// A 4xx carrying a Korbit error envelope is a definitive
					// auth/permission/config-class rejection: reconnecting
					// cannot fix it. A 4xx WITHOUT the envelope may come from
					// an intermediary (proxy/LB) and is retried with backoff
					// like any transient failure.
					m.notice(Fatal, LevelError,
						fmt.Sprintf("%s websocket: %s — cannot continue", m.endpoint, err.Error()),
						map[string]any{"endpoint": m.endpoint, "error": err.Error(), "httpStatus": ue.Status, "code": ue.Code})
					return err
				}
			}
			if m.noReconnect {
				m.notice(ConnectFailed, LevelWarn,
					fmt.Sprintf("%s websocket connection attempt failed: %v", m.endpoint, err),
					map[string]any{"endpoint": m.endpoint, "error": err.Error()})
				return m.failNoReconnect(err, "could not connect")
			}
			m.notice(ConnectFailed, LevelWarn,
				fmt.Sprintf("%s websocket connection attempt failed: %v — retrying in ~%dms", m.endpoint, err, backoffMs),
				map[string]any{"endpoint": m.endpoint, "error": err.Error(), "nextRetryMs": backoffMs})
			recovering = true
			continue
		}

		connects++
		reconnect := connects > 1
		downtimeMs := int64(0)
		if downSince > 0 {
			downtimeMs = m.now() - downSince
		}
		m.health.onConnected()
		m.setUp(true)
		msg := fmt.Sprintf("%s websocket connected", m.endpoint)
		// A connect that follows a CONNECT_FAILED or a DISCONNECTED is the recovery
		// of that problem, so it is graded warn (matching the onset) — the same
		// level threshold then shows both the drop and its clear. A clean first
		// connect, resolving nothing, is routine info.
		connLevel := LevelInfo
		if recovering {
			connLevel = LevelWarn
		}
		if reconnect {
			msg = fmt.Sprintf("%s websocket reconnected after %dms", m.endpoint, downtimeMs)
		}
		m.notice(Connected, connLevel, msg, map[string]any{
			"endpoint": m.endpoint, "attempt": connects, "downtimeMs": downtimeMs,
		})
		recovering = false

		if err := m.subscribeAll(ctx, conn); err != nil {
			_ = conn.Close()
			m.setUp(false)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			downSince = m.now()
			m.health.onDisconnected(downSince, m.notice)
			if m.noReconnect {
				m.notice(Disconnected, LevelWarn,
					fmt.Sprintf("%s websocket disconnected: sending subscriptions failed: %v", m.endpoint, err),
					map[string]any{"endpoint": m.endpoint, "reason": fmt.Sprintf("subscribe write failed: %v", err)})
				return m.failNoReconnect(err, "disconnected")
			}
			m.notice(Disconnected, LevelWarn,
				fmt.Sprintf("%s websocket disconnected: sending subscriptions failed: %v — reconnecting in ~%dms", m.endpoint, err, backoffMs),
				map[string]any{"endpoint": m.endpoint, "reason": fmt.Sprintf("subscribe write failed: %v", err)})
			recovering = true
			continue
		}

		m.onUp(reconnect, downtimeMs, connects)

		upAt := m.now()
		serveErr := m.serve(ctx, conn)
		downSince = m.now()
		m.setUp(false)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(serveErr, errAllSubscriptionsRejected) {
			m.notice(Fatal, LevelError,
				fmt.Sprintf("%s websocket: every subscription was rejected — nothing to stream", m.endpoint),
				map[string]any{"endpoint": m.endpoint, "error": serveErr.Error()})
			return serveErr
		}

		if m.noReconnect {
			m.health.onDisconnected(downSince, m.notice)
			m.notice(Disconnected, LevelWarn,
				fmt.Sprintf("%s websocket disconnected: %v", m.endpoint, serveErr),
				map[string]any{"endpoint": m.endpoint, "reason": fmt.Sprintf("%v", serveErr)})
			return m.failNoReconnect(serveErr, "disconnected")
		}

		// A connection that stayed up long enough proves the path works: restart
		// the backoff ladder (and allow one fresh clock resync). Done before the
		// notice so it reports the reset (minimum) backoff the next reconnect
		// will use. A connection that flaps faster than StableAfterMs keeps
		// climbing the ladder, so repeated instant drops back off increasingly
		// rather than reconnecting hot.
		if downSince-upAt >= m.tun.StableAfterMs {
			backoffMs = m.tun.ReconnectMinMs
			resynced = false
		}
		m.health.onDisconnected(downSince, m.notice)
		m.notice(Disconnected, LevelWarn,
			fmt.Sprintf("%s websocket disconnected: %v — reconnecting in ~%dms", m.endpoint, serveErr, backoffMs),
			map[string]any{"endpoint": m.endpoint, "reason": fmt.Sprintf("%v", serveErr)})
		recovering = true
	}
}

// failNoReconnect emits the terminal FATAL notice that precedes Run's error and
// returns err, used at every connect-failure / drop point when noReconnect is
// set. reason is a short clause ("could not connect" | "disconnected"). The
// preceding lifecycle notice (CONNECT_FAILED / DISCONNECTED) has already been
// emitted by the caller, so a consumer sees both what happened and that the
// session is ending.
func (m *connManager) failNoReconnect(err error, reason string) error {
	m.log().Debug("reconnect loop ending", "reason", reason, "noReconnect", true)
	m.notice(Fatal, LevelError,
		fmt.Sprintf("%s websocket %s and automatic reconnect is disabled — ending the session: %v", m.endpoint, reason, err),
		map[string]any{"endpoint": m.endpoint, "error": err.Error()})
	return err
}

// dialOnce builds the (possibly signed) URL and dials.
func (m *connManager) dialOnce(ctx context.Context) (Conn, error) {
	url := m.url
	var header http.Header
	if m.auth != nil {
		query, err := m.auth.query()
		if err != nil {
			return nil, err
		}
		url += "?" + query
		header = m.auth.header()
	}
	dctx, cancel := context.WithTimeout(ctx, time.Duration(m.tun.DialTimeoutMs)*time.Millisecond)
	defer cancel()
	return m.dial(dctx, url, header)
}

// subscribeAll sends every registered subscription as one JSON array, each
// item carrying a requestId so the server always acks it (success or fail).
func (m *connManager) subscribeAll(ctx context.Context, conn Conn) error {
	m.mu.Lock()
	if len(m.subs) == 0 {
		m.mu.Unlock()
		return errAllSubscriptionsRejected
	}
	m.pending = make(map[int]pendingReq, len(m.subs))
	items := make([]map[string]any, 0, len(m.subs))
	channels := make([]string, 0, len(m.subs))
	for _, sub := range m.subs {
		m.nextReq++
		m.pending[m.nextReq] = pendingReq{op: opSubscribe, sub: sub}
		items = append(items, subItem(sub, opSubscribe, m.nextReq))
		channels = append(channels, sub.Channel)
	}
	m.mu.Unlock()

	m.log().Debug("subscribe", "items", len(items), "channels", channels)

	msg, err := json.Marshal(items)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(m.tun.WriteTimeoutMs)*time.Millisecond)
	defer cancel()
	return conn.Write(wctx, msg)
}

// serve reads frames until the connection dies, with a concurrent ping loop
// measuring RTT and detecting half-open connections. It does not return until
// the ping loop has fully stopped, so nothing emits on a torn-down connection.
func (m *connManager) serve(ctx context.Context, conn Conn) error {
	sctx, cancel := context.WithCancel(ctx)

	// Discard any dynamic commands queued while disconnected (subscribeAll just
	// sent the current registry, which already reflects them), then go live so
	// applyChange starts feeding the writer that owns this conn.
	m.mu.Lock()
	m.drainCmdsLocked()
	m.live = true
	m.mu.Unlock()

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		m.pingLoop(sctx, conn)
	}()
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		m.writeLoop(sctx, conn)
	}()

	var err error
	for {
		var raw []byte
		raw, err = conn.Read(sctx)
		if err != nil {
			break
		}
		if err = m.handleFrame(raw); err != nil {
			break
		}
	}
	_ = conn.Close()
	cancel()
	<-pingDone
	<-writeDone
	m.mu.Lock()
	m.live = false
	m.mu.Unlock()
	return err
}

// writeLoop owns all writes initiated AFTER subscribeAll for the life of one
// connection: it sends dynamic subscribe/unsubscribe frames serialized with the
// ping loop's pings (coder/websocket allows concurrent Write and Ping, each
// internally serialized). Scoping it to one serve call means the conn never
// swaps under it and its sends are sequenced after that connection's
// subscribeAll. It exits when serve cancels sctx.
func (m *connManager) writeLoop(ctx context.Context, conn Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-m.cmds:
			if err := m.sendCommand(ctx, conn, cmd); err != nil {
				// The conn is dying (or ctx ended): serve's Read will observe it
				// and fail over. The registry is already updated, so the next
				// reconnect re-establishes the desired set.
				return
			}
		}
	}
}

// sendCommand allocates a requestId (so the ack is attributable), records the
// pending request, and writes one subscribe/unsubscribe frame.
func (m *connManager) sendCommand(ctx context.Context, conn Conn, cmd subCommand) error {
	m.mu.Lock()
	m.nextReq++
	reqID := m.nextReq
	m.pending[reqID] = pendingReq{op: cmd.op, sub: cmd.sub}
	m.mu.Unlock()

	m.log().Debug("dynamic "+string(cmd.op), "channel", cmd.sub.Channel, "symbols", cmd.sub.Symbols, "requestId", reqID)

	msg, err := json.Marshal([]map[string]any{subItem(cmd.sub, cmd.op, reqID)})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(m.tun.WriteTimeoutMs)*time.Millisecond)
	defer cancel()
	return conn.Write(wctx, msg)
}

// applyChange records a dynamic subscription change in the registry (so a
// reconnect re-subscribes the current set) and, if a connection is live, hands
// it to the writer to put on the wire now. Safe to call from any goroutine;
// never blocks.
func (m *connManager) applyChange(op subOp, sub Subscription) {
	m.mu.Lock()
	switch op {
	case opSubscribe:
		present := false
		for _, s := range m.subs {
			if sameSubscription(s, sub) {
				present = true
				break
			}
		}
		if !present {
			m.subs = append(m.subs, sub)
		}
	case opUnsubscribe:
		// Match by channel + symbols only: an unsubscribe targets a symbol's
		// orderbook/trade regardless of the grouping level it was subscribed at
		// (the symbols-only Unsubscribe API carries no Level/AccountSeqs).
		kept := m.subs[:0]
		for _, s := range m.subs {
			if !sameChannelSymbols(s, sub) {
				kept = append(kept, s)
			}
		}
		m.subs = kept
	}
	live := m.live
	m.mu.Unlock()

	if live {
		select {
		case m.cmds <- subCommand{op: op, sub: sub}:
		default: // full: drop — subs is already updated, reconnect reconciles
		}
	}
}

// tradeHistoryWant returns the trade-history seed depth registered for a
// symbol's trade subscription (0 if none), reading the live registry so a
// dynamically-added trade subscription is included.
func (m *connManager) tradeHistoryWant(symbol string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sub := range m.subs {
		if sub.Channel != ChannelTrade {
			continue
		}
		for _, sym := range sub.Symbols {
			if sym == symbol {
				return sub.TradeHistory
			}
		}
	}
	return 0
}

// drainCmdsLocked empties the command channel without blocking. Caller holds mu.
func (m *connManager) drainCmdsLocked() {
	for {
		select {
		case <-m.cmds:
		default:
			return
		}
	}
}

// subItem builds one subscribe/unsubscribe request object (wrapped in the
// top-level array by the caller). Used by both the initial subscribeAll and the
// dynamic writer so the wire shape can never drift between them.
func subItem(sub Subscription, op subOp, reqID int) map[string]any {
	item := map[string]any{
		"requestId": reqID,
		"method":    string(op),
		"type":      sub.Channel,
	}
	if len(sub.Symbols) > 0 {
		item["symbols"] = sub.Symbols
	}
	if sub.Level != "" {
		item["level"] = sub.Level
	}
	if len(sub.AccountSeqs) > 0 {
		item["accountSeqs"] = sub.AccountSeqs
	}
	return item
}

// pingLoop sends a protocol ping every PingIntervalMs. The round-trip time is
// an RTT sample for health; a ping that gets no pong within PongTimeoutMs
// means the connection is dead or half-open, so it is torn down (which
// unblocks serve's Read and triggers a reconnect).
func (m *connManager) pingLoop(ctx context.Context, conn Conn) {
	ticker := time.NewTicker(time.Duration(m.tun.PingIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		t0 := m.now()
		pctx, cancel := context.WithTimeout(ctx, time.Duration(m.tun.PongTimeoutMs)*time.Millisecond)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				_ = conn.Close() // half-open: force the read loop to fail over
			}
			return
		}
		m.health.recordRTT(m.now()-t0, m.now(), m.notice)
		if m.delay != nil {
			m.delay.sweep(m.now(), m.notice)
		}
	}
}

// handleFrame routes one inbound message: control messages (status present)
// are consumed here, data frames go to onFrame.
func (m *connManager) handleFrame(raw []byte) error {
	var env frameEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not JSON: ignore rather than kill a healthy connection.
		return nil
	}
	if env.Status == "" {
		m.onFrame(raw, env)
		return nil
	}

	switch env.Status {
	case "success":
		if env.RequestID != nil {
			m.mu.Lock()
			pr, known := m.pending[*env.RequestID]
			delete(m.pending, *env.RequestID)
			m.mu.Unlock()
			if known {
				m.log().Debug("subscribe ack", "requestId", *env.RequestID, "op", string(pr.op), "channel", pr.sub.Channel, "symbols", pr.sub.Symbols)
			}
		}
	case "fail":
		m.handleFailAck(env)
		m.mu.Lock()
		fatal := m.fatalSubs
		m.mu.Unlock()
		if fatal {
			return errAllSubscriptionsRejected
		}
	default: // "error" and anything new
		m.notice(ServerError, LevelWarn,
			fmt.Sprintf("%s websocket: server reported an error: %s", m.endpoint, env.Message),
			map[string]any{"endpoint": m.endpoint, "message": env.Message, "code": env.Code})
	}
	return nil
}

// handleFailAck handles a fail ack. A rejected UNsubscribe is benign —
// applyChange already removed that subscription — so it is only reported, never
// dropped-from-subs or made fatal. A rejected SUBSCRIBE is
// deterministic (resubscribing would just be rejected again), so the
// subscription is dropped from the registry and reported; if it was the last
// one the connection is useless and the manager goes fatal.
func (m *connManager) handleFailAck(env frameEnvelope) {
	var pr pendingReq
	known := false
	m.mu.Lock()
	if env.RequestID != nil {
		if p, ok := m.pending[*env.RequestID]; ok {
			pr = p
			known = true
			delete(m.pending, *env.RequestID)
		}
	}
	if known && pr.op == opUnsubscribe {
		m.mu.Unlock()
		m.notice(SubscribeFailed, LevelWarn,
			fmt.Sprintf("%s websocket: unsubscribe from %s was rejected (%s) — ignoring (already removed locally)", m.endpoint, pr.sub.Channel, env.Code),
			map[string]any{"endpoint": m.endpoint, "channel": pr.sub.Channel, "symbols": pr.sub.Symbols, "code": env.Code, "message": env.Message, "method": "unsubscribe"})
		return
	}
	if known {
		kept := m.subs[:0]
		for _, existing := range m.subs {
			if !sameSubscription(existing, pr.sub) {
				kept = append(kept, existing)
			}
		}
		m.subs = kept
	}
	if len(m.subs) == 0 {
		m.fatalSubs = true
	}
	m.mu.Unlock()

	details := map[string]any{
		"endpoint": m.endpoint, "code": env.Code, "message": env.Message,
	}
	channel := "unknown"
	if known {
		channel = pr.sub.Channel
		details["channel"] = pr.sub.Channel
		details["symbols"] = pr.sub.Symbols
	}
	m.notice(SubscribeFailed, LevelError,
		fmt.Sprintf("%s websocket: subscription to %s was rejected (%s) and dropped", m.endpoint, channel, env.Code),
		details)
}

// sameChannelSymbols reports whether a and b name the same channel and exact
// symbol set, ignoring Level/AccountSeqs/TradeHistory — the granularity a
// symbol-targeted unsubscribe operates at.
func sameChannelSymbols(a, b Subscription) bool {
	if a.Channel != b.Channel || len(a.Symbols) != len(b.Symbols) {
		return false
	}
	for i := range a.Symbols {
		if a.Symbols[i] != b.Symbols[i] {
			return false
		}
	}
	return true
}

func sameSubscription(a, b Subscription) bool {
	if a.Channel != b.Channel || a.Level != b.Level ||
		len(a.Symbols) != len(b.Symbols) || len(a.AccountSeqs) != len(b.AccountSeqs) {
		return false
	}
	for i := range a.Symbols {
		if a.Symbols[i] != b.Symbols[i] {
			return false
		}
	}
	for i := range a.AccountSeqs {
		if a.AccountSeqs[i] != b.AccountSeqs[i] {
			return false
		}
	}
	return true
}

func (m *connManager) notice(code NoticeCode, level Level, msg string, details map[string]any) {
	m.emit(Notice{Code: code, Level: level, Message: msg, Details: details, Time: m.now()})
}

// log is this endpoint's operational logger, resolved from the session (never
// nil). It carries the per-endpoint key so a multi-connection debug run shows
// which endpoint a mechanic belongs to. Call unconditionally.
func (m *connManager) log() *slog.Logger {
	return logging.Or(m.lg).With("endpoint", m.endpoint)
}

// redactURL strips the query string from a ws URL for logging: the private
// upgrade carries the signed timestamp/signature there, which must never reach a
// log. Only the scheme://host/path (e.g. wss://ws-api.korbit.co.kr/v2/private)
// is kept — enough to tell a wrong host/path from an auth problem.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// jitterMs spreads a backoff delay ±20% so reconnecting clients don't
// synchronize into thundering herds.
func jitterMs(ms int64) time.Duration {
	f := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(ms)*f) * time.Millisecond
}
