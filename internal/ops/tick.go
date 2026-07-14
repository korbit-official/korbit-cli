// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"sort"

	"github.com/shopspring/decimal"
)

// Tick-size grid math over a symbol's tick-size policy (GET /v2/tickSizePolicy).
// These helpers are pure and advisory: they parse the policy's decimal strings,
// do exact decimal math, and render results back to canonical decimal strings.
// The server remains the authority — a price these helpers produce is meant to
// SATISFY validation, never to bypass it.

// TickBand is one price band of a symbol's tick-size policy: TickSize applies
// at prices at or above PriceGte. Both are decimal strings, as served by
// GET /v2/tickSizePolicy.
type TickBand struct {
	PriceGte string
	TickSize string
}

// tickBand is a parsed policy band.
type tickBand struct {
	gte  decimal.Decimal
	tick decimal.Decimal
}

// parseTickBands parses and sorts policy bands ascending by PriceGte,
// dropping unparseable bands and non-positive tick sizes (a zero tick would
// make the grid degenerate). An empty result means "no usable policy".
func parseTickBands(bands []TickBand) []tickBand {
	out := make([]tickBand, 0, len(bands))
	for _, b := range bands {
		gte, errg := decimal.NewFromString(b.PriceGte)
		tick, errt := decimal.NewFromString(b.TickSize)
		if errg != nil || errt != nil || !tick.IsPositive() || gte.IsNegative() {
			continue
		}
		out = append(out, tickBand{gte: gte, tick: tick})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].gte.LessThan(out[j].gte) })
	return out
}

// bandAt returns the band applicable at price: the one with the greatest
// PriceGte not exceeding it. ok=false when price sits below every band.
func bandAt(bands []tickBand, price decimal.Decimal) (tickBand, bool) {
	var cur tickBand
	found := false
	for _, b := range bands {
		if price.GreaterThanOrEqual(b.gte) {
			cur = b
			found = true
		}
	}
	return cur, found
}

// TickSizeAt returns the tick size applicable at price (the band with the
// greatest PriceGte not exceeding it). ok=false when the policy is empty or
// unusable, or the price is unparseable or below every band.
func TickSizeAt(bands []TickBand, price string) (string, bool) {
	p, err := decimal.NewFromString(price)
	if err != nil {
		return "", false
	}
	b, ok := bandAt(parseTickBands(bands), p)
	if !ok {
		return "", false
	}
	return b.tick.String(), true
}

// SnapToTick floors price onto its band's tick grid (the largest grid point
// not exceeding it). A price already on the grid comes back unchanged (in
// canonical form). ok=false under the same conditions as TickSizeAt.
func SnapToTick(bands []TickBand, price string) (string, bool) {
	p, err := decimal.NewFromString(price)
	if err != nil {
		return "", false
	}
	parsed := parseTickBands(bands)
	snapped, ok := snapDown(parsed, p)
	if !ok {
		return "", false
	}
	return snapped.String(), true
}

// snapDown floors p onto the grid of its band. Flooring can drop the price
// into a lower band (whose grid is finer), so the band is re-resolved and the
// floor re-applied until it settles — bounded by the band count.
func snapDown(bands []tickBand, p decimal.Decimal) (decimal.Decimal, bool) {
	b, ok := bandAt(bands, p)
	if !ok {
		return decimal.Decimal{}, false
	}
	for range bands {
		snapped := p.Div(b.tick).Floor().Mul(b.tick)
		nb, ok := bandAt(bands, snapped)
		if !ok {
			return decimal.Decimal{}, false
		}
		if nb.gte.Equal(b.gte) {
			return snapped, true
		}
		b, p = nb, snapped // crossed into a lower band; re-floor on its grid
	}
	return p, true
}

// SnapUpToTick ceils price onto its band's tick grid (the smallest grid point
// at or above it). A price already on the grid comes back unchanged (in
// canonical form). ok=false under the same conditions as TickSizeAt.
func SnapUpToTick(bands []TickBand, price string) (string, bool) {
	p, err := decimal.NewFromString(price)
	if err != nil {
		return "", false
	}
	snapped, ok := snapUp(parseTickBands(bands), p)
	if !ok {
		return "", false
	}
	return snapped.String(), true
}

// snapUp ceils p onto the grid of its band — the mirror of snapDown. Ceiling
// can lift the price into a higher band (whose grid is coarser), so the band is
// re-resolved and the ceil re-applied until it settles — bounded by band count.
func snapUp(bands []tickBand, p decimal.Decimal) (decimal.Decimal, bool) {
	b, ok := bandAt(bands, p)
	if !ok {
		return decimal.Decimal{}, false
	}
	for range bands {
		snapped := p.Div(b.tick).Ceil().Mul(b.tick)
		nb, ok := bandAt(bands, snapped)
		if !ok {
			return decimal.Decimal{}, false
		}
		if nb.gte.Equal(b.gte) {
			return snapped, true
		}
		b, p = nb, snapped // crossed into a higher band; re-ceil on its grid
	}
	return p, true
}

// OnTick reports whether price sits exactly on its band's tick grid. ok=false
// when the grid can't be resolved (empty/unusable policy, or price unparseable
// or below every band) — the caller then can't check and defers to the server.
func OnTick(bands []TickBand, price string) (onGrid, ok bool) {
	p, err := decimal.NewFromString(price)
	if err != nil {
		return false, false
	}
	snapped, ok := snapDown(parseTickBands(bands), p)
	if !ok {
		return false, false
	}
	return snapped.Equal(p), true
}

// StepTicks moves price by n grid steps (n > 0 up, n < 0 down) along the
// tick-size grid, snapping the starting price onto the grid first and
// re-resolving the band as a step crosses a band edge. Stepping clamps rather
// than leaving the grid: a step below the lowest grid point (or to zero)
// stays put. ok=false under the same conditions as TickSizeAt.
func StepTicks(bands []TickBand, price string, n int) (string, bool) {
	p, err := decimal.NewFromString(price)
	if err != nil {
		return "", false
	}
	parsed := parseTickBands(bands)
	cur, ok := snapDown(parsed, p)
	if !ok {
		return "", false
	}
	for i := 0; i < absSteps(n); i++ {
		var next decimal.Decimal
		var moved bool
		if n > 0 {
			next, moved = stepUp(parsed, cur)
		} else {
			next, moved = stepDown(parsed, cur)
		}
		if !moved {
			break // clamped at an edge of the grid
		}
		cur = next
	}
	return cur.String(), true
}

func absSteps(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// stepUp moves one grid step up from p (which is on the grid). When the step
// lands in a higher band whose (coarser) grid it does not sit on, it is
// ceiled onto that band's grid so an up-step never moves backwards.
func stepUp(bands []tickBand, p decimal.Decimal) (decimal.Decimal, bool) {
	b, ok := bandAt(bands, p)
	if !ok {
		return p, false
	}
	next := p.Add(b.tick)
	nb, ok := bandAt(bands, next)
	if !ok || nb.gte.Equal(b.gte) {
		return next, true
	}
	snapped := next.Div(nb.tick).Floor().Mul(nb.tick)
	if snapped.GreaterThan(p) {
		return snapped, true
	}
	return snapped.Add(nb.tick), true
}

// stepDown moves one grid step down from p (which is on the grid): the
// largest grid point strictly below it. moved=false when there is none (the
// step would leave the grid at zero or below the lowest band).
func stepDown(bands []tickBand, p decimal.Decimal) (decimal.Decimal, bool) {
	b, ok := bandAt(bands, p)
	if !ok {
		return p, false
	}
	next := p.Sub(b.tick)
	if next.GreaterThanOrEqual(b.gte) {
		if next.IsPositive() {
			return next, true
		}
		return p, false
	}
	// Crossed the band's floor: the previous grid point belongs to the band
	// just below it — the largest of ITS grid points strictly below p.
	nb, ok := bandAt(bands, next)
	if !ok {
		return p, false
	}
	snapped := p.Div(nb.tick).Floor().Mul(nb.tick)
	if snapped.GreaterThanOrEqual(p) {
		snapped = snapped.Sub(nb.tick)
	}
	if !snapped.IsPositive() {
		return p, false
	}
	return snapped, true
}
