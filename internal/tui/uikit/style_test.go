// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func lastPlainLine(s string) string {
	lines := strings.Split(ansiRe.ReplaceAllString(s, ""), "\n")
	return lines[len(lines)-1]
}

// TestPanelWithFooterEmptySearchPrompt: an active search renders a "/" prompt in
// the bottom-left even with an empty query, so it is distinct from no search. The
// query string alone cannot carry this — searching is an explicit flag.
func TestPanelWithFooterEmptySearchPrompt(t *testing.T) {
	const w, h = 24, 5
	noSearch := lastPlainLine(PanelWithFooter(false, false, "", "markets", "1/2", nil, w, h))
	empty := lastPlainLine(PanelWithFooter(false, true, "", "markets", "1/2", nil, w, h))
	typed := lastPlainLine(PanelWithFooter(false, true, "k", "markets", "1/2", nil, w, h))

	if strings.Contains(noSearch, "╰─ /") {
		t.Fatalf("no-search border must not show a / prompt, got %q", noSearch)
	}
	if !strings.Contains(empty, "╰─ /") {
		t.Fatalf("active empty search must show a / prompt, got %q", empty)
	}
	if !strings.Contains(typed, "╰─ /k") {
		t.Fatalf("active search must show /query, got %q", typed)
	}
}

// TestTruncateMarksTheCut: a cut line always ends in "…" — a silent clip
// reads as the whole story.
func TestTruncateMarksTheCut(t *testing.T) {
	if got := Truncate("abcdef", 4); got != "abc…" {
		t.Errorf("Truncate must mark the cut, got %q", got)
	}
	if got := Truncate("abcd", 4); got != "abcd" {
		t.Errorf("a fitting line must pass through untouched, got %q", got)
	}
	if got := Truncate("abcdef", 1); got != "…" {
		t.Errorf("a one-cell cut is just the mark, got %q", got)
	}
	if got := Truncate("abcdef", 0); got != "" {
		t.Errorf("zero width renders nothing, got %q", got)
	}
	// Styled input: the width contract holds and the mark survives.
	styled := StyWarn.Render("abcdef")
	if got := Truncate(styled, 4); lipgloss.Width(got) != 4 || !strings.HasSuffix(got, "…") {
		t.Errorf("styled cut must be 4 cells ending in the mark, got %q (w=%d)", got, lipgloss.Width(got))
	}
}
