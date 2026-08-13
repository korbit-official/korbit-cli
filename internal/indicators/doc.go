// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package indicators provides technical-analysis indicator helpers — moving
// averages, oscillators, bands, and volume studies — for trading bots.
//
// The numeric kernels are a thin, hardened wrapper over the pure-Go TA-Lib port
// github.com/markcheno/go-talib, so outputs match the de-facto TA-Lib reference,
// including its initialization conventions (for example, the first true range
// is seeded at 0). The wrapper adds three guarantees the bare port does not:
//
//   - Panic-free. Every function validates its arguments and returns a typed
//     *InputError on misuse (a period below the indicator's minimum, or input
//     series of differing lengths). Too-short input is not an error: it returns
//     an all-NaN result of the input length — the normal "still warming up"
//     state. A defensive recover converts any unexpected port panic into the
//     same typed error, so bad input can never crash the host process.
//
//   - Explicit warm-up. An indicator is undefined until enough samples have
//     accumulated (its lookback). Those leading positions are returned as NaN,
//     never 0: a bare 0 is indistinguishable from a legitimate zero reading of a
//     centered oscillator. Callers test with math.IsNaN; the count of leading
//     NaN values is the indicator's lookback.
//
//   - Float confined to the signal layer. Indicators operate on float64.
//     Convert decimal price/quantity strings to float64 once, at the boundary,
//     and use indicator outputs only as signals — never round-trip a float back
//     into an order price, quantity, or balance, which stay decimal strings end
//     to end.
//
// # Batch and streaming
//
// Each indicator has a batch form (whole input slice in, full output slice out,
// of equal length) and a streaming form (a Stream). A Stream feeds one sample —
// or one OHLCV bar — at a time via Update and returns the latest value(s), NaN
// until warmed up. This is the natural shape for reacting to a live market feed
// bar by bar.
//
// A Stream maintains a bounded window of recent samples. Windowed indicators
// (SMA, WMA, Bollinger Bands, Stochastic, CCI, Williams %R, Aroon, ROC,
// Momentum, MFI) are exact. Recursive indicators (EMA, DEMA, TEMA, RSI, ATR,
// NATR, ADX, MACD, Stochastic RSI) carry effectively unbounded history in the
// reference definition; a Stream computes them over a bounded recent window, so
// each result converges to the batch value after the warm-up region and stays
// within floating-point noise of it thereafter. On-balance volume (OBV) is
// cumulative, and its Stream carries the full running total. A Stream is not
// safe for concurrent use.
//
// # Indicators
//
// Trend and moving averages: SMA, EMA, WMA, DEMA, TEMA.
//
// Momentum and oscillators: RSI, MACD, Stochastic, Stochastic RSI, CCI, ROC,
// Momentum, Williams %R.
//
// Volatility and bands: Bollinger Bands, ATR, NATR.
//
// Directional: ADX, Aroon.
//
// Volume: OBV, MFI.
//
// Multi-component indicators return a result struct: MACD (MACD, Signal,
// Histogram), Bollinger Bands (Upper, Middle, Lower), Stochastic and Stochastic
// RSI (K, D), and Aroon (Down, Up).
package indicators
