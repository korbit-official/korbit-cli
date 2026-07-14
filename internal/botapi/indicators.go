// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"math"
	"strings"

	"github.com/dop251/goja"
	"github.com/korbit-official/korbit-cli/internal/indicators"
	"github.com/shopspring/decimal"
)

// installTA installs the synchronous `ta` global: technical-indicator helpers
// over internal/indicators. Indicator math is fast local compute, so these
// methods run on the loop goroutine and return directly (no promises).
//
// Inputs accept JS numbers or decimal strings and are converted to float64 for
// analysis — the indicator layer is the one place a price string becomes a
// float. Outputs are numbers (NaN warm-up positions become null) and are
// signals only: never feed an indicator output back into an order price,
// quantity, or balance, which stay decimal strings.
//
//   - ta.<name>(...) returns the full series (an array, or an object of arrays
//     for multi-component indicators), aligned to the input with leading null
//     until warmed up.
//   - ta.stream.<name>(...) returns a stateful object whose update(...) feeds
//     one sample (or one OHLCV bar) and returns the latest value(s) — null
//     until warmed up — for reacting to a live feed bar by bar.
func installTA(vm *goja.Runtime) error {
	ta := vm.NewObject()

	// --- batch: single series, single output ---
	batch1 := func(name string, fn func([]float64, int) ([]float64, error)) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			real := taReals(vm, call, 0, name, "values")
			period := taInt(vm, call, 1, name, "period")
			out, err := fn(real, period)
			return taArr(vm, mustSeries(vm, name, out, err))
		}
	}
	// --- batch: (high, low, close), single output ---
	batchHLC := func(name string, fn func(h, l, c []float64, p int) ([]float64, error)) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			h := taReals(vm, call, 0, name, "high")
			l := taReals(vm, call, 1, name, "low")
			c := taReals(vm, call, 2, name, "close")
			period := taInt(vm, call, 3, name, "period")
			out, err := fn(h, l, c, period)
			return taArr(vm, mustSeries(vm, name, out, err))
		}
	}

	must(ta.Set("sma", batch1("sma", indicators.SMA)))
	must(ta.Set("ema", batch1("ema", indicators.EMA)))
	must(ta.Set("wma", batch1("wma", indicators.WMA)))
	must(ta.Set("dema", batch1("dema", indicators.DEMA)))
	must(ta.Set("tema", batch1("tema", indicators.TEMA)))
	must(ta.Set("rsi", batch1("rsi", indicators.RSI)))
	must(ta.Set("roc", batch1("roc", indicators.ROC)))
	must(ta.Set("mom", batch1("mom", indicators.Mom)))
	must(ta.Set("cci", batchHLC("cci", indicators.CCI)))
	must(ta.Set("willr", batchHLC("willr", indicators.WillR)))
	must(ta.Set("atr", batchHLC("atr", indicators.ATR)))
	must(ta.Set("natr", batchHLC("natr", indicators.NATR)))
	must(ta.Set("adx", batchHLC("adx", indicators.ADX)))

	must(ta.Set("macd", func(call goja.FunctionCall) goja.Value {
		real := taReals(vm, call, 0, "macd", "values")
		fast := taInt(vm, call, 1, "macd", "fastPeriod")
		slow := taInt(vm, call, 2, "macd", "slowPeriod")
		signal := taInt(vm, call, 3, "macd", "signalPeriod")
		r, err := indicators.MACD(real, fast, slow, signal)
		mustNoErr(vm, "macd", err)
		o := vm.NewObject()
		must(o.Set("macd", taArr(vm, r.MACD)))
		must(o.Set("signal", taArr(vm, r.Signal)))
		must(o.Set("hist", taArr(vm, r.Hist)))
		return o
	}))

	must(ta.Set("bbands", func(call goja.FunctionCall) goja.Value {
		real := taReals(vm, call, 0, "bbands", "values")
		period := taInt(vm, call, 1, "bbands", "period")
		up := taFloatOpt(vm, call, 2, 2.0)
		dn := taFloatOpt(vm, call, 3, 2.0)
		maType := taIntOpt(vm, call, 4, 0)
		r, err := indicators.BBands(real, period, up, dn, maType)
		mustNoErr(vm, "bbands", err)
		o := vm.NewObject()
		must(o.Set("upper", taArr(vm, r.Upper)))
		must(o.Set("middle", taArr(vm, r.Middle)))
		must(o.Set("lower", taArr(vm, r.Lower)))
		return o
	}))

	must(ta.Set("stoch", func(call goja.FunctionCall) goja.Value {
		h := taReals(vm, call, 0, "stoch", "high")
		l := taReals(vm, call, 1, "stoch", "low")
		c := taReals(vm, call, 2, "stoch", "close")
		fastK := taInt(vm, call, 3, "stoch", "fastKPeriod")
		slowK := taInt(vm, call, 4, "stoch", "slowKPeriod")
		slowD := taInt(vm, call, 5, "stoch", "slowDPeriod")
		kMA := taIntOpt(vm, call, 6, 0)
		dMA := taIntOpt(vm, call, 7, 0)
		r, err := indicators.Stoch(h, l, c, fastK, slowK, kMA, slowD, dMA)
		mustNoErr(vm, "stoch", err)
		return kd(vm, r)
	}))

	must(ta.Set("stochrsi", func(call goja.FunctionCall) goja.Value {
		real := taReals(vm, call, 0, "stochrsi", "values")
		period := taInt(vm, call, 1, "stochrsi", "period")
		fastK := taInt(vm, call, 2, "stochrsi", "fastKPeriod")
		fastD := taInt(vm, call, 3, "stochrsi", "fastDPeriod")
		dMA := taIntOpt(vm, call, 4, 0)
		r, err := indicators.StochRSI(real, period, fastK, fastD, dMA)
		mustNoErr(vm, "stochrsi", err)
		return kd(vm, r)
	}))

	must(ta.Set("aroon", func(call goja.FunctionCall) goja.Value {
		h := taReals(vm, call, 0, "aroon", "high")
		l := taReals(vm, call, 1, "aroon", "low")
		period := taInt(vm, call, 2, "aroon", "period")
		r, err := indicators.Aroon(h, l, period)
		mustNoErr(vm, "aroon", err)
		o := vm.NewObject()
		must(o.Set("down", taArr(vm, r.Down)))
		must(o.Set("up", taArr(vm, r.Up)))
		return o
	}))

	must(ta.Set("obv", func(call goja.FunctionCall) goja.Value {
		c := taReals(vm, call, 0, "obv", "close")
		v := taReals(vm, call, 1, "obv", "volume")
		out, err := indicators.OBV(c, v)
		return taArr(vm, mustSeries(vm, "obv", out, err))
	}))

	must(ta.Set("mfi", func(call goja.FunctionCall) goja.Value {
		h := taReals(vm, call, 0, "mfi", "high")
		l := taReals(vm, call, 1, "mfi", "low")
		c := taReals(vm, call, 2, "mfi", "close")
		v := taReals(vm, call, 3, "mfi", "volume")
		period := taInt(vm, call, 4, "mfi", "period")
		out, err := indicators.MFI(h, l, c, v, period)
		return taArr(vm, mustSeries(vm, "mfi", out, err))
	}))

	// --- streaming ---
	stream := vm.NewObject()

	// streamP: a constructor taking a single period.
	streamP := func(name string, ctor func(int) (*indicators.Stream, error), comps []string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			period := taInt(vm, call, 0, "stream."+name, "period")
			s, err := ctor(period)
			mustNoErr(vm, "stream."+name, err)
			return streamObject(vm, s, comps)
		}
	}

	must(stream.Set("sma", streamP("sma", indicators.NewSMA, nil)))
	must(stream.Set("ema", streamP("ema", indicators.NewEMA, nil)))
	must(stream.Set("wma", streamP("wma", indicators.NewWMA, nil)))
	must(stream.Set("dema", streamP("dema", indicators.NewDEMA, nil)))
	must(stream.Set("tema", streamP("tema", indicators.NewTEMA, nil)))
	must(stream.Set("rsi", streamP("rsi", indicators.NewRSI, nil)))
	must(stream.Set("roc", streamP("roc", indicators.NewROC, nil)))
	must(stream.Set("mom", streamP("mom", indicators.NewMom, nil)))
	must(stream.Set("cci", streamP("cci", indicators.NewCCI, nil)))
	must(stream.Set("willr", streamP("willr", indicators.NewWillR, nil)))
	must(stream.Set("atr", streamP("atr", indicators.NewATR, nil)))
	must(stream.Set("natr", streamP("natr", indicators.NewNATR, nil)))
	must(stream.Set("adx", streamP("adx", indicators.NewADX, nil)))
	must(stream.Set("aroon", streamP("aroon", indicators.NewAroon, []string{"down", "up"})))
	must(stream.Set("mfi", streamP("mfi", indicators.NewMFI, nil)))

	must(stream.Set("macd", func(call goja.FunctionCall) goja.Value {
		fast := taInt(vm, call, 0, "stream.macd", "fastPeriod")
		slow := taInt(vm, call, 1, "stream.macd", "slowPeriod")
		signal := taInt(vm, call, 2, "stream.macd", "signalPeriod")
		s, err := indicators.NewMACD(fast, slow, signal)
		mustNoErr(vm, "stream.macd", err)
		return streamObject(vm, s, []string{"macd", "signal", "hist"})
	}))

	must(stream.Set("bbands", func(call goja.FunctionCall) goja.Value {
		period := taInt(vm, call, 0, "stream.bbands", "period")
		up := taFloatOpt(vm, call, 1, 2.0)
		dn := taFloatOpt(vm, call, 2, 2.0)
		maType := taIntOpt(vm, call, 3, 0)
		s, err := indicators.NewBBands(period, up, dn, maType)
		mustNoErr(vm, "stream.bbands", err)
		return streamObject(vm, s, []string{"upper", "middle", "lower"})
	}))

	must(stream.Set("stoch", func(call goja.FunctionCall) goja.Value {
		fastK := taInt(vm, call, 0, "stream.stoch", "fastKPeriod")
		slowK := taInt(vm, call, 1, "stream.stoch", "slowKPeriod")
		slowD := taInt(vm, call, 2, "stream.stoch", "slowDPeriod")
		kMA := taIntOpt(vm, call, 3, 0)
		dMA := taIntOpt(vm, call, 4, 0)
		s, err := indicators.NewStoch(fastK, slowK, kMA, slowD, dMA)
		mustNoErr(vm, "stream.stoch", err)
		return streamObject(vm, s, []string{"k", "d"})
	}))

	must(stream.Set("stochrsi", func(call goja.FunctionCall) goja.Value {
		period := taInt(vm, call, 0, "stream.stochrsi", "period")
		fastK := taInt(vm, call, 1, "stream.stochrsi", "fastKPeriod")
		fastD := taInt(vm, call, 2, "stream.stochrsi", "fastDPeriod")
		dMA := taIntOpt(vm, call, 3, 0)
		s, err := indicators.NewStochRSI(period, fastK, fastD, dMA)
		mustNoErr(vm, "stream.stochrsi", err)
		return streamObject(vm, s, []string{"k", "d"})
	}))

	must(stream.Set("obv", func(goja.FunctionCall) goja.Value {
		return streamObject(vm, indicators.NewOBV(), nil)
	}))

	must(ta.Set("stream", stream))
	return vm.Set("ta", ta)
}

// streamObject wraps a *Stream as a JS object with an update(...) method.
func streamObject(vm *goja.Runtime, s *indicators.Stream, comps []string) *goja.Object {
	obj := vm.NewObject()
	arity := s.Arity()
	must(obj.Set("update", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) != arity {
			panic(vm.NewTypeError("update: expected %d value(s), got %d", arity, len(call.Arguments)))
		}
		vals := make([]float64, arity)
		for i := 0; i < arity; i++ {
			f, ok := taNum(call.Arguments[i])
			if !ok {
				panic(vm.NewTypeError("update: argument %d is not a number", i+1))
			}
			vals[i] = f
		}
		out, err := s.Update(vals...)
		mustNoErr(vm, "update", err)
		if comps == nil {
			return scalarOrNull(vm, out[0])
		}
		o := vm.NewObject()
		for i, name := range comps {
			must(o.Set(name, scalarOrNull(vm, out[i])))
		}
		return o
	}))
	return obj
}

// kd renders a {k, d} result.
func kd(vm *goja.Runtime, r indicators.StochResult) goja.Value {
	o := vm.NewObject()
	must(o.Set("k", taArr(vm, r.K)))
	must(o.Set("d", taArr(vm, r.D)))
	return o
}

// --- argument + result conversion ------------------------------------------

// taReals reads argument idx as an array of numbers or decimal strings.
func taReals(vm *goja.Runtime, call goja.FunctionCall, idx int, name, arg string) []float64 {
	if idx >= len(call.Arguments) {
		panic(vm.NewTypeError("ta.%s: missing argument %q (an array of numbers)", name, arg))
	}
	exp := call.Arguments[idx].Export()
	arr, ok := exp.([]interface{})
	if !ok {
		panic(vm.NewTypeError("ta.%s: %q must be an array", name, arg))
	}
	out := make([]float64, len(arr))
	for i, e := range arr {
		f, ok := numFromExport(e)
		if !ok {
			panic(vm.NewTypeError("ta.%s: %s[%d] is not a number", name, arg, i))
		}
		out[i] = f
	}
	return out
}

// taInt reads a required integer argument (a JS number or numeric string).
func taInt(vm *goja.Runtime, call goja.FunctionCall, idx int, name, arg string) int {
	if idx >= len(call.Arguments) {
		panic(vm.NewTypeError("ta.%s: missing argument %q", name, arg))
	}
	f, ok := taNum(call.Arguments[idx])
	if !ok {
		panic(vm.NewTypeError("ta.%s: %q must be a number", name, arg))
	}
	return int(f)
}

// taIntOpt reads an optional integer argument, defaulting when absent/undefined.
func taIntOpt(vm *goja.Runtime, call goja.FunctionCall, idx, def int) int {
	if idx >= len(call.Arguments) || goja.IsUndefined(call.Arguments[idx]) {
		return def
	}
	f, ok := taNum(call.Arguments[idx])
	if !ok {
		return def
	}
	return int(f)
}

// taFloatOpt reads an optional float argument, defaulting when absent/undefined.
func taFloatOpt(vm *goja.Runtime, call goja.FunctionCall, idx int, def float64) float64 {
	if idx >= len(call.Arguments) || goja.IsUndefined(call.Arguments[idx]) {
		return def
	}
	f, ok := taNum(call.Arguments[idx])
	if !ok {
		return def
	}
	return f
}

func taNum(v goja.Value) (float64, bool) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return 0, false
	}
	return numFromExport(v.Export())
}

func numFromExport(e interface{}) (float64, bool) {
	switch x := e.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(x))
		if err != nil {
			return 0, false
		}
		f, _ := d.Float64()
		return f, !math.IsInf(f, 0) && !math.IsNaN(f)
	default:
		return 0, false
	}
}

// taArr renders a float64 series as a JS array with NaN -> null.
func taArr(vm *goja.Runtime, xs []float64) goja.Value {
	out := make([]interface{}, len(xs))
	for i, x := range xs {
		if !math.IsNaN(x) {
			out[i] = x
		} // else leave nil -> JS null
	}
	return vm.ToValue(out)
}

func scalarOrNull(vm *goja.Runtime, x float64) goja.Value {
	if math.IsNaN(x) {
		return goja.Null()
	}
	return vm.ToValue(x)
}

// mustSeries throws an indicator error as a JS TypeError, else returns the series.
func mustSeries(vm *goja.Runtime, name string, out []float64, err error) []float64 {
	mustNoErr(vm, name, err)
	return out
}

func mustNoErr(vm *goja.Runtime, name string, err error) {
	if err != nil {
		panic(vm.NewTypeError("ta.%s: %s", name, err.Error()))
	}
}

// must panics on an internal goja Set failure (object construction only).
func must(err error) {
	if err != nil {
		panic(err)
	}
}
