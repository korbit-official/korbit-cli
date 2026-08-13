// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// Fact is one segment of a joined facts line, ranked by how essential it is.
// Rank 0 facts are never dropped; higher ranks drop first when the line runs
// out of room.
type Fact struct {
	Text string // rendered segment (may carry styling); "" is skipped
	Rank int
}

// FactsLine joins facts (in the given order) with sep so the line fits w
// display cells by DROPPING whole low-rank facts rather than clipping the
// tail: a blind tail cut loses whichever fact happens to sit last, and on a
// crowded line that can be a safety disclosure. Rank-0 facts always stay (a
// line of only rank-0 facts falls back to Truncate); when facts are dropped,
// a dim "+n" tail says how many. Within a rank, later facts drop first.
func FactsLine(w int, sep string, facts []Fact) string {
	kept := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if f.Text != "" {
			kept = append(kept, f)
		}
	}
	dropped := 0
	for {
		parts := make([]string, len(kept))
		for i, f := range kept {
			parts[i] = f.Text
		}
		line := strings.Join(parts, sep)
		if dropped > 0 {
			line += sep + StyDim.Render("+"+strconv.Itoa(dropped))
		}
		if lipgloss.Width(line) <= w {
			return line
		}
		// The last fact of the highest droppable rank goes first.
		drop := -1
		for i, f := range kept {
			if f.Rank > 0 && (drop < 0 || f.Rank >= kept[drop].Rank) {
				drop = i
			}
		}
		if drop < 0 {
			return Truncate(line, w)
		}
		kept = append(kept[:drop], kept[drop+1:]...)
		dropped++
	}
}
