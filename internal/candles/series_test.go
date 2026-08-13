// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package candles

import (
	"reflect"
	"testing"
)

// bar is a terse Bar builder for tests.
func bar(ts int64, o, h, l, c, v string) Bar {
	return Bar{Timestamp: ts, Open: o, High: h, Low: l, Close: c, Volume: v}
}

// ohlcv asserts one candle's decimal-string fields.
func ohlcv(t *testing.T, c Candle, o, h, l, cl, v string) {
	t.Helper()
	if c.Open != o || c.High != h || c.Low != l || c.Close != cl || c.Volume != v {
		t.Errorf("candle = O %s H %s L %s C %s V %s, want O %s H %s L %s C %s V %s",
			c.Open, c.High, c.Low, c.Close, c.Volume, o, h, l, cl, v)
	}
}

func TestIntervalEnum(t *testing.T) {
	for _, iv := range Intervals {
		if !ValidInterval(iv) {
			t.Errorf("canonical interval %q not valid", iv)
		}
	}
	for _, iv := range []string{"", "2", "1m", "1d", "1w", "60m", "bogus"} {
		if ValidInterval(iv) {
			t.Errorf("interval %q should be invalid (canonical REST values only)", iv)
		}
	}
	if IntervalMs("1W") != 7*24*60*60_000 {
		t.Errorf("1W = %d ms", IntervalMs("1W"))
	}
}

func TestSeedParsesOrdersAndKeepsStringsVerbatim(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{
		bar(120_000, "3.10", "4.00", "2", "3.5", "10"),
		bar(60_000, "1", "2", "0.50", "1.5", "5"), // out of order on purpose
	}, 0)

	c := s.Candles()
	if len(c) != 2 {
		t.Fatalf("want 2 candles, got %d", len(c))
	}
	if c[0].Time != 60_000 || c[1].Time != 120_000 {
		t.Errorf("candles not sorted ascending: %d then %d", c[0].Time, c[1].Time)
	}
	// Untouched values pass through VERBATIM — "3.10" stays "3.10", never "3.1".
	ohlcv(t, c[1], "3.10", "4.00", "2", "3.5", "10")
	if c[0].OpenF != 1 || c[0].HighF != 2 || c[0].LowF != 0.5 || c[0].CloseF != 1.5 || c[0].VolumeF != 5 {
		t.Errorf("float mirrors wrong: %+v", c[0])
	}
}

func TestSeedDropsUnparseableRow(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{
		bar(60_000, "1", "2", "0.5", "1.5", "5"),
		bar(120_000, "x", "4", "2", "3.5", "10"), // bad open
	}, 0)
	if got := len(s.Candles()); got != 1 {
		t.Fatalf("want 1 candle (bad row dropped), got %d", got)
	}
}

func TestFoldExtendsLiveBucketExactly(t *testing.T) {
	var s Series
	s.Reset("1") // 60s buckets
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "0.1")}, 0)

	// Volume accumulates in decimal: 0.1 + 0.1 + 0.1 is exactly 0.3 (a float
	// fold would say 0.30000000000000004).
	if _, up := s.FoldTrade(1, "105", "0.1", 61_000); !up {
		t.Fatal("fold reported no update")
	}
	s.FoldTrade(2, "98", "0.1", 62_000)

	live, _ := s.Live()
	ohlcv(t, live, "100", "105", "98", "98", "0.3")
}

func TestFoldDedupsByTradeID(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)

	s.FoldTrade(5, "110", "1", 61_000)
	volAfterFirst, _ := s.Live()
	// Re-feeding the same and older ids must be no-ops (batches overlap).
	if _, up := s.FoldTrade(5, "110", "1", 61_000); up {
		t.Error("duplicate id reported an update")
	}
	s.FoldTrade(3, "110", "1", 61_000)
	if live, _ := s.Live(); live.Volume != volAfterFirst.Volume {
		t.Errorf("dedup failed: volume moved from %v to %v", volAfterFirst.Volume, live.Volume)
	}
}

func TestLastTradeIDTracksFoldHighWater(t *testing.T) {
	var s Series
	s.Reset("1")
	// Seed carries the newest already-known id, so nothing at or below it re-folds.
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 7)
	if got := s.LastTradeID(); got != 7 {
		t.Fatalf("after seed(asOf=7): LastTradeID=%d, want 7", got)
	}
	// A committed fold advances the high-water; the next pull past it is empty.
	if _, up := s.FoldTrade(9, "110", "1", 61_000); !up {
		t.Fatal("fold of a newer trade should update")
	}
	if got := s.LastTradeID(); got != 9 {
		t.Fatalf("after fold(9): LastTradeID=%d, want 9", got)
	}
	// A trade at or below the high-water is a no-op and must not move it.
	if _, up := s.FoldTrade(9, "111", "1", 61_500); up {
		t.Error("re-folding id 9 reported an update")
	}
	if got := s.LastTradeID(); got != 9 {
		t.Fatalf("LastTradeID moved to %d on a dedup no-op, want 9", got)
	}
}

func TestFoldIgnoresTradeBehindLiveEdge(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(120_000, "100", "100", "100", "100", "1")}, 0)
	if fin, up := s.FoldTrade(1, "50", "1", 60_500); len(fin) > 0 || up {
		t.Error("a trade before the live bucket must be a no-op")
	}
	if live, _ := s.Live(); live.Close != "100" {
		t.Errorf("close changed to %v; an older trade must be ignored", live.Close)
	}
}

func TestRolloverFinalizesAndOpensNewBucket(t *testing.T) {
	var s Series
	s.Reset("1") // 60s
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)

	fin, up := s.FoldTrade(1, "101", "2", 125_000) // next bucket
	if !up || len(fin) != 1 {
		t.Fatalf("want 1 finalized bucket, got %d (updated=%v)", len(fin), up)
	}
	if fin[0].Time != 60_000 || fin[0].Close != "100" {
		t.Errorf("finalized = %+v, want the 60000 bucket", fin[0])
	}
	c := s.Candles()
	if len(c) != 2 {
		t.Fatalf("want 2 candles after rollover, got %d", len(c))
	}
	if c[1].Time != 120_000 { // aligned to the 60s grid off the live bucket
		t.Errorf("new bucket start = %d, want 120000", c[1].Time)
	}
	ohlcv(t, c[1], "101", "101", "101", "101", "2")
}

func TestRolloverJumpSynthesizesEmptyBuckets(t *testing.T) {
	var s Series
	s.ResetGapless("1")
	s.Seed([]Bar{bar(60_000, "100", "110", "90", "105", "1")}, 0)

	// A trade three buckets ahead: 120000 and 180000 had no trades and are
	// synthesized flat at the previous close; the trade opens 240000.
	fin, _ := s.FoldTrade(1, "108", "1", 245_000)
	if len(fin) != 3 {
		t.Fatalf("want 3 finalized (live + 2 synthesized), got %d", len(fin))
	}
	ohlcv(t, fin[1], "105", "105", "105", "105", "0")
	ohlcv(t, fin[2], "105", "105", "105", "105", "0")
	if fin[1].Time != 120_000 || fin[2].Time != 180_000 {
		t.Errorf("synthesized times = %d, %d", fin[1].Time, fin[2].Time)
	}
	c := s.Candles()
	if len(c) != 4 || c[3].Time != 240_000 || c[3].Open != "108" {
		t.Errorf("series after jump = %+v", c)
	}
}

func TestBoundaryTimestampRollsOver(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)
	if fin, _ := s.FoldTrade(1, "101", "1", 120_000); len(fin) != 1 { // exactly on the boundary
		t.Fatal("a trade exactly on the next boundary should open a new bucket")
	}
	if c := s.Candles(); c[1].Time != 120_000 {
		t.Errorf("new bucket start = %d, want 120000", c[1].Time)
	}
}

func TestRollToFinalizesQuietBuckets(t *testing.T) {
	var s Series
	s.ResetGapless("1")
	s.Seed([]Bar{bar(60_000, "100", "110", "90", "105", "2")}, 0)

	// The clock has not reached the bucket end: nothing to do.
	if fin := s.RollTo(119_999); fin != nil {
		t.Fatalf("premature roll: %+v", fin)
	}
	// One period elapsed: the live bucket closes, a flat zero-volume successor
	// opens live.
	fin := s.RollTo(120_000)
	if len(fin) != 1 || fin[0].Time != 60_000 {
		t.Fatalf("finalized = %+v, want the 60000 bucket", fin)
	}
	live, _ := s.Live()
	if live.Time != 120_000 {
		t.Fatalf("live = %d, want 120000", live.Time)
	}
	ohlcv(t, live, "105", "105", "105", "105", "0")

	// Two more periods at once: both close flat, the newest stays live.
	fin = s.RollTo(240_000 + 5_000)
	if len(fin) != 2 || fin[0].Time != 120_000 || fin[1].Time != 180_000 {
		t.Fatalf("finalized = %+v, want 120000 and 180000", fin)
	}
	if live, _ = s.Live(); live.Time != 240_000 || live.Volume != "0" {
		t.Errorf("live = %+v, want flat 240000", live)
	}
}

func TestRollToThenTradeFoldsIntoNewLive(t *testing.T) {
	var s Series
	s.ResetGapless("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)
	s.RollTo(120_000)
	if fin, up := s.FoldTrade(1, "107", "0.5", 121_000); len(fin) != 0 || !up {
		t.Fatalf("trade in the rolled bucket should extend it, not roll (fin=%v up=%v)", fin, up)
	}
	live, _ := s.Live()
	// The flat open (previous close) stands; the trade sets close/high.
	ohlcv(t, live, "100", "107", "100", "107", "0.5")
}

func TestRollToUnseededOrUnknownIntervalIsNoop(t *testing.T) {
	var s Series
	s.Reset("1")
	if fin := s.RollTo(1_000_000); fin != nil {
		t.Error("unseeded series must not roll")
	}
	s.Reset("bogus")
	s.Seed([]Bar{bar(0, "1", "1", "1", "1", "1")}, 0)
	if fin := s.RollTo(10_000_000); fin != nil {
		t.Error("unknown interval must not roll")
	}
}

func TestJumpPastCapSkipsIntermediates(t *testing.T) {
	var s Series
	s.ResetGapless("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)

	// maxSynthOnJump+1 empty buckets would be needed — the fill is skipped and
	// the series jumps, leaving a hole instead of ballooning.
	target := int64(60_000 + (maxSynthOnJump+2)*60_000)
	fin := s.RollTo(target)
	if len(fin) != 1 || fin[0].Time != 60_000 {
		t.Fatalf("capped roll finalized %d buckets, want just the live one", len(fin))
	}
	c := s.Candles()
	if len(c) != 2 || c[1].Time != target {
		t.Errorf("series = %d bars, live %d, want jump straight to %d", len(c), c[len(c)-1].Time, target)
	}
}

func TestSeedOverwritesFoldedAndHonorsAsOf(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)
	s.FoldTrade(10, "150", "9", 61_000) // drift the live bucket

	// Authoritative re-seed as of trade id 10: overwrites the bucket, and a
	// trade at/below 10 must not re-fold.
	s.Seed([]Bar{bar(60_000, "100", "120", "90", "110", "3")}, 10)
	live, _ := s.Live()
	ohlcv(t, live, "100", "120", "90", "110", "3")
	if _, up := s.FoldTrade(10, "999", "1", 61_500); up {
		t.Error("a trade at/below asOfTradeID must not fold after seed")
	}
	s.FoldTrade(11, "111", "1", 61_500) // strictly newer folds
	if live, _ = s.Live(); live.Close != "111" {
		t.Errorf("close = %v, want 111 after a newer trade", live.Close)
	}
	// The re-parsed live extremes govern later folds: 111 < 120 keeps the high.
	if live.High != "120" {
		t.Errorf("high = %v, want 120 preserved from the seed", live.High)
	}
}

func TestBehindEdgeTradeDoesNotConsumeID(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(120_000, "100", "100", "100", "100", "1")}, 0)

	s.FoldTrade(50, "50", "1", 60_500) // ts behind live edge
	s.FoldTrade(49, "130", "1", 121_000)
	if live, _ := s.Live(); live.Close != "130" {
		t.Errorf("close = %v, want 130 — behind-edge trade wrongly consumed id 50", live.Close)
	}
}

func TestSeedDoesNotRegressHighWater(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 10)
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 3) // stale asOf
	if _, up := s.FoldTrade(5, "999", "1", 61_000); up {
		t.Error("a stale asOfTradeID must not lower the high-water mark")
	}
}

func TestNaNAndInfAreRejected(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{
		bar(60_000, "100", "100", "100", "100", "1"),
		bar(120_000, "NaN", "1", "1", "1", "1"),
		bar(180_000, "Inf", "1", "1", "1", "1"),
	}, 0)
	if got := len(s.Candles()); got != 1 {
		t.Fatalf("want 1 candle (NaN/Inf rows dropped), got %d", got)
	}
	// A NaN trade price is a no-op that must not consume the id.
	if _, up := s.FoldTrade(1, "NaN", "1", 61_000); up {
		t.Error("NaN trade folded")
	}
	s.FoldTrade(1, "105", "1", 61_000) // id 1 still available
	if live, _ := s.Live(); live.Close != "105" {
		t.Errorf("close = %v, want 105 — NaN trade wrongly consumed the id", live.Close)
	}
}

func TestUnknownIntervalSeedsButNeverFolds(t *testing.T) {
	var s Series
	s.Reset("bogus")
	s.Seed([]Bar{bar(0, "1", "1", "1", "1", "1")}, 0)
	if s.Empty() {
		t.Fatal("seed should still install bars for an unknown interval")
	}
	if _, up := s.FoldTrade(1, "2", "1", 10_000_000); up {
		t.Error("unknown interval must not fold")
	}
}

func TestBackfillPrependsOlderAndReportsAdded(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{
		bar(180_000, "3", "3", "3", "3", "1"),
		bar(240_000, "4", "4", "4", "4", "1"), // live edge
	}, 0)
	if got := s.OldestTime(); got != 180_000 {
		t.Fatalf("OldestTime = %d, want 180000", got)
	}

	added := s.Backfill([]Bar{
		bar(60_000, "1", "1", "1", "1", "1"),
		bar(120_000, "2", "2", "2", "2", "1"),
	})
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}
	c := s.Candles()
	if len(c) != 4 || c[0].Time != 60_000 || c[3].Time != 240_000 {
		t.Errorf("backfill did not prepend in order: %+v", c)
	}
	if got := s.OldestTime(); got != 60_000 {
		t.Errorf("OldestTime after backfill = %d, want 60000", got)
	}
}

func TestBackfillDedupsAlreadyLoaded(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(120_000, "2", "2", "2", "2", "1"), bar(180_000, "3", "3", "3", "3", "1")}, 0)
	if added := s.Backfill([]Bar{bar(60_000, "1", "1", "1", "1", "1"), bar(120_000, "9", "9", "9", "9", "9")}); added != 1 {
		t.Errorf("added = %d, want 1 (the overlapping bar is not new)", added)
	}
	if added := s.Backfill([]Bar{bar(120_000, "2", "2", "2", "2", "1")}); added != 0 {
		t.Errorf("added = %d, want 0 (nothing new)", added)
	}
}

func TestSeedPreservesBackfilledHistory(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(180_000, "3", "3", "3", "3", "1")}, 0)
	s.Backfill([]Bar{bar(60_000, "1", "1", "1", "1", "1"), bar(120_000, "2", "2", "2", "2", "1")})

	// A re-seed of the recent window must NOT drop older loaded bars — only
	// overwrite the buckets it covers.
	s.Seed([]Bar{bar(180_000, "3", "30", "3", "30", "5")}, 0)
	c := s.Candles()
	if len(c) != 3 || c[0].Time != 60_000 {
		t.Fatalf("re-seed dropped backfilled history: %+v", c)
	}
	if c[2].Close != "30" || c[2].High != "30" {
		t.Errorf("re-seed did not overwrite the live bucket: %+v", c[2])
	}
}

func TestTrimOldestKeepsNewest(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{
		bar(60_000, "1", "1", "1", "1", "1"),
		bar(120_000, "2", "2", "2", "2", "1"),
		bar(180_000, "3", "3", "3", "3", "1"),
	}, 0)
	s.TrimOldest(2)
	c := s.Candles()
	if len(c) != 2 || c[0].Time != 120_000 || c[1].Time != 180_000 {
		t.Errorf("trim kept %+v, want the newest two", c)
	}
	s.TrimOldest(5) // larger than the series: no-op
	if len(s.Candles()) != 2 {
		t.Error("over-large trim changed the series")
	}
	// The live bucket still folds after a trim.
	if _, up := s.FoldTrade(1, "4", "1", 181_000); !up {
		t.Error("fold after trim failed")
	}
}

func TestResetClearsState(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "1", "1", "1", "1", "1")}, 5)
	s.Reset("5")
	if !s.Empty() {
		t.Error("Reset must clear the series")
	}
	if s.Interval() != "5" {
		t.Errorf("interval = %q, want 5", s.Interval())
	}
	// lastTradeID cleared: a low id folds again after reset+seed.
	s.Seed([]Bar{bar(300_000, "1", "1", "1", "1", "1")}, 0)
	if _, up := s.FoldTrade(1, "2", "1", 300_100); !up {
		t.Fatal("low id must fold after Reset")
	}
}

func TestCandlesReturnsACopy(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "1", "1", "1", "1", "1")}, 0)
	c := s.Candles()
	c[0].Close = "999"
	if live, _ := s.Live(); live.Close != "1" {
		t.Error("Candles() must return a copy")
	}
	if !reflect.DeepEqual(s.Candles()[0].Bar(), bar(60_000, "1", "1", "1", "1", "1")) {
		t.Error("Bar() round-trip mismatch")
	}
}

func TestFarFutureTimestampIsRejectedNotFolded(t *testing.T) {
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)

	// A corrupt far-future timestamp must be a no-op — no wrapped bucket, no
	// consumed id — and RollTo with an insane clock must not roll either.
	huge := int64(60_000) + (maxJumpBuckets+1)*60_000
	if fin, up := s.FoldTrade(1, "101", "1", huge); len(fin) > 0 || up {
		t.Fatal("far-future trade folded")
	}
	if live, _ := s.Live(); live.Time != 60_000 {
		t.Fatalf("live moved to %d", live.Time)
	}
	if fin := s.RollTo(huge); fin != nil {
		t.Fatal("far-future RollTo rolled")
	}
	// The id was not consumed: the same id still folds at a sane timestamp.
	if _, up := s.FoldTrade(1, "101", "1", 61_000); !up {
		t.Fatal("id was wrongly consumed by the rejected trade")
	}
}

func TestPlainResetJumpLeavesHole(t *testing.T) {
	// Plain (non-gapless) mode — the TUI's: a multi-period jump does NOT
	// synthesize the skipped empty buckets; no trades means no price, and the
	// slot-based chart compresses the axis across the hole.
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "100", "110", "90", "105", "1")}, 0)

	fin, up := s.FoldTrade(1, "108", "1", 245_000) // three buckets ahead
	if !up || len(fin) != 1 || fin[0].Time != 60_000 {
		t.Fatalf("plain-mode jump finalized %+v, want just the closed live bucket", fin)
	}
	c := s.Candles()
	if len(c) != 2 || c[1].Time != 240_000 {
		t.Fatalf("plain-mode series = %+v, want a hole between 60000 and 240000", c)
	}
}

func TestNegativeTimestampRowRejected(t *testing.T) {
	// A negative bucket start is corrupt (unix-ms starts are positive) and,
	// unchecked, would let (ts - liveStart) overflow the fold's jump guard.
	var s Series
	s.Reset("1")
	s.Seed([]Bar{
		bar(-9_000_000_000_000_000_000, "1", "1", "1", "1", "1"),
		bar(60_000, "100", "100", "100", "100", "1"),
	}, 0)
	c := s.Candles()
	if len(c) != 1 || c[0].Time != 60_000 {
		t.Fatalf("negative-timestamp row survived: %+v", c)
	}
}

func TestPlainModeRollToIsNoop(t *testing.T) {
	// RollTo's product is synthesized flat buckets — exactly what plain mode
	// exists to avoid — so after a plain Reset it must do nothing.
	var s Series
	s.Reset("1")
	s.Seed([]Bar{bar(60_000, "100", "100", "100", "100", "1")}, 0)
	if fin := s.RollTo(600_000); fin != nil {
		t.Fatalf("plain-mode RollTo rolled: %+v", fin)
	}
	if live, _ := s.Live(); live.Time != 60_000 {
		t.Errorf("plain-mode RollTo moved the live edge to %d", live.Time)
	}
}
