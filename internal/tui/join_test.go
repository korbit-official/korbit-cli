// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/candles"
)

// TestRenderBodyRectangularAcrossResizes: the fixed-geometry HJoin/VJoin assembly
// must produce an exact w×bodyH body at every size — every line exactly w display
// cells, exactly bodyH lines. It covers the three layouts the join feeds: the
// private right column (orders/fills/balances VJoin), the public three-column
// layout (no right column), and the inline candle pane (the VJoin(chart, below)
// over an HJoin). A short or
// mis-padded column would make a line the wrong width, so this pins the geometry
// the join trusts. (The body is checked rather than the full frame: the
// header/footer status lines are intentionally not padded to full width.)
func TestRenderBodyRectangularAcrossResizes(t *testing.T) {
	sizes := [][2]int{{80, 20}, {81, 21}, {100, 30}, {120, 40}, {160, 48}, {200, 50}}
	scenarios := []struct {
		name    string
		private bool
		inline  bool
	}{
		{"private", true, false},
		{"public", false, false},
		{"private+inline-chart", true, true},
	}
	for _, sc := range scenarios {
		m := newModel(Config{
			Symbols:     []string{"btc_krw", "eth_krw", "xrp_krw"},
			Private:     sc.private,
			Trader:      &fakeTrader{},
			KeyName:     "k",
			BaseURL:     "http://127.0.0.1:9999",
			Now:         func() int64 { return 1_700_000_000_000 },
			StopSession: func() {},
			Candles: func(symbol, interval string, limit int, endMs int64) ([]candles.Bar, error) {
				return []candles.Bar{{Timestamp: 0, Open: "100", High: "110", Low: "90", Close: "105", Volume: "1"}}, nil
			},
		})
		mm, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 48})
		m = mm.(model)
		if sc.inline {
			m, _ = press(t, m, k('G', "G")) // enable the inline candle pane (arms a fetch)
			m = seedChart(t, m, []candles.Bar{{Timestamp: 0, Open: "100", High: "110", Low: "90", Close: "105", Volume: "1"}})
		}
		for _, sz := range sizes {
			w, h := sz[0], sz[1]
			mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m = mm.(model)
			bodyH := h - lipgloss.Height(m.renderHeader()) - lipgloss.Height(m.renderFooter())
			lines := strings.Split(m.renderBody(bodyH), "\n")
			if len(lines) != bodyH {
				t.Fatalf("%s %dx%d: body has %d lines, want %d", sc.name, w, h, len(lines), bodyH)
			}
			for i, ln := range lines {
				if got := lipgloss.Width(ln); got != w {
					t.Fatalf("%s %dx%d: body line %d is %d wide, want %d: %q", sc.name, w, h, i, got, w, ln)
				}
			}
		}
	}
}
