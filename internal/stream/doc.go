// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package stream is the resilient real-time data layer over the Korbit
// WebSocket API. It owns everything between "I want these channels" and a
// single ordered stream of events: connection management (signed upgrade for
// the private endpoint, reconnection with backoff, resubscription), REST
// backfill of whatever a disconnection may have missed, and health reporting
// (RTT, delivery delay, disconnect frequency, keepalives).
//
// The package is built for two consumers: a line-oriented streaming command
// whose output is watched by an agent, and a TUI that materializes state from
// the same events. Both depend on the same property: the consumer must either
// be up to date or be TOLD, in-band, that it may not be. Every way the stream
// can silently fall behind therefore has a Notice code, and silence itself has
// one (KEEPALIVE).
//
// One Session (see session.go) = up to two managed connections (public
// /v2/public, private /v2/private) + REST recovery + a single ordered event
// channel of Data and Notice values. The package is self-contained so the
// command surface above it stays stable as it grows.
//
// # The two endpoints, and the recovery matrix that follows
//
// The two Korbit WebSocket endpoints have different reliability contracts, and
// the recovery strategy per channel follows from them.
//
// Public is lossy — the server may drop messages under load — but every
// subscribe is answered with a snapshot, and ticker/orderbook frames each carry
// complete state. So ticker and orderbook self-heal on resubscribe. Public
// trade has per-symbol monotonic (NOT assumed contiguous) tradeIds: duplicates
// below the delivered high-water mark are dropped (a resubscribe snapshot of
// only already-seen trades therefore emits empty — the snapshot boundary is
// still delivered so a consumer is TOLD it is current, never left waiting), and a
// resubscribe snapshot whose oldest row is more than one id above the mark is
// patched from GET /v2/trades when backfill is enabled. Before that snapshot is
// emitted, the session emits either BACKFILL_START (endpoint=public,
// channel=trade, reason=gap, plus the id bounds) or, when recovery is disabled,
// BACKFILL_DISABLED followed by DATA_GAP. An enabled asynchronous patch ends
// with the paired BACKFILL_DONE at the same level as BACKFILL_START, with
// complete=true when the fetch reaches back to mark+1; anything less raises
// DATA_GAP with a saturated detail saying whether the loss is certain (the fetch
// hit its row limit) or merely possible. KNOWN LIMITATION: tradeIds are
// non-contiguous, so mark+1 is rarely the true next id and a resubscribe that
// missed nothing can still raise a spurious BACKFILL_START/DATA_GAP; keying
// completeness on truncation (saturated) instead is deferred. The public snapshot's row count is
// server-defined (it may be a single trade), so a trade subscription can request
// an initial history depth (Subscription.TradeHistory): on the first snapshot
// the session fetches that many trades preceding it from GET /v2/trades and
// emits them as
// origin=backfill. That is best-effort depth, not gap recovery — a short buffer
// just yields fewer rows and raises no DATA_GAP — and, like the gap patch, it
// lands asynchronously, so a consumer orders public trades by tradeId, never by
// arrival.
//
// Private is lossless while connected — the server force-closes rather than
// drop — but sends NO snapshot on subscribe (myAsset frames are partial
// deltas). So private channels are backfilled from REST on every connect: balances
// and open orders always; after a reconnect also the gap's orders/fills via
// GET /v2/allOrders / GET /v2/myTrades (36h documented history window →
// BACKFILL_NOT_VIABLE beyond it). myTrade rows are deduped by tradeId across
// live and backfill (seenIDs), so a fill can never be double-delivered.
//
// Private recovery self-heals: a recovery call that fails past its bounded
// in-call retry ladder (BACKFILL_FAILED) does not wait for the next
// reconnect — while the connection stays up, exactly the failed calls are
// re-run on a jittered exponential backoff, each attempt a fresh
// BACKFILL_START/BACKFILL_DONE pair with reason="retry", so a REST-side
// outage heals in place. A call that succeeded is never replayed — the retry
// shrinks to what is still failing, so it cannot feed a rate limit that
// caused the failure. Only transient failures (network/5xx/429) re-run; a
// definitive 4xx rejection waits for the next (re)connect, whose full pass
// re-attempts everything anyway. On-demand open-order snapshots (the
// LazyOpenOrders tracking path, scoped per {account, symbol} — see
// SetTrackedOrderScopes) self-heal through the same loop, so a scope tracked
// mid-session also becomes ready eventually while the connection stays up.
// A scope RE-tracked within the same private connection is not re-snapshotted
// at all: its recorded baseline plus the lossless account-wide live feed
// already make it current, so flipping focus back and forth costs no REST
// calls — only a reconnect (a new generation) re-baselines.
// (See retryFailedBackfill in backfill.go.)
//
// Config.BackfillSnapshotOnly trims this to the snapshots only — balances and
// the open-order set — and skips the per-symbol allOrders/myTrades gap walks.
// It is for a session subscribing the account channels across many symbols,
// where the per-symbol history fan-out on every reconnect is a REST storm; the
// accepted trade-off (gap order/fill transitions are not recovered, though the
// open-order snapshot still reconciles what is open now) is announced with a
// BACKFILL_SNAPSHOT_ONLY notice on every connect.
//
// # History backfill never silently under-delivers
//
// History backfill paginates until a short page proves coverage (fetchHistory):
// a page that fills the server's 1000-row limit was truncated, so the next page
// narrows the window (endTime = oldest seen +1; start-inclusive/end-exclusive).
// The server's sort order is observed, not contractual, so each page's direction
// is re-detected and an oldest-first page advances startTime forward instead.
// When the window cannot be fully covered (page cap, missing timestamps, no
// progress) the consumer is TOLD via DATA_GAP — never silently under-delivered.
//
// # Notice codes are a stable agent-facing contract
//
// The Notice codes (events.go) are extended additively, never renamed. Anything
// that can make the consumer silently fall behind must have a code; silence
// itself has one (KEEPALIVE, which also reports how many connections are up).
//
// # Notice levels (the log spec)
//
// Each Notice carries a Level so a consumer can threshold on it (the monitor's
// --stream-log-level is exactly such a threshold). The governing rule: a degradation
// and the recovery that clears it share a level, so one threshold catches both
// the problem and its resolution — never a warning with no visible "all clear".
// A code's level is fixed EXCEPT where the same code means different things in
// different contexts (CONNECTED, SUBSCRIBE_FAILED, BACKFILL_SNAPSHOT_ONLY),
// which is why level is a field on each emission, not a property of the code.
//
//	level  notices                                              meaning
//	-----  ---------------------------------------------------  -------------------------------
//	error  FATAL, BACKFILL_FAILED, BACKFILL_NOT_VIABLE,         unrecoverable: data lost for
//	       SUBSCRIBE_FAILED (a dropped subscription)            good, or the session is ending
//	warn   DISCONNECTED, CONNECT_FAILED, DATA_GAP,              a problem AND its recovery —
//	       SERVER_ERROR, BACKFILL_DISABLED, SUBSCRIBE_FAILED    onset and clear share this level:
//	       (a benign unsubscribe reject); the recovery edges    UNRELIABLE↔STABLE, DELAYED↔CURRENT,
//	       CONNECTION_STABLE, DATA_CURRENT; and a CONNECTED      and a CONNECTED that follows a
//	       that resolves a prior drop/failed connect            DISCONNECTED/CONNECT_FAILED
//	info   a clean first CONNECTED, KEEPALIVE, BACKFILL_START,  routine operation, no action
//	       BACKFILL_DONE                                        needed
//
// So a warn threshold yields "every problem and its resolution, no routine
// chatter"; info adds the routine lifecycle; error is the unrecoverable subset.
// (One refinement: a private BACKFILL_FAILED is final for its pass, not
// necessarily for the session — a transient failure is re-run with backoff
// while the connection stays up. The code keeps its error level because at
// emission time the data IS missing and recovery is not guaranteed; the
// retry's own BACKFILL_START/DONE pair reports the healing.)
//
// BACKFILL_SNAPSHOT_ONLY (emitted only in BackfillSnapshotOnly mode, e.g. the
// TUI — never the monitor) is the third context-dependent code and the one
// unpaired warn: info on the initial connect (no gap yet), warn on a reconnect
// (where the skipped per-symbol history is a real, accepted loss). Its warn has
// no clearing notice by design — the loss is announced, not recovered.
//
// # Logging
//
// This layer is logging-capable but NOT logging-opinionated: it logs through an
// OPTIONAL operational logger the frontend injects (Config.Log here, the state
// store's Config.Log in stream/state). nil = silent (resolved via logging.Or),
// so a layer with no logger wired stays quiet. The records split into three
// kinds, each tagged so a reader can filter, all sharing the operational
// logger's destination (the cli's --log-file file, else stderr):
//
//	kind        source                     tag(s)                               gated by             examples
//	----------  -------------------------  ------------------------------------ -------------------  --------------------------------
//	mechanics   internal/stream            component=stream                     --log-level          ws dial, ws upgrade rejected,
//	            (Config.Log)                                                                         subscribe, reconnect backoff,
//	                                                                                                 backfill window
//	state       internal/stream/state      component=stream/state               --log-level          state epoch bump,
//	            (state.Config.Log)                                                                   state order gap-closed
//	notices     the Notice event stream,   component=stream kind=stream_notice  --stream-log-level   DISCONNECTED, DATA_GAP, CONNECTED,
//	            mirrored by the frontend   code=<CODE>                          (default off)        BACKFILL_FAILED (see the level
//	            via LogNotice                                                                        table at the notice's level above)
//
// So `grep component=stream` isolates the whole layer; `grep kind=stream_notice` just
// the reliability notices; `grep component=stream/state` just the reconcile
// trace. (Under --log-format json these tags are emitted as JSON fields.)
//
// The mechanics/state are ordinary operational diagnostics (the same kind as the
// REST/clock/keychain logs): every call site emits at Debug and rides --log-level,
// so they are SILENT unless --log-level debug (or --debug). The NOTICES are
// different — they already appear on the frontend's stdout (monitor) / on-screen
// pane (tui), so logging them is a separate OPT-IN: the monitor's
// --stream-log-level is INDEPENDENT of --log-level and OFF by default; set it to
// also mirror notices into the log (e.g. when --jq/--max-events narrows stdout, or
// for a persistent reliability trail). Notices emit at their own level (the table
// in "Notice levels" above). The tui has no stdout notice stream and no
// --stream-log-level, so there notices follow --log-level into the --log-file.
// LogNotice (events.go) is a pure RENDERING helper
// (notice -> level + message + code attr); the DECISION to mirror — when, to
// which logger/threshold — lives in the frontend (monitor/tui), never here, so
// notices stay program output on the event stream first and a log only by the
// frontend's choice. Inside this layer, call x.log() (logging.Or(Config.Log)) and
// log plain key=value facts; the component tag is attached by the injected
// logger, so call sites never name it. NEVER log a secret — the signed upgrade is
// logged as host/path only, never the signed query/signature.
//
// # Payloads are verbatim source documents
//
// Payloads pass through with decimal strings untouched: WebSocket frames for
// realtime/snapshot origins, REST response data for backfill (Data.Source
// carries the producing REST path, which also identifies the row shape). The one
// exception: a trade/myTrade frame whose rows were partially delivered before is
// re-emitted with only the fresh rows (rows stay verbatim; the enclosing frame
// is rebuilt — and on a rebuild failure the verbatim frame is emitted instead,
// because re-delivering duplicates is harmless while suppressing marked-seen rows
// is silent loss). A resubscribe trade snapshot of only already-seen rows is the
// one case that re-emits EMPTY rather than being dropped: the snapshot marks the
// (re)subscribe boundary, so it is delivered to keep the "up to date or TOLD"
// guarantee (a live all-duplicate frame, by contrast, is dropped). (This rule
// covers what a Session emits. The one non-verbatim Origin, OriginDerived — the
// monitor's CLI-authored candle payload — is produced by internal/candles on
// top of the session, never by a Session itself; see its doc in events.go.)
// Events are not globally ordered across origins — consumers
// order per key, not by arrival. Backfill events carry ServerTime = the
// server-clock estimate when the fetch began (conservative), so "newest
// ServerTime wins per key" orders them against live frames — this is what makes
// the no-ordering-key myAsset channel reconcilable.
//
// # The private upgrade is signed like a REST request
//
// The signed upgrade query (timestamp=<ms>, plus recvWindow when the measured
// clock uncertainty needs a wider validity window, signature appended last) is
// produced by the session's Config.Client itself — Client.SignHandshake reuses
// the SAME ordered-encode + signature-last core as REST signing — and the
// X-KAPI-KEY header carries Client.APIKeyID. Re-signed fresh on every dial. The
// server clock is measured proactively — synchronously, before the first dial —
// when Config.ProactiveTimeSync is set; otherwise the first re-measurement is
// reactive: an EXCEED_TIME_WINDOW handshake rejection (before the retry), or the
// delivery-delay check kicking a resync when it suspects lag against an
// unmeasured clock (Client.Resync, the shared clock.Syncer, leaned into the past
// by the measurement uncertainty). A 4xx upgrade rejection carrying
// a Korbit error envelope is fatal (reconnecting cannot fix credentials); a 4xx
// WITHOUT the envelope may come from an intermediary and is retried with backoff.
//
// # One Client, one shared clock
//
// The session takes a single Config.Client (a *korbit.Client): it performs REST
// backfill, signs the WS upgrade, and owns the shared clock (read via
// Client.Clock, resync via Client.Resync). A consumer that needs signed REST (the
// monitor command's JavaScript korbit.* bindings) builds its OWN korbit.Client
// over the SAME clock.Syncer — so the WS upgrade, backfill, and the consumer's
// calls all sign against one estimate, measured through one single-flight/cooldown
// path, and an EXCEED_TIME_WINDOW resync on any surface fixes them all at once.
// Cursorless window-walk paging lives in internal/ops as WalkHistory (the engine
// behind this layer's fetchHistory); stream imports ops for it (the allowed
// direction is stream → ops). The stream layer itself stays journal-agnostic: it
// imports no journal package; the Client journals each backfill call through its
// own per-call recorder, governed by the consumer's callrec policy, which exempts
// the stream-backfill surface (recovery reads, not actions), so they are not
// recorded.
//
// # Liveness is actively probed
//
// Client pings every PingIntervalMs double as RTT samples; a missed pong tears
// the connection down (half-open detection) and the reconnect loop (jittered
// exponential backoff, fatal-aware) takes over. Reconnect/backoff, keepalive,
// delay and unreliability thresholds all live in Tunables so tests run the real
// machinery in milliseconds. Config.NoReconnect turns the loop off: the first
// failed connect or drop emits its lifecycle notice plus a terminal FATAL and
// returns the error, so Run ends (any one endpoint dropping ends the session);
// the one-shot EXCEED_TIME_WINDOW resync still runs, as it establishes the first
// connection rather than reconnecting.
//
// # Tests
//
// stream_test.go drives the full lifecycle through fake conns/dialers and a stub
// REST doer; dialer_test.go exercises the real coder/websocket adapter against an
// in-process server. e2e_test.go is opt-in (KORBIT_STREAM_E2E_BASE pointing at a
// running sandbox, plus KORBIT_STREAM_E2E_KEY_ID/KORBIT_STREAM_E2E_PEM_FILE for
// the private test and KORBIT_STREAM_E2E_SOAK_MS for a hold-open soak) and
// verifies snapshots, the signed upgrade, REST backfill, and a real placed order
// arriving on myOrder.
package stream
