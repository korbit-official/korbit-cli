// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// taVM is a bare runtime with only the `ta` global installed — the indicator
// binding is synchronous and needs no event loop or credentials.
func taVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	if err := installTA(vm); err != nil {
		t.Fatalf("installTA: %v", err)
	}
	return vm
}

func run(t *testing.T, vm *goja.Runtime, src string) goja.Value {
	t.Helper()
	v, err := vm.RunString(src)
	if err != nil {
		t.Fatalf("script error: %v\nsrc: %s", err, src)
	}
	return v
}

func TestTABatchWarmupIsNull(t *testing.T) {
	vm := taVM(t)
	// SMA(3) over 1..5 -> [null, null, 2, 3, 4]; warm-up is null, not 0.
	out := run(t, vm, `ta.sma([1,2,3,4,5], 3)`).Export().([]interface{})
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5", len(out))
	}
	if out[0] != nil || out[1] != nil {
		t.Errorf("warm-up should be null, got %v %v", out[0], out[1])
	}
	if out[2].(float64) != 2 || out[4].(float64) != 4 {
		t.Errorf("values wrong: %v", out)
	}
}

func TestTAAcceptsDecimalStrings(t *testing.T) {
	vm := taVM(t)
	// API prices arrive as decimal strings; ta must accept them.
	out := run(t, vm, `ta.sma(["10.0","20.0","30.0"], 3)`).Export().([]interface{})
	if out[2].(float64) != 20 {
		t.Errorf("string input SMA = %v, want 20", out[2])
	}
}

func TestTAMultiOutputShape(t *testing.T) {
	vm := taVM(t)
	// macd returns {macd, signal, hist}, each an array of the input length.
	run(t, vm, `
		var closes = [];
		for (var i = 0; i < 100; i++) closes.push(100 + Math.sin(i/5));
		var m = ta.macd(closes, 12, 26, 9);
		if (!Array.isArray(m.macd) || !Array.isArray(m.signal) || !Array.isArray(m.hist))
			throw new Error("macd components must be arrays");
		if (m.macd.length !== 100) throw new Error("length mismatch");
		// histogram is null through the 33-bar lookback, finite after.
		if (m.hist[32] !== null) throw new Error("hist[32] should be null (warm-up)");
		if (typeof m.hist[50] !== "number") throw new Error("hist[50] should be a number");
	`)
}

func TestTAStreamingMatchesBatch(t *testing.T) {
	vm := taVM(t)
	// A windowed streaming indicator must equal the batch value at the same bar.
	run(t, vm, `
		var xs = [];
		for (var i = 0; i < 60; i++) xs.push(50 + (i % 7));
		var batch = ta.sma(xs, 5);
		var s = ta.stream.sma(5);
		for (var i = 0; i < xs.length; i++) {
			var v = s.update(xs[i]);
			var b = batch[i];
			if (b === null) {
				if (v !== null) throw new Error("bar " + i + ": stream " + v + " but batch null");
			} else if (Math.abs(v - b) > 1e-9) {
				throw new Error("bar " + i + ": stream " + v + " != batch " + b);
			}
		}
	`)
}

func TestTAStreamMultiOutputObject(t *testing.T) {
	vm := taVM(t)
	v := run(t, vm, `
		var s = ta.stream.macd(12, 26, 9);
		var last = null;
		for (var i = 0; i < 80; i++) last = s.update(100 + Math.sin(i/4));
		// before warm-up update returns {macd:null,...}; after, numbers.
		last;
	`)
	obj := v.ToObject(vm)
	for _, k := range []string{"macd", "signal", "hist"} {
		if obj.Get(k) == nil {
			t.Errorf("stream macd result missing %q", k)
		}
	}
	if mv := obj.Get("macd"); goja.IsNull(mv) || math.IsNaN(mv.ToFloat()) {
		t.Errorf("macd should be a number after 80 bars, got %v", mv)
	}
}

func TestTAInvalidInputThrows(t *testing.T) {
	vm := taVM(t)
	bad := []string{
		`ta.sma([1,2,3], 0)`,          // period below minimum
		`ta.rsi([1,2,3], 1)`,          // RSI min period is 2
		`ta.macd([1,2,3], 26, 12, 9)`, // fast >= slow
		`ta.sma([1,"x",3], 2)`,        // non-numeric element
		`ta.sma(42, 3)`,               // not an array
		`ta.atr([1,2],[1],[1,2],14)`,  // mismatched OHLC lengths
		`ta.stream.sma(0)`,            // bad stream period
	}
	for _, src := range bad {
		if _, err := vm.RunString(src); err == nil {
			t.Errorf("expected throw for: %s", src)
		}
	}
}

// wantTASurface is the EXACT set of dotted ta.* / ta.stream.* method names —
// the golden surface pin for the indicator API, in the spirit of
// TestGoldenKorbitMethodSet. Adding an indicator adds a line here; renaming or
// removing one breaks an existing line. Extend additively.
var wantTASurface = []string{
	"ta.adx", "ta.aroon", "ta.atr", "ta.bbands", "ta.cci", "ta.dema", "ta.ema",
	"ta.macd", "ta.mfi", "ta.mom", "ta.natr", "ta.obv", "ta.roc", "ta.rsi",
	"ta.sma", "ta.stoch", "ta.stochrsi", "ta.tema", "ta.willr", "ta.wma",
	"ta.stream.adx", "ta.stream.aroon", "ta.stream.atr", "ta.stream.bbands",
	"ta.stream.cci", "ta.stream.dema", "ta.stream.ema", "ta.stream.macd",
	"ta.stream.mfi", "ta.stream.mom", "ta.stream.natr", "ta.stream.obv",
	"ta.stream.roc", "ta.stream.rsi", "ta.stream.sma", "ta.stream.stoch",
	"ta.stream.stochrsi", "ta.stream.tema", "ta.stream.willr", "ta.stream.wma",
}

func TestGoldenTASurface(t *testing.T) {
	vm := taVM(t)
	v := run(t, vm, `
		(function () {
			var out = [];
			Object.keys(ta).forEach(function (k) {
				var val = ta[k];
				if (typeof val === 'function') { out.push('ta.' + k); return; }
				if (val && typeof val === 'object') {
					Object.keys(val).forEach(function (k2) {
						if (typeof val[k2] === 'function') out.push('ta.' + k + '.' + k2);
					});
				}
			});
			return out;
		})()
	`)
	var got []string
	for _, e := range v.Export().([]interface{}) {
		got = append(got, e.(string))
	}
	sort.Strings(got)
	want := append([]string(nil), wantTASurface...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ta surface drift.\n got: %v\nwant: %v\n(extend the golden list ADDITIVELY)", got, want)
	}
}

// TestTAUsableInWhere pins the deliberate decision that ta — being synchronous
// and pure — IS available inside --where, unlike the async api.*/db. surface
// which is gated off there.
func TestTAUsableInWhere(t *testing.T) {
	r := newTestRuntime(t, Options{
		Init:  `globalThis.xs = [1, 2, 3, 4, 5];`,
		Where: `ta.sma(xs, 3)[4] === 4 && typeof ta.stream.rsi(14).update === 'function'`,
	})
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("ta should be usable in --where: ok=%v err=%v", ok, err)
	}
}

func TestTAOBVStreamMatchesBatch(t *testing.T) {
	vm := taVM(t)
	run(t, vm, `
		var c = [], v = [];
		for (var i = 0; i < 30; i++) { c.push(100 + Math.sin(i)); v.push(10 + i); }
		var batch = ta.obv(c, v);
		var s = ta.stream.obv();
		for (var i = 0; i < c.length; i++) {
			var got = s.update(c[i], v[i]);
			if (Math.abs(got - batch[i]) > 1e-9)
				throw new Error("OBV bar " + i + ": stream " + got + " != batch " + batch[i]);
		}
	`)
}
