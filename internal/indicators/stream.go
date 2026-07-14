// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package indicators

import (
	"fmt"
	"math"
)

// recursivePad is the extra history a recursive indicator's bounded window
// keeps beyond its lookback, so the streamed value converges to the batch value
// after the warm-up region and stays within floating-point noise of it.
const recursivePad = 250

// Stream incrementally feeds samples into one indicator. Update appends the new
// sample (or OHLCV bar) and returns the indicator's latest value(s) — NaN until
// warmed up. A Stream is not safe for concurrent use.
//
// For multi-component indicators, Update returns the components in the same
// order as the batch result struct: MACD -> [MACD, Signal, Hist]; BBands ->
// [Upper, Middle, Lower]; Stoch / StochRSI -> [K, D]; Aroon -> [Down, Up].
type Stream struct {
	name  string
	arity int
	fn    func(vals []float64) ([]float64, error)
}

// Update appends one sample per input series and returns the latest value(s).
// The number of values must equal Arity.
func (s *Stream) Update(vals ...float64) ([]float64, error) {
	if len(vals) != s.arity {
		return nil, &InputError{s.name, fmt.Sprintf("expected %d value(s) per update, got %d", s.arity, len(vals))}
	}
	return s.fn(vals)
}

// Name reports the indicator name.
func (s *Stream) Name() string { return s.name }

// Arity reports how many values Update expects per call (1 for single-series
// indicators, more for OHLCV indicators).
func (s *Stream) Arity() int { return s.arity }

// appendCap appends v and keeps at most capN trailing samples.
func appendCap(buf []float64, v float64, capN int) []float64 {
	buf = append(buf, v)
	if len(buf) > capN {
		n := copy(buf, buf[len(buf)-capN:])
		buf = buf[:n]
	}
	return buf
}

// lastOf takes the final element of each component series (NaN if empty).
func lastOf(series [][]float64) []float64 {
	out := make([]float64, len(series))
	for i, s := range series {
		if len(s) == 0 {
			out[i] = math.NaN()
		} else {
			out[i] = s[len(s)-1]
		}
	}
	return out
}

// buffered builds a Stream that keeps the last capN samples of each input
// series and recomputes the indicator over that window on each Update.
func buffered(name string, arity, capN int, batch func(bufs [][]float64) ([][]float64, error)) *Stream {
	bufs := make([][]float64, arity)
	return &Stream{
		name:  name,
		arity: arity,
		fn: func(vals []float64) ([]float64, error) {
			for i := 0; i < arity; i++ {
				bufs[i] = appendCap(bufs[i], vals[i], capN)
			}
			series, err := batch(bufs)
			if err != nil {
				return nil, err
			}
			return lastOf(series), nil
		},
	}
}

// --- trend / moving averages ------------------------------------------------

// NewSMA returns a streaming simple moving average.
func NewSMA(period int) (*Stream, error) {
	if err := checkPeriod("SMA", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("SMA", 1, period, func(b [][]float64) ([][]float64, error) {
		out, err := SMA(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// NewEMA returns a streaming exponential moving average.
func NewEMA(period int) (*Stream, error) {
	if err := checkPeriod("EMA", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("EMA", 1, period+recursivePad, func(b [][]float64) ([][]float64, error) {
		out, err := EMA(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// NewWMA returns a streaming weighted moving average.
func NewWMA(period int) (*Stream, error) {
	if err := checkPeriod("WMA", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("WMA", 1, period, func(b [][]float64) ([][]float64, error) {
		out, err := WMA(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// NewDEMA returns a streaming double exponential moving average.
func NewDEMA(period int) (*Stream, error) {
	if err := checkPeriod("DEMA", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("DEMA", 1, 2*(period-1)+1+recursivePad, func(b [][]float64) ([][]float64, error) {
		out, err := DEMA(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// NewTEMA returns a streaming triple exponential moving average.
func NewTEMA(period int) (*Stream, error) {
	if err := checkPeriod("TEMA", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("TEMA", 1, 3*(period-1)+1+recursivePad, func(b [][]float64) ([][]float64, error) {
		out, err := TEMA(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// --- momentum / oscillators -------------------------------------------------

// NewRSI returns a streaming relative strength index.
func NewRSI(period int) (*Stream, error) {
	if err := checkPeriod("RSI", "period", period, 2); err != nil {
		return nil, err
	}
	return buffered("RSI", 1, period+1+recursivePad, func(b [][]float64) ([][]float64, error) {
		out, err := RSI(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// NewMACD returns a streaming MACD ([MACD, Signal, Hist]).
func NewMACD(fastPeriod, slowPeriod, signalPeriod int) (*Stream, error) {
	if _, err := MACD(nil, fastPeriod, slowPeriod, signalPeriod); err != nil {
		return nil, err
	}
	lookback := (slowPeriod - 1) + (signalPeriod - 1)
	return buffered("MACD", 1, lookback+1+recursivePad, func(b [][]float64) ([][]float64, error) {
		r, err := MACD(b[0], fastPeriod, slowPeriod, signalPeriod)
		return [][]float64{r.MACD, r.Signal, r.Hist}, err
	}), nil
}

// NewStoch returns a streaming stochastic oscillator ([K, D]).
func NewStoch(fastKPeriod, slowKPeriod, slowKMAType, slowDPeriod, slowDMAType int) (*Stream, error) {
	if _, err := Stoch(nil, nil, nil, fastKPeriod, slowKPeriod, slowKMAType, slowDPeriod, slowDMAType); err != nil {
		return nil, err
	}
	lookback := (fastKPeriod - 1) + (slowKPeriod - 1) + (slowDPeriod - 1)
	return buffered("Stoch", 3, lookback+1, func(b [][]float64) ([][]float64, error) {
		r, err := Stoch(b[0], b[1], b[2], fastKPeriod, slowKPeriod, slowKMAType, slowDPeriod, slowDMAType)
		return [][]float64{r.K, r.D}, err
	}), nil
}

// NewStochRSI returns a streaming stochastic RSI ([K, D]).
func NewStochRSI(period, fastKPeriod, fastDPeriod, fastDMAType int) (*Stream, error) {
	if _, err := StochRSI(nil, period, fastKPeriod, fastDPeriod, fastDMAType); err != nil {
		return nil, err
	}
	lookback := period + (fastKPeriod - 1) + (fastDPeriod - 1)
	return buffered("StochRSI", 1, lookback+1+recursivePad, func(b [][]float64) ([][]float64, error) {
		r, err := StochRSI(b[0], period, fastKPeriod, fastDPeriod, fastDMAType)
		return [][]float64{r.K, r.D}, err
	}), nil
}

// NewCCI returns a streaming commodity channel index (high, low, close).
func NewCCI(period int) (*Stream, error) {
	if err := checkPeriod("CCI", "period", period, 2); err != nil {
		return nil, err
	}
	return buffered("CCI", 3, period, func(b [][]float64) ([][]float64, error) {
		out, err := CCI(b[0], b[1], b[2], period)
		return [][]float64{out}, err
	}), nil
}

// NewROC returns a streaming rate of change.
func NewROC(period int) (*Stream, error) {
	if err := checkPeriod("ROC", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("ROC", 1, period+1, func(b [][]float64) ([][]float64, error) {
		out, err := ROC(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// NewMom returns a streaming momentum.
func NewMom(period int) (*Stream, error) {
	if err := checkPeriod("Mom", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("Mom", 1, period+1, func(b [][]float64) ([][]float64, error) {
		out, err := Mom(b[0], period)
		return [][]float64{out}, err
	}), nil
}

// NewWillR returns a streaming Williams %R (high, low, close).
func NewWillR(period int) (*Stream, error) {
	if err := checkPeriod("WillR", "period", period, 2); err != nil {
		return nil, err
	}
	return buffered("WillR", 3, period, func(b [][]float64) ([][]float64, error) {
		out, err := WillR(b[0], b[1], b[2], period)
		return [][]float64{out}, err
	}), nil
}

// --- volatility / bands -----------------------------------------------------

// NewBBands returns a streaming Bollinger Bands ([Upper, Middle, Lower]).
func NewBBands(period int, nbDevUp, nbDevDn float64, maType int) (*Stream, error) {
	if _, err := BBands(nil, period, nbDevUp, nbDevDn, maType); err != nil {
		return nil, err
	}
	return buffered("BBands", 1, period, func(b [][]float64) ([][]float64, error) {
		r, err := BBands(b[0], period, nbDevUp, nbDevDn, maType)
		return [][]float64{r.Upper, r.Middle, r.Lower}, err
	}), nil
}

// NewATR returns a streaming average true range (high, low, close).
func NewATR(period int) (*Stream, error) {
	if err := checkPeriod("ATR", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("ATR", 3, period+1+recursivePad, func(b [][]float64) ([][]float64, error) {
		out, err := ATR(b[0], b[1], b[2], period)
		return [][]float64{out}, err
	}), nil
}

// NewNATR returns a streaming normalized average true range (high, low, close).
func NewNATR(period int) (*Stream, error) {
	if err := checkPeriod("NATR", "period", period, 1); err != nil {
		return nil, err
	}
	return buffered("NATR", 3, period+1+recursivePad, func(b [][]float64) ([][]float64, error) {
		out, err := NATR(b[0], b[1], b[2], period)
		return [][]float64{out}, err
	}), nil
}

// --- directional ------------------------------------------------------------

// NewADX returns a streaming average directional index (high, low, close).
func NewADX(period int) (*Stream, error) {
	if err := checkPeriod("ADX", "period", period, 2); err != nil {
		return nil, err
	}
	return buffered("ADX", 3, 2*period+recursivePad, func(b [][]float64) ([][]float64, error) {
		out, err := ADX(b[0], b[1], b[2], period)
		return [][]float64{out}, err
	}), nil
}

// NewAroon returns a streaming Aroon ([Down, Up]) over (high, low).
func NewAroon(period int) (*Stream, error) {
	if err := checkPeriod("Aroon", "period", period, 2); err != nil {
		return nil, err
	}
	return buffered("Aroon", 2, period+1, func(b [][]float64) ([][]float64, error) {
		r, err := Aroon(b[0], b[1], period)
		return [][]float64{r.Down, r.Up}, err
	}), nil
}

// --- volume -----------------------------------------------------------------

// NewOBV returns a streaming on-balance volume (close, volume). It carries the
// full running total rather than a bounded window, matching the batch result.
func NewOBV() *Stream {
	var obv, prev float64
	var started bool
	return &Stream{
		name:  "OBV",
		arity: 2,
		fn: func(vals []float64) ([]float64, error) {
			c, v := vals[0], vals[1]
			switch {
			case !started:
				started = true
				obv = v
			case c > prev:
				obv += v
			case c < prev:
				obv -= v
			}
			prev = c
			return []float64{obv}, nil
		},
	}
}

// NewMFI returns a streaming money flow index (high, low, close, volume).
func NewMFI(period int) (*Stream, error) {
	if err := checkPeriod("MFI", "period", period, 2); err != nil {
		return nil, err
	}
	return buffered("MFI", 4, period+1, func(b [][]float64) ([][]float64, error) {
		out, err := MFI(b[0], b[1], b[2], b[3], period)
		return [][]float64{out}, err
	}), nil
}
