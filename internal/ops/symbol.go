// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import "strings"

// SplitSymbol splits a trading pair into its base and quote currencies:
// "btc_krw" → ("btc", "krw"). A symbol carries its quote currency in its second
// segment, so the currency an order is priced and settled in is always derivable
// from the symbol alone — which currency that is, is never assumed. A symbol
// with no separator yields the whole string as the base and an empty quote.
func SplitSymbol(symbol string) (base, quote string) {
	if i := strings.IndexByte(symbol, '_'); i > 0 {
		return symbol[:i], symbol[i+1:]
	}
	return symbol, ""
}

// QuoteOf returns a symbol's quote currency ("btc_krw" → "krw"), or "" when the
// symbol carries none.
func QuoteOf(symbol string) string {
	_, quote := SplitSymbol(symbol)
	return quote
}
