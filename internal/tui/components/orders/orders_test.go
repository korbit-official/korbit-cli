// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package orders

import (
	"regexp"
	"strings"
	"testing"
)

// ansi strips SGR escape sequences so a title assertion can match the plain text
// under the tab chips' per-segment color styling.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

func sampleData() Data {
	return Data{
		Rows: [][]string{
			{"btc_krw", "buy", "99000000", "0.5", "0.1"},
			{"eth_krw", "sell", "4200000", "2.0", "0.0"},
		},
		OrderIDs: []int64{11, 22},
	}
}

func key() Key {
	return Key{RowsRev: 1, Cursor: 0, Scroll: 0, Focused: true, Count: 2, Symbol: "btc_krw", W: 70, H: 12}
}

func TestRendersTableAndDimensions(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if strings.Contains(out, "loading") {
		t.Fatalf("a non-loading key should not show the loading hint: %q", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 12 {
		t.Errorf("panel height = %d lines, want 12", got)
	}
	if !strings.Contains(out, "symbol") {
		t.Errorf("table header should be present; got: %q", out)
	}
	if !strings.Contains(out, "btc_krw") {
		t.Errorf("a data row should be present; got: %q", out)
	}
	if !strings.Contains(plain(out), "orders: [open] │ closed") {
		t.Errorf("the title should read the orders noun then the switcher, active tab bracketed; got: %q", out)
	}
	if !strings.Contains(out, "1/2") {
		t.Errorf("footer position should be present; got: %q", out)
	}
}

func TestNoStatusColumn(t *testing.T) {
	m := New()
	out := m.View(key(), sampleData())
	if strings.Contains(out, "status") {
		t.Errorf("the status column was dropped; header should not contain it: %q", out)
	}
	for _, want := range []string{"symbol", "side", "price", "qty", "filled"} {
		if !strings.Contains(out, want) {
			t.Errorf("header should still contain %q; got: %q", want, out)
		}
	}
}

func TestCancelingRowStyled(t *testing.T) {
	m := New()
	k := key()
	k.Cursor = -1 // no selection, so the styling can only come from Canceling
	d := sampleData()
	// Baseline: no canceling → plain rows.
	plain := m.View(k, d)
	// Mark the first order canceling and force a re-render.
	k.RowsRev = 2
	d.Canceling = []bool{true, false}
	styled := m.View(k, d)
	if styled == plain {
		t.Error("a canceling row should render differently from a plain row")
	}
	// The canceling row is gray (basic-ANSI bright black, SGR 90).
	if !strings.Contains(styled, "\x1b[90m") {
		t.Errorf("canceling row should carry the gray (SGR 90) foreground; got: %q", styled)
	}
}

func TestLoadingHintInTitle(t *testing.T) {
	m := New()
	k := key()
	k.Loading = true
	if out := m.View(k, sampleData()); !strings.Contains(out, "loading") {
		t.Errorf("a loading key should show the loading hint, got: %q", out)
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := m.View(k, sampleData())
	// Different DATA but same KEY → cache hit returns the first render (RowsRev is
	// the contract for "rows changed", so a caller must bump it).
	other := sampleData()
	other.Rows[0][0] = "xrp_krw"
	if got := m.View(k, other); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping RowsRev forces a re-render that reflects the new rows.
	k.RowsRev = 2
	if got := m.View(k, other); !strings.Contains(got, "xrp_krw") {
		t.Error("after a RowsRev bump the render should reflect the new rows, not the cached one")
	}
}

func TestKeyChangeInvalidates(t *testing.T) {
	m := New()
	k := key()
	// Moving the cursor changes which row is reverse-highlighted, so a new Key
	// must produce a different (re-rendered) frame.
	first := m.View(k, sampleData())
	k.Cursor = 1
	second := m.View(k, sampleData())
	if first == second {
		t.Error("moving the cursor should re-render (different highlighted row)")
	}
	// The all-pairs scope and the focus flag both feed the chrome, so each is a
	// distinct render.
	k = key()
	k.AllPairs = true
	if m.View(k, sampleData()) == first {
		t.Error("toggling the all-pairs scope should re-render the title")
	}
}

func TestClosedTabTitleAndColumns(t *testing.T) {
	m := New()
	k := key()
	k.Closed = true
	out := m.View(k, Data{Rows: [][]string{{"btc_krw", "buy", "100", "0/1", "expired"}}, OrderIDs: []int64{1}})
	if !strings.Contains(plain(out), "open │ [closed]") {
		t.Errorf("the closed tab must bracket its segment; got: %q", out)
	}
	for _, want := range []string{"filled/qty", "status", "expired"} {
		if !strings.Contains(out, want) {
			t.Errorf("closed tab should contain %q; got: %q", want, out)
		}
	}
}

func TestTitleTabHitRanges(t *testing.T) {
	// English labels, with the "orders: " prefix (8 cells) before the switcher.
	// Open tab active: "orders: [open] │ closed" — "[open]" spans cells 8-13,
	// the separator 14-16, "closed" 17-22.
	for _, tc := range []struct {
		x        int
		closed   bool // current tab
		toClosed bool
		ok       bool
	}{
		{7, false, false, false},  // inside the "orders: " prefix — inert
		{8, false, false, true},   // "[open]" first cell
		{13, false, false, true},  // last cell of "[open]"
		{15, false, false, false}, // the separator
		{17, false, true, true},   // "closed" first cell
		{22, false, true, true},   // last cell of "closed"
		{23, false, false, false}, // past the segments
		{10, true, false, true},   // closed active: "open" segment (cells 8-11)
		{15, true, true, true},    // closed active: "[closed]" starts at cell 15
		{-1, false, false, false},
	} {
		toClosed, ok := TitleTab(tc.x, tc.closed)
		if toClosed != tc.toClosed || ok != tc.ok {
			t.Errorf("TitleTab(%d, closed=%v) = (%v, %v), want (%v, %v)",
				tc.x, tc.closed, toClosed, ok, tc.toClosed, tc.ok)
		}
	}
}
