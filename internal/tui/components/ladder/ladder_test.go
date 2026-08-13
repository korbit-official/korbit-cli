// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func testBook() state.Orderbook {
	return state.Orderbook{
		Asks: []state.PriceLevel{ // best first, ascending
			{Price: "100010000", Qty: "1"},
			{Price: "100020000", Qty: "2"},
			{Price: "100030000", Qty: "0.5"},
		},
		Bids: []state.PriceLevel{ // best first, descending
			{Price: "100000000", Qty: "1.5"},
			{Price: "99990000", Qty: "3"},
			{Price: "99980000", Qty: "0.2"},
		},
	}
}

func order(id int64, side, price, qty, filled string) state.Order {
	return state.Order{OrderID: id, Symbol: "btc_krw", Side: side, OrderType: "limit",
		Price: price, Qty: qty, FilledQty: filled, Status: "open"}
}

// TestRowsLayout: asks above the mid (worst at the top, best at the spread),
// bids below best-first, padding keeping the mid centered.
func TestRowsLayout(t *testing.T) {
	rows := Rows(9, testBook(), nil, "") // perSide = 4
	if len(rows) != 9 {
		t.Fatalf("want 9 rows, got %d", len(rows))
	}
	wantPrices := []string{"", "100030000", "100020000", "100010000", "", "100000000", "99990000", "99980000", ""}
	for i, want := range wantPrices {
		if rows[i].Price != want {
			t.Errorf("row %d: price %q, want %q", i, rows[i].Price, want)
		}
	}
	if !rows[4].Mid {
		t.Error("row 4 should be the mid line")
	}
	if rows[3].AskQty != "1" || rows[5].BidQty != "1.5" {
		t.Errorf("best levels should carry their book qty: ask %q bid %q", rows[3].AskQty, rows[5].BidQty)
	}
}

// TestRowsMineOnLevel: an own order resting on a visible level shows in the
// MINE column of that row (remaining = qty − filled), buys on the bid side and
// sells on the ask side.
func TestRowsMineOnLevel(t *testing.T) {
	mine := []state.Order{
		order(1, "buy", "99990000", "0.9", "0.4"),
		order(2, "sell", "100020000", "0.3", "0"),
	}
	rows := Rows(9, testBook(), mine, "")
	var buyRow, sellRow *Row
	for i := range rows {
		switch rows[i].Price {
		case "99990000":
			buyRow = &rows[i]
		case "100020000":
			sellRow = &rows[i]
		}
	}
	if buyRow == nil || buyRow.MineBuy != "0.5" {
		t.Fatalf("resting buy should show remaining 0.5 at its level, got %+v", buyRow)
	}
	if buyRow.Edge {
		t.Error("an order on a visible level is not an edge row")
	}
	if sellRow == nil || sellRow.MineSell != "0.3" {
		t.Fatalf("resting sell should show 0.3 at its level, got %+v", sellRow)
	}
}

// TestRowsDistantOwnOrderPullsIn: an own order far off the visible book window
// becomes a synthetic edge row, displacing the level farthest from the spread
// on its side — nothing the account owns is ever off-screen.
func TestRowsDistantOwnOrderPullsIn(t *testing.T) {
	mine := []state.Order{order(3, "buy", "90000000", "0.1", "0")}
	rows := Rows(7, testBook(), mine, "") // perSide = 3: the bid side is full
	var edge *Row
	for i := range rows {
		if rows[i].Price == "90000000" {
			edge = &rows[i]
		}
	}
	if edge == nil {
		t.Fatalf("distant own bid must pull in as a row: %+v", rows)
	}
	if !edge.Edge || edge.MineBuy != "0.1" || edge.BidQty != "" {
		t.Errorf("pulled-in row should be a synthetic MINE row: %+v", edge)
	}
	// It displaced the worst visible bid (99980000), not a better one.
	for _, r := range rows {
		if r.Price == "99980000" {
			t.Error("the farthest bid level should have been displaced by the edge row")
		}
	}
	if last := rows[len(rows)-1]; last.Price != "90000000" {
		t.Errorf("the edge row should sit at the bottom of the bid side, got %q", last.Price)
	}
}

// TestRowsInsideSpreadOwnOrder: an own order between the best bid and best
// ask gets a row at its sorted position (top of the bid side for a buy).
func TestRowsInsideSpreadOwnOrder(t *testing.T) {
	mine := []state.Order{order(4, "buy", "100005000", "0.2", "0")}
	rows := Rows(9, testBook(), mine, "")
	var midIdx, rowIdx int = -1, -1
	for i, r := range rows {
		if r.Mid {
			midIdx = i
		}
		if r.Price == "100005000" {
			rowIdx = i
		}
	}
	if rowIdx == -1 {
		t.Fatalf("inside-spread own bid must get a row: %+v", rows)
	}
	if rowIdx != midIdx+1 {
		t.Errorf("inside-spread bid should sit just below the mid (row %d), got row %d", midIdx+1, rowIdx)
	}
}

// TestRenderMatchesRows pins the render to the Rows layout: every price row's
// line contains its formatted price at the same index — the parent's cursor
// walking and click mapping rely on this correspondence.
func TestRenderMatchesRows(t *testing.T) {
	mine := []state.Order{order(1, "buy", "99990000", "0.9", "0")}
	k := Key{Symbol: "btc_krw", Settled: true, Ready: true, W: 60, H: 9,
		Style: uikit.StyleID{Profile: colorprofile.TrueColor}}
	d := Data{Book: testBook(), HasBook: true, Mine: mine}
	lines := strings.Split(plain(New().View(k, d)), "\n")
	rows := Rows(k.H, d.Book, d.Mine, k.Level)
	if len(lines) != len(rows) {
		t.Fatalf("render has %d lines, Rows %d", len(lines), len(rows))
	}
	for i, r := range rows {
		switch {
		case r.Mid:
			if !strings.Contains(lines[i], "─") {
				t.Errorf("row %d should be the mid rule: %q", i, lines[i])
			}
		case r.Price != "":
			if !strings.Contains(lines[i], uikit.GroupThousands(r.Price)) {
				t.Errorf("row %d should show price %s: %q", i, r.Price, lines[i])
			}
		}
	}
	// The MINE cell renders with its marker.
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "0.9 ►") {
		t.Errorf("resting buy should render in the MINE column: %q", joined)
	}
}

// TestRenderCursorAndLoading: the cursor row carries the ▸ marker; an
// unready/unsettled book renders as loading.
func TestRenderCursorAndLoading(t *testing.T) {
	k := Key{Symbol: "btc_krw", Settled: true, Ready: true, CursorPrice: "100000000",
		W: 60, H: 9, Style: uikit.StyleID{Profile: colorprofile.TrueColor}}
	d := Data{Book: testBook(), HasBook: true}
	out := plain(New().View(k, d))
	cursorLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "▸") {
			cursorLine = l
		}
	}
	if !strings.Contains(cursorLine, "100,000,000") {
		t.Errorf("cursor marker should sit on the cursor price row: %q", out)
	}

	k2 := k
	k2.Ready = false
	k2.BookRev = 1 // a different key: no stale cache hit
	if out := plain(New().View(k2, d)); !strings.Contains(out, "loading…") {
		t.Errorf("an unready book should render loading…, got %q", out)
	}
}

// TestBucket: a buy truncates down onto the level grid (it aggregates with
// the bids), a sell rounds up (the asks); the raw book and unparseable
// inputs pass through unchanged.
func TestBucket(t *testing.T) {
	cases := []struct {
		price, level string
		sell         bool
		want         string
	}{
		{"99991000", "10000", false, "99990000"},
		{"99991000", "10000", true, "100000000"},
		{"99990000", "10000", false, "99990000"}, // already on the grid
		{"99990000", "10000", true, "99990000"},
		{"0.523", "0.1", false, "0.5"},
		{"0.523", "0.1", true, "0.6"},
		{"99991000", "", false, "99991000"}, // raw book: identity
		{"abc", "10000", false, "abc"},      // unparseable: identity
		{"99991000", "x", true, "99991000"},
	}
	for _, c := range cases {
		if got := Bucket(c.price, c.level, c.sell); got != c.want {
			t.Errorf("Bucket(%q,%q,sell=%v) = %q, want %q", c.price, c.level, c.sell, got, c.want)
		}
	}
}

// TestRowsBucketsMineOnGroupedBook: with a level set, an own order off the
// grid aggregates into its bucket's row instead of growing a synthetic row.
func TestRowsBucketsMineOnGroupedBook(t *testing.T) {
	mine := []state.Order{order(1, "buy", "99991000", "0.2", "0")}
	rows := Rows(9, testBook(), mine, "10000")
	for _, r := range rows {
		if r.Price == "99991000" {
			t.Fatalf("the exact price must not appear as a synthetic row: %+v", rows)
		}
		if r.Price == "99990000" && r.MineBuy != "0.2" {
			t.Fatalf("the bucket row should carry the MINE qty, got %+v", r)
		}
	}
}
