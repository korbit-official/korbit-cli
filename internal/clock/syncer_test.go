// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSyncerInstallsOnSuccess: a successful measurement installs the estimate
// into the shared State.
func TestSyncerInstallsOnSuccess(t *testing.T) {
	st := New(func() int64 { return 1000 })
	s := NewSyncer(st, func() (int64, int64, error) { return 500, 30, nil }, func() int64 { return 0 }, DefaultCoolDownMs)

	if st.Measured() {
		t.Fatal("State must be unmeasured before Sync")
	}
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !st.Measured() {
		t.Fatal("State must be measured after a successful Sync")
	}
	if got, want := st.SignNow(), int64(1000+500-30); got != want {
		t.Fatalf("SignNow = %d, want %d (local + offset - lean)", got, want)
	}
}

// TestSyncerCooldownReusesEstimate: a fresh estimate is reused (no re-probe)
// within the cooldown window, and re-measured after it.
func TestSyncerCooldownReusesEstimate(t *testing.T) {
	var calls int
	now := int64(1000)
	st := New(func() int64 { return 0 })
	s := NewSyncer(st, func() (int64, int64, error) {
		calls++
		return int64(calls) * 100, 10, nil
	}, func() int64 { return now }, 500)

	if err := s.Sync(); err != nil { // first: measures (lastSyncAt=1000)
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("first Sync calls=%d, want 1", calls)
	}

	now = 1400 // 400 < 500: still fresh
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("within cooldown calls=%d, want 1 (reuse, no probe)", calls)
	}

	now = 1600 // 600 >= 500: re-measure
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("after cooldown calls=%d, want 2 (re-probe)", calls)
	}
}

// TestSyncerFailureDoesNotStartCooldown: a failed measurement leaves the
// estimate untouched and does NOT start the cooldown, so the next trigger
// re-probes immediately rather than reusing a stale/never-measured estimate.
func TestSyncerFailureDoesNotStartCooldown(t *testing.T) {
	var calls int
	now := int64(1000)
	fail := true
	st := New(func() int64 { return 0 })
	s := NewSyncer(st, func() (int64, int64, error) {
		calls++
		if fail {
			return 0, 0, errors.New("unreachable")
		}
		return 100, 10, nil
	}, func() int64 { return now }, 500)

	if err := s.Sync(); err == nil {
		t.Fatal("Sync must surface the measurement error")
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if st.Measured() {
		t.Fatal("a failed measurement must not install an estimate")
	}

	// Within what WOULD be the cooldown if a failure had stamped it.
	now = 1100
	fail = false
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 (a failed probe must not start the cooldown)", calls)
	}
	if !st.Measured() {
		t.Fatal("the second, successful Sync must install")
	}
}

// TestSyncerNeverSyncedIgnoresCooldown: before any successful measurement the
// cooldown never short-circuits — the first measurement always runs.
func TestSyncerNeverSyncedIgnoresCooldown(t *testing.T) {
	var calls int
	st := New(func() int64 { return 0 })
	// now is pinned, so were the cooldown applied to the never-synced state it
	// would (wrongly) suppress the very first probe.
	s := NewSyncer(st, func() (int64, int64, error) {
		calls++
		return 1, 1, nil
	}, func() int64 { return 1000 }, 10_000)

	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 (cooldown must not gate the first measurement)", calls)
	}
}

// TestSyncerSingleFlightCollapsesConcurrentProbes: concurrent Sync calls share
// ONE in-flight measurement instead of each probing /v2/time. This is the guard
// against an EXCEED_TIME_WINDOW burst (e.g. a Promise.all of placements all
// rejected at once) storming the clock endpoint.
func TestSyncerSingleFlightCollapsesConcurrentProbes(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	st := New(func() int64 { return 0 })
	s := NewSyncer(st, func() (int64, int64, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case entered <- struct{}{}: // signal the leader is inside measure
		default:
		}
		<-release // hold the in-flight measurement so callers pile up behind it
		return 100, 10, nil
	}, func() int64 { return 0 }, 0) // cooldown disabled: only single-flight collapses these

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)

	wg.Add(1)
	go func() { defer wg.Done(); errs[0] = s.Sync() }()
	<-entered // the leader now holds the in-flight call

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = s.Sync() }(i)
	}
	// Let the followers reach the in-flight join before releasing the leader.
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("measure called %d times; single-flight must collapse concurrent Syncs to 1", got)
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("caller %d got error %v; all single-flight callers must share the one success", i, e)
		}
	}
	if st.Offset() != 100 {
		t.Fatalf("Offset = %d, want 100 (the shared measurement installed)", st.Offset())
	}
}

// TestSyncerStateRoundTrips: State() returns the same State the Syncer installs
// into (the central instance every surface shares).
func TestSyncerStateRoundTrips(t *testing.T) {
	st := New(func() int64 { return 0 })
	s := NewSyncer(st, func() (int64, int64, error) { return 0, 0, nil }, func() int64 { return 0 }, DefaultCoolDownMs)
	if s.State() != st {
		t.Fatal("State() must return the injected State")
	}
}
