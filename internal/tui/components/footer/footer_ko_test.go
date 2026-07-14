// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package footer

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
)

// A Korean-language frame must respect the same width budget as English: the
// hint line is laid out from localized labels, and every localized string
// measures in display cells (Hangul is two cells per glyph). The assertions
// are language-agnostic — no Korean text is pinned (the test suite stays
// English-only by policy; translations are free to change).
func TestFooterKoreanFitsWidth(t *testing.T) {
	if err := i18n.Activate("ko"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := i18n.Activate(i18n.DefaultLang); err != nil {
			t.Fatal(err)
		}
	}()
	for _, w := range []int{40, 80, 120} {
		for focus := 0; focus <= 2; focus++ {
			k := Key{Private: true, HasTrader: true, MultiAccount: true, FundingWired: true,
				CandlesWired: true, Focus: focus, SizeKeys: "1-4", W: w}
			out := New().View(k, Data{Health: state.Health{}})
			for _, line := range strings.Split(out, "\n") {
				if got := lipgloss.Width(line); got > w {
					t.Errorf("w=%d focus=%d: line overflows (%d cells): %q", w, focus, got, line)
				}
			}
		}
	}
}
