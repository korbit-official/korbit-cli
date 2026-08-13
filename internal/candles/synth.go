// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package candles

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

// Synth derives the monitor's synthesized `candle` channel: it consumes the
// session's public trade events and reliability notices and produces candle
// Data events (Origin [stream.OriginDerived], channel [Channel]) plus its own
// BACKFILL_* notices, one [Series] per symbol×interval.
//
// # Guarantees, and where they come from
//
// The candle stream inherits the trade pipeline's guarantees instead of
// re-implementing them: while the public connection is up, the session's trade
// stream is deduped and gap-patched, so folded buckets are exact over the
// DELIVERED trades — there is NO periodic REST re-sync. (Exact over delivered,
// not over executed: the public endpoint may drop a frame mid-connection
// without a reconnect, which is undetectable — see the stream package doc — so
// a derived bar can differ slightly from the server's own aggregation. The
// REST candles endpoint stays authoritative where exact values matter; this
// channel is for real-time signals.) REST is touched only to SEED a scope (the current
// partial bucket plus the requested history depth; a subscription that starts
// mid-bucket cannot learn the bucket's true open/high/low/volume from trades
// alone) and to RE-SEED it after an event that may have cost trades a fold: a
// public reconnect, a trade DATA_GAP, or a completed gap patch (whose rows can
// land behind an already-rolled live edge, where folding must not touch
// history). A failed seed self-heals on an exponential backoff, each attempt a
// fresh BACKFILL_START (reason "retry"), mirroring the stream layer's private
// recovery. Like that layer, every BACKFILL_START is PAIRED with a
// BACKFILL_DONE — on failure the DONE (failures:1) follows the BACKFILL_FAILED
// alarm, and a result discarded because its scope died still closes its START
// — because consumers balance on the pairing (the --stateful store's backfill
// counter would otherwise wedge Health.Backfilling true forever).
//
// # Seed ordering: the snapshot kicks the fetch; in-flight trades replay
//
// The initial and reconnect seeds are kicked by the trade subscription's OWN
// snapshot frame, not eagerly, and every kick captures the trade-id watermark
// at that moment: everything delivered before the kick was executed before
// the request went out, so it is inside the fetched rows and must not
// re-fold. Trades delivered while the fetch is in flight are BUFFERED (not
// folded) and replayed on top of the authoritative rows at Apply
// ([Series.SeedRebase] resets the fold mark to the kick watermark), so a
// trade the fetch missed can never be lost — without the replay, a trade
// executed after the response was built but delivered before it applied
// would vanish, and a finalizing bucket could keep a stale close/extreme
// forever. The residual error runs the other, self-healing way: a buffered
// trade executed BEFORE the response was built is counted by both the fetched
// row and its replay, a volume overcount bounded by one round trip's trades
// on the applied bucket, gone at the next rollover; prices are exact (replay
// re-asserts real extremes and ends at the true latest close). Gap-patch
// triggers (DATA_GAP, gap BACKFILL_DONE) re-seed immediately — their rows
// were delivered before the notice by construction.
//
// Consumers order per key: the key is {interval, timestamp} and the NEWEST
// line for a key supersedes prior lines — a re-seed re-emits corrected bars
// (final:true for closed buckets) under exactly that rule. A bucket with no
// trades still closes on time (see Tick) as a flat zero-volume bar, and closed
// buckets a REST seed skipped are filled the same way on emission, so the
// emitted series is gapless on its grid.
//
// # Finalization clock
//
// Trade-driven rollover finalizes immediately — a strictly-newer trade past
// the boundary proves the bucket complete. In a quiet market Tick finalizes
// instead, comparing the bucket end against the SERVER-clock estimate (this is
// a compare-against-server-timestamps use, the trade/bucket timestamps being
// server-issued) minus a grace so a slightly-late frame still lands. While the
// public connection is down finalization is suspended — the synthesizer cannot
// know the market went quiet — and the post-reconnect re-seed catches history
// up.
//
// # Threading
//
// All methods run on the monitor's single event-loop goroutine. The only
// concurrency is the seed fetches: goroutines that call Fetch and post to the
// Results channel; the loop selects on Results and feeds each value back into
// Apply. Start wires the fetch context and must be called exactly once, before
// any event is fed.
type Synth struct {
	cfg    Config
	ctx    context.Context
	sem    chan struct{}   // bounds concurrent seed fetches
	out    chan SeedResult // fetch results, consumed by the monitor loop via Results/Apply
	scopes map[scopeKey]*scope
	keys   []scopeKey       // deterministic iteration order (symbols × intervals as configured)
	seen   map[string]int64 // per-symbol max trade id delivered, the seed fold watermark
	up     bool             // public connection state; finalization gates on it
	upOnce bool             // a public CONNECTED was seen; distinguishes reconnects
}

// Config configures a Synth. Symbols and Intervals must be normalized and
// validated by the caller (canonical interval values, deduped).
type Config struct {
	Symbols   []string
	Intervals []string
	// History is the number of closed candles to emit per scope before live
	// streaming (the --candle-history value; 0 = live only).
	History int
	// Fetch returns up to limit REST candle rows for symbol+interval ending at
	// endMs (0 = up to the current still-open bucket), ascending. It must be
	// safe for concurrent use.
	Fetch func(ctx context.Context, symbol, interval string, limit int, endMs int64) ([]Bar, error)
	// ServerNow is the server-clock estimate in unix ms (quiet-market
	// finalization compares bucket ends against server-issued timestamps).
	ServerNow func() int64
	// Now is the local clock, stamped on the notices this layer raises.
	Now func() int64
	// Log is an optional operational logger (nil = silent).
	Log *slog.Logger

	// Tunables (0 = default), exposed so tests run the real machinery fast.
	FinalizeGraceMs int64 // quiet-market finalization lag; default 2000
	RetryMinMs      int64 // seed-retry backoff floor; default 1000
	RetryMaxMs      int64 // seed-retry backoff cap; default 30000
	ReplayBufferCap int   // per-scope in-flight trade buffer cap; default 8192
}

const (
	defaultGraceMs    = 2000
	defaultRetryMinMs = 1000
	defaultRetryMaxMs = 30000
	// futureTradeGraceMs is how far past the server-clock estimate a trade
	// timestamp may sit before it is treated as corrupt and dropped (see
	// OnData). Wide enough to absorb the estimate's uncertainty (±RTT/2, or a
	// plain unmeasured system clock); a legitimate trade is never minutes
	// ahead of the server's own clock.
	futureTradeGraceMs = 120_000
	// defaultReplayBufferCap bounds each scope's in-flight trade buffer. A
	// seed resolves in ~one round trip (the client timeout bounds the worst
	// case), so thousands of buffered trades means a pathological market or a
	// wedged fetch; past the cap the Apply falls back to the conservative
	// exclude-everything watermark instead of growing without bound.
	defaultReplayBufferCap = 8192
	// keepBars bounds each scope's retained series: synthesis only ever needs
	// the live bucket and its predecessor (the flat-fill anchor); the rest is
	// emitted and done. Kept a little deeper for debuggability.
	keepBars = 8
	// maxSeedRows caps any single seed fetch, matching the candles operation's
	// auto-paging ceiling (ops.CandlesMaxLimit; asserted equal by a test rather
	// than imported, to keep this package free of the ops dependency).
	maxSeedRows = 5000
)

// scopeKey identifies one symbol×interval series.
type scopeKey struct{ symbol, interval string }

// scope is the per-symbol×interval state.
type scope struct {
	key    scopeKey
	series Series

	// stale means the next trade snapshot for the symbol must kick a seed
	// (true at start, and again after a reconnect); staleReason names why.
	stale       bool
	staleReason string
	seeded      bool  // a seed landed; trades fold and Tick finalizes
	pending     bool  // a fetch is in flight
	requeued    bool  // a re-seed trigger arrived while one was in flight
	backoffMs   int64 // next retry delay after a failed seed
	retryAt     int64 // server-clock ms when a failed seed re-fetches (0 = none)
	lastFinal   int64 // newest finalized bucket start emitted (sizes re-seed windows)
	// dead means the symbol's trade subscription was rejected and dropped by
	// the server (SUBSCRIBE_FAILED), so this scope can never see trades again
	// this session. A dead scope neither folds, finalizes, nor seeds — a REST
	// seed without the live feed would keep emitting flat "no trades" bars
	// while blind to a market that is actually trading.
	dead bool

	// The in-flight delivery buffer (see the package doc's seed-ordering
	// section): kickSeen is the per-symbol trade-id watermark captured when
	// this scope's fetch was kicked — everything delivered before it was
	// executed before the request went out and is inside the fetched rows.
	// Trades delivered while the fetch is in flight are buffered (not folded)
	// and replayed onto the authoritative rows at Apply, so a trade the fetch
	// missed can never be lost. bufOverflow falls the Apply back to the
	// exclude-everything watermark when the buffer cap was exceeded.
	kickSeen    int64
	buf         []tradeRow
	bufOverflow bool
}

// SeedResult is one finished seed fetch, delivered on Results. Its fields are
// internal; the monitor loop only shuttles it from Results into Apply.
type SeedResult struct {
	key    scopeKey
	reason string // "initial" | "gap" | "reconnect" | "retry"
	bars   []Bar
	err    error
}

// NewSynth validates the configuration and builds the synthesizer.
func NewSynth(cfg Config) (*Synth, error) {
	if len(cfg.Symbols) == 0 || len(cfg.Intervals) == 0 {
		return nil, fmt.Errorf("candles: symbols and intervals are required")
	}
	for _, iv := range cfg.Intervals {
		if !ValidInterval(iv) {
			return nil, fmt.Errorf("candles: %q is not a candle interval", iv)
		}
	}
	if cfg.Fetch == nil || cfg.ServerNow == nil || cfg.Now == nil {
		return nil, fmt.Errorf("candles: Fetch, ServerNow, and Now are required")
	}
	if cfg.FinalizeGraceMs <= 0 {
		cfg.FinalizeGraceMs = defaultGraceMs
	}
	if cfg.RetryMinMs <= 0 {
		cfg.RetryMinMs = defaultRetryMinMs
	}
	if cfg.RetryMaxMs <= 0 {
		cfg.RetryMaxMs = defaultRetryMaxMs
	}
	if cfg.ReplayBufferCap <= 0 {
		cfg.ReplayBufferCap = defaultReplayBufferCap
	}
	s := &Synth{
		cfg:    cfg,
		sem:    make(chan struct{}, 4),
		scopes: map[scopeKey]*scope{},
		seen:   map[string]int64{},
	}
	for _, sym := range cfg.Symbols {
		for _, iv := range cfg.Intervals {
			k := scopeKey{sym, iv}
			if _, dup := s.scopes[k]; dup {
				continue
			}
			sc := &scope{key: k, stale: true, staleReason: "initial"}
			sc.series.ResetGapless(iv) // the emitted stream must have no grid holes
			s.scopes[k] = sc
			s.keys = append(s.keys, k)
		}
	}
	// The fetch-result channel is drained by the monitor loop; size it so every
	// scope can have one result queued plus slack, and no fetch goroutine ever
	// blocks longer than the loop's next Results read.
	s.out = make(chan SeedResult, len(s.keys)+4)
	return s, nil
}

// Start wires the context that bounds every fetch this synthesizer ever runs.
// Seeds are not kicked here — the trade subscription's snapshot kicks them
// (see the package doc's seed-ordering rationale).
func (s *Synth) Start(ctx context.Context) { s.ctx = ctx }

// Results delivers finished seed fetches; the monitor loop selects on it and
// passes each value to Apply.
func (s *Synth) Results() <-chan SeedResult { return s.out }

// Apply folds one seed result into its scope: on success the authoritative
// rows overwrite/extend the series and the candle lines are emitted (closed
// bars final, the live bucket not); on failure a BACKFILL_FAILED notice is
// raised and a retry is scheduled with exponential backoff.
func (s *Synth) Apply(res SeedResult) []stream.Event {
	sc := s.scopes[res.key]
	if sc == nil {
		return nil
	}
	sc.pending = false
	// Take ownership of the in-flight buffer whatever path this Apply takes:
	// on failure/discard/empty it is stale (the next kick re-captures its own
	// watermark and buffer); on success it is replayed below.
	buf, overflow := sc.buf, sc.bufOverflow
	sc.buf, sc.bufOverflow = nil, false
	if sc.dead {
		// The trade subscription died while this fetch was in flight. The
		// result is discarded, but the BACKFILL_START it answers must still be
		// paired with a DONE — the notice contract guarantees the pairing, and
		// consumers (the --stateful store's backfill counter) balance on it.
		return []stream.Event{s.notice(stream.BackfillDone, stream.LevelInfo,
			fmt.Sprintf("candle seed for %s@%s discarded — the channel died while it was in flight", sc.key.symbol, sc.key.interval),
			map[string]any{"endpoint": "public", "channel": Channel, "symbol": sc.key.symbol,
				"interval": sc.key.interval, "reason": res.reason, "discarded": true})}
	}
	if res.err != nil {
		// A retry is now scheduled, which supersedes any queued re-seed (the
		// retry fetch covers the same window).
		sc.requeued = false
		sc.backoffMs = clampBackoff(sc.backoffMs*2, s.cfg.RetryMinMs, s.cfg.RetryMaxMs)
		sc.retryAt = s.cfg.ServerNow() + sc.backoffMs
		s.log().Debug("candle seed failed", "symbol", sc.key.symbol, "interval", sc.key.interval,
			"reason", res.reason, "retryInMs", sc.backoffMs, "err", res.err.Error())
		// FAILED is the alarm; the paired DONE (failures:1, like the stream
		// layer's private recovery) still closes the START so pairing-dependent
		// consumers never see a dangling backfill.
		evs := []stream.Event{
			s.notice(stream.BackfillFailed, stream.LevelError,
				fmt.Sprintf("candle seed for %s@%s failed (%s); retrying in %dms", sc.key.symbol, sc.key.interval, res.reason, sc.backoffMs),
				map[string]any{"endpoint": "public", "channel": Channel, "symbol": sc.key.symbol,
					"interval": sc.key.interval, "reason": res.reason, "error": res.err.Error()}),
			s.notice(stream.BackfillDone, stream.LevelInfo,
				fmt.Sprintf("candle seed for %s@%s ended with a failure (%s); a retry is scheduled", sc.key.symbol, sc.key.interval, res.reason),
				map[string]any{"endpoint": "public", "channel": Channel, "symbol": sc.key.symbol,
					"interval": sc.key.interval, "reason": res.reason, "failures": 1, "complete": false}),
		}
		// An already-live scope keeps flowing through the retry window: fold
		// the trades buffered during the failed fetch (the series took no
		// in-flight folds, so normal fold semantics apply; the retry kick
		// re-captures its watermark AFTER these ids, so its rows — which
		// contain these trades — do not re-fold them). An unseeded scope has
		// no series to fold into; the retry's watermark covers its buffer.
		if sc.seeded {
			sort.Slice(buf, func(i, j int) bool { return buf[i].TradeID < buf[j].TradeID })
			evs = append(evs, s.foldRows(sc, buf, s.cfg.ServerNow())...)
		}
		return evs
	}
	sc.backoffMs, sc.retryAt = 0, 0
	done := s.notice(stream.BackfillDone, stream.LevelInfo,
		fmt.Sprintf("candle seed for %s@%s complete (%s, %d rows)", sc.key.symbol, sc.key.interval, res.reason, len(res.bars)),
		map[string]any{"endpoint": "public", "channel": Channel, "symbol": sc.key.symbol,
			"interval": sc.key.interval, "reason": res.reason, "rows": len(res.bars)})
	if len(res.bars) == 0 {
		// Nothing to anchor the grid on (a brand-new listing, or a server
		// hiccup shaped as an empty array). Retry at the backoff cap — the
		// first real trade will eventually mint history server-side. Like the
		// failure path, an already-live scope folds its buffer so it keeps
		// flowing through the retry window.
		sc.requeued = false
		sc.backoffMs = s.cfg.RetryMaxMs
		sc.retryAt = s.cfg.ServerNow() + sc.backoffMs
		evs := []stream.Event{done}
		if sc.seeded {
			sort.Slice(buf, func(i, j int) bool { return buf[i].TradeID < buf[j].TradeID })
			evs = append(evs, s.foldRows(sc, buf, s.cfg.ServerNow())...)
		}
		return evs
	}

	// Rebase to the KICK-time watermark and replay the in-flight buffer on top
	// of the authoritative rows (see the package doc's seed-ordering section):
	// everything delivered before the kick was executed before the request and
	// is inside the fetched rows; everything delivered during the flight
	// replays, so a trade the fetch missed can never be lost. On buffer
	// overflow, fall back to excluding everything delivered so far — the
	// conservative direction (a bounded undercount) rather than unbounded
	// memory.
	oldest := res.bars[0].Timestamp
	if overflow {
		sc.series.Seed(res.bars, s.seen[sc.key.symbol])
		s.log().Debug("candle seed replay buffer overflowed; applied with the exclude-all watermark",
			"symbol", sc.key.symbol, "interval", sc.key.interval)
	} else {
		sc.series.SeedRebase(res.bars, sc.kickSeen)
		sort.Slice(buf, func(i, j int) bool { return buf[i].TradeID < buf[j].TradeID })
		for _, r := range buf {
			sc.series.FoldTrade(r.TradeID, r.Price, r.Qty, r.Timestamp)
		}
	}
	sc.seeded = true

	evs := []stream.Event{done}
	evs = append(evs, s.emitFrom(sc, oldest, s.cfg.ServerNow())...)
	sc.series.TrimOldest(keepBars)
	if sc.requeued {
		// A re-seed trigger arrived while this fetch was in flight: emit what
		// landed (corrections supersede it under last-wins), then heal again.
		sc.requeued = false
		evs = append(evs, s.kickSeed(sc, "gap")...)
	}
	return evs
}

// OnData consumes one session data event. Only public trade events for a
// configured symbol do anything. A snapshot frame kicks any stale scope's seed
// (initial or post-reconnect; see the package doc); then each row folds into
// every seeded interval series of the symbol, and the resulting candle lines
// are emitted (finalized buckets first, then the updated live bucket).
func (s *Synth) OnData(d stream.Data) []stream.Event {
	if d.Channel != stream.ChannelTrade {
		return nil
	}
	rows := parseTradeRows(d)
	// A trade cannot come from the server's own future: a timestamp past the
	// server-clock estimate (plus a generous clock-uncertainty grace) is a
	// corrupt row, and folding it would relocate the live edge into the future
	// — where every subsequent REAL trade is "behind the edge" and the scope is
	// stranded until the next re-seed. Drop such rows entirely, watermark
	// included (their id is as untrusted as their timestamp).
	futureLimit := s.cfg.ServerNow() + futureTradeGraceMs
	kept := rows[:0]
	for _, r := range rows {
		if r.Timestamp > futureLimit {
			s.log().Debug("candle fold dropped future-dated trade",
				"symbol", d.Symbol, "tradeId", r.TradeID, "ts", r.Timestamp, "limit", futureLimit)
			continue
		}
		kept = append(kept, r)
	}
	rows = kept
	// Fold oldest-first so the id high-water mark never skips a row of the
	// same batch; advance the per-symbol watermark BEFORE any seed kicks below,
	// so a kick's captured watermark covers this frame's own rows (a snapshot's
	// rows were executed before the fetch request and are inside its response).
	if len(rows) > 0 {
		sort.Slice(rows, func(i, j int) bool { return rows[i].TradeID < rows[j].TradeID })
		if max := rows[len(rows)-1].TradeID; max > s.seen[d.Symbol] {
			s.seen[d.Symbol] = max
		}
	}

	var evs []stream.Event
	if d.Origin == stream.OriginSnapshot {
		for _, k := range s.keys {
			if k.symbol != d.Symbol {
				continue
			}
			if sc := s.scopes[k]; sc.stale {
				evs = append(evs, s.kickSeed(sc, sc.staleReason)...)
			}
		}
	}
	if len(rows) == 0 {
		return evs
	}

	for _, k := range s.keys {
		if k.symbol != d.Symbol {
			continue
		}
		sc := s.scopes[k]
		if sc.dead {
			continue
		}
		if sc.pending {
			// A seed is in flight: buffer instead of folding. Folding now would
			// leave this scope's series holding contributions the Apply merge
			// cannot distinguish from the fetched rows (double-count); the
			// buffered rows replay onto the authoritative base at Apply, so
			// nothing the fetch missed is lost. Live output pauses for the
			// fetch's round trip — a seed is already a correction moment.
			sc.bufferRows(rows, s.cfg.ReplayBufferCap)
			continue
		}
		if !sc.seeded {
			continue // waiting on a retry; the retry's watermark covers these
		}
		evs = append(evs, s.foldRows(sc, rows, d.ServerTime)...)
	}
	return evs
}

// foldRows folds parsed trade rows into a seeded scope and emits the
// resulting candle lines: finalized buckets first, then the updated live
// bucket. Nothing is emitted when every row deduped away.
func (s *Synth) foldRows(sc *scope, rows []tradeRow, serverTime int64) []stream.Event {
	var finalized []Candle
	updated := false
	for _, r := range rows {
		fin, up := sc.series.FoldTrade(r.TradeID, r.Price, r.Qty, r.Timestamp)
		finalized = append(finalized, fin...)
		updated = updated || up
	}
	if !updated {
		return nil
	}
	evs := s.emitBars(sc, finalized, serverTime, true)
	if live, ok := sc.series.Live(); ok {
		evs = append(evs, s.emitBars(sc, []Candle{live}, serverTime, false)...)
	}
	sc.series.TrimOldest(keepBars)
	return evs
}

// bufferRows appends one frame's rows to the in-flight buffer, tripping the
// overflow fallback rather than growing without bound.
func (sc *scope) bufferRows(rows []tradeRow, cap int) {
	if sc.bufOverflow {
		return
	}
	if len(sc.buf)+len(rows) > cap {
		sc.buf, sc.bufOverflow = nil, true
		return
	}
	sc.buf = append(sc.buf, rows...)
}

// OnNotice consumes one session notice. It tracks the public connection state
// (finalization gates on it), marks scopes stale on a public reconnect (the
// resubscribe snapshot then kicks the re-seed), and re-seeds immediately on a
// trade DATA_GAP or a completed trade gap patch — events whose rows can land
// behind an already-rolled live edge, where folding must not touch history.
// The notice itself is not re-emitted — the monitor already delivers every
// session notice.
func (s *Synth) OnNotice(n stream.Notice) []stream.Event {
	endpoint, _ := n.Details["endpoint"].(string)
	switch n.Code {
	case stream.Disconnected:
		if endpoint == "public" {
			s.up = false
		}
	case stream.Connected:
		if endpoint != "public" {
			return nil
		}
		s.up = true
		if s.upOnce {
			// Trades during the downtime may already sit behind the live edge;
			// the resubscribe snapshot (which always follows) kicks the fetch,
			// so the snapshot's rows sit under the seed watermark.
			for _, k := range s.keys {
				s.scopes[k].stale, s.scopes[k].staleReason = true, "reconnect"
			}
		}
		s.upOnce = true
	case stream.DataGap, stream.BackfillDone:
		if n.Code == stream.BackfillDone {
			if reason, _ := n.Details["reason"].(string); reason != "gap" {
				return nil
			}
		}
		if ch, _ := n.Details["channel"].(string); ch != stream.ChannelTrade {
			return nil
		}
		sym, _ := n.Details["symbol"].(string)
		return s.reseedSymbolScopes(sym, "gap")
	case stream.SubscribeFailed:
		// The server rejected (and dropped) a trade subscription: every candle
		// scope it fed is dead for the session — no snapshot will ever kick its
		// seed, and finalizing from REST alone would emit flat "no trades" bars
		// while blind. Mark the scopes dead and say so on the channel the user
		// actually asked for.
		if ch, _ := n.Details["channel"].(string); ch != stream.ChannelTrade {
			return nil
		}
		if method, _ := n.Details["method"].(string); method == "unsubscribe" {
			return nil // a benign unsubscribe reject drops nothing
		}
		syms, _ := n.Details["symbols"].([]string)
		return s.killSymbolScopes(syms, n.Details["code"], n.Details["message"])
	}
	return nil
}

// killSymbolScopes marks every candle scope of the listed symbols (nil = all)
// dead and emits one candle-scoped SUBSCRIBE_FAILED per affected symbol, so
// the user learns the channel THEY requested is gone — the session's own
// notice names only the trade channel, which they may never have asked for.
func (s *Synth) killSymbolScopes(symbols []string, code, message any) []stream.Event {
	named := map[string]bool{}
	for _, sym := range symbols {
		named[sym] = true
	}
	var evs []stream.Event
	noticed := map[string]bool{}
	for _, k := range s.keys {
		if len(named) > 0 && !named[k.symbol] {
			continue
		}
		sc := s.scopes[k]
		if sc.dead {
			noticed[k.symbol] = true // already reported for this symbol
			continue
		}
		sc.dead = true
		sc.stale, sc.seeded, sc.retryAt, sc.requeued = false, false, 0, false
		if noticed[k.symbol] {
			continue
		}
		noticed[k.symbol] = true
		evs = append(evs, s.notice(stream.SubscribeFailed, stream.LevelError,
			fmt.Sprintf("candle channel for %s is unavailable — its trade subscription was rejected and dropped", k.symbol),
			map[string]any{"endpoint": "public", "channel": Channel, "symbol": k.symbol,
				"intervals": s.cfg.Intervals, "code": code, "message": message}))
	}
	return evs
}

// Tick advances the synthesizer's clocks: due seed retries re-fetch, and — on
// the server-clock estimate, minus the grace — quiet buckets finalize as flat
// zero-volume bars so a bucket closes on time without a trade to roll it.
// Call it about once a second.
func (s *Synth) Tick() []stream.Event {
	now := s.cfg.ServerNow()
	var evs []stream.Event
	for _, k := range s.keys {
		sc := s.scopes[k]
		if sc.retryAt > 0 && now >= sc.retryAt && !sc.pending {
			sc.retryAt = 0
			evs = append(evs, s.kickSeed(sc, "retry")...)
		}
		if !sc.seeded || !s.up || sc.pending {
			// Never finalize blind (down connection) or mid-revision (a seed in
			// flight is about to rebase this series; its buffered trades have
			// not folded yet, so sealing now could close a bucket short).
			continue
		}
		finalized := sc.series.RollTo(now - s.cfg.FinalizeGraceMs)
		if len(finalized) == 0 {
			continue
		}
		evs = append(evs, s.emitBars(sc, finalized, now, true)...)
		if live, ok := sc.series.Live(); ok {
			evs = append(evs, s.emitBars(sc, []Candle{live}, now, false)...)
		}
		sc.series.TrimOldest(keepBars)
	}
	return evs
}

// kickSeed starts one scope's fetch goroutine (bounded by the semaphore) and
// returns its BACKFILL_START notice. A scope with a fetch already in flight is
// queued for one follow-up instead of double-fetching.
func (s *Synth) kickSeed(sc *scope, reason string) []stream.Event {
	if sc.dead {
		return nil // no live feed to extend a seed — see scope.dead
	}
	sc.stale = false
	if sc.pending {
		sc.requeued = true
		return nil
	}
	sc.pending = true
	sc.retryAt = 0
	// The replay boundary: everything delivered up to now was executed before
	// this request goes out, so it is inside the response; everything after is
	// buffered and replayed at Apply.
	sc.kickSeen = s.seen[sc.key.symbol]
	sc.buf, sc.bufOverflow = nil, false
	limit := s.seedLimit(sc)
	key, ctx := sc.key, s.ctx
	go func() {
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return // the monitor is shutting down; the loop no longer reads Results
		}
		defer func() { <-s.sem }()
		bars, err := s.cfg.Fetch(ctx, key.symbol, key.interval, limit, 0)
		if ctx.Err() != nil {
			return
		}
		select {
		case s.out <- SeedResult{key: key, reason: reason, bars: bars, err: err}:
		case <-ctx.Done():
		}
	}()
	return []stream.Event{s.notice(stream.BackfillStart, stream.LevelInfo,
		fmt.Sprintf("candle seed for %s@%s started (%s)", sc.key.symbol, sc.key.interval, reason),
		map[string]any{"endpoint": "public", "channel": Channel, "symbol": sc.key.symbol,
			"interval": sc.key.interval, "reason": reason, "limit": limit})}
}

// seedLimit sizes a fetch: the initial seed wants the history depth plus the
// live bucket; a re-seed wants everything since the last finalized bucket plus
// slack, so the whole potentially-stale span is overwritten. Before anything
// has finalized (lastFinal 0), the live bucket anchors the window instead —
// a reconnect that lands within the very first bucket must still cover every
// boundary the quiet downtime crossed, or those closed bars are never emitted.
func (s *Synth) seedLimit(sc *scope) int {
	limit := s.cfg.History + 1
	anchor := sc.lastFinal
	if anchor == 0 {
		if live, ok := sc.series.Live(); ok {
			anchor = live.Time
		}
	}
	if anchor > 0 {
		if p := sc.series.PeriodMs(); p > 0 {
			span := int((s.cfg.ServerNow()-anchor)/p) + 2
			if span > limit {
				limit = span
			}
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxSeedRows {
		limit = maxSeedRows
	}
	return limit
}

// reseedSymbolScopes kicks a re-seed for every scope of sym ("" = all) and
// returns the BACKFILL_START notices.
func (s *Synth) reseedSymbolScopes(sym, reason string) []stream.Event {
	var evs []stream.Event
	for _, k := range s.keys {
		if sym != "" && k.symbol != sym {
			continue
		}
		evs = append(evs, s.kickSeed(s.scopes[k], reason)...)
	}
	return evs
}

// emitFrom emits the scope's series from bucket start `oldest` on: every
// closed bucket final, the live bucket not. Grid holes inside the emitted span
// (REST rows a quiet market skipped) are filled flat at the previous close so
// the emitted series is gapless.
func (s *Synth) emitFrom(sc *scope, oldest, serverTime int64) []stream.Event {
	all := sc.series.Candles()
	from := 0
	for i, c := range all {
		if c.Time >= oldest {
			from = i
			break
		}
	}
	span := fillEmitGaps(all[from:], sc.series.PeriodMs())
	if len(span) == 0 {
		return nil
	}
	evs := s.emitBars(sc, span[:len(span)-1], serverTime, true)
	return append(evs, s.emitBars(sc, span[len(span)-1:], serverTime, false)...)
}

// emitBars renders candles as candle-channel Data events. Final buckets also
// advance the scope's lastFinal watermark.
func (s *Synth) emitBars(sc *scope, cs []Candle, serverTime int64, final bool) []stream.Event {
	evs := make([]stream.Event, 0, len(cs))
	for _, c := range cs {
		if final && c.Time > sc.lastFinal {
			sc.lastFinal = c.Time
		}
		payload, err := json.Marshal(candleLine{
			Interval: sc.key.interval, Timestamp: c.Time,
			Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume,
			Final: final,
		})
		if err != nil {
			continue // unreachable: every field is marshalable
		}
		evs = append(evs, stream.Data{
			Channel: Channel, Symbol: sc.key.symbol, Origin: stream.OriginDerived,
			ServerTime: serverTime, Payload: payload,
		})
	}
	return evs
}

// candleLine is the candle channel's payload — a stable CLI-owned contract
// (REST candle row fields plus interval and final). Extend additively, never
// rename.
type candleLine struct {
	Interval  string `json:"interval"`
	Timestamp int64  `json:"timestamp"`
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	Volume    string `json:"volume"`
	Final     bool   `json:"final"`
}

// notice builds one synthesizer notice, stamped from the local clock like
// every stream notice.
func (s *Synth) notice(code stream.NoticeCode, level stream.Level, msg string, details map[string]any) stream.Notice {
	return stream.Notice{Code: code, Level: level, Message: msg, Details: details, Time: s.cfg.Now()}
}

func (s *Synth) log() *slog.Logger { return logging.Or(s.cfg.Log) }

// tradeRow is the subset of a public trade row the fold needs.
type tradeRow struct {
	TradeID   int64  `json:"tradeId"`
	Timestamp int64  `json:"timestamp"`
	Price     string `json:"price"`
	Qty       string `json:"qty"`
}

// parseTradeRows extracts trade rows from either payload shape: a WebSocket
// frame ({"data":[rows]}) for realtime/snapshot origins, or the bare REST row
// array for backfill.
func parseTradeRows(d stream.Data) []tradeRow {
	var raw []json.RawMessage
	if d.Origin == stream.OriginBackfill {
		if json.Unmarshal(d.Payload, &raw) != nil {
			return nil
		}
	} else {
		var frame struct {
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(d.Payload, &frame) != nil {
			return nil
		}
		raw = frame.Data
	}
	rows := make([]tradeRow, 0, len(raw))
	for _, r := range raw {
		var t tradeRow
		if json.Unmarshal(r, &t) == nil && t.TradeID > 0 {
			rows = append(rows, t)
		}
	}
	return rows
}

// fillEmitGaps returns cs with any interior grid holes filled flat at the
// previous close (volume "0"), so an emitted history span is gapless like the
// live-derived series. Holes wider than maxSynthOnJump buckets are left as-is.
func fillEmitGaps(cs []Candle, periodMs int64) []Candle {
	if periodMs <= 0 || len(cs) < 2 {
		return cs
	}
	out := make([]Candle, 0, len(cs))
	out = append(out, cs[0])
	for _, c := range cs[1:] {
		prev := out[len(out)-1]
		gaps := (c.Time - prev.Time) / periodMs
		if gaps > 1 && gaps-1 <= maxSynthOnJump {
			for t := prev.Time + periodMs; t < c.Time; t += periodMs {
				out = append(out, flatCandle(t, out[len(out)-1]))
			}
		}
		out = append(out, c)
	}
	return out
}

// clampBackoff doubles into [min, max].
func clampBackoff(v, min, max int64) int64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
