// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/logging"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
	"github.com/digitalx-official/digitalx-cli/internal/version"
)

// Production WebSocket endpoints.
const (
	DefaultPublicURL  = "wss://ws-api.digitalx.miraeasset.com/v2/public"
	DefaultPrivateURL = "wss://ws-api.digitalx.miraeasset.com/v2/private"
)

// Config describes a streaming session.
type Config struct {
	// PublicURL/PrivateURL override the production WebSocket endpoints
	// (e.g. to point at the local sandbox).
	PublicURL  string
	PrivateURL string
	// Subscriptions is the channel set to stream. At least one is required;
	// it is fixed for the session's lifetime.
	Subscriptions []Subscription
	// Client is the session's single Digital X API handle (required): it performs
	// REST backfill + public-trade gap patching, signs the private WebSocket
	// upgrade (Client.SignHandshake), and owns the shared server-clock estimate
	// (read via Client.Clock, resync via Client.Resync). Its Creds (set for a
	// private session) sign the upgrade and the account-data reads; a public-only
	// session passes a creds-less client whose Clock still drives delivery-delay
	// detection. The session records nothing of its own — the Client journals each
	// backfill call through its own per-call recorder (the caller's callrec policy,
	// which exempts the stream-backfill surface). The Client's BaseURL is required
	// whenever any private channel is subscribed, or the trade channel is
	// subscribed with backfill enabled.
	Client *apiclient.Client
	// DisableBackfill turns off all REST recovery. Private (re)connections then
	// raise BACKFILL_DISABLED, and detected public trade gaps raise
	// BACKFILL_DISABLED + DATA_GAP before the resubscribe snapshot is emitted.
	DisableBackfill bool
	// NoReconnect ends the session the first time a connection cannot be kept
	// up, instead of reconnecting. With it set, a failed initial connect, a
	// rejected subscribe write, or a connection that drops after serving all
	// emit their usual lifecycle notice (CONNECT_FAILED / DISCONNECTED) followed
	// by a terminal FATAL notice, and Run returns the underlying error. Because
	// Run stops on the first connManager error and cancels the rest, ANY one of
	// the (up to two) endpoints dropping ends the whole session. The one-shot
	// EXCEED_TIME_WINDOW clock resync is preserved (it is part of establishing
	// the first connection, not a reconnect). When false (the default) the
	// session reconnects indefinitely with jittered backoff and REST backfill.
	NoReconnect bool
	// BackfillSnapshotOnly trims private backfill to the per-connect SNAPSHOTS
	// only — balances and the authoritative open-order set (GET /v2/openOrders)
	// — and skips the per-symbol gap WALKS (GET /v2/allOrders order history and
	// GET /v2/myTrades fills). It exists for a session subscribing the account
	// channels across MANY symbols (e.g. the TUI watching every launched pair),
	// where fanning the history walks out over all of them on every reconnect is
	// a REST storm. The trade-off is explicit and announced: order/fill
	// transitions that happened DURING a disconnect are not recovered (a
	// BACKFILL_SNAPSHOT_ONLY notice says so). The open-order snapshot still
	// re-syncs the current open set, but stream/state only infers disappearances
	// for orders known from a prior private-connection epoch; same-epoch closes are
	// expected to arrive as live myOrder terminal frames. Public trade backfill
	// (Subscription.TradeHistory seed and the gap patch) is unaffected. Ignored
	// when DisableBackfill is set.
	BackfillSnapshotOnly bool
	// LazyOpenOrders scopes the open-order SNAPSHOT backfill to a dynamic
	// "tracked" scope set (SetTrackedOrderScopes — {accountSeq, symbol} pairs)
	// instead of every subscribed myOrder symbol × account. The live myOrder
	// channel is account-wide regardless, so this changes only which scopes get
	// a GET /v2/openOrders snapshot on (re)connect — letting a session that
	// subscribes the account channels across MANY symbols (and, with several
	// AccountSeqs, several sub-accounts) snapshot just the currently viewed
	// scope. Tracking a new scope snapshots it on demand, rather than fanning a
	// per-symbol×account snapshot out over every pair. In stream/state that
	// snapshot marks the scope ready and can close orders from an earlier
	// private-connection epoch; it is NOT a same-epoch substitute for a missing
	// live terminal myOrder frame. When false (the default, e.g. monitor) every
	// subscribed myOrder symbol is snapshotted for every subscribed account.
	// Independent of BackfillSnapshotOnly (which governs the gap WALKS).
	LazyOpenOrders bool
	// ProactiveTimeSync gates the proactive server-clock measurement at Run start.
	// When true (the monitor/tui --time-sync on) the clock is measured ONCE,
	// SYNCHRONOUSLY, before any connection is dialed, so the first frames' delay
	// check and the first private-upgrade signature already see a corrected clock —
	// no false-DATA_DELAYED flash on a skewed host clock. When false the estimate
	// stays the bare local clock and is corrected reactively: on an
	// EXCEED_TIME_WINDOW rejection, or when the delivery-delay check suspects lag
	// against an unmeasured clock and kicks a one-off resync (see the delayCheck).
	// That reactive kick works for a public-only session too — no signing needed —
	// as long as the Client carries a resync hook (omitted only under --time-sync
	// off). So with this false a skewed host clock trips one false DATA_DELAYED that
	// then self-corrects to DATA_CURRENT; set it true to measure up front and skip
	// the flash. Signing correctness never depends on this field: an out-of-window
	// signed request is corrected reactively whenever the Client carries a resync
	// hook.
	ProactiveTimeSync bool
	// BackfillRetryBudgetMs bounds the retry sleep budget of each backfill
	// REST call (default 15000).
	BackfillRetryBudgetMs int
	// EventBuffer is the event channel's capacity (default 1024). When the
	// buffer is full the session blocks rather than drop events; a consumer
	// that stops receiving eventually stalls the WebSocket reads.
	EventBuffer int
	// UserAgent is the User-Agent stamped on every WebSocket dial (the upgrade
	// handshake is a Digital X API request). Empty falls back to the bare
	// "digitalx-cli/<version>". The REST/backfill side uses the Client's own
	// UserAgent. The caller composes both (see internal/useragent) so the stream
	// layer carries no environment-gathering of its own.
	UserAgent string
	// Log is the optional operational logger for the connection mechanics behind
	// the notice channel: the resolved dial target and signed-vs-public upgrade,
	// subscribe/unsubscribe frames, reconnect backoff/epoch timing, and the REST
	// backfill window math. nil is silent. These are Debug-dominant DIAGNOSTICS
	// that COMPLEMENT — never duplicate — the Notice channel (which is program
	// output); a --debug run shows them, a normal run does not. No secrets ever
	// reach it (the signed upgrade is logged as host/path only, never the signed
	// query). This is the layer's only logging surface; the cli wires it once
	// and it flows to every internal goroutine.
	Log *slog.Logger
	// Test seams. Dial defaults to DefaultDialer, Now to wall-clock unix ms,
	// Sleep to time.Sleep.
	Dial  Dialer
	Now   func() int64
	Sleep func(time.Duration)
	// Tunables are the timing/threshold knobs; zero fields take defaults.
	Tunables Tunables
}

// Session is one resilient streaming session: up to two managed WebSocket
// connections (public/private), REST recovery, and a single ordered event
// stream. Create with New, start with Run, consume Events until it closes.
type Session struct {
	cfg  Config
	tun  Tunables
	lg   *slog.Logger    // resolved once from cfg.Log (never nil); see log()
	clk  apiclient.Clock // the shared clock read view (cfg.Client.Clock); never nil
	rest *restClient     // nil when the client has no BaseURL (no REST recovery)

	events chan Event
	stop   chan struct{} // closed when Run exits: unblocks emitters
	conns  []*connManager
	pub    *connManager // the public connManager (nil if no public channels) — dynamic Subscribe target
	keep   *keepalive

	started atomic.Bool
	upConns atomic.Int32   // currently-connected websocket count
	bg      sync.WaitGroup // backfill goroutines
	// bgMu/stopping gate consumer-driven bg spawns (startBg): Run sets stopping
	// before its final bg.Wait, so a SetTrackedOrderScopes/BackfillOpenOrders
	// call racing (or following) shutdown can neither race bg.Add against that
	// Wait nor spawn a goroutine that would emit on the closed events channel.
	// Internal spawns don't need the gate — they all originate from goroutines
	// Run has already waited on by the time stopping is set.
	bgMu       sync.Mutex
	stopping   bool
	backfillMu sync.Mutex // serializes private backfills across reconnects
	// privateGen is the latest private-connection generation (the private
	// connManager's connection count), set on each private connect. Stamped onto
	// on-demand open-order backfill emits (Data.PrivateEpoch) so a stale on-demand
	// fetch is distinguishable from a current one. The per-connect backfill uses
	// the generation captured synchronously at connect (passed to backfillPrivate)
	// instead, so a slow snapshot keeps its own connection's generation even after
	// privateGen advances.
	privateGen atomic.Int64
	// privateUp mirrors the private connection's up/down state (setUp), read by
	// the backfill self-heal loop: a retry pass only runs while the connection
	// that spawned it is still up — once it is down, the coming reconnect's own
	// pass owns recovery, and a REST snapshot succeeding while the WS is down
	// must not present re-baselined state as live.
	privateUp atomic.Bool

	// runCtx is the session's run context (set once at Run start, canceled on
	// stop). Backfill REST reads sign and fetch under it via ctx(), so an
	// in-flight recovery call is canceled when the session stops instead of
	// running its full retry budget while shutdown waits on s.bg. Atomic
	// because the consumer-facing on-demand backfills may read it from any
	// goroutine, concurrently with Run's store.
	runCtx atomic.Pointer[context.Context]

	tradeMu     sync.Mutex
	tradeLastID map[string]int64 // per symbol: highest public tradeId delivered

	myTradeSeen *seenIDs

	// Open-order snapshot tracking (LazyOpenOrders mode). orderMu guards the
	// maps. trackedOrders is the {account, symbol} scope set whose GET
	// /v2/openOrders snapshot the session keeps current (re-snapshotted on every
	// reconnect); inFlightOrders dedupes concurrent on-demand snapshot fetches
	// per scope; orderBaselined records the private generation of each scope's
	// last successful snapshot, so a re-track within the SAME generation (while
	// the feed is up) skips the redundant fetch — the account-wide lossless live
	// feed has kept the scope current since. orderSeqs is the subscription's
	// accountSeq set (nil = the server's default account), fixed from the
	// myOrder subscription at New: the non-lazy per-connect fan-out expands
	// symbols over it, and allowedOrderSeqs (its int form; 0 = default) bounds
	// which scope AccountSeqs may be tracked — the live feed only covers
	// subscribed accounts, so a snapshot outside them would baseline state no
	// live frame ever updates.
	orderMu          sync.Mutex
	trackedOrders    map[OrderScope]bool
	inFlightOrders   map[OrderScope]bool
	orderBaselined   map[OrderScope]int64
	orderSeqs        []*int
	allowedOrderSeqs map[int]bool
}

// OrderScope identifies one open-order snapshot unit in LazyOpenOrders mode:
// one symbol's open orders under one sub-account. AccountSeq <= 0 means the
// server's default account (the snapshot call carries no accountSeq
// parameter) — the only valid value when the myOrder subscription pinned no
// AccountSeqs. A scope's AccountSeq must otherwise be one of the
// subscription's pinned seqs; out-of-subscription scopes are ignored with a
// Debug log (see SetTrackedOrderScopes).
type OrderScope struct {
	AccountSeq int
	Symbol     string
}

// norm canonicalizes the scope for map keying: every "default account"
// spelling (any AccountSeq <= 0) becomes 0.
func (sc OrderScope) norm() OrderScope {
	if sc.AccountSeq < 0 {
		sc.AccountSeq = 0
	}
	return sc
}

// seqPtr is the scope's accountSeq as the REST call parameter: nil for the
// server's default account, the pinned value otherwise.
func (sc OrderScope) seqPtr() *int {
	if sc.AccountSeq <= 0 {
		return nil
	}
	v := sc.AccountSeq
	return &v
}

// seqVal is the int form of a subscription accountSeq entry (nil = 0, the
// default account) — the inverse of OrderScope.seqPtr.
func seqVal(p *int) int {
	if p == nil || *p <= 0 {
		return 0
	}
	return *p
}

// New validates the config and builds a Session.
func New(cfg Config) (*Session, error) {
	if len(cfg.Subscriptions) == 0 {
		return nil, errors.New("stream: at least one subscription is required")
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = DefaultPublicURL
	}
	if cfg.PrivateURL == "" {
		cfg.PrivateURL = DefaultPrivateURL
	}
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.Dial == nil {
		cfg.Dial = DefaultDialer
	}
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = 1024
	}
	if cfg.BackfillRetryBudgetMs <= 0 {
		cfg.BackfillRetryBudgetMs = 15_000
	}

	var publicSubs, privateSubs []Subscription
	hasTrade := false
	for _, sub := range cfg.Subscriptions {
		if err := validateSubscription(sub); err != nil {
			return nil, err
		}
		if IsPrivateChannel(sub.Channel) {
			privateSubs = append(privateSubs, sub)
		} else {
			publicSubs = append(publicSubs, sub)
			if sub.Channel == ChannelTrade {
				hasTrade = true
			}
		}
	}
	if cfg.Client == nil || cfg.Client.Clock == nil {
		return nil, errors.New("stream: a Client with a Clock is required")
	}
	if len(privateSubs) > 0 {
		// The private upgrade signs with these credentials AND sends the api-key id
		// as X-KAPI-KEY, so require both to be present (not just a non-nil Creds):
		// a nil Signer would panic at sign time and an empty APIKeyID would send an
		// empty key header.
		if cfg.Client.Creds == nil || cfg.Client.Creds.APIKeyID == "" || cfg.Client.Creds.Signer == nil {
			return nil, errors.New("stream: private channels require a Client with credentials (api key id + signer)")
		}
		if cfg.Client.BaseURL == "" {
			return nil, errors.New("stream: private channels require the Client's BaseURL (for state backfill and clock sync)")
		}
	}
	if hasTrade && !cfg.DisableBackfill && cfg.Client.BaseURL == "" {
		return nil, errors.New("stream: the trade channel with backfill enabled requires the Client's BaseURL (or set DisableBackfill)")
	}

	// orderSeqs is the accountSeq fan-out for open-order snapshots, taken from the
	// myOrder subscription (the server's default account when none is pinned).
	var orderSub Subscription
	for _, sub := range privateSubs {
		if sub.Channel == ChannelMyOrder {
			orderSub = sub
			break
		}
	}
	s := &Session{
		cfg:            cfg,
		tun:            cfg.Tunables.withDefaults(),
		lg:             logging.Or(cfg.Log),
		clk:            cfg.Client.Clock,
		events:         make(chan Event, cfg.EventBuffer),
		stop:           make(chan struct{}),
		tradeLastID:    make(map[string]int64),
		myTradeSeen:    newSeenIDs(4096),
		trackedOrders:  make(map[OrderScope]bool),
		inFlightOrders: make(map[OrderScope]bool),
		orderBaselined: make(map[OrderScope]int64),
		orderSeqs:      accountSeqValues(orderSub),
	}
	s.allowedOrderSeqs = make(map[int]bool, len(s.orderSeqs))
	for _, p := range s.orderSeqs {
		s.allowedOrderSeqs[seqVal(p)] = true
	}
	// The REST recovery side is the one Client wrapped in the typed layer: it signs
	// (when it has Creds), owns the shared clock (read + resync, so an
	// EXCEED_TIME_WINDOW correction is handled internally), and records each call
	// through its own per-call recorder — whether a backfill read is journaled is
	// decided by callrec.DefaultPolicy (which exempts the "stream-backfill"
	// surface), not at this layer. A creds-less public client still patches public
	// trade gaps; a client with no BaseURL means no REST recovery at all.
	if cfg.Client.BaseURL != "" {
		s.rest = &restClient{
			raw:      rawapi.New(cfg.Client, s.lg),
			budgetMs: cfg.BackfillRetryBudgetMs,
			lg:       s.lg,
		}
	}
	s.keep = &keepalive{tun: s.tun, now: cfg.Now, notice: s.notice, status: func() (int, int) {
		return int(s.upConns.Load()), len(s.conns)
	}}

	if len(publicSubs) > 0 {
		s.pub = s.newConn("public", cfg.PublicURL, publicSubs, nil)
		s.conns = append(s.conns, s.pub)
	}
	if len(privateSubs) > 0 {
		auth := &connAuth{
			header: func() http.Header {
				h := http.Header{}
				// Source the key id from Creds (the value that actually signs), so the
				// upgrade header and the REST signing never diverge. Creds is validated
				// non-nil with a non-empty APIKeyID above.
				h.Set("X-KAPI-KEY", cfg.Client.Creds.APIKeyID)
				return h
			},
			query: s.signedUpgradeQuery,
		}
		s.conns = append(s.conns, s.newConn("private", cfg.PrivateURL, privateSubs, auth))
	}
	return s, nil
}

func validateSubscription(sub Subscription) error {
	switch sub.Channel {
	case ChannelTicker, ChannelOrderbook, ChannelTrade, ChannelMyOrder, ChannelMyTrade, ChannelMyAsset:
	default:
		return fmt.Errorf("stream: unknown channel %q", sub.Channel)
	}
	if sub.Channel != ChannelMyAsset && len(sub.Symbols) == 0 {
		return fmt.Errorf("stream: the %s channel requires at least one symbol", sub.Channel)
	}
	if sub.Level != "" && sub.Channel != ChannelOrderbook {
		return fmt.Errorf("stream: Level applies to the orderbook channel only (got %s)", sub.Channel)
	}
	if len(sub.AccountSeqs) > 0 && !IsPrivateChannel(sub.Channel) {
		return fmt.Errorf("stream: AccountSeqs applies to private channels only (got %s)", sub.Channel)
	}
	if sub.TradeHistory != 0 && sub.Channel != ChannelTrade {
		return fmt.Errorf("stream: TradeHistory applies to the trade channel only (got %s)", sub.Channel)
	}
	if sub.TradeHistory < 0 {
		return fmt.Errorf("stream: TradeHistory must be non-negative (got %d)", sub.TradeHistory)
	}
	return nil
}

func (s *Session) newConn(endpoint, wsURL string, subs []Subscription, auth *connAuth) *connManager {
	// The delivery-delay check reads the shared clock's offset + measured state and,
	// when a suspected delay is measured against an unmeasured clock, kicks a
	// one-off resync (see delayCheck). kickSync is left nil when the client carries
	// no resync (--time-sync off, or a public client with correction disabled),
	// which the delayCheck reads as "warn but do not auto-correct".
	dc := &delayCheck{endpoint: endpoint, tun: s.tun, offsetMs: s.clk.Offset, measured: s.clk.Measured}
	if s.cfg.Client.Resync != nil {
		dc.kickSync = s.kickSync
	}
	m := &connManager{
		endpoint:    endpoint,
		url:         wsURL,
		lg:          s.lg,
		dial:        s.dialWithUA(),
		auth:        auth,
		emit:        s.emitEvent,
		onFrame:     s.makeFrameRouter(dc),
		now:         s.cfg.Now,
		sleep:       s.cfg.Sleep,
		tun:         s.tun,
		noReconnect: s.cfg.NoReconnect,
		health:      &connHealth{endpoint: endpoint, tun: s.tun},
		delay:       dc,
		subs:        subs,
		cmds:        make(chan subCommand, 16),
		setUp: func(up bool) {
			if up {
				s.upConns.Add(1)
			} else {
				s.upConns.Add(-1)
			}
			if endpoint == "private" {
				s.privateUp.Store(up)
			}
		},
	}
	if endpoint == "private" {
		m.onUp = func(reconnect bool, downtimeMs int64, gen int) {
			s.privateGen.Store(int64(gen))
			s.bg.Add(1)
			go func() {
				defer s.bg.Done()
				s.backfillMu.Lock()
				plan, units := s.backfillPrivate(reconnect, downtimeMs, int64(gen))
				s.backfillMu.Unlock()
				// Self-heal outside the mutex: the loop sleeps between passes and
				// must not block a newer connect's own backfill while it does.
				s.retryFailedBackfill(plan, units)
			}()
		}
	} else {
		m.onUp = func(bool, int64, int) {}
	}
	// The handshake-ETW resync hook is the Client's one resync (shared with REST
	// backfill and every other signer in the process). nil when the client has no
	// resync wired (e.g. a public session that never auto-corrects).
	if s.cfg.Client.Resync != nil {
		m.resyncClock = s.cfg.Client.Resync
	}
	return m
}

// dialWithUA stamps the configured User-Agent onto every dial; an empty
// Config.UserAgent falls back to the bare "digitalx-cli/<version>".
func (s *Session) dialWithUA() Dialer {
	dial := s.cfg.Dial
	ua := s.cfg.UserAgent
	if ua == "" {
		ua = version.Token()
	}
	return func(ctx context.Context, wsURL string, header http.Header) (Conn, error) {
		if header == nil {
			header = http.Header{}
		}
		header.Set("User-Agent", ua)
		return dial(ctx, wsURL, header)
	}
}

// signedUpgradeQuery builds the private endpoint's signed upgrade query through
// the session's one Client (Client.SignHandshake), which uses the SAME
// ordered-encode + signature-last core as REST signing and the same shared clock
// (timestamp + the widened recvWindow, omit-below-5s). Signed fresh on every dial.
func (s *Session) signedUpgradeQuery() (string, error) {
	return s.cfg.Client.SignHandshake()
}

// Events is the session's output. It delivers Data and Notice values in
// emission order and is closed when Run returns; consume until closed.
func (s *Session) Events() <-chan Event { return s.events }

// Subscribe adds a PUBLIC market-data subscription (orderbook or trade) to the
// live session: the request is sent now if a public connection is up, and the
// change is recorded so a reconnect re-subscribes it. It is how a consumer
// follows a changing focus — e.g. the TUI switching the active symbol's
// orderbook/trades — without tearing down the session. Safe to call from any
// goroutine and never blocks.
//
// Restricted to the public orderbook/trade channels on purpose: ticker is
// subscribed up front (it powers the symbol list, which never shrinks), and the
// private channels are fixed at New because their per-connect backfill and
// per-key ordering assume a stable symbol set. A trade subscription may carry
// TradeHistory to seed depth on its first snapshot (see Subscription).
func (s *Session) Subscribe(sub Subscription) error {
	if err := validateSubscription(sub); err != nil {
		return err
	}
	if sub.Channel != ChannelOrderbook && sub.Channel != ChannelTrade {
		return fmt.Errorf("stream: dynamic Subscribe supports the orderbook and trade channels only (got %s)", sub.Channel)
	}
	if s.pub == nil {
		return errors.New("stream: no public connection to subscribe on")
	}
	s.pub.applyChange(opSubscribe, sub)
	return nil
}

// Unsubscribe removes a public orderbook/trade subscription for the given
// symbols (the counterpart to Subscribe), matched by channel + symbols
// regardless of any grouping level. Same constraints; never blocks.
func (s *Session) Unsubscribe(channel string, symbols []string) error {
	if channel != ChannelOrderbook && channel != ChannelTrade {
		return fmt.Errorf("stream: dynamic Unsubscribe supports the orderbook and trade channels only (got %s)", channel)
	}
	if len(symbols) == 0 {
		return errors.New("stream: Unsubscribe requires at least one symbol")
	}
	if s.pub == nil {
		return errors.New("stream: no public connection to unsubscribe on")
	}
	s.pub.applyChange(opUnsubscribe, Subscription{Channel: channel, Symbols: symbols})
	return nil
}

// SetTrackedOrderScopes sets the {account, symbol} scope set whose open-order
// snapshot the session keeps authoritative (LazyOpenOrders mode). It is how a
// consumer follows a changing focus — e.g. the TUI snapshotting just the
// active pair's open orders under the active sub-account, or every pair in an
// "all pairs" view — without changing the account-wide live myOrder
// subscription. Newly-tracked scopes are snapshotted now (GET /v2/openOrders;
// a transient failure self-heals with backoff while the private connection
// stays up) when the session is already running; before Run, or while
// reconnecting, the next per-connect backfill covers the tracked set.
//
// A re-tracked scope whose last successful snapshot was taken in the CURRENT
// private connection is NOT re-snapshotted while the feed is up: the myOrder
// feed is lossless and account-wide while connected, so the store's view of
// that scope has stayed current since its baseline — flipping focus back and
// forth within one connection costs no REST calls. Across a reconnect the
// memo generation no longer matches, so the scope is re-baselined the normal
// way (the per-connect pass when tracked at connect time, or on demand at the
// next re-track). Dropped scopes keep their stored state only as
// reconciliation context for that future re-baseline; consumers must gate
// display on the store's readiness (cleared on a private disconnect), never on
// the data's presence. A scope whose AccountSeq is not covered by the myOrder
// subscription is ignored (Debug-logged): the live feed doesn't cover it, so a
// snapshot would baseline state no frame ever updates.
//
// Important stream/state contract: these snapshots are authoritative for
// baseline/readiness and for cross-epoch disappearance inference. They do not
// close an order first seen in the current private-connection epoch; same-epoch
// order closes must arrive as live terminal myOrder frames (the private feed's
// lossless-while-connected contract). Safe to call from any goroutine; never
// blocks. A no-op unless LazyOpenOrders is set, and inert once the session has
// stopped.
func (s *Session) SetTrackedOrderScopes(scopes []OrderScope) {
	if !s.cfg.LazyOpenOrders {
		return
	}
	up := s.privateUp.Load()
	gen := s.privateGen.Load()
	s.orderMu.Lock()
	next := make(map[OrderScope]bool, len(scopes))
	var added, skipped []OrderScope
	for _, sc := range scopes {
		sc = sc.norm()
		if sc.Symbol == "" || next[sc] {
			continue
		}
		if !s.allowedOrderSeqs[sc.AccountSeq] {
			s.log().Debug("tracked order scope ignored: accountSeq not subscribed",
				"accountSeq", sc.AccountSeq, "symbol", sc.Symbol)
			continue
		}
		next[sc] = true
		if s.trackedOrders[sc] {
			continue
		}
		// Baselined in the current generation with the feed up: current by the
		// lossless-while-connected contract, no fetch needed (see the doc above).
		if b, ok := s.orderBaselined[sc]; ok && up && b == gen {
			skipped = append(skipped, sc)
			continue
		}
		added = append(added, sc)
	}
	s.trackedOrders = next
	s.orderMu.Unlock()
	// Close the one non-benign race on the up/gen pair read above: a RECONNECT
	// landing between that read and the set swap. Its per-connect pass may have
	// read the tracked set before the swap (missing the re-tracked scopes), while
	// the memo skip compared against the superseded generation — leaving a
	// skipped scope unbaselined in the new generation until the next focus
	// change. If the generation moved, fetch the skipped scopes after all (the
	// worst case is one redundant snapshot; the store's newest-wins guard absorbs
	// it). A plain disconnect needs nothing: the scopes stay tracked and the
	// coming reconnect's pass — which reads the set after this swap — covers them.
	if len(skipped) > 0 && s.privateGen.Load() != gen {
		added = append(added, skipped...)
	}
	// Snapshot the newly-tracked scopes only once Run is underway (the REST path
	// works even while the WS is down; a result superseded by a concurrent
	// reconnect backfill is dropped by the store's newest-wins guard). Before Run
	// this only seeds the set — the first per-connect backfill covers it. The
	// batch is bounded-concurrent so toggling an "all pairs" view over hundreds of
	// pairs doesn't fan an unbounded burst of GET /v2/openOrders out at once.
	if s.started.Load() {
		s.backfillOpenOrdersBatch(added)
	}
}

// BackfillOpenOrders refreshes the authoritative open-order snapshot for one
// currently tracked scope (GET /v2/openOrders) on a background goroutine and
// emits it as an OriginBackfill event. It is an explicit refresh: unlike a
// re-track it does NOT consult the same-generation baseline memo, so a caller
// that wants a fresh snapshot despite a current baseline gets one. It does not
// add the scope to the tracked set; callers that change focus use
// SetTrackedOrderScopes, which records the tracked set and then snapshots newly
// tracked scopes. Untracked scopes' stored rows are reconciliation context for
// a future re-track; consumers gate display on the store's readiness. In
// stream/state a current tracked snapshot updates the scope's baseline and can
// mark orders from an earlier private-connection epoch as closed-unknown when
// absent; it deliberately does not close same-epoch orders that should be resolved
// by live terminal myOrder frames. Concurrent requests for the same tracked scope
// are deduped, including against a pending self-heal retry of a failed fetch
// (which already guarantees the snapshot). A no-op unless LazyOpenOrders is set,
// when the scope is untracked, when REST is not configured, or once the session
// has stopped.
func (s *Session) BackfillOpenOrders(scope OrderScope) {
	if !s.cfg.LazyOpenOrders {
		return
	}
	s.backfillOpenOrdersBatch([]OrderScope{scope.norm()})
}

// backfillOpenOrdersBatch snapshots open orders for the given tracked scopes on
// a single background goroutine, bounded-concurrent (via backfillOrderSnapshots),
// skipping any scope whose snapshot is already in flight or whose tracked-set
// membership was dropped. This is the on-demand path (SetTrackedOrderScopes /
// BackfillOpenOrders); it shares the same per-scope dedup and the same
// concurrency bound as the per-connect fan-out, so growing the tracked set — e.g.
// switching to an "all pairs" view — can never burst an unbounded number of REST
// calls. A no-op when REST is not configured.
//
// Transiently-failed snapshots self-heal exactly like a per-connect pass's
// (retryFailedBackfill), so a scope tracked mid-session still becomes ready
// eventually while the connection stays up. While the retry owns a failed
// scope its in-flight mark stays held, which makes the dedup above CORRECT
// rather than lossy for a re-track during the failure window: the queued unit
// already guarantees the snapshot, so skipping the duplicate loses nothing.
// A mark can never dangle or swallow a re-track: a stale-dropped unit
// releases its mark atomically with the drop decision (the unit's stale
// closure in backfillOrderSnapshot), and every other loop exit (healed,
// superseded, feed down, or session stop) releases the remainder via the
// deferred release below.
func (s *Session) backfillOpenOrdersBatch(scopes []OrderScope) {
	if s.rest == nil {
		return
	}
	s.orderMu.Lock()
	todo := make([]OrderScope, 0, len(scopes))
	for _, sc := range scopes {
		if sc.Symbol == "" || s.inFlightOrders[sc] {
			continue
		}
		if s.cfg.LazyOpenOrders && !s.trackedOrders[sc] {
			continue
		}
		s.inFlightOrders[sc] = true
		todo = append(todo, sc)
	}
	s.orderMu.Unlock()
	if len(todo) == 0 {
		return
	}
	if !s.startBg() {
		// Shutting down (or already stopped): nothing may emit anymore. Release
		// the in-flight marks taken above so they don't dangle.
		s.releaseOrderInFlight(todo)
		return
	}
	gen := s.privateGen.Load()
	go func() {
		defer s.bg.Done()
		c := &unitCollector{}
		s.backfillOrderSnapshots(todo, gen, c)
		pending := make(map[OrderScope]bool, len(c.units))
		pendingList := make([]OrderScope, 0, len(c.units))
		for _, u := range c.units {
			sc := OrderScope{AccountSeq: seqVal(u.seq), Symbol: u.symbol}
			if u.symbol != "" && !pending[sc] {
				pending[sc] = true
				pendingList = append(pendingList, sc)
			}
		}
		healed := make([]OrderScope, 0, len(todo))
		for _, sc := range todo {
			if !pending[sc] {
				healed = append(healed, sc)
			}
		}
		s.releaseOrderInFlight(healed)
		if len(c.units) == 0 {
			return
		}
		if !s.privateUp.Load() {
			// The private feed is down: the next connect's own pass re-snapshots
			// every tracked scope, so recovery ownership moves there. Release the
			// marks now so a later re-track is not pointlessly deduped while
			// nothing is retrying.
			s.releaseOrderInFlight(pendingList)
			return
		}
		defer s.releaseOrderInFlight(pendingList)
		s.retryFailedBackfill(privatePlan{gen: gen}, c.units)
	}()
}

// releaseOrderInFlight clears the on-demand in-flight snapshot marks for the
// given scopes, so a later SetTrackedOrderScopes/BackfillOpenOrders for them
// fires a fresh fetch instead of being deduped.
func (s *Session) releaseOrderInFlight(scopes []OrderScope) {
	if len(scopes) == 0 {
		return
	}
	s.orderMu.Lock()
	for _, sc := range scopes {
		delete(s.inFlightOrders, sc)
	}
	s.orderMu.Unlock()
}

// startBg registers one background goroutine with the session's shutdown wait,
// refusing (false) once shutdown has begun — a goroutine admitted then could
// outlive Run's bg.Wait and emit on the closed events channel. Only the
// consumer-facing paths (the on-demand open-order backfills, callable from any
// goroutine at any time) need this gate; internal spawns happen-before Run sets
// stopping.
func (s *Session) startBg() bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.stopping {
		return false
	}
	s.bg.Add(1)
	return true
}

// Run connects, streams, and recovers until ctx is canceled (returns nil) or
// a fatal condition stops the session (a Fatal notice precedes the error).
// Call once.
func (s *Session) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("stream: Run may only be called once per Session")
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Backfill goroutines (spawned from connection callbacks below) fetch under
	// this context, so they cancel promptly when the session stops.
	s.runCtx.Store(&rctx)

	s.log().Debug("session run",
		"endpoints", len(s.conns), "subscriptions", len(s.cfg.Subscriptions),
		"proactiveTimeSync", s.cfg.ProactiveTimeSync, "noReconnect", s.cfg.NoReconnect,
		"backfill", !s.cfg.DisableBackfill, "snapshotOnly", s.cfg.BackfillSnapshotOnly,
		"eventBuffer", cap(s.events))

	// Measure the server clock up front (ProactiveTimeSync only) when the Client
	// can resync, SYNCHRONOUSLY before any connection is dialed: the first frames'
	// delivery-delay check and the first private-upgrade signature must see a
	// corrected clock. It runs in a goroutine with a stop-interruptible wait —
	// otherwise a skewed local clock races the measurement and trips a false
	// DATA_DELAYED (and a needless EXCEED_TIME_WINDOW upgrade round-trip) on the
	// very first frame. This is the deliberate cost of --time-sync on (correctness
	// before the first frame): the wait spans MeasureClockOffset's sequential
	// /v2/time probes — each capped well under --timeout (see
	// maxClockProbeTimeoutMs), which bounds the worst case — and on failure
	// proceeds with the local clock. Without ProactiveTimeSync the estimate is corrected reactively
	// instead — on an EXCEED_TIME_WINDOW rejection, or when the delivery-delay
	// check kicks a resync against an unmeasured clock (see kickSync). The probe
	// goroutine is untracked by s.bg (it emits nothing and only installs the shared
	// offset), so a late completion after an early stop is harmless.
	if s.cfg.Client.Resync != nil && s.cfg.ProactiveTimeSync {
		done := make(chan struct{})
		go func() { defer close(done); _ = s.resyncClock("at start") }()
		select {
		case <-done:
		case <-rctx.Done():
			s.log().Debug("clock sync at start canceled before completion")
		}
	}

	kaDone := make(chan struct{})
	go func() {
		defer close(kaDone)
		s.keep.run(s.stop)
	}()

	errCh := make(chan error, len(s.conns))
	var wg sync.WaitGroup
	for _, m := range s.conns {
		wg.Add(1)
		go func(m *connManager) {
			defer wg.Done()
			errCh <- m.run(rctx)
		}(m)
	}

	var fatal error
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) && rctx.Err() == nil {
			fatal = err
		}
	case <-ctx.Done():
	}

	cancel()
	close(s.stop) // unblock anything mid-emit
	wg.Wait()     // connection goroutines (incl. their ping loops)
	// Refuse consumer-driven bg spawns from here on: everything bg.Wait sees is
	// final, so nothing can emit after the events channel closes below.
	s.bgMu.Lock()
	s.stopping = true
	s.bgMu.Unlock()
	s.bg.Wait() // backfill goroutines
	<-kaDone    // the keepalive emitter
	close(s.events)
	return fatal
}

// emitEvent delivers to the consumer, giving up only when the session is
// shutting down.
func (s *Session) emitEvent(ev Event) {
	// Fast path: deliver without blocking. When the buffer is full the consumer
	// is falling behind — log the backpressure (the mechanic behind a stalled
	// read → eventual reconnect) before blocking on the slow path.
	select {
	case s.events <- ev:
		return
	default:
	}
	s.log().Debug("event buffer full", "cap", cap(s.events))
	select {
	case s.events <- ev:
	case <-s.stop:
	}
}

func (s *Session) emitData(d Data) {
	s.keep.touch()
	s.emitEvent(d)
}

// log is the session's operational logger, resolved once at New (never nil), so
// call sites log unconditionally — a session with no Config.Log discards every
// record. The stream layer's Info/Warn story is the notice channel; these logs
// are Debug-dominant mechanics that complement it.
func (s *Session) log() *slog.Logger { return s.lg }

func (s *Session) notice(code NoticeCode, level Level, msg string, details map[string]any) {
	s.emitEvent(Notice{Code: code, Level: level, Message: msg, Details: details, Time: s.cfg.Now()})
}

func (s *Session) noticef(code NoticeCode, level Level, details map[string]any, format string, args ...any) {
	s.notice(code, level, fmt.Sprintf(format, args...), details)
}

// serverNow is the current server-clock estimate (local clock when the offset
// is unmeasured). Used to stamp backfill events so consumers can order them
// against live frame timestamps.
func (s *Session) serverNow() int64 { return s.cfg.Now() + s.clk.Offset() }

// resyncClock runs the one shared server-clock measurement and logs the outcome
// at Debug — the installed offset on success, the error otherwise — tagging the
// line with `when` so the startup ("at start") and delay-triggered
// ("delay-triggered") syncs are distinguishable. Both callers route through it
// so the invoke-and-log stays in one place; only the wait/cancel wrapper differs.
func (s *Session) resyncClock(when string) error {
	if err := s.cfg.Client.Resync(); err != nil {
		s.log().Debug("clock sync "+when+" failed", "error", err.Error())
		return err
	}
	s.log().Debug("clock sync "+when, "offsetMs", s.clk.Offset())
	return nil
}

// kickSync triggers a one-off server-clock measurement OFF the hot frame path,
// used by the delivery-delay check to disambiguate a suspected delay from local
// clock skew while the clock is still unmeasured (the --time-sync auto reactive
// path). It never blocks the caller: the measurement runs on its own goroutine
// and coalesces onto the shared clock.Syncer (single-flight + cooldown), so a
// burst of triggers collapses to at most one /v2/time probe. It emits no notice
// — it only installs the offset a later frame will read — so it adds no ordering
// race with DATA_DELAYED/DATA_CURRENT. Deliberately NOT registered with s.bg:
// it touches neither the events channel nor any shutdown-guarded state, so a
// probe still in flight at shutdown is harmless and must not delay Run's return
// by up to a REST timeout. Wired only when the client can resync (see newConn).
func (s *Session) kickSync() {
	go func() { _ = s.resyncClock("delay-triggered") }()
}

// sleepStop waits d, returning false when the session stopped first. Unlike
// cfg.Sleep (a plain blocking sleep, tolerable for the ≤15s reconnect
// ladder), this is stop-interruptible: the backfill self-heal backoff caps
// at minutes, and Run's shutdown waits on that goroutine via s.bg.
func (s *Session) sleepStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.stop:
		return false
	}
}

func (s *Session) privateChannels() []string {
	var out []string
	for _, sub := range s.cfg.Subscriptions {
		if IsPrivateChannel(sub.Channel) {
			out = append(out, sub.Channel)
		}
	}
	return out
}

// makeFrameRouter routes one connection's data frames: delay measurement,
// then per-channel handling (trade and myTrade get dedupe/gap logic; the
// rest pass through verbatim).
func (s *Session) makeFrameRouter(dc *delayCheck) func(raw []byte, env frameEnvelope) {
	return func(raw []byte, env frameEnvelope) {
		dc.observe(env.Timestamp, s.cfg.Now(), s.notice)
		switch env.channel() {
		case ChannelTrade:
			s.handleTradeFrame(raw, env)
		case ChannelMyTrade:
			s.handleMyTradeFrame(raw, env)
		case "":
			// No channel field: nothing routable; drop.
		default:
			s.emitData(Data{
				Channel: env.channel(), Symbol: env.Symbol,
				Origin: originOf(env), ServerTime: env.Timestamp, Payload: raw,
				AccountSeq: frameAccountSeq(raw, env.channel()),
			})
		}
	}
}

func originOf(env frameEnvelope) Origin {
	if env.Snapshot {
		return OriginSnapshot
	}
	return OriginRealtime
}

// frameAccountSeq extracts the sub-account from a live private frame: the
// accountSeq the server stamps on the order/trade/asset wrapper. It is present
// only when the subscription requested accountSeqs (the server omits it
// otherwise), so this returns nil for an untagged frame and for any public
// channel — a nil that consumers read as the default account (1).
func frameAccountSeq(raw []byte, channel string) *int {
	if !IsPrivateChannel(channel) {
		return nil
	}
	var wrap struct {
		Order *struct {
			AccountSeq *int `json:"accountSeq"`
		} `json:"order"`
		Trade *struct {
			AccountSeq *int `json:"accountSeq"`
		} `json:"trade"`
		Asset *struct {
			AccountSeq *int `json:"accountSeq"`
		} `json:"asset"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return nil
	}
	switch channel {
	case ChannelMyOrder:
		if wrap.Order != nil {
			return wrap.Order.AccountSeq
		}
	case ChannelMyTrade:
		if wrap.Trade != nil {
			return wrap.Trade.AccountSeq
		}
	case ChannelMyAsset:
		if wrap.Asset != nil {
			return wrap.Asset.AccountSeq
		}
	}
	return nil
}

// tradeHistoryFor returns the requested initial trade-history depth for a
// symbol's trade subscription, 0 when none was asked for. It reads the live
// public registry (not the static config) so the seed also fires for a symbol
// whose trade channel was added dynamically via Subscribe (the active-symbol
// switch in the TUI).
func (s *Session) tradeHistoryFor(symbol string) int {
	if s.pub == nil {
		return 0
	}
	return s.pub.tradeHistoryWant(symbol)
}

// handleTradeFrame applies the public-trade continuity logic. tradeIds are
// per-symbol monotonic but not contiguous: rows at or below the delivered
// high-water mark are duplicates (resubscribe snapshots overlap what was
// already delivered) and are dropped; when a resubscribe snapshot's oldest
// row sits more than one id above the mark, the range between them is patched
// from REST asynchronously — best-effort, since non-contiguous ids make a real
// gap indistinguishable from a normal id skip (see patchTradeGap).
func (s *Session) handleTradeFrame(raw []byte, env frameEnvelope) {
	var frame struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Data == nil {
		// Unexpected shape: pass through rather than suppress.
		s.emitData(Data{Channel: ChannelTrade, Symbol: env.Symbol, Origin: originOf(env), ServerTime: env.Timestamp, Payload: raw})
		return
	}

	minID, maxID := int64(0), int64(0)
	ids := make([]int64, len(frame.Data))
	for i, row := range frame.Data {
		id := rowTradeID(row)
		ids[i] = id
		if id > 0 {
			if minID == 0 || id < minID {
				minID = id
			}
			if id > maxID {
				maxID = id
			}
		}
	}

	s.tradeMu.Lock()
	last := s.tradeLastID[env.Symbol]
	if maxID > last {
		s.tradeLastID[env.Symbol] = maxID
	}
	s.tradeMu.Unlock()

	var patchGap bool
	var gapLow, snapMin int64
	if env.Snapshot && minID > 0 {
		switch {
		case last == 0:
			// The first snapshot for this symbol: seed recent history from REST
			// when the subscription asked for it, so the consumer starts with
			// depth rather than only the rows this snapshot happened to carry.
			// Unlike a reconnect gap, this seed emits NO BACKFILL_START, so
			// derived state (e.g. the state store's LastTick) is intentionally
			// NOT gated on it: with no prior high-water mark there is no provable
			// gap or completeness to signal, and the seed is best-effort depth.
			// The cost is a possibly-wrong initial tick until the first live
			// trade folds forward; do not "fix" it by gating without first giving
			// the seed a completeness protocol (see backfill.go seedTradeHistory).
			if n := s.tradeHistoryFor(env.Symbol); n > 0 && !s.cfg.DisableBackfill && s.rest != nil {
				snapMin, snapCount := minID, len(frame.Data)
				s.bg.Add(1)
				go func() {
					defer s.bg.Done()
					s.seedTradeHistory(env.Symbol, n, snapCount, snapMin)
				}()
			}
		case minID > last+1:
			// The snapshot's oldest row sits more than one id above the high-water
			// mark, so trades may have been missed while disconnected — backfill
			// the range from REST. Non-contiguous ids mean this also fires when
			// nothing was missed (the range simply held no trades), in which case
			// the backfill recovers nothing. Tell consumers before emitting the
			// snapshot so derived state that needs the missing predecessor can stay
			// neutral until the range is patched or reported unrecoverable.
			gapLow, snapMin = last, minID
			s.log().Debug("trade gap detected",
				"symbol", env.Symbol, "afterTradeId", gapLow, "beforeTradeId", snapMin)
			switch {
			case s.cfg.DisableBackfill:
				s.noticePublicTradeGapDisabled(env.Symbol, gapLow, snapMin)
			case s.rest == nil:
				s.noticePublicTradeGapUnavailable(env.Symbol, gapLow, snapMin)
			default:
				patchGap = true
				s.noticef(BackfillStart, LevelInfo,
					publicTradeGapDetails(env.Symbol, gapLow, snapMin),
					"backfilling public trades for %s between tradeId %d and %d", env.Symbol, gapLow, snapMin)
			}
		}
	}

	kept := frame.Data
	if last > 0 {
		kept = make([]json.RawMessage, 0, len(frame.Data))
		for i, row := range frame.Data {
			if ids[i] == 0 || ids[i] > last {
				kept = append(kept, row)
			}
		}
	}
	if len(frame.Data) > 0 && len(kept) == 0 && !env.Snapshot {
		return // a live frame of only already-delivered rows: nothing to add
	}
	// A resubscribe SNAPSHOT whose rows were all already delivered still emits —
	// as an empty data frame — rather than being suppressed. The snapshot marks
	// the (re)subscribe boundary, so emitting it keeps the stream's "up to date or
	// TOLD" guarantee: a consumer gating freshness on snapshot receipt (e.g. the
	// TUI's per-symbol trade latch) learns the channel is current again instead of
	// waiting forever for a frame that, with no new trades, never comes.

	payload := raw
	if len(kept) != len(frame.Data) {
		// Rebuild the frame carrying only the not-yet-delivered rows. The marshal
		// is fallible only in principle: encoding/json re-validates each
		// json.RawMessage as it writes it, but these rows were just parsed out of
		// the inbound frame above, so they are valid JSON and the scalar fields
		// cannot fail either — err is effectively unreachable.
		rebuilt, err := json.Marshal(struct {
			Type      string            `json:"type"`
			Timestamp int64             `json:"timestamp"`
			Symbol    string            `json:"symbol"`
			Snapshot  bool              `json:"snapshot,omitempty"`
			Data      []json.RawMessage `json:"data"`
		}{Type: ChannelTrade, Timestamp: env.Timestamp, Symbol: env.Symbol, Snapshot: env.Snapshot, Data: kept})
		if err == nil {
			payload = rebuilt
		}
		// The err==nil guard keeps this fail-safe rather than clever: on the
		// unreachable failure we fall through with the verbatim frame. The ids
		// are already marked delivered, so suppressing it would silently lose the
		// fresh rows, while re-delivering a few duplicates is harmless (every
		// consumer dedupes by tradeId) — the stream's duplicates-over-loss rule.
	}
	s.emitData(Data{Channel: ChannelTrade, Symbol: env.Symbol, Origin: originOf(env), ServerTime: env.Timestamp, Payload: payload})
	if patchGap {
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			s.patchTradeGap(env.Symbol, gapLow, snapMin)
		}()
	}
}

// handleMyTradeFrame dedupes fills against everything already delivered
// (live or backfill) by tradeId, so a REST gap patch overlapping the live
// stream can never double-deliver a fill.
func (s *Session) handleMyTradeFrame(raw []byte, env frameEnvelope) {
	var frame struct {
		Trade struct {
			AccountSeq *int              `json:"accountSeq"`
			Trades     []json.RawMessage `json:"trades"`
		} `json:"trade"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Trade.Trades == nil {
		s.emitData(Data{Channel: ChannelMyTrade, Symbol: env.Symbol, Origin: originOf(env), ServerTime: env.Timestamp, Payload: raw, AccountSeq: frameAccountSeq(raw, ChannelMyTrade)})
		return
	}

	kept := make([]json.RawMessage, 0, len(frame.Trade.Trades))
	for _, row := range frame.Trade.Trades {
		id := rowTradeID(row)
		if id == 0 || s.myTradeSeen.checkAndMark(env.Symbol, id) {
			kept = append(kept, row)
		}
	}
	if len(frame.Trade.Trades) > 0 && len(kept) == 0 {
		return
	}

	payload := raw
	if len(kept) != len(frame.Trade.Trades) {
		var rebuilt struct {
			ChannelType string `json:"channelType"`
			Timestamp   int64  `json:"timestamp"`
			Symbol      string `json:"symbol"`
			Trade       struct {
				AccountSeq *int              `json:"accountSeq,omitempty"`
				Trades     []json.RawMessage `json:"trades"`
			} `json:"trade"`
		}
		rebuilt.ChannelType = ChannelMyTrade
		rebuilt.Timestamp = env.Timestamp
		rebuilt.Symbol = env.Symbol
		rebuilt.Trade.AccountSeq = frame.Trade.AccountSeq
		rebuilt.Trade.Trades = kept
		b, err := json.Marshal(rebuilt)
		if err == nil {
			payload = b
		}
		// On a marshal failure fall through with the verbatim frame (see
		// handleTradeFrame): duplicates are harmless, silent loss is not.
	}
	s.emitData(Data{Channel: ChannelMyTrade, Symbol: env.Symbol, Origin: originOf(env), ServerTime: env.Timestamp, Payload: payload, AccountSeq: frame.Trade.AccountSeq})
}

// seenIDs is a per-symbol bounded set of delivered tradeIds (FIFO eviction).
// The capacity only needs to exceed the overlap between a REST gap patch and
// the live stream — a few thousand ids dwarfs any realistic overlap.
type seenIDs struct {
	capacity int
	mu       sync.Mutex
	bySymbol map[string]*idWindow
}

type idWindow struct {
	set   map[int64]struct{}
	order []int64
	next  int
}

func newSeenIDs(capacity int) *seenIDs {
	return &seenIDs{capacity: capacity, bySymbol: make(map[string]*idWindow)}
}

// checkAndMark returns true (and records the id) when the id was not seen
// before.
func (s *seenIDs) checkAndMark(symbol string, id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.bySymbol[symbol]
	if w == nil {
		w = &idWindow{set: make(map[int64]struct{})}
		s.bySymbol[symbol] = w
	}
	if _, dup := w.set[id]; dup {
		return false
	}
	if len(w.order) < s.capacity {
		w.order = append(w.order, id)
	} else {
		delete(w.set, w.order[w.next])
		w.order[w.next] = id
		w.next = (w.next + 1) % s.capacity
	}
	w.set[id] = struct{}{}
	return true
}
