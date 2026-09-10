// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
)

// A pair's order value bounds, as published by GET /v2/currencyPairs. The
// bounds are per-pair configuration, so they are read per pair and never
// carried as one constant: a figure taken from one market rejects orders
// another market accepts, and it does so silently, in the direction that costs
// the caller a fill.

// OrderValueBounds is one pair's order value bounds, denominated in the pair's
// quote currency. An empty Min or Max is a bound the pair does not publish: the
// corresponding check is skipped and the server decides. Empty is never read as
// zero, and never filled in from another pair.
type OrderValueBounds struct {
	QuoteCurrency string // the unit of Min and Max, as the pair publishes it
	Min           string // decimal string; "" = the pair publishes no minimum
	Max           string // decimal string; "" = the pair publishes no maximum
}

// The KRW market's documented order value bounds, used ONLY as the legacy
// fallback below. They are the figures `apidocs.yaml` has always carried for the
// KRW market; every other market's bounds have to come from the listing, because
// nothing here can know them.
const (
	krwQuote    = "krw"
	krwMinOrder = "5000"
	krwMaxOrder = "1000000000"
)

// ResolveBoundsForSymbol reads symbol's bounds from a GET /v2/currencyPairs
// listing, falling back to the KRW market's documented figures against a server
// that predates the bound fields.
//
// A listing that publishes a pair's identity (`quoteCurrency`) is the new shape,
// and is trusted VERBATIM: a bound it omits is a figure the pair does not
// publish, so that check is skipped and the server decides. Only when nothing in
// the listing publishes a currency — an older server, or a listing that never
// arrived — does the fallback apply, and then only to a KRW-quoted symbol.
//
// A symbol the listing does not carry at all yields no unit and no bounds, so
// BOTH checks are skipped and the server decides. That is the contract, not an
// accident of the caller: such a symbol is one this server does not trade, and a
// listing that publishes currencies is not a pre-publication server just because
// it lacks one entry — inventing the KRW figures for it would apply another
// market's bound to a market that does not exist here. Reachable by a
// `--symbols` flag naming a pair the server does not list, and by a pair
// delisted between two listing reads.
//
// The fallback exists so upgrading the CLI ahead of the server (or pointing it
// at an older sandbox) does not silently drop the below-min/above-max warnings
// the KRW market has always raised. A missing warning reads exactly like a safe
// order, so losing one costs the caller a server-side rejection they were
// previously told about. Delete this once no reachable server serves the old
// shape.
func ResolveBoundsForSymbol(pairs []rawapi.Pair, symbol string) OrderValueBounds {
	b := BoundsForSymbol(pairs, symbol)
	// The entry said something about this pair — its currency, or a bound. Either
	// way it is the authority, and a published figure is never thrown away for a
	// constant. (A bound without a currency violates the spec, which marks
	// quoteCurrency non-optional; honour it anyway rather than discard it.)
	if b.QuoteCurrency != "" || b.Min != "" || b.Max != "" {
		if b.QuoteCurrency == "" {
			b.QuoteCurrency = QuoteOf(strings.ToLower(symbol))
		}
		return b
	}
	if listingPublishesCurrencies(pairs) {
		// Unlisted symbol on a publishing server: no unit, no bounds, both checks
		// skipped. Returned explicitly rather than as b, so the empty result reads as
		// the decision it is instead of a miss falling through.
		return OrderValueBounds{}
	}
	return legacyBoundsForSymbol(symbol)
}

// listingPublishesCurrencies reports whether the listing carries the pair
// identity fields at all. One entry is enough: they are non-optional on the
// documented shape, so a server that publishes them anywhere publishes them
// everywhere. It distinguishes "this server cannot publish a bound" from "this
// pair publishes no figure for one" — the fallback must never override the latter.
func listingPublishesCurrencies(pairs []rawapi.Pair) bool {
	for _, p := range pairs {
		if strings.TrimSpace(p.QuoteCurrency) != "" {
			return true
		}
	}
	return false
}

// legacyBoundsForSymbol derives what a pre-publication server cannot tell us:
// the quote currency from the symbol's second segment, and the bounds from the
// documented KRW figures when that segment is krw. Any other quote currency
// yields the unit alone — inventing a figure for a market whose bounds this code
// has never known is the failure the whole change removed.
func legacyBoundsForSymbol(symbol string) OrderValueBounds {
	quote := QuoteOf(strings.ToLower(symbol))
	if quote != krwQuote {
		return OrderValueBounds{QuoteCurrency: quote}
	}
	return OrderValueBounds{QuoteCurrency: quote, Min: krwMinOrder, Max: krwMaxOrder}
}

// BoundsForSymbol returns symbol's currencies and order value bounds from a
// GET /v2/currencyPairs listing, exactly as the listing states them. A symbol
// the listing does not carry yields the zero value — no unit and no bounds, so
// every bound check is skipped rather than run against a figure that belongs to
// a different pair. Callers wanting the legacy fallback use
// ResolveBoundsForSymbol.
func BoundsForSymbol(pairs []rawapi.Pair, symbol string) OrderValueBounds {
	want := strings.ToLower(symbol)
	for _, p := range pairs {
		if strings.ToLower(p.Symbol) != want {
			continue
		}
		return OrderValueBounds{
			// Trimmed as well as folded, so this agrees with
			// listingPublishesCurrencies on what counts as a published currency.
			QuoteCurrency: strings.ToLower(strings.TrimSpace(p.QuoteCurrency)),
			Min:           strings.TrimSpace(p.MinOrderValue),
			Max:           strings.TrimSpace(p.MaxOrderValue),
		}
	}
	return OrderValueBounds{}
}

// NotionalBoundWarnings raises the below-min / above-max warnings for an order
// notional (an exact decimal string, in the pair's quote currency) against the
// bounds that pair publishes. It is the ONE place those two warnings are worded
// and thresholded, so every caller raises exactly the warnings placement would
// rather than re-deriving a threshold of its own. The bounds are book-independent,
// which is why AnalyzePlace still runs this check on a book with no resting orders
// on the order's side: the notional is known from price × qty (or amt) alone, and a
// below-min first maker is the mistake such a book makes MORE likely, not less.
//
// fallbackQuote names the unit when the pair entry carries no currency of its
// own (the symbol's second segment). A bound the pair does not publish is
// skipped, leaving the server as the authority, and an unparseable notional
// raises nothing — neither can support a rejection claim.
func NotionalBoundWarnings(notional string, bounds OrderValueBounds, fallbackQuote string) []PlaceWarning {
	d, err := decimal.NewFromString(notional)
	if err != nil {
		return nil
	}
	unit := strings.ToUpper(bounds.QuoteCurrency)
	if unit == "" {
		unit = strings.ToUpper(fallbackQuote)
	}
	var ws []PlaceWarning
	add := func(code PlaceWarningCode, format string, a ...any) {
		ws = append(ws, PlaceWarning{Code: code, Message: fmt.Sprintf(format, a...), Format: format, Args: a})
	}
	if min, ok := bounds.min(); ok && d.LessThan(min) {
		add(WarnNotionalBelowMin, "order notional ~%s %s is below the %s %s minimum — it will be rejected.", dec(d), unit, dec(min), unit)
	}
	if max, ok := bounds.max(); ok && d.GreaterThan(max) {
		add(WarnNotionalAboveMax, "order notional ~%s %s exceeds the %s %s maximum — it will be rejected.", dec(d), unit, dec(max), unit)
	}
	return ws
}

// min/max parse a bound. ok=false covers both "the pair publishes none" and an
// unparseable figure: neither can support a rejection claim, so the check is
// skipped in both cases rather than guessed at.
func (b OrderValueBounds) min() (decimal.Decimal, bool) { return parseBound(b.Min) }
func (b OrderValueBounds) max() (decimal.Decimal, bool) { return parseBound(b.Max) }

func parseBound(s string) (decimal.Decimal, bool) {
	if s == "" {
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, false
	}
	return d, true
}
