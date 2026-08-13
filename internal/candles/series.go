// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package candles

import "sort"

// maxSynthOnJump bounds how many empty buckets a single rollover or RollTo may
// synthesize (a broken far-future trade timestamp, or a clock that jumped,
// must not inflate the series unboundedly). Past the cap the series jumps
// straight to the target bucket, leaving the un-synthesized span as a hole.
const maxSynthOnJump = 1000

// maxJumpBuckets rejects a roll target absurdly far past the live bucket
// (~19 years of 1-minute buckets) BEFORE the grid arithmetic runs: a hostile
// or corrupt far-future timestamp must not overflow liveStart + n*periodMs
// into a wrapped bucket time that would poison the live edge. Timestamps are
// untrusted wire data; nothing legitimate jumps this far.
const maxJumpBuckets = 10_000_000

// Series accumulates the candle series for one symbol+interval. The zero value
// is not ready; call [Series.Reset] with an interval first. It is not safe for
// concurrent use; both consumers drive it from a single goroutine.
type Series struct {
	interval string
	periodMs int64
	bars     []Candle // ascending by Time; last is the live bucket
	lastID   int64    // trade-id high-water mark, for fold dedup

	// fillEmpty (ResetGapless) makes a multi-period jump synthesize the skipped
	// empty buckets flat at the previous close with volume "0", keeping the
	// live-derived series gapless on its grid — what a STREAM consumer needs.
	// Off (plain Reset) the series jumps and leaves the span as a hole — what a
	// RENDERER wants: an empty bucket has no trades and therefore no price, so
	// there is nothing truthful to draw.
	fillEmpty bool

	// Parsed mirrors of the live bucket's extremes and volume, so folding a
	// trade compares and accumulates in decimal without re-parsing the bucket.
	// Rebuilt whenever the live bucket is replaced (seed/backfill/rollover).
	liveHi, liveLo, liveVol dec
}

// Reset points the series at a new interval and clears any accumulated state.
// An unrecognized interval leaves the series seed-able but unable to fold or
// roll (periodMs 0), so it degrades to whatever the last Seed delivered.
// A jump over empty buckets leaves a hole (see Series.fillEmpty); use
// ResetGapless for the synthesizing mode.
func (s *Series) Reset(interval string) {
	*s = Series{interval: interval, periodMs: IntervalMs(interval)}
}

// ResetGapless is Reset in the gapless mode: jumps synthesize the skipped
// empty buckets (flat at the previous close, volume "0") so the live-derived
// series never has grid holes. This is the monitor synthesizer's mode; RollTo
// is designed for this mode too (its whole product is on-time flat closes).
func (s *Series) ResetGapless(interval string) {
	s.Reset(interval)
	s.fillEmpty = true
}

// Interval reports the series' current interval (empty until Reset).
func (s *Series) Interval() string { return s.interval }

// PeriodMs is the bucket duration in ms (0 for an unrecognized interval).
func (s *Series) PeriodMs() int64 { return s.periodMs }

// Empty reports whether the series has no candles yet.
func (s *Series) Empty() bool { return len(s.bars) == 0 }

// OldestTime is the bucket start (unix ms) of the oldest loaded candle, or 0
// if the series is empty. It is the end bound for the next Backfill page.
func (s *Series) OldestTime() int64 {
	if len(s.bars) == 0 {
		return 0
	}
	return s.bars[0].Time
}

// Candles returns a copy of the current series, oldest first.
func (s *Series) Candles() []Candle {
	out := make([]Candle, len(s.bars))
	copy(out, s.bars)
	return out
}

// Len is the number of candles in the series.
func (s *Series) Len() int { return len(s.bars) }

// At returns the candle at index i (0 = oldest), copying one bucket. A renderer
// walks the series with Len/At to convert it without the whole-slice allocation
// Candles makes — the copy that matters on a hot redraw path. i must be in
// [0, Len); an out-of-range index panics like a slice access.
func (s *Series) At(i int) Candle { return s.bars[i] }

// LastTradeID is the fold dedup high-water: the highest trade id already folded,
// or the asOfTradeID the last Seed carried. A consumer pulls only trades past it
// so an already-folded batch costs nothing.
func (s *Series) LastTradeID() int64 { return s.lastID }

// Live returns the live (newest, still-open) bucket, if any.
func (s *Series) Live() (Candle, bool) {
	if len(s.bars) == 0 {
		return Candle{}, false
	}
	return s.bars[len(s.bars)-1], true
}

// Seed merges authoritative REST rows into the series, overwriting any folded
// approximation on the buckets they cover while preserving older loaded
// history (see the package doc's merge rule). asOfTradeID is the newest public
// trade id the caller had seen when it kicked off the fetch; the series will
// not fold a trade at or below it, so trades the fetched rows already counted
// are not double-counted. Pass 0 if unknown (the next strictly-newer trade
// still folds). Rows with an unparseable price are dropped.
func (s *Series) Seed(bars []Bar, asOfTradeID int64) {
	s.bars = merge(s.bars, convert(bars))
	if asOfTradeID > s.lastID {
		s.lastID = asOfTradeID
	}
	s.reparseLive()
}

// SeedRebase is Seed for a consumer that will REPLAY its own record of the
// trades delivered after asOfTradeID: unlike Seed, which only ever RAISES the
// fold high-water mark, it RESETS the mark to asOfTradeID so trades whose
// folds the authoritative merge just overwrote can be re-applied on top of
// the fetched rows. Only safe when the caller's replay set is exactly the
// post-asOf deliveries (the monitor synthesizer's in-flight buffer) and the
// series received NO folds while the fetch was in flight — anything else
// double-counts.
func (s *Series) SeedRebase(bars []Bar, asOfTradeID int64) {
	s.bars = merge(s.bars, convert(bars))
	s.lastID = asOfTradeID
	s.reparseLive()
}

// Backfill merges an older page of authoritative REST rows and reports how
// many buckets were newly added. Zero means the page held nothing the series
// lacked — the caller can treat that as the start of available history and
// stop paging (use OldestTime minus one as the next page's end bound).
// Backfill never touches the trade high-water mark.
func (s *Series) Backfill(bars []Bar) (added int) {
	before := len(s.bars)
	s.bars = merge(s.bars, convert(bars))
	s.reparseLive()
	return len(s.bars) - before
}

// FoldTrade extends the live bucket with one public trade, or rolls over when
// the trade falls past it. It returns the buckets the roll finalized (empty
// intermediates synthesized flat at the previous close — capped, see
// maxSynthOnJump) and whether the trade changed the series at all. Trades are
// deduped by id (ids at or below the high-water mark are ignored), so feeding
// an overlapping batch repeatedly is safe. A trade older than the live bucket,
// an unseeded series, or an unknown interval is a no-op.
func (s *Series) FoldTrade(id int64, price, qty string, ts int64) (finalized []Candle, updated bool) {
	if s.periodMs <= 0 || len(s.bars) == 0 || id <= s.lastID {
		return nil, false
	}
	p, ok := parseDec(price)
	if !ok {
		return nil, false
	}
	liveStart := s.bars[len(s.bars)-1].Time
	if ts < liveStart {
		return nil, false // history is authoritative; ignore a trade behind the live edge
	}
	if (ts-liveStart)/s.periodMs > maxJumpBuckets {
		return nil, false // corrupt far-future timestamp; must not consume the id either
	}
	// Only now is the trade committed to fold or roll over, so advance the
	// high-water mark here — a behind-edge or unparseable trade must not consume
	// an id, or a later valid trade with a lower id could be skipped.
	s.lastID = id
	q, okQ := parseDec(qty) // a bad qty folds as 0 rather than dropping the price update
	if !okQ {
		q = dec{s: "0"}
	}
	if ts < liveStart+s.periodMs {
		live := &s.bars[len(s.bars)-1]
		live.Close, live.CloseF = p.s, p.f
		if p.d.GreaterThan(s.liveHi.d) {
			s.liveHi = p
			live.High, live.HighF = p.s, p.f
		}
		if p.d.LessThan(s.liveLo.d) {
			s.liveLo = p
			live.Low, live.LowF = p.s, p.f
		}
		s.liveVol.d = s.liveVol.d.Add(q.d)
		s.liveVol.s = s.liveVol.d.String()
		s.liveVol.f, _ = s.liveVol.d.Float64()
		live.Volume, live.VolumeF = s.liveVol.s, s.liveVol.f
		return nil, true
	}
	// Rollover: the live bucket is complete; synthesize any empty buckets the
	// trade jumped over, then open a fresh bucket at the trade, aligned to the
	// live bucket's grid.
	newStart := liveStart + s.periodMs*((ts-liveStart)/s.periodMs)
	finalized = append(finalized, s.bars[len(s.bars)-1])
	finalized = append(finalized, s.fillTo(newStart)...)
	s.bars = append(s.bars, Candle{
		Time: newStart,
		Open: p.s, High: p.s, Low: p.s, Close: p.s, Volume: q.s,
		OpenF: p.f, HighF: p.f, LowF: p.f, CloseF: p.f, VolumeF: q.f,
	})
	s.liveHi, s.liveLo, s.liveVol = p, p, q
	return finalized, true
}

// RollTo closes the live bucket (repeatedly) when nowMs has passed its end,
// synthesizing each successor flat at the previous close with volume "0", and
// returns the finalized buckets. The bucket containing nowMs stays live. Call
// it on a clock tick so a bucket closes on time in a quiet market; pass a
// grace-adjusted now so a slightly-late trade still lands before its bucket is
// sealed. A jump past maxSynthOnJump buckets skips the intermediates (they are
// left as a hole) and re-opens at the bucket containing nowMs. RollTo is
// gapless-mode-only (its whole product is synthesized flat buckets, exactly
// what plain mode exists to avoid) and is a no-op after a plain Reset.
func (s *Series) RollTo(nowMs int64) (finalized []Candle) {
	if !s.fillEmpty || s.periodMs <= 0 || len(s.bars) == 0 {
		return nil
	}
	liveStart := s.bars[len(s.bars)-1].Time
	if nowMs < liveStart+s.periodMs {
		return nil
	}
	if (nowMs-liveStart)/s.periodMs > maxJumpBuckets {
		return nil // a clock this far ahead is broken; rolling would corrupt the grid
	}
	target := liveStart + s.periodMs*((nowMs-liveStart)/s.periodMs)
	finalized = append(finalized, s.bars[len(s.bars)-1])
	finalized = append(finalized, s.fillTo(target)...)
	prev := s.bars[len(s.bars)-1]
	s.bars = append(s.bars, flatCandle(target, prev))
	s.reparseLive()
	return finalized
}

// fillTo synthesizes the empty buckets between the current live bucket
// (exclusive) and target (exclusive), each flat at its predecessor's close,
// appending them to the series and returning them. A no-op unless the series
// is in gapless mode (ResetGapless). Spans wider than maxSynthOnJump are
// skipped entirely — the caller jumps to target and the span stays a hole
// rather than ballooning memory.
func (s *Series) fillTo(target int64) (filled []Candle) {
	if !s.fillEmpty {
		return nil
	}
	liveStart := s.bars[len(s.bars)-1].Time
	gaps := (target - liveStart) / s.periodMs
	if gaps <= 1 || gaps-1 > maxSynthOnJump {
		return nil
	}
	for t := liveStart + s.periodMs; t < target; t += s.periodMs {
		c := flatCandle(t, s.bars[len(s.bars)-1])
		s.bars = append(s.bars, c)
		filled = append(filled, c)
	}
	return filled
}

// flatCandle is an empty (zero-trade) bucket at t: OHLC pinned to the previous
// bucket's close, volume "0".
func flatCandle(t int64, prev Candle) Candle {
	return Candle{
		Time: t,
		Open: prev.Close, High: prev.Close, Low: prev.Close, Close: prev.Close, Volume: "0",
		OpenF: prev.CloseF, HighF: prev.CloseF, LowF: prev.CloseF, CloseF: prev.CloseF,
	}
}

// TrimOldest drops all but the newest keep buckets, releasing the backing
// memory. A long-running consumer bounds growth with it: the monitor keeps a
// small emit window, the TUI chart a large scroll-back cap so an open chart's
// series cannot grow without limit as buckets roll over.
func (s *Series) TrimOldest(keep int) {
	if keep < 1 || len(s.bars) <= keep {
		return
	}
	trimmed := make([]Candle, keep)
	copy(trimmed, s.bars[len(s.bars)-keep:])
	s.bars = trimmed
}

// reparseLive rebuilds the live bucket's decimal mirrors after the live bucket
// may have been replaced (seed/backfill merge, or a synthesized rollover). A
// stored bucket's strings always re-parse (they were validated on the way in);
// on the impossible failure the zero decimals keep folds conservative.
func (s *Series) reparseLive() {
	if len(s.bars) == 0 {
		s.liveHi, s.liveLo, s.liveVol = dec{}, dec{}, dec{}
		return
	}
	live := s.bars[len(s.bars)-1]
	s.liveHi, _ = parseDec(live.High)
	s.liveLo, _ = parseDec(live.Low)
	s.liveVol, _ = parseDec(live.Volume)
	if s.liveVol.s == "" {
		s.liveVol = dec{s: "0"}
	}
}

// convert turns REST Bars into Candles, dropping unparseable rows. Order is
// not preserved; merge sorts the result.
func convert(bars []Bar) []Candle {
	out := make([]Candle, 0, len(bars))
	for _, b := range bars {
		if c, ok := toCandle(b); ok {
			out = append(out, c)
		}
	}
	return out
}

// merge overlays authoritative REST candles onto an existing series by bucket
// timestamp: an incoming candle replaces one with the same Time (authoritative
// wins, so a re-seed heals a folded bucket), and non-overlapping candles union
// in (so a recent-window Seed never drops older loaded history). The result is
// sorted ascending, so the last element stays the live (newest) bucket.
func merge(existing, incoming []Candle) []Candle {
	if len(incoming) == 0 {
		return existing
	}
	if len(existing) == 0 {
		sort.Slice(incoming, func(i, j int) bool { return incoming[i].Time < incoming[j].Time })
		return incoming
	}
	byTime := make(map[int64]Candle, len(existing)+len(incoming))
	for _, c := range existing {
		byTime[c.Time] = c
	}
	for _, c := range incoming {
		byTime[c.Time] = c // authoritative wins on an equal bucket
	}
	out := make([]Candle, 0, len(byTime))
	for _, c := range byTime {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}
