// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package chartind

import (
	"math"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/tui/candlechart"
)

func mkCandles(closes ...float64) []candlechart.Candle {
	cs := make([]candlechart.Candle, len(closes))
	for i, c := range closes {
		cs[i] = candlechart.Candle{Time: int64(i), Open: c, High: c, Low: c, Close: c}
	}
	return cs
}

func TestEMAOverlayAlignedAndWarmsUp(t *testing.T) {
	cs := mkCandles(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	ov := EMA{Period: 3}.Overlays(cs)
	if len(ov) != 1 {
		t.Fatalf("want 1 overlay, got %d", len(ov))
	}
	o := ov[0]
	if o.Name != "EMA 3" {
		t.Errorf("name = %q, want %q", o.Name, "EMA 3")
	}
	if len(o.Values) != len(cs) {
		t.Fatalf("values len = %d, want %d (1:1 with candles)", len(o.Values), len(cs))
	}
	// Warm-up: the first (period-1) positions are NaN, breaking the line.
	for i := 0; i < 2; i++ {
		if !math.IsNaN(o.Values[i]) {
			t.Errorf("value[%d] = %v, want NaN (warm-up)", i, o.Values[i])
		}
	}
	if math.IsNaN(o.Values[2]) {
		t.Errorf("value[2] = NaN, want a real EMA reading at the lookback")
	}
	// EMA of a monotonic rising series lags below the latest close.
	last := o.Values[len(o.Values)-1]
	if last <= 0 || last >= 10 {
		t.Errorf("last EMA = %v, want a lagging value in (0,10)", last)
	}
}

func TestEMAEmptyAndBadPeriod(t *testing.T) {
	if ov := (EMA{Period: 20}).Overlays(nil); ov != nil {
		t.Errorf("empty series: want nil, got %+v", ov)
	}
	if ov := (EMA{Period: 0}).Overlays(mkCandles(1, 2, 3)); ov != nil {
		t.Errorf("zero period: want nil, got %+v", ov)
	}
}

func TestEMATooShortReturnsWarmupLine(t *testing.T) {
	// Series shorter than the warm-up still returns one overlay (all NaN), so the
	// chart simply draws no line yet rather than the caller special-casing it.
	cs := mkCandles(1, 2)
	ov := EMA{Period: 20}.Overlays(cs)
	if len(ov) != 1 || len(ov[0].Values) != len(cs) {
		t.Fatalf("want 1 overlay aligned to %d candles, got %+v", len(cs), ov)
	}
	for i, v := range ov[0].Values {
		if !math.IsNaN(v) {
			t.Errorf("value[%d] = %v, want NaN (too short to warm up)", i, v)
		}
	}
}

// EMA must satisfy candlechart.Indicator so it plugs into Model.SetIndicators.
var _ candlechart.Indicator = EMA{}
