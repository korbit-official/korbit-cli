// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"testing"

	"github.com/korbit-official/korbit-cli/internal/stream"
)

// TestSectionRevisionsBump pins the render-cache contract: each section's
// revision changes when (and only when) that section's data or freshness
// changes, so a consumer keying on a revision can't miss a change (stale frame)
// and an unrelated section's update doesn't needlessly invalidate it.
func TestSectionRevisionsBump(t *testing.T) {
	s := newTestStore()
	if s.TickerRev() != 0 || s.BookRev() != 0 || s.OrderRev() != 0 || s.HealthRev() != 0 {
		t.Fatalf("revisions should start at 0")
	}

	// A ticker frame bumps ticker + health, not book/order.
	book0, order0 := s.BookRev(), s.OrderRev()
	s.Apply(data("ticker", "btc_krw", stream.OriginRealtime, 200, "", tickerFrame("99100000", 200)))
	if s.TickerRev() == 0 {
		t.Error("ticker frame did not bump TickerRev")
	}
	if s.HealthRev() == 0 {
		t.Error("a data event must bump HealthRev")
	}
	if s.BookRev() != book0 || s.OrderRev() != order0 {
		t.Error("a ticker frame must not bump book/order revisions")
	}

	// An orderbook frame bumps only book (+health).
	bk := s.BookRev()
	s.Apply(data("orderbook", "btc_krw", stream.OriginRealtime, 60, "",
		`{"data":{"timestamp":55,"asks":[{"price":"1","qty":"2"}],"bids":[]}}`))
	if s.BookRev() == bk {
		t.Error("orderbook frame did not bump BookRev")
	}

	// MarkMarketStale flips book/trade to loading, so both must bump (display change).
	bk, tr := s.BookRev(), s.TradeRev()
	s.MarkMarketStale("btc_krw")
	if s.BookRev() == bk || s.TradeRev() == tr {
		t.Error("MarkMarketStale must bump BookRev and TradeRev")
	}

	// A notice can change connection/freshness for several panes, so it bumps the
	// notice + health revisions at minimum.
	nr, hr := s.NoticeRev(), s.HealthRev()
	s.Apply(notice(stream.Disconnected, stream.LevelWarn, 300, nil))
	if s.NoticeRev() == nr || s.HealthRev() == hr {
		t.Error("a notice must bump NoticeRev and HealthRev")
	}
}
