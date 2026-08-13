// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package chartind holds the technical-indicator implementations for the candle
// chart. Each type satisfies [candlechart.Indicator] — it turns a candle series
// into the overlay line(s) the chart draws. The chart owns the wiring and the
// drawing; this package owns only the math, computed over the shared
// indicators library (which wraps go-talib). Add an indicator by adding a type
// here that implements [candlechart.Indicator]; nothing in candlechart changes.
//
// Inputs are the chart's float64 candles (a chart is a visual artifact, so
// prices are already floats here) and outputs are overlay values aligned 1:1
// with the input, NaN for warm-up positions — the chart breaks the line across a
// NaN. An indicator with no value for the given series returns nil.
package chartind

import (
	"image/color"
	"strconv"

	"github.com/korbit-official/korbit-cli/internal/indicators"
	"github.com/korbit-official/korbit-cli/internal/tui/candlechart"
)

// EMA overlays the exponential moving average of candle closes as one line.
// Period is the EMA length in bars (e.g. 20); Color overrides the chart's
// default overlay color when non-nil.
type EMA struct {
	Period int
	Color  color.Color
}

// Overlays computes the EMA line for cs. It returns nil for a non-positive
// period or an empty series; a series shorter than the warm-up still returns one
// overlay whose values are all NaN (the chart simply draws no line yet).
func (e EMA) Overlays(cs []candlechart.Candle) []candlechart.Overlay {
	if e.Period < 1 || len(cs) == 0 {
		return nil
	}
	closes := make([]float64, len(cs))
	for i, c := range cs {
		closes[i] = c.Close
	}
	vals, err := indicators.EMA(closes, e.Period)
	if err != nil {
		return nil
	}
	return []candlechart.Overlay{{
		Name:   "EMA " + strconv.Itoa(e.Period),
		Color:  e.Color,
		Values: vals,
	}}
}
