// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"fmt"
	"sync"
	"time"
)

// Tunables are the session's timing and threshold knobs. The zero value is
// not usable; call (&Tunables{}).withDefaults() — Config does this. Every
// knob is injectable so tests can run the real machinery in milliseconds.
type Tunables struct {
	// DialTimeoutMs bounds one WebSocket dial (TCP+TLS+upgrade).
	DialTimeoutMs int64
	// WriteTimeoutMs bounds one outbound write (the subscribe batch).
	WriteTimeoutMs int64
	// ReconnectMinMs/ReconnectMaxMs bound the jittered exponential backoff
	// between reconnect attempts.
	ReconnectMinMs int64
	ReconnectMaxMs int64
	// StableAfterMs is how long a connection must stay up for the backoff
	// ladder to reset.
	StableAfterMs int64
	// PingIntervalMs is how often a protocol ping measures RTT and probes for
	// half-open connections; PongTimeoutMs is how long to wait for the pong
	// before declaring the connection dead.
	PingIntervalMs int64
	PongTimeoutMs  int64
	// KeepaliveAfterMs: with no data for this long, a KEEPALIVE notice is
	// emitted (and repeated each further interval of silence).
	KeepaliveAfterMs int64
	// DelayWarnMs: a frame whose server timestamp lags local receipt by more
	// than this triggers a DATA_DELAYED notice.
	DelayWarnMs int64
	// UnreliableRTTMs: an RTT sample above this counts the connection as
	// degraded.
	UnreliableRTTMs int64
	// UnreliableDisconnects within UnreliableWindowMs triggers
	// CONNECTION_UNRELIABLE.
	UnreliableDisconnects int
	UnreliableWindowMs    int64
	// NoticeMinIntervalMs rate-limits the repeatable warnings (DATA_DELAYED,
	// CONNECTION_UNRELIABLE) per connection.
	NoticeMinIntervalMs int64
	// BackfillRetryMinMs/BackfillRetryMaxMs bound the jittered exponential
	// backoff between self-heal re-runs of a private backfill pass whose
	// leaf calls stayed transiently failed (retryFailedBackfill). Distinct from
	// Config.BackfillRetryBudgetMs, which bounds the in-call retry ladder of
	// ONE REST call; these pace the pass-level re-runs after that ladder gave
	// up, while the connection stays up.
	BackfillRetryMinMs int64
	BackfillRetryMaxMs int64
}

func (t Tunables) withDefaults() Tunables {
	def := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&t.DialTimeoutMs, 10_000)
	def(&t.WriteTimeoutMs, 10_000)
	def(&t.ReconnectMinMs, 250)
	def(&t.ReconnectMaxMs, 15_000)
	def(&t.StableAfterMs, 30_000)
	def(&t.PingIntervalMs, 15_000)
	def(&t.PongTimeoutMs, 5_000)
	def(&t.KeepaliveAfterMs, 5*60_000)
	def(&t.DelayWarnMs, 10_000)
	def(&t.UnreliableRTTMs, 2_000)
	def(&t.UnreliableWindowMs, 5*60_000)
	def(&t.NoticeMinIntervalMs, 30_000)
	def(&t.BackfillRetryMinMs, 5_000)
	def(&t.BackfillRetryMaxMs, 120_000)
	if t.UnreliableDisconnects <= 0 {
		t.UnreliableDisconnects = 3
	}
	return t
}

// connHealth tracks one connection's quality: recent disconnect times and RTT
// samples. It raises CONNECTION_UNRELIABLE (rate-limited) when either crosses
// its threshold and CONNECTION_STABLE once both have recovered, latching
// `unreliable` so the recovery edge fires exactly once. It never blocks the
// hot path.
//
// Recovery is driven by the live connection: a drop burst clears once it ages
// out of the window on a later ping (recordRTT) or disconnect, and a slow RTT
// clears on the next good sample. A connection that never comes back (a fatal
// stop) therefore keeps its standing CONNECTION_UNRELIABLE — its down state is
// the controlling signal at that point.
type connHealth struct {
	endpoint string
	tun      Tunables

	mu          sync.Mutex
	disconnects []int64 // unix-ms times of recent disconnects
	lastNotice  int64
	unreliable  bool // a CONNECTION_UNRELIABLE is currently standing
}

type noticeFn func(code NoticeCode, level Level, msg string, details map[string]any)

func (h *connHealth) onConnected() {}

// onDisconnected records a drop and re-evaluates connection health.
func (h *connHealth) onDisconnected(nowMs int64, notice noticeFn) {
	h.mu.Lock()
	h.disconnects = append(h.disconnects, nowMs)
	h.mu.Unlock()
	h.evaluate(nowMs, 0, false, notice)
}

// recordRTT files one ping round-trip sample and re-evaluates connection
// health. The ping loop calls this every interval while connected, so it is
// also what notices recovery: a drop burst that has aged out of the window
// clears here even though no further disconnect occurs.
func (h *connHealth) recordRTT(rttMs, nowMs int64, notice noticeFn) {
	h.evaluate(nowMs, rttMs, true, notice)
}

// evaluate recomputes health from the pruned disconnect window plus an
// optional fresh RTT sample. It raises CONNECTION_UNRELIABLE on the degraded
// edge (and on rate-limited repeats while still degraded) and CONNECTION_STABLE
// once neither cause holds. Either cause — too many recent drops, or a single
// slow RTT — counts as degraded.
func (h *connHealth) evaluate(nowMs, rttMs int64, haveRTT bool, notice noticeFn) {
	h.mu.Lock()
	cutoff := nowMs - h.tun.UnreliableWindowMs
	kept := h.disconnects[:0]
	for _, t := range h.disconnects {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	h.disconnects = kept
	count := len(h.disconnects)

	dropBad := count >= h.tun.UnreliableDisconnects
	rttBad := haveRTT && rttMs > h.tun.UnreliableRTTMs
	degraded := dropBad || rttBad

	var fireWarn, fireStable bool
	var msg string
	var details map[string]any
	switch {
	case degraded:
		// Fire on the degraded edge regardless of rate limit, and on later
		// repeats only once the min interval has passed.
		if !h.unreliable || nowMs-h.lastNotice >= h.tun.NoticeMinIntervalMs {
			fireWarn = true
			h.lastNotice = nowMs
			msg, details = h.unreliableNotice(count, rttMs, dropBad, rttBad)
		}
		h.unreliable = true
	case h.unreliable && haveRTT:
		// Only a live ping (haveRTT) may declare recovery: it proves the
		// connection is alive AND that RTT is back under the threshold. A
		// disconnect (haveRTT=false) can only add degradation, never clear it —
		// otherwise a drop while RTT-degraded would announce "stable" with RTT
		// never having recovered.
		h.unreliable = false
		fireStable = true
	}
	h.mu.Unlock()

	if fireWarn {
		notice(ConnectionUnreliable, LevelWarn, msg, details)
	}
	if fireStable {
		// A recovery edge carries the SAME level as the onset it clears
		// (CONNECTION_UNRELIABLE, warn), so a level threshold that catches the
		// degradation also catches its resolution — never a warning with no
		// visible "all clear".
		notice(ConnectionStable, LevelWarn,
			fmt.Sprintf("%s websocket connection is back to normal", h.endpoint),
			map[string]any{"endpoint": h.endpoint})
	}
}

// unreliableNotice builds the CONNECTION_UNRELIABLE message and details for the
// cause(s) currently tripping. Called with h.mu held.
func (h *connHealth) unreliableNotice(count int, rttMs int64, dropBad, rttBad bool) (string, map[string]any) {
	details := map[string]any{"endpoint": h.endpoint}
	switch {
	case dropBad && rttBad:
		details["disconnects"] = count
		details["windowMs"] = h.tun.UnreliableWindowMs
		details["rttMs"] = rttMs
		return fmt.Sprintf("%s websocket has dropped %d times in the last %s and round-trip time is %dms — the connection is unreliable",
			h.endpoint, count, msToHuman(h.tun.UnreliableWindowMs), rttMs), details
	case rttBad:
		details["rttMs"] = rttMs
		return fmt.Sprintf("%s websocket round-trip time is %dms — the connection is degraded", h.endpoint, rttMs), details
	default:
		details["disconnects"] = count
		details["windowMs"] = h.tun.UnreliableWindowMs
		return fmt.Sprintf("%s websocket has dropped %d times in the last %s — the connection is unreliable",
			h.endpoint, count, msToHuman(h.tun.UnreliableWindowMs)), details
	}
}

// delayCheck raises DATA_DELAYED (rate-limited) when a frame's server
// timestamp lags local receipt beyond the threshold, and DATA_CURRENT once a
// later frame is current again. Recovery is frame-driven: it is observed only
// when a frame arrives, so a channel that goes silent while delayed keeps the
// standing warning until its next frame (or until a reconnect, which the state
// layer treats as clearing the delay).
//
// The measurement is only as good as the server-clock offset: a skewed local
// clock (offset unmeasured) shifts it and can trip a false DATA_DELAYED. The
// warning is NOT suppressed to hide that: dropping a real "you are seeing the
// past" signal is worse for a trading agent than a self-correcting false alarm.
// Instead a suspected delay against an unmeasured clock triggers a one-off
// reactive resync (kickSync), rate-limited by its own clock. Once the offset
// lands, a genuinely late feed re-warns against the corrected delay and a false
// alarm clears via DATA_CURRENT. Under --time-sync on the clock is normally
// measured before the first frame, so this path is usually inert — but if that
// startup measurement failed the clock stays unmeasured and the kick still
// covers it. Under off kickSync is nil and the warning stands against the local
// clock (--time-sync off opts out of correction).
type delayCheck struct {
	endpoint string
	tun      Tunables
	// offsetMs returns the current (serverClock - localClock) estimate, 0 when
	// unmeasured.
	offsetMs func() int64
	// measured reports whether the shared clock has a measured estimate yet, and
	// kickSync triggers a one-off, non-blocking server-clock resync (see observe).
	// kickSync is wired only when the client can resync — nil under --time-sync off
	// (no clock correction at all). measured is always wired alongside a non-nil
	// kickSync, so observe may read measured() whenever kickSync is non-nil; both
	// are nil only in tests that construct a delayCheck directly.
	measured func() bool
	kickSync func()

	mu          sync.Mutex
	lastNotice  int64
	lastKick    int64 // local time of the most recent delay-triggered resync kick
	lastObserve int64 // local time of the most recent frame seen
	warned      bool  // a DATA_DELAYED is currently standing
}

func (d *delayCheck) observe(frameServerTime, nowMs int64, notice noticeFn) {
	if frameServerTime <= 0 {
		return
	}
	delay := nowMs + d.offsetMs() - frameServerTime

	var fireWarn, fireRecover, fireKick bool
	d.mu.Lock()
	d.lastObserve = nowMs
	switch {
	case delay > d.tun.DelayWarnMs:
		// Fire on the rising edge (a fresh delay episode) regardless of rate
		// limit, and on later repeats only once the min interval has passed —
		// otherwise a re-delay within the window would set the latch silently,
		// leaving a standing warning with no notice (and no consumer-side flag).
		if !d.warned || nowMs-d.lastNotice >= d.tun.NoticeMinIntervalMs {
			fireWarn = true
			d.lastNotice = nowMs
		}
		d.warned = true
		// If the clock is still unmeasured, this "delay" may be pure local-clock
		// skew rather than real lag. Kick a one-off resync to disambiguate (the
		// --time-sync auto reactive path): a genuinely late feed re-warns against
		// the corrected offset, a skewed clock clears via DATA_CURRENT. Rate-limited
		// by its OWN clock (lastKick) — NOT the warn edge, which fires on every
		// rising edge regardless of interval, and NOT the shared Syncer's cooldown,
		// which a *failed* probe never arms; leaning on either would let a flapping
		// feed re-probe a degraded /v2/time far more often than intended. Gated to
		// the unmeasured state (a known offset is trustworthy — no reason to
		// re-probe). kickSync is nil under --time-sync off (warn but do not correct);
		// a non-nil kickSync always has measured wired, so calling it here is safe.
		if d.kickSync != nil && !d.measured() && nowMs-d.lastKick >= d.tun.NoticeMinIntervalMs {
			fireKick = true
			d.lastKick = nowMs
		}
	case d.warned && delay <= d.tun.DelayWarnMs/2:
		// Recover only well under the threshold (hysteresis) so a frame hovering
		// at the boundary does not flap between warn and recover.
		d.warned = false
		fireRecover = true
	}
	d.mu.Unlock()

	if fireWarn {
		notice(DataDelayed, LevelWarn,
			fmt.Sprintf("%s websocket data is arriving %s late — you are seeing the past", d.endpoint, msToHuman(delay)),
			map[string]any{"endpoint": d.endpoint, "delayMs": delay})
	}
	if fireKick {
		d.kickSync()
	}
	if fireRecover {
		// Recovery edge matches its onset (DATA_DELAYED, warn) — see ConnectionStable.
		notice(DataCurrent, LevelWarn,
			fmt.Sprintf("%s websocket data is current again", d.endpoint),
			map[string]any{"endpoint": d.endpoint, "delayMs": delay})
	}
}

// sweep clears a standing DATA_DELAYED when the feed has gone quiet: recovery is
// otherwise observed only when a frame arrives, so a channel that goes silent
// while delayed would keep the warning forever. Once the silence reaches the
// keepalive threshold (the point at which "no data" is itself notable) there is
// no current evidence of lag, so the warning clears. The connection's ping loop
// drives this while connected.
func (d *delayCheck) sweep(nowMs int64, notice noticeFn) {
	d.mu.Lock()
	fire := d.warned && d.lastObserve > 0 && nowMs-d.lastObserve >= d.tun.KeepaliveAfterMs
	if fire {
		d.warned = false
	}
	d.mu.Unlock()

	if fire {
		notice(DataCurrent, LevelWarn,
			fmt.Sprintf("%s websocket data has gone quiet — clearing the delayed warning", d.endpoint),
			map[string]any{"endpoint": d.endpoint})
	}
}

// keepalive emits a KEEPALIVE notice when no data event has been delivered
// for KeepaliveAfterMs, then keeps repeating per silent interval — so a
// watcher of the stream can tell "quiet market" from "dead pipe".
type keepalive struct {
	tun    Tunables
	now    func() int64
	notice noticeFn
	// status reports how many of the session's connections are currently up.
	status func() (up, total int)

	mu       sync.Mutex
	lastData int64
}

// touch records that a data event was just delivered.
func (k *keepalive) touch() {
	k.mu.Lock()
	k.lastData = k.now()
	k.mu.Unlock()
}

// run ticks at a fraction of the keepalive interval and fires when the
// silence crosses it. The check interval (1/4 of the threshold) bounds how
// late a keepalive can fire without needing a wakeable timer.
func (k *keepalive) run(done <-chan struct{}) {
	k.touch()
	interval := time.Duration(k.tun.KeepaliveAfterMs/4+1) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastFire int64
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		now := k.now()
		k.mu.Lock()
		silent := now - k.lastData
		k.mu.Unlock()
		if silent >= k.tun.KeepaliveAfterMs && now-lastFire >= k.tun.KeepaliveAfterMs {
			lastFire = now
			up, total := k.status()
			state := "there has just been no data to deliver"
			if up < total {
				state = fmt.Sprintf("NOTE: only %d of %d connections are up", up, total)
			}
			k.notice(Keepalive, LevelInfo,
				fmt.Sprintf("still running — no data for %s; %s", msToHuman(silent), state),
				map[string]any{"silentMs": silent, "connectionsUp": up, "connectionsTotal": total})
		}
	}
}

func msToHuman(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	return d.Truncate(100 * time.Millisecond).String()
}
