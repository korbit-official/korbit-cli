// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package candles derives live OHLC candle series from the two sources the
// Korbit API offers — the REST candles endpoint and the public trade stream —
// because the API has no candle WebSocket channel. It is the single home of
// the bucket math shared by its two consumers:
//
//   - the monitor command's synthesized `candle` channel (Synth, synth.go),
//     which needs exact decimal-string output and stream-grade guarantees;
//   - the TUI's candle chart (a Series per open chart, converted to chart
//     floats at the edge — chartCandles in internal/tui/chart.go).
//
// # Model — REST seeds are authoritative, trades extend the live edge
//
// A Series holds one symbol+interval's candles, ascending by bucket start; the
// last bucket is the live (still-open) one. Seed/Backfill merge authoritative
// REST rows in by bucket timestamp: an incoming row overwrites a same-Time
// bucket (authoritative wins, so a re-seed heals any folded approximation) and
// non-overlapping rows union in (so a recent re-seed never drops older loaded
// history). FoldTrade extends the live bucket as trades arrive (close = last
// price, high/low = running extremes, volume accumulates) and rolls over to a
// fresh bucket when a trade falls past it; RollTo does the same on a clock
// tick, so a bucket closes on time even when no trade crosses the boundary.
//
// Bucket alignment always derives from the seeded grid (newStart = liveStart +
// n*period), never from the unix epoch: the server anchors day/week buckets to
// its own calendar, and only its own rows know that grid. This is why a Series
// folds nothing before its first Seed.
//
// A bucket with no trades has no traded price, and the two consumers want
// opposite things from it, so it is a per-Series MODE ([Series.Reset] vs
// [Series.ResetGapless]): the monitor's stream must stay gapless on its grid
// (a consumer waits on bar-closes and keeps indicator arrays aligned), so in
// gapless mode a jumped-over bucket is synthesized flat at the previous close
// with volume "0" — volume "0" being the unambiguous "no trades, prices are
// carried-forward filler" marker; the TUI's chart must not draw a price at
// which nothing traded, so in plain mode the bucket is skipped and the
// slot-based chart compresses the time axis across it.
//
// # Money stays decimal strings
//
// Prices and volumes are decimal strings end to end: comparison and volume
// accumulation run through shopspring/decimal, and a value no arithmetic ever
// touched passes through verbatim. Each Candle also carries float64 mirrors
// (computed once, when the value is set) so a chart consumer can render without
// re-parsing; the floats are display copies, never inputs to the math. A row
// whose OHLC does not parse as a finite value is dropped, so a NaN/Inf can
// never enter a series.
package candles

import (
	"math"
	"strings"

	"github.com/shopspring/decimal"
)

// Channel is the synthesized candle channel's name in the monitor's output.
// It is CLI-derived — deliberately NOT one of the stream package's subscribable
// channel names, because no such WebSocket channel exists on the server.
const Channel = "candle"

// Intervals is the canonical candle-interval enum, exactly the REST candles
// endpoint's values (minutes, then 1D/1W). It is the single source for flag
// validation and error text; no aliases are accepted.
var Intervals = []string{"1", "5", "15", "30", "60", "240", "1D", "1W"}

// IntervalMs maps a canonical interval to its duration in milliseconds, or 0
// if unrecognized.
func IntervalMs(interval string) int64 {
	const min = 60_000
	switch interval {
	case "1":
		return 1 * min
	case "5":
		return 5 * min
	case "15":
		return 15 * min
	case "30":
		return 30 * min
	case "60":
		return 60 * min
	case "240":
		return 240 * min
	case "1D":
		return 24 * 60 * min
	case "1W":
		return 7 * 24 * 60 * min
	}
	return 0
}

// ValidInterval reports whether interval is one of the canonical enum values.
func ValidInterval(interval string) bool { return IntervalMs(interval) > 0 }

// Bar is one OHLC row in the REST candles response shape: a bucket-start
// timestamp (unix ms) and decimal-string prices. It is the input to
// [Series.Seed]/[Series.Backfill] (unmarshaled straight from the REST rows)
// and the price fields of the monitor's emitted candle payload.
type Bar struct {
	Timestamp int64  `json:"timestamp"`
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	Volume    string `json:"volume"`
}

// Candle is one stored bucket: the decimal strings that are the source of
// truth, plus float64 mirrors for chart consumers (display copies computed
// when the string was set — never inputs to the bucket math).
type Candle struct {
	Time int64 // bucket start, unix ms

	Open, High, Low, Close, Volume string

	OpenF, HighF, LowF, CloseF, VolumeF float64
}

// Bar returns the candle's decimal-string form (the emission shape).
func (c Candle) Bar() Bar {
	return Bar{Timestamp: c.Time, Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume}
}

// dec is a parsed decimal value paired with the verbatim string it came from
// and its float display copy.
type dec struct {
	s string
	d decimal.Decimal
	f float64
}

// parseDec parses a decimal string, rejecting "", malformed input, and any
// value whose float64 conversion is not finite (so a NaN/Inf/overflow can
// never corrupt a chart axis or an emitted extreme).
func parseDec(s string) (dec, bool) {
	if s == "" {
		return dec{}, false
	}
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return dec{}, false
	}
	f, _ := d.Float64()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return dec{}, false
	}
	return dec{s: s, d: d, f: f}, true
}

// toCandle converts a Bar, reporting false if any OHLC field is unparseable
// (an unparseable volume folds as "0" rather than dropping the price row) or
// the timestamp is negative — a unix-ms bucket start is never negative, and a
// hugely-negative one would let (ts - liveStart) overflow int64 in the fold's
// jump guard.
func toCandle(b Bar) (Candle, bool) {
	if b.Timestamp < 0 {
		return Candle{}, false
	}
	o, ok1 := parseDec(b.Open)
	h, ok2 := parseDec(b.High)
	l, ok3 := parseDec(b.Low)
	c, ok4 := parseDec(b.Close)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Candle{}, false
	}
	v, okV := parseDec(b.Volume)
	if !okV {
		v = dec{s: "0"}
	}
	return Candle{
		Time: b.Timestamp,
		Open: o.s, High: h.s, Low: l.s, Close: c.s, Volume: v.s,
		OpenF: o.f, HighF: h.f, LowF: l.f, CloseF: c.f, VolumeF: v.f,
	}, true
}
