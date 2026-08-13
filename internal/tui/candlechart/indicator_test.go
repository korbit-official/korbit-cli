// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package candlechart

import (
	"slices"
	"testing"
)

// fakeIndicator records the series it last saw and how many times it ran, and
// returns one overlay carrying the candle closes (so a test can assert what it
// ran against and whether the cache skipped it).
type fakeIndicator struct {
	sawLen *int
	calls  *int
}

func (f fakeIndicator) Overlays(cs []Candle) []Overlay {
	if f.calls != nil {
		*f.calls++
	}
	if f.sawLen != nil {
		*f.sawLen = len(cs)
	}
	vals := make([]float64, len(cs))
	for i, c := range cs {
		vals[i] = c.Close
	}
	return []Overlay{{Name: "fake", Values: vals}}
}

func TestSetIndicatorsComputesOverlaysFromClosedCandles(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true) // last bar is live -> excluded from the indicator
	m.SetIndicators([]Indicator{fakeIndicator{}})

	if len(m.overlays) != 1 {
		t.Fatalf("want 1 overlay, got %d", len(m.overlays))
	}
	closed := len(sample()) - 1 // the live bar is dropped
	if got := len(m.overlays[0].Values); got != closed {
		t.Fatalf("overlay values len = %d, want %d (closed candles only)", got, closed)
	}
	if last := m.overlays[0].Values[closed-1]; last != sample()[closed-1].Close {
		t.Errorf("overlay last value = %v, want %v (the last CLOSED close)", last, sample()[closed-1].Close)
	}
}

func TestIndicatorsExcludeLiveAndRecomputeOnRollover(t *testing.T) {
	var saw int
	m := New(80, 20)
	m.SetIndicators([]Indicator{fakeIndicator{sawLen: &saw}})
	if saw != 0 {
		t.Fatalf("indicator saw %d candles before any were set, want 0", saw)
	}

	m.SetCandles(sample(), true)
	if want := len(sample()) - 1; saw != want {
		t.Fatalf("SetCandles: indicator saw %d candles, want %d (live excluded)", saw, want)
	}

	// A rollover appends a new live bar; the prior live bar becomes closed and is
	// included, while the fresh live bar is excluded.
	m.UpsertLive(Candle{Time: 99, Open: 129, High: 135, Low: 128, Close: 134})
	if want := len(sample()); saw != want {
		t.Fatalf("rollover: indicator saw %d candles, want %d", saw, want)
	}
	last := m.overlays[0].Values[len(m.overlays[0].Values)-1]
	if last != sample()[len(sample())-1].Close {
		t.Errorf("overlay last value = %v, want %v (the prior live bar closed by the rollover, not the new live %v)",
			last, sample()[len(sample())-1].Close, 134.0)
	}
}

func TestIndicatorsCachedWhenClosedSeriesUnchanged(t *testing.T) {
	var calls int
	m := New(80, 20)
	m.SetCandles(sample(), true)
	m.SetIndicators([]Indicator{fakeIndicator{calls: &calls}})
	base := calls // one compute over the closed bars
	if base == 0 {
		t.Fatal("indicator never ran")
	}

	// A live-only tick replaces the in-progress bucket; the closed series is
	// unchanged, so the cache must skip the recompute.
	liveTime := sample()[len(sample())-1].Time
	m.UpsertLive(Candle{Time: liveTime, Open: 106, High: 140, Low: 106, Close: 140})
	if calls != base {
		t.Errorf("live-only tick recomputed indicators (calls %d -> %d); want a cache hit", base, calls)
	}

	// Re-feeding the same series (the inline pane's per-frame SetCandles) is also a hit.
	m.SetCandles(m.Candles(), true)
	if calls != base {
		t.Errorf("redundant re-feed recomputed indicators (calls %d -> %d); want a cache hit", base, calls)
	}

	// A rollover changes the closed series, so it must recompute.
	m.UpsertLive(Candle{Time: liveTime + 1, Open: 5, High: 5, Low: 5, Close: 5})
	if calls == base {
		t.Error("rollover did not recompute indicators")
	}
}

func TestSetIndicatorsInvalidatesCacheOnIndicatorChange(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true)

	var a, b int
	m.SetIndicators([]Indicator{fakeIndicator{calls: &a}})
	if a == 0 {
		t.Fatal("first indicator never ran")
	}
	// Same candles, a different indicator set: the cache keys on candles, so it
	// must still recompute (not reuse the prior indicator's overlays).
	m.SetIndicators([]Indicator{fakeIndicator{calls: &b}})
	if b == 0 {
		t.Error("changing the indicator set did not recompute despite identical candles")
	}
}

// TestValueCopyRecomputeDoesNotCorruptOriginalCache guards the value-copy
// invariant the inline pane relies on: recomputing on a copy must not write
// through into the original's cache (which would let the original falsely hit and
// draw a stale line).
func TestValueCopyRecomputeDoesNotCorruptOriginalCache(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true)
	m.SetIndicators([]Indicator{fakeIndicator{}})
	origValues := slices.Clone(m.overlays[0].Values)
	origCacheLen := len(m.ovCacheClosed)

	// Value-copy, then feed the copy a different series (forces a recompute miss).
	c := m
	c.SetCandles(manyCandles(40), true)

	if !slices.Equal(m.overlays[0].Values, origValues) {
		t.Error("original overlay values mutated through a value-copy recompute")
	}
	if len(m.ovCacheClosed) != origCacheLen {
		t.Errorf("original cache backing mutated through a value-copy: len %d, want %d",
			len(m.ovCacheClosed), origCacheLen)
	}
}

func TestSetOverlaysClearsIndicators(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true)
	m.SetIndicators([]Indicator{fakeIndicator{}})

	manual := []Overlay{{Name: "manual", Values: []float64{1, 2, 3, 4, 5}}}
	m.SetOverlays(manual)
	if len(m.overlays) != 1 || m.overlays[0].Name != "manual" {
		t.Fatalf("SetOverlays did not replace overlays: %+v", m.overlays)
	}

	// A later candle change must NOT resurrect the indicator (it was cleared).
	m.SetCandles(sample(), true)
	if m.overlays[0].Name != "manual" {
		t.Errorf("indicator recomputed after SetOverlays cleared it: %+v", m.overlays)
	}
}

func TestSetIndicatorsNilClearsOverlays(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true)
	m.SetIndicators([]Indicator{fakeIndicator{}})
	if len(m.overlays) == 0 {
		t.Fatal("expected overlays after SetIndicators")
	}
	m.SetIndicators(nil)
	if len(m.overlays) != 0 {
		t.Errorf("SetIndicators(nil) left %d overlays, want 0", len(m.overlays))
	}
}
