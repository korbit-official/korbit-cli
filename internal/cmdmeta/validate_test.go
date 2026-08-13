// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cmdmeta

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"90s", 90 * time.Second},
		{"150ms", 150 * time.Millisecond},
		{"1h30m", 90 * time.Minute},
		{" 2m ", 2 * time.Minute}, // trimmed
	} {
		got, ok := ParseDuration(tc.raw)
		if !ok || got != tc.want {
			t.Fatalf("ParseDuration(%q) = %s %v, want %s", tc.raw, got, ok, tc.want)
		}
	}
	for _, raw := range []string{"90", "abc", "", "1d"} { // unitless / garbage / empty / unsupported unit
		if _, ok := ParseDuration(raw); ok {
			t.Fatalf("ParseDuration(%q) should fail", raw)
		}
	}
}

// KindDuration: NormalizeValue parses-and-canonicalizes (no Min/Max bounds —
// those are not carried for durations), CoerceValue requires a string.
func TestDurationKind(t *testing.T) {
	p := Param{Flag: "duration", API: "duration", Kind: KindDuration}
	if got, err := NormalizeValue(p, "1h30m", "--duration"); err != nil || got != "1h30m0s" {
		t.Fatalf("NormalizeValue duration: got %q err %v", got, err)
	}
	if _, err := NormalizeValue(p, "90", "--duration"); err == nil {
		t.Fatalf("unitless duration must be rejected")
	}
	if _, _, err := CoerceValue(p, float64(90), "--duration"); err == nil {
		t.Fatalf("a numeric duration must be rejected (needs a unit, must be a string)")
	}
	raw, present, err := CoerceValue(p, "90s", "--duration")
	if err != nil || !present || raw != "90s" {
		t.Fatalf("a string duration must pass: raw=%q present=%v err=%v", raw, present, err)
	}
}

func TestNormalizeValueMoneyStaysString(t *testing.T) {
	p := Param{Flag: "price", API: "price", Kind: KindDecimal}
	got, err := NormalizeValue(p, "0.001", "--price")
	if err != nil || got != "0.001" {
		t.Fatalf("decimal passthrough: got %q err %v", got, err)
	}
	got, err = NormalizeValue(p, "001.2300", "--price")
	if err != nil || got != "001.2300" {
		t.Fatalf("decimal string must pass through verbatim: got %q err %v", got, err)
	}
	if _, err := NormalizeValue(p, "0", "--price"); err == nil {
		t.Fatalf("zero must be rejected")
	}
	if _, err := NormalizeValue(p, "1.0e3", "--price"); err == nil {
		t.Fatalf("exponent must be rejected")
	}
	for _, raw := range []string{".1", "1.", "-1", "1,000"} {
		if _, err := NormalizeValue(p, raw, "--price"); err == nil {
			t.Fatalf("%q must be rejected", raw)
		}
	}
}

func TestCoerceValueMoneyMustBeString(t *testing.T) {
	p := Param{Flag: "price", API: "price", Kind: KindDecimal}
	if _, _, err := CoerceValue(p, float64(0.001), "--price"); err == nil {
		t.Fatalf("a float decimal must be rejected before any request is built")
	}
	raw, present, err := CoerceValue(p, "0.001", "--price")
	if err != nil || !present || raw != "0.001" {
		t.Fatalf("a string decimal must pass: raw=%q present=%v err=%v", raw, present, err)
	}
}
