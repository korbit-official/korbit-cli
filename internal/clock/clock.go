// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package clock

import "sync"

// recvWindow limits. The server's default validity window is 5s and its
// maximum is 60s; the +1s future bound is fixed and cannot be widened (a fast
// local clock is unfixable with recvWindow — only a real clock fix or the
// past-lean helps).
const (
	defaultRecvWindowMs = 5000
	maxRecvWindowMs     = 60000
)

// State is one session's (or one process's) estimate of the Digital X server
// clock, shared by everything that signs against it: the WebSocket upgrade
// query, REST request signing, the EXCEED_TIME_WINDOW corrective resync, and
// delivery-delay measurement. It is mutex-guarded and safe for concurrent use.
//
// One State == one estimate. Sharing a single State across a session's WS
// upgrade signing, REST signing, and corrective resync is the whole point: a
// clock correction triggered by any one of them (e.g. a REST EXCEED_TIME_WINDOW
// resync) is immediately visible to the others, so the skew is measured once
// per session rather than re-discovered on every surface.
type State struct {
	now func() int64 // the local clock (unix ms); injectable for tests

	mu       sync.Mutex
	offsetMs int64 // serverClock - localClock
	leanMs   int64 // measurement uncertainty; signing leans into the past by this
	measured bool
}

// New builds a State over the given local clock. now must be non-nil (the unix
// ms reader); callers wire it to time.Now().UnixMilli or a test seam. Until
// Install is called the estimate is the bare local clock (offset 0, no lean).
func New(now func() int64) *State { return &State{now: now} }

// Install records a measured server-clock estimate. offsetMs is
// (serverClock - localClock); leanMs is the measurement uncertainty that
// SignNow leans the signed timestamp into the past by (so it can never cross
// the server's fixed +1s future bound). Callers convert these from a
// apiclient.ClockOffset (offsetMs = off.OffsetMs, leanMs = off.UncertaintyMs());
// the conversion lives at the call site so this package stays free of any
// apiclient import (no dependency cycle — see doc.go).
func (s *State) Install(offsetMs, leanMs int64) {
	s.mu.Lock()
	s.offsetMs = offsetMs
	s.leanMs = leanMs
	s.measured = true
	s.mu.Unlock()
}

// SignNow is the clock to sign with: the server-clock estimate leaned into the
// past by the measurement uncertainty, so a signed timestamp can never cross
// the server's fixed +1s future bound. Before Install it is the bare local
// clock.
func (s *State) SignNow() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now() + s.offsetMs - s.leanMs
}

// Offset is the raw (un-leaned) offset estimate, 0 until measured. Used to
// translate server-stamped frame timestamps into local terms (delivery-delay
// measurement) and to stamp backfill events with the server's notion of now.
func (s *State) Offset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offsetMs
}

// ServerNowMs is the best estimate of the server's current unix-ms time: the
// local clock plus the raw offset, UN-leaned (unlike SignNow, which leans into
// the past for the signing bound). It reads the same local-clock seam the
// estimate was measured against, so it honors a test clock. Before Install it is
// the bare local clock. Used to default server-relative lookback windows sent on
// the wire (the history walk's start).
func (s *State) ServerNowMs() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now() + s.offsetMs
}

// Measured reports whether an estimate has been installed yet.
func (s *State) Measured() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.measured
}

// RecvWindowMs returns the single signed-request validity window every signer
// includes — REST signing, the WebSocket upgrade, and the EXCEED_TIME_WINDOW
// corrective resync all read it, so the enforced window is identical on every
// surface. It is the widened window to include when the measured uncertainty is
// large enough that the past-lean could push a signed timestamp below the
// server's default `serverTime - 5000` lower bound, else 0 — the omit-below-5s
// convention: at or under the 5s default the param is omitted and the server
// applies its own 5s window (sending an explicit 5000 would enforce the same
// window). The widened value is 3×RTTmin (== 6×lean, since lean is RTTmin/2),
// capped at the server maximum of 60s. 0 until measured.
func (s *State) RecvWindowMs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.measured {
		return 0
	}
	w := s.rawRecvWindowMsLocked()
	if w <= defaultRecvWindowMs {
		return 0
	}
	return w
}

// rawRecvWindowMsLocked computes 3×RTTmin (== 6×lean) capped at the server max.
// Caller holds s.mu.
func (s *State) rawRecvWindowMsLocked() int {
	w := int(6 * s.leanMs) // 6*lean == 3*RTTmin (lean is RTTmin/2)
	if w > maxRecvWindowMs {
		w = maxRecvWindowMs
	}
	return w
}
