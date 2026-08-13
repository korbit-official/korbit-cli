// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/korbit"
)

func TestSpliceField(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"appends to object", `{"orderId":1}`, `{"orderId":1,"clientOrderId":"X"}`},
		{"empty object", `{}`, `{"clientOrderId":"X"}`},
		{"replaces existing (no dup key)", `{"orderId":1,"clientOrderId":"server"}`, `{"orderId":1,"clientOrderId":"X"}`},
		{"preserves nested + braces in strings", `{"a":{"b":1},"note":"}{"}`, `{"a":{"b":1},"note":"}{","clientOrderId":"X"}`},
		{"non-object passes through", `[1,2,3]`, `[1,2,3]`},
		{"string passes through", `"hi"`, `"hi"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(spliceField(json.RawMessage(tt.raw), "clientOrderId", "X"))
			if got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
			// Result must always be valid JSON.
			if !json.Valid([]byte(got)) {
				t.Fatalf("produced invalid JSON: %s", got)
			}
		})
	}
}

func TestOrderedObject(t *testing.T) {
	if got := string(orderedObject(nil)); got != `{}` {
		t.Fatalf("empty -> %s", got)
	}
	got := string(orderedObject([]korbit.KV{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}}))
	if got != `{"b":"2","a":"1"}` { // insertion order preserved, not sorted
		t.Fatalf("order not preserved: %s", got)
	}
}

func TestParseRange(t *testing.T) {
	if _, err := parseRange("abc", "--x", 1, 10); err == nil {
		t.Fatal("non-integer should error")
	}
	if _, err := parseRange("0", "--x", 1, 10); err == nil {
		t.Fatal("below min should error")
	}
	if _, err := parseRange("11", "--x", 1, 10); err == nil {
		t.Fatal("above max should error")
	}
	if n, err := parseRange("5", "--x", 1, 10); err != nil || n != 5 {
		t.Fatalf("valid -> %d %v", n, err)
	}
}

func TestParseDuration(t *testing.T) {
	// A bare integer has no unit — time.ParseDuration rejects it (the whole point
	// of the change: users write a unit, not raw ms).
	if _, err := parseDuration("150", "--duration", time.Millisecond, time.Second); err == nil {
		t.Fatal("unitless integer should error")
	}
	if _, err := parseDuration("abc", "--duration", time.Millisecond, time.Second); err == nil {
		t.Fatal("garbage should error")
	}
	if _, err := parseDuration("500us", "--duration", time.Millisecond, time.Second); err == nil {
		t.Fatal("below min should error")
	}
	if _, err := parseDuration("2s", "--duration", time.Millisecond, time.Second); err == nil {
		t.Fatal("above max should error")
	}
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"90s", 90 * time.Second},
		{"150ms", 150 * time.Millisecond},
		{"2m", 2 * time.Minute},
		{"1h", time.Hour},
		{"1h30m", 90 * time.Minute},
	} {
		got, err := parseDuration(tc.raw, "--duration", time.Millisecond, 7*24*time.Hour)
		if err != nil || got != tc.want {
			t.Fatalf("%s -> %s %v, want %s", tc.raw, got, err, tc.want)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		url    string
		signed bool
		ok     bool
	}{
		{"https://api.korbit.co.kr", true, true},
		{"http://127.0.0.1:9999", true, true}, // local sandbox over http is fine
		{"http://localhost:9999", true, true},
		{"http://evil.example", true, false}, // plaintext to remote host, signed -> refuse
		{"http://evil.example", false, true}, // public (unsigned) request to http is allowed
		{"ftp://x", true, false},
		{"not a url", true, false},
	}
	for _, tt := range tests {
		err := probe.ValidateBaseURL(tt.url, tt.signed)
		if (err == nil) != tt.ok {
			t.Errorf("probe.ValidateBaseURL(%q, signed=%v) err=%v, wanted ok=%v", tt.url, tt.signed, err, tt.ok)
		}
	}
}

func TestSuggestCommand(t *testing.T) {
	if got := suggestCommand("tickr"); got != "ticker" {
		t.Errorf("tickr -> %q, want ticker", got)
	}
	if got := suggestCommand("ordr"); got != "order" {
		t.Errorf("ordr -> %q, want order", got)
	}
	if got := suggestCommand("zzzzzzzz"); got != "" {
		t.Errorf("far-off input should not suggest, got %q", got)
	}
}
