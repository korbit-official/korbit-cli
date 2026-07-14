// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package footer

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
)

// plain strips ANSI styling so a hint assertion reads the key text without the
// per-glyph styling the keystrip applies (each cap is styled separately, so
// "x:cancel" is several escape sequences on screen but one string once stripped).
func plain(s string) string { return ansi.Strip(s) }

func sampleData() Data {
	return Data{
		Health: state.Health{
			Public:    state.EndpointHealth{Known: true, Up: true},
			Private:   state.EndpointHealth{Known: true, Up: true},
			DataCount: 42,
		},
	}
}

func key() Key {
	return Key{
		HealthRev: 1,
		NoticeRev: 1,
		Private:   true,
		HasTrader: true,
		Focus:     FocusOrders,
		W:         120,
	}
}

func TestRendersTwoLinesAndDots(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if got := strings.Count(out, "\n") + 1; got != 2 {
		t.Fatalf("footer = %d lines, want 2: %q", got, out)
	}
	// Both endpoint dots and the event counter appear on line one.
	if !strings.Contains(out, "pub") || !strings.Contains(out, "prv") {
		t.Errorf("line one should show both connection dots; got: %q", out)
	}
	if !strings.Contains(out, "events:42") {
		t.Errorf("line one should show the event counter; got: %q", out)
	}
	// The orders-focus hint line shows the cancel and cancel-all keys.
	if p := plain(out); !strings.Contains(p, "x:cancel") || !strings.Contains(p, "X:cancel-all") {
		t.Errorf("orders-focus hint should list cancel keys; got: %q", p)
	}
}

// When an overlay owns the keyboard, the footer must not advertise the dead
// main-screen keys; it shows only a plain (non-clickable) reminder of the one
// globally-valid key.
func TestOverlayHidesMainKeys(t *testing.T) {
	m := New()
	k := key() // orders focus
	k.OverlayOpen = true
	out := plain(m.View(k, sampleData()))
	if !strings.Contains(out, "esc to close overlay") || !strings.Contains(out, "ctrl+c to quit") {
		t.Errorf("overlay footer should remind of esc-close and ctrl+c-quit; got: %q", out)
	}
	for _, dead := range []string{"x:cancel", "tab:focus", "X:cancel-all", "n:notices"} {
		if strings.Contains(out, dead) {
			t.Errorf("overlay footer must not advertise the dead main key %q; got: %q", dead, out)
		}
	}
	// The reminder is prose, not a clickable cap — so a footer-row click can't
	// accidentally fire anything while an overlay is open.
	if hits := m.Hits(k); len(hits) != 0 {
		t.Errorf("overlay footer should expose no clickable caps, got %d", len(hits))
	}
}

func TestPublicModeHint(t *testing.T) {
	m := New()
	k := key()
	k.Private = false
	k.HasTrader = false
	out := m.View(k, Data{Health: state.Health{Public: state.EndpointHealth{Known: true, Up: true}}})
	if !strings.Contains(out, "public mode") {
		t.Errorf("public mode should show the order-entry-disabled hint; got: %q", out)
	}
	if strings.Contains(out, "prv") {
		t.Errorf("public mode should not show the private dot; got: %q", out)
	}
}

func TestLoadingDotsBeforeConnect(t *testing.T) {
	m := New()
	k := key()
	d := Data{Health: state.Health{Public: state.EndpointHealth{Known: false}, Private: state.EndpointHealth{Known: false}}}
	out := m.View(k, d)
	// Unknown endpoints draw the dim ○ glyph rather than the connected ●.
	if !strings.Contains(out, "○") {
		t.Errorf("an unconnected endpoint should draw the ○ glyph; got: %q", out)
	}
}

func TestNoticeShownWhenNotInfo(t *testing.T) {
	m := New()
	k := key()
	d := sampleData()
	d.HasNotice = true
	d.NoticeCode = stream.NoticeCode("DATA_GAP")
	d.NoticeMsg = "a gap was detected"
	d.NoticeLvl = stream.LevelWarn
	out := m.View(k, d)
	if !strings.Contains(out, "DATA_GAP") {
		t.Errorf("a non-info notice should appear on line one; got: %q", out)
	}
}

func TestDegradationTagsRenderFromHealth(t *testing.T) {
	m := New()
	d := sampleData()
	d.Health.Public.Delayed = true
	d.Health.Private.Unreliable = true
	out := m.View(key(), d)
	if !strings.Contains(out, "delayed") {
		t.Errorf("a delayed endpoint should show the 'delayed' tag; got: %q", out)
	}
	if !strings.Contains(out, "unreliable") {
		t.Errorf("an unreliable endpoint should show the 'unreliable' tag; got: %q", out)
	}
}

func TestDegradationTagsHiddenWhenEndpointDown(t *testing.T) {
	m := New()
	d := sampleData()
	// A down endpoint that still carries an Unreliable flag (it never recovered
	// before dropping) must not strand the tag — the down dot is the signal.
	d.Health.Private = state.EndpointHealth{Known: true, Up: false, Unreliable: true, Delayed: true}
	out := m.View(key(), d)
	if strings.Contains(out, "unreliable") {
		t.Errorf("a down endpoint should not show the 'unreliable' tag; got: %q", out)
	}
	if strings.Contains(out, "delayed") {
		t.Errorf("a down endpoint should not show the 'delayed' tag; got: %q", out)
	}
}

func TestFlagBackedNoticeNotStuckInSlot(t *testing.T) {
	m := New()
	d := sampleData()
	// CONNECTION_UNRELIABLE is represented by the self-clearing tag, so it must
	// not also occupy the sticky latest-notice slot (where it would linger after
	// the tag cleared).
	d.HasNotice = true
	d.NoticeCode = stream.ConnectionUnreliable
	d.NoticeMsg = "the connection is unreliable"
	d.NoticeLvl = stream.LevelWarn
	out := m.View(key(), d)
	if strings.Contains(out, "the connection is unreliable") {
		t.Errorf("a flag-backed warning should not appear in the latest-notice slot; got: %q", out)
	}
}

// The recovery edges (and a recovery CONNECTED) now carry warn, so without the
// taggedStatusCode additions they would leak into the latest-notice slot. Their
// state is shown by the self-clearing tags / connection dot, so the slot must
// stay clear of them.
func TestRecoveryEdgeNoticesNotInSlot(t *testing.T) {
	for _, c := range []struct {
		code stream.NoticeCode
		msg  string
	}{
		{stream.DataCurrent, "data is current again"},
		{stream.ConnectionStable, "connection is back to normal"},
		{stream.Connected, "reconnected after 800ms"},
	} {
		m := New()
		d := sampleData()
		d.HasNotice = true
		d.NoticeCode = c.code
		d.NoticeMsg = c.msg
		d.NoticeLvl = stream.LevelWarn
		out := m.View(key(), d)
		if strings.Contains(out, c.msg) {
			t.Errorf("%s (warn recovery) should not occupy the latest-notice slot; got: %q", c.code, out)
		}
	}
}

func TestToastReplacesHintUntilExpiry(t *testing.T) {
	m := New()
	k := key()
	k.ToastText = "order placed"
	k.ToastUntil = 10_000 // ms
	k.NowSec = 5          // 5_000 ms < 10_000 ⇒ live
	out := m.View(k, sampleData())
	if !strings.Contains(out, "order placed") {
		t.Errorf("a live toast should replace the hint line; got: %q", out)
	}
	// Once the clock passes the deadline the toast disappears and the hint returns.
	k.NowSec = 11 // 11_000 ms >= 10_000 ⇒ expired
	out = m.View(k, sampleData())
	if strings.Contains(out, "order placed") {
		t.Errorf("an expired toast should be gone; got: %q", out)
	}
	if !strings.Contains(plain(out), "x:cancel") {
		t.Errorf("after toast expiry the hint should return; got: %q", plain(out))
	}
}

func TestHitsClickableKeys(t *testing.T) {
	m := New()
	k := key() // orders focus
	k.W = 300  // wide enough that the whole strip (incl. order-entry caps) fits
	hits := m.Hits(k)
	if len(hits) == 0 {
		t.Fatal("orders focus should expose clickable keys")
	}
	// Every advertised key must be reachable by a click at its own column.
	want := map[string]bool{"tab": false, "x": false, "X": false, "a": false, "n": false, "?": false, "q": false, "up": false, "down": false}
	for _, h := range hits {
		if _, ok := want[h.Send.String()]; ok {
			want[h.Send.String()] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("key %q has no clickable hit", k)
		}
	}
}

func TestHitsEmptyUnderToast(t *testing.T) {
	m := New()
	k := key()
	k.ToastText = "order placed"
	k.ToastUntil = 10_000
	k.NowSec = 5 // live toast covers the hint line
	if h := m.Hits(k); len(h) != 0 {
		t.Errorf("a live toast should expose no clickable keys, got %d", len(h))
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := m.View(k, sampleData())
	// Different DATA but same KEY → cache hit returns the first render (the
	// revision in the key is the contract for "data changed").
	other := sampleData()
	other.Health.DataCount = 999
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render that reflects new data.
	k.HealthRev = 2
	if got := m.View(k, other); strings.Contains(got, "events:42") {
		t.Error("after a HealthRev bump the render should reflect the new health, not the cached one")
	}
}

func TestFocusChangeInvalidates(t *testing.T) {
	m := New()
	k := key()
	a := m.View(k, sampleData())
	k.Focus = FocusBalances
	b := m.View(k, sampleData())
	if a == b {
		t.Error("a focus change should produce a different (re-rendered) hint line")
	}
}
