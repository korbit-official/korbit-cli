// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package indicators

import (
	"fmt"
	"math"

	talib "github.com/markcheno/go-talib"
)

// InputError reports misuse of an indicator: a period below its minimum, input
// series of differing lengths, or input the underlying kernel cannot process.
// Too-short input is NOT an InputError — it returns an all-NaN warm-up result.
type InputError struct {
	Indicator string
	Reason    string
}

func (e *InputError) Error() string { return fmt.Sprintf("%s: %s", e.Indicator, e.Reason) }

// Multi-component results. Each component slice has the same length as the
// input, with leading warm-up positions set to NaN. A 0 at or after the
// lookback is a genuine reading (e.g. Aroon Down or Stochastic RSI %K can be 0),
// not a warm-up marker.

// MACDResult holds the MACD line, its signal line, and the histogram.
type MACDResult struct{ MACD, Signal, Hist []float64 }

// BBandsResult holds the upper, middle, and lower Bollinger Bands.
type BBandsResult struct{ Upper, Middle, Lower []float64 }

// StochResult holds the %K and %D lines (Stochastic and Stochastic RSI).
type StochResult struct{ K, D []float64 }

// AroonResult holds the Aroon Down and Aroon Up lines.
type AroonResult struct{ Down, Up []float64 }

// --- shared helpers ---------------------------------------------------------

func nanSlice(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// maskWarmup overwrites the leading lookback positions (the port leaves them as
// an untouched 0) with NaN, marking them undefined.
func maskWarmup(out []float64, lookback int) []float64 {
	for i := 0; i < lookback && i < len(out); i++ {
		out[i] = math.NaN()
	}
	return out
}

func checkPeriod(name, label string, period, min int) error {
	if period < min {
		return &InputError{name, fmt.Sprintf("%s must be >= %d (got %d)", label, min, period)}
	}
	return nil
}

// sameLen returns the common length of the series, or an InputError if they
// differ.
func sameLen(name string, series ...[]float64) (int, error) {
	n := len(series[0])
	for _, s := range series[1:] {
		if len(s) != n {
			return 0, &InputError{name, "input series must all have the same length"}
		}
	}
	return n, nil
}

// guard runs the kernel call, converting any panic (e.g. a port indexing bug on
// pathological input) into a typed InputError so the host never crashes.
func guard(name string, run func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &InputError{name, "input could not be processed"}
		}
	}()
	run()
	return nil
}

// one wraps a single-output kernel: validates length, short-circuits the
// warm-up case to all-NaN, runs the kernel under guard, then masks warm-up.
func one(name string, n, lookback int, run func() []float64) ([]float64, error) {
	if n == 0 {
		return []float64{}, nil
	}
	if n <= lookback {
		return nanSlice(n), nil
	}
	var raw []float64
	if err := guard(name, func() { raw = run() }); err != nil {
		return nil, err
	}
	return maskWarmup(raw, lookback), nil
}

func toMAType(name string, maType int) (talib.MaType, error) {
	if maType < 0 || maType > 8 {
		return 0, &InputError{name, fmt.Sprintf("maType must be 0..8 (got %d)", maType)}
	}
	return talib.MaType(maType), nil
}

// --- trend / moving averages ------------------------------------------------

// SMA is the simple moving average.
func SMA(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("SMA", "period", period, 1); err != nil {
		return nil, err
	}
	return one("SMA", len(real), period-1, func() []float64 { return talib.Sma(real, period) })
}

// EMA is the exponential moving average.
func EMA(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("EMA", "period", period, 1); err != nil {
		return nil, err
	}
	return one("EMA", len(real), period-1, func() []float64 { return talib.Ema(real, period) })
}

// WMA is the weighted moving average.
func WMA(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("WMA", "period", period, 1); err != nil {
		return nil, err
	}
	return one("WMA", len(real), period-1, func() []float64 { return talib.Wma(real, period) })
}

// DEMA is the double exponential moving average.
func DEMA(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("DEMA", "period", period, 1); err != nil {
		return nil, err
	}
	return one("DEMA", len(real), 2*(period-1), func() []float64 { return talib.Dema(real, period) })
}

// TEMA is the triple exponential moving average.
func TEMA(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("TEMA", "period", period, 1); err != nil {
		return nil, err
	}
	return one("TEMA", len(real), 3*(period-1), func() []float64 { return talib.Tema(real, period) })
}

// --- momentum / oscillators -------------------------------------------------

// RSI is the relative strength index.
func RSI(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("RSI", "period", period, 2); err != nil {
		return nil, err
	}
	return one("RSI", len(real), period, func() []float64 { return talib.Rsi(real, period) })
}

// MACD is the moving-average convergence/divergence (line, signal, histogram).
// fastPeriod must be less than slowPeriod.
func MACD(real []float64, fastPeriod, slowPeriod, signalPeriod int) (MACDResult, error) {
	if err := checkPeriod("MACD", "fastPeriod", fastPeriod, 1); err != nil {
		return MACDResult{}, err
	}
	if err := checkPeriod("MACD", "slowPeriod", slowPeriod, 2); err != nil {
		return MACDResult{}, err
	}
	if err := checkPeriod("MACD", "signalPeriod", signalPeriod, 1); err != nil {
		return MACDResult{}, err
	}
	if fastPeriod >= slowPeriod {
		return MACDResult{}, &InputError{"MACD", fmt.Sprintf("fastPeriod (%d) must be < slowPeriod (%d)", fastPeriod, slowPeriod)}
	}
	n := len(real)
	lookback := (slowPeriod - 1) + (signalPeriod - 1)
	if n == 0 {
		return MACDResult{[]float64{}, []float64{}, []float64{}}, nil
	}
	if n <= lookback {
		return MACDResult{nanSlice(n), nanSlice(n), nanSlice(n)}, nil
	}
	var macd, signal, hist []float64
	if err := guard("MACD", func() { macd, signal, hist = talib.Macd(real, fastPeriod, slowPeriod, signalPeriod) }); err != nil {
		return MACDResult{}, err
	}
	return MACDResult{maskWarmup(macd, lookback), maskWarmup(signal, lookback), maskWarmup(hist, lookback)}, nil
}

// Stoch is the (slow) stochastic oscillator (%K, %D). maType selects the
// smoothing average (0=SMA, 1=EMA, ...; see TA-Lib MaType).
func Stoch(high, low, close []float64, fastKPeriod, slowKPeriod, slowKMAType, slowDPeriod, slowDMAType int) (StochResult, error) {
	if err := checkPeriod("Stoch", "fastKPeriod", fastKPeriod, 1); err != nil {
		return StochResult{}, err
	}
	if err := checkPeriod("Stoch", "slowKPeriod", slowKPeriod, 1); err != nil {
		return StochResult{}, err
	}
	if err := checkPeriod("Stoch", "slowDPeriod", slowDPeriod, 1); err != nil {
		return StochResult{}, err
	}
	kMA, err := toMAType("Stoch", slowKMAType)
	if err != nil {
		return StochResult{}, err
	}
	dMA, err := toMAType("Stoch", slowDMAType)
	if err != nil {
		return StochResult{}, err
	}
	n, err := sameLen("Stoch", high, low, close)
	if err != nil {
		return StochResult{}, err
	}
	lookback := (fastKPeriod - 1) + (slowKPeriod - 1) + (slowDPeriod - 1)
	if n == 0 {
		return StochResult{[]float64{}, []float64{}}, nil
	}
	if n <= lookback {
		return StochResult{nanSlice(n), nanSlice(n)}, nil
	}
	var k, d []float64
	if err := guard("Stoch", func() { k, d = talib.Stoch(high, low, close, fastKPeriod, slowKPeriod, kMA, slowDPeriod, dMA) }); err != nil {
		return StochResult{}, err
	}
	return StochResult{maskWarmup(k, lookback), maskWarmup(d, lookback)}, nil
}

// StochRSI is the stochastic RSI (%K, %D). maType selects the %D smoothing.
func StochRSI(real []float64, period, fastKPeriod, fastDPeriod, fastDMAType int) (StochResult, error) {
	if err := checkPeriod("StochRSI", "period", period, 2); err != nil {
		return StochResult{}, err
	}
	if err := checkPeriod("StochRSI", "fastKPeriod", fastKPeriod, 1); err != nil {
		return StochResult{}, err
	}
	if err := checkPeriod("StochRSI", "fastDPeriod", fastDPeriod, 1); err != nil {
		return StochResult{}, err
	}
	dMA, err := toMAType("StochRSI", fastDMAType)
	if err != nil {
		return StochResult{}, err
	}
	n := len(real)
	lookback := period + (fastKPeriod - 1) + (fastDPeriod - 1)
	if n == 0 {
		return StochResult{[]float64{}, []float64{}}, nil
	}
	if n <= lookback {
		return StochResult{nanSlice(n), nanSlice(n)}, nil
	}
	var k, d []float64
	if err := guard("StochRSI", func() { k, d = talib.StochRsi(real, period, fastKPeriod, fastDPeriod, dMA) }); err != nil {
		return StochResult{}, err
	}
	return StochResult{maskWarmup(k, lookback), maskWarmup(d, lookback)}, nil
}

// CCI is the commodity channel index.
func CCI(high, low, close []float64, period int) ([]float64, error) {
	if err := checkPeriod("CCI", "period", period, 2); err != nil {
		return nil, err
	}
	n, err := sameLen("CCI", high, low, close)
	if err != nil {
		return nil, err
	}
	return one("CCI", n, period-1, func() []float64 { return talib.Cci(high, low, close, period) })
}

// ROC is the rate of change (percentage) over period bars.
func ROC(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("ROC", "period", period, 1); err != nil {
		return nil, err
	}
	return one("ROC", len(real), period, func() []float64 { return talib.Roc(real, period) })
}

// Mom is the momentum: real[i] - real[i-period].
func Mom(real []float64, period int) ([]float64, error) {
	if err := checkPeriod("Mom", "period", period, 1); err != nil {
		return nil, err
	}
	return one("Mom", len(real), period, func() []float64 { return talib.Mom(real, period) })
}

// WillR is Williams %R.
func WillR(high, low, close []float64, period int) ([]float64, error) {
	if err := checkPeriod("WillR", "period", period, 2); err != nil {
		return nil, err
	}
	n, err := sameLen("WillR", high, low, close)
	if err != nil {
		return nil, err
	}
	return one("WillR", n, period-1, func() []float64 { return talib.WillR(high, low, close, period) })
}

// --- volatility / bands -----------------------------------------------------

// BBands is the Bollinger Bands (upper, middle, lower). nbDevUp/nbDevDn are the
// standard-deviation multipliers; maType selects the middle-band average.
func BBands(real []float64, period int, nbDevUp, nbDevDn float64, maType int) (BBandsResult, error) {
	if err := checkPeriod("BBands", "period", period, 2); err != nil {
		return BBandsResult{}, err
	}
	ma, err := toMAType("BBands", maType)
	if err != nil {
		return BBandsResult{}, err
	}
	n := len(real)
	lookback := period - 1
	if n == 0 {
		return BBandsResult{[]float64{}, []float64{}, []float64{}}, nil
	}
	if n <= lookback {
		return BBandsResult{nanSlice(n), nanSlice(n), nanSlice(n)}, nil
	}
	var up, mid, low []float64
	if err := guard("BBands", func() { up, mid, low = talib.BBands(real, period, nbDevUp, nbDevDn, ma) }); err != nil {
		return BBandsResult{}, err
	}
	return BBandsResult{maskWarmup(up, lookback), maskWarmup(mid, lookback), maskWarmup(low, lookback)}, nil
}

// ATR is the average true range.
func ATR(high, low, close []float64, period int) ([]float64, error) {
	if err := checkPeriod("ATR", "period", period, 1); err != nil {
		return nil, err
	}
	n, err := sameLen("ATR", high, low, close)
	if err != nil {
		return nil, err
	}
	return one("ATR", n, period, func() []float64 { return talib.Atr(high, low, close, period) })
}

// NATR is the normalized average true range (ATR as a percentage of price).
func NATR(high, low, close []float64, period int) ([]float64, error) {
	if err := checkPeriod("NATR", "period", period, 1); err != nil {
		return nil, err
	}
	n, err := sameLen("NATR", high, low, close)
	if err != nil {
		return nil, err
	}
	return one("NATR", n, period, func() []float64 { return talib.Natr(high, low, close, period) })
}

// --- directional ------------------------------------------------------------

// ADX is the average directional movement index.
func ADX(high, low, close []float64, period int) ([]float64, error) {
	if err := checkPeriod("ADX", "period", period, 2); err != nil {
		return nil, err
	}
	n, err := sameLen("ADX", high, low, close)
	if err != nil {
		return nil, err
	}
	return one("ADX", n, 2*period-1, func() []float64 { return talib.Adx(high, low, close, period) })
}

// Aroon is the Aroon indicator (Down, Up).
func Aroon(high, low []float64, period int) (AroonResult, error) {
	if err := checkPeriod("Aroon", "period", period, 2); err != nil {
		return AroonResult{}, err
	}
	n, err := sameLen("Aroon", high, low)
	if err != nil {
		return AroonResult{}, err
	}
	lookback := period
	if n == 0 {
		return AroonResult{[]float64{}, []float64{}}, nil
	}
	if n <= lookback {
		return AroonResult{nanSlice(n), nanSlice(n)}, nil
	}
	var down, up []float64
	if err := guard("Aroon", func() { down, up = talib.Aroon(high, low, period) }); err != nil {
		return AroonResult{}, err
	}
	return AroonResult{maskWarmup(down, lookback), maskWarmup(up, lookback)}, nil
}

// --- volume -----------------------------------------------------------------

// OBV is on-balance volume. It is cumulative (no warm-up); the first value is
// the first volume.
func OBV(close, volume []float64) ([]float64, error) {
	n, err := sameLen("OBV", close, volume)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return []float64{}, nil
	}
	var raw []float64
	if err := guard("OBV", func() { raw = talib.Obv(close, volume) }); err != nil {
		return nil, err
	}
	return raw, nil
}

// MFI is the money flow index.
func MFI(high, low, close, volume []float64, period int) ([]float64, error) {
	if err := checkPeriod("MFI", "period", period, 2); err != nil {
		return nil, err
	}
	n, err := sameLen("MFI", high, low, close, volume)
	if err != nil {
		return nil, err
	}
	return one("MFI", n, period, func() []float64 { return talib.Mfi(high, low, close, volume, period) })
}
