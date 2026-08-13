// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// The TUI is tested by driving the model directly: messages in, model/cmd
// out, rendered frames inspected as plain text. No real terminal or program
// loop is involved.

type fakeTrader struct {
	tickets   []OrderForm
	cancels   [][3]string // symbol, orderID, accountSeq
	placeErr  error
	cancelErr error // when set, every Cancel fails with it
	result    PlaceResult
}

func (f *fakeTrader) Place(t OrderForm) (PlaceResult, error) {
	f.tickets = append(f.tickets, t)
	if f.placeErr != nil {
		return PlaceResult{}, f.placeErr
	}
	return f.result, nil
}

func (f *fakeTrader) Cancel(symbol string, orderID int64, accountSeq int) (string, error) {
	f.cancels = append(f.cancels, [3]string{symbol, fmt.Sprint(orderID), fmt.Sprint(accountSeq)})
	return "", f.cancelErr
}

// assertPlacedForm compares a dispatched form against the expected wire
// fields. The model mints a fresh ClientOrderID per dispatch, so it is
// asserted present and compared blanked.
func assertPlacedForm(t *testing.T, got, want OrderForm) {
	t.Helper()
	if got.ClientOrderID == "" {
		t.Fatal("dispatched form must carry a minted clientOrderId")
	}
	got.ClientOrderID = ""
	if got != want {
		t.Fatalf("form mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func testModel(t *testing.T, private bool, trader Trader) model {
	t.Helper()
	stopped := false
	m := newModel(Config{
		Symbols:     []string{"btc_krw", "eth_krw"},
		Private:     private,
		Trader:      trader,
		KeyName:     "testkey",
		BaseURL:     "http://127.0.0.1:9999",
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() { stopped = true },
	})
	_ = stopped
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	return mm.(model)
}

func press(t *testing.T, m model, keys ...tea.KeyPressMsg) (model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, k := range keys {
		var mm tea.Model
		mm, cmd = m.Update(k)
		m = mm.(model)
	}
	return m, cmd
}

func k(code rune, text string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code, Text: text} }
func special(code rune) tea.KeyPressMsg        { return tea.KeyPressMsg{Code: code} }

func typeText(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		m, _ = press(t, m, k(r, string(r)))
	}
	return m
}

func feed(t *testing.T, m model, ev stream.Event) model {
	t.Helper()
	mm, _ := m.Update(streamEventMsg{ev: ev})
	return mm.(model)
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func dataEvent(channel, symbol string, origin stream.Origin, serverTime int64, source, payload string) stream.Data {
	return stream.Data{Channel: channel, Symbol: symbol, Origin: origin,
		ServerTime: serverTime, Source: source, Payload: json.RawMessage(payload)}
}

const openOrderRow = `{"orderId":777,"status":"open","side":"buy","orderType":"limit",
	"price":"99017000","qty":"0.9","filledQty":"0","filledAmt":"0","createdAt":900,"clientOrderId":"cid-7"}`

func seedOpenOrder(t *testing.T, m model) model {
	t.Helper()
	return feed(t, m, dataEvent("myOrder", "btc_krw", stream.OriginBackfill, 100, "/v2/openOrders",
		"["+openOrderRow+"]"))
}

// seedOrders feeds an open-orders snapshot of the given ids for one symbol (so
// the symbol becomes "ready"), createdAt ascending in id order.
func seedOrders(t *testing.T, m model, symbol string, ids ...int64) model {
	t.Helper()
	rows := make([]string, len(ids))
	for i, id := range ids {
		rows[i] = fmt.Sprintf(`{"orderId":%d,"status":"open","side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","createdAt":%d,"clientOrderId":"c%d"}`,
			id, 900+i, id)
	}
	return feed(t, m, dataEvent("myOrder", symbol, stream.OriginBackfill, 100, "/v2/openOrders",
		"["+strings.Join(rows, ",")+"]"))
}

// drainCmds runs a chain of tea.Cmds to completion, feeding each resulting msg
// back into the model — used to drive a sequential cancel-all batch.
func drainCmds(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for i := 0; len(queue) > 0 && i < 1000; i++ {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		msg := next()
		if msg == nil {
			continue
		}
		// A batched command hands its children back as a message the runtime
		// expands; off the runtime this helper has to expand them itself, or only
		// one of the batched fetches (bands, bounds, fees) would ever land.
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		mm, more := m.Update(msg)
		m = mm.(model)
		queue = append(queue, more)
	}
	return m
}

// --- navigation / lifecycle ---

func TestSymbolSwitching(t *testing.T) {
	m := testModel(t, false, nil) // public: the market selector is the only pane
	// Arrows change the selection immediately (no enter needed).
	m, _ = press(t, m, special(tea.KeyDown))
	if m.symbol() != "eth_krw" {
		t.Fatalf("down should select the next symbol immediately, got %s", m.symbol())
	}
	m, _ = press(t, m, special(tea.KeyUp))
	if m.symbol() != "btc_krw" {
		t.Fatalf("up should go back, got %s", m.symbol())
	}
	// Digits are deliberately unbound (reserved for future account-seq
	// selection) — pressing one must not move the selection.
	m, _ = press(t, m, k('2', "2"))
	if m.symbol() != "btc_krw" {
		t.Fatalf("a digit must not jump the selection, got %s", m.symbol())
	}
}

// TestFocusCyclesBetweenMarketAndOrders: tab moves focus around the ring
// (markets → orders → balances → markets), and navigation keys go to the
// focused pane (symbol selection vs the orders table vs the balances scroll).
func TestFocusCyclesBetweenMarketAndOrders(t *testing.T) {
	m := testModel(t, true, &fakeTrader{}) // private: market + orders + balances
	if m.focus != focusMarket {
		t.Fatalf("default focus should be the market selector, got %v", m.focus)
	}
	m, _ = press(t, m, special(tea.KeyTab))
	if m.focus != focusOrders {
		t.Fatalf("tab should move focus to the orders table, got %v", m.focus)
	}
	// With orders focused, down drives the table, not the symbol.
	before := m.symbol()
	m, _ = press(t, m, special(tea.KeyDown))
	if m.symbol() != before {
		t.Fatalf("with orders focused, down must not change the symbol (got %s)", m.symbol())
	}
	m, _ = press(t, m, special(tea.KeyTab))
	if m.focus != focusBalances {
		t.Fatalf("tab should move focus to the balances list, got %v", m.focus)
	}
	m, _ = press(t, m, special(tea.KeyTab))
	if m.focus != focusMarket {
		t.Fatalf("tab should wrap back to the market selector, got %v", m.focus)
	}
	m, _ = press(t, m, special(tea.KeyDown))
	if m.symbol() != "eth_krw" {
		t.Fatalf("with markets focused, down should switch the symbol, got %s", m.symbol())
	}
}

// TestBalancesScroll: the balances pane is focusable and scrolls (no selection)
// via the arrows and the mouse wheel; its footer shows the visible range.
func TestBalancesScroll(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	rows := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		rows = append(rows, fmt.Sprintf(
			`{"currency":"c%02d","balance":"%d","available":"%d","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}`, i, i, i))
	}
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		"["+strings.Join(rows, ",")+"]"))

	vis := m.balancesVisible()
	if vis < 1 || vis >= 10 {
		t.Fatalf("expected a windowed balances panel, visible=%d", vis)
	}

	m, _ = press(t, m, special(tea.KeyTab), special(tea.KeyTab))
	if m.focus != focusBalances {
		t.Fatalf("two tabs should focus the balances list, got %v", m.focus)
	}
	m, _ = press(t, m, special(tea.KeyDown))
	if m.balScroll != 1 {
		t.Fatalf("down should scroll balances by 1, got %d", m.balScroll)
	}
	m, _ = press(t, m, special(tea.KeyHome), special(tea.KeyPgDown))
	if m.balScroll != vis {
		t.Fatalf("pgdown should scroll balances by one page (%d), got %d", vis, m.balScroll)
	}
	m, _ = press(t, m, special(tea.KeyEnd))
	if m.balScroll != 10-vis {
		t.Fatalf("end should scroll to the last window (%d), got %d", 10-vis, m.balScroll)
	}
	_, _, _, rightW := m.colWidths()
	_, _, bh := m.rightHeights(m.bodyHeight())
	if f := plain(m.viewBalances(rightW, bh)); !strings.Contains(f, "/10") {
		t.Fatalf("footer should show the total, got %q", f)
	}
	m, _ = press(t, m, special(tea.KeyHome))
	if m.balScroll != 0 {
		t.Fatalf("home should scroll back to the top, got %d", m.balScroll)
	}
	// The wheel scrolls the balances window when the pointer is over it (the
	// right column's bottom panel at 120x32).
	m = send(t, m, tea.MouseWheelMsg{X: 90, Y: 24, Button: tea.MouseWheelDown})
	if m.balScroll != wheelStep {
		t.Fatalf("wheel down over balances should scroll by %d, got %d", wheelStep, m.balScroll)
	}
}

// TestQuickSearchMarkets: "/" filters the markets list live; the active symbol
// is NOT touched until enter (command keys are captured as query text), and
// enter selects the highlighted match and drops back to the full list.
func TestQuickSearchMarkets(t *testing.T) {
	m := testModel(t, false, nil) // markets is the only / focused pane
	m, _ = press(t, m, k('/', "/"))
	if !m.searching || m.searchPane != focusMarket {
		t.Fatalf("/ should start a market search, got searching=%v pane=%v", m.searching, m.searchPane)
	}
	m = typeText(t, m, "eth")
	if got := m.filteredSymbols(); len(got) != 1 || m.cfg.Symbols[got[0]] != "eth_krw" {
		t.Fatalf("search should filter the list to eth_krw, got %v", got)
	}
	if m.mode != modeNormal {
		t.Fatalf("a command key typed into the search must not execute, mode=%v", m.mode)
	}
	if m.symbol() != "btc_krw" {
		t.Fatalf("filtering must not move the active symbol before enter, got %s", m.symbol())
	}
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.searching {
		t.Fatal("enter should end the search (the full list returns)")
	}
	if m.symbol() != "eth_krw" {
		t.Fatalf("enter should select the highlighted match, got %s", m.symbol())
	}
}

// TestQuickSearchArrows: with several matches, the arrows move the highlight in
// the filtered list and enter selects whichever is highlighted.
func TestQuickSearchArrows(t *testing.T) {
	m := testModel(t, false, nil)
	m, _ = press(t, m, k('/', "/"))
	m = typeText(t, m, "krw") // both btc_krw and eth_krw match
	if len(m.filteredSymbols()) != 2 {
		t.Fatalf("both symbols should match 'krw', got %d", len(m.filteredSymbols()))
	}
	m, _ = press(t, m, special(tea.KeyDown)) // highlight the 2nd match
	if m.searchCursor != 1 {
		t.Fatalf("down should move the search cursor, got %d", m.searchCursor)
	}
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.symbol() != "eth_krw" {
		t.Fatalf("enter should select the highlighted (2nd) match, got %s", m.symbol())
	}
}

// TestQuickSearchWindowingAndNoMatch: with more matches than fit, moving the
// highlight down scrolls the filtered window to keep it visible; a query that
// matches nothing shows "no match" and commits nothing.
func TestQuickSearchWindowingAndNoMatch(t *testing.T) {
	syms := make([]string, 60)
	for i := range syms {
		syms[i] = fmt.Sprintf("c%02d_krw", i)
	}
	m := newModel(Config{Symbols: syms, Now: func() int64 { return 0 }})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = mm.(model)

	m, _ = press(t, m, k('/', "/"))
	m = typeText(t, m, "krw") // all 60 match
	if len(m.filteredSymbols()) != 60 {
		t.Fatalf("all symbols should match, got %d", len(m.filteredSymbols()))
	}
	vis := m.sidebarVisible()
	for i := 0; i < 40; i++ { // drive the highlight well past one screen
		m, _ = press(t, m, special(tea.KeyDown))
	}
	if m.searchCursor != 40 {
		t.Fatalf("down x40 should land on cursor 40, got %d", m.searchCursor)
	}
	if m.searchCursor < m.searchScroll || m.searchCursor >= m.searchScroll+vis {
		t.Fatalf("the highlight must stay in the window: cursor=%d scroll=%d vis=%d",
			m.searchCursor, m.searchScroll, vis)
	}
	// Render the sidebar wide enough that the bottom-border footer keeps both the
	// live query and the match-count indicator (a narrow column drops the count).
	if f := plain(m.viewSidebar(40, m.bodyHeight())); !strings.Contains(f, "/60 match") {
		t.Fatalf("footer should show the match count, got %q", f)
	}

	// A query matching nothing: empty result, "no match" line, enter is a no-op.
	before := m.symbol()
	m = typeText(t, m, "zzz")
	if len(m.filteredSymbols()) != 0 {
		t.Fatalf("'krwzzz' should match nothing, got %d", len(m.filteredSymbols()))
	}
	if out := plain(m.viewSidebar(40, m.bodyHeight())); !strings.Contains(out, "no match") {
		t.Fatalf("an empty filter should render a 'no match' line, got %q", out)
	}
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.searching {
		t.Fatal("enter should close the search even with no match")
	}
	if m.symbol() != before {
		t.Fatalf("a no-match enter must not move the selection, got %s want %s", m.symbol(), before)
	}
}

// TestQuickSearchEscCancels: filtering touches nothing underneath, so esc just
// closes the search and the selection is unchanged.
func TestQuickSearchEscCancels(t *testing.T) {
	m := testModel(t, false, nil)
	m, _ = press(t, m, k('/', "/"))
	m = typeText(t, m, "eth")
	if m.symbol() != "btc_krw" {
		t.Fatalf("filtering must not move the selection, got %s", m.symbol())
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.searching || m.searchQuery != "" {
		t.Fatalf("esc should close the search, got searching=%v q=%q", m.searching, m.searchQuery)
	}
	if m.symbol() != "btc_krw" {
		t.Fatalf("esc must leave the selection unchanged, got %s", m.symbol())
	}
}

func TestQuickSearchBackspaceOnEmptyCancels(t *testing.T) {
	m := testModel(t, false, nil)
	m, _ = press(t, m, k('/', "/"))
	m = typeText(t, m, "et")
	// Backspacing the query down to empty keeps the search open.
	m, _ = press(t, m, special(tea.KeyBackspace))
	m, _ = press(t, m, special(tea.KeyBackspace))
	if !m.searching || m.searchQuery != "" {
		t.Fatalf("backspacing to empty should keep search open, got searching=%v q=%q", m.searching, m.searchQuery)
	}
	// One more backspace on the empty query backs out of search, like esc.
	m, _ = press(t, m, special(tea.KeyBackspace))
	if m.searching {
		t.Fatalf("backspace on an empty query should cancel the search, got searching=%v", m.searching)
	}
	if m.symbol() != "btc_krw" {
		t.Fatalf("cancelling must leave the selection unchanged, got %s", m.symbol())
	}
}

// searchPromptMark is the bottom-left prompt a pane's border shows while search
// is active ("╰─ /"); absent when not searching.
const searchPromptMark = "╰─ /"

func lastBorderLine(s string) string {
	lines := strings.Split(plain(s), "\n")
	return lines[len(lines)-1]
}

// TestQuickSearchShowsPromptOnSlash: pressing "/" shows the "/" search prompt in
// the pane border immediately, before any query is typed — an active empty
// search must be visibly distinct from no search.
func TestQuickSearchShowsPromptOnSlash(t *testing.T) {
	m := testModel(t, false, nil) // markets focused
	if b := lastBorderLine(m.viewSidebar(40, m.bodyHeight())); strings.Contains(b, searchPromptMark) {
		t.Fatalf("no search prompt expected before /, got %q", b)
	}
	m, _ = press(t, m, k('/', "/"))
	if b := lastBorderLine(m.viewSidebar(40, m.bodyHeight())); !strings.Contains(b, searchPromptMark) {
		t.Fatalf("pressing / must show the search prompt with an empty query, got %q", b)
	}
}

// TestQuickSearchTypesLetterKeys: every printable key (including j and k) is
// appended to the query, not treated as navigation; the arrows, not the letters,
// move the candidate.
func TestQuickSearchTypesLetterKeys(t *testing.T) {
	m := testModel(t, false, nil) // btc_krw, eth_krw
	m, _ = press(t, m, k('/', "/"))
	m = typeText(t, m, "kj") // both letters must land in the query
	if m.searchQuery != "kj" {
		t.Fatalf("k and j must be typed into the query, not consumed as navigation, got %q", m.searchQuery)
	}
	// A query that does match, then the arrow moves the candidate.
	m, _ = press(t, m, special(tea.KeyBackspace), special(tea.KeyBackspace))
	m = typeText(t, m, "krw") // matches both symbols
	if len(m.filteredSymbols()) != 2 {
		t.Fatalf("'krw' should match both symbols, got %d", len(m.filteredSymbols()))
	}
	m, _ = press(t, m, special(tea.KeyDown))
	if m.searchCursor != 1 {
		t.Fatalf("the down arrow should move the candidate, got %d", m.searchCursor)
	}
}

// TestQuickSearchBalancesPromptAndLetters covers both behaviors on the balances
// pane: "/" shows the prompt immediately on an empty query, and "/k" filters by
// "k" (the k is typed, not treated as navigation).
func TestQuickSearchBalancesPromptAndLetters(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1","available":"1","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"1","available":"1","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	m, _ = press(t, m, special(tea.KeyTab), special(tea.KeyTab)) // focus balances
	m, _ = press(t, m, k('/', "/"))
	if b := lastBorderLine(m.viewBalances(40, m.bodyHeight())); !strings.Contains(b, searchPromptMark) {
		t.Fatalf("balances / must show the prompt with an empty query, got %q", b)
	}
	m = typeText(t, m, "k")
	if m.searchQuery != "k" {
		t.Fatalf("typing k must append to the balances query, got %q", m.searchQuery)
	}
	if got := m.filteredBalances(); len(got) != 1 {
		t.Fatalf("query 'k' should match only krw, got %d", len(got))
	}
	if b := lastBorderLine(m.viewBalances(40, m.bodyHeight())); !strings.Contains(b, searchPromptMark+"k") {
		t.Fatalf("balances border must show /k, got %q", b)
	}
}

// TestQuickSearchBalances: with balances focused, "/" + enter scrolls the window
// so the first matching currency is visible.
func TestQuickSearchBalances(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	rows := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		rows = append(rows, fmt.Sprintf(
			`{"currency":"c%02d","balance":"%d","available":"%d","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}`, i, i, i))
	}
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		"["+strings.Join(rows, ",")+"]"))
	m, _ = press(t, m, special(tea.KeyTab), special(tea.KeyTab)) // focus balances
	m, _ = press(t, m, k('/', "/"))
	if m.searchPane != focusBalances {
		t.Fatalf("/ on the balances pane should target it, got %v", m.searchPane)
	}
	m = typeText(t, m, "c07")
	if got := m.filteredBalances(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("search should filter balances to c07 (index 7), got %v", got)
	}
	if m.balScroll != 0 {
		t.Fatalf("filtering must not move the real scroll before enter, got %d", m.balScroll)
	}
	// enter scrolls the full list to the match and the full list returns.
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.searching {
		t.Fatal("enter should end the balances search")
	}
	vis := m.balancesVisible()
	if 7 < m.balScroll || 7 >= m.balScroll+vis { // index 7 (c07) must now be in the full-list window
		t.Fatalf("enter should scroll the full list to the match: scroll=%d vis=%d", m.balScroll, vis)
	}
}

// TestPublicModeTabIsNoOp: with a single focusable pane, tab can't change focus.
func TestPublicModeTabIsNoOp(t *testing.T) {
	m := testModel(t, false, nil)
	m, _ = press(t, m, special(tea.KeyTab))
	if m.focus != focusMarket {
		t.Fatalf("public mode has one pane; tab must stay on markets, got %v", m.focus)
	}
}

// TestPublicModeOrdersTabKeyIsNoOp: there is no orders pane in public mode, so
// `o` must not toggle its (invisible) tab state.
func TestPublicModeOrdersTabKeyIsNoOp(t *testing.T) {
	m := testModel(t, false, nil)
	if m.ordersClosed {
		t.Fatal("the closed tab must not be selected at startup")
	}
	m, _ = press(t, m, k('o', "o"))
	if m.ordersClosed {
		t.Fatal("o must be a no-op in public mode (no orders pane to toggle)")
	}
}

// TestMouseClickFocusesPane: a left click on a focusable pane focuses it; a click
// on a non-focusable area (orderbook/trades center, header) is a no-op and leaves
// focus where it was. (120x32 private model: the right column starts at x=72 and
// the orders sub-panel spans y∈[2,13).)
func TestMouseClickFocusesPane(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, mclick(90, 5)) // inside the open-orders panel (right column)
	if m.focus != focusOrders {
		t.Fatalf("click in the orders panel should focus it, got %v", m.focus)
	}
	// The orderbook/trades center is not focusable: clicking it must not steal
	// focus back to markets.
	m = send(t, m, mclick(40, 5))
	if m.focus != focusOrders {
		t.Fatalf("click on the orderbook center should not change focus, got %v", m.focus)
	}
	// The header is outside the body — also a no-op.
	m = send(t, m, mclick(10, 0))
	if m.focus != focusOrders {
		t.Fatalf("click on the header should not change focus, got %v", m.focus)
	}
	// A click on the market sidebar focuses markets.
	m = send(t, m, mclick(5, 5))
	if m.focus != focusMarket {
		t.Fatalf("click on the sidebar should focus markets, got %v", m.focus)
	}
}

// TestStreamBatchAppliesAllAndRefreshesOnce: the coalesced pump delivers events
// in batches; the batch handler must apply every event in order and rebuild the
// orders table once, only when an order actually changed.
func TestStreamBatchAppliesAllAndRefreshesOnce(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	batch := []stream.Event{
		dataEvent("ticker", "btc_krw", stream.OriginSnapshot, 1, "", `{"type":"ticker","timestamp":1,"symbol":"btc_krw","data":{"close":"100"}}`),
		dataEvent("myOrder", "btc_krw", stream.OriginBackfill, 1, "/v2/openOrders", "["+openOrderRow+"]"),
		dataEvent("ticker", "eth_krw", stream.OriginSnapshot, 1, "", `{"type":"ticker","timestamp":1,"symbol":"eth_krw","data":{"close":"200"}}`),
	}
	mm, _ := m.Update(streamBatchMsg{evs: batch})
	m = mm.(model)
	if _, ok := m.store.Ticker("btc_krw"); !ok {
		t.Fatal("batch must apply every event (btc ticker missing)")
	}
	if _, ok := m.store.Ticker("eth_krw"); !ok {
		t.Fatal("batch must apply every event (eth ticker missing)")
	}
	if len(m.orderIDs) != 1 || m.orderIDs[0] != 777 {
		t.Fatalf("batch with a myOrder event must rebuild the orders table, got %v", m.orderIDs)
	}
}

// TestNonOrderEventDoesNotRebuildOrders: a non-order event must not touch the
// orders table (the per-event refresh that did is what starved input under a
// many-pair stream).
func TestNonOrderEventDoesNotRebuildOrders(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = seedOpenOrder(t, m) // 1 order
	// Simulate a stale order id sneaking into the table, then feed a ticker:
	// if the ticker rebuilt the table it would be corrected; we assert it does NOT.
	m.orderIDs = []int64{777, 999}
	m = feed(t, m, dataEvent("ticker", "btc_krw", stream.OriginSnapshot, 2, "", `{"type":"ticker","timestamp":2,"symbol":"btc_krw","data":{"close":"1"}}`))
	if len(m.orderIDs) != 2 {
		t.Fatalf("a ticker event must not rebuild the orders table, got %v", m.orderIDs)
	}
}

func TestQuitStopsSession(t *testing.T) {
	stopped := false
	m := newModel(Config{Symbols: []string{"btc_krw"}, Now: func() int64 { return 0 },
		StopSession: func() { stopped = true }})
	_, cmd := press(t, m, k('q', "q"))
	if cmd == nil || cmd() != (tea.QuitMsg{}) {
		t.Fatal("q must quit")
	}
	if !stopped {
		t.Fatal("q must stop the stream session")
	}
}

// TestColorSchemeTogglePersistDecision: the C key toggles the live scheme, and
// persistColorScheme (what Run writes on exit) reports the new scheme only when
// it differs from the loaded one — toggling back to the start persists nothing.
func TestColorSchemeTogglePersistDecision(t *testing.T) {
	m := testModel(t, false, nil) // loads with no stored scheme → green-red default
	if _, changed := m.persistColorScheme(); changed {
		t.Fatal("unchanged scheme must not persist")
	}
	m, _ = press(t, m, k('C', "C"))
	if m.colorScheme != uikit.ColorSchemeRedBlue {
		t.Fatalf("C must toggle to red-blue, got %v", m.colorScheme)
	}
	name, changed := m.persistColorScheme()
	if !changed || name != "red-blue" {
		t.Fatalf("toggled scheme must persist as red-blue, got (%q, %v)", name, changed)
	}
	// Toggling back to the loaded scheme persists nothing.
	m, _ = press(t, m, k('C', "C"))
	if _, changed := m.persistColorScheme(); changed {
		t.Fatal("toggling back to the loaded scheme must not persist")
	}
}

// TestColorSchemeLoadsFromConfig: a stored scheme seeds the initial view, and a
// no-op session (back to the stored value) persists nothing.
func TestColorSchemeLoadsFromConfig(t *testing.T) {
	m := newModel(Config{Symbols: []string{"btc_krw"}, Now: func() int64 { return 0 },
		ColorScheme: "red-blue"})
	if m.colorScheme != uikit.ColorSchemeRedBlue {
		t.Fatalf("stored scheme must seed the model, got %v", m.colorScheme)
	}
	if _, changed := m.persistColorScheme(); changed {
		t.Fatal("no toggle from the stored scheme must not persist")
	}
}

// TestOrderLevelsFromConfig: configured size levels seed both order UIs (panel
// and ladder), and an unset config falls back to the built-in defaults.
func TestOrderLevelsFromConfig(t *testing.T) {
	m := newModel(Config{Symbols: []string{"btc_krw"}, Now: func() int64 { return 0 },
		OrderLevels: []int{5, 10, 20}})
	for _, got := range [][]int{m.order.sizeLevels, m.ladder.sizeLevels} {
		if len(got) != 3 || got[0] != 5 || got[2] != 20 {
			t.Fatalf("size levels = %v, want [5 10 20]", got)
		}
	}

	def := newModel(Config{Symbols: []string{"btc_krw"}, Now: func() int64 { return 0 }})
	if len(def.order.sizeLevels) != 4 || def.order.sizeLevels[3] != 100 {
		t.Fatalf("default size levels = %v", def.order.sizeLevels)
	}

	// The footer's ladder hint must track the configured level count too.
	m3 := newModel(Config{Symbols: []string{"btc_krw"}, Now: func() int64 { return 0 },
		OrderLevels: []int{5, 10, 20}})
	if got := m3.footerKey().SizeKeys; got != "1-3" {
		t.Fatalf("footer SizeKeys = %q, want 1-3", got)
	}
}

// TestLadderPresetKeyUsesConfiguredLevel: a number key arms the configured
// level at that index; a digit past the configured levels is a no-op.
func TestLadderPresetKeyUsesConfiguredLevel(t *testing.T) {
	l := newLadderModel(nil, accountseq.Main, []int{5, 10, 20})
	l.symbol = "btc_krw"
	l, _ = l.handleBrowseKey(k('2', "2"), "", "", nil, nil, "")
	if l.sizePct() != 10 {
		t.Fatalf("key 2 armed %d%%, want 10%%", l.sizePct())
	}
	l, _ = l.handleBrowseKey(k('4', "4"), "", "", nil, nil, "") // beyond 3 levels
	if l.sizePct() != 10 {
		t.Fatalf("out-of-range key changed size to %d%%", l.sizePct())
	}
}

// TestDefaultOrderLevelsValidPerConfig pins the built-in defaults (what the cli
// pins into config on first launch) against config's own validator, so the
// pinned value can never fail the validation config load enforces.
func TestDefaultOrderLevelsValidPerConfig(t *testing.T) {
	if !config.ValidOrderLevels(DefaultOrderLevels()) {
		t.Fatalf("DefaultOrderLevels %v rejected by config.ValidOrderLevels", DefaultOrderLevels())
	}
	// The order UIs must bind at least as many presets as config accepts, or a
	// valid pinned config would be silently truncated to the defaults here.
	if maxSizeLevels < config.MaxOrderLevels {
		t.Fatalf("maxSizeLevels (%d) < config.MaxOrderLevels (%d): configured levels would be dropped",
			maxSizeLevels, config.MaxOrderLevels)
	}
}

func TestSizeHintLabels(t *testing.T) {
	levels := []int{10, 25, 50, 100}
	if got := sizeKeysLabel(levels); got != "1-4" {
		t.Fatalf("sizeKeysLabel = %q", got)
	}
	if got := sizeValuesLabel(levels); got != "10/25/50/max" {
		t.Fatalf("sizeValuesLabel = %q", got)
	}
	if got := sizeKeysLabel([]int{10}); got != "1" {
		t.Fatalf("single-level keys label = %q", got)
	}
}

func TestEventsClosedQuits(t *testing.T) {
	m := testModel(t, false, nil)
	_, cmd := m.Update(eventsClosedMsg{})
	if cmd == nil || cmd() != (tea.QuitMsg{}) {
		t.Fatal("a closed event stream must end the TUI")
	}
}

// TestQuitConfirmWhileActionInFlight: q is the soft quit — with a money action
// on the wire it opens a confirm dialog instead of quitting; enter quits, esc
// stays. When idle, q quits at once (TestQuitStopsSession).
func TestQuitConfirmWhileActionInFlight(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m.orderInFlight = true
	m, cmd := press(t, m, k('q', "q"))
	if cmd != nil || m.mode != modeQuitConfirm {
		t.Fatalf("q with an action in flight must ask first, mode=%v", m.mode)
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal {
		t.Fatalf("esc must stay in the TUI, mode=%v", m.mode)
	}
	m, _ = press(t, m, k('q', "q"))
	_, cmd = press(t, m, special(tea.KeyEnter))
	if cmd == nil || cmd() != (tea.QuitMsg{}) {
		t.Fatal("enter on the quit confirm must quit")
	}
}

// TestQuitConfirmAutoDismissOnActionDone: the soft-quit dialog exists only to
// warn about an in-flight action; when that action's result lands while the
// dialog is open, it dismisses itself so the result toast surfaces.
func TestQuitConfirmAutoDismissOnActionDone(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m.orderInFlight = true
	m, _ = press(t, m, k('q', "q"))
	if m.mode != modeQuitConfirm {
		t.Fatalf("setup: q must open the quit confirm, mode=%v", m.mode)
	}
	mm, cmd := m.Update(placeDoneMsg{origin: placeFromBar})
	m = mm.(model)
	if m.mode != modeNormal {
		t.Fatalf("the dialog must auto-dismiss once the action lands, mode=%v", m.mode)
	}
	if m.orderInFlight {
		t.Fatal("the in-flight gate must have cleared")
	}
	if cmd != nil && cmd() == (tea.QuitMsg{}) {
		t.Fatal("the landing action must not quit the TUI")
	}
}

// TestQuitConfirmFundingBusy: a funding action on the wire also arms the soft
// quit, and the dialog names the funding request.
func TestQuitConfirmFundingBusy(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m.funding.actionInFlight = true
	m, _ = press(t, m, k('q', "q"))
	if m.mode != modeQuitConfirm {
		t.Fatalf("q with a funding action in flight must ask first, mode=%v", m.mode)
	}
	if frame := plain(m.render()); !strings.Contains(frame, "funding request") {
		t.Fatalf("the dialog must name the funding request:\n%s", frame)
	}
}

// TestEscNeverQuitsAndQClosesNoOverlay: esc is strictly "one level back" (a
// dead key at the top level), and q closes no overlay — help/notices/chart
// dismiss on esc/enter (plus the same key that opened them), never on q.
func TestEscNeverQuitsAndQClosesNoOverlay(t *testing.T) {
	m := testModel(t, false, nil)
	var cmd tea.Cmd
	m, cmd = press(t, m, special(tea.KeyEscape))
	if cmd != nil || m.mode != modeNormal {
		t.Fatal("esc at the top level must do nothing")
	}
	for _, tc := range []struct {
		open  tea.KeyPressMsg
		mode  mode
		cross tea.KeyPressMsg // the OTHER overlay's toggle key — must not close
	}{
		{k('?', "?"), modeHelp, k('n', "n")},
		{k('n', "n"), modeNotices, k('?', "?")},
	} {
		m, _ = press(t, m, tc.open)
		if m.mode != tc.mode {
			t.Fatalf("overlay should open, mode=%v want %v", m.mode, tc.mode)
		}
		m, cmd = press(t, m, k('q', "q"))
		if m.mode != tc.mode || cmd != nil {
			t.Fatalf("q must not close (or quit through) the overlay, mode=%v", m.mode)
		}
		m, _ = press(t, m, tc.cross)
		if m.mode != tc.mode {
			t.Fatalf("the other overlay's key must not cross-close, mode=%v", m.mode)
		}
		m, _ = press(t, m, tc.open) // same-key toggle closes
		if m.mode != modeNormal {
			t.Fatalf("pressing the opener again must close, mode=%v", m.mode)
		}
	}
}

// --- order entry (the docked order panel) ---

// seedOrderMarket feeds a live orderbook and balances for btc_krw so the
// panel's freshness gate opens and the presets can size: best bid
// 100,000,000, best ask 100,010,000; 1,000,000 KRW and 0.5 BTC available.
func seedOrderMarket(t *testing.T, m model) model {
	t.Helper()
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 101, "", `{
		"data":{"timestamp":99,
		"asks":[{"price":"100010000","qty":"1"},{"price":"100020000","qty":"2"}],
		"bids":[{"price":"100000000","qty":"1"},{"price":"99990000","qty":"2"}]}}`))
	return feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"1000000","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"0.5","available":"0.5","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
}

// TestPlaceRegistersLocalHold: dispatching an order registers a local balance
// hold under its minted clientOrderId (Available drops by the estimated
// reservation before the request is even sent), the hold is released when the
// order shows up on the myOrder channel, and a failed place releases it
// immediately.
func TestPlaceRegistersLocalHold(t *testing.T) {
	tr := &fakeTrader{result: PlaceResult{OrderID: "9001", ClientOrderID: "unused"}}
	m := seedOrderMarket(t, testModel(t, true, tr))

	availKRW := func() string {
		t.Helper()
		for _, b := range m.store.BalancesFor(1) {
			if b.Currency == "krw" {
				return b.Available
			}
		}
		t.Fatal("no krw balance")
		return ""
	}

	// Dispatch a 300,000 KRW limit buy; the hold lands before the trader runs.
	form := OrderForm{Symbol: "btc_krw", Side: "buy", Type: "limit",
		Price: "100000000", Qty: "0.003", TIF: "gtc"}
	mm, cmd := m.dispatchPlaceForm(form, placeFromBar)
	m = mm.(model)
	if got := availKRW(); got != "700000" {
		t.Fatalf("available after dispatch = %s, want 700000 (hold registered)", got)
	}
	sent := tr.tickets // not yet called
	if len(sent) != 0 {
		t.Fatal("hold must be registered before the trader runs")
	}
	msg := cmd().(placeDoneMsg)
	if msg.clientOrderID == "" || msg.clientOrderID != tr.tickets[0].ClientOrderID {
		t.Fatalf("placeDoneMsg must carry the dispatched clientOrderId, got %q", msg.clientOrderID)
	}
	mm, _ = m.Update(msg)
	m = mm.(model)
	// Accepted: the hold stands until the myOrder event lands…
	if got := availKRW(); got != "700000" {
		t.Fatalf("available after accept = %s, want 700000 (hold kept)", got)
	}
	// …and releases on observation.
	m = feed(t, m, dataEvent("myOrder", "btc_krw", stream.OriginRealtime, 200, "",
		`{"channelType":"myOrder","order":{"accountSeq":1,"orders":[
			{"orderId":9001,"clientOrderId":"`+msg.clientOrderID+`","side":"buy","status":"unfilled","price":"100000000","qty":"0.003"}]}}`))
	if got := availKRW(); got != "1000000" {
		t.Fatalf("available after myOrder = %s, want 1000000 (hold released)", got)
	}

	// A rejected place releases its hold on the spot.
	tr.placeErr = errors.New("NO_BALANCE")
	mm, cmd = m.dispatchPlaceForm(form, placeFromBar)
	m = mm.(model)
	if got := availKRW(); got != "700000" {
		t.Fatalf("available after second dispatch = %s, want 700000", got)
	}
	mm, _ = m.Update(cmd())
	m = mm.(model)
	if got := availKRW(); got != "1000000" {
		t.Fatalf("available after rejection = %s, want 1000000 (hold released)", got)
	}
}

// TestOrderFeeGateAndHoldHeadroom: with a Fees seam wired, arming is gated
// until the active {account, symbol} fee policy has loaded (the fetch is
// async and the gate shows why), and once loaded a quote-fee buy's local
// hold reserves the maxFeeRate headroom on top of the notional.
func TestOrderFeeGateAndHoldHeadroom(t *testing.T) {
	tr := &fakeTrader{result: PlaceResult{OrderID: "9001", ClientOrderID: "unused"}}
	stopped := false
	m := newModel(Config{
		Symbols: []string{"btc_krw", "eth_krw"},
		Private: true,
		Trader:  tr,
		Fees: func(symbol string, accountSeq int) (FeeRates, error) {
			return FeeRates{MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw", SellFeeCurrency: "btc"}, nil
		},
		KeyName:     "testkey",
		BaseURL:     "http://127.0.0.1:9999",
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() { stopped = true },
	})
	_ = stopped
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))

	// The fee policy has not loaded yet: the gate blocks arming with a reason.
	if gate := m.orderGate(); gate == "" || !strings.Contains(gate, "fee policy") {
		t.Fatalf("arming must be gated on the fee policy, got %q", gate)
	}
	// The (async) reply lands; the gate clears.
	mm, _ = m.Update(orderFeesMsg{symbol: "btc_krw", accountSeq: 1, fees: FeeRates{
		MakerRate: "0.001", TakerRate: "0.002", MaxRate: "0.002", BuyFeeCurrency: "krw", SellFeeCurrency: "btc"}})
	m = mm.(model)
	if gate := m.orderGate(); gate != "" {
		t.Fatalf("gate must clear once fees are cached, got %q", gate)
	}

	// A quote-fee buy holds notional*(1+maxFeeRate): 100,000,000 x 0.003 x 1.002.
	form := OrderForm{Symbol: "btc_krw", Side: "buy", Type: "limit",
		Price: "100000000", Qty: "0.003", TIF: "gtc"}
	mm, _ = m.dispatchPlaceForm(form, placeFromBar)
	m = mm.(model)
	for _, b := range m.store.BalancesFor(1) {
		if b.Currency == "krw" && b.Available != "699400" {
			t.Fatalf("available with fee-headroom hold = %s, want 699400", b.Available)
		}
	}
}

// TestPlaceOrderAvailableWithTrader: order entry is available whenever a Trader
// is wired (private mode) — 'b' opens the panel and the footer/help advertise
// it. There is no experimental opt-in; the only gate left is public mode.
func TestPlaceOrderAvailableWithTrader(t *testing.T) {
	m := newModel(Config{
		Symbols:     []string{"btc_krw", "eth_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		KeyName:     "testkey",
		BaseURL:     "http://127.0.0.1:9999",
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = mm.(model)

	// The main-screen footer and help advertise order entry (no opt-in needed).
	if help := plain(m.renderFooter()); !strings.Contains(help, "b:buy") || !strings.Contains(help, "s:sell") || !strings.Contains(help, ":cmd") {
		t.Fatalf("footer must advertise order entry with a Trader wired: %q", help)
	}
	if !strings.Contains(plain(m.renderHelp()), "order panel") {
		t.Fatal("help must list the order-panel key with a Trader wired")
	}

	// And 'b' opens the panel.
	m, _ = press(t, m, k('b', "b"))
	if m.mode != modeOrder {
		t.Fatalf("'b' must open the order panel when a Trader is wired, mode=%v", m.mode)
	}
}

// TestOrderPanelPlacesLimitOrder: the golden path — open, size, review, place.
// The price is pre-seeded from the best bid; the field cursor starts on the
// size row; enter arms (the in-place review), enter places; success keeps the
// panel open (sticky) and flashes the accepted order.
func TestOrderPanelPlacesLimitOrder(t *testing.T) {
	tr := &fakeTrader{result: PlaceResult{OrderID: "424242", ClientOrderID: "cid-42"}}
	m := seedOrderMarket(t, testModel(t, true, tr))

	m, _ = press(t, m, k('b', "b"))
	if m.mode != modeOrder {
		t.Fatal("o must open order mode")
	}
	if m.order.draft.price != "100000000" {
		t.Fatalf("a buy panel must seed the price from the best bid, got %q", m.order.draft.price)
	}
	if m.order.curField() != ofQty {
		t.Fatalf("the field cursor should start on the size row, got %v", m.order.curField())
	}
	m = typeText(t, m, "0.5")
	m, cmd := press(t, m, special(tea.KeyEnter)) // arm
	if m.order.view != orderConfirm || cmd != nil {
		t.Fatalf("enter must arm the review without dispatching, view=%v", m.order.view)
	}
	m, cmd = press(t, m, special(tea.KeyEnter)) // place
	if cmd == nil || !m.orderInFlight || m.order.view != orderBusy {
		t.Fatalf("the second enter must dispatch: inFlight=%v view=%v", m.orderInFlight, m.order.view)
	}

	msg := cmd() // runs the fake trader synchronously
	if len(tr.tickets) != 1 {
		t.Fatalf("trader not called: %+v", tr.tickets)
	}
	want := OrderForm{Symbol: "btc_krw", Side: "buy", Type: "limit", Price: "100000000", Qty: "0.5", TIF: "gtc", AccountSeq: 1}
	assertPlacedForm(t, tr.tickets[0], want)

	mm, _ := m.Update(msg)
	m = mm.(model)
	if m.mode != modeOrder || m.order.view != orderForm {
		t.Fatalf("success must return to the (sticky) form: mode=%v view=%v", m.mode, m.order.view)
	}
	if !strings.Contains(m.toast.text, "424242") || !strings.Contains(m.toast.text, "cid-42") {
		t.Fatalf("toast must echo orderId and clientOrderId: %q", m.toast.text)
	}
	if m.flashOrderID != 424242 {
		t.Fatalf("the accepted order should flash in the open-orders panel, got %d", m.flashOrderID)
	}
}

// TestOrderPanelMarketBuyCollectsAmtOnly: toggling to a market buy swaps the
// size field to amt, and the placed form carries amt + price protection (on
// by default for market orders) and no price/qty.
func TestOrderPanelMarketBuyCollectsAmtOnly(t *testing.T) {
	tr := &fakeTrader{}
	m := seedOrderMarket(t, testModel(t, true, tr))
	m, _ = press(t, m, k('b', "b"))
	// Cursor starts on qty; up twice lands on the type row (qty ← price ← type).
	m, _ = press(t, m, special(tea.KeyUp), special(tea.KeyUp), special(tea.KeyRight))
	if m.order.draft.typ != "market" {
		t.Fatalf("right on type must toggle to market, got %s", m.order.draft.typ)
	}
	// Fields now: side, type, amt, tif, pp, button; down moves onto amt.
	m, _ = press(t, m, special(tea.KeyDown))
	if m.order.curField() != ofAmt {
		t.Fatalf("a market buy should offer the amt row, got %v", m.order.curField())
	}
	m = typeText(t, m, "10000")
	m, _ = press(t, m, special(tea.KeyEnter))
	_, cmd := press(t, m, special(tea.KeyEnter))
	cmd()
	want := OrderForm{Symbol: "btc_krw", Side: "buy", Type: "market", Amt: "10000", PP: true, TIF: "ioc", AccountSeq: 1}
	assertPlacedForm(t, tr.tickets[0], want)
}

// TestOrderPanelTifMandatoryAndModeSwitch: the tif is always set — a limit
// order defaults to gtc and cycles the explicit choices, a market order is
// ioc-only and cannot be cycled. Flipping the type carries the limit-mode
// selection across untouched: market's forced ioc never overwrites it, so
// returning to limit restores the chosen limit tif.
func TestOrderPanelTifMandatoryAndModeSwitch(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('b', "b"))
	o := m.order

	if got := o.draft.tif(); got != "gtc" {
		t.Fatalf("a fresh limit order must default to gtc, got %q", got)
	}

	// Cycle the limit tif off its default to a non-ioc choice.
	o = o.moveCursorTo(ofTIF)
	o = o.adjustField(true) // gtc -> ioc
	o = o.adjustField(true) // ioc -> fok
	if got := o.draft.tif(); got != "fok" {
		t.Fatalf("limit tif must cycle to fok, got %q", got)
	}

	// Flip to a market order: tif is fixed to ioc, and cycling it is a no-op.
	o = o.moveCursorTo(ofType)
	o = o.adjustField(true)
	if got := o.draft.typ; got != "market" {
		t.Fatalf("type must toggle to market, got %q", got)
	}
	if got := o.draft.tif(); got != "ioc" {
		t.Fatalf("a market order must be ioc, got %q", got)
	}
	o = o.moveCursorTo(ofTIF)
	o.formErr = "enter the amount (% cycles balance presets)"
	o = o.adjustField(true)
	if got := o.draft.tif(); got != "ioc" {
		t.Fatalf("a market order's tif must stay ioc (ioc-only), got %q", got)
	}
	if o.formErr != "" {
		// ←/→ on the market tif row clears a stale inline error, like every other row.
		t.Fatalf("adjusting the market tif row must clear the form error, got %q", o.formErr)
	}

	// Flip back to limit: the limit selection (fok) is restored, not ioc.
	o = o.moveCursorTo(ofType)
	o = o.adjustField(true)
	if got := o.draft.tif(); got != "fok" {
		t.Fatalf("returning to limit must restore the limit tif fok, not ioc, got %q", got)
	}
}

// TestOrderPanelSellOpenAndSideToggle: 's' opens a sell panel seeded from the
// best ask; ←/→ on the side row toggles, re-seeding the price to the new
// side's best (a buy price must not survive into a sell form, and vice versa).
func TestOrderPanelSellOpenAndSideToggle(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('s', "s"))
	if m.order.draft.side != "sell" {
		t.Fatal("s must open a sell panel")
	}
	if m.order.draft.price != "100010000" {
		t.Fatalf("a sell panel must seed the price from the best ask, got %q", m.order.draft.price)
	}
	// Up to the side row (qty ← price ← type ← side), then toggle.
	m, _ = press(t, m, special(tea.KeyUp), special(tea.KeyUp), special(tea.KeyUp), special(tea.KeyLeft))
	if m.order.draft.side != "buy" {
		t.Fatal("left/right on the side row must toggle")
	}
	if m.order.draft.price != "100000000" {
		t.Fatalf("a side flip must re-seed the price to the new side's best, got %q", m.order.draft.price)
	}
}

// TestOrderPanelSideKeys: inside the panel b/s declare the side — a flip
// re-seeds the price to the new side's best, and the current side's key
// re-anchors there (join) after the price wandered.
func TestOrderPanelSideKeys(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('b', "b")) // buy panel, price = best bid
	m, _ = press(t, m, k('s', "s"))
	if m.order.draft.side != "sell" || m.order.draft.price != "100010000" {
		t.Fatalf("s must flip to sell and re-seed at the best ask, got side=%q price=%q",
			m.order.draft.side, m.order.draft.price)
	}
	// Wander (down one book level), then re-join the ask with the side's key.
	m, _ = press(t, m, k('j', "j"))
	if m.order.draft.price == "100010000" {
		t.Fatal("j should have moved the price off the best ask")
	}
	m, _ = press(t, m, k('s', "s"))
	if m.order.draft.side != "sell" || m.order.draft.price != "100010000" {
		t.Fatalf("s on a sell form must re-anchor to the best ask, got side=%q price=%q",
			m.order.draft.side, m.order.draft.price)
	}
	// Aggress: for a sell, a jumps to the opposite touch (the best bid).
	m, _ = press(t, m, k('a', "a"))
	if m.order.draft.price != "100000000" {
		t.Fatalf("a on a sell form must anchor to the best bid (cross), got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('b', "b"))
	if m.order.draft.side != "buy" || m.order.draft.price != "100000000" {
		t.Fatalf("b must flip back to buy at the best bid, got side=%q price=%q",
			m.order.draft.side, m.order.draft.price)
	}
}

// TestOKeyIsUnbound: 'o' is not an order key (nor anything else) in normal mode.
func TestOKeyIsUnbound(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('o', "o"))
	if m.mode != modeNormal {
		t.Fatalf("'o' must do nothing in normal mode, got mode %v", m.mode)
	}
}

// TestOrderPanelFreshnessGateBlocksArm: without a live book the review cannot
// be armed — the gate reason lands inline and nothing dispatches.
func TestOrderPanelFreshnessGateBlocksArm(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr) // no book seeded: OrderbookReady is false
	m, _ = press(t, m, k('b', "b"))
	m = typeText(t, m, "0.5")
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.order.view != orderForm {
		t.Fatalf("arming against a not-ready book must be refused, view=%v", m.order.view)
	}
	if !strings.Contains(m.order.formErr, "orderbook is not live") {
		t.Fatalf("the gate reason must show inline: %q", m.order.formErr)
	}
	if len(tr.tickets) != 0 {
		t.Fatal("nothing may dispatch against a stale book")
	}
}

// TestOrderPanelLocalValidation: a missing size is caught locally (no
// round-trip), inline.
func TestOrderPanelLocalValidation(t *testing.T) {
	tr := &fakeTrader{}
	m := seedOrderMarket(t, testModel(t, true, tr))
	m, _ = press(t, m, k('b', "b"))
	m, _ = press(t, m, special(tea.KeyEnter)) // no qty typed
	if m.order.view != orderForm || len(tr.tickets) != 0 {
		t.Fatalf("an empty size must not arm/dispatch, view=%v", m.order.view)
	}
	if !strings.Contains(m.order.formErr, "quantity") {
		t.Fatalf("the validation must show inline: %q", m.order.formErr)
	}
}

// TestOrderPanelRejectionStaysInline: both a validation error and an API
// rejection land inline on the form (inputs preserved) — fix and re-arm.
func TestOrderPanelRejectionStaysInline(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"usage", &output.UsageError{Message: "--qty must be a decimal"}, "--qty must be a decimal"},
		{"api", &output.ApiError{Message: "insufficient balance", Code: "NOT_ENOUGH_BALANCE", HTTPStatus: 400}, "NOT_ENOUGH_BALANCE"},
	} {
		tr := &fakeTrader{placeErr: c.err}
		m := seedOrderMarket(t, testModel(t, true, tr))
		m, _ = press(t, m, k('b', "b"))
		m = typeText(t, m, "0.5")
		m, _ = press(t, m, special(tea.KeyEnter))
		m, cmd := press(t, m, special(tea.KeyEnter))
		mm, _ := m.Update(cmd())
		m = mm.(model)
		if m.mode != modeOrder || m.order.view != orderForm {
			t.Fatalf("%s: a rejection must return to the form, mode=%v view=%v", c.name, m.mode, m.order.view)
		}
		if !strings.Contains(m.order.formErr, c.want) {
			t.Fatalf("%s: the rejection must show inline: %q", c.name, m.order.formErr)
		}
		if m.orderInFlight {
			t.Fatalf("%s: the in-flight gate must clear", c.name)
		}
		if m.order.draft.qty != "0.5" {
			t.Fatalf("%s: inputs must be preserved for a fix-and-retry, qty=%q", c.name, m.order.draft.qty)
		}
	}
}

// TestOrderPanelTickStepAnchorsAndLadder: with the tick-size policy loaded,
// ←/→ on the price row steps by one tick, the anchors put the price on the
// live book (on a buy: a = the opposite touch, b = re-join the bid), and j/k
// walk the book's visible levels.
func TestOrderPanelTickStepAnchorsAndLadder(t *testing.T) {
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{Bands: []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))

	m2, cmd := sendC(t, m, k('b', "b"))
	m = m2
	if cmd == nil {
		t.Fatal("opening must kick off the tick-policy fetch")
	}
	m = drainCmds(t, m, cmd) // lands orderBandsMsg
	if len(m.order.symBands()) == 0 {
		t.Fatal("the tick bands should be cached after the fetch")
	}

	// Onto the price row (qty ← price), then step with the bracket keys.
	m, _ = press(t, m, special(tea.KeyUp))
	if m.order.curField() != ofPrice {
		t.Fatalf("expected the price row, got %v", m.order.curField())
	}
	m, _ = press(t, m, k(']', "]"))
	if m.order.draft.price != "100001000" {
		t.Fatalf("] must step +1 tick, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('[', "["), k('[', "["))
	if m.order.draft.price != "99999000" {
		t.Fatalf("[ must step -1 tick, got %q", m.order.draft.price)
	}

	// Anchors (side = buy): a crosses to the ask, m mids, b re-joins the bid.
	m, _ = press(t, m, k('a', "a"))
	if m.order.draft.price != "100010000" {
		t.Fatalf("a on a buy must anchor to the best ask (cross), got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('m', "m"))
	if m.order.draft.price != "100005000" {
		t.Fatalf("m must anchor to the (snapped) mid, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('b', "b"))
	if m.order.draft.side != "buy" || m.order.draft.price != "100000000" {
		t.Fatalf("b on a buy must re-join the best bid, got side=%q price=%q",
			m.order.draft.side, m.order.draft.price)
	}

	// The ladder: j walks down the book (lower price), k back up across the mid.
	m, _ = press(t, m, k('j', "j"))
	if m.order.draft.price != "99990000" {
		t.Fatalf("j must step down to the next level, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('k', "k"), k('k', "k"))
	if m.order.draft.price != "100010000" {
		t.Fatalf("k twice must cross the mid onto the best ask, got %q", m.order.draft.price)
	}
}

// TestOrderPanelNumberLegibility: the panel's display formatting — a blurred
// price input renders grouped, KRW figures render whole-won, the notional
// holds a row of its own on the form and gets its "=" line on the confirm —
// while the draft floors the size to the request precision (the raw text stays
// in the input).
func TestOrderPanelNumberLegibility(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('b', "b")) // buy panel: price 100,000,000, cursor on qty
	m = typeText(t, m, "0.123456789")

	panel := func() string {
		var lines []string
		for _, l := range m.order.panelLines(40, "") {
			lines = append(lines, plain(l.text))
		}
		return strings.Join(lines, "\n")
	}
	body := panel()
	if !strings.Contains(body, "100,000,000") {
		t.Fatalf("a blurred price input must render grouped:\n%s", body)
	}
	if !strings.Contains(body, "notional 12,345,678 KRW") || strings.Contains(body, "12,345,678.") {
		t.Fatalf("the notional must render whole-won on its own row:\n%s", body)
	}
	if !strings.Contains(body, "vs mid") {
		t.Fatalf("the mid distance must keep its own row:\n%s", body)
	}
	if m.order.draft.price != "100000000" || m.order.draft.qty != "0.12345678" {
		t.Fatalf("the draft must floor the size to the request precision, got price=%q qty=%q",
			m.order.draft.price, m.order.draft.qty)
	}
	if got := m.order.qty.Value(); got != "0.123456789" {
		t.Fatalf("the input keeps the raw typed text, got %q", got)
	}

	// The avail line leads each figure with its unit: the line tail-truncates,
	// and a clipped trailing unit misreads ("…961 K" looks like thousands).
	if !strings.Contains(body, "KRW 1,000,000 · BTC 0.5") {
		t.Fatalf("the avail line must be unit-first:\n%s", body)
	}

	m, _ = press(t, m, special(tea.KeyEnter)) // arm
	if m.order.view != orderConfirm {
		t.Fatalf("expected the confirm view, got %v", m.order.view)
	}
	if body := panel(); !strings.Contains(body, "= 12,345,678 KRW") {
		t.Fatalf("the confirm must give the whole-won notional its own line:\n%s", body)
	}
}

// TestOrderPanelSizeUnitToggle: `u` flips a limit order between quantity and
// amount entry, carrying the size across at the price; a market order's unit is
// fixed, so `u` there is a no-op.
func TestOrderPanelSizeUnitToggle(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('b', "b")) // buy limit; price seeded to best bid 100,000,000
	m = typeText(t, m, "0.05")      // quantity
	if m.order.draft.sizeInAmt {
		t.Fatal("a limit order starts in quantity mode")
	}

	m, _ = press(t, m, k('u', "u")) // → amount mode: 0.05 × 100,000,000 = 5,000,000
	if !m.order.draft.sizeInAmt || m.order.draft.amt != "5000000" {
		t.Fatalf("u must carry the size into amount mode, got amtMode=%v amt=%q", m.order.draft.sizeInAmt, m.order.draft.amt)
	}
	if !m.order.draft.inputAmt() {
		t.Fatal("the amount field must be the active size input")
	}

	m, _ = press(t, m, k('u', "u")) // back to quantity: derived qty carries
	if m.order.draft.sizeInAmt || m.order.draft.qty != "0.05" {
		t.Fatalf("u must carry the size back to quantity, got amtMode=%v qty=%q", m.order.draft.sizeInAmt, m.order.draft.qty)
	}

	// With the price cleared, a qty→amt→qty round trip must not wipe the qty:
	// the amount can't derive a quantity, so the existing one is kept.
	m, _ = press(t, m, k('u', "u")) // → amount mode
	m.order.price.SetValue("")      // user clears the price
	m.order.draft.price = ""
	m, _ = press(t, m, k('u', "u")) // → quantity mode with no price
	if m.order.draft.qty != "0.05" {
		t.Fatalf("toggling without a price must keep the qty, got %q", m.order.draft.qty)
	}

	// On a market order the unit is fixed by the side — `u` does nothing.
	m, _ = press(t, m, special(tea.KeyUp), special(tea.KeyUp)) // cursor: qty → price → type
	m, _ = press(t, m, special(tea.KeyRight))                  // type → market
	if m.order.draft.typ != "market" {
		t.Fatalf("expected a market order, got %q", m.order.draft.typ)
	}
	before := m.order.draft.sizeInAmt
	m, _ = press(t, m, k('u', "u"))
	if m.order.draft.sizeInAmt != before {
		t.Fatal("u must be a no-op on a market order")
	}
}

// TestOrderPanelOffGridPriceRejected: a typed price off the tick grid is
// checked, not silently snapped — arming refuses with the tick size, and the
// price the user typed is left untouched.
func TestOrderPanelOffGridPriceRejected(t *testing.T) {
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{Bands: []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m2, cmd := sendC(t, m, k('b', "b"))
	m = drainCmds(t, m2, cmd) // lands the tick bands
	if len(m.order.symBands()) == 0 {
		t.Fatal("tick bands must be loaded for the off-grid check")
	}

	m = typeText(t, m, "0.05")             // qty (cursor starts on the size row)
	m, _ = press(t, m, special(tea.KeyUp)) // size → price row
	m.order.price.SetValue("")             // clear the seeded price
	m = typeText(t, m, "100000500")        // type an off-grid price (tick 1000)
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.order.view == orderConfirm {
		t.Fatal("an off-grid typed price must not arm")
	}
	if !strings.Contains(m.order.formErr, "off the tick grid") {
		t.Fatalf("the off-grid price must be reported: %q", m.order.formErr)
	}
	if m.order.draft.price != "100000500" {
		t.Fatalf("the typed price must be left untouched, got %q", m.order.draft.price)
	}
}

// TestOrderPanelConfirmClickValidates: the [ enter place ] click goes through
// the same sizing-matrix guard as Enter, so an armed order that became invalid
// cannot be placed by clicking (regression: the mouse path once bypassed it).
func TestOrderPanelConfirmClickValidates(t *testing.T) {
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{Bands: []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m2, cmd := sendC(t, m, k('b', "b"))
	m = drainCmds(t, m2, cmd)
	m = typeText(t, m, "0.05")
	m, _ = press(t, m, special(tea.KeyEnter)) // arm
	if m.order.view != orderConfirm {
		t.Fatalf("expected the confirm view, err=%q", m.order.formErr)
	}

	// Force the armed draft invalid (a limit with no price). Enter refuses this;
	// the [ enter place ] click must refuse it too via the shared guard.
	m.order.draft.price = ""
	inner := 40
	row, x := -1, 0
	for i, l := range m.order.panelLines(inner, "") {
		for _, sp := range l.spans {
			if sp.kind == spanOrdConfirmY {
				row, x = i, sp.x0
			}
		}
	}
	if row < 0 {
		t.Fatal("the confirm view must render an [ enter place ] click target")
	}
	o, act := m.order.panelClick(row, x, inner, "")
	if act == orderActPlace || o.view == orderBusy {
		t.Fatal("clicking place on an invalid armed order must not dispatch")
	}
	if !strings.Contains(o.formErr, "requires a price") {
		t.Fatalf("the refused click must report why: %q", o.formErr)
	}
}

// TestOrderPanelConfirmShowsExactAmount: the confirm view shows the exact
// committed amount (grouped, not sub-unit-trimmed) — a fractional-KRW amount
// must not review as 0 while the wire carries the real value.
func TestOrderPanelConfirmShowsExactAmount(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('b', "b"))
	m, _ = press(t, m, special(tea.KeyUp), special(tea.KeyUp)) // qty → price → type
	m, _ = press(t, m, special(tea.KeyRight))                  // type → market (buy sizes in amt)
	if m.order.draft.typ != "market" {
		t.Fatalf("expected a market order, got %q", m.order.draft.typ)
	}
	m, _ = press(t, m, special(tea.KeyDown)) // type → amount row
	m = typeText(t, m, "0.5")                // 0.5 KRW (valid at 8 dp; below min-notional but not blocked)
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.order.view != orderConfirm {
		t.Fatalf("expected the confirm view, err=%q", m.order.formErr)
	}
	var body strings.Builder
	for _, l := range m.order.panelLines(40, "") {
		body.WriteString(plain(l.text) + "\n")
	}
	if !strings.Contains(body.String(), "BUY 0.5 KRW") {
		t.Fatalf("the confirm must show the exact committed amount:\n%s", body.String())
	}
}

// TestOrderPanelCompactFallback: a derived figure that cannot fit its line
// exact renders compact ("9.00B KRW") — never a clipped digit string wearing
// fake precision, and never a figure separated from its unit.
func TestOrderPanelCompactFallback(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 102, "/v2/balance",
		`[{"currency":"krw","balance":"9621139961","available":"9621139961","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	m, _ = press(t, m, k('b', "b"))
	m = typeText(t, m, "90") // 90 BTC at 100,000,000 → 9B KRW notional

	var lines []string
	for _, l := range m.order.panelLines(24, "") { // a narrow panel
		lines = append(lines, plain(l.text))
	}
	body := strings.Join(lines, "\n")
	if !strings.Contains(body, "notional 9.00B KRW") {
		t.Fatalf("an over-wide notional must compact with its unit:\n%s", body)
	}
	if !strings.Contains(body, "KRW 9.62B") {
		t.Fatalf("an over-wide avail figure must compact, unit first:\n%s", body)
	}
}

// TestOrderPanelPresetKeys: % cycles the %-of-balance presets forward, wrapping
// (fee headroom untested here — no fee rates are loaded); typing a size detaches.
func TestOrderPanelPresetKeys(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('b', "b")) // cursor on qty; price = 100,000,000
	// The % key cue is rendered beside the chips so the shortcut is discoverable.
	if pl := plain(m.order.presetLine().text); !strings.Contains(pl, "%: 10%") {
		t.Fatalf("preset line must lead with the %% cue, got %q", pl)
	}
	m, _ = press(t, m, k('%', "%"))
	if m.order.draft.qty != "0.001" { // 10% × 1,000,000 / 100,000,000
		t.Fatalf("%% must apply the first preset, got %q", m.order.draft.qty)
	}
	m, _ = press(t, m, k('%', "%"))
	if m.order.draft.qty != "0.0025" { // 25%
		t.Fatalf("%% again must apply the next preset, got %q", m.order.draft.qty)
	}
	// % cycles forward and wraps — pressing it once per preset returns to where
	// it started (a clamp would stick at the last preset instead).
	start := m.order.presetIdx
	for range m.order.sizeLevels {
		m, _ = press(t, m, k('%', "%"))
	}
	if m.order.presetIdx != start {
		t.Fatalf("%% must wrap the preset ring back to %d, got %d", start, m.order.presetIdx)
	}
	m = typeText(t, m, "9")
	if m.order.presetIdx != -1 {
		t.Fatal("typing must detach the preset (custom size)")
	}
}

// TestOrderPanelArrowsMoveCaret: on a numeric input row ←/→ move the text caret
// and leave the value untouched (the value steps on [ ]{ } for price, % for size).
func TestOrderPanelArrowsMoveCaret(t *testing.T) {
	m := seedOrderMarket(t, testModel(t, true, &fakeTrader{}))
	m, _ = press(t, m, k('b', "b")) // limit buy

	// Price row: a known value + caret, then walk it with ←/→.
	m.order = m.order.moveCursorTo(ofPrice)
	m.order.price.SetValue("12345")
	m.order.price.SetCursor(5)
	m, _ = press(t, m, special(tea.KeyLeft), special(tea.KeyLeft))
	if p := m.order.price.Position(); p != 3 {
		t.Fatalf("←← must move the price caret to 3, got %d", p)
	}
	m, _ = press(t, m, special(tea.KeyRight))
	if p := m.order.price.Position(); p != 4 {
		t.Fatalf("→ must move the price caret to 4, got %d", p)
	}
	if v := m.order.price.Value(); v != "12345" {
		t.Fatalf("←/→ must not change the price value, got %q", v)
	}

	// Quantity row: same contract on a different numeric input.
	m.order = m.order.moveCursorTo(ofQty)
	m.order.qty.SetValue("0.5")
	m.order.qty.SetCursor(3)
	m, _ = press(t, m, special(tea.KeyLeft))
	if p := m.order.qty.Position(); p != 2 {
		t.Fatalf("← must move the qty caret to 2, got %d", p)
	}
	if v := m.order.qty.Value(); v != "0.5" {
		t.Fatalf("← must not change the qty value, got %q", v)
	}
}

// TestOrderPanelHintKeyClicks: clicking a hinted key-cap presses that key — [
// steps the price a tick, u toggles the size unit, % cycles the presets.
func TestOrderPanelHintKeyClicks(t *testing.T) {
	m := newModel(Config{
		Symbols: []string{"btc_krw"}, Private: true, Trader: &fakeTrader{},
		Now: func() int64 { return 1_700_000_000_000 }, StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{Bands: []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m2, cmd := sendC(t, m, k('b', "b"))
	m = drainCmds(t, m2, cmd) // price seeds 100,000,000; tick bands load

	// Click the "[" cap → step the price down one tick.
	col, row, line, ok := orderPanelLineScreen(m, ":±1")
	if !ok {
		t.Fatal("the panel should render the price key hint")
	}
	m = send(t, m, mclick(col+glyphCol(line, "["), row))
	if m.order.draft.price != "99999000" {
		t.Fatalf("clicking [ should step -1 tick, got %q", m.order.draft.price)
	}

	// Click the "u" cap → switch to amount entry.
	col, row, line, ok = orderPanelLineScreen(m, "u:")
	if !ok {
		t.Fatal("the panel should render the u-toggle hint")
	}
	m = send(t, m, mclick(col+glyphCol(line, "u"), row))
	if !m.order.draft.sizeInAmt {
		t.Fatal("clicking u should switch to amount entry")
	}

	// Click the "%" cap → arm the first preset.
	col, row, line, ok = orderPanelLineScreen(m, "%:")
	if !ok {
		t.Fatal("the panel should render the % preset cue")
	}
	m = send(t, m, mclick(col+glyphCol(line, "%"), row))
	if m.order.presetIdx != 0 {
		t.Fatalf("clicking %% should arm the first preset, got idx %d", m.order.presetIdx)
	}
}

// TestOrderPanelConfirmNudgeAndBack: while armed, [ ]{ } nudge the price on the
// tick grid and ←/→ do nothing; esc returns to the form with everything intact.
func TestOrderPanelConfirmNudgeAndBack(t *testing.T) {
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{Bands: []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m2, cmd := sendC(t, m, k('b', "b"))
	m = drainCmds(t, m2, cmd)

	m = typeText(t, m, "0.5")
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.order.view != orderConfirm {
		t.Fatalf("expected the armed review, got %v", m.order.view)
	}
	m, _ = press(t, m, k(']', "]"))
	if m.order.draft.price != "100001000" {
		t.Fatalf("] while armed must nudge +1 tick, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('}', "}"))
	if m.order.draft.price != "100011000" {
		t.Fatalf("} while armed must nudge +10 ticks, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('{', "{"))
	if m.order.draft.price != "100001000" {
		t.Fatalf("{ while armed must nudge -10 ticks, got %q", m.order.draft.price)
	}
	// ←/→ are the form's adjust-field keys; while armed they must do nothing.
	m, _ = press(t, m, special(tea.KeyRight))
	if m.order.draft.price != "100001000" {
		t.Fatalf("→ while armed must not nudge, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.order.view != orderForm || m.mode != modeOrder {
		t.Fatalf("esc must back out to the form, view=%v mode=%v", m.order.view, m.mode)
	}
	if m.order.draft.qty != "0.5" || m.order.draft.price != "100001000" {
		t.Fatalf("backing out must keep the draft, price=%q qty=%q", m.order.draft.price, m.order.draft.qty)
	}
}

// TestOrderPanelFormBracketNudge: [ ]/{ } step the price on the tick grid
// from the FORM too, regardless of which row holds the field cursor (the
// cursor opens on the size row).
func TestOrderPanelFormBracketNudge(t *testing.T) {
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{Bands: []ops.TickBand{{PriceGte: "0", TickSize: "1000"}}}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m2, cmd := sendC(t, m, k('b', "b")) // price seeds at the best bid, cursor on qty
	m = drainCmds(t, m2, cmd)
	if m.order.view != orderForm || m.order.draft.price != "100000000" {
		t.Fatalf("setup: view=%v price=%q", m.order.view, m.order.draft.price)
	}
	m, _ = press(t, m, k(']', "]"))
	if m.order.draft.price != "100001000" {
		t.Fatalf("] in the form must nudge +1 tick, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('}', "}"))
	if m.order.draft.price != "100011000" {
		t.Fatalf("} in the form must nudge +10 ticks, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('{', "{"))
	if m.order.draft.price != "100001000" {
		t.Fatalf("{ in the form must nudge -10 ticks, got %q", m.order.draft.price)
	}
	m, _ = press(t, m, k('[', "["))
	if m.order.draft.price != "100000000" {
		t.Fatalf("[ in the form must nudge -1 tick, got %q", m.order.draft.price)
	}
}

func TestPublicModeDisablesOrderEntry(t *testing.T) {
	m := testModel(t, false, nil)
	m, _ = press(t, m, k('b', "b"))
	if m.mode != modeNormal {
		t.Fatal("public mode must not open the order panel")
	}
	if !strings.Contains(m.toast.text, "public mode") {
		t.Fatalf("user must be told why: %q", m.toast.text)
	}
}

// --- cancel flow ---

func TestCancelFlowConfirmsAndCalls(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr)
	// An empty refresh first: an empty openOrders snapshot refreshes the table,
	// and an empty SetRows parks the bubbles table cursor at -1 — the selection
	// must recover once an order appears (regression: cancel was dead until then).
	m = feed(t, m, dataEvent("myOrder", "btc_krw", stream.OriginBackfill, 50, "/v2/openOrders", `[]`))
	m = seedOpenOrder(t, m)
	if len(m.orderIDs) != 1 {
		t.Fatalf("order table not refreshed: %+v", m.orderIDs)
	}

	m, _ = press(t, m, special(tea.KeyTab)) // focus the open-orders pane (cancel is gated on it)
	m, _ = press(t, m, k('x', "x"))
	if m.mode != modeConfirm || m.confirmOrderID != 777 {
		t.Fatalf("x must ask for confirmation: mode=%v id=%d", m.mode, m.confirmOrderID)
	}
	// esc keeps the order (y/n aliases are deliberately unbound).
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal || len(tr.cancels) != 0 {
		t.Fatal("esc must abort the cancel")
	}

	m, _ = press(t, m, k('x', "x"))
	m, _ = press(t, m, k('y', "y"))
	if m.mode != modeConfirm || len(tr.cancels) != 0 {
		t.Fatal("y must be a no-op on the confirm (enter confirms)")
	}
	m, cmd := press(t, m, special(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("enter must dispatch the cancel")
	}
	msg := cmd()
	if len(tr.cancels) != 1 || tr.cancels[0] != [3]string{"btc_krw", "777", "1"} {
		t.Fatalf("cancel call wrong: %+v", tr.cancels)
	}
	mm, _ := m.Update(msg)
	m = mm.(model)
	if m.toast.isError || !strings.Contains(m.toast.text, "777") {
		t.Fatalf("toast must report the accepted cancel: %+v", m.toast)
	}
	if m.cancelsInFlight[777] {
		t.Fatal("in-flight cancel must clear")
	}
}

// TestCancelGatedOnOrdersFocus: x cancels only with the open-orders pane
// focused; on the market selector it's an inert hint, never a confirm.
func TestCancelGatedOnOrdersFocus(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr)
	m = seedOpenOrder(t, m) // order 777, btc_krw; focus defaults to the market selector

	m, _ = press(t, m, k('x', "x"))
	if m.mode == modeConfirm {
		t.Fatal("x on the market pane must not start a cancel")
	}
	if !m.toast.isError {
		t.Fatalf("x on the market pane should hint how to cancel, got toast %+v", m.toast)
	}

	m, _ = press(t, m, special(tea.KeyTab)) // focus the open-orders pane
	m, _ = press(t, m, k('x', "x"))
	if m.mode != modeConfirm || m.confirmOrderID != 777 {
		t.Fatalf("x with the orders pane focused must confirm the cancel: mode=%v id=%d", m.mode, m.confirmOrderID)
	}
}

// TestClickSelectsOrderRow: a left click on an open-orders row focuses the pane
// and selects that order, so the user can click then cancel it (x).
func TestClickSelectsOrderRow(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr)
	rows := []string{
		`{"orderId":779,"status":"open","side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","createdAt":902,"clientOrderId":"c9"}`,
		`{"orderId":778,"status":"open","side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","createdAt":901,"clientOrderId":"c8"}`,
		`{"orderId":777,"status":"open","side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":"0","createdAt":900,"clientOrderId":"c7"}`,
	}
	m = feed(t, m, dataEvent("myOrder", "btc_krw", stream.OriginBackfill, 100, "/v2/openOrders",
		"["+strings.Join(rows, ",")+"]"))
	if len(m.orderIDs) != 3 || m.orderIDs[0] != 779 {
		t.Fatalf("expected 3 orders newest-first, got %v", m.orderIDs)
	}
	// Click the 2nd data row (order 778). At 120x32 the orders panel's first data
	// row is y=5 and the right column starts at x=82.
	m = send(t, m, mclick(90, 6))
	if m.focus != focusOrders {
		t.Fatalf("clicking an order row should focus the orders pane, got %v", m.focus)
	}
	if id, ok := m.selectedOrder(); !ok || id != 778 {
		t.Fatalf("clicking row 2 should select order 778, got %d (ok=%v)", id, ok)
	}
	m, _ = press(t, m, k('x', "x"))
	if m.mode != modeConfirm || m.confirmOrderID != 778 {
		t.Fatalf("x after clicking should confirm cancel of 778, got mode=%v id=%d", m.mode, m.confirmOrderID)
	}
}

// TestFooterHelpVariesByFocus: the keybind hint advertises cancel only when the
// open-orders pane is focused (the only place x is live).
func TestFooterHelpVariesByFocus(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	if help := plain(m.renderFooter()); strings.Contains(help, "x:cancel") {
		t.Fatalf("market focus should not advertise cancel: %q", help)
	}
	m, _ = press(t, m, special(tea.KeyTab)) // focus the open-orders pane
	if help := plain(m.renderFooter()); !strings.Contains(help, "x:cancel") {
		t.Fatalf("orders focus should advertise cancel: %q", help)
	}
}

// --- rendering ---

func TestNoSecondOrderWhileOneInFlight(t *testing.T) {
	tr := &fakeTrader{result: PlaceResult{OrderID: "1", ClientOrderID: "c"}}
	m := seedOrderMarket(t, testModel(t, true, tr))
	m = seedOpenOrder(t, m)

	// Arm and place a limit order but DON'T deliver placeDoneMsg yet — the
	// place is "in flight".
	m, _ = press(t, m, k('b', "b"))
	m = typeText(t, m, "0.001")
	m, _ = press(t, m, special(tea.KeyEnter))
	m, placeCmd := press(t, m, special(tea.KeyEnter))
	if placeCmd == nil || !m.orderInFlight {
		t.Fatal("the confirmed review should dispatch and mark an order in flight")
	}

	// Esc backs out of the busy view, then out of order mode, while the place
	// is still pending — the gate must hold throughout.
	m, _ = press(t, m, special(tea.KeyEscape), special(tea.KeyEscape))
	if m.mode != modeNormal || !m.orderInFlight {
		t.Fatalf("esc should return to normal but keep the in-flight gate: mode=%v inFlight=%v", m.mode, m.orderInFlight)
	}

	// A second order attempt must be refused while in flight.
	m, _ = press(t, m, k('b', "b"))
	if m.mode == modeOrder {
		t.Fatal("must not open the order panel while an order is in flight")
	}
	// A cancel must also be refused — even with the open-orders pane focused (so
	// it's the in-flight gate refusing it, not the focus gate).
	m, _ = press(t, m, special(tea.KeyTab))
	m, _ = press(t, m, k('x', "x"))
	if m.mode == modeConfirm {
		t.Fatal("must not start a cancel while an order is in flight")
	}

	// Only the original place ran.
	placeCmd()
	if len(tr.tickets) != 1 {
		t.Fatalf("exactly one order should have been dispatched, got %d", len(tr.tickets))
	}

	// Once it completes, order entry is allowed again.
	mm, _ := m.Update(placeDoneMsg{res: tr.result})
	m = mm.(model)
	if m.orderInFlight {
		t.Fatal("in-flight gate must clear when the place completes")
	}
	m, _ = press(t, m, k('b', "b"))
	if m.mode != modeOrder {
		t.Fatal("order entry must be allowed again after the in-flight order finished")
	}
}

func TestCancelUsesSymbolCapturedAtConfirmTime(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr)
	m = seedOpenOrder(t, m) // order 777, btc_krw

	m, _ = press(t, m, special(tea.KeyTab)) // focus the open-orders pane (cancel is gated on it)
	m, _ = press(t, m, k('x', "x"))
	if m.confirmSymbol != "btc_krw" {
		t.Fatalf("symbol must be captured at x time, got %q", m.confirmSymbol)
	}
	// The order vanishes from the store before the user confirms (it filled).
	m.store = state.New(state.Config{}, func() int64 { return 0 })
	m, cmd := press(t, m, special(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("enter must dispatch the cancel")
	}
	cmd()
	if len(tr.cancels) != 1 || tr.cancels[0] != [3]string{"btc_krw", "777", "1"} {
		t.Fatalf("cancel must use the captured symbol even after the order left the store: %+v", tr.cancels)
	}
}

// TestCancelAllThisPair: X opens the dialog (default scope = the active pair),
// snapshots the open orders, and enter cancels each of them via a sequential batch.
func TestCancelAllThisPair(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr)
	m = seedOrders(t, m, "btc_krw", 777, 778, 779)
	m, _ = press(t, m, special(tea.KeyTab)) // focus orders
	m, _ = press(t, m, k('X', "X"))
	if m.mode != modeCancelAll || m.cancelAll.allPairs {
		t.Fatalf("X should open the dialog scoped to the active pair, mode=%v allPairs=%v", m.mode, m.cancelAll.allPairs)
	}
	if !m.cancelAll.snapped || len(m.cancelAll.snapshot) != 3 {
		t.Fatalf("the dialog should snapshot the 3 open orders, snapped=%v n=%d", m.cancelAll.snapped, len(m.cancelAll.snapshot))
	}
	m, _ = press(t, m, k('y', "y"))
	if m.cancelAll.running {
		t.Fatal("y must be a no-op on the cancel-all dialog (enter confirms)")
	}
	m, cmd := press(t, m, special(tea.KeyEnter))
	if !m.cancelAll.running || !m.orderInFlight {
		t.Fatal("enter should start the batch and hold the in-flight gate")
	}
	m = drainCmds(t, m, cmd)
	if len(tr.cancels) != 3 {
		t.Fatalf("the batch should issue 3 cancels, got %d (%v)", len(tr.cancels), tr.cancels)
	}
	if m.mode != modeNormal || m.cancelAll.active {
		t.Fatal("the dialog should close after the batch finishes")
	}
	if m.orderInFlight {
		t.Fatal("the in-flight gate must clear after the batch")
	}
}

// TestCancelAllAllPairsWaitsForLoad: switching the scope to all pairs while a
// watched pair hasn't loaded its snapshot blocks the confirm until it does, then
// snapshots the full set.
func TestCancelAllAllPairsWaitsForLoad(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr)               // btc_krw, eth_krw
	m = seedOrders(t, m, "btc_krw", 777)      // btc ready; eth_krw NOT loaded yet
	m, _ = press(t, m, special(tea.KeyTab))   // focus orders
	m, _ = press(t, m, k('X', "X"))           // this-pair scope (snapped)
	m, _ = press(t, m, special(tea.KeyRight)) // switch to all pairs
	if !m.cancelAll.allPairs || m.cancelAll.snapped {
		t.Fatalf("all-pairs scope must wait for every pair to load, snapped=%v", m.cancelAll.snapped)
	}
	mY, _ := press(t, m, special(tea.KeyEnter)) // confirm refused while loading
	if mY.cancelAll.running {
		t.Fatal("must not start the batch while orders are still loading")
	}
	m = mY
	m = feed(t, m, dataEvent("myOrder", "eth_krw", stream.OriginBackfill, 100, "/v2/openOrders", "[]"))
	if !m.cancelAll.snapped || len(m.cancelAll.snapshot) != 1 {
		t.Fatalf("once every pair loads, the snapshot should settle to 1, snapped=%v n=%d",
			m.cancelAll.snapped, len(m.cancelAll.snapshot))
	}
}

// TestCancelAllSnapshotFrozenAndResets: the snapshot doesn't drift as orders
// change while the dialog is open, and closing+reopening resets the state.
func TestCancelAllSnapshotFrozenAndResets(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = seedOrders(t, m, "btc_krw", 777, 778)
	m, _ = press(t, m, special(tea.KeyTab))
	m, _ = press(t, m, k('X', "X"))
	if len(m.cancelAll.snapshot) != 2 {
		t.Fatalf("snapshot should capture 2 orders, got %d", len(m.cancelAll.snapshot))
	}
	// A third order appears while the dialog is open — the snapshot stays frozen.
	m = seedOrders(t, m, "btc_krw", 777, 778, 779)
	if len(m.cancelAll.snapshot) != 2 {
		t.Fatalf("the snapshot must stay frozen at 2, got %d", len(m.cancelAll.snapshot))
	}
	// Switch scope to all pairs (waiting), then esc closes and resets.
	m, _ = press(t, m, special(tea.KeyRight))
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal || m.cancelAll.active {
		t.Fatal("esc should close the dialog when no batch is running")
	}
	// Reopen: fresh state — scope back to the view default, snapshot re-taken (now 3).
	m, _ = press(t, m, k('X', "X"))
	if m.cancelAll.allPairs {
		t.Fatal("a reopened dialog must reset the scope to the view default")
	}
	if !m.cancelAll.snapped || len(m.cancelAll.snapshot) != 3 {
		t.Fatalf("the reopened snapshot should reflect the current 3 orders, got %d", len(m.cancelAll.snapshot))
	}
}

// TestCancelAllReportsFailures: a batch where every cancel errors still runs to
// completion, tallies the failures, and reports them.
func TestCancelAllReportsFailures(t *testing.T) {
	tr := &fakeTrader{cancelErr: fmt.Errorf("boom")}
	m := testModel(t, true, tr)
	m = seedOrders(t, m, "btc_krw", 777, 778)
	m, _ = press(t, m, special(tea.KeyTab))
	m, _ = press(t, m, k('X', "X"))
	m, cmd := press(t, m, special(tea.KeyEnter))
	m = drainCmds(t, m, cmd)
	if len(tr.cancels) != 2 {
		t.Fatalf("the batch should attempt all 2 cancels, got %d", len(tr.cancels))
	}
	if !m.toast.isError || !strings.Contains(m.toast.text, "failed") {
		t.Fatalf("a failing batch should report failures, toast=%+v", m.toast)
	}
	if m.orderInFlight || m.cancelAll.active {
		t.Fatal("the gate and dialog must clear even when every cancel failed")
	}
}

// TestCancelAllAbort: esc during the run stops after the in-flight cancel returns
// — the remaining orders are left alone and the dialog closes.
func TestCancelAllAbort(t *testing.T) {
	tr := &fakeTrader{}
	m := testModel(t, true, tr)
	m = seedOrders(t, m, "btc_krw", 777, 778, 779)
	m, _ = press(t, m, special(tea.KeyTab))
	m, _ = press(t, m, k('X', "X"))
	m, cmd := press(t, m, special(tea.KeyEnter)) // starts the batch; cmd cancels the 1st order
	if cmd == nil {
		t.Fatal("enter should dispatch the first cancel")
	}
	step := cmd()                              // executes the 1st cancel (tr.cancels == 1)
	m, _ = press(t, m, special(tea.KeyEscape)) // abort while the 1st is "in flight"
	if !m.cancelAll.aborted {
		t.Fatal("esc during the run should request an abort")
	}
	mm, _ := m.Update(step) // the in-flight cancel returns → finalize, no further dispatch
	m = mm.(model)
	if len(tr.cancels) != 1 {
		t.Fatalf("abort should stop after the in-flight cancel, got %d", len(tr.cancels))
	}
	if m.mode != modeNormal || m.cancelAll.active || m.orderInFlight {
		t.Fatal("an aborted batch should finalize: close the dialog and clear the gate")
	}
	if !strings.Contains(m.toast.text, "stopped") {
		t.Fatalf("an aborted batch should report it stopped, toast=%+v", m.toast)
	}
}

func TestViewRendersMarketData(t *testing.T) {
	m := testModel(t, false, nil)
	m = feed(t, m, dataEvent("ticker", "btc_krw", stream.OriginSnapshot, 100, "", `{
		"type":"ticker","timestamp":100,"symbol":"btc_krw","data":{
		"open":"1","high":"2","low":"3","close":"99027000","prevClose":"1","priceChange":"4348000",
		"priceChangePercent":"4.59","volume":"147.9","quoteVolume":"1","bestAskPrice":"99027000",
		"bestBidPrice":"99026000","lastTradedAt":90}}`))
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 101, "", `{
		"data":{"timestamp":99,"asks":[{"price":"99131000","qty":"0.004"}],"bids":[{"price":"99120000","qty":"0.003"}]}}`))
	m = feed(t, m, dataEvent("trade", "btc_krw", stream.OriginSnapshot, 102, "",
		`{"data":[{"timestamp":95,"price":"98909000","qty":"0.001","isBuyerTaker":true,"tradeId":5}]}`))

	out := plain(m.render())
	for _, want := range []string{"99,027,000", "4.59%", "99,131,000", "99,120,000", "98,909,000", "public"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered view missing %q:\n%s", want, out)
		}
	}
}

func TestViewRendersAccountPanels(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	// A comfortably wide terminal so the orders table shows a full KRW price
	// without truncating its content (the market sidebar narrows the right
	// column, so this is wider than the pre-sidebar test used).
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 180, Height: 32})
	m = mm.(model)
	m = seedOpenOrder(t, m)
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"900000","tradeInUse":"100000","withdrawalInUse":"0","avgPrice":"0"}]`))
	m = feed(t, m, dataEvent("myTrade", "btc_krw", stream.OriginRealtime, 100, "", `{
		"symbol":"btc_krw","timestamp":100,"channelType":"myTrade","trade":{"trades":[
		{"tradeId":9,"orderId":777,"side":"buy","price":"99000000","qty":"0.1","fee":"10","feeCurrency":"krw","filledAt":95,"isTaker":true}]}}`))

	out := plain(m.render())
	for _, want := range []string{"[open] │ closed · BTC/KRW (1)", "99,017,000", "balances", "900,000", "fills", "key:testkey"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered view missing %q:\n%s", want, out)
		}
	}
}

func TestViewTooSmall(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = mm.(model)
	out := plain(m.render())
	if !strings.Contains(out, "terminal too small") || !strings.Contains(out, "100x28") {
		t.Fatalf("small terminals must get a clear message naming the floor: %q", out)
	}
	// One short of the hard floor on either axis still replaces the screen.
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 99, Height: 32})
	if !strings.Contains(plain(mm.(model).render()), "terminal too small") {
		t.Fatal("width below the hard floor must replace the screen")
	}
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 27})
	if !strings.Contains(plain(mm.(model).render()), "terminal too small") {
		t.Fatal("height below the hard floor must replace the screen")
	}
}

// TestViewCrampedChip: between the hard floor and the recommended size the TUI
// renders normally with a size warning in the header; at the recommended size
// the chip is gone.
func TestViewCrampedChip(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m = mm.(model)
	out := plain(m.render())
	if strings.Contains(out, "terminal too small") {
		t.Fatal("a cramped-but-supported size must render the normal screen")
	}
	if !strings.Contains(out, "⚠ 110x30 — best ≥120x32") {
		t.Fatalf("a cramped size must warn in the header: %q", out)
	}
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	if strings.Contains(plain(mm.(model).render()), "best ≥") {
		t.Fatal("the recommended size must not warn")
	}
}

// TestPublicModeSmallTerminal: public mode has no account right column, so it
// runs at sizes that would be below the private four-column floor — it stays
// usable on a classic 80x24 terminal (no resize notice, no cramped chip), warns
// between its own floor and recommended size, and only blanks well below that.
func TestPublicModeSmallTerminal(t *testing.T) {
	m := testModel(t, false, nil)

	// 80x24 is comfortable for public mode: normal screen, no cramped chip.
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	out := plain(mm.(model).render())
	if strings.Contains(out, "terminal too small") {
		t.Fatalf("public mode must run at 80x24, not blank it: %q", out)
	}
	if strings.Contains(out, "best ≥") {
		t.Fatalf("80x24 is the recommended public size — it must not warn: %q", out)
	}

	// Between the public floor and its recommended size: warn, don't blank.
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	out = plain(mm.(model).render())
	if strings.Contains(out, "terminal too small") {
		t.Fatalf("a cramped public size must still render: %q", out)
	}
	if !strings.Contains(out, "⚠ 70x20 — best ≥80x24") {
		t.Fatalf("a cramped public size must warn in the header: %q", out)
	}

	// Below the public floor the screen is replaced, naming the public floor.
	mm, _ = m.Update(tea.WindowSizeMsg{Width: 55, Height: 24})
	out = plain(mm.(model).render())
	if !strings.Contains(out, "terminal too small") || !strings.Contains(out, "60x18") {
		t.Fatalf("below the public floor must name 60x18: %q", out)
	}
}

// TestPublicModeNoNoticesPane: public mode drops the notices pane entirely — the
// body is three columns and the pane's title never renders — while the 'n'
// popup still surfaces notices on demand.
func TestPublicModeNoNoticesPane(t *testing.T) {
	m := testModel(t, false, nil)
	if _, _, _, right := m.colWidths(); right != 0 {
		t.Fatalf("public mode must have no right column, got width %d", right)
	}
	out := plain(m.render())
	if strings.Contains(out, "stream notices") || strings.Contains(out, "no notices") {
		t.Fatalf("public mode must not render a notices pane:\n%s", out)
	}
	// The popup is still reachable with 'n'.
	m, _ = press(t, m, k('n', "n"))
	if m.mode != modeNotices {
		t.Fatal("n must still open the notices popup in public mode")
	}
	if !strings.Contains(plain(m.render()), "stream notices") {
		t.Fatal("the notices popup must render the notices title")
	}
}

func TestNoticesOverlayAndStatusDots(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = feedNotice(t, m, stream.Connected, stream.LevelInfo, map[string]any{"endpoint": "public"})
	m = feedNotice(t, m, stream.DataGap, stream.LevelWarn, map[string]any{"channel": "trade", "symbol": "btc_krw"})

	out := plain(m.render())
	if !strings.Contains(out, "gaps:1") {
		t.Fatalf("status line must count gaps:\n%s", out)
	}
	m, _ = press(t, m, k('n', "n"))
	out = plain(m.render())
	if !strings.Contains(out, "DATA_GAP") {
		t.Fatalf("notices overlay must list the gap:\n%s", out)
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal {
		t.Fatal("esc must close the overlay")
	}
}

func feedNotice(t *testing.T, m model, code stream.NoticeCode, level stream.Level, details map[string]any) model {
	t.Helper()
	return feed(t, m, stream.Notice{Code: code, Level: level, Message: string(code), Details: details, Time: 1})
}

// --- mouse resize ---

func send(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	mm, _ := m.Update(msg)
	return mm.(model)
}

func mclick(x, y int) tea.MouseClickMsg { return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft} }
func mmotion(x, y int) tea.MouseMotionMsg {
	return tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft}
}

func TestDefaultColumnProportions(t *testing.T) {
	m := testModel(t, true, &fakeTrader{}) // 120x32
	// Sidebar 16% / orderbook 26% / trades 26% / account the rest, with the
	// per-column minimums applied — chosen so the market columns stay legible.
	side, book, trades, right := m.colWidths()
	if side != 19 || book != 31 || trades != 32 || right != 38 {
		t.Fatalf("default widths side/book/trades/right = %d/%d/%d/%d, want 19/31/32/38", side, book, trades, right)
	}
}

func TestMouseDragResizesColumns(t *testing.T) {
	m := testModel(t, true, &fakeTrader{}) // 120x32 -> side/book/trades/right = 19/31/32/38
	m = send(t, m, mclick(50, 10))         // grab the orderbook|trades seam (sideW+bookW=50)
	if m.drag != dragColA {
		t.Fatalf("a click on the seam should start a colA drag, got %v", m.drag)
	}
	m = send(t, m, mmotion(62, 10)) // drag it right
	_, book, _, right := m.colWidths()
	if book <= 31 {
		t.Fatalf("dragging the seam right should widen the orderbook, got %d", book)
	}
	if right != 38 {
		t.Fatalf("the trades|right seam should not move (right=%d, want 38)", right)
	}
	m = send(t, m, tea.MouseReleaseMsg{Button: tea.MouseLeft})
	if m.drag != dragNone {
		t.Fatal("release must end the drag")
	}
}

func TestMouseDragResizesSidebar(t *testing.T) {
	m := testModel(t, true, &fakeTrader{}) // 120x32 -> sideW=19
	m = send(t, m, mclick(19, 10))         // grab the sidebar|orderbook seam
	if m.drag != dragColS {
		t.Fatalf("a click on the sidebar seam should start a colS drag, got %v", m.drag)
	}
	m = send(t, m, mmotion(28, 10)) // drag it right
	if side, _, _, _ := m.colWidths(); side <= 19 {
		t.Fatalf("dragging the sidebar seam right should widen it, got %d", side)
	}
}

// TestPublicModeSidebarDragWidens: public mode has no right column, so the
// sidebar seam's upper bound must not reserve minRightW — at a narrow public
// terminal that reservation would pin the sidebar and freeze the drag.
func TestPublicModeSidebarDragWidens(t *testing.T) {
	m := testModel(t, false, nil)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 62, Height: 20}) // just above the public floor
	m = mm.(model)
	sideBefore, _, _, _ := m.colWidths()
	m = send(t, m, mclick(sideBefore, 5)) // grab the sidebar|orderbook seam
	if m.drag != dragColS {
		t.Fatalf("a click on the sidebar seam should start a colS drag, got %v", m.drag)
	}
	m = send(t, m, mmotion(sideBefore+6, 5)) // drag it right
	if side, _, _, _ := m.colWidths(); side <= sideBefore {
		t.Fatalf("public sidebar drag must widen the sidebar past %d, got %d", sideBefore, side)
	}
}

// modelWithSwitch builds a 120x32 model whose SetActiveMarket records every
// (prev,next) switch, for asserting the dynamic re-subscribe seam fires.
func modelWithSwitch(t *testing.T, private bool) (model, *[][2]string) {
	t.Helper()
	switches := &[][2]string{}
	var trader Trader
	if private {
		trader = &fakeTrader{}
	}
	m := newModel(Config{
		Symbols:         []string{"btc_krw", "eth_krw", "xrp_krw"},
		Private:         private,
		Trader:          trader,
		KeyName:         "testkey",
		BaseURL:         "http://127.0.0.1:9999",
		Now:             func() int64 { return 1_700_000_000_000 },
		StopSession:     func() {},
		SetActiveMarket: func(prev, next, _ string) { *switches = append(*switches, [2]string{prev, next}) },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	return mm.(model), switches
}

// sendC drives one message through Update and returns the model AND the command
// (so a test can fire a debounce tick).
func sendC(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	mm, cmd := m.Update(msg)
	return mm.(model), cmd
}

// TestSidebarSwitchFiresResubscribe: an arrow selects immediately, and the
// re-subscribe fires only after the debounce tick settles — with the prev
// symbol tracked across switches.
func TestSidebarSwitchFiresResubscribe(t *testing.T) {
	m, switches := modelWithSwitch(t, false)
	m, cmd := sendC(t, m, special(tea.KeyDown)) // select eth immediately
	if m.symbol() != "eth_krw" {
		t.Fatalf("down should select eth immediately, got %s", m.symbol())
	}
	if len(*switches) != 0 {
		t.Fatalf("the re-subscribe must be debounced, not immediate: %+v", *switches)
	}
	m = send(t, m, cmd()) // fire the debounce tick
	if len(*switches) != 1 || (*switches)[0] != [2]string{"btc_krw", "eth_krw"} {
		t.Fatalf("a settled selection should fire SetActiveMarket(prev,next), got %+v", *switches)
	}
	m, cmd = sendC(t, m, special(tea.KeyDown)) // move on to xrp
	m = send(t, m, cmd())
	if len(*switches) != 2 || (*switches)[1] != [2]string{"eth_krw", "xrp_krw"} {
		t.Fatalf("the second switch should fire with the updated prev, got %+v", *switches)
	}
}

// TestOrderbookGroupingCycle: +/- cycle the active symbol's grouping level.
// Unknown levels kick the metadata fetch first; a change re-points the
// orderbook subscription (alone) on the settle debounce, hiding the book —
// and only the book — until the regrouped snapshot lands; the ends clamp.
func TestOrderbookGroupingCycle(t *testing.T) {
	var repoints [][2]string
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
		TickSizePolicy: func(string) (TickPolicy, error) {
			return TickPolicy{
				Bands:  []ops.TickBand{{PriceGte: "0", TickSize: "1000"}},
				Levels: []string{"10000", "1000"}, // deliberately unsorted
			}, nil
		},
		SetActiveMarket:   func(prev, next, _ string) {},
		SetOrderbookLevel: func(symbol, level string) { repoints = append(repoints, [2]string{symbol, level}) },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = seedOrderMarket(t, mm.(model))
	m = feed(t, m, dataEvent("trade", "btc_krw", stream.OriginSnapshot, 200, "",
		`{"data":[{"timestamp":1000,"price":"100000000","qty":"0.5","isBuyerTaker":true,"tradeId":5}]}`))

	// Levels not fetched yet: the key kicks the fetch and waits for a re-press.
	m2, cmd := sendC(t, m, k('-', "-"))
	m = m2
	if cmd == nil {
		t.Fatal("the first press must kick the metadata fetch")
	}
	m = drainCmds(t, m, cmd)
	if len(repoints) != 0 || m.bookGrp["btc_krw"] != "" {
		t.Fatalf("no grouping change before the levels are known, got %+v", repoints)
	}

	// Coarser: raw → 1,000 (the smallest level, regardless of fetch order).
	m, cmd = sendC(t, m, k('-', "-"))
	if m.bookGrp["btc_krw"] != "1000" {
		t.Fatalf("- must step to the finest level first, got %q", m.bookGrp["btc_krw"])
	}
	if len(repoints) != 0 {
		t.Fatalf("the re-point must be debounced, not immediate: %+v", repoints)
	}
	m = send(t, m, cmd()) // fire the settle debounce
	if len(repoints) != 1 || repoints[0] != [2]string{"btc_krw", "1000"} {
		t.Fatalf("the settle must re-point the orderbook at the level, got %+v", repoints)
	}
	if m.store.OrderbookReady("btc_krw") {
		t.Fatal("the old-granularity book must hide until the regrouped snapshot lands")
	}
	if !m.store.TradesReady("btc_krw") {
		t.Fatal("a grouping change must not mark the untouched trade feed stale")
	}

	// Coarser to the largest level, then the end clamps quietly.
	m, cmd = sendC(t, m, k('-', "-"))
	m = send(t, m, cmd())
	if m.bookGrp["btc_krw"] != "10000" {
		t.Fatalf("- must step to the next coarser level, got %q", m.bookGrp["btc_krw"])
	}
	m, cmd = sendC(t, m, k('-', "-"))
	if cmd != nil || m.bookGrp["btc_krw"] != "10000" {
		t.Fatalf("the coarsest level must clamp, got %q", m.bookGrp["btc_krw"])
	}

	// Finer all the way back to the raw book.
	m, cmd = sendC(t, m, k('+', "+"))
	m = send(t, m, cmd())
	m, cmd = sendC(t, m, k('+', "+"))
	m = send(t, m, cmd())
	if m.bookGrp["btc_krw"] != "" {
		t.Fatalf("+ must step back to the raw book, got %q", m.bookGrp["btc_krw"])
	}
	if last := repoints[len(repoints)-1]; last != [2]string{"btc_krw", ""} {
		t.Fatalf("the raw book must re-point with an empty level, got %+v", last)
	}
}

// TestSidebarRapidSwitchCoalesces: scrubbing through several symbols quickly
// re-subscribes once (to the final selection); a superseded debounce tick is a
// no-op.
func TestSidebarRapidSwitchCoalesces(t *testing.T) {
	m, switches := modelWithSwitch(t, false)    // btc, eth, xrp
	m, _ = sendC(t, m, special(tea.KeyDown))    // -> eth (gen 1, tick discarded)
	m, cmd := sendC(t, m, special(tea.KeyDown)) // -> xrp (gen 2)
	m = send(t, m, cmd())                       // the latest tick goes straight btc->xrp
	if len(*switches) != 1 || (*switches)[0] != [2]string{"btc_krw", "xrp_krw"} {
		t.Fatalf("a coalesced switch should re-subscribe once btc->xrp, got %+v", *switches)
	}
	m = send(t, m, resubscribeMsg{gen: 1}) // the stale earlier tick must not fire
	if len(*switches) != 1 {
		t.Fatalf("a superseded debounce tick must be a no-op, got %+v", *switches)
	}
}

// TestMarketSwitchAndDisconnectHideStaleData: the orderbook pane hides a prior
// subscription's retained book after a market switch (until the resubscribe
// snapshot lands) and after a public disconnect — it never shows stale depth.
func TestMarketSwitchAndDisconnectHideStaleData(t *testing.T) {
	m, _ := modelWithSwitch(t, false) // btc, eth, xrp; active + subscribed = btc
	book := `{"data":{"timestamp":99,"asks":[{"price":"99131000","qty":"0.004"}],"bids":[{"price":"99120000","qty":"0.003"}]}}`
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 100, "", book))
	if !strings.Contains(plain(m.render()), "99,131,000") {
		t.Fatal("a fresh orderbook for the active symbol must render")
	}

	// Switch to eth and back to btc: btc's retained book must not show until a
	// fresh snapshot for this subscription arrives.
	m, cmd := sendC(t, m, special(tea.KeyDown)) // -> eth
	m = send(t, m, cmd())                       // resubscribe eth
	m, cmd = sendC(t, m, special(tea.KeyUp))    // -> btc
	m = send(t, m, cmd())                       // resubscribe btc (MarkMarketStale)
	if strings.Contains(plain(m.render()), "99,131,000") {
		t.Fatal("switching back must hide the prior subscription's stale book")
	}
	// The resubscribe snapshot restores it.
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 200, "", book))
	if !strings.Contains(plain(m.render()), "99,131,000") {
		t.Fatal("the resubscribe snapshot must restore the orderbook")
	}
	// A public disconnect makes it stale again.
	m = feedNotice(t, m, stream.Disconnected, stream.LevelWarn, map[string]any{"endpoint": "public"})
	if strings.Contains(plain(m.render()), "99,131,000") {
		t.Fatal("a public disconnect must hide the stale orderbook")
	}
}

// TestSidebarClickSwitchesSymbol: clicking a sidebar row selects it immediately
// (the re-subscribe still debounces).
func TestSidebarClickSwitchesSymbol(t *testing.T) {
	m, switches := modelWithSwitch(t, false)
	// Content rows start at bodyTop+2 = 4: row 0 btc (active), row 1 eth.
	m, cmd := sendC(t, m, mclick(3, 5)) // click the eth row
	if m.symbol() != "eth_krw" {
		t.Fatalf("clicking a sidebar row should switch to it, got %s", m.symbol())
	}
	m = send(t, m, cmd())
	if len(*switches) != 1 || (*switches)[0] != [2]string{"btc_krw", "eth_krw"} {
		t.Fatalf("a sidebar click should fire SetActiveMarket, got %+v", *switches)
	}
}

// TestMouseWheelScrollsSidebarOnly: the wheel moves the sidebar window without
// changing the selection or re-subscribing.
func TestMouseWheelScrollsSidebarOnly(t *testing.T) {
	syms := make([]string, 50)
	for i := range syms {
		syms[i] = fmt.Sprintf("sym%02d_krw", i)
	}
	switches := &[][2]string{}
	m := newModel(Config{
		Symbols: syms, BaseURL: "http://127.0.0.1:9999",
		Now: func() int64 { return 1 }, StopSession: func() {},
		SetActiveMarket: func(p, n, _ string) { *switches = append(*switches, [2]string{p, n}) },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = mm.(model)
	m = send(t, m, tea.MouseWheelMsg{X: 5, Y: 6, Button: tea.MouseWheelDown})
	if m.scroll != wheelStep {
		t.Fatalf("wheel down should scroll the window by %d, got %d", wheelStep, m.scroll)
	}
	if m.active != 0 {
		t.Fatalf("the wheel must not change the selection, got active=%d", m.active)
	}
	if len(*switches) != 0 {
		t.Fatalf("the wheel must not re-subscribe, got %+v", *switches)
	}
}

// TestSidebarPositionIndicator: the sidebar's bottom-right shows the selected
// symbol's position in the list, tracking the selection.
func TestSidebarPositionIndicator(t *testing.T) {
	m := testModel(t, false, nil) // 2 symbols
	if out := plain(m.render()); !strings.Contains(out, "1/2") {
		t.Fatalf("sidebar should show position 1/2:\n%s", out)
	}
	m, _ = press(t, m, special(tea.KeyDown))
	if out := plain(m.render()); !strings.Contains(out, "2/2") {
		t.Fatalf("the indicator should track the selection (2/2):\n%s", out)
	}
}

// TestSidebarRendersSymbols: the sidebar lists the symbols with their %change.
func TestSidebarRendersSymbols(t *testing.T) {
	m := testModel(t, false, nil) // btc_krw, eth_krw
	m = feed(t, m, dataEvent("ticker", "eth_krw", stream.OriginSnapshot, 100, "",
		`{"type":"ticker","timestamp":100,"symbol":"eth_krw","data":{"close":"5000000","priceChange":"-100","priceChangePercent":"-2.0"}}`))
	out := plain(m.render())
	for _, want := range []string{"markets", "BTC/KRW", "ETH/KRW", "-2.0%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sidebar missing %q:\n%s", want, out)
		}
	}
}

func TestMouseDragResizesRightRows(t *testing.T) {
	m := testModel(t, true, &fakeTrader{}) // 120x32
	bodyH := m.bodyHeight()
	oh0, _, _ := m.rightHeights(bodyH)
	side, book, trades, _ := m.colWidths()
	rightStart := side + book + trades
	seamY := m.bodyTop() + oh0
	m = send(t, m, mclick(rightStart+5, seamY)) // grab the orders|fills seam mid-column
	if m.drag != dragRowA {
		t.Fatalf("a click on the orders|fills seam should start a rowA drag, got %v", m.drag)
	}
	m = send(t, m, mmotion(rightStart+5, seamY-3)) // drag it up
	if oh1, _, _ := m.rightHeights(bodyH); oh1 >= oh0 {
		t.Fatalf("dragging the orders|fills seam up should shrink orders (%d -> %d)", oh0, oh1)
	}
}

func TestLayoutResetKey(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = send(t, m, mclick(50, 10))
	m = send(t, m, mmotion(64, 10)) // widen the orderbook
	if _, b, _, _ := m.colWidths(); b == 31 {
		t.Fatal("precondition: the drag should have changed the layout")
	}
	m, _ = press(t, m, k('=', "="))
	if s, b, tr, r := m.colWidths(); s != 19 || b != 31 || tr != 32 || r != 38 {
		t.Fatalf("'=' should restore the default layout, got %d/%d/%d/%d", s, b, tr, r)
	}
}

func TestMouseIgnoredUnderOverlay(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m, _ = press(t, m, k('?', "?")) // open the help overlay
	if m.mode != modeHelp {
		t.Fatal("precondition: the help overlay must be open")
	}
	m = send(t, m, mclick(50, 2)) // on a column seam's x, inside the body
	if m.drag != dragNone {
		t.Fatal("a click under an overlay must not start a resize drag")
	}
}

// TestPaletteProfileAndColorScheme pins the profile/color-scheme color decisions: bars are
// drawn only where the terminal can render a subtle background, and the color scheme
// swaps which color marks up vs down.
func TestPaletteProfileAndColorScheme(t *testing.T) {
	// Bars only on truecolor / 256-color; dropped on 16-color and below.
	for _, c := range []struct {
		p    colorprofile.Profile
		bars bool
	}{
		{colorprofile.TrueColor, true},
		{colorprofile.ANSI256, true},
		{colorprofile.ANSI, false},
		{colorprofile.Ascii, false},
		{colorprofile.NoTTY, false},
	} {
		if got := uikit.PaletteFor(uikit.ColorSchemeGreenRed, c.p).DrawBars(); got != c.bars {
			t.Errorf("drawBars on %v = %v, want %v", c.p, got, c.bars)
		}
	}
	// The color scheme swaps the down color (red→blue) while up stays the up role.
	gr := uikit.PaletteFor(uikit.ColorSchemeGreenRed, colorprofile.TrueColor)
	rb := uikit.PaletteFor(uikit.ColorSchemeRedBlue, colorprofile.TrueColor)
	if gr.Up.BarBG == rb.Up.BarBG {
		t.Error("red-blue must recolor the up side vs green-red")
	}
	if uikit.ColorSchemeGreenRed.Next() != uikit.ColorSchemeRedBlue || uikit.ColorSchemeRedBlue.Next() != uikit.ColorSchemeGreenRed {
		t.Error("color-scheme toggle must alternate between the two color schemes")
	}
}

// TestColorSchemeToggleAndDepthBars drives the model end to end: a truecolor
// profile message makes the orderbook emit a truecolor background depth bar, and
// the C key toggles the up/down color scheme.
func TestColorSchemeToggleAndDepthBars(t *testing.T) {
	m := testModel(t, false, nil)
	m = send(t, m, tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m = feed(t, m, dataEvent("ticker", "btc_krw", stream.OriginSnapshot, 100, "", `{
		"data":{"close":"99027000","priceChange":"4348000","priceChangePercent":"4.59",
		"bestAskPrice":"99027000","bestBidPrice":"99026000","low":"3","high":"2","volume":"1"}}`))
	m = feed(t, m, dataEvent("orderbook", "btc_krw", stream.OriginSnapshot, 101, "",
		`{"data":{"timestamp":99,"asks":[{"price":"99131000","qty":"0.004"}],"bids":[{"price":"99120000","qty":"0.003"}]}}`))

	if raw := m.render(); !strings.Contains(raw, "48;2;") {
		t.Fatalf("a truecolor orderbook must emit a truecolor background depth bar")
	}
	m, _ = press(t, m, k('C', "C"))
	if m.colorScheme != uikit.ColorSchemeRedBlue {
		t.Fatalf("C must toggle the color scheme, got %v", m.colorScheme)
	}
	if !strings.Contains(m.toast.text, "red-blue") {
		t.Fatalf("the toggle should toast the new color scheme, got %q", m.toast.text)
	}
}

func TestGroupThousands(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"5":           "5",
		"99027000":    "99,027,000",
		"-4348000":    "-4,348,000",
		"1234.5678":   "1,234.5678",
		"0.00146702":  "0.00146702",
		"not-a-price": "not-a-price",
	}
	for in, want := range cases {
		if got := uikit.GroupThousands(in); got != want {
			t.Fatalf("groupThousands(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHelpOverlayClickOutsideCloses(t *testing.T) {
	m := testModel(t, true, nil)
	m, _ = press(t, m, k('?', "?"))
	if m.mode != modeHelp {
		t.Fatal("? should open the help overlay")
	}
	left, top, _, _ := m.centeredOverlayBounds(m.renderHelp())
	if left <= 0 {
		t.Skip("no left margin at this width to click into")
	}
	m = send(t, m, mclick(left-1, top+1)) // just outside the box frame
	if m.mode != modeNormal {
		t.Errorf("a click outside the help box should dismiss it; mode = %v", m.mode)
	}
}

func TestHelpOverlayClickFrameStaysOpen(t *testing.T) {
	m := testModel(t, true, nil)
	m, _ = press(t, m, k('?', "?"))
	left, top, _, _ := m.centeredOverlayBounds(m.renderHelp())
	m = send(t, m, mclick(left, top)) // the top-left border cell counts as inside
	if m.mode != modeHelp {
		t.Error("a click on the help frame must not dismiss it")
	}
}

func TestNoticesOverlayClickOutsideCloses(t *testing.T) {
	m := testModel(t, true, nil)
	m, _ = press(t, m, k('n', "n"))
	if m.mode != modeNotices {
		t.Fatal("n should open the notices overlay")
	}
	left, top, _, _ := m.centeredOverlayBounds(m.renderNotices(m.bodyHeight()))
	if left <= 0 {
		t.Skip("no left margin at this width to click into")
	}
	m = send(t, m, mclick(left-1, top+1)) // just outside the box frame
	if m.mode != modeNormal {
		t.Errorf("a click outside the notices box should dismiss it; mode = %v", m.mode)
	}
}

func TestNoticesOverlayClickFrameStaysOpen(t *testing.T) {
	m := testModel(t, true, nil)
	m, _ = press(t, m, k('n', "n"))
	left, top, _, _ := m.centeredOverlayBounds(m.renderNotices(m.bodyHeight()))
	m = send(t, m, mclick(left, top)) // the top-left border cell counts as inside
	if m.mode != modeNotices {
		t.Error("a click on the notices frame must not dismiss it")
	}
}

// TestOrderTrackingScopesCarryAccountSeq: the tracked-order seam receives
// {account, symbol} scopes under the active account — the per-pair view tracks
// the active pair, the all-pairs view every watched pair.
func TestOrderTrackingScopesCarryAccountSeq(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	var got [][]stream.OrderScope
	m.cfg.SetTrackedOrders = func(scopes []stream.OrderScope) { got = append(got, scopes) }

	m.applyOrderTracking()
	m.ordersAllPairs = true
	m.applyOrderTracking()

	if len(got) != 2 {
		t.Fatalf("want 2 tracking calls, got %d", len(got))
	}
	if len(got[0]) != 1 || got[0][0] != (stream.OrderScope{AccountSeq: 1, Symbol: "btc_krw"}) {
		t.Fatalf("per-pair view must track the active pair under the active account: %+v", got[0])
	}
	if len(got[1]) != 2 {
		t.Fatalf("all-pairs view must track every watched pair: %+v", got[1])
	}
	for _, sc := range got[1] {
		if sc.AccountSeq != 1 {
			t.Fatalf("all-pairs scopes must carry the active account: %+v", got[1])
		}
	}
}

// --- placed-order terminal watch (noticePlacedTerminal) ---

// myOrderLive builds one live myOrder frame carrying a single order row for
// the main account on btc_krw.
func myOrderLive(id int64, status, filledQty string) stream.Data {
	row := fmt.Sprintf(`{"orderId":%d,"status":%q,"side":"buy","orderType":"limit","price":"100","qty":"1","filledQty":%q,"createdAt":900}`,
		id, status, filledQty)
	payload := fmt.Sprintf(`{"symbol":"btc_krw","timestamp":1000,"channelType":"myOrder","order":{"accountSeq":1,"orders":[%s]}}`, row)
	return dataEvent("myOrder", "btc_krw", stream.OriginRealtime, 1000, "", payload)
}

// placeAccepted feeds an accepted placeDoneMsg for the given order id, putting
// it on the placed watch.
func placeAccepted(t *testing.T, m model, id int64) model {
	t.Helper()
	mm, _ := m.Update(placeDoneMsg{origin: placeFromBar,
		res: PlaceResult{OrderID: fmt.Sprintf("%d", id), ClientOrderID: "c"}})
	return mm.(model)
}

// TestPlacedOrderExpiredToast: the REST accept is only an ack — a
// time-in-force reject arrives later as a terminal `expired` myOrder status.
// A session-placed order that dies with no fill must raise an error toast;
// nothing else surfaces it.
func TestPlacedOrderExpiredToast(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = placeAccepted(t, m, 901)
	if !m.placedWatch[901] {
		t.Fatal("an accepted place must be watched for its terminal status")
	}
	m = feed(t, m, myOrderLive(901, "expired", "0"))
	if got := m.toast.text; !strings.Contains(got, "901") || !strings.Contains(got, "rejected") {
		t.Fatalf("zero-fill expired must toast the order's fate as rejected, got %q", got)
	}
	if !m.toast.isError {
		t.Fatal("a rejected order must toast as an error")
	}
	if m.placedWatch[901] {
		t.Fatal("a real terminal status must end the watch")
	}
}

// TestPlacedOrderTerminalBeforeAck: the terminal myOrder frame can arrive over
// the stream before the REST place ack (an immediately-rejected order). The
// order is already terminal in the store when the ack arms the watch, so the
// place path must run the no-fill notice itself — no later order event will.
func TestPlacedOrderTerminalBeforeAck(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	// The reject's terminal frame lands first, before the id is on the watch.
	m = feed(t, m, myOrderLive(906, "expired", "0"))
	if m.toast.text != "" {
		t.Fatalf("an unwatched order's terminal frame must not toast, got %q", m.toast.text)
	}
	// Then the ack arms the watch — the toast must fire from the place path.
	m = placeAccepted(t, m, 906)
	if got := m.toast.text; !strings.Contains(got, "906") || !strings.Contains(got, "rejected") {
		t.Fatalf("an already-terminal order must toast on the place ack, got %q", got)
	}
	if !m.toast.isError {
		t.Fatal("a rejected order must toast as an error")
	}
	if m.placedWatch[906] {
		t.Fatal("the terminal status must end the watch")
	}
}

// TestPlacedOrderFilledStaysSilent: an order that ends WITH fills must not
// toast (the fills panel shows it), and a canceled order stays silent too — the
// cancel path already reports it, so a "no fill" toast would be redundant (and
// would burst over the cancel-all summary).
func TestPlacedOrderFilledStaysSilent(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = placeAccepted(t, m, 902)
	m.toast = toast{}
	m = feed(t, m, myOrderLive(902, "filled", "1"))
	if m.toast.text != "" {
		t.Fatalf("a filled order must not toast, got %q", m.toast.text)
	}
	if m.placedWatch[902] {
		t.Fatal("a terminal order must leave the watch")
	}

	m = placeAccepted(t, m, 903)
	m.toast = toast{}
	m = feed(t, m, myOrderLive(903, "canceled", "0"))
	if m.toast.text != "" {
		t.Fatalf("a canceled order must stay silent here — the cancel path reports it, got %q", m.toast.text)
	}
	if m.placedWatch[903] {
		t.Fatal("a terminal order must leave the watch")
	}
}

// TestPlacedOrderClosedUnknownKeepsWatching: the synthetic closed-while-
// disconnected status is not a real fate (the order may have filled during
// the gap), so it must NOT toast "no fill" — and a real terminal status that
// later supersedes it must still be reported.
func TestPlacedOrderClosedUnknownKeepsWatching(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = placeAccepted(t, m, 904)
	m.toast = toast{}
	// The store treats any unknown "closed" string as the synthetic status;
	// feeding it as a frame stands in for the snapshot reconcile's inference.
	m = feed(t, m, myOrderLive(904, state.StatusClosedUnknown, "0"))
	if m.toast.text != "" {
		t.Fatalf("closed-unknown must stay silent (fate unknown), got %q", m.toast.text)
	}
	if !m.placedWatch[904] {
		t.Fatal("closed-unknown must keep the watch alive")
	}
	m = feed(t, m, myOrderLive(904, "expired", "0"))
	if got := m.toast.text; !strings.Contains(got, "rejected") {
		t.Fatalf("the real terminal status must still toast, got %q", got)
	}
}

// TestPlacedOrderTerminalToastBatchPath: the batched stream delivery runs the
// same watch as the single-event path.
func TestPlacedOrderTerminalToastBatchPath(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = placeAccepted(t, m, 905)
	mm, _ := m.Update(streamBatchMsg{evs: []stream.Event{myOrderLive(905, "expired", "0")}})
	m = mm.(model)
	if !strings.Contains(m.toast.text, "rejected") || !m.toast.isError {
		t.Fatalf("a batched terminal frame must toast, got %q", m.toast.text)
	}
}

// --- closed-orders tab ---

// TestClosedOrdersTab: 'o' switches the orders pane to the closed tab from
// any pane — the read-only, session-observed terminal orders with their
// status column — and the selection is sticky across focus moves.
func TestClosedOrdersTab(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = seedOpenOrder(t, m) // order 777, open

	m, _ = press(t, m, k('o', "o")) // markets focus: o needs no focus round-trip
	if !m.ordersClosed {
		t.Fatal("o must open the closed tab from any pane")
	}
	if frame := plain(m.render()); !strings.Contains(frame, "[closed]") {
		t.Fatalf("the title switcher must bracket the active tab:\n%s", frame)
	}
	if len(m.orderRows) != 0 {
		t.Fatalf("the open order must not be listed closed: %+v", m.orderRows)
	}

	// The order dies with no fill: the closed tab lists it, status visible.
	m = feed(t, m, myOrderLive(777, "canceled", "0"))
	if len(m.orderRows) != 1 || m.orderRows[0][4] != "canceled" {
		t.Fatalf("closed tab must list the canceled order with its status: %+v", m.orderRows)
	}

	// x is dead on the read-only tab (it acts on the pane's selection, so it
	// still requires the pane's focus).
	m, _ = press(t, m, special(tea.KeyTab)) // markets → orders
	m, _ = press(t, m, k('x', "x"))
	if m.mode != modeNormal || !m.toast.isError {
		t.Fatal("x on the closed tab must refuse with a hint")
	}

	// The selection is sticky: leaving the pane keeps the closed tab.
	m, _ = press(t, m, special(tea.KeyTab)) // orders → balances
	if !m.ordersClosed {
		t.Fatal("the tab selection must survive a focus move")
	}
}

// TestClosedOrdersTabClickAndModes: the title's tab switcher is per-segment
// clickable (the mouse analogue of 'o'), the selection survives docking the
// order panel, both 'o' and a title-tab click switch the tab in place while
// the panel is docked, and 'o' also works while browsing the ladder.
func TestClosedOrdersTabClickAndModes(t *testing.T) {
	m := testModel(t, true, &fakeTrader{})
	m = seedOpenOrder(t, m)
	sideW, bookW, tradesW, _ := m.colWidths()
	inner := sideW + bookW + tradesW + 1 // first title cell (inside the border)
	titleY := m.bodyTop() + 1            // the title renders below the top border

	// English labels: "orders: [open] │ closed" — the "orders: " prefix takes
	// the first 8 cells, so the closed segment lands at cell 17.
	m = send(t, m, mclick(inner+18, titleY))
	if m.focus != focusOrders || !m.ordersClosed {
		t.Fatalf("clicking the closed segment must focus the pane and select the tab (focus=%v closed=%v)", m.focus, m.ordersClosed)
	}
	// Now "orders: open │ [closed]": the same x lands on "[closed]" — idempotent.
	m = send(t, m, mclick(inner+18, titleY))
	if !m.ordersClosed {
		t.Fatal("clicking the active segment must be a no-op, not a toggle")
	}
	// The open segment sits at cells 8-11, clear of the column seam's drag zone.
	m = send(t, m, mclick(inner+9, titleY))
	if m.ordersClosed {
		t.Fatal("clicking the open segment must select the open tab")
	}

	// Sticky across the order-panel dock.
	m, _ = press(t, m, k('o', "o"))
	m, _ = press(t, m, k('b', "b"))
	if m.mode != modeOrder || !m.ordersClosed {
		t.Fatalf("the tab selection must survive the order panel (mode=%v closed=%v)", m.mode, m.ordersClosed)
	}
	// 'o' switches the tab in place while the entry panel is docked — a trader
	// checks a close without leaving order entry.
	m, _ = press(t, m, k('o', "o"))
	if m.mode != modeOrder || m.ordersClosed {
		t.Fatalf("o in order mode must switch the tab in place (mode=%v closed=%v)", m.mode, m.ordersClosed)
	}
	// The title tabs stay clickable there too: the pane's title sits below the
	// docked panel, not at the body top, so the click Y follows ordersPaneTop.
	m = send(t, m, mclick(inner+18, m.ordersPaneTop()+1))
	if m.mode != modeOrder || !m.ordersClosed {
		t.Fatalf("clicking the closed segment in order mode must switch the tab in place (mode=%v closed=%v)", m.mode, m.ordersClosed)
	}
	// The switch is form-view only, click as well as key: once the order is
	// armed (or on the wire), the review owns every gesture, so a title-tab
	// click is inert — the same gate as the 'o' key.
	m.order.view = orderConfirm
	m = send(t, m, mclick(inner+9, m.ordersPaneTop()+1)) // the open segment
	if !m.ordersClosed {
		t.Fatal("a title-tab click during the armed review must not switch the tab")
	}
	m.order.view = orderForm                   // restore before leaving the mode
	m, _ = press(t, m, special(tea.KeyEscape)) // back to normal

	// 'o' works in ladder browse — checking a close must not force the
	// trader out of the ladder.
	m, _ = press(t, m, k('t', "t"))
	if m.mode != modeLadder {
		t.Fatalf("setup: want ladder mode, got %v", m.mode)
	}
	m, _ = press(t, m, k('o', "o"))
	if m.mode != modeLadder || m.ordersClosed {
		t.Fatalf("o in ladder browse must switch the tab in place (mode=%v closed=%v)", m.mode, m.ordersClosed)
	}
	// The right column keeps its normal layout in ladder mode, so the title
	// tabs stay clickable too — the mouse analogue of 'o', in place.
	m = send(t, m, mclick(inner+18, m.ordersPaneTop()+1))
	if m.mode != modeLadder || !m.ordersClosed {
		t.Fatalf("clicking the closed segment in ladder browse must switch the tab in place (mode=%v closed=%v)", m.mode, m.ordersClosed)
	}
}
