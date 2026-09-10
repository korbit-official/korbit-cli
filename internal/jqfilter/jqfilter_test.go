// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package jqfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/itchyny/gojq"
)

// TestGojqPreservesJSONNumber pins the gojq library behavior this package
// depends on: when the input is decoded with json.Number (UseNumber), a program
// that does NOT do arithmetic on a number (select / identity / field access /
// comparison) leaves that number as a verbatim json.Number — no float64
// rounding. Digital X money/quantity values are decimal strings (gojq never
// touches strings), but this guards the numeric-field case too. If a future
// gojq bump reverts to eagerly normalizing decimals to float64, this fails.
func TestGojqPreservesJSONNumber(t *testing.T) {
	// A 20-significant-digit decimal and a 25-digit integer both exceed float64
	// precision, so a float64 round-trip would visibly corrupt them.
	const bigDec = "0.12345678901234567890"
	const bigInt = "1234567890123456789012345"
	src := `{"dec":` + bigDec + `,"int":` + bigInt + `,"s":"139000000.00000001"}`

	decode := func() map[string]any {
		dec := json.NewDecoder(bytes.NewReader([]byte(src)))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return v.(map[string]any)
	}

	run := func(t *testing.T, program string) map[string]any {
		t.Helper()
		q, err := gojq.Parse(program)
		if err != nil {
			t.Fatalf("parse %q: %v", program, err)
		}
		out, ok := q.Run(decode()).Next()
		if !ok {
			t.Fatalf("program %q produced no output", program)
		}
		if e, ok := out.(error); ok {
			t.Fatalf("program %q errored: %v", program, e)
		}
		m, ok := out.(map[string]any)
		if !ok {
			t.Fatalf("program %q output is %T, want map", program, out)
		}
		return m
	}

	// Non-math filters: numbers stay json.Number, exact to the original literal.
	for _, program := range []string{`.`, `select(.dec != null)`, `select((.int|tostring) != "")`} {
		m := run(t, program)
		dec, ok := m["dec"].(json.Number)
		if !ok {
			t.Fatalf("%q: dec is %T, want json.Number (precision would be lost)", program, m["dec"])
		}
		if dec.String() != bigDec {
			t.Errorf("%q: dec = %q, want %q", program, dec.String(), bigDec)
		}
		bi, ok := m["int"].(json.Number)
		if !ok || bi.String() != bigInt {
			t.Errorf("%q: int = %v (%T), want json.Number %q", program, m["int"], m["int"], bigInt)
		}
		if s, ok := m["s"].(string); !ok || s != "139000000.00000001" {
			t.Errorf("%q: money string not preserved: %v (%T)", program, m["s"], m["s"])
		}
	}

	// Boundary: arithmetic is the ONE place a number becomes float64. Documents
	// why the monitor never feeds a jq-computed number back into an order.
	m := run(t, `.dec += 0`)
	if _, ok := m["dec"].(float64); !ok {
		t.Errorf(".dec += 0: dec is %T, want float64 (arithmetic forces the conversion)", m["dec"])
	}
}

const tickerLine = `{"type":"data","channel":"ticker","symbol":"btc_krw","origin":"realtime","serverTime":1718000000000,"payload":{"data":{"close":"139000000.12345678","open":"140000000"}}}`

// TestFilterPassEmitsVerbatim: a filter that passes re-emits the ORIGINAL bytes
// byte-for-byte — exact money strings, original key order — never a re-encode.
func TestFilterPassEmitsVerbatim(t *testing.T) {
	p, err := Compile(`select((.payload.data.close|tonumber) < 140000000)`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), []byte(tickerLine))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d output lines, want 1", len(out))
	}
	if string(out[0]) != tickerLine {
		t.Errorf("filtered line not verbatim:\n got %s\nwant %s", out[0], tickerLine)
	}
}

// TestFilterDrops: a predicate that fails produces no output line.
func TestFilterDrops(t *testing.T) {
	p, err := Compile(`select((.payload.data.close|tonumber) > 200000000)`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), []byte(tickerLine))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d output lines, want 0 (dropped)", len(out))
	}
}

// TestTransformEncodesOutput: a reshaping program emits gojq's encoding, and a
// money string carried through (not arithmetic'd) stays an exact string.
func TestTransformEncodesOutput(t *testing.T) {
	p, err := Compile(`{sym: .symbol, px: .payload.data.close}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), []byte(tickerLine))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d output lines, want 1", len(out))
	}
	var got map[string]any
	dec := json.NewDecoder(bytes.NewReader(out[0]))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	want := map[string]any{"sym": "btc_krw", "px": "139000000.12345678"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("transform = %v, want %v", got, want)
	}
}

// TestMultipleOutputs: a program that yields several values emits several lines
// (e.g. exploding a trade array). Each array element is its own line.
func TestMultipleOutputs(t *testing.T) {
	line := `{"type":"data","channel":"trades","payload":{"data":[{"qty":"0.1"},{"qty":"0.9"}]}}`
	p, err := Compile(`.payload.data[] | select((.qty|tonumber) > 0.5)`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d lines, want 1 (only the 0.9 row passes)", len(out))
	}
	if string(out[0]) != `{"qty":"0.9"}` {
		t.Errorf("got %s, want {\"qty\":\"0.9\"}", out[0])
	}
}

// TestRuntimeErrorReturned: a per-event evaluation error (tonumber on a
// non-numeric string) is surfaced so the caller can skip-and-warn.
func TestRuntimeErrorReturned(t *testing.T) {
	line := `{"payload":{"data":{"close":"not-a-number"}}}`
	p, err := Compile(`select((.payload.data.close|tonumber) > 0)`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), []byte(line))
	if err == nil {
		t.Fatalf("expected a runtime error, got output %q", out)
	}
}

// TestHaltEndsOutputWithoutError: a jq `halt` ends output for the document and
// is not reported as a runtime error; lines produced before it are kept.
func TestHaltEndsOutputWithoutError(t *testing.T) {
	// `error("boom")` is a genuine runtime error → reported.
	p, err := Compile(`error("boom")`)
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := p.Run(context.Background(), []byte(tickerLine)); rerr == nil {
		t.Fatal("error(...) should surface as a runtime error")
	}

	// `., halt` emits the document then halts: one verbatim line, no error.
	p, err = Compile(`., halt`)
	if err != nil {
		t.Fatal(err)
	}
	out, rerr := p.Run(context.Background(), []byte(tickerLine))
	if rerr != nil {
		t.Fatalf("halt should not be a runtime error, got %v", rerr)
	}
	if len(out) != 1 || string(out[0]) != tickerLine {
		t.Fatalf("expected the one line produced before halt, got %d: %q", len(out), out)
	}
}

// TestCompileError: an invalid jq program fails at Compile with a user-facing
// message naming --jq.
func TestCompileError(t *testing.T) {
	if _, err := Compile(`select(`); err == nil {
		t.Fatal("expected a compile error for an incomplete program")
	}
}
