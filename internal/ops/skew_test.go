// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
)

// TestWithTimeSkew pins the skew math the `time` op appends: offsetMs is
// serverClock - localMidpoint (round-trip latency charged to rtt, not the
// clock), the server `time` is preserved and stays first, and the guards leave
// the body untouched when the sample can't be trusted.
func TestWithTimeSkew(t *testing.T) {
	// mid = (900+1100)/2 = 1000; server = 1000 => offset 0, rtt 200, unc 100.
	got := string(withTimeSkew([]byte(`{"time":1000}`), apiclient.Meta{Attempts: 1, StartedAtMs: 900, FinishedAtMs: 1100}))
	want := `{"time":1000,"localTime":1000,"offsetMs":0,"rttMs":200,"uncertaintyMs":100}`
	if got != want {
		t.Fatalf("skew:\n got %s\nwant %s", got, want)
	}

	// A server clock behind the local midpoint yields a negative offset (local
	// runs ahead): server 800, mid 1000 => offset -200.
	got = string(withTimeSkew([]byte(`{"time":800}`), apiclient.Meta{Attempts: 1, StartedAtMs: 900, FinishedAtMs: 1100}))
	if want := `{"time":800,"localTime":1000,"offsetMs":-200,"rttMs":200,"uncertaintyMs":100}`; got != want {
		t.Fatalf("negative offset:\n got %s\nwant %s", got, want)
	}

	// Guards: a retry (Attempts>1), an unbracketed meta, or a wall clock that
	// stepped backward mid-call (finish < start) must leave the body alone rather
	// than report a skew derived from a bad bracket.
	for name, meta := range map[string]apiclient.Meta{
		"retry":        {Attempts: 2, StartedAtMs: 900, FinishedAtMs: 1100},
		"unbracketed":  {Attempts: 1, StartedAtMs: 0, FinishedAtMs: 0},
		"backwardJump": {Attempts: 1, StartedAtMs: 1100, FinishedAtMs: 900},
	} {
		if got := string(withTimeSkew([]byte(`{"time":1000}`), meta)); got != `{"time":1000}` {
			t.Fatalf("%s: expected untouched body, got %s", name, got)
		}
	}

	// A body without a usable `time` field is returned unchanged (no skew from a
	// zero/absent server clock).
	if got := string(withTimeSkew([]byte(`{}`), apiclient.Meta{Attempts: 1, StartedAtMs: 900, FinishedAtMs: 1100})); got != `{}` {
		t.Fatalf("missing time: expected untouched body, got %s", got)
	}
}

// TestWithFields checks the additive splice preserves existing fields/order,
// handles an empty object, and no-ops on a non-object.
func TestWithFields(t *testing.T) {
	if got := string(withFields([]byte(`{"a":1}`), numField{"b", 2}, numField{"c", 3})); got != `{"a":1,"b":2,"c":3}` {
		t.Fatalf("append: got %s", got)
	}
	if got := string(withFields([]byte(`{}`), numField{"b", 2})); got != `{"b":2}` {
		t.Fatalf("empty object: got %s", got)
	}
	// A string value ending in '}' must not confuse the last-byte closing-brace
	// slice: the real closing brace is still the trimmed object's final byte.
	if got := string(withFields([]byte(`{"a":"}"}`), numField{"b", 2})); got != `{"a":"}","b":2}` {
		t.Fatalf("string-with-brace value: got %s", got)
	}
	if got := string(withFields([]byte(`[1,2]`), numField{"b", 2})); got != `[1,2]` {
		t.Fatalf("non-object should be unchanged: got %s", got)
	}
}
