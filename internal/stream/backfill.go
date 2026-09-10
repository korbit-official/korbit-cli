// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/ops"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
)

// historyWindowMs is the documented retention for /v2/allOrders and
// /v2/myTrades (36 hours per the public API docs). A disconnection longer
// than this is treated as unrecoverable from REST. Being conservative here is
// safe: a window that is really longer only means BACKFILL_NOT_VIABLE fires
// earlier than strictly necessary, never silent loss. It mirrors the L2 layer's
// ops.HistoryWindowMs (the window-walk lives there) so the two never drift.
const historyWindowMs = ops.HistoryWindowMs

// HistoryWindowMs re-exports the documented /v2/allOrders + /v2/myTrades
// retention (36h) for any stream consumer that references it. The canonical
// value and the window-walk live in internal/ops.
const HistoryWindowMs = ops.HistoryWindowMs

// backfillOverlapMs is how far before the disconnection a gap query starts.
// The private stream is lossless while connected, so anything before the drop
// was delivered; the overlap only absorbs residual slop between the
// server-clock-anchored window start (see gapStart in backfillPrivate) and the
// server's row timestamps — offset-measurement uncertainty, or the whole skew
// when no offset has been measured. Duplicates it causes are removed by the
// per-row dedupe (myTrade) or are idempotent re-states (myOrder rows).
const backfillOverlapMs = 60_000

// historyPageLimit is the server's maximum `limit` for /v2/openOrders snapshots
// (a saturated snapshot may be truncated). The history window-walk's own page
// limit lives in internal/ops with WalkHistory.
const historyPageLimit = 1000

// tradesPageLimit is the server's maximum `limit` for /v2/trades (the public
// recent-trades buffer). Both the gap patch and the history seed page through
// this cap.
const tradesPageLimit = 500

// restClient runs the session's REST recovery calls (backfill + public-trade gap
// patching) through the session's one apiclient.Client (the typed rawapi layer over
// it). That Client signs with the shared clock, handles EXCEED_TIME_WINDOW
// internally (its own Resync), and records each call through its own per-call
// recorder — the journaling policy (callrec) exempts the stream-backfill surface,
// so reads are consulted but never journaled. The session's backfill reads are
// all GETs (always safe to retry).
type restClient struct {
	raw      *rawapi.Client // typed endpoint layer over the session's Client
	budgetMs int            // default retry-sleep budget for idempotent calls
	lg       *slog.Logger   // resolved from the session (never nil)
}

// ctx returns the session's run context (canceled on stop) for backfill REST
// reads, falling back to context.Background when called outside a run.
func (s *Session) ctx() context.Context {
	if p := s.runCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// pol is the policy every backfill read runs under: idempotent (all reads are
// GETs, always safe to retry) within the session's backfill sleep budget, so
// the bounded retry ladder (network/5xx backoff, 429 Retry-After,
// EXCEED_TIME_WINDOW correction handled inside the Client) applies.
func (r *restClient) pol() apiclient.Policy {
	return apiclient.Policy{Idempotent: true, BudgetMs: r.budgetMs}
}

// privatePlan is one connect's private recovery window, captured at connect
// so a self-heal retry (retryFailedBackfill) re-runs the SAME window.
// gapStart is an absolute server-time anchor: recomputing it at retry time
// would slide the window start forward, silently skipping the head of the
// gap the failed pass was supposed to recover.
type privatePlan struct {
	reconnect  bool
	gapStart   int64
	histViable bool
	gen        int64
}

// unitOutcome is the result of running one retryUnit.
type unitOutcome int

const (
	unitOK    unitOutcome = iota // succeeded (or stale/no-op)
	unitRetry                    // failed transiently — re-run on the next self-heal pass
	unitFatal                    // definitively rejected — noticed, never re-run
)

// retryUnit is one leaf REST call of a private recovery pass — one
// /v2/balance per account, one /v2/openOrders snapshot or /v2/allOrders or
// /v2/myTrades gap walk per symbol+account — re-runnable in isolation. The
// self-heal retry (retryFailedBackfill) re-runs EXACTLY the units a pass left
// transiently failed, so a call that succeeded is never replayed: a broad
// fan-out with one persistently rate-limited call retries only that call,
// instead of feeding the limit that caused the failure. run captures its
// plan (generation, gap window) by value at construction, so a re-run
// recovers the SAME window the failed pass owned; the label fields feed the
// retry notices.
type retryUnit struct {
	channel string
	source  string
	symbol  string // "" when the call is not per-symbol (balances)
	seq     *int   // pinned accountSeq; nil = the server's default account
	run     func() unitOutcome
	stale   func() bool // optional: true = the unit is stale; dropped unrun
}

// unitCollector accumulates one pass's failure count and its transiently-
// failed units. Safe for concurrent use (the open-order snapshot fan-out).
type unitCollector struct {
	mu       sync.Mutex
	failures int
	units    []retryUnit
}

// runNow executes a freshly-built unit as part of the current pass and
// records its outcome: a transient failure enqueues the unit for the
// self-heal retry; a fatal one only counts (the in-call ladder already
// refused to retry it, and the next (re)connect's full pass re-attempts
// everything anyway).
func (c *unitCollector) runNow(u retryUnit) {
	out := u.run()
	if out == unitOK {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if out == unitRetry {
		c.units = append(c.units, u)
	}
}

// backfillPrivate is the REST recovery pass for the private endpoint, run after
// every connect. Initial connect establishes the baseline (balances, open
// orders); a reconnect additionally patches the gap (order/fill activity
// since the drop). Each leaf call is independent: one failing call emits
// BACKFILL_FAILED for that call and the others still run. It returns the
// pass's plan and the leaf calls left transiently failed, for the caller's
// self-heal retry loop (retryFailedBackfill).
func (s *Session) backfillPrivate(reconnect bool, downtimeMs int64, gen int64) (privatePlan, []retryUnit) {
	reason := "initial"
	if reconnect {
		reason = "reconnect"
	}
	channels := s.privateChannels()
	if s.cfg.DisableBackfill {
		s.noticef(BackfillDisabled, LevelWarn,
			map[string]any{"endpoint": "private", "reason": reason, "channels": channels},
			"backfill is disabled: private channels are NOT backfilled (%s) — order/balance state may be incomplete", reason)
		return privatePlan{}, nil
	}

	// Snapshot-only mode backfills balances + the open-order snapshot but skips the
	// per-symbol order/fill history walks. Announce it so the "up to date or
	// TOLD" contract holds: on a reconnect, transitions during the gap are not
	// recovered (Info on the initial connect — there is no gap yet — Warn on a
	// reconnect, where the skipped recovery is a real, accepted loss).
	if s.cfg.BackfillSnapshotOnly {
		level := LevelInfo
		if reconnect {
			level = LevelWarn
		}
		s.noticef(BackfillSnapshotOnly, level,
			map[string]any{"endpoint": "private", "reason": reason, "channels": channels},
			"snapshot-only backfill (%s): backfilling balances + open orders only — order/fill history during a disconnect is NOT recovered", reason)
	}

	// History-window viability for the gap queries. The snapshots
	// (balances, open orders) are always viable. gapStart anchors on the
	// server-clock estimate — startTime is a filter the server evaluates
	// against its own row timestamps — while downtimeMs is a local duration
	// (skew-free). With a measured offset this removes local clock skew from
	// the window entirely, leaving the overlap to absorb only measurement
	// slop; anchoring on the bare local clock instead would silently start the
	// walk late (missing gap rows) whenever the local clock ran more than the
	// overlap ahead of the server.
	gapStart := s.serverNow() - downtimeMs - backfillOverlapMs
	histViable := downtimeMs+backfillOverlapMs < historyWindowMs

	// The window math the BACKFILL_START notice summarizes: on a reconnect the gap
	// walk starts at gapStart (now - downtime - overlap) and runs to now, viable
	// only within the 36h history window.
	if reconnect {
		s.log().Debug("backfill window",
			"endpoint", "private", "downtimeMs", downtimeMs,
			"gapStartMs", gapStart, "historyViable", histViable, "gen", gen)
	}

	plan := privatePlan{reconnect: reconnect, gapStart: gapStart, histViable: histViable, gen: gen}
	return plan, s.backfillPass(plan, reason)
}

// backfillPass runs the per-connect private recovery pass over every
// subscribed private channel, bracketed by its own BACKFILL_START/
// BACKFILL_DONE pair, and returns the leaf calls that failed in a way worth
// re-running (non-fatal per apiclient.Classify) as retry units. Re-running a
// leaf call is safe: snapshots re-baseline idempotently, and history rows are
// deduped on both sides (myTradeSeen here, id dedupe in stream/state) — the
// duplicates-over-loss rule.
func (s *Session) backfillPass(plan privatePlan, reason string) []retryUnit {
	channels := s.privateChannels()
	s.noticef(BackfillStart, LevelInfo,
		map[string]any{"endpoint": "private", "reason": reason, "channels": channels},
		"backfilling private channels from REST (%s)", reason)

	c := &unitCollector{}
	for _, sub := range s.cfg.Subscriptions {
		switch sub.Channel {
		case ChannelMyAsset:
			s.backfillBalances(sub, plan.gen, c)
		case ChannelMyOrder:
			// The snapshot scope set is read at pass time; a failed snapshot's
			// retry unit self-drops (stale) once its scope leaves the
			// LazyOpenOrders tracked set. The gap walk below covers the static
			// sub.Symbols.
			s.backfillOrderSnapshots(s.orderSnapshotScopes(sub), plan.gen, c)
			if plan.reconnect && !s.cfg.BackfillSnapshotOnly {
				s.backfillOrderGap(sub, plan.histViable, plan.gapStart, plan.gen, c)
			}
		case ChannelMyTrade:
			if s.cfg.BackfillSnapshotOnly {
				continue // snapshot-only: no fill-history walk (myTrade has no snapshot)
			}
			if !plan.reconnect {
				continue // fills are events: there is no baseline to backfill
			}
			s.backfillMyTrades(sub, plan.histViable, plan.gapStart, plan.gen, c)
		}
	}

	s.noticef(BackfillDone, LevelInfo,
		map[string]any{"endpoint": "private", "reason": reason, "channels": channels, "failures": c.failures},
		"private backfill finished (%s, %d failures)", reason, c.failures)
	return c.units
}

// unitChannels is the unique channels of the units in first-appearance order —
// the channel-level `channels` detail retry notices keep for consumers that
// key on it (the `units` detail carries the precise per-call view).
func unitChannels(units []retryUnit) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(units))
	for _, u := range units {
		if !seen[u.channel] {
			seen[u.channel] = true
			out = append(out, u.channel)
		}
	}
	return out
}

// unitNoticeDetails is the `units` detail on a retry pass's notices: exactly
// which leaf calls the pass re-runs.
func unitNoticeDetails(units []retryUnit) []map[string]any {
	out := make([]map[string]any, 0, len(units))
	for _, u := range units {
		d := map[string]any{"channel": u.channel, "source": u.source}
		if u.symbol != "" {
			d["symbol"] = u.symbol
		}
		if u.seq != nil {
			d["accountSeq"] = *u.seq
		}
		out = append(out, d)
	}
	return out
}

// rerunUnits is one self-heal pass: it re-runs exactly the leaf calls a prior
// pass left transiently failed — never a call that already succeeded — and
// returns the ones that failed transiently again. Stale units (a
// LazyOpenOrders snapshot whose symbol was untracked between attempts) are
// dropped before the pass; when nothing is left the pass emits no notices.
// Deliberately sequential, unlike the per-connect snapshot fan-out: the list
// is small by construction and the most likely cause of the failures is rate
// limiting, so the retry stays gentle instead of re-fanning out.
func (s *Session) rerunUnits(units []retryUnit, attempt int) []retryUnit {
	live := make([]retryUnit, 0, len(units))
	for _, u := range units {
		if u.stale != nil && u.stale() {
			continue
		}
		live = append(live, u)
	}
	if len(live) == 0 {
		return nil
	}
	channels := unitChannels(live)
	s.noticef(BackfillStart, LevelInfo,
		map[string]any{"endpoint": "private", "reason": "retry", "channels": channels, "units": unitNoticeDetails(live), "attempt": attempt},
		"backfilling private channels from REST (retry)")
	c := &unitCollector{}
	for _, u := range live {
		c.runNow(u)
	}
	s.noticef(BackfillDone, LevelInfo,
		map[string]any{"endpoint": "private", "reason": "retry", "channels": channels, "failures": c.failures, "attempt": attempt},
		"private backfill finished (retry, %d failures)", c.failures)
	return c.units
}

// retryFailedBackfill self-heals a private backfill (a per-connect pass, or
// an on-demand snapshot batch — backfillOpenOrdersBatch) that left leaf
// calls failed after each call's bounded in-call retry ladder: while the
// SAME private connection stays up and current, exactly those calls are
// re-run on a jittered exponential backoff (Tunables.BackfillRetryMinMs
// doubling to BackfillRetryMaxMs), each attempt a fresh BACKFILL_START/DONE
// pass with reason "retry", until every call succeeds. A call that succeeded
// is never replayed — the pass shrinks to what is still failing, so a retry
// cannot feed the rate limit that caused the failure. Without this loop a
// REST-side outage that outlives one call's budget would leave the stream
// degraded (balances/orders never ready, gap unrecovered) until the next
// reconnect — which a healthy connection may not have for hours.
//
// The loop exits when recovery ownership moves on: the session stopping, or
// the spawning connection superseded or down (the next connect's own full
// pass covers everything, and a REST snapshot succeeding while the WS is
// down must not present re-baselined state as live). Definitively-rejected
// calls (ClassFatal — an auth/permission/config-class 4xx) are not re-run:
// the in-call ladder already refused to retry them, and repeating a provably
// non-transient call forever is noise, not healing; they are re-attempted on
// the next (re)connect as before. Like the reconnect ladder, the loop is
// bounded by rate (the backoff cap), not by count.
func (s *Session) retryFailedBackfill(plan privatePlan, units []retryUnit) {
	backoffMs := s.tun.BackfillRetryMinMs
	for attempt := 1; len(units) > 0; attempt++ {
		s.log().Debug("backfill retry scheduled",
			"attempt", attempt, "backoffMs", backoffMs, "units", len(units), "gen", plan.gen)
		if !s.sleepStop(jitterMs(backoffMs)) {
			return
		}
		backoffMs = min(backoffMs*2, s.tun.BackfillRetryMaxMs)
		if s.privateGen.Load() != plan.gen || !s.privateUp.Load() {
			return
		}
		s.backfillMu.Lock()
		if s.privateGen.Load() != plan.gen {
			// A reconnect raced the sleep; its pass owns recovery now.
			s.backfillMu.Unlock()
			return
		}
		units = s.rerunUnits(units, attempt)
		s.backfillMu.Unlock()
	}
}

// fetchHistory pages a history endpoint through the typed REST client; the
// window-walk algorithm lives in ops.WalkHistory. The per-page get closure
// builds the typed request from the symbol/accountSeq base and the walk's
// paging params (limit, startTime, optional endTime), so every endpoint's wire
// facts (path, param names) come from internal/rawapi rather than being
// hand-built here.
func (s *Session) fetchHistory(get func(req historyPageRequest) (json.RawMessage, error), symbol string, accountSeq *int, startMs int64, tsField string, emitPage func(rows []json.RawMessage)) (complete bool, err error) {
	return ops.WalkHistory(func(params []apiclient.KV) (json.RawMessage, error) {
		return get(historyPageRequestFrom(symbol, accountSeq, params))
	}, nil, startMs, 0, tsField, emitPage)
}

// historyPageRequest is one history page's typed parameters, parsed from the
// window-walk's KV params plus the per-call symbol/accountSeq.
type historyPageRequest struct {
	Symbol     rawapi.Symbol
	Limit      *int
	StartTime  *int
	EndTime    *int
	AccountSeq *int
}

// historyPageRequestFrom parses the walk-supplied paging params (limit,
// startTime, optional endTime) into a typed page request, carrying the per-call
// symbol and accountSeq through.
func historyPageRequestFrom(symbol string, accountSeq *int, params []apiclient.KV) historyPageRequest {
	req := historyPageRequest{Symbol: rawapi.Symbol(symbol), AccountSeq: accountSeq}
	for _, kv := range params {
		n, err := strconv.Atoi(kv.Value)
		if err != nil {
			continue
		}
		v := n
		switch kv.Key {
		case "limit":
			req.Limit = &v
		case "startTime":
			req.StartTime = &v
		case "endTime":
			req.EndTime = &v
		}
	}
	return req
}

// accountSeqValues expands a subscription's AccountSeqs into per-call accountSeq
// values: one call per account seq, or one call with no accountSeq (the
// server's default account) when the subscription didn't pin any. A nil entry
// means the default account.
func accountSeqValues(sub Subscription) []*int {
	if len(sub.AccountSeqs) == 0 {
		return []*int{nil}
	}
	out := make([]*int, 0, len(sub.AccountSeqs))
	for _, seq := range sub.AccountSeqs {
		v := seq
		out = append(out, &v)
	}
	return out
}

func (s *Session) backfillBalances(sub Subscription, gen int64, c *unitCollector) {
	for _, seq := range accountSeqValues(sub) {
		c.runNow(retryUnit{channel: ChannelMyAsset, source: "/v2/balance", seq: seq, run: func() unitOutcome {
			asOf := s.serverNow()
			_, data, _, err := s.rest.raw.Balance(s.ctx(), rawapi.BalanceRequest{AccountSeq: seq}, s.rest.pol())
			if err != nil {
				return s.failureOutcome(ChannelMyAsset, "", "/v2/balance", err)
			}
			s.emitData(Data{Channel: ChannelMyAsset, Origin: OriginBackfill, Source: "/v2/balance", ServerTime: asOf, Payload: data, AccountSeq: seq, PrivateEpoch: gen})
			return unitOK
		}})
	}
}

// orderSnapshotScopes is the {account, symbol} scope set to snapshot for the
// open-order channel: the dynamic tracked set in LazyOpenOrders mode, else
// every subscribed myOrder symbol × subscribed account (the non-lazy default,
// e.g. monitor).
func (s *Session) orderSnapshotScopes(sub Subscription) []OrderScope {
	if !s.cfg.LazyOpenOrders {
		out := make([]OrderScope, 0, len(sub.Symbols)*len(s.orderSeqs))
		for _, sym := range sub.Symbols {
			for _, seq := range s.orderSeqs {
				out = append(out, OrderScope{AccountSeq: seqVal(seq), Symbol: sym})
			}
		}
		return out
	}
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	out := make([]OrderScope, 0, len(s.trackedOrders))
	for sc := range s.trackedOrders {
		out = append(out, sc)
	}
	return out
}

// backfillOrderSnapshots fetches the authoritative open-order snapshot for each
// scope concurrently (bounded), recording outcomes into c. The per-scope
// snapshots are independent — each reconciles only its own {account, symbol} —
// so fanning them out turns an N-scope REST round-trip chain into one batched
// wait.
func (s *Session) backfillOrderSnapshots(scopes []OrderScope, gen int64, c *unitCollector) {
	if len(scopes) == 0 {
		return
	}
	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, scope := range scopes {
		wg.Add(1)
		sem <- struct{}{}
		go func(scope OrderScope) {
			defer wg.Done()
			defer func() { <-sem }()
			s.backfillOrderSnapshot(scope, gen, c)
		}(scope)
	}
	wg.Wait()
}

// backfillOrderSnapshot emits the authoritative current open-order set for one
// scope (GET /v2/openOrders for one symbol under one account). Consumers can use
// it as their baseline and, when they track connection epochs like stream/state,
// infer that an order known from a PRIOR private connection and absent from the
// snapshot closed while disconnected. Do not read same-connection absence as a
// close: while the private feed is up, terminal order transitions are expected
// to arrive as live myOrder frames, and /v2/openOrders may be briefly
// stale/cache-backed. This is the snapshot half of the myOrder backfill: a plain
// GET, safe to run any time, shared by the per-connect pass and the on-demand
// BackfillOpenOrders. A success records the scope's baseline generation, which
// is what lets a same-generation re-track skip the fetch (SetTrackedOrderScopes).
func (s *Session) backfillOrderSnapshot(scope OrderScope, gen int64, c *unitCollector) {
	seq := scope.seqPtr()
	symbol := scope.Symbol
	c.runNow(retryUnit{
		channel: ChannelMyOrder, source: "/v2/openOrders", symbol: symbol, seq: seq,
		// Under LazyOpenOrders a scope untracked between attempts is stale:
		// its visible data must not be treated as authoritative until it is
		// tracked again, so the self-heal retry (per-connect and on-demand
		// alike) drops its unit. The scope's on-demand in-flight mark is
		// released in the SAME critical section as the drop decision, so a
		// re-track can never be lost to the loop's teardown: one landing
		// before this instant makes the unit non-stale (kept, heals under
		// the held mark), one landing after finds the mark free and fires a
		// fresh fetch. (Deleting a mark held by no one is a no-op.)
		stale: func() bool {
			if !s.cfg.LazyOpenOrders {
				return false
			}
			s.orderMu.Lock()
			defer s.orderMu.Unlock()
			if s.trackedOrders[scope] {
				return false
			}
			delete(s.inFlightOrders, scope)
			return true
		},
		run: func() unitOutcome {
			pageLimit := historyPageLimit
			asOf := s.serverNow()
			_, data, _, err := s.rest.raw.OrderOpen(s.ctx(), rawapi.OrderOpenRequest{Symbol: rawapi.Symbol(symbol), Limit: &pageLimit, AccountSeq: seq}, s.rest.pol())
			if err != nil {
				return s.failureOutcome(ChannelMyOrder, symbol, "/v2/openOrders", err)
			}
			// Record before emitting: anyone who has SEEN the snapshot event can
			// then rely on the memo being set (a skip decided between the two
			// would still be safe — the emitted event is already on its way —
			// but never the reverse, a memo claiming a snapshot that failed).
			s.recordOrderBaseline(scope, gen)
			s.emitData(Data{Channel: ChannelMyOrder, Symbol: symbol, Origin: OriginBackfill, Source: "/v2/openOrders", ServerTime: asOf, Payload: data, AccountSeq: seq, PrivateEpoch: gen})
			if n := rowCount(data); n >= historyPageLimit {
				s.noticef(DataGap, LevelWarn,
					map[string]any{"channel": ChannelMyOrder, "symbol": symbol, "source": "/v2/openOrders", "rows": n},
					"the open-order snapshot for %s filled the server's %d-row limit and may be truncated", symbol, historyPageLimit)
			}
			return unitOK
		},
	})
}

// recordOrderBaseline notes that scope's open-order snapshot succeeded in
// private generation gen — the memo behind SetTrackedOrderScopes's
// same-generation re-track skip. Never downgraded: a slow unit from a
// superseded connection landing after a newer generation's snapshot must not
// roll the memo back (its emitted data is likewise rejected by the store's
// gen guard).
func (s *Session) recordOrderBaseline(scope OrderScope, gen int64) {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	if gen > s.orderBaselined[scope] {
		s.orderBaselined[scope] = gen
	}
}

// backfillOrderGap patches the gap's order activity (reconnect only, the
// non-snapshot path): GET /v2/allOrders walked back over the disconnect window
// per subscribed symbol and account. The authoritative open-order snapshot is
// fetched separately (backfillOrderSnapshots); this recovers the transitions
// that happened DURING the gap. The caller skips it entirely in
// BackfillSnapshotOnly mode.
func (s *Session) backfillOrderGap(sub Subscription, histViable bool, gapStart int64, gen int64, c *unitCollector) {
	for _, symbol := range sub.Symbols {
		if !histViable {
			s.noticef(BackfillNotViable, LevelError,
				map[string]any{"endpoint": "private", "channel": ChannelMyOrder, "symbol": symbol, "reason": "history-window"},
				"the disconnection exceeded the server's 36h history window — order transitions during the gap for %s cannot be recovered (open orders were re-synced)", symbol)
			continue
		}
		for _, seq := range accountSeqValues(sub) {
			c.runNow(retryUnit{channel: ChannelMyOrder, source: "/v2/allOrders", symbol: symbol, seq: seq, run: func() unitOutcome {
				asOf := s.serverNow()
				complete, err := s.fetchHistory(func(req historyPageRequest) (json.RawMessage, error) {
					_, data, _, err := s.rest.raw.OrderHistory(s.ctx(), rawapi.OrderHistoryRequest{
						Symbol: req.Symbol, Limit: req.Limit, StartTime: req.StartTime, EndTime: req.EndTime, AccountSeq: req.AccountSeq,
					}, s.rest.pol())
					return data, err
				}, symbol, seq, gapStart, "createdAt", func(rows []json.RawMessage) {
					if len(rows) == 0 {
						return
					}
					s.emitData(Data{Channel: ChannelMyOrder, Symbol: symbol, Origin: OriginBackfill, Source: "/v2/allOrders", ServerTime: asOf, Payload: joinRows(rows), AccountSeq: seq, PrivateEpoch: gen})
				})
				if err != nil {
					return s.failureOutcome(ChannelMyOrder, symbol, "/v2/allOrders", err)
				}
				if !complete {
					s.noticef(DataGap, LevelWarn,
						map[string]any{"channel": ChannelMyOrder, "symbol": symbol, "source": "/v2/allOrders", "sinceMs": gapStart},
						"order history for %s during the gap was too large to fully recover — some order transitions may be missing (open orders were re-synced)", symbol)
				}
				return unitOK
			}})
		}
	}
}

// backfillMyTrades fetches the fills of the gap and emits the not-yet-seen
// ones (per-row dedupe by tradeId, shared with the live frame path).
func (s *Session) backfillMyTrades(sub Subscription, histViable bool, gapStart int64, gen int64, c *unitCollector) {
	for _, symbol := range sub.Symbols {
		if !histViable {
			s.noticef(BackfillNotViable, LevelError,
				map[string]any{"endpoint": "private", "channel": ChannelMyTrade, "symbol": symbol, "reason": "history-window"},
				"the disconnection exceeded the server's 36h history window — fills during the gap for %s cannot be recovered", symbol)
			continue
		}
		for _, seq := range accountSeqValues(sub) {
			c.runNow(retryUnit{channel: ChannelMyTrade, source: "/v2/myTrades", symbol: symbol, seq: seq, run: func() unitOutcome {
				asOf := s.serverNow()
				complete, err := s.fetchHistory(func(req historyPageRequest) (json.RawMessage, error) {
					_, data, _, err := s.rest.raw.Fills(s.ctx(), rawapi.FillsRequest{
						Symbol: req.Symbol, Limit: req.Limit, StartTime: req.StartTime, EndTime: req.EndTime, AccountSeq: req.AccountSeq,
					}, s.rest.pol())
					return data, err
				}, symbol, seq, gapStart, "tradedAt", func(rows []json.RawMessage) {
					kept := make([]json.RawMessage, 0, len(rows))
					for _, row := range rows {
						id := rowTradeID(row)
						if id == 0 || s.myTradeSeen.checkAndMark(symbol, id) {
							kept = append(kept, row)
						}
					}
					if len(kept) == 0 {
						return // nothing new (empty page, or all rows delivered live)
					}
					s.emitData(Data{Channel: ChannelMyTrade, Symbol: symbol, Origin: OriginBackfill, Source: "/v2/myTrades", ServerTime: asOf, Payload: joinRows(kept), AccountSeq: seq, PrivateEpoch: gen})
				})
				if err != nil {
					return s.failureOutcome(ChannelMyTrade, symbol, "/v2/myTrades", err)
				}
				if !complete {
					s.noticef(DataGap, LevelWarn,
						map[string]any{"channel": ChannelMyTrade, "symbol": symbol, "source": "/v2/myTrades", "sinceMs": gapStart},
						"fill history for %s during the gap was too large to fully recover — some fills may be missing", symbol)
				}
				return unitOK
			}})
		}
	}
}

// rowCount returns the number of elements when data is a JSON array, else 0.
func rowCount(data json.RawMessage) int {
	var rows []json.RawMessage
	if json.Unmarshal(data, &rows) != nil {
		return 0
	}
	return len(rows)
}

// joinRows reassembles verbatim row documents into a JSON array. Unlike
// json.Marshal it cannot fail, so a filtered payload can never be dropped
// after its rows were already marked delivered.
func joinRows(rows []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, row := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(row)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// tradesFetch is the result of one /v2/trades read: the rows selected by the
// caller's id range (kept, newest-first as the server returns them), the oldest
// tradeId the page reached (minFetched, 0 when the page held no valid ids),
// whether the page saturated the request limit (more rows may exist below it),
// and the server time captured at fetch (asOf).
type tradesFetch struct {
	kept       []json.RawMessage
	minFetched int64
	saturated  bool
	asOf       int64
}

// fetchTradesBelow reads up to `limit` recent public trades (capped at the
// server's page limit) and selects the rows with tradeId < highID and, when
// lowID > 0, tradeId > lowID. On a REST or response-shape error it raises
// BACKFILL_FAILED through fail (or the generic reporter when fail is nil) and
// returns ok=false; the caller decides whether that warrants a DATA_GAP. Both
// the public-trade gap patch and the initial history seed read /v2/trades
// through this one path.
func (s *Session) fetchTradesBelow(symbol string, limit int, lowID, highID int64, fail func(error)) (tradesFetch, bool) {
	if limit > tradesPageLimit {
		limit = tradesPageLimit
	}
	report := fail
	if report == nil {
		report = func(err error) {
			s.backfillFailed(ChannelTrade, symbol, "/v2/trades", err)
		}
	}
	asOf := s.serverNow()
	_, data, _, err := s.rest.raw.Trades(s.ctx(), rawapi.TradesRequest{Symbol: rawapi.Symbol(symbol), Limit: &limit}, s.rest.pol())
	if err != nil {
		report(err)
		return tradesFetch{}, false
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		report(fmt.Errorf("response was not an array: %v", err))
		return tradesFetch{}, false
	}
	f := tradesFetch{asOf: asOf, saturated: len(rows) >= limit}
	for _, row := range rows {
		id := rowTradeID(row)
		if id <= 0 {
			continue
		}
		if f.minFetched == 0 || id < f.minFetched {
			f.minFetched = id
		}
		if id < highID && (lowID <= 0 || id > lowID) {
			f.kept = append(f.kept, row)
		}
	}
	return f, true
}

// patchTradeGap recovers a public-trade gap detected at resubscribe: the
// snapshot's oldest tradeId left a hole above the last delivered one. Rows in
// (gapLowID, snapMinID) are fetched from /v2/trades and emitted; whatever the
// REST window cannot reach back to is reported as DATA_GAP.
func (s *Session) patchTradeGap(symbol string, gapLowID, snapMinID int64) {
	bounds := publicTradeGapDetails(symbol, gapLowID, snapMinID)
	recoveredRows, failures := 0, 0
	complete := false
	defer func() {
		s.noticef(BackfillDone, LevelInfo,
			withDetails(bounds, "recoveredRows", recoveredRows, "failures", failures, "complete", complete),
			"public trade gap backfill finished for %s (%d rows, complete=%t)", symbol, recoveredRows, complete)
	}()
	f, ok := s.fetchTradesBelow(symbol, tradesPageLimit, gapLowID, snapMinID, func(err error) {
		s.noticePublicTradeGapFailed(symbol, gapLowID, snapMinID, err)
	})
	if !ok {
		failures = 1
		s.noticef(DataGap, LevelWarn, bounds,
			"public trades for %s may have been missed between tradeId %d and %d (backfill failed)", symbol, gapLowID, snapMinID)
		return
	}
	recoveredRows = len(f.kept)
	if len(f.kept) > 0 {
		s.emitData(Data{Channel: ChannelTrade, Symbol: symbol, Origin: OriginBackfill, Source: "/v2/trades", ServerTime: f.asOf, Payload: joinRows(f.kept)})
	}
	// KNOWN LIMITATION: tradeIds are monotonic but NOT contiguous, so a real gap
	// cannot be distinguished from a normal id skip. Completeness is declared
	// only when the oldest fetched row reaches gapLowID+1 — rarely the true next
	// id — so a legitimate non-contiguous resubscribe that missed nothing can be
	// reported as a possible DATA_GAP below. Keying completeness on
	// truncation instead (`saturated`: whether the fetch hit the request limit,
	// certain truncation, vs returned the server's full recent buffer) is
	// deferred.
	complete = f.minFetched > 0 && f.minFetched <= gapLowID+1
	if !complete {
		bounds["recoveredRows"] = len(f.kept)
		bounds["saturated"] = f.saturated
		if f.minFetched > 0 {
			bounds["oldestAvailableTradeId"] = f.minFetched
		}
		s.noticef(DataGap, LevelWarn, bounds,
			"public trades for %s between tradeId %d and %d may have been missed — the server no longer has that range", symbol, gapLowID, snapMinID)
	}
}

func publicTradeGapDetails(symbol string, gapLowID, snapMinID int64) map[string]any {
	return map[string]any{
		"endpoint": "public", "reason": "gap", "channel": ChannelTrade,
		"symbol": symbol, "afterTradeId": gapLowID, "beforeTradeId": snapMinID,
	}
}

func withDetails(base map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		if key != "" {
			out[key] = kv[i+1]
		}
	}
	return out
}

func (s *Session) noticePublicTradeGapDisabled(symbol string, gapLowID, snapMinID int64) {
	details := publicTradeGapDetails(symbol, gapLowID, snapMinID)
	s.noticef(BackfillDisabled, LevelWarn, withDetails(details, "backfill", "disabled"),
		"backfill is disabled: public trades for %s between tradeId %d and %d are NOT patched", symbol, gapLowID, snapMinID)
	s.noticePublicTradeGapLost(symbol, gapLowID, snapMinID, "disabled")
}

func (s *Session) noticePublicTradeGapUnavailable(symbol string, gapLowID, snapMinID int64) {
	details := publicTradeGapDetails(symbol, gapLowID, snapMinID)
	s.noticef(BackfillFailed, LevelError, withDetails(details, "source", "/v2/trades", "error", "REST backfill unavailable"),
		"backfill /v2/trades for trade failed: REST backfill is unavailable")
	s.noticePublicTradeGapLost(symbol, gapLowID, snapMinID, "unavailable")
}

func (s *Session) noticePublicTradeGapFailed(symbol string, gapLowID, snapMinID int64, err error) {
	details := publicTradeGapDetails(symbol, gapLowID, snapMinID)
	s.noticef(BackfillFailed, LevelError, withDetails(details, "source", "/v2/trades", "error", err.Error()),
		"backfill /v2/trades for public trade gap in %s failed: %v", symbol, err)
}

func (s *Session) noticePublicTradeGapLost(symbol string, gapLowID, snapMinID int64, backfill string) {
	s.noticef(DataGap, LevelWarn, withDetails(publicTradeGapDetails(symbol, gapLowID, snapMinID), "backfill", backfill),
		"public trades for %s may have been missed between tradeId %d and %d (backfill is %s)", symbol, gapLowID, snapMinID, backfill)
}

// seedTradeHistory seeds the trade channel's initial subscription with recent
// history: it fetches the trades preceding the live snapshot and emits the most
// recent `want` of them with Origin OriginBackfill. The snapshot already
// delivered the newest `snapCount` rows, so the page asks for want+snapCount and
// keeps only the rows below snapMinID, then trims to `want`. This is best-effort
// depth, not gap recovery — fetching fewer than asked just means the server's
// recent-trades buffer holds no more, so it raises no DATA_GAP; only a REST
// failure is surfaced (as BACKFILL_FAILED, from the shared fetch).
func (s *Session) seedTradeHistory(symbol string, want, snapCount int, snapMinID int64) {
	if s.cfg.DisableBackfill || s.rest == nil || want <= 0 {
		return
	}
	f, ok := s.fetchTradesBelow(symbol, want+snapCount, 0, snapMinID, nil)
	if !ok {
		return
	}
	if len(f.kept) > want {
		f.kept = f.kept[:want] // newest-first: keep the `want` most recent
	}
	if len(f.kept) > 0 {
		s.emitData(Data{Channel: ChannelTrade, Symbol: symbol, Origin: OriginBackfill, Source: "/v2/trades", ServerTime: f.asOf, Payload: joinRows(f.kept)})
	}
}

// backfillFailed emits the BACKFILL_FAILED notice for one recovery call and
// reports whether the failure is worth re-running (non-fatal per
// apiclient.Classify): network/5xx/429 may heal on a later attempt; a
// definitive 4xx rejection cannot.
func (s *Session) backfillFailed(channel, symbol, source string, err error) bool {
	details := map[string]any{"channel": channel, "source": source, "error": err.Error()}
	if symbol != "" {
		details["symbol"] = symbol
	}
	s.noticef(BackfillFailed, LevelError, details,
		"backfill %s for %s failed: %v — live data keeps flowing but the gap was not recovered", source, channel, err)
	return apiclient.Classify(err) != apiclient.ClassFatal
}

// failureOutcome maps one failed recovery call to its retry-unit outcome,
// emitting the BACKFILL_FAILED notice on the way (backfillFailed).
func (s *Session) failureOutcome(channel, symbol, source string, err error) unitOutcome {
	if s.backfillFailed(channel, symbol, source, err) {
		return unitRetry
	}
	return unitFatal
}

// rowTradeID extracts tradeId from one trade row, 0 when absent/invalid.
func rowTradeID(row json.RawMessage) int64 {
	var v struct {
		TradeID int64 `json:"tradeId"`
	}
	if json.Unmarshal(row, &v) != nil {
		return 0
	}
	return v.TradeID
}
