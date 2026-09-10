// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package clock holds one session's (or process's) estimate of the Digital X
// server clock — the single piece of shared state behind every signed request —
// and the one operation that measures and installs it. State is the estimate;
// Syncer is the measurement home shared by every signing surface (see below).
//
// # Why one estimate is shared
//
// Digital X verifies each signed `timestamp` against an asymmetric window:
// serverTime − recvWindow ≤ ts < serverTime + 1000 (recvWindow default 5s,
// max 60s; the +1s future bound is fixed). So a corrected timestamp must track
// the SERVER's clock, not UTC, and must lean into the past by the measurement
// uncertainty so it can never cross the +1s bound.
//
// A single State is meant to be shared by every surface that signs against that
// window within a session or process:
//
//   - WebSocket upgrade signing (the private endpoint's signed query),
//   - REST request signing (backfill reads, and any L1 client wired to it),
//   - the corrective EXCEED_TIME_WINDOW resync (re-measure → Install),
//   - delivery-delay measurement (translating server frame timestamps into
//     local terms via Offset).
//
// Sharing one State means a correction discovered on any surface (typically a
// REST EXCEED_TIME_WINDOW resync) is instantly visible to all the others: the
// skew is measured once per session, not re-learned per call.
//
// # The surface
//
// State — the estimate (mutex-guarded, safe for concurrent use):
//
//   - New(now)            — build over a local-clock reader (test seam).
//   - Install(offset,lean)— record a measured estimate (plain int64s).
//   - SignNow()           — the timestamp to sign with (estimate, leaned past).
//   - ServerNowMs()       — the server-now estimate (local + offset, UN-leaned),
//     for defaulting server-relative lookback windows sent on the wire (the
//     history walk's start). NOT for stamping local records — the action journal
//     uses the caller's own system clock.
//   - Offset()            — the raw un-leaned offset (delay measurement).
//   - Measured()          — whether an estimate has been installed yet.
//   - RecvWindowMs()      — the one widened recvWindow every signer includes
//     (REST, the WS upgrade, the corrective resync); 0 below the 5s default
//     (omit-below-5s — the server then applies its own 5s window).
//
// Syncer — the measurement operation over a State (safe for concurrent use):
//
//   - NewSyncer(state,measure,now,coolDownMs) — build it; measure is a MeasureFunc.
//   - Sync()              — measure and Install, subject to single-flight + cooldown.
//   - State()             — the shared State it installs into.
//   - DefaultCoolDownMs   — the production cooldown spacing.
//
// The 3×RTTmin (== 6×lean) widening rule lives here in one place, so every
// signing surface — REST, the WS upgrade, and the corrective resync — enforces
// the identical validity window (RecvWindowMs, omit-below-5s).
//
// # Requirements for callers
//
//   - Build ONE Syncer per process and share it (with its State) across every
//     signing surface, so the offset is measured once and a correction on any
//     surface is seen by all. Do NOT make a second Syncer/State for the same
//     process — that defeats the shared-estimate property.
//   - The estimate is injected, not a package global, so the test suite can run
//     many in-process commands without cross-test skew leakage.
//   - Proactive measurement is the caller's choice (the CLI gates it on
//     --time-sync on); the reactive resync calls Sync — on a correctable
//     EXCEED_TIME_WINDOW rejection, or the stream's delivery-delay kick against an
//     unmeasured clock — and is wired for every signed call (and a public
//     streaming session that opts into delay detection) except under --time-sync
//     off (which opts out of all correction). Loop/storm safety comes from three
//     layers: the retry layer's per-call once-flag, the Syncer's single-flight,
//     and its cooldown.
//
// # The Syncer (measurement + install)
//
// State holds the estimate; the Syncer owns the one operation that MEASURES and
// installs it. A process builds ONE Syncer over its State and shares it with
// every signing surface, so the offset is measured once and a correction is
// visible everywhere. The Syncer adds single-flight (concurrent triggers share
// one probe) and a cooldown (a fresh estimate is reused for DefaultCoolDownMs
// before re-probing), which together with the retry layer's per-call once-flag
// keep an EXCEED_TIME_WINDOW condition from looping or storming /v2/time. See
// syncer.go. The measurement primitive itself (MeasureClockOffset) stays in
// internal/apiclient; the Syncer takes a plain MeasureFunc the caller wires to it,
// preserving the no-import-cycle rule below.
//
// # Dependency rule (no cycle with package apiclient)
//
// This package MUST NOT import internal/apiclient, and internal/apiclient MUST NOT
// import this package. The measurement primitive (MeasureClockOffset, the
// ClockOffset type) stays in apiclient; the L1 apiclient.Client takes a plain
// `func() int64` (wired from State.SignNow) rather than a *State. Install
// therefore takes plain int64s — the caller converts from apiclient.ClockOffset
// (offsetMs = off.OffsetMs, leanMs = off.UncertaintyMs()) at the wiring seam.
package clock
