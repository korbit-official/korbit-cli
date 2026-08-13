// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"testing"

	"github.com/korbit-official/korbit-cli/internal/ops"
)

// TestCandleHistoryMaxMatchesOpsCap pins the monitor --candle-history ceiling
// advertised in this registry to the candles operation's auto-paging ceiling
// that the runtime range check (cli/monitorcmd) actually enforces — the two
// are declared in different packages and would otherwise drift silently.
// (internal/candles pins its own maxSeedRows copy the same way.)
func TestCandleHistoryMaxMatchesOpsCap(t *testing.T) {
	for _, c := range Registry {
		if len(c.ID) != 1 || c.ID[0] != "monitor" {
			continue
		}
		for _, p := range c.Params {
			if p.Flag != "candle-history" {
				continue
			}
			if p.Max == nil || *p.Max != ops.CandlesMaxLimit {
				t.Fatalf("monitor --candle-history Max = %v, want ops.CandlesMaxLimit (%d)", p.Max, ops.CandlesMaxLimit)
			}
			return
		}
		t.Fatal("monitor entry has no candle-history param")
	}
	t.Fatal("no monitor entry in the registry")
}
