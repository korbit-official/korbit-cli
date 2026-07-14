// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import "testing"

func TestOrderedParamsPreserveInsertionOrder(t *testing.T) {
	p := &orderedParams{}
	// Deliberately out of alphabetical order: a sorting encoder would reorder.
	p.add("timestamp", "1700000000000")
	p.add("symbol", "btc_krw")
	p.add("amt", "50000")
	got := p.encode()
	want := "timestamp=1700000000000&symbol=btc_krw&amt=50000"
	if got != want {
		t.Fatalf("order not preserved:\n got %q\nwant %q", got, want)
	}
}

func TestOrderedParamsEscaping(t *testing.T) {
	p := &orderedParams{}
	p.add("a b", "c+d/e")
	got := p.encode()
	want := "a+b=c%2Bd%2Fe"
	if got != want {
		t.Fatalf("escaping: got %q want %q", got, want)
	}
}
