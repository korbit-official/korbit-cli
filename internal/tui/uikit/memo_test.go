// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import "testing"

func TestMemoCachesUntilKeyChanges(t *testing.T) {
	var m Memo[int]
	calls := 0
	draw := func() string { calls++; return "render" }

	if got := m.Do(1, draw); got != "render" || calls != 1 {
		t.Fatalf("first Do: got %q calls %d, want render/1", got, calls)
	}
	// Same key: cached, draw not called again.
	if got := m.Do(1, draw); got != "render" || calls != 1 {
		t.Fatalf("same key: got %q calls %d, want a cache hit (calls still 1)", got, calls)
	}
	// New key: miss, re-render.
	if got := m.Do(2, draw); got != "render" || calls != 2 {
		t.Fatalf("new key: got %q calls %d, want a miss (calls 2)", got, calls)
	}
	// Back to the previous value re-renders (single-entry cache, not a map).
	if m.Do(1, draw); calls != 3 {
		t.Errorf("a single-entry cache should re-render on key 1 after key 2; calls = %d, want 3", calls)
	}
	// Invalidate forces a miss even on the same key.
	m.Invalidate()
	if m.Do(1, draw); calls != 4 {
		t.Errorf("Invalidate did not force a re-render; calls = %d, want 4", calls)
	}
}

func TestMemoStructKey(t *testing.T) {
	type key struct {
		Rev  uint64
		W, H int
		S    StyleID
	}
	var m Memo[key]
	calls := 0
	draw := func() string { calls++; return "x" }
	k := key{Rev: 7, W: 80, H: 24, S: StyleID{Scheme: 1}}

	m.Do(k, draw)
	m.Do(k, draw) // identical struct → hit
	if calls != 1 {
		t.Fatalf("identical struct key should hit; calls = %d, want 1", calls)
	}
	k.Rev = 8 // a bumped store revision → miss
	m.Do(k, draw)
	if calls != 2 {
		t.Errorf("a changed revision should miss; calls = %d, want 2", calls)
	}
}
