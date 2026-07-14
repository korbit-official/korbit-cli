// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import "testing"

func TestFmtSymbol(t *testing.T) {
	cases := map[string]string{
		"btc_krw": "BTC/KRW",
		"eth_krw": "ETH/KRW",
		"BTC_KRW": "BTC/KRW",
		"btc":     "BTC", // no separator: just upper-cased
		"":        "",
		"a_b_c":   "A/B/C", // no length/segment assumption
	}
	for in, want := range cases {
		if got := FmtSymbol(in); got != want {
			t.Errorf("FmtSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFmtCurrency(t *testing.T) {
	cases := map[string]string{"btc": "BTC", "KRW": "KRW", "Eth": "ETH", "": ""}
	for in, want := range cases {
		if got := FmtCurrency(in); got != want {
			t.Errorf("FmtCurrency(%q) = %q, want %q", in, got, want)
		}
	}
}

// SymbolSearchKey must fold case AND the "/"↔"_" separator so a user typing
// either the displayed form or the wire form matches the underlying wire id.
func TestSymbolSearchKeyMatches(t *testing.T) {
	const wire = "btc_krw"
	queries := []string{"btc", "BTC", "Btc/krw", "BTC_krw", "btc/krw", "/krw"}
	wireKey := SymbolSearchKey(wire)
	for _, q := range queries {
		qk := SymbolSearchKey(q)
		if !contains(wireKey, qk) {
			t.Errorf("query %q (key %q) should match %q (key %q)", q, qk, wire, wireKey)
		}
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
