// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"log/slog"
	"sync"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// DefaultCoolDownMs is the minimum spacing between server-clock measurements a
// Syncer enforces (see Syncer). A successful estimate is reused for this long
// before another trigger re-probes, so a persistently rejected signer cannot
// hammer /v2/time. It is short enough that a genuinely needed correction after
// the window is barely delayed, and the shared estimate means the first
// successful resync already fixes signing for every surface.
const DefaultCoolDownMs = 2000

// MeasureFunc probes the server clock and returns the offset
// (serverClock − localClock) and the lean (the measurement uncertainty,
// RTTmin/2) to install. It performs network I/O and is called by Sync WITHOUT
// any Syncer lock held. Returning an error leaves the previous estimate
// untouched. It is the seam that keeps this package free of any korbit import:
// the caller wraps apiclient.MeasureClockOffset and converts the result to plain
// int64s (offsetMs = off.OffsetMs, leanMs = off.UncertaintyMs()).
type MeasureFunc func() (offsetMs, leanMs int64, err error)

// Syncer is the single home for server-clock measurement in a process: one
// shared State plus the one measurement operation that installs into it. It is
// the central timekeeping instance — every signing surface (REST signing, the
// WebSocket upgrade, the place protocol's resync) shares ONE Syncer, so a
// correction discovered on any of them is instantly visible to all.
//
// Two guards keep an EXCEED_TIME_WINDOW condition from looping or storming
// /v2/time:
//
//   - Single-flight: concurrent Sync calls collapse onto ONE in-flight probe;
//     the late callers wait for it and share its result. A burst of
//     simultaneously rejected signed calls (e.g. a Promise.all of placements)
//     therefore triggers a single measurement, not one per call.
//   - Cooldown: a successful estimate is reused (no re-probe) for coolDownMs.
//     Rapid sequential rejections re-measure at most once per window. The
//     estimate is not given up — a trigger after the window re-probes — so a
//     clock that legitimately drifts over a long session is still corrected.
//
// These complement the per-call once-flag in the retry layer (which caps each
// logical call to a single resync). Together: at most one resync per call, and
// at most one measurement per cooldown window across the whole process.
//
// A Syncer is safe for concurrent use.
type Syncer struct {
	state      *State
	measure    MeasureFunc
	now        func() int64
	coolDownMs int64

	mu         sync.Mutex
	inflight   *syncCall
	lastSyncAt int64 // local-clock ms of the last SUCCESSFUL measurement
	everSynced bool

	// Log is the optional operational logger for sync decisions: single-flight
	// share and cooldown reuse at Debug, a new estimate installed at Info, a
	// measurement failure at Warn. nil = silent. The probe-level RTT/offset
	// telemetry lives in the MeasureFunc (the caller threads a logger into
	// apiclient.MeasureClockOffset). It carries no secret.
	Log *slog.Logger
}

// log returns the syncer's logger, or a no-op when unwired.
func (s *Syncer) log() *slog.Logger { return logging.Or(s.Log) }

// syncCall is one in-flight measurement shared by single-flight callers. err is
// written once (before done is closed) and read only after the close, so the
// close→receive synchronization makes the read race-free without a lock.
type syncCall struct {
	done chan struct{}
	err  error
}

// NewSyncer builds a Syncer over state, measuring with measure. now is the local
// clock (unix ms) used only for the cooldown spacing; pass the same clock the
// rest of the process uses (injectable for tests). coolDownMs <= 0 disables the
// cooldown (every non-concurrent trigger re-probes); production wires
// DefaultCoolDownMs.
func NewSyncer(state *State, measure MeasureFunc, now func() int64, coolDownMs int64) *Syncer {
	return &Syncer{state: state, measure: measure, now: now, coolDownMs: coolDownMs}
}

// State returns the shared estimate the Syncer installs into — the clock to sign
// against (State.SignNow) and read the offset/recvWindow from.
func (s *Syncer) State() *State { return s.state }

// Sync measures the server clock and installs the estimate, subject to
// single-flight and the cooldown. It returns nil when it reused a recent
// estimate (no probe) or after a successful measurement, and the measurement
// error otherwise. A failed measurement leaves the previous estimate in place
// and does NOT start the cooldown, so the next trigger re-probes immediately
// rather than reusing a stale (or never-measured) estimate.
func (s *Syncer) Sync() error {
	log := s.log()
	s.mu.Lock()
	if c := s.inflight; c != nil {
		// A measurement is already running: wait for it and share its result
		// instead of starting a redundant probe.
		s.mu.Unlock()
		log.Debug("clock sync: joining in-flight probe")
		<-c.done
		return c.err
	}
	if s.everSynced && s.coolDownMs > 0 && s.now()-s.lastSyncAt < s.coolDownMs {
		// Measured recently enough — reuse the installed estimate.
		ageMs := s.now() - s.lastSyncAt
		s.mu.Unlock()
		log.Debug("clock sync: reusing recent estimate", "ageMs", ageMs, "coolDownMs", s.coolDownMs)
		return nil
	}
	c := &syncCall{done: make(chan struct{})}
	s.inflight = c
	s.mu.Unlock()

	// Finalize even if measure() panics (e.g. a panicking injected Doer): clearing
	// inflight and closing done must happen, or every joined/subsequent caller
	// would deadlock waiting on a measurement that never completes. The panic
	// still propagates — it is not recovered, only made non-deadlocking.
	defer func() {
		s.mu.Lock()
		s.inflight = nil
		s.mu.Unlock()
		close(c.done)
	}()

	offsetMs, leanMs, err := s.measure()

	s.mu.Lock()
	if err == nil {
		s.state.Install(offsetMs, leanMs)
		s.lastSyncAt = s.now()
		s.everSynced = true
	}
	c.err = err
	s.mu.Unlock()

	if err != nil {
		log.Warn("clock sync failed; keeping previous estimate", "err", err.Error())
		return err
	}
	// Notable-normal milestone: the shared estimate every signing surface now uses.
	log.Info("clock corrected", "offsetMs", offsetMs, "leanMs", leanMs)
	if rw := s.state.RecvWindowMs(); rw > defaultRecvWindowMs {
		log.Info("recvWindow auto-widened", "recvWindowMs", rw)
	}
	return err
}
