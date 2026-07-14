// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package candlechart

import (
	"fmt"
	"testing"
)

// BenchmarkView contrasts a frame-cache hit (the steady state, where nothing the
// chart depends on changed) against a full render (what every redraw would cost
// without the cache), at two sizes: the compact inline pane and the full-screen
// overlay. The host value-copies the chart and calls View each frame, so an
// unchanged frame returns the cached string instead of re-plotting.
func BenchmarkView(b *testing.B) {
	sizes := []struct {
		name string
		w, h int
	}{
		{"inline", 70, 9},    // the compact pane over the orderbook/trades
		{"overlay", 120, 30}, // the full 'g' overlay
	}
	for _, s := range sizes {
		b.Run(fmt.Sprintf("%s/cached", s.name), func(b *testing.B) {
			m := New(s.w, s.h)
			m.SetCandles(manyCandles(200), true)
			_ = m.View() // warm the cache
			b.ReportAllocs()
			for b.Loop() {
				_ = m.View()
			}
		})
		b.Run(fmt.Sprintf("%s/cold", s.name), func(b *testing.B) {
			m := New(s.w, s.h)
			m.SetCandles(manyCandles(200), true)
			b.ReportAllocs()
			for b.Loop() {
				m.frame.valid = false // force a full render, as a pre-cache frame would
				_ = m.View()
			}
		})
	}
}
