// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/components/header"
)

var sgrParamsRe = regexp.MustCompile("\x1b\\[([0-9;]*)m")

// hasUnderlineSGR reports whether s contains an SGR escape that enables underline
// (parameter 4), robust to underline being merged with a color code in one
// sequence (e.g. "\x1b[4;36m").
func hasUnderlineSGR(s string) bool {
	for _, m := range sgrParamsRe.FindAllStringSubmatch(s, -1) {
		for _, p := range strings.Split(m[1], ";") {
			if p == "4" {
				return true
			}
		}
	}
	return false
}

// multiAccountModel builds a private, sized model that subscribed several
// sub-accounts (active on the first), so the '@' switcher is live.
func multiAccountModel(t *testing.T, seqs ...int) model {
	t.Helper()
	m := newModel(Config{
		Symbols:     []string{"btc_krw", "eth_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		KeyName:     "testkey",
		BaseURL:     "http://127.0.0.1:9999",
		AccountSeq:  seqs[0],
		AccountSeqs: seqs,
		Now:         func() int64 { return 1_700_000_000_000 },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	return mm.(model)
}

func TestAccountSwitchArrowAndEnter(t *testing.T) {
	m := multiAccountModel(t, 1, 2, 3)
	if m.accountSeq() != 1 {
		t.Fatalf("initial active = %d, want 1", m.accountSeq())
	}
	m = send(t, m, k('@', "@"))
	if m.mode != modeAccountSwitch {
		t.Fatalf("@ did not open the switcher (mode=%v)", m.mode)
	}
	if m.acctCursor != 0 {
		t.Fatalf("cursor opened at %d, want the active account's index 0", m.acctCursor)
	}
	// Move to the third account and switch to it.
	m = send(t, m, special(tea.KeyDown))
	m = send(t, m, special(tea.KeyDown))
	m = send(t, m, special(tea.KeyEnter))
	if m.mode != modeNormal {
		t.Fatalf("enter did not close the switcher (mode=%v)", m.mode)
	}
	if m.accountSeq() != 3 {
		t.Fatalf("active after switch = %d, want 3", m.accountSeq())
	}
	// The sub-models capture the account; a switch must re-stamp all three.
	if m.order.accountSeq != 3 || m.ladder.accountSeq != 3 || m.funding.accountSeq != 3 {
		t.Fatalf("sub-models not re-stamped: order=%d ladder=%d funding=%d",
			m.order.accountSeq, m.ladder.accountSeq, m.funding.accountSeq)
	}
}

func TestAccountSwitchEscKeepsActive(t *testing.T) {
	m := multiAccountModel(t, 1, 2)
	m = send(t, m, k('@', "@"))
	m = send(t, m, special(tea.KeyDown)) // highlight account 2
	m = send(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal {
		t.Fatalf("esc did not close the switcher (mode=%v)", m.mode)
	}
	if m.accountSeq() != 1 {
		t.Fatalf("esc must not switch — active = %d, want 1", m.accountSeq())
	}
}

func TestAccountSwitchSingleAccountInert(t *testing.T) {
	m := multiAccountModel(t, 2) // one account
	m = send(t, m, k('@', "@"))
	if m.mode != modeNormal {
		t.Fatalf("@ with one account must not open a popup (mode=%v)", m.mode)
	}
	if m.toast.text == "" {
		t.Fatalf("@ with one account should toast an explanation")
	}
}

// A public session has no account channels, so the '@' switcher is a no-op even
// if the model somehow carries several accountSeqs (defense in depth — the
// command layer rejects --account-seq with --public).
func TestAccountSwitchInertInPublicMode(t *testing.T) {
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     false,
		BaseURL:     "http://127.0.0.1:9999",
		AccountSeq:  1,
		AccountSeqs: []int{1, 2},
		Now:         func() int64 { return 1_700_000_000_000 },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = mm.(model)
	m = send(t, m, k('@', "@"))
	if m.mode != modeNormal {
		t.Fatalf("@ in public mode must not open the switcher (mode=%v)", m.mode)
	}
}

// clickAccountRow clicks the switcher row for the account at list index i.
func clickAccountRow(t *testing.T, m model, i int) model {
	t.Helper()
	parts := m.accountSwitchParts()
	// Content lines: 0 title, 1 blank, 2 header, 3.. the (windowed) account rows.
	col, row := m.overlayLineOrigin(parts.lines, 3+(i-m.acctScroll))
	return send(t, m, mclick(col, row))
}

// hintCapX finds the screen (x,y) of the switcher hint cap that presses `key`.
func hintCapX(t *testing.T, m model, key string) (x, y int) {
	t.Helper()
	parts := m.accountSwitchParts()
	cl := parts.clicks[len(parts.clicks)-1] // the hint line is the last clickable line
	col, row := m.overlayLineOrigin(parts.lines, cl.idx)
	for _, h := range cl.hits {
		if h.Send.String() == key {
			return col + h.X, row
		}
	}
	t.Fatalf("no clickable %q cap on the switcher hint line", key)
	return 0, 0
}

// A click selects a row (moves the highlight) but does NOT switch — the active
// account is untouched until the selection is committed. A second click on the
// now-selected row confirms it, exactly like enter.
func TestAccountSwitchClickSelectsThenConfirms(t *testing.T) {
	m := multiAccountModel(t, 1, 2, 3)
	m = send(t, m, k('@', "@"))
	m = clickAccountRow(t, m, 2) // click seq 3: selects it, no switch
	if m.mode != modeAccountSwitch {
		t.Fatalf("a row click must not close the popup (mode=%v)", m.mode)
	}
	if m.accountSeq() != 1 {
		t.Fatalf("a row click must not switch yet; active = %d, want 1", m.accountSeq())
	}
	if m.acctCursor != 2 {
		t.Fatalf("a row click must move the selection; cursor = %d, want 2", m.acctCursor)
	}
	// The chevron still marks the active (seq 1) account, not the selection.
	got := plain(m.renderAccountSwitch())
	if !strings.Contains(got, "› 1") {
		t.Fatalf("the active account (1) must still carry the chevron; got %q", got)
	}
	// A second click on the already-selected row confirms, like enter.
	m = clickAccountRow(t, m, 2)
	if m.mode != modeNormal {
		t.Fatalf("clicking the selected row must confirm and close (mode=%v)", m.mode)
	}
	if m.accountSeq() != 3 {
		t.Fatalf("confirm-click active = %d, want 3", m.accountSeq())
	}
}

// The popup's hint caps are clickable like every other modal's: clicking "enter"
// commits the selection, and clicking "esc" cancels without switching.
func TestAccountSwitchHintCapsClickable(t *testing.T) {
	// enter cap commits the selection.
	m := multiAccountModel(t, 1, 2, 3)
	m = send(t, m, k('@', "@"))
	m = clickAccountRow(t, m, 2) // select seq 3 (no switch yet)
	x, y := hintCapX(t, m, "enter")
	m = send(t, m, mclick(x, y))
	if m.mode != modeNormal || m.accountSeq() != 3 {
		t.Fatalf("clicking enter must commit the selection; mode=%v active=%d", m.mode, m.accountSeq())
	}

	// esc cap cancels without switching.
	m = multiAccountModel(t, 1, 2, 3)
	m = send(t, m, k('@', "@"))
	m = clickAccountRow(t, m, 2) // select seq 3
	x, y = hintCapX(t, m, "esc")
	m = send(t, m, mclick(x, y))
	if m.mode != modeNormal {
		t.Fatalf("clicking esc must dismiss the popup (mode=%v)", m.mode)
	}
	if m.accountSeq() != 1 {
		t.Fatalf("clicking esc must not switch; active = %d, want 1", m.accountSeq())
	}
}

// The switcher is a two-column table (seq · name); only seq 1 is named "main".
func TestAccountSwitchTableColumns(t *testing.T) {
	m := multiAccountModel(t, 1, 2, 3)
	m = send(t, m, k('@', "@"))
	got := plain(m.renderAccountSwitch())
	for _, want := range []string{"seq", "name", "main", "› 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("switcher table missing %q; got %q", want, got)
		}
	}
}

// A long account list scrolls within the fixed window instead of growing the box:
// the box height is capped and stepping the cursor past the bottom moves the
// window (acctScroll), keeping the cursor visible.
func TestAccountSwitchScrollsLongList(t *testing.T) {
	seqs := make([]int, 20)
	for i := range seqs {
		seqs[i] = i + 1
	}
	m := multiAccountModel(t, seqs...)
	m = send(t, m, k('@', "@"))
	if h := strings.Count(m.renderAccountSwitch(), "\n") + 1; h > accountSwitchMaxRows+6 {
		t.Fatalf("popup height %d exceeds the fixed window (max rows %d + chrome)", h, accountSwitchMaxRows)
	}
	// Step to the last account; the window must have scrolled to keep it visible.
	m = send(t, m, special(tea.KeyEnd))
	if m.acctCursor != len(seqs)-1 {
		t.Fatalf("end did not move the cursor to the last account; got %d", m.acctCursor)
	}
	if m.acctScroll == 0 {
		t.Fatal("stepping to the last account must scroll the window")
	}
	if m.acctCursor < m.acctScroll || m.acctCursor >= m.acctScroll+accountSwitchMaxRows {
		t.Fatalf("cursor %d out of the visible window [%d,%d)", m.acctCursor, m.acctScroll, m.acctScroll+accountSwitchMaxRows)
	}
	got := plain(m.renderAccountSwitch())
	if !strings.Contains(got, "/20") {
		t.Fatalf("a scrolling list should show a position indicator; got %q", got)
	}
}

func TestAccountSwitchClickOutsideCloses(t *testing.T) {
	m := multiAccountModel(t, 1, 2, 3)
	m = send(t, m, k('@', "@"))
	m = send(t, m, mclick(0, 0)) // top-left corner, outside the centered box
	if m.mode != modeNormal {
		t.Fatalf("a click outside the box must close the switcher (mode=%v)", m.mode)
	}
	if m.accountSeq() != 1 {
		t.Fatalf("closing must not switch — active = %d, want 1", m.accountSeq())
	}
}

func TestAccountChipClickOpensSwitcher(t *testing.T) {
	m := multiAccountModel(t, 1, 2, 3)
	start, w, ok := header.AcctChipSpan(m.headerKey())
	if !ok || w == 0 {
		t.Fatalf("expected an acct chip span, got start=%d w=%d ok=%v", start, w, ok)
	}
	m = send(t, m, mclick(start, 0)) // click the acct chip on the header row
	if m.mode != modeAccountSwitch {
		t.Fatalf("clicking the acct chip must open the switcher (mode=%v)", m.mode)
	}
}

func TestAccountHeaderChipShowsActive(t *testing.T) {
	m := multiAccountModel(t, 1, 2, 3)
	if got := plain(m.renderHeader()); !strings.Contains(got, "accountSeq:1") {
		t.Fatalf("header should show the active account chip; got %q", got)
	}
	// Switch to account 2; the header chip must follow.
	m = send(t, m, k('@', "@"))
	m = send(t, m, special(tea.KeyDown))
	m = send(t, m, special(tea.KeyEnter))
	if got := plain(m.renderHeader()); !strings.Contains(got, "accountSeq:2") {
		t.Fatalf("header chip did not follow the switch; got %q", got)
	}
}

// A switch must never happen while a money action is in flight — the header
// would flip account under a place/cancel/withdrawal still on the wire.
func TestAccountSwitchBlockedWhileMoneyInFlight(t *testing.T) {
	m := multiAccountModel(t, 1, 2, 3)
	m.orderInFlight = true // a place/cancel is on the wire (busy view esc'd to normal)
	m = send(t, m, k('@', "@"))
	if m.mode != modeNormal {
		t.Fatalf("'@' must not open the switcher while a money action is in flight (mode=%v)", m.mode)
	}
	if m.toast.text == "" || !m.toast.isError {
		t.Fatalf("expected an error toast explaining the block; got %+v", m.toast)
	}
	// The header acct-chip click is gated identically.
	m2 := multiAccountModel(t, 1, 2, 3)
	m2.orderInFlight = true
	start, _, ok := header.AcctChipSpan(m2.headerKey())
	if !ok {
		t.Fatal("expected an acct chip")
	}
	m2 = send(t, m2, mclick(start, 0))
	if m2.mode != modeNormal {
		t.Fatalf("chip click must not open the switcher while money in flight (mode=%v)", m2.mode)
	}
	if m2.accountSeq() != 1 {
		t.Fatalf("blocked switch must not change the active account (got %d)", m2.accountSeq())
	}
}

// '@' is inert in the order-entry modes (they own the keyboard) — covering "no
// switching in order mode / with an armed order", which live in those modes.
func TestAccountSwitchUnavailableInEntryModes(t *testing.T) {
	for _, key := range []string{"b", "t"} { // order panel, trade ladder
		m := send(t, multiAccountModel(t, 1, 2), k([]rune(key)[0], key))
		if m.mode == modeNormal {
			t.Fatalf("setup: %q did not enter an entry mode", key)
		}
		before := m.mode
		m = send(t, m, k('@', "@"))
		if m.mode == modeAccountSwitch {
			t.Fatalf("'@' must not open the switcher from %q mode", key)
		}
		if m.mode != before {
			t.Fatalf("'@' from %q mode changed mode %v -> %v", key, before, m.mode)
		}
	}
}

// A chip click in a docked entry mode toasts instead of opening the switcher,
// and stays put.
func TestAccountChipClickTogglesToastInEntryModes(t *testing.T) {
	for _, key := range []string{"b", "t"} { // order panel, trade ladder
		m := send(t, multiAccountModel(t, 1, 2), k([]rune(key)[0], key))
		if m.mode == modeNormal {
			t.Fatalf("setup: %q did not enter an entry mode", key)
		}
		before := m.mode
		start, _, ok := header.AcctChipSpan(m.headerKey())
		if !ok {
			t.Fatalf("%q mode: expected an acct chip", key)
		}
		m = send(t, m, mclick(start, 0))
		if m.mode != before {
			t.Fatalf("chip click from %q mode must not change mode (%v -> %v)", key, before, m.mode)
		}
		if m.toast.text == "" {
			t.Fatalf("chip click from %q mode must toast why switching is unavailable", key)
		}
	}
}

// The acct chip is underlined (the clickable affordance) only in a multi-account
// session; single-account stays dim.
func TestAccountChipUnderlinedForMultiAccountOnly(t *testing.T) {
	prof := tea.ColorProfileMsg{Profile: colorprofile.TrueColor}
	multi := send(t, multiAccountModel(t, 1, 2), prof).renderHeader()
	one := send(t, multiAccountModel(t, 1), prof).renderHeader()

	// Same plain "accountSeq:1" either way — the cue is style, not text.
	for _, h := range []string{multi, one} {
		if p := plain(h); !strings.Contains(p, "accountSeq:1") || strings.Contains(p, "accountSeq:1+") {
			t.Fatalf("chip text should be plain \"accountSeq:1\"; got %q", p)
		}
	}
	if !hasUnderlineSGR(multi) {
		t.Fatalf("multi-account chip must be underlined (clickable affordance); got %q", multi)
	}
	if hasUnderlineSGR(one) {
		t.Fatalf("single-account chip must not be underlined; got %q", one)
	}
}

// runCmds executes a tea.Cmd (flattening a tea.Batch) and returns the messages
// it produced, so a fetch command's side effects and result can be inspected.
func runCmds(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmds(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// Fee estimates are per sub-account: the fetch is stamped with the active
// account, every reply lands under its own {account, symbol} key, and reads
// resolve for the active account only.
func TestOrderFeesFollowActiveAccount(t *testing.T) {
	var seqs []int
	feeFn := func(symbol string, accountSeq int) (FeeRates, error) {
		seqs = append(seqs, accountSeq)
		return FeeRates{MakerRate: "0.001", TakerRate: "0.001"}, nil
	}
	store := state.New(state.Config{}, func() int64 { return 0 })
	o := newOrderModel(store, 2, nil, nil, feeFn, []int{10, 25, 50, 100})

	msgs := runCmds(o.fetchMeta("btc_krw"))
	var fm orderFeesMsg
	found := false
	for _, mm := range msgs {
		if x, ok := mm.(orderFeesMsg); ok {
			fm, found = x, true
		}
	}
	if !found {
		t.Fatal("fetchMeta produced no orderFeesMsg")
	}
	if len(seqs) != 1 || seqs[0] != 2 {
		t.Fatalf("fetchFees called with %v, want [2] (the active account)", seqs)
	}
	if fm.accountSeq != 2 {
		t.Fatalf("orderFeesMsg.accountSeq=%d, want 2", fm.accountSeq)
	}

	// A reply for the active account lands and resolves.
	o = o.applyFees(fm)
	o.draft.symbol = "btc_krw"
	if o.symFees() == nil {
		t.Fatal("a fee reply for the active account should be cached")
	}

	// A reply for another account is cached under ITS key — invisible to the
	// active account's read, present the moment that account becomes active.
	o2 := newOrderModel(store, 2, nil, nil, feeFn, []int{10})
	o2.draft.symbol = "btc_krw"
	o2 = o2.applyFees(orderFeesMsg{symbol: "btc_krw", accountSeq: 1, fees: FeeRates{MakerRate: "9"}})
	if o2.symFees() != nil {
		t.Fatal("another account's fees must not resolve for the active account")
	}
	o2.accountSeq = 1
	if f := o2.symFees(); f == nil || f.MakerRate != "9" {
		t.Fatalf("the reply must be cached under its own account, got %+v", f)
	}
}

// A switch re-stamps the order sub-model's account and leaves the fee cache
// alone: the old account's entry stays (a switch back reads it instantly),
// the new account's entry is simply missing until fetched.
func TestAccountSwitchKeepsOrderFeeCache(t *testing.T) {
	m := multiAccountModel(t, 1, 2)
	m.order.fees[feesKey{accountSeq: 1, symbol: "btc_krw"}] = FeeRates{MakerRate: "0.001"}
	m = send(t, m, k('@', "@"))
	m = send(t, m, special(tea.KeyDown))
	m = send(t, m, special(tea.KeyEnter)) // switch to account 2
	if m.order.accountSeq != 2 {
		t.Fatalf("order sub-model account=%d, want 2", m.order.accountSeq)
	}
	if m.order.symFeesFor("btc_krw") != nil {
		t.Fatal("account 2 must not resolve account 1's cached fees")
	}
	if f, ok := m.order.fees[feesKey{accountSeq: 1, symbol: "btc_krw"}]; !ok || f.MakerRate != "0.001" {
		t.Fatal("the switch must keep account 1's cached fees")
	}
}

func TestAccountSwitchFooterHintOnlyWhenMulti(t *testing.T) {
	multi := plain(multiAccountModel(t, 1, 2).renderFooter())
	if !strings.Contains(multi, "account") {
		t.Fatalf("multi-account footer should advertise the '@' switcher; got %q", multi)
	}
	single := plain(multiAccountModel(t, 1).renderFooter())
	if strings.Contains(single, "@") {
		t.Fatalf("single-account footer must not advertise the switcher; got %q", single)
	}
}

// balanceBackfill builds a per-account /v2/balance backfill event, stamped with
// the sub-account it was fetched for (the AccountSeq the store keys it under).
func balanceBackfill(seq int, payload string) stream.Data {
	ev := dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance", payload)
	ev.AccountSeq = &seq
	return ev
}

// TestAccountSwitchRerendersPerAccountPanes is the regression guard for the
// memoized per-account panes (balances, fills). Each pane caches its render via
// uikit.Memo keyed on a Key; the Data it shows is per-account (BalancesFor /
// FillsFor), but a pure account switch bumps no store revision — so the Key MUST
// carry the active accountSeq, or the memo replays the previous account's render
// after the switch. We render BEFORE switching (to populate the cache) and again
// after, and assert the panes follow the active account.
func TestAccountSwitchRerendersPerAccountPanes(t *testing.T) {
	m := multiAccountModel(t, 1, 2)
	// Bring the private feed up so the fills pane renders rows (not "loading…").
	m = feedNotice(t, m, stream.Connected, stream.LevelInfo, map[string]any{"endpoint": "private"})

	// Distinct holdings per account: account 1 holds SOL, account 2 holds XRP.
	m = feed(t, m, balanceBackfill(1,
		`[{"currency":"krw","balance":"100","available":"100","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"sol","balance":"5","available":"5","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	m = feed(t, m, balanceBackfill(2,
		`[{"currency":"krw","balance":"200","available":"200","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"xrp","balance":"9","available":"9","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	// Distinct fills per account: account 1 traded btc_krw, account 2 ada_krw.
	m = feed(t, m, dataEvent("myTrade", "btc_krw", stream.OriginRealtime, 100, "",
		`{"symbol":"btc_krw","channelType":"myTrade","trade":{"accountSeq":1,"trades":[
			{"tradeId":1,"orderId":1,"side":"buy","price":"1","qty":"1","fee":"0","feeCurrency":"krw","filledAt":990,"isTaker":true}]}}`))
	m = feed(t, m, dataEvent("myTrade", "ada_krw", stream.OriginRealtime, 100, "",
		`{"symbol":"ada_krw","channelType":"myTrade","trade":{"accountSeq":2,"trades":[
			{"tradeId":2,"orderId":2,"side":"buy","price":"1","qty":"1","fee":"0","feeCurrency":"krw","filledAt":991,"isTaker":true}]}}`))

	has := func(s, sub string) bool { return strings.Contains(strings.ToLower(s), sub) }

	// Render account 1's panes first — this is what seeds the memo cache.
	bal1 := plain(m.viewBalances(30, 12))
	fill1 := plain(m.viewFills(30, 12))
	if !has(bal1, "sol") || has(bal1, "xrp") {
		t.Fatalf("account 1 balances should show SOL not XRP; got %q", bal1)
	}
	if !has(fill1, "btc") || has(fill1, "ada") {
		t.Fatalf("account 1 fills should show BTC not ADA; got %q", fill1)
	}

	// Switch to account 2. The panes must now show account 2's data — a stale
	// memo (the bug) would replay account 1's SOL/BTC render.
	m.switchAccount(2)
	if m.accountSeq() != 2 {
		t.Fatalf("switch failed, active = %d", m.accountSeq())
	}
	bal2 := plain(m.viewBalances(30, 12))
	fill2 := plain(m.viewFills(30, 12))
	if !has(bal2, "xrp") || has(bal2, "sol") {
		t.Fatalf("after switch, balances should show XRP not SOL (stale memo?); got %q", bal2)
	}
	if !has(fill2, "ada") || has(fill2, "btc") {
		t.Fatalf("after switch, fills should show ADA not BTC (stale memo?); got %q", fill2)
	}
}
