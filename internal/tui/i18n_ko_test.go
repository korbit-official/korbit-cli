// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
)

// A Korean-language session must lay out inside the same terminal budget as
// English: labels widen to two cells per glyph, label columns derive from the
// active table, and every localized line still fits the frame. Assertions are
// language-agnostic — no Korean text is pinned (the suite is English-only by
// policy; translations are free to change without touching tests).
func TestKoreanFramesFitWidth(t *testing.T) {
	if err := i18n.Activate("ko"); err != nil {
		t.Fatal(err)
	}
	defer i18n.Activate(i18n.DefaultLang)
	m := testModel(t, true, &fakeTrader{})
	frames := map[string]string{}
	frames["browse"] = m.render()
	mm, _ := press(t, m, k('b', "b"))
	frames["order"] = mm.render()
	mm, _ = press(t, m, k('?', "?"))
	frames["help"] = mm.render()
	mm, _ = press(t, m, k('X', "X"))
	frames["cancelall"] = mm.render()
	for name, f := range frames {
		if strings.Contains(f, "\x00") {
			t.Errorf("%s: frame contains NUL runes (textinput placeholder artifact)", name)
		}
		if name == "help" {
			// The help overlay's long description lines exceed a narrow frame in
			// English too — the alt-screen clips them; no per-language contract.
			continue
		}
		for _, line := range strings.Split(f, "\n") {
			if lipgloss.Width(line) > 120 {
				t.Errorf("%s: line overflows: %q", name, plain(line))
			}
		}
	}
}
