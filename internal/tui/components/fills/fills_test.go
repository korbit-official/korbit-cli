// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package fills

import (
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/stream/state"
	"github.com/digitalx-official/digitalx-cli/internal/tui/uikit"
)

func sampleData() Data {
	return Data{
		Fills: []state.Fill{
			{Symbol: "btc_krw", Side: "buy", Price: "99500000", Qty: "0.01", Time: 1700000000000},
			{Symbol: "eth_krw", Side: "sell", Price: "4200000", Qty: "1.5", Time: 1700000001000},
		},
	}
}

func key() Key {
	return Key{FillRev: 1, HealthRev: 1, PrivateUp: true, W: 60, H: 10}
}

func TestRendersFillsAndDimensions(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if strings.Contains(out, "loading") {
		t.Fatalf("up private feed should not show loading: %q", out)
	}
	// Panel is H lines tall; assert the line count matches the box.
	if got := strings.Count(out, "\n") + 1; got != 10 {
		t.Errorf("panel height = %d lines, want 10", got)
	}
	// The grouped price appears.
	if !strings.Contains(out, "99,500,000") {
		t.Errorf("row should show the grouped price; got: %q", out)
	}
}

func TestLoadingWhenPrivateDown(t *testing.T) {
	m := New()
	k := key()
	k.PrivateUp = false
	if out := m.View(k, sampleData()); !strings.Contains(out, "loading") {
		t.Errorf("private-down should show loading, got: %q", out)
	}
}

func TestEmptyShowsNoFills(t *testing.T) {
	m := New()
	if out := m.View(key(), Data{}); !strings.Contains(out, "no fills yet") {
		t.Errorf("empty fills should show the empty notice, got: %q", out)
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := m.View(k, sampleData())
	// Different DATA but same KEY → cache hit returns the first render (the
	// revision in the key is the contract for "data changed").
	other := sampleData()
	other.Fills[0].Price = "1"
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render that reflects new data.
	k.FillRev = 2
	if got := m.View(k, other); strings.Contains(got, "99,500,000") {
		t.Error("after a FillRev bump the render should reflect the new fills, not the cached one")
	}
}

func TestStyleChangeInvalidates(t *testing.T) {
	m := New()
	k := key()
	a := m.View(k, sampleData())
	k.Style = uikit.StyleID{Scheme: uint8(uikit.ColorSchemeRedBlue)}
	b := m.View(k, sampleData())
	if a == b {
		t.Error("a style change should produce a different (re-rendered) frame")
	}
}
