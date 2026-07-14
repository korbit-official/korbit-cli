// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"strings"
	"testing"
)

func TestFactsLineAllFit(t *testing.T) {
	got := FactsLine(40, " · ", []Fact{
		{Text: "BUY 0.5 BTC"}, {Text: "fee ~100", Rank: 2}, {Text: "book ●"},
	})
	if got != "BUY 0.5 BTC · fee ~100 · book ●" {
		t.Fatalf("a fitting line must keep every fact: %q", got)
	}
}

func TestFactsLineSkipsEmpty(t *testing.T) {
	got := FactsLine(40, " · ", []Fact{{Text: "a"}, {Text: ""}, {Text: "b", Rank: 1}})
	if got != "a · b" {
		t.Fatalf("empty facts must be skipped without a separator: %q", got)
	}
}

// TestFactsLineDropsLowRankFirst: the essentials (rank 0) survive on a narrow
// line; droppable facts leave highest-rank-last-position first, and the +n
// tail discloses the count.
func TestFactsLineDropsLowRankFirst(t *testing.T) {
	facts := []Fact{
		{Text: "BUY 0.5 BTC", Rank: 0},
		{Text: "= 50,000,000 KRW", Rank: 1},
		{Text: "fee ~100", Rank: 2},
		{Text: "(note)", Rank: 2},
		{Text: "⚠ NOTIONAL_ABOVE_MAX", Rank: 0},
		{Text: "book ●", Rank: 0},
	}
	got := FactsLine(50, " · ", facts)
	for _, want := range []string{"BUY 0.5 BTC", "⚠ NOTIONAL_ABOVE_MAX", "book ●"} {
		if !strings.Contains(got, want) {
			t.Errorf("rank-0 fact %q must survive: %q", want, got)
		}
	}
	if strings.Contains(got, "(note)") {
		t.Errorf("the last rank-2 fact should drop first: %q", got)
	}
	if !strings.Contains(got, "+") {
		t.Errorf("dropped facts must be disclosed with a +n tail: %q", got)
	}
	// Roomier: rank 2 drops before rank 1.
	got = FactsLine(60, " · ", facts)
	if strings.Contains(got, "fee ~100") && !strings.Contains(got, "= 50,000,000 KRW") {
		t.Errorf("rank 1 must outlive rank 2: %q", got)
	}
}

// TestFactsLineRankZeroOverflow: when even the undroppable facts overflow,
// the line truncates rather than dropping one.
func TestFactsLineRankZeroOverflow(t *testing.T) {
	got := FactsLine(10, " · ", []Fact{
		{Text: "AAAAAAAA"}, {Text: "BBBBBBBB"},
	})
	if w := lineWidth(got); w > 10 {
		t.Fatalf("an overflowing rank-0 line must truncate to width, got %d: %q", w, got)
	}
	if !strings.HasPrefix(got, "AAAAAAAA") {
		t.Fatalf("truncation must keep the head: %q", got)
	}
}

func lineWidth(s string) int {
	return len([]rune(s)) // test inputs are plain single-cell text
}
