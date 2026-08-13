// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"sync"
	"testing"
)

// noticeRec collects the notices a health detector emits.
type noticeRec struct {
	mu  sync.Mutex
	got []Notice
}

func (r *noticeRec) fn(code NoticeCode, level Level, msg string, details map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, Notice{Code: code, Level: level, Message: msg, Details: details})
}

func (r *noticeRec) codes() []NoticeCode {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NoticeCode, len(r.got))
	for i, n := range r.got {
		out[i] = n.Code
	}
	return out
}

// levelOf returns the level of the first recorded notice with code, or "" if
// none — used to assert a recovery edge shares its onset's level.
func (r *noticeRec) levelOf(code NoticeCode) Level {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.got {
		if n.Code == code {
			return n.Level
		}
	}
	return ""
}

func healthTunables() Tunables {
	return Tunables{
		DelayWarnMs:           10_000,
		UnreliableRTTMs:       1_000,
		UnreliableDisconnects: 3,
		UnreliableWindowMs:    60_000,
		NoticeMinIntervalMs:   30_000,
	}.withDefaults()
}

// A delayed frame raises DATA_DELAYED, and a later current frame raises the
// DATA_CURRENT recovery edge exactly once.
func TestDelayCheckRecovers(t *testing.T) {
	rec := &noticeRec{}
	dc := &delayCheck{endpoint: "public", tun: healthTunables(), offsetMs: func() int64 { return 0 }}

	now := int64(1_000_000)
	// Frame 20s behind → delayed.
	dc.observe(now-20_000, now, rec.fn)
	// A current frame well under the threshold → recovered.
	dc.observe(now, now, rec.fn)
	// Another current frame → no repeat recovery.
	dc.observe(now, now, rec.fn)

	want := []NoticeCode{DataDelayed, DataCurrent}
	if got := rec.codes(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("delayCheck notices = %v, want %v", got, want)
	}
	// The recovery edge shares its onset's level so one --stream-log-level
	// threshold catches both the warning and its clear.
	if l := rec.levelOf(DataCurrent); l != LevelWarn {
		t.Fatalf("DATA_CURRENT level = %q, want warn (matching DATA_DELAYED)", l)
	}
}

// A frame hovering just under the threshold (but above the hysteresis floor)
// does not yet recover.
func TestDelayCheckHysteresis(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	dc := &delayCheck{endpoint: "public", tun: tun, offsetMs: func() int64 { return 0 }}

	now := int64(1_000_000)
	dc.observe(now-20_000, now, rec.fn) // delayed
	// delay just under the warn threshold but above DelayWarnMs/2 → still warned.
	dc.observe(now-(tun.DelayWarnMs-1), now, rec.fn)
	if got := rec.codes(); len(got) != 1 || got[0] != DataDelayed {
		t.Fatalf("expected only DATA_DELAYED, got %v", got)
	}
}

// A channel that goes silent while delayed clears the warning once the silence
// reaches the keepalive threshold (frame-driven recovery would otherwise never
// fire). The sweep is the connection ping loop's responsibility.
func TestDelayCheckSweepsOnSilence(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	dc := &delayCheck{endpoint: "public", tun: tun, offsetMs: func() int64 { return 0 }}

	now := int64(1_000_000)
	dc.observe(now-20_000, now, rec.fn) // delayed; lastObserve = now
	// A sweep before the silence threshold does nothing.
	dc.sweep(now+tun.KeepaliveAfterMs-1, rec.fn)
	if got := rec.codes(); len(got) != 1 || got[0] != DataDelayed {
		t.Fatalf("premature sweep fired: %v", got)
	}
	// A sweep past the silence threshold clears the standing warning.
	dc.sweep(now+tun.KeepaliveAfterMs+1, rec.fn)
	if got := rec.codes(); len(got) != 2 || got[1] != DataCurrent {
		t.Fatalf("silence sweep did not clear: %v", got)
	}
	// A further sweep does not re-fire.
	dc.sweep(now+2*tun.KeepaliveAfterMs, rec.fn)
	if got := rec.codes(); len(got) != 2 {
		t.Fatalf("sweep re-fired after clearing: %v", got)
	}
	// After the clear, a fresh late frame re-arms the warning.
	late := now + 3*tun.KeepaliveAfterMs
	dc.observe(late-20_000, late, rec.fn)
	if got := rec.codes(); len(got) != 3 || got[2] != DataDelayed {
		t.Fatalf("a late frame after a silence-clear should re-raise DATA_DELAYED: %v", got)
	}
}

// A fresh delay episode after a recovery must re-raise DATA_DELAYED even within
// the repeat rate-limit window — otherwise the warning latch sets silently and
// the consumer's flag/tag never reflects the active delay.
func TestDelayCheckReWarnsAfterRecoveryWithinRateLimit(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	dc := &delayCheck{endpoint: "public", tun: tun, offsetMs: func() int64 { return 0 }}

	now := int64(1_000_000)
	dc.observe(now-20_000, now, rec.fn) // delayed → DATA_DELAYED (lastNotice = now)
	dc.observe(now+1, now+1, rec.fn)    // current → DATA_CURRENT
	// A new delay episode well within NoticeMinIntervalMs of the first warning.
	dc.observe(now+2-20_000, now+2, rec.fn)

	want := []NoticeCode{DataDelayed, DataCurrent, DataDelayed}
	if got := rec.codes(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("delayCheck notices = %v, want %v", got, want)
	}
}

// A continuously-late feed emits DATA_DELAYED once, then suppresses repeats
// within the rate-limit window (the rising-edge bypass must not fire per frame).
func TestDelayCheckRepeatSuppressedWithinWindow(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	dc := &delayCheck{endpoint: "public", tun: tun, offsetMs: func() int64 { return 0 }}

	now := int64(1_000_000)
	dc.observe(now-20_000, now, rec.fn)             // DATA_DELAYED
	dc.observe(now+1_000-20_000, now+1_000, rec.fn) // still late, within the window
	dc.observe(now+2_000-20_000, now+2_000, rec.fn) // still late, within the window

	if got := rec.codes(); len(got) != 1 || got[0] != DataDelayed {
		t.Fatalf("repeat warnings within the window should be suppressed: %v", got)
	}
}

// A delay measured against an UNMEASURED clock still warns (we never suppress a
// real "seeing the past" signal) AND kicks a one-off resync to disambiguate skew
// from genuine lag — but only on the rate-limited warn edge, not per frame. Once
// the clock reports measured, no further kick fires (a known offset is trusted).
func TestDelayCheckKicksResyncWhileUnmeasured(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	var measured bool
	var kicks int
	dc := &delayCheck{
		endpoint: "public", tun: tun,
		offsetMs: func() int64 { return 0 },
		measured: func() bool { return measured },
		kickSync: func() { kicks++ },
	}

	now := int64(1_000_000)
	dc.observe(now-20_000, now, rec.fn) // delayed → DATA_DELAYED + one kick (unmeasured)
	// Still late within the rate-limit window: no repeat warn, so no repeat kick.
	dc.observe(now+1_000-20_000, now+1_000, rec.fn)
	dc.observe(now+2_000-20_000, now+2_000, rec.fn)
	if kicks != 1 {
		t.Fatalf("kicks = %d, want 1 (one per warn edge, not per frame)", kicks)
	}
	if got := rec.codes(); len(got) != 1 || got[0] != DataDelayed {
		t.Fatalf("notices = %v, want [DATA_DELAYED]", got)
	}

	// The resync landed — the clock is now measured. A fresh warn edge past the
	// rate-limit window still WARNS (the feed is genuinely late) but must NOT
	// re-probe: a known offset is trustworthy.
	measured = true
	late := now + tun.NoticeMinIntervalMs + 1
	dc.observe(late-20_000, late, rec.fn)
	if kicks != 1 {
		t.Fatalf("kicks = %d after measured, want 1 (no re-probe once measured)", kicks)
	}
	if got := rec.codes(); len(got) != 2 || got[1] != DataDelayed {
		t.Fatalf("notices = %v, want a repeat DATA_DELAYED after the window", got)
	}
}

// A feed that flaps across the delay threshold re-warns on each rising edge, but
// the resync kick is rate-limited by its OWN clock — it must NOT re-probe on
// every up-crossing while the clock stays unmeasured (a failing /v2/time never
// cools down, so the warn edge alone is not a safe rate limit).
func TestDelayCheckKickRateLimitedAcrossFlaps(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	var kicks int
	dc := &delayCheck{
		endpoint: "public", tun: tun,
		offsetMs: func() int64 { return 0 },
		measured: func() bool { return false }, // probe keeps failing → never measured
		kickSync: func() { kicks++ },
	}

	now := int64(1_000_000)
	// Flap late → current → late → current …: each "late" is a fresh rising warn
	// edge (the intervening recovery resets warned), all inside one
	// NoticeMinIntervalMs window. Without a dedicated kick rate-limit this would
	// kick on every up-crossing.
	for i := int64(0); i < 5; i++ {
		t0 := now + i*100
		dc.observe(t0-20_000, t0, rec.fn) // late → rising warn edge
		dc.observe(t0+1, t0+1, rec.fn)    // current → recover (warned reset)
	}
	if kicks != 1 {
		t.Fatalf("kicks = %d across flaps within the window, want 1 (rate-limited by lastKick, not the warn edge)", kicks)
	}
}

// Under --time-sync off the delayCheck has no kickSync/measured wiring: a delay
// still warns (unsuppressed), and observe must not panic on the nil hooks.
func TestDelayCheckWarnsWithoutKickHooks(t *testing.T) {
	rec := &noticeRec{}
	dc := &delayCheck{endpoint: "public", tun: healthTunables(), offsetMs: func() int64 { return 0 }}
	now := int64(1_000_000)
	dc.observe(now-20_000, now, rec.fn)
	if got := rec.codes(); len(got) != 1 || got[0] != DataDelayed {
		t.Fatalf("notices = %v, want [DATA_DELAYED] with no kick hooks", got)
	}
}

// A disconnect while an RTT-driven CONNECTION_UNRELIABLE is standing must NOT
// emit CONNECTION_STABLE: the missing RTT sample is not evidence of recovery.
func TestConnHealthDisconnectDoesNotFalselyStabilize(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	h := &connHealth{endpoint: "public", tun: tun}

	now := int64(1_000_000)
	h.recordRTT(tun.UnreliableRTTMs+500, now, rec.fn) // slow → unreliable
	h.onDisconnected(now+1, rec.fn)                   // a drop before any good ping

	if got := rec.codes(); len(got) != 1 || got[0] != ConnectionUnreliable {
		t.Fatalf("a disconnect after high RTT must not stabilize: %v", got)
	}

	// A live good-RTT ping after the drop window decays is what legitimately
	// recovers.
	h.recordRTT(10, now+1+tun.UnreliableWindowMs+1, rec.fn)
	if got := rec.codes(); len(got) != 2 || got[1] != ConnectionStable {
		t.Fatalf("a live good ping should stabilize: %v", got)
	}
}

// A high RTT raises CONNECTION_UNRELIABLE; a later good RTT raises the
// CONNECTION_STABLE recovery edge exactly once.
func TestConnHealthRTTRecovers(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	h := &connHealth{endpoint: "public", tun: tun}

	now := int64(1_000_000)
	h.recordRTT(tun.UnreliableRTTMs+500, now, rec.fn) // slow → unreliable
	h.recordRTT(10, now+1, rec.fn)                    // fast → stable
	h.recordRTT(10, now+2, rec.fn)                    // still fast → no repeat

	want := []NoticeCode{ConnectionUnreliable, ConnectionStable}
	if got := rec.codes(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("RTT notices = %v, want %v", got, want)
	}
	// Recovery edge shares its onset's level (warn).
	if l := rec.levelOf(ConnectionStable); l != LevelWarn {
		t.Fatalf("CONNECTION_STABLE level = %q, want warn (matching CONNECTION_UNRELIABLE)", l)
	}
}

// Enough drops inside the window raise CONNECTION_UNRELIABLE; once they age out
// of the window a later evaluation (the ping cycle) raises CONNECTION_STABLE
// even though no further disconnect occurred.
func TestConnHealthDropWindowRecovers(t *testing.T) {
	rec := &noticeRec{}
	tun := healthTunables()
	h := &connHealth{endpoint: "private", tun: tun}

	now := int64(1_000_000)
	h.onDisconnected(now, rec.fn)
	h.onDisconnected(now+1, rec.fn)
	h.onDisconnected(now+2, rec.fn) // third drop reaches the threshold
	if got := rec.codes(); len(got) != 1 || got[0] != ConnectionUnreliable {
		t.Fatalf("after the drop storm, notices = %v, want [CONNECTION_UNRELIABLE]", got)
	}

	// A ping after the window has fully elapsed prunes the drops → recovered.
	h.recordRTT(10, now+2+tun.UnreliableWindowMs+1, rec.fn)
	got := rec.codes()
	if len(got) != 2 || got[1] != ConnectionStable {
		t.Fatalf("after the window cleared, notices = %v, want trailing CONNECTION_STABLE", got)
	}
}

// A single disconnect (below the threshold) raises nothing — neither a warning
// nor a spurious recovery.
func TestConnHealthSingleDropSilent(t *testing.T) {
	rec := &noticeRec{}
	h := &connHealth{endpoint: "public", tun: healthTunables()}
	h.onDisconnected(1_000_000, rec.fn)
	if got := rec.codes(); len(got) != 0 {
		t.Fatalf("a single drop should be silent, got %v", got)
	}
}
