// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package korbit is the wire layer (L0) plus the L1 primitive client for the
// Korbit Open API v2.
//
// # L0 — wire building blocks (no policy)
//
//   - request building + ordered, insertion-order param encoding (encode.go),
//   - ED25519 signing over the exact sent bytes, signature appended last
//     (sign.go),
//   - the {success,data} / {success,error} envelope unwrap into json.RawMessage
//     or an *output.ApiError (client.go),
//   - the server-clock measurement primitive MeasureClockOffset + ClockOffset
//     (timesync.go),
//   - the retry taxonomy: Classify → RetryClass, the backoff ladder, and the
//     bounded ExecuteWithRetry loop (retry.go).
//
// These carry no call policy: they do exactly what they are told once.
//
// # L1 — the primitive client (Client.Do)
//
// Client.Do(ctx, Call, Policy) is a contract-faithful transport: it signs via a
// shared clock, executes exactly the retry Policy it is HANDED, and reports
// every logical call to a per-call Recorder it mints itself. The same Client
// also signs the private WebSocket upgrade (SignHandshake), so it is the single
// front door for calling Korbit. Higher layers (L2 ops, frontends) own the
// policy and the journaling; the Client owns neither.
//
// # Contracts
//
//   - Policy is INPUT. The zero Policy is a single shot — the fail-safe default,
//     identical to the RetryPolicy zero value. Idempotent gates the full ladder;
//     RetryPreExec retries only the provably-pre-execution classes.
//   - One Do == one Recorder.Record. NewRecorder mints a FRESH per-call Recorder
//     for each Do (nil = not recorded); Do calls its Ready ONCE before the first
//     send (an error aborts with nothing on the wire — preserving the
//     open-journal-before-send hard guarantee) and Record ONCE after the call
//     completes, with the attempt count folded in.
//   - A POST-call journal failure never returns through Do. The Recorder decides
//     fail-vs-warn and delivers it through its OWN sink (callrec's onPostFailure):
//     Record returns no error and Meta carries none, so the API error and the
//     journal error stay on strictly separate channels. A frontend's sink can
//     warn, toast, or capture-for-fatal after emitting the API result.
//   - Origin flows into CallInfo (the journal `origin` column is reserved, not
//     written). ParamsJSON is PRE-SIGNING params only — the timestamp,
//     recvWindow, and signature are added inside Build after the CallInfo is
//     captured, so no secret or signing artifact ever reaches a Recorder.
//   - The User-Agent seam: Options.UserAgent / Client.UserAgent override the
//     header; "" keeps the default ("korbit-cli/<version>"). COMPOSING the
//     string (program version + OS + Origin) is the caller's job at wiring
//     time — this package gathers no OS info and only carries the seam.
//
// # The pre-execution vs ambiguous taxonomy
//
// Classify splits errors into four RetryClasses grouped by side-effect
// provability: the PRE-EXECUTION group {ClassTimeWindow, ClassRateLimited} —
// the server provably rejected the request before any side effect, so a resend
// is safe even for a money mover — and the AMBIGUOUS group {ClassTransient} —
// the request may have executed (lost response), so only an idempotent request
// may be blindly resent. ClassFatal is everything else. This vocabulary is what
// the L2 order-place reconcile protocol consumes (IsPreExecution / IsAmbiguous).
//
// # Loop-safety invariants (every retry/reconnect loop in the tree)
//
// Auto-retry is spread across layers, so this is the single audit reference for
// "no loop can spin uncontrolled" — by a single class or any combination. Each
// loop below names its TERMINATION bound (what makes it stop / why it cannot run
// hot) and, where applicable, its structural BACKSTOP (a bound that holds even
// if the semantic logic is later broken). The governing rule across all of them:
// every iteration must make progress — it returns, sleeps (drawing down a finite
// budget), or spends one of a bounded number of no-sleep corrective retries; an
// iteration that does none is the only way to spin, and the backstops below
// catch exactly that.
//
//   - retryEngine.run (retry.go) — the shared L0/L1 ladder behind
//     ExecuteWithRetry and Client.Do. Bound: the sleeping classes (429,
//     network/5xx) each draw down BudgetMs; EXCEED_TIME_WINDOW is a single
//     no-sleep corrective resync capped by the `corrected` once-flag; everything
//     else is single-shot or surfaced. Backstop: a hard iteration ceiling
//     (MaxRetryIterations) — unreachable while the above hold, surfaces the last
//     error if a future edit adds an unbounded path.
//
//   - the order-place reconcile loop (placeOp.runReconcile, ops/op_place.go). It
//     drives single-shot sends itself (the L1 ladder is NOT engaged: zero Policy),
//     so a money mover is never blindly resent. Bound: 429/transient resends draw
//     down RetryBudgetMs; EXCEED_TIME_WINDOW is one resync capped by `resynced`;
//     DUPLICATE/fatal exit. Backstop: the same MaxRetryIterations ceiling; an
//     exit without positive proof of placement fails safe to the UNKNOWN
//     endgame (never "placed", never re-places).
//
//   - the eventual-consistency read-back (lookupOrderTyped, ops/op_place.go).
//     Bound: a fixed lookupAttempts cap with fixed spacing (~1s total). Each
//     attempt's GET is itself bounded by the L1 ladder above.
//
//   - ops.WalkHistory / the candles op (ops/history.go, ops/op_candles.go) — cursorless
//     pagination. Bound: an explicit page cap (maxHistoryPages) / row ceiling
//     (CandlesMaxLimit) plus no-progress guards; each page request is bounded by
//     the L1 ladder.
//
//   - clock.Syncer.Sync (internal/clock) — server-clock measurement. Not a loop:
//     single-flight collapses concurrent triggers onto one probe and a cooldown
//     reuses a fresh estimate, so a persistently rejected clock can neither loop
//     nor storm /v2/time. This complements the per-call resync once-flags above:
//     at most one resync per logical call, at most one probe per cooldown window
//     process-wide.
//
//   - stream.connManager.run (internal/stream) — the WebSocket reconnect loop,
//     intentionally unbounded (a resilient stream reconnects forever). Its
//     safety is a RATE bound, not a count: EVERY reconnect attempt is preceded
//     by a jittered exponential backoff (capped at ReconnectMaxMs), applied once
//     at the top of the loop so no path — refused dial, failed subscribe, or a
//     connection accepted then instantly dropped — can re-dial without pausing.
//     The lone no-backoff retry is a single EXCEED_TIME_WINDOW resync, capped by
//     `resynced`. Definitive 4xx rejections and all-subscriptions-rejected are
//     fatal; ctx cancellation always exits.
//
// # Dependency rule
//
// This package MUST NOT import internal/journal (the Recorder interface is the
// seam — concrete recorders live in a frontend) and MUST NOT import
// internal/clock (no cycle). The L1 client reads the shared clock through the
// Clock interface (clock.State satisfies it structurally) and resyncs through a
// plain Resync func() error hook, rather than holding a *clock.State. The clock
// measurement primitive (MeasureClockOffset) lives here; the shared estimate
// holder lives in internal/clock.
package korbit
