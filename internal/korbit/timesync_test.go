// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// clockStub serves a programmable sequence of /v2/time outcomes (one per Do
// call) and reads its local-clock readings from a fixed sequence, so a test can
// dictate the exact RTT and server time of each probe.
type clockStub struct {
	results     []clockProbe
	call        int
	maxDeadline time.Duration // largest time-until-deadline seen across probes (0 = none observed)
}

type clockProbe struct {
	serverTime int64
	err        error // transport error for this probe
	badShape   bool  // 2xx but not the {time} shape
}

func (c *clockStub) Do(r *http.Request) (*http.Response, error) {
	i := c.call
	c.call++
	if dl, ok := r.Context().Deadline(); ok {
		if d := time.Until(dl); d > c.maxDeadline {
			c.maxDeadline = d
		}
	}
	if i >= len(c.results) {
		return nil, fmt.Errorf("unexpected probe %d", i)
	}
	p := c.results[i]
	if p.err != nil {
		return nil, p.err
	}
	if p.badShape {
		return mkResp(200, `{"success":true,"data":{"notTime":1}}`, nil), nil
	}
	body := fmt.Sprintf(`{"success":true,"data":{"time":%d}}`, p.serverTime)
	return mkResp(200, body, nil), nil
}

// seqNow returns a Now func that yields the given readings in order (t0, t1 per
// probe).
func seqNow(readings ...int64) func() int64 {
	i := 0
	return func() int64 {
		v := readings[i]
		i++
		return v
	}
}

func TestMeasureClockOffsetPicksMinDelaySample(t *testing.T) {
	const srv = 1_000_000
	d := &clockStub{results: []clockProbe{
		{serverTime: srv}, // rtt 100
		{serverTime: srv}, // rtt 20  <- min
		{serverTime: srv}, // rtt 60
	}}
	opts := Options{Doer: d, Now: seqNow(0, 100, 200, 220, 300, 360)}

	got, err := MeasureClockOffset(opts, 3, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Samples != 3 {
		t.Errorf("Samples = %d, want 3", got.Samples)
	}
	if got.RTTMinMs != 20 {
		t.Errorf("RTTMinMs = %d, want 20", got.RTTMinMs)
	}
	// min-delay sample: t0=200, t1=220, mid=210 -> offset = srv - 210
	if want := int64(srv - 210); got.OffsetMs != want {
		t.Errorf("OffsetMs = %d, want %d", got.OffsetMs, want)
	}
	if got.UncertaintyMs() != 10 {
		t.Errorf("UncertaintyMs = %d, want 10", got.UncertaintyMs())
	}
}

func TestMeasureClockOffsetSkipsFailedProbes(t *testing.T) {
	const srv = 2_000_000
	d := &clockStub{results: []clockProbe{
		{err: errors.New("connection refused")},
		{badShape: true},
		{serverTime: srv}, // the only good one, rtt 40
	}}
	opts := Options{Doer: d, Now: seqNow(0, 10, 20, 30, 100, 140)}

	got, err := MeasureClockOffset(opts, 3, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Samples != 1 || got.RTTMinMs != 40 {
		t.Errorf("got Samples=%d RTTMinMs=%d, want 1 and 40", got.Samples, got.RTTMinMs)
	}
	if want := int64(srv - 120); got.OffsetMs != want { // mid of 100,140
		t.Errorf("OffsetMs = %d, want %d", got.OffsetMs, want)
	}
}

func TestMeasureClockOffsetAllFail(t *testing.T) {
	d := &clockStub{results: []clockProbe{
		{err: errors.New("net down")},
		{err: errors.New("net down")},
	}}
	opts := Options{Doer: d, Now: seqNow(0, 5, 10, 15)}

	_, err := MeasureClockOffset(opts, 2, nil)
	if err == nil {
		t.Fatal("expected an error when every probe fails")
	}
}

// Each probe's HTTP timeout is capped at maxClockProbeTimeoutMs even when the
// caller passes a much larger --timeout, so a black-holed /v2/time cannot stall
// a sync for probes×--timeout.
func TestMeasureClockOffsetCapsProbeTimeout(t *testing.T) {
	const srv = 1_000_000
	d := &clockStub{results: []clockProbe{{serverTime: srv}, {serverTime: srv}}}
	opts := Options{Doer: d, Now: seqNow(0, 10, 20, 30), TimeoutMs: 60_000}

	if _, err := MeasureClockOffset(opts, 2, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.maxDeadline == 0 {
		t.Fatal("no per-probe deadline observed; timeout not applied")
	}
	if d.maxDeadline > maxClockProbeTimeoutMs*time.Millisecond {
		t.Fatalf("per-probe deadline %v exceeds cap %dms — caller's 60s --timeout was not trimmed",
			d.maxDeadline, maxClockProbeTimeoutMs)
	}
}

// An explicit caller timeout below the cap wins (min semantics), so --timeout
// still tightens the probe when the user asks for something stricter.
func TestMeasureClockOffsetHonorsTighterCallerTimeout(t *testing.T) {
	const srv = 1_000_000
	const tight = 500
	d := &clockStub{results: []clockProbe{{serverTime: srv}}}
	opts := Options{Doer: d, Now: seqNow(0, 10), TimeoutMs: tight}

	if _, err := MeasureClockOffset(opts, 1, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.maxDeadline > tight*time.Millisecond {
		t.Fatalf("per-probe deadline %v exceeds the caller's %dms — min(caller, cap) not honored",
			d.maxDeadline, tight)
	}
}
