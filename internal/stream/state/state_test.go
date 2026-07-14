// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

func data(channel, symbol string, origin stream.Origin, serverTime int64, source, payload string) stream.Data {
	return stream.Data{
		Channel: channel, Symbol: symbol, Origin: origin,
		ServerTime: serverTime, Source: source, Payload: json.RawMessage(payload),
	}
}

func newTestStore() *Store {
	return New(Config{}, func() int64 { return 1_000 })
}

// connectedPrivate / disconnectedPrivate drive the private-endpoint connection
// lifecycle the order-snapshot reconcile keys on: each private CONNECTED opens a
// new epoch.
func connectedPrivate(t int64) stream.Event {
	return notice(stream.Connected, stream.LevelInfo, t, map[string]any{"endpoint": "private"})
}

func disconnectedPrivate(t int64) stream.Event {
	return notice(stream.Disconnected, stream.LevelWarn, t, map[string]any{"endpoint": "private", "reason": "test"})
}

// --- ticker / orderbook ---

func tickerFrame(close string, ts int64) string {
	return fmt.Sprintf(`{"type":"ticker","timestamp":%d,"symbol":"btc_krw","data":{
		"open":"94679000","high":"111162000","low":"93861000","close":%q,
		"prevClose":"94679000","priceChange":"4348000","priceChangePercent":"4.59",
		"volume":"147.94385655","quoteVolume":"14311735005.18033",
		"bestAskPrice":"99027000","bestBidPrice":"99026000","lastTradedAt":%d}}`, ts, close, ts-100)
}

func TestTickerNewestServerTimeWins(t *testing.T) {
	s := newTestStore()
	s.Apply(data("ticker", "btc_krw", stream.OriginSnapshot, 100, "", tickerFrame("99027000", 100)))
	s.Apply(data("ticker", "btc_krw", stream.OriginRealtime, 200, "", tickerFrame("99100000", 200)))
	// An out-of-order older frame must not regress the state.
	s.Apply(data("ticker", "btc_krw", stream.OriginRealtime, 150, "", tickerFrame("98000000", 150)))

	tk, ok := s.Ticker("btc_krw")
	if !ok {
		t.Fatal("ticker missing")
	}
	if tk.Close != "99100000" || tk.ServerTime != 200 {
		t.Fatalf("got close=%s serverTime=%d, want the t=200 frame", tk.Close, tk.ServerTime)
	}
	if tk.PriceChangePercent != "4.59" || tk.BestBidPrice != "99026000" {
		t.Fatalf("decimal strings not preserved: %+v", tk)
	}
}

func TestOrderbookCompleteStateReplaced(t *testing.T) {
	s := newTestStore()
	book := `{"type":"orderbook","timestamp":50,"symbol":"btc_krw","snapshot":true,"data":{
		"timestamp":40,
		"asks":[{"price":"99131000","qty":"0.004"},{"price":"99132000","qty":"0.006"}],
		"bids":[{"price":"99120000","qty":"0.003"}]}}`
	s.Apply(data("orderbook", "btc_krw", stream.OriginSnapshot, 50, "", book))

	b, ok := s.Orderbook("btc_krw")
	if !ok || len(b.Asks) != 2 || len(b.Bids) != 1 || b.Timestamp != 40 {
		t.Fatalf("book not materialized: %+v", b)
	}
	if b.Asks[0].Price != "99131000" || b.Bids[0].Qty != "0.003" {
		t.Fatalf("levels wrong: %+v", b)
	}

	// Older frame ignored, newer frame replaces the whole book.
	s.Apply(data("orderbook", "btc_krw", stream.OriginRealtime, 30, "", `{"data":{"timestamp":20,"asks":[],"bids":[]}}`))
	if b, _ := s.Orderbook("btc_krw"); len(b.Asks) != 2 {
		t.Fatal("older frame must not replace the book")
	}
	s.Apply(data("orderbook", "btc_krw", stream.OriginRealtime, 60, "", `{"data":{"timestamp":55,"asks":[{"price":"1","qty":"2"}],"bids":[]}}`))
	if b, _ := s.Orderbook("btc_krw"); len(b.Asks) != 1 || len(b.Bids) != 0 {
		t.Fatalf("newer frame must replace the whole book: %+v", b)
	}
}

// --- public trades ---

func wsTrades(ids ...int64) string {
	rows := ""
	for i, id := range ids {
		if i > 0 {
			rows += ","
		}
		rows += fmt.Sprintf(`{"timestamp":%d,"price":"98909000","qty":"0.001","isBuyerTaker":true,"tradeId":%d}`, 1000+id, id)
	}
	return fmt.Sprintf(`{"type":"trade","timestamp":5,"symbol":"btc_krw","data":[%s]}`, rows)
}

func TestTradesDedupedSortedNewestFirst(t *testing.T) {
	s := newTestStore()
	s.Apply(data("trade", "btc_krw", stream.OriginSnapshot, 5, "", wsTrades(10, 11, 12)))
	// A REST gap patch (bare array) with one overlap and one older row.
	s.Apply(data("trade", "btc_krw", stream.OriginBackfill, 6, "/v2/trades",
		`[{"timestamp":1008,"price":"1","qty":"1","isBuyerTaker":false,"tradeId":8},
		  {"timestamp":1012,"price":"1","qty":"1","isBuyerTaker":false,"tradeId":12}]`))
	// A duplicate-bearing live frame (the stream layer re-emits verbatim on
	// rebuild failure; the store must stay idempotent).
	s.Apply(data("trade", "btc_krw", stream.OriginRealtime, 7, "", wsTrades(12, 13)))

	got := s.Trades("btc_krw")
	want := []int64{13, 12, 11, 10, 8}
	if len(got) != len(want) {
		t.Fatalf("got %d trades, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].TradeID != id {
			t.Fatalf("position %d: got tradeId %d, want %d", i, got[i].TradeID, id)
		}
	}
}

func TestTradesCapEvictsOldest(t *testing.T) {
	s := New(Config{TradeCap: 3}, nil)
	s.Apply(data("trade", "btc_krw", stream.OriginRealtime, 5, "", wsTrades(1, 2, 3, 4, 5)))
	got := s.Trades("btc_krw")
	if len(got) != 3 || got[0].TradeID != 5 || got[2].TradeID != 3 {
		t.Fatalf("cap not applied newest-kept: %+v", got)
	}
}

func TestTradesSinceReturnsNewerTailOldestFirst(t *testing.T) {
	s := newTestStore()
	s.Apply(data("trade", "btc_krw", stream.OriginRealtime, 5, "", wsTrades(10, 11, 12, 13)))

	// The tail strictly newer than a mid id, ascending (fold order).
	got := s.TradesSince("btc_krw", 11)
	want := []int64{12, 13}
	if len(got) != len(want) {
		t.Fatalf("since(11): got %d trades, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].TradeID != id {
			t.Fatalf("since(11) position %d: got %d, want %d", i, got[i].TradeID, id)
		}
	}
	// Nothing newer than the newest id: the poll-every-batch no-op path allocates nil.
	if got := s.TradesSince("btc_krw", 13); got != nil {
		t.Fatalf("since(newest) = %+v, want nil", got)
	}
	// An unknown symbol is nil, never a panic.
	if got := s.TradesSince("eth_krw", 0); got != nil {
		t.Fatalf("since(unknown symbol) = %+v, want nil", got)
	}
}

// --- last-trade tick direction ---

type tp struct {
	id    int64
	price string
}

// tradeFrame builds a public-trade event from (tradeId, price) pairs, in either
// the WS frame shape or the /v2/trades backfill (bare array) shape.
func tradeFrame(origin stream.Origin, serverTime int64, ps ...tp) stream.Event {
	rows := ""
	for i, p := range ps {
		if i > 0 {
			rows += ","
		}
		rows += fmt.Sprintf(`{"timestamp":%d,"price":%q,"qty":"0.001","isBuyerTaker":true,"tradeId":%d}`, 1000+p.id, p.price, p.id)
	}
	if origin == stream.OriginBackfill {
		return data("trade", "btc_krw", origin, serverTime, "/v2/trades", "["+rows+"]")
	}
	return data("trade", "btc_krw", origin, serverTime, "", fmt.Sprintf(`{"type":"trade","timestamp":%d,"symbol":"btc_krw","data":[%s]}`, serverTime, rows))
}

func wantTick(t *testing.T, s *Store, want Direction) {
	t.Helper()
	if got := s.LastTick("btc_krw"); got != want {
		t.Fatalf("LastTick = %v, want %v", got, want)
	}
}

// TestLastTickCarryForward walks the price path 5,5,5,6,6,6,4,4,4,5,4: equal
// prices carry the prior direction, and only a differing price flips it. The
// 4,4,4,5 stretch is the case a naive last-two-trades compare gets wrong.
func TestLastTickCarryForward(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{1, "5"}))
	wantTick(t, s, Neutral) // first price only seeds the reference
	s.Apply(tradeFrame(stream.OriginRealtime, 2, tp{2, "5"}, tp{3, "5"}))
	wantTick(t, s, Neutral) // no change yet
	s.Apply(tradeFrame(stream.OriginRealtime, 3, tp{4, "6"}))
	wantTick(t, s, Up)
	s.Apply(tradeFrame(stream.OriginRealtime, 4, tp{5, "6"}, tp{6, "6"}))
	wantTick(t, s, Up) // equals carry up
	s.Apply(tradeFrame(stream.OriginRealtime, 5, tp{7, "4"}))
	wantTick(t, s, Down)
	s.Apply(tradeFrame(stream.OriginRealtime, 6, tp{8, "4"}, tp{9, "4"}))
	wantTick(t, s, Down) // equals carry down
	s.Apply(tradeFrame(stream.OriginRealtime, 7, tp{10, "5"}))
	wantTick(t, s, Up)
	s.Apply(tradeFrame(stream.OriginRealtime, 8, tp{11, "4"}))
	wantTick(t, s, Down) // 4 < 5: a downtick, not neutral
}

// TestLastTickEqualScales treats numerically-equal prices as equal (carry), not
// a tick, regardless of string form.
func TestLastTickEqualScales(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{1, "5"}, tp{2, "6"})) // Up
	s.Apply(tradeFrame(stream.OriginRealtime, 2, tp{3, "6.0"}))           // == 6, carry
	wantTick(t, s, Up)
}

func TestLastTickNeutralBeforeReady(t *testing.T) {
	s := newTestStore()
	wantTick(t, s, Neutral) // no trades applied: not ready
}

// TestLastTickForwardFoldEqualRun exercises the O(1) forward-fold path across
// separate live frames, including an equal-price run that must carry the prior
// direction rather than reset to Neutral.
func TestLastTickForwardFoldEqualRun(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{1, "5"}, tp{2, "6"})) // Up
	wantTick(t, s, Up)
	s.Apply(tradeFrame(stream.OriginRealtime, 2, tp{3, "6"})) // == 6: carry Up
	wantTick(t, s, Up)
	s.Apply(tradeFrame(stream.OriginRealtime, 3, tp{4, "6"}, tp{5, "4"})) // 6 then 4: Down
	wantTick(t, s, Down)
}

// TestLastTickIgnoresOlderBackfill: a backfill of older (lower-id) trades must
// not move the direction set by the newest trade.
func TestLastTickIgnoresOlderBackfill(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{10, "5"}, tp{11, "6"})) // Up
	wantTick(t, s, Up)
	s.Apply(tradeFrame(stream.OriginBackfill, 2, tp{1, "100"}, tp{2, "1"})) // older ids
	wantTick(t, s, Up)
}

// TestLastTickBackfillCorrectsPredecessor: a gap patch that delivers the tip's
// true immediate predecessor (a lower id than the tip, but higher than the
// earlier reference) must flip the direction — the tip is a downtick from it.
func TestLastTickBackfillCorrectsPredecessor(t *testing.T) {
	s := newTestStore()
	// Snapshot: earlier trade 10 @ 5, tip trade 20 @ 6 — looks like an uptick.
	s.Apply(tradeFrame(stream.OriginSnapshot, 1, tp{10, "5"}, tp{20, "6"}))
	wantTick(t, s, Up)
	// Gap patch fills in trade 19 @ 7, the tip's real predecessor: 6 < 7.
	s.Apply(tradeFrame(stream.OriginBackfill, 2, tp{19, "7"}))
	wantTick(t, s, Down)
}

func TestLastTickNeutralWhilePublicTradeGapPending(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{3, "90"}, tp{4, "100"}))
	wantTick(t, s, Up)

	s.Apply(notice(stream.BackfillStart, stream.LevelInfo, 2, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(4), "beforeTradeId": int64(10),
	}))
	s.Apply(tradeFrame(stream.OriginSnapshot, 3, tp{10, "95"}))
	wantTick(t, s, Neutral) // 95 vs stale 100 is not a trustworthy downtick.

	s.Apply(tradeFrame(stream.OriginBackfill, 4, tp{9, "80"}))
	wantTick(t, s, Neutral) // still hidden until the gap patch is known complete.
	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 5, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "complete": true,
	}))
	wantTick(t, s, Up) // rescan sees the true predecessor: 95 > 80.
}

func TestLastTickStaysNeutralAfterUnrecoveredPublicTradeGap(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{3, "90"}, tp{4, "100"}))
	wantTick(t, s, Up)

	s.Apply(notice(stream.BackfillStart, stream.LevelInfo, 2, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(4), "beforeTradeId": int64(10),
	}))
	s.Apply(tradeFrame(stream.OriginSnapshot, 3, tp{10, "95"}))
	s.Apply(notice(stream.DataGap, stream.LevelWarn, 4, map[string]any{
		"channel": stream.ChannelTrade, "symbol": "btc_krw",
	}))
	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 5, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "complete": false,
	}))
	wantTick(t, s, Neutral)

	s.Apply(tradeFrame(stream.OriginSnapshot, 6, tp{10, "95"}, tp{11, "105"}))
	wantTick(t, s, Up) // a later clean snapshot re-establishes a trustworthy window.
}

// TestLastTickRecoversAfterFailedPublicTradeGapPatch: an async gap-patch REST
// failure emits its BACKFILL_FAILED (endpoint=public, reason=gap) AFTER the gap
// snapshot, while its BACKFILL_START is still pending. The pending count already
// shielded that snapshot, so the failure must NOT arm the snapshot latch — a
// single later clean snapshot has to restore the tick (not require two).
func TestLastTickRecoversAfterFailedPublicTradeGapPatch(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{3, "90"}, tp{4, "100"}))
	wantTick(t, s, Up)

	s.Apply(notice(stream.BackfillStart, stream.LevelInfo, 2, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(4), "beforeTradeId": int64(10),
	}))
	s.Apply(tradeFrame(stream.OriginSnapshot, 3, tp{10, "95"}))
	// The patch fails: BACKFILL_FAILED arrives after the snapshot, gap still pending.
	s.Apply(notice(stream.BackfillFailed, stream.LevelError, 4, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(4), "beforeTradeId": int64(10),
		"source": "/v2/trades", "error": "boom",
	}))
	s.Apply(notice(stream.DataGap, stream.LevelWarn, 5, map[string]any{
		"channel": stream.ChannelTrade, "symbol": "btc_krw",
	}))
	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 6, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "complete": false,
	}))
	wantTick(t, s, Neutral)

	s.Apply(tradeFrame(stream.OriginSnapshot, 7, tp{20, "95"}, tp{21, "105"}))
	wantTick(t, s, Up) // one clean snapshot restores it — the latch was not armed.
}

// applyUnrecoveredGap leaves the store with tradeGapLost set and a poisoned tick
// direction (95 computed against the pre-gap 100 across a hole at 5..9).
func applyUnrecoveredGap(t *testing.T, s *Store) {
	t.Helper()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{3, "90"}, tp{4, "100"}))
	wantTick(t, s, Up)
	s.Apply(notice(stream.BackfillStart, stream.LevelInfo, 2, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(4), "beforeTradeId": int64(10),
	}))
	s.Apply(tradeFrame(stream.OriginSnapshot, 3, tp{10, "95"}))
	s.Apply(notice(stream.DataGap, stream.LevelWarn, 4, map[string]any{
		"channel": stream.ChannelTrade, "symbol": "btc_krw",
	}))
	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 5, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "complete": false,
	}))
	wantTick(t, s, Neutral)
}

// TestLastTickEmptySnapshotDoesNotClearLostGap: an empty (all-duplicate)
// resubscribe snapshot carries no new trade, so it must not clear the lost latch
// and expose the stale across-gap direction. Only a snapshot with real rows can.
func TestLastTickEmptySnapshotDoesNotClearLostGap(t *testing.T) {
	s := newTestStore()
	applyUnrecoveredGap(t, s)

	s.Apply(tradeFrame(stream.OriginSnapshot, 6)) // empty snapshot: no rows
	wantTick(t, s, Neutral)                       // still latched — no new evidence

	s.Apply(tradeFrame(stream.OriginSnapshot, 7, tp{20, "95"}, tp{21, "105"}))
	wantTick(t, s, Up) // a snapshot with real rows re-establishes the window
}

// TestLastTickCleanSnapshotDoesNotCarryPreGapDirection: clearing a lost gap must
// rebase the tick on the new snapshot's own window, not fold the snapshot onto
// the poisoned pre-gap tip. A same-price single-row snapshot therefore reads
// Neutral (not the carried-forward across-gap direction) until a fresh,
// contiguous trade establishes a trustworthy tick.
func TestLastTickCleanSnapshotDoesNotCarryPreGapDirection(t *testing.T) {
	s := newTestStore()
	applyUnrecoveredGap(t, s) // poisoned tick dir = Down (95 vs pre-gap 100)

	s.Apply(tradeFrame(stream.OriginSnapshot, 6, tp{11, "95"}))
	wantTick(t, s, Neutral) // must not report the carried-forward Down

	s.Apply(tradeFrame(stream.OriginRealtime, 7, tp{12, "105"}))
	wantTick(t, s, Up) // 105 > 95, both post-gap and contiguous: trustworthy
}

func TestLastTickNeutralAfterPublicTradeBackfillDisabled(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{3, "90"}, tp{4, "100"}))
	wantTick(t, s, Up)

	s.Apply(notice(stream.BackfillDisabled, stream.LevelWarn, 2, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(4), "beforeTradeId": int64(10),
	}))
	s.Apply(notice(stream.DataGap, stream.LevelWarn, 3, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "backfill": "disabled",
	}))
	s.Apply(tradeFrame(stream.OriginSnapshot, 4, tp{10, "95"}))
	wantTick(t, s, Neutral)

	s.Apply(tradeFrame(stream.OriginSnapshot, 5, tp{10, "95"}, tp{11, "105"}))
	wantTick(t, s, Up)
}

func TestLastTickTracksOverlappingPublicTradeGaps(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{3, "90"}, tp{4, "100"}))
	wantTick(t, s, Up)

	gap1Start := notice(stream.BackfillStart, stream.LevelInfo, 2, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(4), "beforeTradeId": int64(10),
	})
	gap2Start := notice(stream.BackfillStart, stream.LevelInfo, 3, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "afterTradeId": int64(10), "beforeTradeId": int64(20),
	})
	s.Apply(gap1Start)
	s.Apply(tradeFrame(stream.OriginSnapshot, 4, tp{10, "95"}))
	s.Apply(gap2Start)
	s.Apply(tradeFrame(stream.OriginSnapshot, 5, tp{20, "110"}))
	wantTick(t, s, Neutral)

	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 6, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "complete": true,
	}))
	wantTick(t, s, Neutral) // one gap is still pending.

	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 7, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "complete": true,
	}))
	wantTick(t, s, Up)
}

func TestLastTickCompleteGapDoesNotClearPriorLostGap(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{3, "90"}, tp{4, "100"}))
	s.Apply(notice(stream.DataGap, stream.LevelWarn, 2, map[string]any{
		"channel": stream.ChannelTrade, "symbol": "btc_krw",
	}))
	wantTick(t, s, Neutral)

	s.Apply(notice(stream.BackfillStart, stream.LevelInfo, 3, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw",
	}))
	s.Apply(tradeFrame(stream.OriginSnapshot, 4, tp{10, "95"}))
	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 5, map[string]any{
		"endpoint": "public", "reason": "gap", "channel": stream.ChannelTrade,
		"symbol": "btc_krw", "complete": true,
	}))
	wantTick(t, s, Neutral)

	s.Apply(tradeFrame(stream.OriginSnapshot, 6, tp{10, "95"}, tp{11, "105"}))
	wantTick(t, s, Up)
}

// TestLastTickResetOnReconnect: a public drop makes the direction Neutral, and
// the resubscribe recomputes it from the retained ring plus the new snapshot.
func TestLastTickResetOnReconnect(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{1, "5"}, tp{2, "6"})) // Up
	wantTick(t, s, Up)
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 2, map[string]any{"endpoint": "public"}))
	wantTick(t, s, Neutral)                                               // trades no longer current
	s.Apply(tradeFrame(stream.OriginSnapshot, 3, tp{2, "6"}, tp{3, "7"})) // resubscribe (overlap + new)
	wantTick(t, s, Up)                                                    // recomputed over 5,6,7
}

func TestLastTickResetOnMarketSwitch(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginRealtime, 1, tp{1, "5"}, tp{2, "6"})) // Up
	wantTick(t, s, Up)
	s.MarkMarketStale("btc_krw")
	wantTick(t, s, Neutral)
}

// --- orders ---

func wsMyOrder(serverTime int64, rows string) stream.Data {
	payload := fmt.Sprintf(`{"symbol":"btc_krw","timestamp":%d,"channelType":"myOrder","order":{"accountSeq":1,"orders":[%s]}}`, serverTime, rows)
	return data("myOrder", "btc_krw", stream.OriginRealtime, serverTime, "", payload)
}

const liveOrderRow = `{"orderId":111,"status":"unfilled","side":"buy","orderType":"limit",
	"timeInForce":"gtc","price":"99017000","qty":"0.9","filledQty":"0",
	"filledAmt":"0","createdAt":900,"clientOrderId":"cid-1"}`

func TestOrderStatusNormalizedAndMerged(t *testing.T) {
	s := newTestStore()
	s.Apply(wsMyOrder(100, liveOrderRow))

	o, ok := s.Order(111)
	if !ok {
		t.Fatal("order missing")
	}
	if o.Status != "open" {
		t.Fatalf("WS 'unfilled' must normalize to 'open', got %q", o.Status)
	}
	if o.Symbol != "btc_krw" || o.ClientOrderID != "cid-1" || o.Price != "99017000" {
		t.Fatalf("fields wrong: %+v", o)
	}
	if o.AccountSeq != 1 {
		t.Fatalf("wrapper accountSeq must be stamped on the order, got %d", o.AccountSeq)
	}

	// A partial fill update: sparse row (no clientOrderId) must not blank the
	// previously known fields.
	s.Apply(wsMyOrder(200, `{"orderId":111,"status":"partiallyFilled","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0.5","filledAmt":"49508500","avgPrice":"99017000",
		"createdAt":900,"lastFilledAt":190}`))
	o, _ = s.Order(111)
	if o.Status != "partiallyFilled" || o.FilledQty != "0.5" {
		t.Fatalf("update not applied: %+v", o)
	}
	if o.ClientOrderID != "cid-1" {
		t.Fatalf("sparse row blanked merged fields: %+v", o)
	}
	if len(s.OpenOrders("btc_krw")) != 1 {
		t.Fatal("partiallyFilled order must be listed open")
	}
}

func TestOrderTerminalLatch(t *testing.T) {
	s := newTestStore()
	s.Apply(wsMyOrder(100, liveOrderRow))
	s.Apply(wsMyOrder(300, `{"orderId":111,"status":"filled","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0.9","filledAmt":"89115300","createdAt":900}`))

	// A stale openOrders snapshot (fetched before the fill, delivered after)
	// still lists the order as open — it must NOT reopen it.
	snap := data("myOrder", "btc_krw", stream.OriginBackfill, 250, "/v2/openOrders",
		`[{"orderId":111,"status":"open","side":"buy","orderType":"limit","price":"99017000","qty":"0.9","filledQty":"0","filledAmt":"0","createdAt":900}]`)
	s.Apply(snap)

	o, _ := s.Order(111)
	if o.Status != "filled" {
		t.Fatalf("terminal status was overwritten: %q", o.Status)
	}
	if len(s.OpenOrders("")) != 0 {
		t.Fatal("filled order must not be open")
	}
}

// TestStatusClassification pins the open/terminal rule: open is the fixed set
// {pending, open, partiallyFilled}; terminal is its complement, so a status the
// API may add in the future classifies terminal with no code change. The empty
// string is "status not yet known" — neither.
func TestStatusClassification(t *testing.T) {
	cases := []struct {
		status         string
		open, terminal bool
	}{
		{"pending", true, false},
		{"open", true, false},
		{"partiallyFilled", true, false},
		{"filled", false, true},
		{"canceled", false, true},
		{"partiallyFilledCanceled", false, true},
		{"expired", false, true},
		{StatusClosedUnknown, false, true},
		// A status this build has never seen: terminal by complement, not open.
		{"rejected", false, true},
		{"someFutureTerminalState", false, true},
		// "unfilled" is the raw WS spelling of open; it is normalized to "open"
		// before classification, so the un-normalized form is not itself open.
		{"unfilled", false, true},
		// Empty string: unknown, neither open nor terminal.
		{"", false, false},
	}
	for _, c := range cases {
		o := Order{Status: c.status}
		if got := o.Open(); got != c.open {
			t.Errorf("Order{Status:%q}.Open() = %v, want %v", c.status, got, c.open)
		}
		if got := o.Terminal(); got != c.terminal {
			t.Errorf("Order{Status:%q}.Terminal() = %v, want %v", c.status, got, c.terminal)
		}
	}
}

func TestOpenOrdersSnapshotReconcilesDisappearance(t *testing.T) {
	s := newTestStore()
	// Two live orders in the first connection (epoch 1).
	s.Apply(connectedPrivate(10))
	s.Apply(wsMyOrder(100, liveOrderRow))
	s.Apply(wsMyOrder(110, `{"orderId":222,"status":"unfilled","side":"sell","orderType":"limit",
		"price":"99500000","qty":"0.1","filledQty":"0","filledAmt":"0","createdAt":905}`))

	// Drop, then reconnect (epoch 2): the backfill snapshot only contains order
	// 222 — order 111 (known from the earlier epoch) closed during the gap, real
	// status unknown yet.
	s.Apply(disconnectedPrivate(400))
	s.Apply(connectedPrivate(450))
	snap := data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders",
		`[{"orderId":222,"status":"open","side":"sell","orderType":"limit","price":"99500000","qty":"0.1","filledQty":"0","filledAmt":"0","createdAt":905}]`)
	s.Apply(snap)

	if o, _ := s.Order(111); o.Status != StatusClosedUnknown {
		t.Fatalf("missing order should be %q, got %q", StatusClosedUnknown, o.Status)
	}
	open := s.OpenOrders("btc_krw")
	if len(open) != 1 || open[0].OrderID != 222 {
		t.Fatalf("open set wrong: %+v", open)
	}

	// The allOrders gap rows then deliver the truth — it replaces the
	// placeholder even at an equal/older ServerTime.
	hist := data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/allOrders",
		`[{"orderId":111,"symbol":"btc_krw","status":"canceled","side":"buy","orderType":"limit","timeInForce":"gtc","price":"99017000","qty":"0.9","filledQty":"0","filledAmt":"0","createdAt":900}]`)
	s.Apply(hist)
	if o, _ := s.Order(111); o.Status != "canceled" {
		t.Fatalf("history row must replace the placeholder, got %q", o.Status)
	}
}

// TestDebugLogsReconcileDecisions wires a Debug logger onto Config.Log and
// asserts the correctness-driving reconcile mechanics are logged: the private
// epoch bump on each connect and a gap-closed order. The Store is confined to one
// goroutine, so a plain buffer is fine.
func TestDebugLogsReconcileDecisions(t *testing.T) {
	var buf bytes.Buffer
	s := New(Config{Log: logging.New(&buf, slog.LevelDebug)}, func() int64 { return 1_000 })

	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(wsMyOrder(100, liveOrderRow))
	s.Apply(disconnectedPrivate(400))
	s.Apply(connectedPrivate(450)) // epoch 2
	// Reconnect snapshot omits order 111 (known from epoch 1) → gap-closed.
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders", `[]`))

	out := buf.String()
	if !strings.Contains(out, "state epoch bump") || !strings.Contains(out, "epoch=2") {
		t.Fatalf("expected epoch-bump log; got:\n%s", out)
	}
	if !strings.Contains(out, "state order gap-closed") || !strings.Contains(out, "orderId=111") {
		t.Fatalf("expected gap-closed log; got:\n%s", out)
	}
}

// TestOpenOrdersSnapshotNewestWins: a /v2/openOrders snapshot older than the
// one already applied for a symbol is dropped wholesale, so a late/stale
// on-demand fetch cannot revive an order a fresher snapshot correctly closed
// (the placeholder-revive path in upsertOrder would otherwise reopen it).
func TestOpenOrdersSnapshotNewestWins(t *testing.T) {
	s := newTestStore()
	// Order 777 known open from a live frame in the first connection (epoch 1).
	s.Apply(connectedPrivate(10))
	s.Apply(wsMyOrder(100, `{"orderId":777,"status":"unfilled","side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","filledAmt":"0","createdAt":90}`))
	// Reconnect (epoch 2). The fresh snapshot at t=500 omits it → closed.
	s.Apply(disconnectedPrivate(400))
	s.Apply(connectedPrivate(450))
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders", `[]`))
	if len(s.OpenOrders("btc_krw")) != 0 {
		t.Fatalf("fresh snapshot must close the absent order: %+v", s.OpenOrders("btc_krw"))
	}
	// A STALE snapshot at t=300 (same epoch, slower on-demand fetch) still lists
	// 777 as open; the ServerTime newest-wins guard drops it before it revives.
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 300, "/v2/openOrders",
		`[{"orderId":777,"status":"open","side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","filledAmt":"0","createdAt":90}]`))
	if len(s.OpenOrders("btc_krw")) != 0 {
		t.Fatalf("stale snapshot revived a closed order: %+v", s.OpenOrders("btc_krw"))
	}
}

// TestOpenOrdersReadyTracking: a symbol becomes "ready" once its open-order
// snapshot is applied, an unrelated symbol stays not-ready, and a PRIVATE
// disconnect clears readiness (so the UI shows loading until the reconnect
// backfill re-snapshots) while a PUBLIC disconnect leaves it intact.
func TestOpenOrdersReadyTracking(t *testing.T) {
	s := newTestStore()
	if s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("symbol must not be ready before any snapshot")
	}
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 100, "/v2/openOrders",
		`[{"orderId":1,"status":"open","symbol":"btc_krw","createdAt":1}]`))
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("symbol must be ready after its snapshot")
	}
	if s.OpenOrdersReady(1, "eth_krw") {
		t.Fatal("an unrelated symbol must not be ready")
	}
	// A public disconnect must not affect open-order readiness.
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 200, map[string]any{"endpoint": "public"}))
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("a public disconnect must not clear order readiness")
	}
	// A private disconnect makes snapshots stale again.
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 300, map[string]any{"endpoint": "private"}))
	if s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("a private disconnect must clear order readiness")
	}
}

// TestPublicFreshnessTracking: ticker/orderbook/trade become "ready" once a
// frame arrives, an unrelated symbol stays not-ready, a PUBLIC disconnect clears
// them all (a PRIVATE one does not), and MarkMarketStale clears only the
// per-symbol book/trade latches (ticker is account-wide, not market-switched).
func TestPublicFreshnessTracking(t *testing.T) {
	s := newTestStore()
	for _, ready := range []bool{s.OrderbookReady("btc_krw"), s.TradesReady("btc_krw"), s.TickerReady("btc_krw")} {
		if ready {
			t.Fatal("public data must not be ready before any frame")
		}
	}
	s.Apply(data("ticker", "btc_krw", stream.OriginSnapshot, 1, "", tickerFrame("1", 1)))
	s.Apply(data("orderbook", "btc_krw", stream.OriginSnapshot, 1, "", `{"data":{"timestamp":1,"asks":[{"price":"1","qty":"2"}],"bids":[]}}`))
	s.Apply(data("trade", "btc_krw", stream.OriginSnapshot, 1, "", wsTrades(10)))
	if !s.OrderbookReady("btc_krw") || !s.TradesReady("btc_krw") || !s.TickerReady("btc_krw") {
		t.Fatal("a frame must mark its channel ready for the symbol")
	}
	if s.OrderbookReady("eth_krw") || s.TradesReady("eth_krw") || s.TickerReady("eth_krw") {
		t.Fatal("an unrelated symbol must not be ready")
	}
	// A private disconnect must not touch public freshness.
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 100, map[string]any{"endpoint": "private"}))
	if !s.OrderbookReady("btc_krw") || !s.TradesReady("btc_krw") || !s.TickerReady("btc_krw") {
		t.Fatal("a private disconnect must not clear public readiness")
	}
	// A public disconnect makes all public panes stale.
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 200, map[string]any{"endpoint": "public"}))
	if s.OrderbookReady("btc_krw") || s.TradesReady("btc_krw") || s.TickerReady("btc_krw") {
		t.Fatal("a public disconnect must clear book/trade/ticker readiness")
	}
	// A market switch invalidates the new symbol's retained book/trades, but not
	// its ticker (the ticker channel is not re-pointed on a switch).
	s.Apply(data("orderbook", "btc_krw", stream.OriginSnapshot, 3, "", `{"data":{"timestamp":3,"asks":[],"bids":[]}}`))
	s.Apply(data("trade", "btc_krw", stream.OriginRealtime, 3, "", wsTrades(11)))
	s.Apply(data("ticker", "btc_krw", stream.OriginRealtime, 3, "", tickerFrame("2", 3)))
	s.MarkMarketStale("btc_krw")
	if s.OrderbookReady("btc_krw") || s.TradesReady("btc_krw") {
		t.Fatal("MarkMarketStale must clear the symbol's book/trade readiness")
	}
	if !s.TickerReady("btc_krw") {
		t.Fatal("MarkMarketStale must not clear ticker readiness")
	}
}

// TestBalancesFreshnessTracking: balances become ready on the snapshot and a
// PRIVATE disconnect clears them (a PUBLIC one does not).
func TestBalancesFreshnessTracking(t *testing.T) {
	s := newTestStore()
	if s.BalancesReady(1) {
		t.Fatal("balances must not be ready before a snapshot")
	}
	s.Apply(data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1","available":"1","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	if !s.BalancesReady(1) {
		t.Fatal("balances must be ready after the snapshot")
	}
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 200, map[string]any{"endpoint": "public"}))
	if !s.BalancesReady(1) {
		t.Fatal("a public disconnect must not clear balance readiness")
	}
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 300, map[string]any{"endpoint": "private"}))
	if s.BalancesReady(1) {
		t.Fatal("a private disconnect must clear balance readiness")
	}
}

// TestReadinessIsPerAccount: balance/open-order readiness latches per
// sub-account — one account's snapshot does not make another read as ready.
// The store never rolls this up ("all subscribed accounts ready" is the
// consumer's call, since it is the only layer that knows the subscribed set —
// see the botapi state.ready rollup); the store only answers per account.
func TestReadinessIsPerAccount(t *testing.T) {
	s := newTestStore()
	one := 1

	bal1 := data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1","available":"1","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	bal1.AccountSeq = &one
	s.Apply(bal1)
	if !s.BalancesReady(1) {
		t.Fatal("account 1 balances must be ready after its snapshot")
	}
	if s.BalancesReady(2) {
		t.Fatal("account 2 must NOT be ready — it had no snapshot")
	}

	oo1 := data("myOrder", "btc_krw", stream.OriginBackfill, 200, "/v2/openOrders",
		`[{"orderId":1,"status":"open","symbol":"btc_krw","createdAt":1}]`)
	oo1.AccountSeq = &one
	s.Apply(oo1)
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("account 1 openOrders must be ready after its snapshot")
	}
	if s.OpenOrdersReady(2, "btc_krw") {
		t.Fatal("account 2 openOrders must NOT be ready — it had no snapshot")
	}
}

func TestClosedPlaceholderRevivedByLiveFrame(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10))
	s.Apply(wsMyOrder(100, liveOrderRow))
	// A truncated /v2/openOrders snapshot on reconnect omits the order, so it is
	// marked closed — but it is actually still open on the exchange.
	s.Apply(disconnectedPrivate(400))
	s.Apply(connectedPrivate(450))
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders", `[]`))
	if o, _ := s.Order(111); o.Status != StatusClosedUnknown {
		t.Fatalf("setup: want placeholder, got %q", o.Status)
	}

	// A later live myOrder frame proves it is still partiallyFilled — the
	// placeholder must NOT latch it closed (the synthetic "closed" status is
	// exempt from the terminal-absorbing rule for exactly this reason).
	s.Apply(wsMyOrder(600, `{"orderId":111,"status":"partiallyFilled","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0.3","filledAmt":"29705100","createdAt":900}`))
	o, _ := s.Order(111)
	if o.Status != "partiallyFilled" {
		t.Fatalf("a live frame must revive a falsely-closed order, got %q", o.Status)
	}
	if len(s.OpenOrders("btc_krw")) != 1 {
		t.Fatal("revived order must be listed open")
	}

	// A real terminal status still latches afterward.
	s.Apply(wsMyOrder(700, `{"orderId":111,"status":"filled","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0.9","filledAmt":"89115300","createdAt":900}`))
	s.Apply(wsMyOrder(800, `{"orderId":111,"status":"open","side":"buy","orderType":"limit","price":"99017000","qty":"0.9","filledQty":"0","filledAmt":"0","createdAt":900}`))
	if o, _ := s.Order(111); o.Status != "filled" {
		t.Fatalf("a real terminal status must still latch, got %q", o.Status)
	}
}

// TestOrderOrdersByProgressNotTimestamp: a more-advanced order state must win
// even when it arrives in a frame whose (transport) timestamp is OLDER than a
// later, less-advanced frame. The merge keys on the order's intrinsic progress
// (status + cumulative filledQty), never the WebSocket send-time.
func TestOrderOrdersByProgressNotTimestamp(t *testing.T) {
	s := newTestStore()
	// A partial fill is observed first, stamped with a high frame time.
	s.Apply(wsMyOrder(900, `{"orderId":111,"status":"partiallyFilled","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0.5","filledAmt":"49508500","createdAt":900}`))
	// A frame describing the EARLIER "open" state then arrives late, with a
	// LOWER frame time — it must not regress the order back to open.
	s.Apply(wsMyOrder(800, `{"orderId":111,"status":"open","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0","filledAmt":"0","createdAt":900}`))
	o, _ := s.Order(111)
	if o.Status != "partiallyFilled" || o.FilledQty != "0.5" {
		t.Fatalf("stale lower-progress frame regressed the order: %+v", o)
	}

	// A further fill (same status, larger cumulative qty) advances it, again
	// independent of frame time.
	s.Apply(wsMyOrder(810, `{"orderId":111,"status":"partiallyFilled","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0.7","filledAmt":"69311900","createdAt":900}`))
	if o, _ := s.Order(111); o.FilledQty != "0.7" {
		t.Fatalf("a larger cumulative fill must win: %+v", o)
	}
}

// TestOrderStatuslessRowAdvancesFillWithoutBlankingStatus: a row that carries a
// larger cumulative filledQty but no status must apply the fill and leave the
// known status intact (the empty-status path through upsertOrder + supersedes
// rule 5).
func TestOrderStatuslessRowAdvancesFillWithoutBlankingStatus(t *testing.T) {
	s := newTestStore()
	s.Apply(wsMyOrder(100, `{"orderId":111,"status":"partiallyFilled","side":"buy","orderType":"limit",
		"price":"99017000","qty":"0.9","filledQty":"0.3","filledAmt":"29705100","createdAt":900}`))
	// A status-less row with a grown fill (no "status" key at all).
	s.Apply(wsMyOrder(110, `{"orderId":111,"filledQty":"0.6","filledAmt":"59410200"}`))
	o, _ := s.Order(111)
	if o.Status != "partiallyFilled" {
		t.Fatalf("status-less row blanked the status: %q", o.Status)
	}
	if o.FilledQty != "0.6" {
		t.Fatalf("status-less row must advance the fill: %+v", o)
	}
}

func TestOpenOrdersSnapshotSparesNewerOrders(t *testing.T) {
	s := newTestStore()
	// An order placed during the reconnect fetch — same connection (epoch) as
	// the snapshot — must be spared even though the empty snapshot omits it.
	s.Apply(connectedPrivate(10))
	snap := data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders", `[]`)
	s.Apply(wsMyOrder(600, liveOrderRow))
	s.Apply(snap)

	if o, _ := s.Order(111); o.Status != "open" {
		t.Fatalf("an order from the current connection must be left alone, got %q", o.Status)
	}
}

// TestOpenOrdersSnapshotEpochScopesToConnection: a snapshot only closes orders
// known from an EARLIER connection. An order seen in the CURRENT connection is
// never closed by this connection's snapshot — even an empty one (e.g. the
// per-user openOrders cache briefly stale) — because a real close would arrive
// as a lossless live terminal frame instead. Here the snapshot's ServerTime
// (400) is HIGHER than the live frame's (300), so the old transport-timestamp
// gate would have false-closed the order; epoch scoping spares it.
func TestOpenOrdersSnapshotEpochScopesToConnection(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10))         // epoch 1
	s.Apply(wsMyOrder(100, liveOrderRow)) // 111 open, epoch 1
	s.Apply(disconnectedPrivate(200))
	s.Apply(connectedPrivate(250))        // epoch 2
	s.Apply(wsMyOrder(300, liveOrderRow)) // 111 re-asserted open, epoch 2
	// A same-connection snapshot at a LATER ServerTime momentarily omits 111.
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 400, "/v2/openOrders", `[]`))
	if o, _ := s.Order(111); o.Status != "open" {
		t.Fatalf("a current-connection order must not be closed by its own snapshot, got %q", o.Status)
	}
}

func TestOrdersAcrossSymbolsFilteredAndSorted(t *testing.T) {
	s := newTestStore()
	s.Apply(wsMyOrder(100, liveOrderRow))
	eth := `{"orderId":333,"status":"unfilled","side":"buy","orderType":"limit","price":"5000000","qty":"1","filledQty":"0","filledAmt":"0","createdAt":950}`
	s.Apply(data("myOrder", "eth_krw", stream.OriginRealtime, 120,
		"", fmt.Sprintf(`{"symbol":"eth_krw","timestamp":120,"channelType":"myOrder","order":{"orders":[%s]}}`, eth)))

	all := s.OpenOrders("")
	if len(all) != 2 || all[0].OrderID != 333 || all[1].OrderID != 111 {
		t.Fatalf("want newest-created first across symbols: %+v", all)
	}
	if got := s.OpenOrders("eth_krw"); len(got) != 1 || got[0].OrderID != 333 {
		t.Fatalf("symbol filter wrong: %+v", got)
	}
}

// TestOrderAccountSeqStamped: a live myOrder frame stamps its wrapper accountSeq
// on each order; a frame without the wrapper field defaults to the main account
// (1); a /v2/openOrders backfill stamps the seq carried on the Data event (the
// bare-array rows don't echo it).
func TestOrderAccountSeqStamped(t *testing.T) {
	s := newTestStore()
	// A live frame for sub-account 2.
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 100, "",
		`{"symbol":"btc_krw","channelType":"myOrder","order":{"accountSeq":2,"orders":[
			{"orderId":111,"status":"unfilled","side":"buy","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":90}]}}`))
	if o, _ := s.Order(111); o.AccountSeq != 2 {
		t.Fatalf("live wrapper accountSeq not stamped, got %d", o.AccountSeq)
	}

	// A live frame with no wrapper accountSeq (server omits it unless the
	// subscription asked) defaults to the main account.
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 110, "",
		`{"symbol":"btc_krw","channelType":"myOrder","order":{"orders":[
			{"orderId":222,"status":"unfilled","side":"sell","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":95}]}}`))
	if o, _ := s.Order(222); o.AccountSeq != 1 {
		t.Fatalf("untagged frame must default to account 1, got %d", o.AccountSeq)
	}

	// A /v2/openOrders backfill carrying accountSeq=3 on the Data event stamps it.
	seq3 := 3
	snap := data("myOrder", "btc_krw", stream.OriginBackfill, 200, "/v2/openOrders",
		`[{"orderId":333,"status":"open","side":"buy","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":190}]`)
	snap.AccountSeq = &seq3
	s.Apply(snap)
	if o, _ := s.Order(333); o.AccountSeq != 3 {
		t.Fatalf("backfill Data.AccountSeq not stamped, got %d", o.AccountSeq)
	}
}

// TestOpenSnapshotReconcileScopedByAccount: a /v2/openOrders snapshot is
// authoritative only for its own sub-account. Account 1's snapshot must not close
// account 2's open order for the same symbol, and the per-account freshness guard
// must not let one account's snapshot ServerTime drop another's.
func TestOpenSnapshotReconcileScopedByAccount(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	// Two sub-accounts each holding an open btc_krw order in epoch 1.
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 100, "",
		`{"symbol":"btc_krw","channelType":"myOrder","order":{"accountSeq":1,"orders":[
			{"orderId":111,"status":"unfilled","side":"buy","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":90}]}}`))
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 100, "",
		`{"symbol":"btc_krw","channelType":"myOrder","order":{"accountSeq":2,"orders":[
			{"orderId":222,"status":"unfilled","side":"sell","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":95}]}}`))

	// Reconnect (epoch 2). Account 1's snapshot is empty (111 closed during the
	// gap); it must NOT touch account 2's 222.
	s.Apply(disconnectedPrivate(400))
	s.Apply(connectedPrivate(450)) // epoch 2
	one := 1
	snap1 := data("myOrder", "btc_krw", stream.OriginBackfill, 600, "/v2/openOrders", `[]`)
	snap1.AccountSeq = &one
	s.Apply(snap1)
	if o, _ := s.Order(111); o.Status != StatusClosedUnknown {
		t.Fatalf("account 1's gap-closed order should be %q, got %q", StatusClosedUnknown, o.Status)
	}
	if o, _ := s.Order(222); o.Status != "open" {
		t.Fatalf("account 1's snapshot must NOT close account 2's order, got %q", o.Status)
	}

	// Account 2's snapshot at a LOWER ServerTime than account 1's must still
	// apply (independent per-account freshness guard) and keep 222 open.
	two := 2
	snap2 := data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders",
		`[{"orderId":222,"status":"open","side":"sell","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":95}]`)
	snap2.AccountSeq = &two
	s.Apply(snap2)
	if o, _ := s.Order(222); o.Status != "open" {
		t.Fatalf("account 2's own snapshot (lower ServerTime) must not be dropped, got %q", o.Status)
	}
}

// TestOpenSnapshotStaleGenSkipped: a /v2/openOrders snapshot from a SUPERSEDED
// connection (a slow REST stamped with the old generation that lands after a newer
// reconnect) is skipped whole — it must not mark openOrders ready, must not run the
// disappearance reconcile (false-closing a current order absent from its stale
// view), and must not revive a placeholder. The current-generation snapshot then
// reconciles authoritatively.
func TestOpenSnapshotStaleGenSkipped(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10))         // epoch 1
	s.Apply(wsMyOrder(100, liveOrderRow)) // 111 open, epoch 1
	s.Apply(disconnectedPrivate(200))
	s.Apply(connectedPrivate(250)) // epoch 2
	// A new order placed on connection 2.
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 300, "",
		`{"symbol":"btc_krw","channelType":"myOrder","order":{"accountSeq":1,"orders":[
			{"orderId":222,"status":"unfilled","side":"sell","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":295}]}}`))

	// A connection-1 (gen-1) snapshot finally lands — it only knows 111, not 222.
	stale := data("myOrder", "btc_krw", stream.OriginBackfill, 400, "/v2/openOrders",
		`[{"orderId":111,"status":"open","side":"buy","orderType":"limit","price":"99017000","qty":"0.9","filledQty":"0","filledAmt":"0","createdAt":900}]`)
	stale.PrivateEpoch = 1
	s.Apply(stale)

	if s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("a stale (gen-1) snapshot must NOT mark openOrders ready in epoch 2")
	}
	if o, _ := s.Order(222); o.Status != "open" {
		t.Fatalf("stale snapshot (missing 222) must NOT false-close the current order, got %q", o.Status)
	}

	// The current (gen-2) snapshot omits 111 (it closed during the gap): now the
	// reconcile runs and closes 111, spares 222, and marks ready.
	fresh := data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders",
		`[{"orderId":222,"status":"open","side":"sell","orderType":"limit","price":"1","qty":"1","filledQty":"0","filledAmt":"0","createdAt":295}]`)
	fresh.PrivateEpoch = 2
	s.Apply(fresh)
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("the current-generation snapshot must mark openOrders ready")
	}
	if o, _ := s.Order(111); o.Status != StatusClosedUnknown {
		t.Fatalf("current snapshot must close the gap-closed order, got %q", o.Status)
	}
	if o, _ := s.Order(222); o.Status != "open" {
		t.Fatalf("current order present in the snapshot must stay open, got %q", o.Status)
	}
}

// TestOpenSnapshotStaleGenDoesNotRevivePlaceholder: a placeholder-closed order
// must not be revived by a stale-generation snapshot that still lists it as open
// (the stale snapshot is skipped); a current-generation live frame still can.
func TestOpenSnapshotStaleGenDoesNotRevivePlaceholder(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10))         // epoch 1
	s.Apply(wsMyOrder(100, liveOrderRow)) // 111 open, epoch 1
	s.Apply(disconnectedPrivate(200))
	s.Apply(connectedPrivate(250)) // epoch 2
	// Current snapshot omits 111 -> placeholder-closed.
	cur := data("myOrder", "btc_krw", stream.OriginBackfill, 300, "/v2/openOrders", `[]`)
	cur.PrivateEpoch = 2
	s.Apply(cur)
	if o, _ := s.Order(111); o.Status != StatusClosedUnknown {
		t.Fatalf("setup: 111 should be placeholder-closed, got %q", o.Status)
	}

	// A stale gen-1 snapshot still listing 111 as open must NOT revive it.
	stale := data("myOrder", "btc_krw", stream.OriginBackfill, 400, "/v2/openOrders",
		`[{"orderId":111,"status":"open","side":"buy","orderType":"limit","price":"99017000","qty":"0.9","filledQty":"0","filledAmt":"0","createdAt":900}]`)
	stale.PrivateEpoch = 1
	s.Apply(stale)
	if o, _ := s.Order(111); o.Status != StatusClosedUnknown {
		t.Fatalf("stale snapshot must NOT revive the placeholder, got %q", o.Status)
	}
}

// TestBalancesPerAccountNoCollision: two sub-accounts holding the same currency
// are kept apart (keyed by account+currency), not collapsed onto one row.
func TestBalancesPerAccountNoCollision(t *testing.T) {
	s := newTestStore()
	one, two := 1, 2
	row := `[{"currency":"krw","balance":%q,"available":%q,"tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`
	b1 := data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance", fmt.Sprintf(row, "100", "100"))
	b1.AccountSeq = &one
	b2 := data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance", fmt.Sprintf(row, "200", "200"))
	b2.AccountSeq = &two
	s.Apply(b1)
	s.Apply(b2)

	bals := s.Balances()
	if len(bals) != 2 {
		t.Fatalf("two sub-accounts' krw must not collide: %+v", bals)
	}
	if bals[0].AccountSeq != 1 || bals[0].Balance != "100" || bals[1].AccountSeq != 2 || bals[1].Balance != "200" {
		t.Fatalf("balances wrong/out of order: %+v", bals)
	}
}

// TestFillAccountSeqStamped: a live myTrade frame stamps its wrapper accountSeq
// on each fill; a backfill stamps the Data event's seq.
func TestFillAccountSeqStamped(t *testing.T) {
	s := newTestStore()
	s.Apply(data("myTrade", "btc_krw", stream.OriginRealtime, 1000, "",
		`{"symbol":"btc_krw","channelType":"myTrade","trade":{"accountSeq":2,"trades":[
			{"tradeId":52,"orderId":1,"side":"buy","price":"1","qty":"1","fee":"0","feeCurrency":"krw","filledAt":990,"isTaker":true}]}}`))
	fills := s.Fills()
	if len(fills) != 1 || fills[0].AccountSeq != 2 {
		t.Fatalf("live fill accountSeq not stamped: %+v", fills)
	}

	seq5 := 5
	rest := data("myTrade", "btc_krw", stream.OriginBackfill, 1100, "/v2/myTrades",
		`[{"symbol":"btc_krw","tradeId":53,"orderId":2,"side":"sell","price":"1","qty":"1","tradedAt":1000,"isTaker":false,"feeCurrency":"krw","feeQty":"0"}]`)
	rest.AccountSeq = &seq5
	s.Apply(rest)
	for _, f := range s.Fills() {
		if f.TradeID == 53 && f.AccountSeq != 5 {
			t.Fatalf("backfill fill accountSeq not stamped: %+v", f)
		}
	}
}

// --- fills ---

func TestFillsNormalizedAndDeduped(t *testing.T) {
	s := newTestStore()
	// Live WS fill: fee/filledAt spelling, symbol on the envelope.
	live := `{"symbol":"btc_krw","timestamp":1000,"channelType":"myTrade","trade":{"accountSeq":1,"trades":[
		{"tradeId":52,"orderId":382312,"side":"buy","price":"5000","qty":"10","fee":"50","feeCurrency":"krw","filledAt":990,"isTaker":true}]}}`
	s.Apply(data("myTrade", "btc_krw", stream.OriginRealtime, 1000, "", live))

	// REST backfill of the same fill plus an older one: feeQty/tradedAt
	// spelling, symbol in the row.
	rest := `[{"symbol":"btc_krw","tradeId":52,"orderId":382312,"side":"buy","price":"5000","qty":"10","amt":"50000","tradedAt":990,"isTaker":true,"feeCurrency":"krw","feeQty":"50"},
	          {"symbol":"btc_krw","tradeId":51,"orderId":382311,"side":"sell","price":"4990","qty":"2","amt":"9980","tradedAt":900,"isTaker":false,"feeCurrency":"krw","feeQty":"10"}]`
	s.Apply(data("myTrade", "btc_krw", stream.OriginBackfill, 1100, "/v2/myTrades", rest))

	fills := s.Fills()
	if len(fills) != 2 {
		t.Fatalf("dedupe by tradeId failed: %+v", fills)
	}
	if fills[0].TradeID != 52 || fills[1].TradeID != 51 {
		t.Fatalf("want newest first: %+v", fills)
	}
	if fills[0].Fee != "50" || fills[0].Time != 990 || fills[0].Symbol != "btc_krw" {
		t.Fatalf("WS row not normalized: %+v", fills[0])
	}
	if fills[1].Fee != "10" || fills[1].Time != 900 {
		t.Fatalf("REST row not normalized (feeQty/tradedAt): %+v", fills[1])
	}
}

func TestFillsCap(t *testing.T) {
	s := New(Config{FillCap: 2}, nil)
	for i := int64(1); i <= 4; i++ {
		row := fmt.Sprintf(`{"symbol":"btc_krw","timestamp":%d,"channelType":"myTrade","trade":{"trades":[
			{"tradeId":%d,"orderId":1,"side":"buy","price":"1","qty":"1","fee":"0","feeCurrency":"krw","filledAt":%d,"isTaker":true}]}}`, i*10, i, i*10)
		s.Apply(data("myTrade", "btc_krw", stream.OriginRealtime, i*10, "", row))
	}
	fills := s.Fills()
	if len(fills) != 2 || fills[0].TradeID != 4 || fills[1].TradeID != 3 {
		t.Fatalf("cap must keep the newest fills: %+v", fills)
	}
}

// --- balances ---

// TestBalancesLiveBeatsSnapshotSameConnection: within one connection a live
// myAsset delta supersedes the reconnect REST snapshot for its currency, and a
// snapshot can never clobber a value a live frame already set — the clock-free
// rule (origin precedence within an epoch), so the (now ignored) frame
// ServerTimes are irrelevant.
func TestBalancesLiveBeatsSnapshotSameConnection(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	// REST snapshot (full, no updatedAt) — the reconnect baseline.
	snapshot := `[{"currency":"krw","balance":"1000000","available":"900000","tradeInUse":"100000","withdrawalInUse":"0","avgPrice":"0"},
	           {"currency":"btc","balance":"10","available":"7","tradeInUse":"2","withdrawalInUse":"1","avgPrice":"50000"}]`
	s.Apply(data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance", snapshot))

	// A live partial delta for btc only must win over the snapshot.
	delta := `{"timestamp":200,"channelType":"myAsset","asset":{"accountSeq":1,"assets":[
		{"currency":"btc","balance":"11","available":"8","tradeInUse":"2","withdrawalInUse":"1","avgPrice":"50000","updatedAt":190}]}}`
	s.Apply(data("myAsset", "", stream.OriginRealtime, 200, "", delta))

	// A second REST snapshot in the SAME connection (e.g. a stale in-flight fetch)
	// carries an older btc and must NOT clobber the live delta — origin precedence,
	// not the frame's ServerTime (here even HIGHER than the live frame's).
	stale := `[{"currency":"btc","balance":"9","available":"6","tradeInUse":"2","withdrawalInUse":"1","avgPrice":"50000"}]`
	s.Apply(data("myAsset", "", stream.OriginBackfill, 999, "/v2/balance", stale))

	bals := s.Balances()
	if len(bals) != 2 {
		t.Fatalf("want btc+krw: %+v", bals)
	}
	if bals[0].Currency != "btc" || bals[0].Balance != "11" || bals[0].UpdatedAt != 190 {
		t.Fatalf("btc must hold the live delta, not the snapshot: %+v", bals[0])
	}
	if bals[1].Currency != "krw" || bals[1].Available != "900000" {
		t.Fatalf("krw must hold the snapshot: %+v", bals[1])
	}
}

// TestBalancesReconnectRebaselines: a newer connection's REST snapshot (higher
// epoch) re-baselines a balance even though it is a snapshot superseding a prior
// connection's live value — the higher epoch wins, with no clock involved. A late
// frame from the OLD connection cannot regress it (live frames only flow on the
// current connection; balance backfill is serialized across reconnects), and a
// fresh live delta on the new connection still wins.
func TestBalancesReconnectRebaselines(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"btc","balance":"10","available":"10","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	s.Apply(data("myAsset", "", stream.OriginRealtime, 200, "",
		`{"channelType":"myAsset","asset":{"accountSeq":1,"assets":[{"currency":"btc","balance":"11","available":"11","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0","updatedAt":190}]}}`))
	if b := s.Balances(); b[0].Balance != "11" {
		t.Fatalf("setup: live should hold, got %+v", b[0])
	}

	// Reconnect: epoch 2. The reconnect snapshot re-baselines btc to 20.
	s.Apply(disconnectedPrivate(300))
	s.Apply(connectedPrivate(350)) // epoch 2
	s.Apply(data("myAsset", "", stream.OriginBackfill, 400, "/v2/balance",
		`[{"currency":"btc","balance":"20","available":"20","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	if b := s.Balances(); len(b) != 1 || b[0].Balance != "20" {
		t.Fatalf("newer connection's snapshot must re-baseline, got %+v", b)
	}
	// A fresh live delta on the new connection still wins.
	s.Apply(data("myAsset", "", stream.OriginRealtime, 500, "",
		`{"channelType":"myAsset","asset":{"accountSeq":1,"assets":[{"currency":"btc","balance":"21","available":"21","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0","updatedAt":490}]}}`))
	if b := s.Balances(); b[0].Balance != "21" {
		t.Fatalf("live delta on the new connection must win, got %+v", b[0])
	}
}

// TestBalancesStaleBackfillIgnoredAfterReconnect: a /v2/balance snapshot fetched
// in connection 1 whose slow REST lands AFTER connection 2 already bumped the
// epoch (carried as Data.PrivateEpoch=1) must neither clobber the current value
// nor mark balances ready — it is from a superseded connection. This is the hole
// the apply-time epoch left and the PrivateEpoch stamp closes.
func TestBalancesStaleBackfillIgnoredAfterReconnect(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(data("myAsset", "", stream.OriginRealtime, 100, "",
		`{"channelType":"myAsset","asset":{"accountSeq":1,"assets":[{"currency":"btc","balance":"11","available":"11","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0","updatedAt":90}]}}`))
	s.Apply(disconnectedPrivate(200)) // balReady cleared
	s.Apply(connectedPrivate(250))    // epoch 2

	// The connection-1 snapshot finally arrives, stamped with its own generation.
	stale := data("myAsset", "", stream.OriginBackfill, 300, "/v2/balance",
		`[{"currency":"btc","balance":"99","available":"99","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	stale.PrivateEpoch = 1
	s.Apply(stale)

	if s.BalancesReady(1) {
		t.Fatal("a stale (gen-1) snapshot must NOT mark balances ready in epoch 2")
	}
	if b := s.Balances(); len(b) != 1 || b[0].Balance != "11" {
		t.Fatalf("stale snapshot must not clobber the current value: %+v", b)
	}

	// The real connection-2 snapshot (current generation) re-baselines and marks ready.
	fresh := data("myAsset", "", stream.OriginBackfill, 400, "/v2/balance",
		`[{"currency":"btc","balance":"20","available":"20","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	fresh.PrivateEpoch = 2
	s.Apply(fresh)
	if !s.BalancesReady(1) {
		t.Fatal("the current-generation snapshot must mark balances ready")
	}
	if b := s.Balances(); b[0].Balance != "20" {
		t.Fatalf("current snapshot must re-baseline, got %+v", b[0])
	}
}

// TestBalancesSnapshotReconcilesDisappearance: a currency zeroed while
// disconnected can disappear from the reconnect /v2/balance snapshot (the
// session fetches it unfiltered; the server's row universe for zeroed holdings
// is not a guaranteed contract), so the store must drop the stale prior-epoch
// balance — its zeroing myAsset frame was lost with the old connection.
func TestBalancesSnapshotReconcilesDisappearance(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"1000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"10","available":"10","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	if len(s.Balances()) != 2 {
		t.Fatalf("setup: want btc+krw, got %+v", s.Balances())
	}

	// btc is fully sold/withdrawn during the disconnect; the epoch-2 snapshot
	// no longer carries it.
	s.Apply(disconnectedPrivate(200))
	s.Apply(connectedPrivate(250)) // epoch 2
	snap := data("myAsset", "", stream.OriginBackfill, 300, "/v2/balance",
		`[{"currency":"krw","balance":"1200000","available":"1200000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	snap.PrivateEpoch = 2
	s.Apply(snap)

	b := s.Balances()
	if len(b) != 1 || b[0].Currency != "krw" || b[0].Balance != "1200000" {
		t.Fatalf("the epoch-2 snapshot must drop the vanished btc balance: %+v", b)
	}
}

// TestBalancesSnapshotSparesSameConnectionLiveSet: within one connection the
// lossless live feed is the authority, so the connect snapshot must not sweep a
// balance a live delta already set in the SAME epoch — the snapshot may simply
// have been fetched before the currency's first delta.
func TestBalancesSnapshotSparesSameConnectionLiveSet(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(data("myAsset", "", stream.OriginRealtime, 100, "",
		`{"channelType":"myAsset","asset":{"accountSeq":1,"assets":[{"currency":"xrp","balance":"5","available":"5","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0","updatedAt":90}]}}`))
	// The connect snapshot, fetched before that delta, arrives without xrp.
	s.Apply(data("myAsset", "", stream.OriginBackfill, 150, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"1000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	b := s.Balances()
	if len(b) != 2 || b[1].Currency != "xrp" || b[1].Balance != "5" {
		t.Fatalf("a same-epoch live balance must survive the snapshot sweep: %+v", b)
	}
}

// TestBalancesStaleSnapshotDoesNotSweep: a snapshot from a superseded
// connection (a slow REST landing after a newer reconnect) has no disappearance
// authority — it must not drop balances the current connection re-baselined.
func TestBalancesStaleSnapshotDoesNotSweep(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10))     // epoch 1
	s.Apply(disconnectedPrivate(200)) //
	s.Apply(connectedPrivate(250))    // epoch 2
	fresh := data("myAsset", "", stream.OriginBackfill, 300, "/v2/balance",
		`[{"currency":"krw","balance":"1","available":"1","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"2","available":"2","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	fresh.PrivateEpoch = 2
	s.Apply(fresh)

	stale := data("myAsset", "", stream.OriginBackfill, 350, "/v2/balance",
		`[{"currency":"krw","balance":"9","available":"9","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	stale.PrivateEpoch = 1
	s.Apply(stale)

	b := s.Balances()
	if len(b) != 2 || b[0].Currency != "btc" || b[0].Balance != "2" {
		t.Fatalf("a superseded connection's snapshot must sweep nothing: %+v", b)
	}
}

// TestOpenOrdersLateSnapshotWhileDownDoesNotMarkReady: a snapshot fetch can
// outlive its connection (REST is transport-independent) and land after the
// DISCONNECTED cleared readiness but before the next CONNECTED bumps the
// epoch — so it passes the generation guard. Its rows are valid point-in-time
// data and apply; it must NOT re-mark the symbol ready while the feed is down
// (the disconnect contract is "loading until the reconnect re-snapshots").
func TestOpenOrdersLateSnapshotWhileDownDoesNotMarkReady(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 100, "/v2/openOrders", `[]`))
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("setup: symbol must be ready after the epoch-1 snapshot")
	}

	s.Apply(disconnectedPrivate(200))
	// The late epoch-1 snapshot, emitted by a backfill goroutine that outlived
	// the connection (a real backfill always stamps its connection's gen).
	late := data("myOrder", "btc_krw", stream.OriginBackfill, 300, "/v2/openOrders",
		`[{"orderId":9,"status":"open","side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","filledAmt":"0","createdAt":90}]`)
	late.PrivateEpoch = 1
	s.Apply(late)
	if s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("a late snapshot must not mark the symbol ready while the private feed is down")
	}
	if len(s.OpenOrders("btc_krw")) != 1 {
		t.Fatalf("the late snapshot's rows are valid and must still apply: %+v", s.OpenOrders("btc_krw"))
	}

	// The reconnect's own snapshot restores readiness.
	s.Apply(connectedPrivate(400)) // epoch 2
	snap := data("myOrder", "btc_krw", stream.OriginBackfill, 450, "/v2/openOrders", `[]`)
	snap.PrivateEpoch = 2
	s.Apply(snap)
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("the reconnect snapshot must mark the symbol ready again")
	}
}

// TestBalancesLateSnapshotWhileDownDoesNotMarkReady: the balance analogue —
// a /v2/balance snapshot landing between DISCONNECTED and the next CONNECTED
// re-baselines values (valid point-in-time data) but must not present
// balances as ready while the feed is down.
func TestBalancesLateSnapshotWhileDownDoesNotMarkReady(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"1000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	if !s.BalancesReady(1) {
		t.Fatal("setup: balances must be ready after the epoch-1 snapshot")
	}

	s.Apply(disconnectedPrivate(200))
	late := data("myAsset", "", stream.OriginBackfill, 300, "/v2/balance",
		`[{"currency":"krw","balance":"1100000","available":"1100000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	late.PrivateEpoch = 1
	s.Apply(late)
	if s.BalancesReady(1) {
		t.Fatal("a late snapshot must not mark balances ready while the private feed is down")
	}
	if b := s.Balances(); len(b) != 1 || b[0].Balance != "1100000" {
		t.Fatalf("the late snapshot's values are valid and must still re-baseline: %+v", b)
	}

	s.Apply(connectedPrivate(400)) // epoch 2
	snap := data("myAsset", "", stream.OriginBackfill, 450, "/v2/balance",
		`[{"currency":"krw","balance":"1200000","available":"1200000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	snap.PrivateEpoch = 2
	s.Apply(snap)
	if !s.BalancesReady(1) {
		t.Fatal("the reconnect snapshot must mark balances ready again")
	}
}

// TestBalancesLiveDeltaDoesNotLatchReady: after a reconnect a live myAsset
// delta can arrive BEFORE the /v2/balance snapshot. It updates its own
// currencies but proves nothing about the rest of the set — prior-epoch
// balances zeroed during the disconnect are still un-swept — so it must not
// mark balances ready; only the current-generation snapshot (which sweeps)
// may. Otherwise a delayed or failed snapshot leaves stale holdings presented
// behind a true BalancesReady until the next reconnect.
func TestBalancesLiveDeltaDoesNotLatchReady(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	s.Apply(data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"1000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"10","available":"10","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	if !s.BalancesReady(1) {
		t.Fatal("setup: balances must be ready after the epoch-1 snapshot")
	}

	// btc is fully sold during the disconnect (its zeroing frame lost with the
	// old connection); on reconnect a live krw delta beats the snapshot.
	s.Apply(disconnectedPrivate(200))
	s.Apply(connectedPrivate(250)) // epoch 2
	s.Apply(data("myAsset", "", stream.OriginRealtime, 300, "",
		`{"channelType":"myAsset","asset":{"accountSeq":1,"assets":[{"currency":"krw","balance":"1500000","available":"1500000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0","updatedAt":290}]}}`))
	if s.BalancesReady(1) {
		t.Fatal("a live delta must not mark balances ready while the stale btc row is un-swept")
	}

	// The epoch-2 snapshot lands: sweeps btc, keeps the newer live krw, latches.
	snap := data("myAsset", "", stream.OriginBackfill, 350, "/v2/balance",
		`[{"currency":"krw","balance":"1400000","available":"1400000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`)
	snap.PrivateEpoch = 2
	s.Apply(snap)
	if !s.BalancesReady(1) {
		t.Fatal("the epoch-2 snapshot must mark balances ready")
	}
	if b := s.Balances(); len(b) != 1 || b[0].Currency != "krw" || b[0].Balance != "1500000" {
		t.Fatalf("snapshot must sweep btc and spare the same-epoch live krw: %+v", b)
	}
}

// TestTradesReadyNotRelatchedByLateBackfill: a gap-patch payload can land AFTER
// the public feed dropped (the patch goroutine outlives its connection). It
// delivers history, not proof of a live feed, so it must not re-latch trade
// freshness — the pane would otherwise present retained trades as current
// during an outage. The next WS snapshot re-latches.
func TestTradesReadyNotRelatchedByLateBackfill(t *testing.T) {
	s := newTestStore()
	s.Apply(tradeFrame(stream.OriginSnapshot, 1, tp{1, "5"}))
	if !s.TradesReady("btc_krw") {
		t.Fatal("the snapshot must latch readiness")
	}
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 2, map[string]any{"endpoint": "public"}))
	if s.TradesReady("btc_krw") {
		t.Fatal("a public disconnect must clear readiness")
	}
	s.Apply(tradeFrame(stream.OriginBackfill, 3, tp{2, "6"}))
	if s.TradesReady("btc_krw") {
		t.Fatal("a late backfill payload must not re-latch readiness while the feed is down")
	}
	s.Apply(tradeFrame(stream.OriginSnapshot, 4, tp{3, "7"}))
	if !s.TradesReady("btc_krw") {
		t.Fatal("the resubscribe snapshot must re-latch readiness")
	}
}

// TestOpenOrdersSnapshotAppliesAfterBackwardClockJump: ServerTime (the
// backfill's fetch-start server-clock estimate) can move BACKWARD across a
// reconnect when a resync corrects the offset down. The new epoch's snapshot
// must still apply — the newest-wins watermark is per-epoch, not global.
func TestOpenOrdersSnapshotAppliesAfterBackwardClockJump(t *testing.T) {
	s := newTestStore()
	s.Apply(connectedPrivate(10)) // epoch 1
	snap1 := data("myOrder", "btc_krw", stream.OriginBackfill, 5_000, "/v2/openOrders",
		`[{"orderId":1,"status":"open","side":"buy"}]`)
	snap1.PrivateEpoch = 1
	s.Apply(snap1)
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("setup: the epoch-1 snapshot must mark the symbol ready")
	}

	s.Apply(disconnectedPrivate(20))
	s.Apply(connectedPrivate(30)) // epoch 2

	// Fetched under a corrected (lower) server-clock estimate: ServerTime sits
	// BELOW the epoch-1 watermark.
	snap2 := data("myOrder", "btc_krw", stream.OriginBackfill, 1_000, "/v2/openOrders", `[]`)
	snap2.PrivateEpoch = 2
	s.Apply(snap2)
	if !s.OpenOrdersReady(1, "btc_krw") {
		t.Fatal("the new epoch's snapshot must apply despite a lower ServerTime")
	}
	// And its disappearance inference must run: order 1 (epoch 1) closed offline.
	o, ok := s.Order(1)
	if !ok || o.Status != StatusClosedUnknown {
		t.Fatalf("the epoch-1 order absent from the epoch-2 snapshot must be closed-unknown: %+v", o)
	}
}

// --- health / notices ---

func notice(code stream.NoticeCode, level stream.Level, t int64, details map[string]any) stream.Notice {
	return stream.Notice{Code: code, Level: level, Message: string(code), Details: details, Time: t}
}

func TestHealthTracksConnectionsBackfillGaps(t *testing.T) {
	s := newTestStore()
	s.Apply(notice(stream.Connected, stream.LevelInfo, 10, map[string]any{"endpoint": "public"}))
	s.Apply(notice(stream.Connected, stream.LevelInfo, 11, map[string]any{"endpoint": "private"}))
	s.Apply(notice(stream.BackfillStart, stream.LevelInfo, 12, map[string]any{"endpoint": "private"}))

	h := s.Health()
	if !h.Public.Up || !h.Private.Up || !h.Backfilling {
		t.Fatalf("health wrong after connect: %+v", h)
	}

	s.Apply(notice(stream.BackfillDone, stream.LevelInfo, 13, map[string]any{"endpoint": "private"}))
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 20, map[string]any{"endpoint": "private", "reason": "read failed"}))
	s.Apply(notice(stream.DataGap, stream.LevelWarn, 21, map[string]any{"channel": "trade"}))

	h = s.Health()
	if h.Backfilling {
		t.Fatal("backfill must be done")
	}
	if h.Private.Up || h.Private.LastError != "read failed" || !h.Public.Up {
		t.Fatalf("disconnect not tracked: %+v", h)
	}
	if h.GapCount != 1 {
		t.Fatalf("gap count wrong: %+v", h)
	}

	got := s.Notices(2)
	if len(got) != 2 || got[0].Code != stream.DataGap || got[1].Code != stream.Disconnected {
		t.Fatalf("notices must be newest first: %+v", got)
	}
}

// Degradation warnings set a standing flag that its recovery notice clears, so
// the UI shows "currently degraded" rather than a warning that never lifts.
func TestHealthDegradationFlagsClearOnRecovery(t *testing.T) {
	s := newTestStore()
	s.Apply(notice(stream.Connected, stream.LevelInfo, 10, map[string]any{"endpoint": "public"}))

	s.Apply(notice(stream.DataDelayed, stream.LevelWarn, 20, map[string]any{"endpoint": "public", "delayMs": int64(20_000)}))
	s.Apply(notice(stream.ConnectionUnreliable, stream.LevelWarn, 21, map[string]any{"endpoint": "public", "rttMs": int64(3_000)}))
	if h := s.Health(); !h.Public.Delayed || !h.Public.Unreliable {
		t.Fatalf("warnings should set the flags: %+v", h.Public)
	}

	s.Apply(notice(stream.DataCurrent, stream.LevelInfo, 30, map[string]any{"endpoint": "public", "delayMs": int64(100)}))
	s.Apply(notice(stream.ConnectionStable, stream.LevelInfo, 31, map[string]any{"endpoint": "public"}))
	if h := s.Health(); h.Public.Delayed || h.Public.Unreliable {
		t.Fatalf("recovery notices should clear the flags: %+v", h.Public)
	}
}

// A reconnect clears Delayed (no current evidence of lag) but preserves
// Unreliable, which the stream owns until recent drops age out and it emits
// CONNECTION_STABLE — so a single reconnect cannot hide an ongoing flap.
func TestHealthReconnectPreservesUnreliableClearsDelayed(t *testing.T) {
	s := newTestStore()
	s.Apply(notice(stream.Connected, stream.LevelInfo, 10, map[string]any{"endpoint": "private"}))
	s.Apply(notice(stream.DataDelayed, stream.LevelWarn, 20, map[string]any{"endpoint": "private"}))
	s.Apply(notice(stream.ConnectionUnreliable, stream.LevelWarn, 21, map[string]any{"endpoint": "private"}))

	// A drop then reconnect.
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 30, map[string]any{"endpoint": "private", "reason": "read failed"}))
	if h := s.Health(); !h.Private.Unreliable {
		t.Fatalf("disconnect should preserve Unreliable: %+v", h.Private)
	}
	s.Apply(notice(stream.Connected, stream.LevelInfo, 40, map[string]any{"endpoint": "private"}))

	h := s.Health()
	if !h.Private.Up {
		t.Fatalf("should be reconnected: %+v", h.Private)
	}
	if h.Private.Delayed {
		t.Fatalf("reconnect should clear Delayed: %+v", h.Private)
	}
	if !h.Private.Unreliable {
		t.Fatalf("reconnect should preserve Unreliable: %+v", h.Private)
	}
}

func TestNoticeCap(t *testing.T) {
	s := New(Config{NoticeCap: 3}, nil)
	for i := int64(0); i < 5; i++ {
		s.Apply(notice(stream.Keepalive, stream.LevelInfo, i, nil))
	}
	if got := s.Notices(0); len(got) != 3 || got[0].Time != 4 {
		t.Fatalf("notice cap wrong: %+v", got)
	}
}

func TestDataCountAndLastDataAt(t *testing.T) {
	now := int64(0)
	s := New(Config{}, func() int64 { return now })
	now = 42
	s.Apply(data("ticker", "btc_krw", stream.OriginRealtime, 1, "", tickerFrame("1", 1)))
	h := s.Health()
	if h.DataCount != 1 || h.LastDataAt != 42 {
		t.Fatalf("data stats wrong: %+v", h)
	}
}

// Malformed payloads must be ignored, never panic.
func TestMalformedPayloadsIgnored(t *testing.T) {
	s := newTestStore()
	for _, ch := range []string{"ticker", "orderbook", "trade", "myOrder", "myTrade", "myAsset"} {
		s.Apply(data(ch, "btc_krw", stream.OriginRealtime, 1, "", `not-json`))
		s.Apply(data(ch, "btc_krw", stream.OriginBackfill, 1, "/v2/x", `{"weird":true}`))
	}
	if _, ok := s.Ticker("btc_krw"); ok {
		t.Fatal("malformed ticker must not materialize")
	}
}

// TestPerAccountAccessors: OpenOrdersFor/BalancesFor/FillsFor return exactly one
// sub-account's view — the reads a per-account consumer (the TUI) renders from —
// while the unscoped accessors keep the cross-account union.
func TestPerAccountAccessors(t *testing.T) {
	s := newTestStore()
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 1000, "",
		`{"symbol":"btc_krw","channelType":"myOrder","order":{"accountSeq":1,"orders":[
			{"orderId":11,"side":"buy","price":"1","qty":"1","status":"open","createdAt":900}]}}`))
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 1001, "",
		`{"symbol":"btc_krw","channelType":"myOrder","order":{"accountSeq":2,"orders":[
			{"orderId":22,"side":"sell","price":"1","qty":"1","status":"open","createdAt":901}]}}`))

	one, two := 1, 2
	row := `[{"currency":"krw","balance":%q,"available":%q,"tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`
	b1 := data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance", fmt.Sprintf(row, "100", "100"))
	b1.AccountSeq = &one
	b2 := data("myAsset", "", stream.OriginBackfill, 100, "/v2/balance", fmt.Sprintf(row, "200", "200"))
	b2.AccountSeq = &two
	s.Apply(b1)
	s.Apply(b2)

	s.Apply(data("myTrade", "btc_krw", stream.OriginRealtime, 1000, "",
		`{"symbol":"btc_krw","channelType":"myTrade","trade":{"accountSeq":1,"trades":[
			{"tradeId":51,"orderId":11,"side":"buy","price":"1","qty":"1","fee":"0","feeCurrency":"krw","filledAt":990,"isTaker":true}]}}`))
	s.Apply(data("myTrade", "btc_krw", stream.OriginRealtime, 1001, "",
		`{"symbol":"btc_krw","channelType":"myTrade","trade":{"accountSeq":2,"trades":[
			{"tradeId":52,"orderId":22,"side":"sell","price":"1","qty":"1","fee":"0","feeCurrency":"krw","filledAt":991,"isTaker":true}]}}`))

	if open := s.OpenOrdersFor(1, ""); len(open) != 1 || open[0].OrderID != 11 {
		t.Fatalf("OpenOrdersFor(1) must see only account 1's order: %+v", open)
	}
	if open := s.OpenOrdersFor(2, "btc_krw"); len(open) != 1 || open[0].OrderID != 22 {
		t.Fatalf("OpenOrdersFor(2, btc_krw) must see only account 2's order: %+v", open)
	}
	if open := s.OpenOrdersFor(1, "eth_krw"); len(open) != 0 {
		t.Fatalf("OpenOrdersFor symbol filter broken: %+v", open)
	}
	if all := s.OpenOrders(""); len(all) != 2 {
		t.Fatalf("unscoped OpenOrders must keep the union: %+v", all)
	}

	if bals := s.BalancesFor(1); len(bals) != 1 || bals[0].Balance != "100" {
		t.Fatalf("BalancesFor(1) wrong: %+v", bals)
	}
	if bals := s.BalancesFor(2); len(bals) != 1 || bals[0].Balance != "200" {
		t.Fatalf("BalancesFor(2) wrong: %+v", bals)
	}

	if fills := s.FillsFor(1); len(fills) != 1 || fills[0].TradeID != 51 {
		t.Fatalf("FillsFor(1) wrong: %+v", fills)
	}
	if fills := s.FillsFor(2); len(fills) != 1 || fills[0].TradeID != 52 {
		t.Fatalf("FillsFor(2) wrong: %+v", fills)
	}
}

func TestClosedOrdersFor(t *testing.T) {
	now := int64(1_000)
	s := New(Config{}, func() int64 { return now })
	row := func(id int64, status string) string {
		return fmt.Sprintf(`{"orderId":%d,"status":%q,"side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","createdAt":%d}`, id, status, id*10)
	}
	s.Apply(wsMyOrder(100, row(1, "open")))
	s.Apply(wsMyOrder(100, row(2, "open")))
	s.Apply(wsMyOrder(100, row(3, "open")))
	if got := s.ClosedOrdersFor(1, "btc_krw", 0); len(got) != 0 {
		t.Fatalf("open orders must not be listed closed: %+v", got)
	}

	// Close 3 first, then 1: the closed list orders by OBSERVED close, not by
	// creation or id.
	now = 2_000
	s.Apply(wsMyOrder(200, row(3, "expired")))
	now = 3_000
	s.Apply(wsMyOrder(300, row(1, "canceled")))

	got := s.ClosedOrdersFor(1, "btc_krw", 0)
	if len(got) != 2 || got[0].OrderID != 1 || got[1].OrderID != 3 {
		t.Fatalf("want [1 3] most recently observed closed first, got %+v", got)
	}
	if got[0].ClosedAt != 3_000 || got[1].ClosedAt != 2_000 {
		t.Fatalf("ClosedAt must stamp the first terminal observation: %+v", got)
	}

	// The stamp is once-only: a duplicate terminal frame must not move it
	// (terminal is absorbing, so the order's state cannot change either).
	now = 9_000
	s.Apply(wsMyOrder(400, row(3, "expired")))
	if o, _ := s.Order(3); o.ClosedAt != 2_000 {
		t.Fatalf("a repeated terminal frame must not restamp ClosedAt: %d", o.ClosedAt)
	}

	// Cap keeps the newest; the symbol filter scopes.
	if got := s.ClosedOrdersFor(1, "btc_krw", 1); len(got) != 1 || got[0].OrderID != 1 {
		t.Fatalf("limit must keep the most recently closed: %+v", got)
	}
	if got := s.ClosedOrdersFor(1, "eth_krw", 0); len(got) != 0 {
		t.Fatalf("symbol filter leaked: %+v", got)
	}
}

// TestTerminalOrdersCapPrunes: the store bounds retained terminal orders to
// terminalOrdersCap, evicting the oldest observed-close first and never
// touching an open order.
func TestTerminalOrdersCapPrunes(t *testing.T) {
	now := int64(0)
	s := New(Config{}, func() int64 { return now })
	row := func(id int64, status string) string {
		return fmt.Sprintf(`{"orderId":%d,"status":%q,"side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","createdAt":1}`, id, status)
	}
	// An open order that must survive every prune.
	s.Apply(wsMyOrder(1, row(1, "open")))

	// Close cap+50 orders, each at a later observed-close time so the eviction
	// order is unambiguous (oldest ClosedAt goes first).
	total := terminalOrdersCap + 50
	for i := 0; i < total; i++ {
		now = int64(i + 1)
		s.Apply(wsMyOrder(now, row(int64(1000+i), "expired")))
	}

	closed := s.ClosedOrdersFor(1, "btc_krw", 0) // uncapped read
	if len(closed) != terminalOrdersCap {
		t.Fatalf("retained terminal orders = %d, want the cap %d", len(closed), terminalOrdersCap)
	}
	// The newest cap orders survive; the 50 oldest were evicted.
	if want := int64(1000 + total - 1); closed[0].OrderID != want {
		t.Fatalf("newest close must be retained first, got %d want %d", closed[0].OrderID, want)
	}
	if want, got := int64(1000+50), closed[len(closed)-1].OrderID; got != want {
		t.Fatalf("oldest RETAINED close = %d, want %d (older ones evicted)", got, want)
	}
	if _, ok := s.Order(1000); ok {
		t.Fatal("the oldest terminal order must be pruned from the store")
	}
	if o, ok := s.Order(1); !ok || !o.Open() {
		t.Fatal("an open order must never be pruned")
	}
}

func TestClosedAtStampsGapClose(t *testing.T) {
	now := int64(1_000)
	s := New(Config{}, func() int64 { return now })
	s.Apply(connectedPrivate(10))
	s.Apply(wsMyOrder(100, liveOrderRow)) // order 111, open, epoch 1

	// Reconnect: the authoritative snapshot no longer contains 111 — the
	// disappearance inference closes it, and that observation is the ClosedAt.
	s.Apply(disconnectedPrivate(400))
	s.Apply(connectedPrivate(450))
	now = 5_000
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders", `[]`))

	o, _ := s.Order(111)
	if o.Status != StatusClosedUnknown {
		t.Fatalf("setup: want gap-close, got %q", o.Status)
	}
	if o.ClosedAt != 5_000 {
		t.Fatalf("gap-close must stamp ClosedAt at the reconcile: %d", o.ClosedAt)
	}
	if got := s.ClosedOrdersFor(1, "btc_krw", 0); len(got) != 1 || got[0].OrderID != 111 {
		t.Fatalf("a gap-closed order must be listed closed: %+v", got)
	}
}

func TestClosedAtRearmsOnRevival(t *testing.T) {
	now := int64(1_000)
	s := New(Config{}, func() int64 { return now })
	s.Apply(connectedPrivate(10))
	s.Apply(wsMyOrder(100, liveOrderRow)) // order 111, open, epoch 1
	s.Apply(disconnectedPrivate(400))
	s.Apply(connectedPrivate(450))
	now = 2_000
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 500, "/v2/openOrders", `[]`))
	if o, _ := s.Order(111); o.Status != StatusClosedUnknown || o.ClosedAt != 2_000 {
		t.Fatalf("setup: want a stamped gap-close, got %+v", o)
	}

	// A live frame revives the placeholder (it yields to any real status):
	// the false-close stamp must clear with it...
	now = 3_000
	s.Apply(wsMyOrder(600, `{"orderId":111,"status":"unfilled","side":"buy","orderType":"limit","price":"99017000","qty":"0.9","filledQty":"0","createdAt":900}`))
	if o, _ := s.Order(111); !o.Open() || o.ClosedAt != 0 {
		t.Fatalf("a revived order must be open with no close stamp: %+v", o)
	}

	// ...and re-arm on the real close, so the closed view orders by the true
	// observation rather than the superseded gap inference.
	now = 4_000
	s.Apply(wsMyOrder(700, `{"orderId":111,"status":"canceled","side":"buy","orderType":"limit","price":"99017000","qty":"0.9","filledQty":"0","createdAt":900}`))
	if o, _ := s.Order(111); o.ClosedAt != 4_000 {
		t.Fatalf("the real close must restamp ClosedAt: %+v", o)
	}
}
