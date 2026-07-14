// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package callrec is the single home of the CLI's journaling POLICY plus the
// journal-backed korbit.Recorder that enacts it. The L1 client (internal/korbit)
// owns no journaling: it calls an injected Recorder's Ready before the first
// send and Record once after, and a post-call failure is delivered through this
// package's onPostFailure sink — never back through the client. This package is
// the concrete Recorder, and — crucially — the one place the policy lives.
//
// # Why the policy lives here, alone
//
// Whether a given call is journaled, and whether a post-call journal failure is
// FATAL or merely a warning, is decided in ONE place: DefaultPolicy (or a caller
// swapping in its own PolicyFunc). Every surface — cli, doctor, monitor — routes
// through the same decision.
//
// A Decision has two axes:
//
//   - Record   — journal this call at all?
//   - PostFailure — if the post-call Record write fails, is that FATAL (Fail: the
//     onPostFailure sink captures it, and the cli surfaces it AFTER emitting the
//     API result) or a WARNING (Warn: the sink logs/toasts it and the command
//     still succeeds)?
//
// DefaultPolicy(debug) is the policy. See its table for the per-surface rules;
// the doctor row (never record) is kept explicit rather than left to the
// default.
//
// # Lazy open + the pre-send hard guarantee
//
// The journal DB file must not even be created unless a call actually records —
// this preserves the property that pure public use never touches a read-only
// home. So the Recorder opens the journal LAZILY: it opens only when (and only
// when) the policy says a call records, at Ready time. Ready is the L1 client's
// pre-send gate; opening there means a broken journal (e.g. a read-only home)
// fails the command with NOTHING sent on the wire — the open-journal-before-send
// hard guarantee. On open failure Ready returns the output.Configf error text
// so the error and its exit-4 classification are preserved.
//
// For order placement the order INTENT row is written before the placement send
// too (StartOrder, an explicit pass-through the CLI calls before korbit.Do); its
// failure aborts with nothing sent, and FinishOrder folds in the result after.
// Routing orders through this package — rather than a caller holding a raw
// journal.Logger — keeps every journaled write behind one lazily-opened handle.
//
// KORBIT_CLI_NO_JOURNAL is honored here: the Decision short-circuits to "don't
// record" before any open, so opting out never creates the file.
//
// # Per-call isolation (ForCall) — concurrency
//
// The korbit.Recorder interface (Ready then Record) carries no per-call token,
// so any state a call needs between its Ready and its Record — the start
// timestamp captured pre-send, and the spec/insertion-ordered params_json that
// overrides the client's map-sorted CallInfo.ParamsJSON — must NOT live on the
// shared Recorder, or two concurrent calls would clobber each other. Instead
// Recorder.ForCall(orderedParams) returns a fresh per-call CallRecorder (sharing
// the parent's journal, policy, clock, and onPostFailure sink) that the client's
// NewRecorder hands back for exactly one Do. The CLI makes one per runEndpoint; the
// monitor surface, which fires concurrent calls — including several of the same
// command — through one shared parent, gets correct isolation for free by taking
// a fresh ForCall per Do. The parent Recorder owns only shared, safely-locked
// state (the lazily-opened journal handle and the order-row seam).
//
// # Close concurrent with in-flight calls
//
// A recorder can be Close()d while a call is still in flight — the TUI closes
// its recorders as soon as the Bubble Tea program returns, but Bubble Tea does
// not await the command goroutines that run order placements, cancels, and
// candle fetches, so one can call into the recorder after Close(). Every journal
// handle read therefore goes through the lock: ensureOpen returns the live
// *journal.Logger under mu (open-then-write paths), and lockedJL reads it under
// mu (the post-send order/operation finish paths and the per-call recorders,
// whose journal was opened earlier), never a panic. Close is also terminal: it
// sets a closed flag so ensureOpen never reopens a fresh handle that nobody would
// close. What a late call does after Close depends on whether its write is
// pre-send (the hard guarantee) or post-send (diagnostic):
//
//   - Pre-send, would-record writes REFUSE: Begin (the operations ledger row) and
//     StartOrder (the order intent row) return a fatal ConfigError, so the
//     operation aborts before anything is sent. This is the same failure an
//     open/insert error produces; it must NOT be downgraded to a non-recording
//     handle, or a money op could place an order with no mint-time row.
//   - Post-send, diagnostic writes NO-OP: Record (api_calls) and the
//     order/operation finish paths cleanly skip — the outcome is already decided,
//     so a dropped row is diagnostic, not a safety signal.
//
// This kills the pointer data race and the nil-deref. A write can still
// hit a handle Close shuts mid-call (the narrow TUI-shutdown window); that
// degrades to a journal error handled as a post-send Warn — an accepted
// limitation, since the lost row is post-send diagnostic, not a money-safety
// signal (the pre-send order intent is written and gated before any send). The
// recorder deliberately does NOT drain in-flight writes before Close: the
// pre-send hard guarantee is unaffected, and serializing Close against every
// write to recover a diagnostic row in a shutdown-only window is not worth the
// added lifecycle coupling.
//
// The injectable clock: korbit.Do brackets a call with the real, un-injectable
// wall clock (Outcome.StartedAtMs/FinishedAtMs), but the journal stamps ALL its
// own time columns — every operations, orders, and api_calls row — from the
// caller's single injectable clock (the system clock; ops supplies no timestamp).
// So the CallRecorder captures the api_call start on that clock at Ready and the
// finish at Record, and the operation/order rows are stamped on the same clock at
// Begin/StartOrder/Finish — one clock for the whole journal, deterministic under a
// test clock.
//
// # No secrets
//
// Only pre-signing facts are ever written: the public api-key id and the
// pre-signing params (korbit assembles the timestamp/recvWindow/signature AFTER
// the CallInfo is captured, so none of them reach a Recorder). The keystore is
// never read for journaling. This is the same no-secrets rule the journal
// package itself documents.
//
// # FailMode semantics
//
// A post-call write failure never returns through the wire client; it is handed
// to the onPostFailure(FailMode, error) sink the caller injects, and the sink
// reacts per mode:
//
//   - Fail — the sink captures the error; the CLI emits the API result first,
//     then surfaces the journal failure with a non-zero exit (runEndpoint
//     sequencing). The api_calls write uses the call's FailMode; the
//     operations-ledger Finish additionally returns its error so the cli can join
//     it with the api_calls one (ops.Result.JournalErr).
//   - Warn — the sink logs/toasts the error and the command still succeeds. This
//     is the monitor/tui/mcp surfaces' behavior; any surface whose policy selects
//     Warn gets it. The order-outcome (FinishOrder) and operations-ledger
//     warnings are ALWAYS delivered as Warn (their result is already decided), so
//     a finish-write failure never fails a placement that already happened.
//
// A Ready (pre-send) open failure is ALWAYS fatal regardless of PostFailure: it
// is the hard guarantee, and nothing has been sent yet. PostFailure governs only
// the post-call write.
package callrec
