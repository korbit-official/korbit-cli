// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package textout

import (
	"encoding/json"
	"testing"
)

// Num groups thousands on a DISPLAY copy of a decimal string and returns
// anything that isn't a clean decimal verbatim (never guesses).
func TestNum(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"0":           "0",
		"100":         "100",
		"1000":        "1,000",
		"1234567":     "1,234,567",
		"1234567.89":  "1,234,567.89",
		"-1234.5":     "-1,234.5",
		"0.000001":    "0.000001",
		"12.34.56":    "12.34.56", // not a clean decimal → verbatim
		"abc":         "abc",
		"1,000":       "1,000", // already grouped → not plain digits → verbatim
		"  5":         "  5",   // leading space → verbatim
		"1000000.123": "1,000,000.123",
	}
	for in, want := range cases {
		if got := Num(in); got != want {
			t.Errorf("Num(%q) = %q, want %q", in, got, want)
		}
	}
}

// Table sizes each column to its widest cell (measured in runes, not bytes),
// right-aligns the columns flagged in align, and joins columns with two spaces.
func TestTable(t *testing.T) {
	headers := []string{"sym", "px"}
	rows := [][]string{{"btc", "100"}, {"eth", "9"}}
	got := Table(headers, rows, []bool{false, true})
	want := "sym   px\nbtc  100\neth    9"
	if got != want {
		t.Errorf("Table mismatch:\n got=%q\nwant=%q", got, want)
	}

	// Rune-width alignment: a multibyte cell aligns by rune count, not bytes.
	got = Table([]string{"a"}, [][]string{{"가나"}, {"x"}}, []bool{false})
	want = "a \n가나\nx "
	if got != want {
		t.Errorf("Table rune-width mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// KVBlock renders aligned "label: value" lines and SKIPS rows whose value is "".
func TestKVBlock(t *testing.T) {
	got := KVBlock([][2]string{{"a", "1"}, {"bb", ""}, {"ccc", "3"}})
	want := "a:    1\nccc:  3"
	if got != want {
		t.Errorf("KVBlock mismatch:\n got=%q\nwant=%q", got, want)
	}
	if got := KVBlock([][2]string{{"x", ""}}); got != "" {
		t.Errorf("KVBlock all-empty = %q, want empty", got)
	}
}

// ListView unwraps a truncation envelope and passes a bare array through; an
// object that isn't the envelope is ok=false.
func TestListView(t *testing.T) {
	rows, note, ok := ListView(json.RawMessage(`[1,2,3]`))
	if !ok || note != "" || string(rows) != "[1,2,3]" {
		t.Errorf("bare array: rows=%q note=%q ok=%v", rows, note, ok)
	}

	rows, note, ok = ListView(json.RawMessage(`{"data":[1],"truncated":true,"note":"partial"}`))
	if !ok || note != "partial" || string(rows) != "[1]" {
		t.Errorf("envelope: rows=%q note=%q ok=%v", rows, note, ok)
	}

	if _, _, ok := ListView(json.RawMessage(`{"foo":1}`)); ok {
		t.Errorf("non-envelope object should be ok=false")
	}
}

// OrderedKV preserves the object's field order (not Go's map ordering).
func TestOrderedKV(t *testing.T) {
	kv, ok := OrderedKV(json.RawMessage(`{"z":"1","a":"2","m":3}`))
	if !ok {
		t.Fatal("OrderedKV ok=false")
	}
	want := [][2]string{{"z", "1"}, {"a", "2"}, {"m", "3"}}
	if len(kv) != len(want) {
		t.Fatalf("len=%d, want %d (%v)", len(kv), len(want), kv)
	}
	for i, w := range want {
		if kv[i] != w {
			t.Errorf("kv[%d] = %v, want %v", i, kv[i], w)
		}
	}
	if _, ok := OrderedKV(json.RawMessage(`[1,2]`)); ok {
		t.Errorf("non-object should be ok=false")
	}
}

func TestSmallHelpers(t *testing.T) {
	if PadRune("가", 3) != "가  " {
		t.Errorf("PadRune rune width wrong: %q", PadRune("가", 3))
	}
	if PadLeftRune("가", 3) != "  가" {
		t.Errorf("PadLeftRune rune width wrong: %q", PadLeftRune("가", 3))
	}
	if YesNo(true) != "yes" || YesNo(false) != "no" {
		t.Error("YesNo")
	}
	if OrNone("") != "(none)" || OrNone("x") != "x" {
		t.Error("OrNone")
	}
	if WithTruncationNote("body", "") != "body" {
		t.Error("WithTruncationNote no-note should be unchanged")
	}
	if WithTruncationNote("body", "n") != "body\n\n⚠ n" {
		t.Errorf("WithTruncationNote: %q", WithTruncationNote("body", "n"))
	}
	if IndentLines("a\nb", "  ") != "  a\n  b" {
		t.Errorf("IndentLines: %q", IndentLines("a\nb", "  "))
	}
	if OneLine("a   b\nc", 80) != "a b c" {
		t.Errorf("OneLine collapse: %q", OneLine("a   b\nc", 80))
	}
	if OneLine("abcdef", 3) != "abc…" {
		t.Errorf("OneLine truncate: %q", OneLine("abcdef", 3))
	}
}
