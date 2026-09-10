// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"fmt"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/stream"
)

// newHoldStore builds a Store with a controllable clock (default 1_000 ms,
// matching newTestStore).
func newHoldStore() (*Store, *int64) {
	now := int64(1_000)
	s := New(Config{}, func() int64 { return now })
	return s, &now
}

// krwBalance applies a live myAsset frame giving account 1 the KRW available.
func krwBalance(s *Store, available string) {
	s.Apply(data("myAsset", "", stream.OriginRealtime, 100, "", fmt.Sprintf(
		`{"channelType":"myAsset","asset":{"accountSeq":1,"assets":[
			{"currency":"krw","balance":"1000000","available":%q,"tradeInUse":"0","withdrawalInUse":"0"}]}}`,
		available)))
}

func availOf(t *testing.T, s *Store, accountSeq int, currency string) string {
	t.Helper()
	for _, b := range s.BalancesFor(accountSeq) {
		if b.Currency == currency {
			return b.Available
		}
	}
	t.Fatalf("no %s balance for account %d", currency, accountSeq)
	return ""
}

func TestLocalHoldReducesAvailable(t *testing.T) {
	s, _ := newHoldStore()
	krwBalance(s, "1000000")

	s.AddLocalHold(1, "cid-1", "krw", "300000")
	if got := availOf(t, s, 1, "krw"); got != "700000" {
		t.Fatalf("available with hold = %s, want 700000", got)
	}
	// A second hold stacks; Balance/TradeInUse stay the server's values.
	s.AddLocalHold(1, "cid-2", "krw", "200000")
	bals := s.BalancesFor(1)
	if bals[0].Available != "500000" || bals[0].Balance != "1000000" || bals[0].TradeInUse != "0" {
		t.Fatalf("stacked holds wrong: %+v", bals[0])
	}
	// Balances() (the all-account read) applies the same adjustment.
	if all := s.Balances(); all[0].Available != "500000" {
		t.Fatalf("Balances() available = %s, want 500000", all[0].Available)
	}
	// Releasing one hold restores its amount.
	s.ReleaseLocalHold(1, "cid-2")
	if got := availOf(t, s, 1, "krw"); got != "700000" {
		t.Fatalf("available after release = %s, want 700000", got)
	}
}

func TestLocalHoldClampsAtZeroAndScopes(t *testing.T) {
	s, _ := newHoldStore()
	krwBalance(s, "100000")

	// An over-sized hold clamps at zero rather than going negative.
	s.AddLocalHold(1, "cid-1", "krw", "150000")
	if got := availOf(t, s, 1, "krw"); got != "0" {
		t.Fatalf("over-held available = %s, want 0", got)
	}
	// A hold on another account or currency does not touch this row.
	s.ReleaseLocalHold(1, "cid-1")
	s.AddLocalHold(2, "cid-2", "krw", "50000")
	s.AddLocalHold(1, "cid-3", "btc", "1")
	if got := availOf(t, s, 1, "krw"); got != "100000" {
		t.Fatalf("cross-scope hold leaked: available = %s, want 100000", got)
	}
}

func TestLocalHoldReleasedOnMyOrderObservation(t *testing.T) {
	s, _ := newHoldStore()
	krwBalance(s, "1000000")
	s.AddLocalHold(1, "cid-1", "krw", "300000")

	// The order appears on the myOrder channel (open) — the hold releases,
	// whatever the status.
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 200, "",
		`{"channelType":"myOrder","order":{"accountSeq":1,"orders":[
			{"orderId":9001,"clientOrderId":"cid-1","side":"buy","status":"unfilled","qty":"0.003","price":"100000000"}]}}`))
	if got := availOf(t, s, 1, "krw"); got != "1000000" {
		t.Fatalf("available after myOrder = %s, want 1000000", got)
	}

	// A terminal first observation (async rejection after the accept ack)
	// releases too.
	s.AddLocalHold(1, "cid-2", "krw", "100000")
	s.Apply(data("myOrder", "btc_krw", stream.OriginRealtime, 210, "",
		`{"channelType":"myOrder","order":{"accountSeq":1,"orders":[
			{"orderId":9002,"clientOrderId":"cid-2","side":"buy","status":"expired","qty":"0.001","filledQty":"0"}]}}`))
	if got := availOf(t, s, 1, "krw"); got != "1000000" {
		t.Fatalf("available after terminal myOrder = %s, want 1000000", got)
	}

	// An /v2/openOrders backfill row releases through the same upsert path.
	s.AddLocalHold(1, "cid-3", "krw", "100000")
	s.Apply(data("myOrder", "btc_krw", stream.OriginBackfill, 300, "/v2/openOrders",
		`[{"orderId":9003,"clientOrderId":"cid-3","side":"buy","status":"open","qty":"0.001","price":"100000000"}]`))
	if got := availOf(t, s, 1, "krw"); got != "1000000" {
		t.Fatalf("available after backfill row = %s, want 1000000", got)
	}
}

func TestLocalHoldTTLBackstop(t *testing.T) {
	s, now := newHoldStore()
	krwBalance(s, "1000000")
	s.AddLocalHold(1, "cid-1", "krw", "300000")

	// Just before the deadline the hold is still active.
	*now += localHoldTTLMs - 1
	if got := availOf(t, s, 1, "krw"); got != "700000" {
		t.Fatalf("available before TTL = %s, want 700000", got)
	}
	// At the deadline the read itself expires it.
	*now++
	if got := availOf(t, s, 1, "krw"); got != "1000000" {
		t.Fatalf("available after TTL = %s, want 1000000", got)
	}
}

func TestLocalHoldsClearedOnPrivateTransitions(t *testing.T) {
	s, _ := newHoldStore()
	krwBalance(s, "1000000")

	// A private disconnect releases everything (the release signal can no
	// longer arrive).
	s.AddLocalHold(1, "cid-1", "krw", "300000")
	s.Apply(disconnectedPrivate(500))
	if got := availOf(t, s, 1, "krw"); got != "1000000" {
		t.Fatalf("available after disconnect = %s, want 1000000", got)
	}

	// A private (re)connect — the epoch bump — releases holds from the
	// superseded connection too.
	s.AddLocalHold(1, "cid-2", "krw", "300000")
	s.Apply(connectedPrivate(600))
	if got := availOf(t, s, 1, "krw"); got != "1000000" {
		t.Fatalf("available after reconnect = %s, want 1000000", got)
	}
}

func TestLocalHoldUnusableInputs(t *testing.T) {
	// Unusable registrations are dropped, not applied.
	s, _ := newHoldStore()
	krwBalance(s, "1000000")
	s.AddLocalHold(1, "", "krw", "300000")   // no clientOrderId
	s.AddLocalHold(1, "cid-2", "", "300000") // no currency
	s.AddLocalHold(1, "cid-3", "krw", "-5")  // non-positive
	s.AddLocalHold(1, "cid-4", "krw", "x")   // unparseable
	if got := availOf(t, s, 1, "krw"); got != "1000000" {
		t.Fatalf("unusable hold applied: available = %s", got)
	}
	// An unparseable server Available is returned verbatim, never invented.
	s.Apply(data("myAsset", "", stream.OriginRealtime, 110, "", `{"channelType":"myAsset","asset":{"accountSeq":1,"assets":[
		{"currency":"xyz","balance":"1","available":"","tradeInUse":"0","withdrawalInUse":"0"}]}}`))
	s.AddLocalHold(1, "cid-5", "xyz", "1")
	if got := availOf(t, s, 1, "xyz"); got != "" {
		t.Fatalf("unparseable available rewritten: %q", got)
	}
}

func TestLocalHoldBumpsBalanceRev(t *testing.T) {
	s, now := newHoldStore()
	krwBalance(s, "1000000")

	rev := s.BalanceRev()
	s.AddLocalHold(1, "cid-1", "krw", "300000")
	if s.BalanceRev() == rev {
		t.Fatal("AddLocalHold did not bump BalanceRev")
	}
	rev = s.BalanceRev()
	s.ReleaseLocalHold(1, "cid-1")
	if s.BalanceRev() == rev {
		t.Fatal("ReleaseLocalHold did not bump BalanceRev")
	}
	// TTL expiry during a read bumps too (a cached renderer must notice).
	s.AddLocalHold(1, "cid-2", "krw", "300000")
	rev = s.BalanceRev()
	*now += localHoldTTLMs
	_ = s.BalancesFor(1)
	if s.BalanceRev() == rev {
		t.Fatal("TTL expiry did not bump BalanceRev")
	}
}

func TestEstimateHold(t *testing.T) {
	base := HoldIntent{Base: "btc", Quote: "krw"}
	cases := []struct {
		name    string
		in      HoldIntent
		wantCur string
		wantAmt string
		wantOK  bool
	}{
		{"sell reserves qty verbatim whatever the shape", with(base, func(h *HoldIntent) {
			h.Side, h.Price, h.Qty = "sell", "100000000", "0.0030"
		}), "btc", "0.0030", true},
		{"qty-only sell (market/best) reserves qty", with(base, func(h *HoldIntent) {
			h.Side, h.Qty = "sell", "0.5"
		}), "btc", "0.5", true},
		{"limit buy (price+qty) reserves notional", with(base, func(h *HoldIntent) {
			h.Side, h.Price, h.Qty = "buy", "100000000", "0.003"
		}), "krw", "300000", true},
		{"limit buy with quote-fee headroom", with(base, func(h *HoldIntent) {
			h.Side, h.Price, h.Qty, h.QuoteFeeRate = "buy", "100000000", "0.003", "0.002"
		}), "krw", "300600", true},
		{"amt-sized buy (market/best) reserves amt plus headroom", with(base, func(h *HoldIntent) {
			h.Side, h.Amt, h.QuoteFeeRate = "buy", "500000", "0.002"
		}), "krw", "501000", true},
		{"qty-only buy has no bounded notional", with(base, func(h *HoldIntent) {
			h.Side, h.Qty = "buy", "0.003"
		}), "", "", false},
		{"sell without qty has no estimate", with(base, func(h *HoldIntent) {
			h.Side, h.Amt = "sell", "500000"
		}), "", "", false},
		{"unknown side has no estimate", with(base, func(h *HoldIntent) {
			h.Side, h.Qty = "hold", "1"
		}), "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur, amt, ok := tc.in.EstimateHold()
			if cur != tc.wantCur || amt != tc.wantAmt || ok != tc.wantOK {
				t.Fatalf("EstimateHold() = (%q, %q, %t), want (%q, %q, %t)",
					cur, amt, ok, tc.wantCur, tc.wantAmt, tc.wantOK)
			}
		})
	}
}

func with(h HoldIntent, mut func(*HoldIntent)) HoldIntent {
	mut(&h)
	return h
}
