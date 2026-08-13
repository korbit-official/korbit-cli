// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package candlechart

import "testing"

func TestViewFrameCacheHitsAndInvalidates(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true)

	a := m.View()
	if !m.frame.valid {
		t.Fatal("frame cache not populated after View")
	}
	first := m.frame.key

	// Unchanged inputs: a hit returns the identical frame and keeps the key.
	if b := m.View(); b != a || m.frame.key != first {
		t.Error("an unchanged View should return the cached frame and keep the key")
	}

	// A candle-content change bumps rev → new key.
	m.SetCandles(manyCandles(40), true)
	_ = m.View()
	if m.frame.key.rev == first.rev {
		t.Error("a candle-content change should bump the content revision in the key")
	}

	// A resize changes the key directly.
	wBefore := m.frame.key.w
	m.SetSize(100, 24)
	_ = m.View()
	if m.frame.key.w == wBefore {
		t.Error("a resize should change the cache key width")
	}

	// A selection change changes the key.
	selBefore := m.frame.key.selected
	m.SelectPrev()
	_ = m.View()
	if m.frame.key.selected == selBefore {
		t.Error("a selection move should change the cache key")
	}
}

func TestViewFrameCacheStyleProbe(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true)
	green := m.View()
	m.SetStyles(RedBlueStyles())
	red := m.View()
	if green == red {
		t.Error("a color-scheme change should produce a different frame (style probe missed it)")
	}
}

// TestViewFrameCacheSharedAcrossValueCopy guards the host's inline-pane usage: a
// value-copy of the Model shares the cache pointer, but a different size renders
// its own frame rather than wrongly returning the original's cached one.
func TestViewFrameCacheSharedAcrossValueCopy(t *testing.T) {
	m := New(80, 20)
	m.SetCandles(sample(), true)
	full := m.View()

	c := m // value copy: shares the frame-cache pointer
	c.SetSize(40, 10)
	inline := c.View()
	if inline == full {
		t.Fatal("a different-sized value-copy must render its own frame, not the cached full-size one")
	}
	// The original still renders its own frame correctly afterward.
	if again := m.View(); again != full {
		t.Error("the original Model's frame should be unaffected by the value-copy render")
	}
}
