// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package candles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/stream"
)

// fakeClock is an adjustable server/local clock for the synth.
type fakeClock struct {
	mu sync.Mutex
	ms int64
}

func (c *fakeClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

func (c *fakeClock) set(ms int64) {
	c.mu.Lock()
	c.ms = ms
	c.mu.Unlock()
}

// fetchStub queues per-scope responses and records calls.
type fetchStub struct {
	mu    sync.Mutex
	rows  map[string][]Bar // keyed "symbol@interval"
	fails map[string]error
	calls []fetchCall
}

type fetchCall struct {
	key   string
	limit int
}

func (f *fetchStub) fetch(_ context.Context, symbol, interval string, limit int, _ int64) ([]Bar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := symbol + "@" + interval
	f.calls = append(f.calls, fetchCall{key: key, limit: limit})
	if err := f.fails[key]; err != nil {
		return nil, err
	}
	return f.rows[key], nil
}

func (f *fetchStub) lastLimit(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].key == key {
			return f.calls[i].limit
		}
	}
	return -1
}

// newTestSynth builds and starts a synth over the stub for btc_krw.
func newTestSynth(t *testing.T, clk *fakeClock, fs *fetchStub, history int, intervals ...string) *Synth {
	t.Helper()
	if len(intervals) == 0 {
		intervals = []string{"1"}
	}
	s, err := NewSynth(Config{
		Symbols: []string{"btc_krw"}, Intervals: intervals, History: history,
		Fetch: fs.fetch, ServerNow: clk.now, Now: clk.now,
		FinalizeGraceMs: 2000, RetryMinMs: 100, RetryMaxMs: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Start(ctx)
	return s
}

// drain applies every seed result that arrives within the deadline.
func drain(t *testing.T, s *Synth, want int) []stream.Event {
	t.Helper()
	var evs []stream.Event
	for i := 0; i < want; i++ {
		select {
		case res := <-s.Results():
			evs = append(evs, s.Apply(res)...)
		case <-time.After(5 * time.Second):
			t.Fatalf("seed result %d/%d never arrived", i+1, want)
		}
	}
	return evs
}

// seedScopes connects, delivers the subscribe snapshot (which kicks the
// seeds), and applies n results.
func seedScopes(t *testing.T, s *Synth, n int) []stream.Event {
	t.Helper()
	s.OnNotice(connected("public"))
	evs := s.OnData(snapEvent("btc_krw"))
	if got := len(notices(evs, stream.BackfillStart)); got != n {
		t.Fatalf("snapshot kicked %d seeds, want %d: %+v", got, n, evs)
	}
	return drain(t, s, n)
}

// lines unpacks the candle Data events among evs.
func lines(t *testing.T, evs []stream.Event) []candleLine {
	t.Helper()
	var out []candleLine
	for _, ev := range evs {
		d, ok := ev.(stream.Data)
		if !ok {
			continue
		}
		if d.Channel != Channel || d.Origin != stream.OriginDerived {
			t.Fatalf("candle event with wrong channel/origin: %+v", d)
		}
		var l candleLine
		if err := json.Unmarshal(d.Payload, &l); err != nil {
			t.Fatalf("bad candle payload: %v", err)
		}
		out = append(out, l)
	}
	return out
}

// notices unpacks the Notice events among evs, filtered by code.
func notices(evs []stream.Event, code stream.NoticeCode) []stream.Notice {
	var out []stream.Notice
	for _, ev := range evs {
		if n, ok := ev.(stream.Notice); ok && n.Code == code {
			out = append(out, n)
		}
	}
	return out
}

// tradeEvent builds a live WS trade frame event.
func tradeEvent(sym string, serverTime int64, rows ...string) stream.Data {
	return stream.Data{
		Channel: stream.ChannelTrade, Symbol: sym, Origin: stream.OriginRealtime,
		ServerTime: serverTime,
		Payload:    json.RawMessage(fmt.Sprintf(`{"data":[%s]}`, join(rows))),
	}
}

// snapEvent builds the trade subscription's snapshot frame (possibly empty —
// an all-duplicate resubscribe snapshot is rebuilt empty yet still delivered).
func snapEvent(sym string, rows ...string) stream.Data {
	return stream.Data{
		Channel: stream.ChannelTrade, Symbol: sym, Origin: stream.OriginSnapshot,
		ServerTime: 0,
		Payload:    json.RawMessage(fmt.Sprintf(`{"snapshot":true,"data":[%s]}`, join(rows))),
	}
}

func join(rows []string) string {
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += ","
		}
		out += r
	}
	return out
}

func trow(id, ts int64, price, qty string) string {
	return fmt.Sprintf(`{"tradeId":%d,"timestamp":%d,"price":%q,"qty":%q}`, id, ts, price, qty)
}

func connected(endpoint string) stream.Notice {
	return stream.Notice{Code: stream.Connected, Details: map[string]any{"endpoint": endpoint}}
}

func disconnected(endpoint string) stream.Notice {
	return stream.Notice{Code: stream.Disconnected, Details: map[string]any{"endpoint": endpoint}}
}

func TestSynthSeedEmitsHistoryAndLive(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {
		bar(60_000, "1", "2", "0.5", "1.5", "5"),
		bar(120_000, "1.5", "3", "1", "2", "7"),
		bar(180_000, "2", "2", "2", "2", "1"), // current still-open bucket
	}}}
	s := newTestSynth(t, clk, fs, 2)

	evs := seedScopes(t, s, 1)
	if got := fs.lastLimit("btc_krw@1"); got != 3 { // history 2 + live bucket
		t.Errorf("seed limit = %d, want 3", got)
	}
	if len(notices(evs, stream.BackfillDone)) != 1 {
		t.Fatalf("no BACKFILL_DONE in %+v", evs)
	}
	ls := lines(t, evs)
	if len(ls) != 3 {
		t.Fatalf("want 3 candle lines (2 final + live), got %d: %+v", len(ls), ls)
	}
	if !ls[0].Final || !ls[1].Final || ls[2].Final {
		t.Errorf("final flags wrong: %+v", ls)
	}
	if ls[2].Timestamp != 180_000 || ls[2].Close != "2" || ls[2].Interval != "1" {
		t.Errorf("live line = %+v", ls[2])
	}
	// The snapshot kicked the seed exactly once — a second snapshot without a
	// reconnect must not re-fetch.
	if evs := s.OnData(snapEvent("btc_krw")); len(notices(evs, stream.BackfillStart)) != 0 {
		t.Errorf("non-stale snapshot re-seeded: %+v", evs)
	}
}

func TestSynthSeedFillsRESTHoles(t *testing.T) {
	clk := &fakeClock{ms: 250_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {
		bar(60_000, "1", "2", "0.5", "1.5", "5"),
		// 120_000 skipped by the server (no trades that minute)
		bar(180_000, "1.5", "3", "1", "2", "7"),
		bar(240_000, "2", "2", "2", "2", "1"),
	}}}
	s := newTestSynth(t, clk, fs, 10)
	ls := lines(t, seedScopes(t, s, 1))
	if len(ls) != 4 {
		t.Fatalf("want 4 lines (hole filled), got %d: %+v", len(ls), ls)
	}
	hole := ls[1]
	if hole.Timestamp != 120_000 || hole.Open != "1.5" || hole.Close != "1.5" || hole.Volume != "0" || !hole.Final {
		t.Errorf("hole line = %+v, want flat 1.5 vol 0 final", hole)
	}
}

func TestSynthSnapshotRowsSitUnderSeedWatermark(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "105", "100", "105", "3")}}}
	s := newTestSynth(t, clk, fs, 0)
	s.OnNotice(connected("public"))

	// The snapshot carries a trade already counted inside the REST bucket (its
	// volume 3 includes qty 0.5 of trade 30). Kicking the seed FROM the
	// snapshot puts id 30 under the watermark, so it must not re-fold.
	evs := s.OnData(snapEvent("btc_krw", trow(30, 184_000, "105", "0.5")))
	if len(notices(evs, stream.BackfillStart)) != 1 {
		t.Fatalf("snapshot did not kick the seed: %+v", evs)
	}
	ls := lines(t, drain(t, s, 1))
	if len(ls) != 1 || ls[0].Volume != "3" {
		t.Fatalf("seed emit = %+v, want the authoritative volume 3", ls)
	}
	if evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(30, 184_000, "105", "0.5"))); len(evs) != 0 {
		t.Errorf("watermarked trade re-folded: %+v", evs)
	}
	// A strictly newer trade folds on top of the authoritative bucket.
	ls = lines(t, s.OnData(tradeEvent("btc_krw", 187_000, trow(31, 187_000, "106", "0.5"))))
	if len(ls) != 1 {
		t.Fatalf("newer trade did not fold: %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "106", "100", "106", "3.5")
}

func TestSynthTradesFoldAndEmitLiveUpdates(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	evs := s.OnData(tradeEvent("btc_krw", 186_000,
		trow(11, 186_000, "105", "0.5"), trow(12, 187_000, "95", "0.25")))
	ls := lines(t, evs)
	if len(ls) != 1 || ls[0].Final {
		t.Fatalf("want 1 non-final live update per frame, got %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "105", "95", "95", "1.75")

	// A duplicate frame folds nothing and emits nothing.
	if evs := s.OnData(tradeEvent("btc_krw", 186_500, trow(11, 186_000, "105", "0.5"))); len(evs) != 0 {
		t.Errorf("duplicate frame emitted %+v", evs)
	}
	// A non-trade channel is ignored.
	if evs := s.OnData(stream.Data{Channel: stream.ChannelTicker, Symbol: "btc_krw"}); len(evs) != 0 {
		t.Errorf("ticker event emitted %+v", evs)
	}
}

func TestSynthTradeRolloverEmitsFinalThenLive(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	ls := lines(t, s.OnData(tradeEvent("btc_krw", 241_000, trow(11, 241_000, "108", "2"))))
	if len(ls) != 2 {
		t.Fatalf("want final+live, got %+v", ls)
	}
	if !ls[0].Final || ls[0].Timestamp != 180_000 || ls[0].Close != "100" {
		t.Errorf("final line = %+v", ls[0])
	}
	if ls[1].Final || ls[1].Timestamp != 240_000 || ls[1].Open != "108" || ls[1].Volume != "2" {
		t.Errorf("live line = %+v", ls[1])
	}
}

func TestSynthTickFinalizesQuietBucketAfterGrace(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	// Bucket [180000, 240000) — grace 2000 means it seals at 242000.
	clk.set(241_900)
	if evs := s.Tick(); len(lines(t, evs)) != 0 {
		t.Fatalf("finalized before the grace elapsed: %+v", evs)
	}
	clk.set(242_000)
	ls := lines(t, s.Tick())
	if len(ls) != 2 {
		t.Fatalf("want final+live at the boundary, got %+v", ls)
	}
	if !ls[0].Final || ls[0].Timestamp != 180_000 {
		t.Errorf("final = %+v", ls[0])
	}
	// The successor is the synthesized flat zero-volume live bucket.
	if ls[1].Final || ls[1].Timestamp != 240_000 || ls[1].Close != "100" || ls[1].Volume != "0" {
		t.Errorf("live = %+v, want flat zero bucket", ls[1])
	}
	// A later trade inside the sealed bucket's window must NOT reopen it.
	if evs := s.OnData(tradeEvent("btc_krw", 242_100, trow(20, 239_000, "50", "1"))); len(lines(t, evs)) != 0 {
		t.Errorf("behind-edge trade reopened a sealed bucket: %+v", evs)
	}
}

func TestSynthTickHoldsWhileDisconnected(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	// A disconnect suspends finalization: no bar seals while blind.
	s.OnNotice(disconnected("public"))
	clk.set(600_000)
	if evs := s.Tick(); len(evs) != 0 {
		t.Fatalf("finalized while disconnected: %+v", evs)
	}
}

func TestSynthReconnectReseedsOnSnapshot(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	s.OnNotice(disconnected("public"))
	// The reconnect itself marks scopes stale but does not fetch — the
	// resubscribe snapshot does, so its rows sit under the seed watermark.
	if evs := s.OnNotice(connected("public")); len(notices(evs, stream.BackfillStart)) != 0 {
		t.Fatalf("reconnect fetched before the snapshot: %+v", evs)
	}
	fs.mu.Lock()
	fs.rows["btc_krw@1"] = []Bar{bar(180_000, "100", "120", "95", "110", "9")}
	fs.mu.Unlock()
	evs := s.OnData(snapEvent("btc_krw"))
	starts := notices(evs, stream.BackfillStart)
	if len(starts) != 1 || starts[0].Details["reason"] != "reconnect" {
		t.Fatalf("snapshot after reconnect did not re-seed: %+v", evs)
	}
	// The corrected rows re-emit under last-wins.
	ls := lines(t, drain(t, s, 1))
	if len(ls) != 1 || ls[0].Final || ls[0].Close != "110" {
		t.Fatalf("corrected live = %+v", ls)
	}
}

func TestSynthTradeGapReseedsSymbol(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	for _, n := range []stream.Notice{
		{Code: stream.DataGap, Details: map[string]any{"channel": "trade", "symbol": "btc_krw"}},
		{Code: stream.BackfillDone, Details: map[string]any{"reason": "gap", "channel": "trade", "symbol": "btc_krw"}},
	} {
		evs := s.OnNotice(n)
		if len(notices(evs, stream.BackfillStart)) != 1 {
			t.Fatalf("%s did not re-seed: %+v", n.Code, evs)
		}
		drain(t, s, 1)
	}
	// A gap on another symbol or a non-gap BACKFILL_DONE is ignored.
	if evs := s.OnNotice(stream.Notice{Code: stream.DataGap, Details: map[string]any{"channel": "trade", "symbol": "eth_krw"}}); len(evs) != 0 {
		t.Errorf("foreign-symbol gap re-seeded: %+v", evs)
	}
	if evs := s.OnNotice(stream.Notice{Code: stream.BackfillDone, Details: map[string]any{"reason": "initial", "channels": "..."}}); len(evs) != 0 {
		t.Errorf("non-gap BACKFILL_DONE re-seeded: %+v", evs)
	}
}

func TestSynthSeedFailureRetriesWithBackoff(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{
		rows:  map[string][]Bar{},
		fails: map[string]error{"btc_krw@1": errors.New("boom")},
	}
	s := newTestSynth(t, clk, fs, 0)
	s.OnNotice(connected("public"))
	s.OnData(snapEvent("btc_krw"))

	evs := drain(t, s, 1)
	if len(notices(evs, stream.BackfillFailed)) != 1 {
		t.Fatalf("no BACKFILL_FAILED: %+v", evs)
	}
	// The START must still be closed by a paired DONE (failures:1) — the
	// --stateful store's backfill counter balances on the pairing.
	dones := notices(evs, stream.BackfillDone)
	if len(dones) != 1 || dones[0].Details["failures"] != 1 {
		t.Fatalf("failed seed did not pair its START with a DONE(failures:1): %+v", evs)
	}
	// Not due yet: no retry.
	if evs := s.Tick(); len(evs) != 0 {
		t.Fatalf("retried early: %+v", evs)
	}
	// Past the first backoff (RetryMinMs=100): a retry kicks with reason retry.
	clk.set(clk.now() + 150)
	evs = s.Tick()
	starts := notices(evs, stream.BackfillStart)
	if len(starts) != 1 || starts[0].Details["reason"] != "retry" {
		t.Fatalf("retry notice = %+v", evs)
	}
	// Let the retry succeed and verify the scope becomes live.
	fs.mu.Lock()
	delete(fs.fails, "btc_krw@1")
	fs.rows["btc_krw@1"] = []Bar{bar(180_000, "100", "100", "100", "100", "1")}
	fs.mu.Unlock()
	ls := lines(t, drain(t, s, 1))
	if len(ls) != 1 || ls[0].Final {
		t.Fatalf("post-retry emit = %+v", ls)
	}
}

func TestSynthGapReseedWindowCoversStaleSpan(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {
		bar(120_000, "1", "1", "1", "1", "1"),
		bar(180_000, "100", "100", "100", "100", "1"),
	}}}
	s := newTestSynth(t, clk, fs, 1)
	seedScopes(t, s, 1) // lastFinal = 120_000

	// 10 buckets later, a gap: the re-seed window must span back to lastFinal.
	clk.set(120_000 + 11*60_000)
	s.OnNotice(stream.Notice{Code: stream.DataGap, Details: map[string]any{"channel": "trade", "symbol": "btc_krw"}})
	drain(t, s, 1) // the fetch has necessarily recorded its call once its result lands
	if got := fs.lastLimit("btc_krw@1"); got < 12 {
		t.Errorf("gap re-seed limit = %d, want >= 12 (cover the stale span)", got)
	}
}

func TestSynthMultipleIntervalsShareTrades(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{
		"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")},
		"btc_krw@5": {bar(0, "90", "110", "80", "100", "42")},
	}}
	s := newTestSynth(t, clk, fs, 0, "1", "5")
	seedScopes(t, s, 2)

	evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(11, 186_000, "105", "0.5")))
	ls := lines(t, evs)
	if len(ls) != 2 {
		t.Fatalf("want one live update per interval, got %+v", ls)
	}
	ivs := map[string]bool{}
	for _, l := range ls {
		ivs[l.Interval] = true
		if l.Final {
			t.Errorf("unexpected final: %+v", l)
		}
	}
	if !ivs["1"] || !ivs["5"] {
		t.Errorf("intervals covered = %v", ivs)
	}
}

func TestSynthUnseededScopeIgnoresTradesUntilSeeded(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{}, fails: map[string]error{"btc_krw@1": errors.New("down")}}
	s := newTestSynth(t, clk, fs, 0)
	s.OnNotice(connected("public"))
	s.OnData(snapEvent("btc_krw"))
	drain(t, s, 1) // failed

	// Trades before any seed: no emission, but the watermark advances.
	if evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(30, 186_000, "105", "0.5"))); len(evs) != 0 {
		t.Fatalf("unseeded scope emitted %+v", evs)
	}
	fs.mu.Lock()
	delete(fs.fails, "btc_krw@1")
	fs.rows["btc_krw@1"] = []Bar{bar(180_000, "100", "105", "100", "105", "3")}
	fs.mu.Unlock()
	clk.set(clk.now() + 150)
	s.Tick()
	drain(t, s, 1)
	// Trade id 30 was seen before the seed landed — it must not re-fold.
	if evs := s.OnData(tradeEvent("btc_krw", 187_000, trow(30, 186_000, "105", "0.5"))); len(evs) != 0 {
		t.Errorf("pre-seed trade re-folded after seed: %+v", evs)
	}
	// A strictly newer trade folds.
	if ls := lines(t, s.OnData(tradeEvent("btc_krw", 188_000, trow(31, 188_000, "107", "1")))); len(ls) != 1 {
		t.Errorf("newer trade did not fold: %+v", ls)
	}
}

func TestSynthBackfillOriginTradesFold(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	// A gap patch delivers REST rows (bare array) — in-bucket rows fold.
	ev := stream.Data{
		Channel: stream.ChannelTrade, Symbol: "btc_krw", Origin: stream.OriginBackfill,
		ServerTime: 186_000,
		Payload:    json.RawMessage(`[` + trow(12, 185_000, "103", "0.5") + `]`),
	}
	ls := lines(t, s.OnData(ev))
	if len(ls) != 1 {
		t.Fatalf("backfill trade did not fold: %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "103", "100", "103", "1.5")
}

func TestSynthMaxSeedRowsMatchesOpsCap(t *testing.T) {
	// maxSeedRows mirrors ops.CandlesMaxLimit without importing ops; this pins
	// the value so a cap change there is noticed here.
	if maxSeedRows != 5000 {
		t.Errorf("maxSeedRows = %d — keep in sync with ops.CandlesMaxLimit", maxSeedRows)
	}
}

// ohlcvLine asserts one emitted line's decimal-string fields.
func ohlcvLine(t *testing.T, l candleLine, o, h, lo, c, v string) {
	t.Helper()
	if l.Open != o || l.High != h || l.Low != lo || l.Close != c || l.Volume != v {
		t.Errorf("line = O %s H %s L %s C %s V %s, want O %s H %s L %s C %s V %s",
			l.Open, l.High, l.Low, l.Close, l.Volume, o, h, lo, c, v)
	}
}

func TestSynthSubscribeFailedKillsScopes(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	evs := s.OnNotice(stream.Notice{Code: stream.SubscribeFailed, Details: map[string]any{
		"endpoint": "public", "channel": "trade", "symbols": []string{"btc_krw"},
		"code": "INVALID_SYMBOL", "message": "unknown symbol",
	}})
	kills := notices(evs, stream.SubscribeFailed)
	if len(kills) != 1 || kills[0].Details["channel"] != Channel || kills[0].Details["symbol"] != "btc_krw" {
		t.Fatalf("want one candle-scoped SUBSCRIBE_FAILED, got %+v", evs)
	}
	// Dead scope: no finalization while blind, no folds, no re-seed kicks.
	clk.set(400_000)
	if evs := s.Tick(); len(evs) != 0 {
		t.Errorf("dead scope finalized: %+v", evs)
	}
	if evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(50, 186_000, "105", "1"))); len(evs) != 0 {
		t.Errorf("dead scope folded: %+v", evs)
	}
	if evs := s.OnData(snapEvent("btc_krw")); len(evs) != 0 {
		t.Errorf("dead scope re-seeded on snapshot: %+v", evs)
	}
	if evs := s.OnNotice(stream.Notice{Code: stream.DataGap, Details: map[string]any{"channel": "trade", "symbol": "btc_krw"}}); len(evs) != 0 {
		t.Errorf("dead scope re-seeded on gap: %+v", evs)
	}
	// A second failure notice does not re-report.
	if evs := s.OnNotice(stream.Notice{Code: stream.SubscribeFailed, Details: map[string]any{
		"endpoint": "public", "channel": "trade", "symbols": []string{"btc_krw"}, "code": "INVALID_SYMBOL",
	}}); len(evs) != 0 {
		t.Errorf("dead scope re-reported: %+v", evs)
	}
}

func TestSynthUnsubscribeRejectDoesNotKill(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	if evs := s.OnNotice(stream.Notice{Code: stream.SubscribeFailed, Details: map[string]any{
		"endpoint": "public", "channel": "trade", "symbols": []string{"btc_krw"}, "method": "unsubscribe",
	}}); len(evs) != 0 {
		t.Fatalf("benign unsubscribe reject produced events: %+v", evs)
	}
	// Still alive: trades keep folding.
	if ls := lines(t, s.OnData(tradeEvent("btc_krw", 186_000, trow(50, 186_000, "105", "1")))); len(ls) != 1 {
		t.Errorf("scope should still fold after a benign unsubscribe reject: %+v", ls)
	}
}

func TestSynthDropsFutureDatedTrades(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	// A trade from the server's future (past now + grace) is corrupt: folding
	// it would strand the live edge in the future, where every real trade is
	// "behind the edge". It must be dropped — watermark included.
	future := clk.now() + 10*60_000
	if evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(50, future, "999", "1"))); len(evs) != 0 {
		t.Fatalf("future-dated trade folded: %+v", evs)
	}
	// The live edge did not move: a real trade still folds, and the corrupt
	// row's id was not consumed by the watermark.
	ls := lines(t, s.OnData(tradeEvent("btc_krw", 187_000, trow(50, 187_000, "105", "0.5"))))
	if len(ls) != 1 || ls[0].Timestamp != 180_000 {
		t.Fatalf("live edge stranded after future-dated trade: %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "105", "100", "105", "1.5")

	// A mixed batch keeps the legit rows.
	ls = lines(t, s.OnData(tradeEvent("btc_krw", 188_000,
		trow(60, future, "999", "1"), trow(61, 188_000, "106", "0.5"))))
	if len(ls) != 1 || ls[0].Close != "106" {
		t.Fatalf("mixed batch mishandled: %+v", ls)
	}
}

func TestSynthReseedBeforeFirstFinalizationCoversDowntime(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1) // nothing finalized yet: lastFinal == 0

	// The connection drops inside the very first bucket and stays down across
	// five quiet boundaries. The reconnect re-seed must anchor its window on
	// the live edge (180000), not on the zero lastFinal, or those closed
	// buckets are never fetched and never finalized.
	s.OnNotice(disconnected("public"))
	clk.set(180_000 + 6*60_000) // 540000
	fs.mu.Lock()
	fs.rows["btc_krw@1"] = []Bar{
		bar(180_000, "100", "100", "100", "100", "1"),
		// quiet downtime: the server may skip the empty buckets entirely
		bar(540_000, "101", "101", "101", "101", "0.5"),
	}
	fs.mu.Unlock()
	s.OnNotice(connected("public"))
	evs := s.OnData(snapEvent("btc_krw"))
	if len(notices(evs, stream.BackfillStart)) != 1 {
		t.Fatalf("reconnect snapshot did not re-seed: %+v", evs)
	}
	evs = drain(t, s, 1)
	if got := fs.lastLimit("btc_krw@1"); got < 7 {
		t.Fatalf("re-seed limit = %d, want >= 7 (cover the 6-bucket downtime)", got)
	}
	// Every crossed boundary is finalized (REST holes filled flat), and the
	// current bucket is live.
	ls := lines(t, evs)
	var finals []int64
	for _, l := range ls {
		if l.Final {
			finals = append(finals, l.Timestamp)
		}
	}
	want := []int64{180_000, 240_000, 300_000, 360_000, 420_000, 480_000}
	if len(finals) != len(want) {
		t.Fatalf("finalized %v, want %v", finals, want)
	}
	for i := range want {
		if finals[i] != want[i] {
			t.Fatalf("finalized %v, want %v", finals, want)
		}
	}
	if last := ls[len(ls)-1]; last.Final || last.Timestamp != 540_000 {
		t.Errorf("live after re-seed = %+v, want non-final 540000", last)
	}
}

func TestSynthKillWhilePendingStillPairsDone(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	block := make(chan struct{})
	fetch := func(ctx context.Context, _, _ string, _ int, _ int64) ([]Bar, error) {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []Bar{bar(180_000, "100", "100", "100", "100", "1")}, nil
	}
	s, err := NewSynth(Config{
		Symbols: []string{"btc_krw"}, Intervals: []string{"1"},
		Fetch: fetch, ServerNow: clk.now, Now: clk.now,
		FinalizeGraceMs: 2000, RetryMinMs: 100, RetryMaxMs: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Start(ctx)
	s.OnNotice(connected("public"))
	s.OnData(snapEvent("btc_krw")) // kicks the seed, which blocks in fetch

	// The trade subscription dies while the fetch is in flight.
	s.OnNotice(stream.Notice{Code: stream.SubscribeFailed, Details: map[string]any{
		"endpoint": "public", "channel": "trade", "symbols": []string{"btc_krw"}, "code": "X",
	}})
	close(block) // let the fetch resolve; its result must be discarded
	evs := drain(t, s, 1)
	if len(lines(t, evs)) != 0 {
		t.Fatalf("discarded seed emitted candle lines: %+v", evs)
	}
	dones := notices(evs, stream.BackfillDone)
	if len(dones) != 1 || dones[0].Details["discarded"] != true {
		t.Fatalf("dead-scope discard did not pair its START with a discarded DONE: %+v", evs)
	}
}

func TestSynthReplaysInFlightTradesOverSeed(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "104", "100", "104", "3")}}}
	s := newTestSynth(t, clk, fs, 0)
	s.OnNotice(connected("public"))

	// Kick the seed, then deliver a trade the fetch cannot contain (executed
	// after the response was built). It must be buffered — no emission yet —
	// and replayed on top of the authoritative rows at Apply, or the bucket's
	// close/high would be stale forever (there is no periodic re-sync).
	if evs := s.OnData(snapEvent("btc_krw")); len(notices(evs, stream.BackfillStart)) != 1 {
		t.Fatalf("snapshot did not kick: %+v", evs)
	}
	if evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(40, 186_000, "110", "0.5"))); len(evs) != 0 {
		t.Fatalf("in-flight trade emitted before the seed applied: %+v", evs)
	}
	ls := lines(t, drain(t, s, 1))
	if len(ls) != 1 || ls[0].Final {
		t.Fatalf("seed emit = %+v", ls)
	}
	// REST said close 104 vol 3; the replayed trade extends it.
	ohlcvLine(t, ls[0], "100", "110", "100", "110", "3.5")
	// The replayed id is consumed: re-delivery is a no-op, newer folds.
	if evs := s.OnData(tradeEvent("btc_krw", 187_000, trow(40, 186_000, "110", "0.5"))); len(evs) != 0 {
		t.Errorf("replayed trade re-folded: %+v", evs)
	}
	if ls := lines(t, s.OnData(tradeEvent("btc_krw", 188_000, trow(41, 188_000, "111", "0.5")))); len(ls) != 1 || ls[0].Close != "111" {
		t.Errorf("post-apply trade did not fold: %+v", ls)
	}
}

func TestSynthReseedReplayPreservesNewerInFlightTrade(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	// A gap re-seed goes out; while it is in flight a newer trade arrives.
	// It is buffered (live output pauses for the round trip) and replayed, so
	// the applied bucket ends at the TRUE latest close even though the fetched
	// row predates the trade. (The corrected rows are staged BEFORE the gap
	// notice — the notice kicks the fetch goroutine immediately.)
	fs.mu.Lock()
	fs.rows["btc_krw@1"] = []Bar{bar(180_000, "100", "105", "99", "103", "2")} // as-of fetch build: pre-trade
	fs.mu.Unlock()
	s.OnNotice(stream.Notice{Code: stream.DataGap, Details: map[string]any{"channel": "trade", "symbol": "btc_krw"}})
	if evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(30, 186_000, "111", "0.25"))); len(evs) != 0 {
		t.Fatalf("in-flight trade folded during the re-seed: %+v", evs)
	}
	ls := lines(t, drain(t, s, 1))
	if len(ls) != 1 {
		t.Fatalf("re-seed emit = %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "111", "99", "111", "2.25")
}

func TestSynthReplayBufferOverflowFallsBack(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "3")}}}
	s, err := NewSynth(Config{
		Symbols: []string{"btc_krw"}, Intervals: []string{"1"},
		Fetch: fs.fetch, ServerNow: clk.now, Now: clk.now,
		FinalizeGraceMs: 2000, RetryMinMs: 100, RetryMaxMs: 1000,
		ReplayBufferCap: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Start(ctx)
	s.OnNotice(connected("public"))
	s.OnData(snapEvent("btc_krw"))

	// Three in-flight trades overflow the cap of 2: the Apply falls back to
	// the conservative exclude-all watermark (REST values stand; the buffered
	// trades are not replayed) instead of growing without bound.
	s.OnData(tradeEvent("btc_krw", 186_000, trow(41, 186_000, "110", "1"), trow(42, 186_100, "112", "1")))
	s.OnData(tradeEvent("btc_krw", 186_200, trow(43, 186_200, "114", "1")))
	ls := lines(t, drain(t, s, 1))
	if len(ls) != 1 {
		t.Fatalf("seed emit = %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "100", "100", "100", "3")
	// The exclude-all watermark consumed the buffered ids; a newer trade folds.
	if ls := lines(t, s.OnData(tradeEvent("btc_krw", 187_000, trow(44, 187_000, "115", "1")))); len(ls) != 1 || ls[0].Close != "115" {
		t.Errorf("post-overflow trade did not fold: %+v", ls)
	}
}

func TestSynthTickHoldsWhileSeedInFlight(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	// A re-seed goes out; the clock passes the bucket boundary while it is in
	// flight. Finalizing mid-revision could seal the bucket short (its
	// buffered trades have not folded yet), so Tick must hold.
	s.OnNotice(stream.Notice{Code: stream.DataGap, Details: map[string]any{"channel": "trade", "symbol": "btc_krw"}})
	clk.set(245_000)
	if evs := s.Tick(); len(lines(t, evs)) != 0 {
		t.Fatalf("finalized while a seed was in flight: %+v", evs)
	}
	drain(t, s, 1)
	if ls := lines(t, s.Tick()); len(ls) == 0 {
		t.Error("no finalization after the seed applied")
	}
}

func TestSynthFailedReseedFoldsBufferSoLiveKeepsFlowing(t *testing.T) {
	clk := &fakeClock{ms: 185_000}
	fs := &fetchStub{rows: map[string][]Bar{"btc_krw@1": {bar(180_000, "100", "100", "100", "100", "1")}}}
	s := newTestSynth(t, clk, fs, 0)
	seedScopes(t, s, 1)

	// A gap re-seed fails while a trade sits in the buffer: the already-live
	// scope must not go dark for the retry window — the buffered trade folds
	// on the failure path (the retry's own watermark re-captures after it, so
	// the retry rows do not re-fold it).
	fs.mu.Lock()
	fs.fails = map[string]error{"btc_krw@1": errors.New("boom")}
	fs.mu.Unlock()
	s.OnNotice(stream.Notice{Code: stream.DataGap, Details: map[string]any{"channel": "trade", "symbol": "btc_krw"}})
	if evs := s.OnData(tradeEvent("btc_krw", 186_000, trow(30, 186_000, "108", "0.5"))); len(evs) != 0 {
		t.Fatalf("in-flight trade folded during the re-seed: %+v", evs)
	}
	evs := drain(t, s, 1)
	if len(notices(evs, stream.BackfillFailed)) != 1 || len(notices(evs, stream.BackfillDone)) != 1 {
		t.Fatalf("failure notices wrong: %+v", evs)
	}
	ls := lines(t, evs)
	if len(ls) != 1 || ls[0].Final {
		t.Fatalf("failure path did not fold the buffer: %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "108", "100", "108", "1.5")

	// The retry succeeds with authoritative rows that INCLUDE the trade — it
	// must not double-fold on top.
	fs.mu.Lock()
	fs.fails = nil
	fs.rows["btc_krw@1"] = []Bar{bar(180_000, "100", "108", "100", "108", "1.5")}
	fs.mu.Unlock()
	clk.set(clk.now() + 150)
	s.Tick()
	ls = lines(t, drain(t, s, 1))
	if len(ls) != 1 {
		t.Fatalf("retry emit = %+v", ls)
	}
	ohlcvLine(t, ls[0], "100", "108", "100", "108", "1.5")
}
