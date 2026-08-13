// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package indicators

import (
	"math"
	"testing"

	talib "github.com/markcheno/go-talib"
)

// deterministic, non-monotonic, strictly-positive OHLCV series. close is always
// strictly inside (low, high), so range-bound oscillators (Williams %R,
// stochastic) are never exactly at a 0/100 boundary at their first valid bar —
// which lets leadingZeros pinpoint the port's true warm-up length.
func series(n int) (high, low, close, vol []float64) {
	high = make([]float64, n)
	low = make([]float64, n)
	close = make([]float64, n)
	vol = make([]float64, n)
	for i := 0; i < n; i++ {
		f := float64(i)
		base := 100.0 + 15*math.Sin(f/7.0) + 0.3*f + 5*math.Cos(f/3.0)
		c := base + math.Sin(f/5.0)
		spread := 1.0 + 0.5*math.Abs(math.Sin(f/2.0))
		high[i] = math.Max(base, c) + spread
		low[i] = math.Min(base, c) - spread
		close[i] = c
		vol[i] = 1000 + 200*math.Cos(f/4.0) + 3*f
	}
	return
}

func leadingZeros(s []float64) int {
	for i, v := range s {
		if v != 0 {
			return i
		}
	}
	return len(s)
}

// firstValid is the first index where any component is non-zero — the port's
// true warm-up boundary (the warm-up region is left as untouched 0).
func firstValid(components ...[]float64) int {
	best := math.MaxInt
	for _, c := range components {
		if lz := leadingZeros(c); lz < best {
			best = lz
		}
	}
	return best
}

func leadingNaNs(s []float64) int {
	for i, v := range s {
		if !math.IsNaN(v) {
			return i
		}
	}
	return len(s)
}

const n = 400

// TestLookbackMatchesPort pins every lookback formula to go-talib's actual
// warm-up region. If a formula drifts, masking would either hide real values or
// leak a 0 warm-up value as if valid — this is the guard against both.
func TestLookbackMatchesPort(t *testing.T) {
	h, l, c, v := series(n)
	cases := []struct {
		name     string
		expected int
		raw      func() [][]float64
	}{
		{"SMA", 19, func() [][]float64 { return [][]float64{talib.Sma(c, 20)} }},
		{"EMA", 19, func() [][]float64 { return [][]float64{talib.Ema(c, 20)} }},
		{"WMA", 19, func() [][]float64 { return [][]float64{talib.Wma(c, 20)} }},
		{"DEMA", 18, func() [][]float64 { return [][]float64{talib.Dema(c, 10)} }},
		{"TEMA", 27, func() [][]float64 { return [][]float64{talib.Tema(c, 10)} }},
		{"RSI", 14, func() [][]float64 { return [][]float64{talib.Rsi(c, 14)} }},
		{"ROC", 10, func() [][]float64 { return [][]float64{talib.Roc(c, 10)} }},
		{"Mom", 10, func() [][]float64 { return [][]float64{talib.Mom(c, 10)} }},
		{"CCI", 13, func() [][]float64 { return [][]float64{talib.Cci(h, l, c, 14)} }},
		{"WillR", 13, func() [][]float64 { return [][]float64{talib.WillR(h, l, c, 14)} }},
		{"ATR", 14, func() [][]float64 { return [][]float64{talib.Atr(h, l, c, 14)} }},
		{"NATR", 14, func() [][]float64 { return [][]float64{talib.Natr(h, l, c, 14)} }},
		{"ADX", 27, func() [][]float64 { return [][]float64{talib.Adx(h, l, c, 14)} }},
		{"MFI", 14, func() [][]float64 { return [][]float64{talib.Mfi(h, l, c, v, 14)} }},
		{"Aroon", 14, func() [][]float64 {
			d, u := talib.Aroon(h, l, 14)
			return [][]float64{d, u}
		}},
		{"BBands", 19, func() [][]float64 {
			up, mid, lo := talib.BBands(c, 20, 2, 2, talib.SMA)
			return [][]float64{up, mid, lo}
		}},
		// go-talib writes the MACD line one bar early (at lookbackTotal-1) and the
		// signal EMA turns non-zero there too as a transient; only the histogram
		// starts at the clean lookbackTotal. The wrapper masks all three uniformly
		// to that boundary, so pin the expectation against the histogram.
		{"MACD", 33, func() [][]float64 {
			_, _, hh := talib.Macd(c, 12, 26, 9)
			return [][]float64{hh}
		}},
		{"Stoch", 17, func() [][]float64 {
			k, d := talib.Stoch(h, l, c, 14, 3, talib.SMA, 3, talib.SMA)
			return [][]float64{k, d}
		}},
		{"StochRSI", 20, func() [][]float64 {
			k, d := talib.StochRsi(c, 14, 5, 3, talib.SMA)
			return [][]float64{k, d}
		}},
	}
	for _, tc := range cases {
		if got := firstValid(tc.raw()...); got != tc.expected {
			t.Errorf("%s: port warm-up boundary = %d, expected lookback = %d", tc.name, got, tc.expected)
		}
	}
}

// batchCall runs one batch indicator over the full series and returns its
// component series, for the streaming-vs-batch comparison.
type indicatorCase struct {
	name      string
	recursive bool
	batch     func(h, l, c, v []float64) [][]float64
	stream    func() *Stream
	step      func(s *Stream, h, l, c, v []float64, i int) []float64
}

func single(fn func(c []float64) ([]float64, error)) func(h, l, c, v []float64) [][]float64 {
	return func(h, l, c, v []float64) [][]float64 {
		out, err := fn(c)
		if err != nil {
			panic(err)
		}
		return [][]float64{out}
	}
}

func cases() []indicatorCase {
	stepReal := func(s *Stream, h, l, c, v []float64, i int) []float64 { r, _ := s.Update(c[i]); return r }
	stepHLC := func(s *Stream, h, l, c, v []float64, i int) []float64 { r, _ := s.Update(h[i], l[i], c[i]); return r }
	stepHL := func(s *Stream, h, l, c, v []float64, i int) []float64 { r, _ := s.Update(h[i], l[i]); return r }
	stepHLCV := func(s *Stream, h, l, c, v []float64, i int) []float64 {
		r, _ := s.Update(h[i], l[i], c[i], v[i])
		return r
	}
	mk := func(s *Stream, _ error) *Stream { return s }

	return []indicatorCase{
		{"SMA", false, single(func(c []float64) ([]float64, error) { return SMA(c, 20) }), func() *Stream { return mk(NewSMA(20)) }, stepReal},
		{"WMA", false, single(func(c []float64) ([]float64, error) { return WMA(c, 20) }), func() *Stream { return mk(NewWMA(20)) }, stepReal},
		{"ROC", false, single(func(c []float64) ([]float64, error) { return ROC(c, 10) }), func() *Stream { return mk(NewROC(10)) }, stepReal},
		{"Mom", false, single(func(c []float64) ([]float64, error) { return Mom(c, 10) }), func() *Stream { return mk(NewMom(10)) }, stepReal},
		{"CCI", false, func(h, l, c, v []float64) [][]float64 { o, _ := CCI(h, l, c, 14); return [][]float64{o} }, func() *Stream { return mk(NewCCI(14)) }, stepHLC},
		{"WillR", false, func(h, l, c, v []float64) [][]float64 { o, _ := WillR(h, l, c, 14); return [][]float64{o} }, func() *Stream { return mk(NewWillR(14)) }, stepHLC},
		{"MFI", false, func(h, l, c, v []float64) [][]float64 { o, _ := MFI(h, l, c, v, 14); return [][]float64{o} }, func() *Stream { return mk(NewMFI(14)) }, stepHLCV},
		{"Aroon", false, func(h, l, c, v []float64) [][]float64 { r, _ := Aroon(h, l, 14); return [][]float64{r.Down, r.Up} }, func() *Stream { return mk(NewAroon(14)) }, stepHL},
		{"BBands", false, func(h, l, c, v []float64) [][]float64 {
			r, _ := BBands(c, 20, 2, 2, 0)
			return [][]float64{r.Upper, r.Middle, r.Lower}
		}, func() *Stream { return mk(NewBBands(20, 2, 2, 0)) }, stepReal},
		{"Stoch", false, func(h, l, c, v []float64) [][]float64 {
			r, _ := Stoch(h, l, c, 14, 3, 0, 3, 0)
			return [][]float64{r.K, r.D}
		}, func() *Stream { return mk(NewStoch(14, 3, 0, 3, 0)) }, stepHLC},

		{"EMA", true, single(func(c []float64) ([]float64, error) { return EMA(c, 20) }), func() *Stream { return mk(NewEMA(20)) }, stepReal},
		{"DEMA", true, single(func(c []float64) ([]float64, error) { return DEMA(c, 10) }), func() *Stream { return mk(NewDEMA(10)) }, stepReal},
		{"TEMA", true, single(func(c []float64) ([]float64, error) { return TEMA(c, 10) }), func() *Stream { return mk(NewTEMA(10)) }, stepReal},
		{"RSI", true, single(func(c []float64) ([]float64, error) { return RSI(c, 14) }), func() *Stream { return mk(NewRSI(14)) }, stepReal},
		{"ATR", true, func(h, l, c, v []float64) [][]float64 { o, _ := ATR(h, l, c, 14); return [][]float64{o} }, func() *Stream { return mk(NewATR(14)) }, stepHLC},
		{"NATR", true, func(h, l, c, v []float64) [][]float64 { o, _ := NATR(h, l, c, 14); return [][]float64{o} }, func() *Stream { return mk(NewNATR(14)) }, stepHLC},
		{"ADX", true, func(h, l, c, v []float64) [][]float64 { o, _ := ADX(h, l, c, 14); return [][]float64{o} }, func() *Stream { return mk(NewADX(14)) }, stepHLC},
		{"MACD", true, func(h, l, c, v []float64) [][]float64 {
			r, _ := MACD(c, 12, 26, 9)
			return [][]float64{r.MACD, r.Signal, r.Hist}
		}, func() *Stream { return mk(NewMACD(12, 26, 9)) }, stepReal},
		{"StochRSI", true, func(h, l, c, v []float64) [][]float64 {
			r, _ := StochRSI(c, 14, 5, 3, 0)
			return [][]float64{r.K, r.D}
		}, func() *Stream { return mk(NewStochRSI(14, 5, 3, 0)) }, stepReal},
	}
}

// TestWindowedStreamingExact: a windowed indicator's stream must equal the batch
// value at every valid index (same trailing window).
func TestWindowedStreamingExact(t *testing.T) {
	h, l, c, v := series(n)
	for _, tc := range cases() {
		if tc.recursive {
			continue
		}
		batch := tc.batch(h, l, c, v)
		s := tc.stream()
		for i := 0; i < n; i++ {
			got := tc.step(s, h, l, c, v, i)
			for comp := range batch {
				want := batch[comp][i]
				g := got[comp]
				if math.IsNaN(want) {
					if !math.IsNaN(g) {
						t.Errorf("%s comp %d idx %d: batch NaN but stream %v", tc.name, comp, i, g)
					}
					continue
				}
				if math.IsNaN(g) || math.Abs(g-want) > 1e-9 {
					t.Errorf("%s comp %d idx %d: stream %v != batch %v", tc.name, comp, i, g, want)
				}
			}
		}
	}
}

// TestRecursiveStreamingConverges: a recursive indicator's stream converges to
// the batch value once the bounded window's warm-up padding has elapsed.
func TestRecursiveStreamingConverges(t *testing.T) {
	h, l, c, v := series(n)
	for _, tc := range cases() {
		if !tc.recursive {
			continue
		}
		batch := tc.batch(h, l, c, v)
		s := tc.stream()
		var last []float64
		for i := 0; i < n; i++ {
			last = tc.step(s, h, l, c, v, i)
		}
		for comp := range batch {
			want := batch[comp][n-1]
			g := last[comp]
			if math.IsNaN(want) {
				t.Errorf("%s comp %d: batch final unexpectedly NaN", tc.name, comp)
				continue
			}
			if math.IsNaN(g) || math.Abs(g-want) > 1e-6 {
				t.Errorf("%s comp %d: stream final %v not converged to batch %v", tc.name, comp, g, want)
			}
		}
	}
}

// TestOBVExact: streaming OBV matches the batch (cumulative) value at every index.
func TestOBVExact(t *testing.T) {
	_, _, c, v := series(n)
	batch, err := OBV(c, v)
	if err != nil {
		t.Fatal(err)
	}
	s := NewOBV()
	for i := 0; i < n; i++ {
		got, _ := s.Update(c[i], v[i])
		if math.Abs(got[0]-batch[i]) > 1e-9 {
			t.Fatalf("OBV idx %d: stream %v != batch %v", i, got[0], batch[i])
		}
	}
}

func TestValidationErrors(t *testing.T) {
	if _, err := SMA([]float64{1, 2, 3}, 0); err == nil {
		t.Error("SMA period 0 should error")
	} else if _, ok := err.(*InputError); !ok {
		t.Errorf("SMA period 0: want *InputError, got %T", err)
	}
	if _, err := RSI([]float64{1, 2, 3}, 1); err == nil {
		t.Error("RSI period 1 should error (min 2)")
	}
	if _, err := MACD([]float64{1, 2, 3}, 26, 12, 9); err == nil {
		t.Error("MACD fast>=slow should error")
	}
	if _, err := ATR([]float64{1, 2}, []float64{1}, []float64{1, 2}, 14); err == nil {
		t.Error("ATR mismatched lengths should error")
	}
	if _, err := BBands([]float64{1, 2, 3}, 20, 2, 2, 99); err == nil {
		t.Error("BBands bad maType should error")
	}
}

// TestWarmupNotError: too-short input is the normal warm-up state, not an error.
func TestWarmupNotError(t *testing.T) {
	out, err := SMA([]float64{1, 2, 3}, 20)
	if err != nil {
		t.Fatalf("short SMA should not error: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("short SMA: want len 3, got %d", len(out))
	}
	for i, x := range out {
		if !math.IsNaN(x) {
			t.Errorf("short SMA idx %d: want NaN, got %v", i, x)
		}
	}
	// empty input -> empty output.
	if out, err := SMA(nil, 20); err != nil || len(out) != 0 {
		t.Errorf("nil SMA: want empty no-error, got %v %v", out, err)
	}
}

func TestLeadingNaNCount(t *testing.T) {
	_, _, c, _ := series(n)
	out, _ := RSI(c, 14)
	if got := leadingNaNs(out); got != 14 {
		t.Errorf("RSI leading NaN = %d, want 14", got)
	}
}

// TestKnownValues checks the math against hand-computed references.
func TestKnownValues(t *testing.T) {
	data := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	sma, _ := SMA(data, 3)
	// SMA(3) at index 2 = (1+2+3)/3 = 2; index 9 = (8+9+10)/3 = 9.
	if math.Abs(sma[2]-2) > 1e-9 || math.Abs(sma[9]-9) > 1e-9 {
		t.Errorf("SMA known values wrong: idx2=%v idx9=%v", sma[2], sma[9])
	}
	// RSI of a strictly increasing series is 100 (all gains, no losses).
	rsi, _ := RSI(data, 5)
	if math.Abs(rsi[len(rsi)-1]-100) > 1e-9 {
		t.Errorf("RSI of rising series should be 100, got %v", rsi[len(rsi)-1])
	}
	// Momentum(3): data[i]-data[i-3] = 3 throughout the valid region.
	mom, _ := Mom(data, 3)
	if math.Abs(mom[9]-3) > 1e-9 {
		t.Errorf("Mom(3) should be 3, got %v", mom[9])
	}
}
