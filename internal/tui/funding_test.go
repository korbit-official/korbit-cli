// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/tui/components/header"
)

// fakeFunding is a canned Funding seam that counts calls, so the tests can
// assert the load-bearing property that a money mover is sent exactly once.
type fakeFunding struct {
	currencies []FundingCurrency
	depAddrs   []FundingDepositAddress
	wdAddrs    []FundingWithdrawAddress
	deposits   []FundingTransfer
	withdraws  []FundingTransfer

	withdrawErr error
	receipt     FundingReceipt

	withdrawCalls   []FundingWithdrawOrder
	krwDepositCalls []string
	krwWithdrawCall []string
	cancelCalls     []int64
	generateCalls   int
	historyCalls    int
	// seqs records the accountSeq passed to each account-scoped call, so a test
	// can assert the screen always sends the currently active account.
	seqs []int
}

func (f *fakeFunding) Currencies() ([]FundingCurrency, error) { return f.currencies, nil }
func (f *fakeFunding) DepositAddresses(accountSeq int) ([]FundingDepositAddress, error) {
	f.seqs = append(f.seqs, accountSeq)
	return f.depAddrs, nil
}
func (f *fakeFunding) GenerateDepositAddress(accountSeq int, currency, network string) (FundingDepositAddress, error) {
	f.seqs = append(f.seqs, accountSeq)
	f.generateCalls++
	a := FundingDepositAddress{Currency: currency, Network: network, Address: "addr-" + currency}
	f.depAddrs = append(f.depAddrs, a)
	return a, nil
}
func (f *fakeFunding) WithdrawAddresses(accountSeq int) ([]FundingWithdrawAddress, error) {
	f.seqs = append(f.seqs, accountSeq)
	return f.wdAddrs, nil
}
func (f *fakeFunding) Withdrawable(accountSeq int, currency string) (FundingWithdrawable, error) {
	f.seqs = append(f.seqs, accountSeq)
	return FundingWithdrawable{Currency: currency, Amount: "5", InUse: "0"}, nil
}
func (f *fakeFunding) DepositHistory(accountSeq int, currency string, limit int) ([]FundingTransfer, error) {
	f.seqs = append(f.seqs, accountSeq)
	f.historyCalls++
	return f.deposits, nil
}
func (f *fakeFunding) WithdrawHistory(accountSeq int, currency string, limit int) ([]FundingTransfer, error) {
	f.seqs = append(f.seqs, accountSeq)
	f.historyCalls++
	return f.withdraws, nil
}
func (f *fakeFunding) RequestWithdrawal(accountSeq int, o FundingWithdrawOrder) (FundingReceipt, error) {
	f.seqs = append(f.seqs, accountSeq)
	f.withdrawCalls = append(f.withdrawCalls, o)
	if f.withdrawErr != nil {
		return FundingReceipt{}, f.withdrawErr
	}
	return f.receipt, nil
}
func (f *fakeFunding) CancelWithdrawal(accountSeq int, id int64) error {
	f.seqs = append(f.seqs, accountSeq)
	f.cancelCalls = append(f.cancelCalls, id)
	return nil
}
func (f *fakeFunding) RequestKRWDeposit(accountSeq int, amount string) error {
	f.seqs = append(f.seqs, accountSeq)
	f.krwDepositCalls = append(f.krwDepositCalls, amount)
	return nil
}
func (f *fakeFunding) RequestKRWWithdraw(accountSeq int, amount string) error {
	f.seqs = append(f.seqs, accountSeq)
	f.krwWithdrawCall = append(f.krwWithdrawCall, amount)
	return nil
}

// testCatalog is a small currency universe: btc (one network, full
// constraints), eth (two networks), ada, and krw (fiat, no networks).
func testCatalog() []FundingCurrency {
	return []FundingCurrency{
		{Currency: "btc", FullName: "Bitcoin", DefaultNetwork: "BTC", Networks: []FundingNetwork{
			{Name: "BTC", DepositLaunched: true, WithdrawalLaunched: true,
				WithdrawalFee: "0.0009", WithdrawalMin: "0.001", WithdrawalPrecision: 8},
		}},
		{Currency: "eth", FullName: "Ethereum", DefaultNetwork: "ETH", Networks: []FundingNetwork{
			{Name: "ETH", DepositLaunched: true, WithdrawalLaunched: true, WithdrawalPrecision: -1},
			{Name: "ARB", DepositLaunched: true, WithdrawalLaunched: true, WithdrawalPrecision: -1},
		}},
		{Currency: "ada", FullName: "Cardano", DefaultNetwork: "ADA", Networks: []FundingNetwork{
			{Name: "ADA", DepositLaunched: true, WithdrawalLaunched: true, WithdrawalPrecision: -1},
		}},
		{Currency: "krw", FullName: "Korean won"},
	}
}

func fundingTestModel(t *testing.T, fk *fakeFunding) model {
	t.Helper()
	m := newModel(Config{
		Symbols:     []string{"btc_krw", "eth_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Funding:     fk,
		KeyName:     "testkey",
		BaseURL:     "http://127.0.0.1:9999",
		Now:         func() int64 { return 1_700_000_000_000 },
		StopSession: func() {},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	return mm.(model)
}

// seedFundingBalances feeds a balance snapshot: krw and eth held, btc zero.
func seedFundingBalances(t *testing.T, m model) model {
	t.Helper()
	return feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 100, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"900000","tradeInUse":"100000","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"eth","balance":"2","available":"2","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"0","available":"0","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
}

// drainFunding executes a command tree synchronously, feeding funding messages
// back into the model. Non-funding messages (spinner ticks, clock ticks) are
// dropped — re-feeding them would re-arm their timer loops forever.
func drainFunding(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if msg == nil {
		return m
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			m = drainFunding(t, m, c)
		}
		return m
	}
	if _, ok := msg.(fundingMsg); !ok {
		return m
	}
	mm, next := m.Update(msg)
	return drainFunding(t, mm.(model), next)
}

// collectMsgs flattens a command tree into the messages it produces.
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, collectMsgs(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// openFundingScreen presses f and drains the entry fetches.
func openFundingScreen(t *testing.T, m model) model {
	t.Helper()
	m2, cmd := press(t, m, k('f', "f"))
	if m2.mode != modeFunding {
		t.Fatalf("f must enter the funding screen, mode=%d", m2.mode)
	}
	return drainFunding(t, m2, cmd)
}

// cursorTo focuses the request pane and walks the field cursor to fld.
func cursorTo(t *testing.T, m model, fld formField) model {
	t.Helper()
	for i := 0; m.funding.focus != fundingFocusForm; i++ {
		if i > 3 {
			t.Fatal("tab never reached the request pane")
		}
		m, _ = press(t, m, special(tea.KeyTab))
	}
	idx := m.funding.fieldIndex(fld)
	if idx < 0 {
		t.Fatalf("field %d not offered; fields=%v", fld, m.funding.formFields())
	}
	// ↑ to the top (claimed by field navigation even while the amount input is
	// under the cursor), then ↓ to the target.
	for range m.funding.formFields() {
		m, _ = press(t, m, special(tea.KeyUp))
	}
	for i := 0; i < idx; i++ {
		m, _ = press(t, m, special(tea.KeyDown))
	}
	return m
}

// TestFundingMainOnlyOnSubAccount: on a non-main active sub-account the funding
// screen still operates on the ACTIVE account (every fetch carries accountSeq 2,
// never defaulting to main 1), shows a main-account-only warning, and neither '@'
// nor a chip click opens the switcher — each toasts and stays put.
func TestFundingMainOnlyOnSubAccount(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := newModel(Config{
		Symbols:     []string{"btc_krw"},
		Private:     true,
		Trader:      &fakeTrader{},
		Funding:     fk,
		KeyName:     "testkey",
		BaseURL:     "http://127.0.0.1:9999",
		AccountSeq:  2,
		AccountSeqs: []int{1, 2},
		Now:         func() int64 { return 1_700_000_000_000 },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m = mm.(model)
	if m.accountSeq() != 2 {
		t.Fatalf("setup: active account = %d, want 2", m.accountSeq())
	}

	m2, cmd := press(t, m, k('f', "f"))
	if m2.mode != modeFunding {
		t.Fatalf("f must enter the funding screen, mode=%d", m2.mode)
	}
	m2 = drainFunding(t, m2, cmd)

	// The screen operates on the active account — every fetch carries seq 2, and
	// nothing ever defaults to main (1).
	if len(fk.seqs) == 0 {
		t.Fatal("funding fired no fetches on a non-main account (must use the active seq, not gate)")
	}
	for _, s := range fk.seqs {
		if s != 2 {
			t.Fatalf("funding used accountSeq %d, want the active 2 (no defaulting to main)", s)
		}
	}
	if frame := plain(m2.render()); !strings.Contains(frame, "main-account-only") {
		t.Fatalf("funding screen did not warn it is main-account-only:\n%s", frame)
	}

	// '@' stays in the funding screen (the screen owns the keyboard).
	m3, _ := press(t, m2, k('@', "@"))
	if m3.mode != modeFunding {
		t.Fatalf("'@' must not leave the funding screen, mode=%v", m3.mode)
	}

	// A chip click doesn't leave funding either — it toasts instead.
	start, _, ok := header.AcctChipSpan(m3.headerKey())
	if !ok {
		t.Fatal("expected an acct chip on the funding screen header")
	}
	m4 := send(t, m3, mclick(start, 0))
	if m4.mode != modeFunding {
		t.Fatalf("chip click must not leave the funding screen, mode=%v", m4.mode)
	}
	if m4.toast.text == "" {
		t.Fatal("chip click on the funding screen must toast why switching is unavailable")
	}
}

// TestFundingDiscardsCrossAccountReply: a hist/withdrawable reply that lands
// after the active account changed (it was issued for a different account) must
// be dropped, never stamped onto the new account's cache. Reachable because the
// screen now fetches on every account, not just main.
func TestFundingDiscardsCrossAccountReply(t *testing.T) {
	m := fundingTestModel(t, &fakeFunding{currencies: testCatalog()})
	if m.funding.accountSeq != 1 {
		t.Fatalf("setup: active account = %d, want 1", m.funding.accountSeq)
	}
	// A reply issued for account 2 (e.g. an in-flight fetch from before a switch
	// to main) carrying a server rejection.
	histErr := errors.New("ACCOUNT_SEQ_NOT_ALLOWED")
	f, _ := m.funding.apply(fundingHistMsg{accountSeq: 2, cur: "btc", dir: fundingDeposit, seq: 1, err: histErr})
	if h := f.histFor(histKey{"btc", fundingDeposit}); h.err != "" || h.loaded {
		t.Fatalf("cross-account hist reply must be discarded, got err=%q loaded=%v", h.err, h.loaded)
	}
	f, _ = m.funding.apply(fundingAmtMsg{accountSeq: 2, cur: "btc", seq: 1, err: histErr})
	if a := f.amtFor("btc"); a.err != "" || a.loaded {
		t.Fatalf("cross-account withdrawable reply must be discarded, got err=%q loaded=%v", a.err, a.loaded)
	}

	// Address + generate replies must be discarded too — they carry no other
	// guard, so a stale account-2 reply must not populate account 1's caches.
	f, _ = m.funding.apply(fundingDepAddrsMsg{accountSeq: 2, list: []FundingDepositAddress{{Currency: "btc", Address: "acct2-dep"}}})
	if f.depAddrsReady || len(f.depAddrs) != 0 {
		t.Fatalf("cross-account deposit-address reply must be discarded, got ready=%v addrs=%+v", f.depAddrsReady, f.depAddrs)
	}
	f, _ = m.funding.apply(fundingWdAddrsMsg{accountSeq: 2, list: []FundingWithdrawAddress{{Currency: "btc", Address: "acct2-wd"}}})
	if f.wdAddrsReady || len(f.wdAddrs) != 0 {
		t.Fatalf("cross-account withdraw-address reply must be discarded, got ready=%v addrs=%+v", f.wdAddrsReady, f.wdAddrs)
	}
	f, _ = m.funding.apply(fundingGenMsg{accountSeq: 2, cur: "btc", addr: FundingDepositAddress{Currency: "btc", Address: "acct2-gen"}})
	if len(f.depAddrs) != 0 {
		t.Fatalf("cross-account generate reply must be discarded, got addrs=%+v", f.depAddrs)
	}

	// Same account (1) but a superseded generation: a stale address reply must be
	// dropped in favor of the newest fetch. Pretend the latest issued dep fetch
	// was generation 5; a reply for generation 3 is stale.
	m.funding.depSeq = 5
	f, _ = m.funding.apply(fundingDepAddrsMsg{accountSeq: 1, seq: 3, list: []FundingDepositAddress{{Currency: "btc", Address: "stale"}}})
	if f.depAddrsReady || len(f.depAddrs) != 0 {
		t.Fatalf("superseded same-account deposit-address reply must be discarded, got ready=%v addrs=%+v", f.depAddrsReady, f.depAddrs)
	}
	f, _ = m.funding.apply(fundingDepAddrsMsg{accountSeq: 1, seq: 5, list: []FundingDepositAddress{{Currency: "btc", Address: "current"}}})
	if !f.depAddrsReady || len(f.depAddrs) != 1 || f.depAddrs[0].Address != "current" {
		t.Fatalf("current-generation deposit-address reply must be accepted, got ready=%v addrs=%+v", f.depAddrsReady, f.depAddrs)
	}

	// Same-account superseded — withdraw addresses (wdSeq guard).
	m.funding.wdSeq = 7
	f, _ = m.funding.apply(fundingWdAddrsMsg{accountSeq: 1, seq: 4, list: []FundingWithdrawAddress{{Currency: "btc", Address: "stale"}}})
	if f.wdAddrsReady || len(f.wdAddrs) != 0 {
		t.Fatalf("superseded same-account withdraw-address reply must be discarded, got ready=%v addrs=%+v", f.wdAddrsReady, f.wdAddrs)
	}
	f, _ = m.funding.apply(fundingWdAddrsMsg{accountSeq: 1, seq: 7, list: []FundingWithdrawAddress{{Currency: "btc", Address: "current"}}})
	if !f.wdAddrsReady || len(f.wdAddrs) != 1 || f.wdAddrs[0].Address != "current" {
		t.Fatalf("current-generation withdraw-address reply must be accepted, got ready=%v addrs=%+v", f.wdAddrsReady, f.wdAddrs)
	}

	// Same-account superseded — history (per-slot seq guard).
	hk := histKey{"eth", fundingDeposit}
	hs := m.funding.histFor(hk)
	hs.seq, hs.loading = 9, true
	f, _ = m.funding.apply(fundingHistMsg{accountSeq: 1, cur: "eth", dir: fundingDeposit, seq: 6, rows: []FundingTransfer{{ID: 1}}})
	if h := f.histFor(hk); h.loaded || len(h.rows) != 0 {
		t.Fatalf("superseded same-account history reply must be discarded, got loaded=%v rows=%+v", h.loaded, h.rows)
	}
	f, _ = m.funding.apply(fundingHistMsg{accountSeq: 1, cur: "eth", dir: fundingDeposit, seq: 9, rows: []FundingTransfer{{ID: 42}}})
	if h := f.histFor(hk); !h.loaded || len(h.rows) != 1 {
		t.Fatalf("current-generation history reply must be accepted, got loaded=%v rows=%+v", h.loaded, h.rows)
	}

	// Same-account superseded — withdrawable amount (per-slot seq guard).
	as := m.funding.amtFor("eth")
	as.seq, as.loading = 11, true
	f, _ = m.funding.apply(fundingAmtMsg{accountSeq: 1, cur: "eth", seq: 8, v: FundingWithdrawable{Currency: "eth", Amount: "1"}})
	if a := f.amtFor("eth"); a.loaded {
		t.Fatalf("superseded same-account withdrawable reply must be discarded, got loaded=%v", a.loaded)
	}
	f, _ = m.funding.apply(fundingAmtMsg{accountSeq: 1, cur: "eth", seq: 11, v: FundingWithdrawable{Currency: "eth", Amount: "2"}})
	if a := f.amtFor("eth"); !a.loaded || a.v.Amount != "2" {
		t.Fatalf("current-generation withdrawable reply must be accepted, got loaded=%v v=%+v", a.loaded, a.v)
	}
}

func TestFundingCurrencyOrderingAndValues(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	// Only eth_krw has a ready ticker: eth gets an estimated value, btc doesn't.
	m = feed(t, m, dataEvent("ticker", "eth_krw", stream.OriginSnapshot, 1, "",
		`{"type":"ticker","timestamp":1,"symbol":"eth_krw","data":{"close":"3000000"}}`))
	m = openFundingScreen(t, m)

	rows := m.funding.currencyRows()
	var order []string
	for _, r := range rows {
		if r.Divider {
			order = append(order, "—")
			continue
		}
		order = append(order, r.Cur)
	}
	want := []string{"krw", "eth", "—", "ada", "btc"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("currency order = %v, want %v", order, want)
	}
	// eth's estimated value is balance × last (2 × 3,000,000); btc holds zero
	// so it is unheld and shows no value.
	for _, r := range rows {
		switch r.Cur {
		case "eth":
			if r.Value != "6000000" {
				t.Fatalf("eth value = %q, want 6000000", r.Value)
			}
		case "ada", "btc":
			if r.Value != "" {
				t.Fatalf("%s must not show a value (no ticker / no holding), got %q", r.Cur, r.Value)
			}
		case "krw":
			if r.Value != "1000000" {
				t.Fatalf("krw value = %q, want its balance", r.Value)
			}
		}
	}
	// A held asset whose ticker is NOT subscribed sorts after priced ones but
	// stays in the held section: make btc held with no btc_krw ticker.
	m = feed(t, m, dataEvent("myAsset", "", stream.OriginBackfill, 200, "/v2/balance",
		`[{"currency":"krw","balance":"1000000","available":"900000","tradeInUse":"100000","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"eth","balance":"2","available":"2","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"},
		  {"currency":"btc","balance":"1","available":"1","tradeInUse":"0","withdrawalInUse":"0","avgPrice":"0"}]`))
	rows = m.funding.currencyRows()
	idxBTC := fundingSelIdx(rows, "btc")
	idxETH := fundingSelIdx(rows, "eth")
	if idxBTC < 0 || idxETH < 0 || idxBTC < idxETH {
		t.Fatalf("unpriced held btc must sort after priced eth: btc=%d eth=%d", idxBTC, idxETH)
	}
	for _, r := range rows {
		if r.Cur == "btc" && (r.Value != "" || !r.Held) {
			t.Fatalf("held btc without a ticker: value hidden, held section; got value=%q held=%v", r.Value, r.Held)
		}
	}
}

func TestFundingOpenFetchesBaseData(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)

	f := m.funding
	if !f.curReady || !f.depAddrsReady || !f.wdAddrsReady {
		t.Fatalf("entry must load catalog+addresses: cur=%v dep=%v wd=%v", f.curReady, f.depAddrsReady, f.wdAddrsReady)
	}
	if f.selected != "krw" || f.tab != fundingDeposit {
		t.Fatalf("initial detail = %s/%d, want krw deposit", f.selected, f.tab)
	}
	if h := f.hist[histKey{"krw", fundingDeposit}]; h == nil || !h.loaded {
		t.Fatalf("the initial selection's history must be fetched")
	}
}

func TestFundingStepSkipsDivider(t *testing.T) {
	rows := []fundingRow{{Cur: "krw"}, {Cur: "eth"}, {Divider: true}, {Cur: "ada"}}
	if got := fundingStep(rows, 1, 1); got != 3 {
		t.Fatalf("step over divider = %d, want 3", got)
	}
	if got := fundingStep(rows, 3, -1); got != 1 {
		t.Fatalf("step back over divider = %d, want 1", got)
	}
	if got := fundingStep(rows, -1, 1); got != 0 {
		t.Fatalf("step from filtered-out selection = %d, want first row", got)
	}
	if got := fundingStep([]fundingRow{{Divider: true}}, 0, 1); got != -1 {
		t.Fatalf("divider-only list = %d, want -1", got)
	}
}

func TestIsDefiniteRejection(t *testing.T) {
	if !isDefiniteRejection(&output.ApiError{Message: "no", Code: "NO_BALANCE"}) {
		t.Fatal("an API error envelope is a definite rejection")
	}
	if !isDefiniteRejection(output.Usagef("bad amount")) {
		t.Fatal("a validation error is a definite rejection")
	}
	if isDefiniteRejection(errors.New("dial tcp: i/o timeout")) {
		t.Fatal("a transport error is ambiguous, not definite")
	}
}

// withdrawToConfirm drives the request pane to the confirm step: pick the
// first address explicitly (→ on the address row), type the amount, submit.
func withdrawToConfirm(t *testing.T, m model, amount string) model {
	t.Helper()
	m = cursorTo(t, m, ffAddress)
	m, _ = press(t, m, special(tea.KeyRight)) // explicit destination pick
	m = cursorTo(t, m, ffAmount)
	m = typeText(t, m, amount)
	m, _ = press(t, m, special(tea.KeyEnter)) // amount → button
	m, _ = press(t, m, special(tea.KeyEnter)) // review → confirm
	if m.funding.view != fundingConfirmView {
		t.Fatalf("expected the confirm step, view=%d err=%q", m.funding.view, m.funding.formErr)
	}
	return m
}

// withdrawSetup opens funding, selects btc on the withdraw tab, and returns
// the model with the request pane showing the withdraw form.
func withdrawSetup(t *testing.T, fk *fakeFunding) model {
	t.Helper()
	if fk.currencies == nil {
		fk.currencies = testCatalog()
	}
	if fk.wdAddrs == nil {
		fk.wdAddrs = []FundingWithdrawAddress{{Currency: "btc", Network: "BTC", Address: "bc1qexampleexampleexampleexample"}}
	}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)
	m.funding.selected = "btc"
	m.funding.tab = fundingWithdraw
	m.funding.resetForm()
	return m
}

func TestFundingWithdrawFlowSendsExactlyOnce(t *testing.T) {
	fk := &fakeFunding{receipt: FundingReceipt{ID: 9812, Status: "reviewing"}}
	m := withdrawSetup(t, fk)

	// The destination is NEVER auto-selected: submitting without picking one
	// must refuse inline, even with a single registered address.
	m = cursorTo(t, m, ffAmount)
	m = typeText(t, m, "0.01")
	m, _ = press(t, m, special(tea.KeyEnter)) // amount → button
	m, _ = press(t, m, special(tea.KeyEnter)) // review
	if m.funding.view != fundingBrowse || !strings.Contains(m.funding.formErr, "destination") {
		t.Fatalf("submit without an address pick must refuse: view=%d err=%q", m.funding.view, m.funding.formErr)
	}
	m = cursorTo(t, m, ffAddress)
	m, _ = press(t, m, special(tea.KeyRight)) // explicit pick (first address)
	m = cursorTo(t, m, ffButton)
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.funding.view != fundingConfirmView {
		t.Fatalf("a valid submit must go to the confirm step, view=%d err=%q", m.funding.view, m.funding.formErr)
	}
	if len(fk.withdrawCalls) != 0 {
		t.Fatal("nothing may be sent before the confirm")
	}
	m, cmd := press(t, m, special(tea.KeyEnter))
	if !m.funding.actionInFlight || m.funding.view != fundingBusy {
		t.Fatalf("confirm must enter the busy state (inFlight=%v view=%d)", m.funding.actionInFlight, m.funding.view)
	}
	if !m.moneyActionInFlight() {
		t.Fatal("a funding action in flight must trip the TUI-wide money gate")
	}
	m = drainFunding(t, m, cmd)

	if len(fk.withdrawCalls) != 1 {
		t.Fatalf("the withdrawal must be sent exactly once, sent %d times", len(fk.withdrawCalls))
	}
	o := fk.withdrawCalls[0]
	if o.Currency != "btc" || o.Amount != "0.01" || o.Address != fk.wdAddrs[0].Address || o.Network != "BTC" {
		t.Fatalf("unexpected order: %+v", o)
	}
	f := m.funding
	if f.actionInFlight || f.view != fundingBrowse || f.tab != fundingWithdraw || f.focus != fundingFocusHistory {
		t.Fatalf("success must land on the withdraw history: %+v", f.view)
	}
	if f.banner.kind != bannerOK || !strings.Contains(f.banner.text, "9812") {
		t.Fatalf("success banner must carry the receipt, got %q", f.banner.text)
	}
	if h := f.hist[histKey{"btc", fundingWithdraw}]; h == nil || !h.loaded {
		t.Fatal("the landed-on history must be refetched")
	}
	// The accepted request's inputs must not survive into the next one.
	if f.amount.Value() != "" || f.wdAddrIdx != -1 {
		t.Fatalf("an accepted request must clear the form: amount=%q addr=%d", f.amount.Value(), f.wdAddrIdx)
	}
}

func TestFundingAmbiguousOutcomeNeverRetries(t *testing.T) {
	fk := &fakeFunding{withdrawErr: errors.New("read tcp: i/o timeout")}
	m := withdrawSetup(t, fk)

	m = withdrawToConfirm(t, m, "0.01")
	m, cmd := press(t, m, special(tea.KeyEnter))
	m = drainFunding(t, m, cmd)

	if len(fk.withdrawCalls) != 1 {
		t.Fatalf("an ambiguous failure must never be retried, sent %d times", len(fk.withdrawCalls))
	}
	f := m.funding
	if f.view != fundingBrowse || f.banner.kind != bannerWarn || !strings.Contains(f.banner.text, "UNKNOWN") {
		t.Fatalf("ambiguous outcome must land on browse with the outcome-unknown banner, got view=%d banner=%q", f.view, f.banner.text)
	}
	// The warning must be VISIBLE at the default layout, not just set in state:
	// the request pane renders the banner first, so tail-truncation can never
	// drop it behind the field rows.
	frame := plain(m.render())
	if !strings.Contains(frame, "UNKNOWN") {
		t.Fatalf("the outcome-unknown banner must be visible in the rendered frame:\n%s", frame)
	}
}

func TestFundingDefiniteRejectionReturnsToForm(t *testing.T) {
	fk := &fakeFunding{withdrawErr: &output.ApiError{Message: "insufficient balance", Code: "NO_BALANCE"}}
	m := withdrawSetup(t, fk)

	m = withdrawToConfirm(t, m, "0.01")
	m, cmd := press(t, m, special(tea.KeyEnter))
	m = drainFunding(t, m, cmd)

	f := m.funding
	if f.view != fundingBrowse || f.focus != fundingFocusForm {
		t.Fatalf("a definite rejection must return to the form, view=%d focus=%d", f.view, f.focus)
	}
	if !strings.Contains(f.formErr, "NO_BALANCE") {
		t.Fatalf("the rejection must show inline, got %q", f.formErr)
	}
	if got := strings.TrimSpace(f.amount.Value()); got != "0.01" {
		t.Fatalf("the form inputs must be preserved, amount=%q", got)
	}
	if _, ok := f.chosenWdAddr(); !ok {
		t.Fatal("the destination pick must be preserved on a rejection")
	}
	// The error is visible in the pane.
	if !strings.Contains(plain(m.render()), "NO_BALANCE") {
		t.Fatal("the inline rejection must render in the request pane")
	}
}

func stripText(f fundingModel) (string, error) {
	line := ""
	for _, it := range f.stripItems() {
		for _, b := range it.Buttons {
			line += b.Glyph
		}
		line += ":" + it.Label + "  "
	}
	return line, nil
}

func TestFundingSingleFlightBlocksDispatch(t *testing.T) {
	fk := &fakeFunding{}
	m := withdrawSetup(t, fk)

	m = withdrawToConfirm(t, m, "0.01")
	m.orderInFlight = true // an order action is on the wire
	m, cmd := press(t, m, special(tea.KeyEnter))
	m = drainFunding(t, m, cmd)
	if len(fk.withdrawCalls) != 0 {
		t.Fatal("a funding money mover must not dispatch while an order action is in flight")
	}
	if m.funding.view != fundingConfirmView {
		t.Fatalf("the confirm must stay up, view=%d", m.funding.view)
	}
}

func TestFundingOrderEntryBlockedWhileFundingBusy(t *testing.T) {
	fk := &fakeFunding{}
	m := withdrawSetup(t, fk)
	m.funding.actionInFlight = true
	m.mode = modeNormal

	m, _ = press(t, m, k('b', "b"))
	if m.mode == modeOrder {
		t.Fatal("the order panel must not open while a funding action is in flight")
	}
}

func TestWithdrawValidation(t *testing.T) {
	fk := &fakeFunding{wdAddrs: []FundingWithdrawAddress{{Currency: "btc", Network: "BTC", Address: "bc1qx"}}}
	base := withdrawSetup(t, fk).funding // btc: min 0.001, precision 8

	// An unchosen destination refuses regardless of the amount.
	f := base
	f.amount.SetValue("0.01")
	f.wdAddrIdx = -1
	if err := f.validateWithdraw(); !strings.Contains(err, "destination") {
		t.Fatalf("unchosen destination must refuse, got %q", err)
	}

	cases := []struct {
		amount string
		max    string
		want   string // substring of the expected error; "" = valid
	}{
		{"", "", "enter the amount"},
		{"abc", "", "decimal"},
		{"0", "", "greater than zero"},
		{"-1", "", "greater than zero"},
		{"0.0001", "", "below"},
		{"0.123456789", "", "decimal places"},
		{"0.0100000000", "", ""}, // trailing zeros: textual scale 10, value fits precision 8
		{"3", "2.5", "exceeds"},
		{"0.01", "2.5", ""},
		{"0.01", "", ""}, // unknown max: bound unchecked
	}
	for _, c := range cases {
		f := base
		f.wdAddrIdx = 0 // destination picked; these cases exercise the amount
		f.amount.SetValue(c.amount)
		f.wdAmts = map[string]*fundingAmt{}
		if c.max != "" {
			f.wdAmts["btc"] = &fundingAmt{v: FundingWithdrawable{Amount: c.max}, loaded: true}
		}
		err := f.validateWithdraw()
		if c.want == "" && err != "" {
			t.Fatalf("amount %q: unexpected error %q", c.amount, err)
		}
		if c.want != "" && !strings.Contains(err, c.want) {
			t.Fatalf("amount %q: error %q, want substring %q", c.amount, err, c.want)
		}
	}
}

// A withdraw-address load that finished with an error (e.g. a non-main account,
// where funding is server-side refused) must surface that error — not the false
// "still loading" hint that a not-yet-ready list would give.
func TestWithdrawValidationSurfacesLoadError(t *testing.T) {
	fk := &fakeFunding{wdAddrs: []FundingWithdrawAddress{{Currency: "btc", Network: "BTC", Address: "bc1qx"}}}
	f := withdrawSetup(t, fk).funding
	f.amount.SetValue("0.01")
	f.wdAddrIdx = 0

	// The load errored: ready stays false but the error is recorded.
	f.wdAddrsReady = false
	f.wdErr = "ACCOUNT_SEQ_NOT_ALLOWED"

	err := f.validateWithdraw()
	if strings.Contains(err, "still loading") {
		t.Fatalf("errored load must not report still-loading, got %q", err)
	}
	if !strings.Contains(err, "ACCOUNT_SEQ_NOT_ALLOWED") || !strings.Contains(err, "unavailable") {
		t.Fatalf("errored load must surface the error, got %q", err)
	}

	// While genuinely still loading (no error yet), the loading hint stands.
	f.wdErr = ""
	if err := f.validateWithdraw(); !strings.Contains(err, "still loading") {
		t.Fatalf("in-flight load must report still-loading, got %q", err)
	}
}

func TestKRWValidation(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m) // krw available: 900000
	m = openFundingScreen(t, m)
	base := m.funding // selected krw
	base.tab = fundingWithdraw

	for amount, want := range map[string]string{
		"":        "enter",
		"abc":     "number",
		"0":       "greater",
		"1.5":     "whole",
		"1000000": "exceeds",
		"50000":   "",
	} {
		f := base
		f.amount.SetValue(amount)
		err := f.validateKRW()
		if want == "" && err != "" {
			t.Fatalf("amount %q: unexpected error %q", amount, err)
		}
		if want != "" && !strings.Contains(err, want) {
			t.Fatalf("amount %q: error %q, want substring %q", amount, err, want)
		}
	}
	// A deposit has no available-balance bound.
	f := base
	f.tab = fundingDeposit
	f.amount.SetValue("1000000")
	if err := f.validateKRW(); err != "" {
		t.Fatalf("deposit amount above the balance must be fine, got %q", err)
	}
}

func TestFundingCancelableRows(t *testing.T) {
	for status, want := range map[string]bool{
		"actionRequired": true, "reviewing": true,
		"pending": false, "processing": false, "done": false, "canceled": false, "failed": false,
	} {
		tr := FundingTransfer{ID: 1, Status: status}
		if got := fundingCancelable(fundingWithdraw, "btc", tr); got != want {
			t.Fatalf("cancelable(%s) = %v, want %v", status, got, want)
		}
	}
	if fundingCancelable(fundingDeposit, "btc", FundingTransfer{Status: "reviewing"}) {
		t.Fatal("deposits are never cancelable")
	}
	if fundingCancelable(fundingWithdraw, "krw", FundingTransfer{Status: "reviewing"}) {
		t.Fatal("KRW withdrawals are not cancelable via the API")
	}
}

func TestFundingKRWDepositPush(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m) // lands on krw / deposit

	m = cursorTo(t, m, ffAmount)
	m = typeText(t, m, "50000")
	m, _ = press(t, m, special(tea.KeyEnter)) // amount → button
	m, _ = press(t, m, special(tea.KeyEnter)) // review
	if m.funding.view != fundingConfirmView {
		t.Fatalf("submit must confirm first, view=%d err=%q", m.funding.view, m.funding.formErr)
	}
	m, cmd := press(t, m, special(tea.KeyEnter))
	m = drainFunding(t, m, cmd)
	if len(fk.krwDepositCalls) != 1 || fk.krwDepositCalls[0] != "50000" {
		t.Fatalf("krw deposit push calls = %v", fk.krwDepositCalls)
	}
	if m.funding.banner.kind != bannerOK || !strings.Contains(m.funding.banner.text, "Korbit app") {
		t.Fatalf("the push result must point at the app, got %q", m.funding.banner.text)
	}
}

func TestFundingGenerateAddressViaButton(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)
	m.funding.selected = "btc"
	m.funding.tab = fundingDeposit
	m.funding.resetForm()

	m = cursorTo(t, m, ffButton) // [ generate deposit address ]
	m2, cmd := press(t, m, special(tea.KeyEnter))
	m = drainFunding(t, m2, cmd)
	if fk.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", fk.generateCalls)
	}
	if _, ok := m.funding.depositAddrFor("btc", "BTC"); !ok {
		t.Fatal("the generated address must land in the model")
	}
	// With the address assigned the generate button is gone — the address (a
	// copy target) stands in its place.
	if idx := m.funding.fieldIndex(ffButton); idx >= 0 {
		t.Fatalf("the generate button must disappear once assigned, fields=%v", m.funding.formFields())
	}
	if idx := m.funding.fieldIndex(ffAddress); idx < 0 {
		t.Fatalf("the assigned address must be a field, fields=%v", m.funding.formFields())
	}
}

// The deposit address (and memo/tag) copy to the system clipboard via OSC 52,
// acked by a parent toast — the TUI captures the mouse, so this is the copy path.
func TestFundingDepositAddressCopy(t *testing.T) {
	fk := &fakeFunding{
		currencies: testCatalog(),
		depAddrs: []FundingDepositAddress{{
			Currency: "eth", Network: "ETH",
			Address: "0xexampleexampleexampleexample", SecondaryAddress: "note-8812",
		}},
	}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)
	m.funding.selected = "eth"
	m.funding.tab = fundingDeposit
	m.funding.resetForm()

	m = cursorTo(t, m, ffAddress)
	m2, cmd := press(t, m, special(tea.KeyEnter))
	msgs := collectMsgs(cmd)
	if len(msgs) < 2 {
		t.Fatalf("copy must emit the clipboard write plus its ack, got %d msgs", len(msgs))
	}
	var copied *fundingCopiedMsg
	for _, msg := range msgs {
		if c, ok := msg.(fundingCopiedMsg); ok {
			copied = &c
		}
	}
	if copied == nil || copied.what != "deposit address" {
		t.Fatalf("expected a deposit-address copy ack, msgs=%v", msgs)
	}
	mm, _ := m2.Update(*copied)
	m = mm.(model)
	if !strings.Contains(m.toast.text, "copied") {
		t.Fatalf("the copy ack must toast, got %q", m.toast.text)
	}

	// The memo/tag is its own copy target.
	m = cursorTo(t, m, ffMemo)
	_, cmd = press(t, m, special(tea.KeyEnter))
	found := false
	for _, msg := range collectMsgs(cmd) {
		if c, ok := msg.(fundingCopiedMsg); ok && c.what == "memo/tag" {
			found = true
		}
	}
	if !found {
		t.Fatal("enter on the memo row must copy the memo")
	}
}

func TestFundingEscClosesAndBalancesEnterJumps(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)

	m, _ = press(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal {
		t.Fatalf("esc must leave the funding screen, mode=%d", m.mode)
	}

	// enter on the focused balances pane jumps in at the window's top currency.
	m, _ = press(t, m, special(tea.KeyTab), special(tea.KeyTab)) // markets → orders → balances
	if m.focus != focusBalances {
		t.Fatalf("focus = %d, want balances", m.focus)
	}
	m2, cmd := press(t, m, special(tea.KeyEnter))
	if m2.mode != modeFunding {
		t.Fatalf("enter on balances must open funding, mode=%d", m2.mode)
	}
	m2 = drainFunding(t, m2, cmd)
	bals := m2.store.Balances()
	if len(bals) == 0 || m2.funding.selected != bals[0].Currency {
		t.Fatalf("funding must open at the balances window's top currency, selected=%q", m2.funding.selected)
	}
}

func TestFundingRenderSmoke(t *testing.T) {
	fk := &fakeFunding{
		currencies: testCatalog(),
		wdAddrs:    []FundingWithdrawAddress{{Currency: "btc", Network: "BTC", Address: "bc1qexample"}},
		withdraws:  []FundingTransfer{{ID: 9812, Currency: "btc", Amount: "0.01", Fee: "0.0009", Status: "reviewing", CreatedAt: 1_700_000_000_000}},
	}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)

	frame := plain(m.render())
	for _, want := range []string{"currencies", "KRW", "deposits · KRW", "[funding]", "direction", "deposit"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("browse frame must contain %q:\n%s", want, frame)
		}
	}

	// The withdraw tab of btc shows the form fields, the history, and the
	// cancelable-row hint — all in one screen, no overlay.
	m.funding.selected = "btc"
	m.funding.resetForm()
	m2, cmd := m.funding.switchTab(fundingWithdraw)
	m.funding = m2
	m = drainFunding(t, m, cmd)
	frame = plain(m.render())
	for _, want := range []string{"withdrawals · BTC (1)", "9812", "x:cancel",
		"address", "select — 1 registered", "amount", "review withdrawal"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("withdraw frame must contain %q:\n%s", want, frame)
		}
	}

	// The confirm step renders in the pane — never as a modal overlay.
	m = withdrawToConfirm(t, m, "0.01")
	frame = plain(m.render())
	for _, want := range []string{"confirm withdrawal", "cannot be reversed", "enter confirm", "esc back"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("confirm frame must contain %q:\n%s", want, frame)
		}
	}
	if _, ok := m.modalOverlayParts(); ok {
		t.Fatal("the funding screen must not present modal overlays")
	}
	// The history stays visible under the confirm.
	if !strings.Contains(frame, "withdrawals · BTC") {
		t.Fatal("the history must stay visible under the in-pane confirm")
	}
}

// tab cycles the browse focus list → form → history → list, and cycleFocus
// runs it in reverse.
func TestFundingFocusCycle(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)
	if m.funding.focus != fundingFocusList {
		t.Fatalf("open focus = %d, want list", m.funding.focus)
	}
	for i, want := range []fundingFocus{fundingFocusForm, fundingFocusHistory, fundingFocusList} {
		m, _ = press(t, m, special(tea.KeyTab))
		if m.funding.focus != want {
			t.Fatalf("tab #%d focus = %d, want %d", i+1, m.funding.focus, want)
		}
	}
	if got := m.funding.cycleFocus(false); got != fundingFocusHistory {
		t.Fatalf("cycleFocus(reverse) from list = %d, want history", got)
	}
}

// ←/→ on the direction row switch deposit ↔ withdraw — the one key rule for
// every selector in the pane (enter never toggles a selector).
func TestFundingDirectionChips(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m) // krw / deposit
	m.funding.selected = "btc"
	m.funding.resetForm()

	m = cursorTo(t, m, ffDirection)
	m2, cmd := press(t, m, special(tea.KeyRight))
	m = drainFunding(t, m2, cmd)
	if m.funding.tab != fundingWithdraw {
		t.Fatalf("→ on the direction row must switch to withdraw, tab=%d", m.funding.tab)
	}
	// The switched-to direction's history loads from the arrow alone.
	if h := m.funding.hist[histKey{"btc", fundingWithdraw}]; h == nil || !h.loaded {
		t.Fatal("switching direction must fetch that direction's history")
	}
	m, _ = press(t, m, special(tea.KeyLeft))
	if m.funding.tab != fundingDeposit {
		t.Fatalf("← must switch back to deposit, tab=%d", m.funding.tab)
	}
	// ← on deposit stays put; enter advances the cursor instead of toggling.
	m, _ = press(t, m, special(tea.KeyLeft))
	if m.funding.tab != fundingDeposit {
		t.Fatalf("← on the deposit side must not flip anything, tab=%d", m.funding.tab)
	}
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.funding.tab != fundingDeposit || m.funding.formCursor == 0 {
		t.Fatalf("enter on the direction row must advance, not toggle: tab=%d cursor=%d",
			m.funding.tab, m.funding.formCursor)
	}
}

// The network selector cycles with ←/→ on both tabs, filters the withdraw
// destination list, and drops the destination pick when it moves.
func TestFundingNetworkSelectorFiltersAddresses(t *testing.T) {
	fk := &fakeFunding{
		currencies: testCatalog(),
		wdAddrs: []FundingWithdrawAddress{
			{Currency: "eth", Network: "ETH", Address: "0xmainnetmainnetmainnetmainnet"},
			{Currency: "eth", Network: "ARB", Address: "0xarbitrumarbitrumarbitrum"},
		},
	}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)
	m.funding.selected = "eth"
	m.funding.tab = fundingWithdraw
	m.funding.resetForm()

	if got := len(m.funding.wdAddrChoices()); got != 1 {
		t.Fatalf("the destination list must be network-filtered, got %d choices", got)
	}
	m = cursorTo(t, m, ffAddress)
	m, _ = press(t, m, special(tea.KeyRight)) // pick the ETH address
	a, ok := m.funding.chosenWdAddr()
	if !ok || a.Network != "ETH" {
		t.Fatalf("chosen = %+v ok=%v, want the ETH address", a, ok)
	}
	m = cursorTo(t, m, ffNetwork)
	m, _ = press(t, m, special(tea.KeyRight)) // ETH → ARB
	if m.funding.netIdx != 1 {
		t.Fatalf("→ on the network row must cycle it, netIdx=%d", m.funding.netIdx)
	}
	if _, ok := m.funding.chosenWdAddr(); ok {
		t.Fatal("a network change must drop the destination pick (the list changed)")
	}
	if got := m.funding.wdAddrChoices(); len(got) != 1 || got[0].Network != "ARB" {
		t.Fatalf("choices after the cycle = %+v, want the ARB address", got)
	}
	// The withdraw tab has the network selector as a field now (it scopes the
	// destination list); btc (one network) offers no such field.
	if m.funding.fieldIndex(ffNetwork) < 0 {
		t.Fatal("a multi-network currency must offer the network field on withdraw")
	}
}

// A refetched registered-address list drops any destination pick: a kept index
// could silently point at a different address.
func TestFundingRefreshDropsDestinationPick(t *testing.T) {
	fk := &fakeFunding{}
	m := withdrawSetup(t, fk)
	m = cursorTo(t, m, ffAddress)
	m, _ = press(t, m, special(tea.KeyRight))
	if _, ok := m.funding.chosenWdAddr(); !ok {
		t.Fatal("setup: a destination must be picked")
	}
	m2, cmd := press(t, m, k('r', "r"))
	m = drainFunding(t, m2, cmd)
	if _, ok := m.funding.chosenWdAddr(); ok {
		t.Fatal("a refetched address list must drop the destination pick")
	}
}

// Typing while the amount input holds the cursor is text, not commands: q must
// not quit, r must not refresh.
func TestFundingAmountInputOwnsTyping(t *testing.T) {
	fk := &fakeFunding{}
	m := withdrawSetup(t, fk)
	m = cursorTo(t, m, ffAmount)
	if !m.funding.amountFocused() {
		t.Fatal("the amount input must be focused while under the cursor")
	}
	hist := fk.historyCalls
	m = typeText(t, m, "qr1")
	if m.mode != modeFunding {
		t.Fatal("q typed into the amount must not leave the screen")
	}
	if fk.historyCalls != hist {
		t.Fatal("r typed into the amount must not refresh")
	}
	if got := m.funding.amount.Value(); got != "qr1" {
		t.Fatalf("amount = %q, want the typed text", got)
	}
	// Moving the cursor off the amount blurs it and the command keys return.
	m, _ = press(t, m, special(tea.KeyUp))
	if m.funding.amountFocused() {
		t.Fatal("moving off the amount row must blur the input")
	}
}

// The withdraw pane explains a missing address registration in place (in the
// pane, not an overlay), and never auto-picks anything once one exists.
func TestWithdrawNoAddressesExplainsInPane(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()} // no wdAddrs registered
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)
	m.funding.selected = "btc"
	m.funding.resetForm()
	mm, cmd := m.funding.switchTab(fundingWithdraw)
	m.funding = mm
	m = drainFunding(t, m, cmd)

	frame := plain(m.render())
	// The explanation may wrap mid-phrase, so match its two halves separately.
	if !strings.Contains(frame, "register one in the Korbit developers") || !strings.Contains(frame, "portal") {
		t.Fatalf("the pane must explain the missing registration in place:\n%s", frame)
	}
	// Submitting refuses with the same pointer.
	m = cursorTo(t, m, ffButton)
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.funding.view != fundingBrowse || !strings.Contains(m.funding.formErr, "register") {
		t.Fatalf("review with no registered addresses must refuse inline, view=%d err=%q",
			m.funding.view, m.funding.formErr)
	}
}

// Leaving the funding screen and re-entering must not resurrect a destination
// pick or a typed amount — the form always opens fresh.
func TestFundingReopenClearsForm(t *testing.T) {
	fk := &fakeFunding{}
	m := withdrawSetup(t, fk)
	m = cursorTo(t, m, ffAddress)
	m, _ = press(t, m, special(tea.KeyRight)) // explicit pick
	m = cursorTo(t, m, ffAmount)
	m = typeText(t, m, "0.01")

	m, _ = press(t, m, special(tea.KeyEscape)) // leave the screen
	if m.mode != modeNormal {
		t.Fatalf("esc must leave funding, mode=%d", m.mode)
	}
	m = openFundingScreen(t, m) // re-enter with f
	f := m.funding
	if _, ok := f.chosenWdAddr(); ok {
		t.Fatal("a destination pick must not survive a close/reopen")
	}
	if f.amount.Value() != "" || f.formErr != "" {
		t.Fatalf("the form must reopen fresh, amount=%q err=%q", f.amount.Value(), f.formErr)
	}
}

// The currency filter follows the main quick search's contract: typing moves a
// highlight (the selection stays put), enter commits the highlighted match,
// and esc cancels with no effect.
func TestFundingSearchCommitOnEnterCancelOnEsc(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m) // selected: krw
	if m.funding.selected != "krw" {
		t.Fatalf("start selection = %q, want krw", m.funding.selected)
	}
	m, _ = press(t, m, k('/', "/")) // open the filter
	m = typeText(t, m, "ada")       // narrows to ada and highlights it
	if m.funding.searchSel != "ada" {
		t.Fatalf("typing a filter must highlight the first match, got %q", m.funding.searchSel)
	}
	if m.funding.selected != "krw" {
		t.Fatalf("typing must not move the selection, selected=%q", m.funding.selected)
	}
	// esc cancels: no effect on the selection.
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.funding.searching || m.funding.selected != "krw" {
		t.Fatalf("esc must cancel without effect, searching=%v selected=%q", m.funding.searching, m.funding.selected)
	}
	// Again, but enter commits the highlight.
	m, _ = press(t, m, k('/', "/"))
	m = typeText(t, m, "ada")
	m, _ = press(t, m, special(tea.KeyEnter))
	if m.funding.searching || m.funding.selected != "ada" {
		t.Fatalf("enter must commit the highlighted match, searching=%v selected=%q", m.funding.searching, m.funding.selected)
	}
}

// The filter owns the mouse as well as the keyboard: a body click while
// filtering must not move the real selection behind the highlight-only
// contract, and the wheel must not scroll the filtered window.
func TestFundingSearchSuppressesMouse(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m) // selected: krw
	// Sanity first: this click position selects the second list row (eth) when
	// no filter is active — so the suppressed click below is a real row hit.
	rowY := m.bodyTop() + 3 + 1
	mm, _ := m.Update(mclick(2, rowY))
	m = mm.(model)
	if m.funding.selected != "eth" {
		t.Fatalf("sanity: the click must select eth without a filter, got %q", m.funding.selected)
	}
	m, _ = press(t, m, k('/', "/"))            // open the filter
	mm, _ = m.Update(mclick(2, m.bodyTop()+3)) // click the KRW row
	m = mm.(model)
	if m.funding.selected != "eth" || !m.funding.searching {
		t.Fatalf("a click during the filter must be inert, selected=%q searching=%v",
			m.funding.selected, m.funding.searching)
	}
	before := m.funding.listScroll
	mm, _ = m.Update(tea.MouseWheelMsg{X: 2, Y: rowY, Button: tea.MouseWheelDown})
	m = mm.(model)
	if m.funding.listScroll != before || !m.funding.searching {
		t.Fatalf("the wheel during the filter must be inert, scroll %d→%d", before, m.funding.listScroll)
	}
}

// Backspace on an empty filter query cancels the filter — the shared
// "delete past the start to back out" gesture.
func TestFundingSearchBackspaceOnEmptyCancels(t *testing.T) {
	fk := &fakeFunding{currencies: testCatalog()}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m)
	m, _ = press(t, m, k('/', "/"))
	m = typeText(t, m, "a")
	m, _ = press(t, m, special(tea.KeyBackspace)) // deletes the "a"
	if !m.funding.searching {
		t.Fatal("backspace with text left must stay in the filter")
	}
	m, _ = press(t, m, special(tea.KeyBackspace)) // empty query → cancel
	if m.funding.searching {
		t.Fatal("backspace on an empty query must cancel the filter")
	}
}

// The destination selector never auto-picks: ← from unchosen stays unchosen,
// → makes the first explicit pick, and there is no way back to "none".
func TestWithdrawAddressSelectorSemantics(t *testing.T) {
	fk := &fakeFunding{wdAddrs: []FundingWithdrawAddress{
		{Currency: "btc", Network: "BTC", Address: "bc1aaaaaa"},
		{Currency: "btc", Network: "BTC", Address: "bc1bbbbbb"},
		{Currency: "btc", Network: "BTC", Address: "bc1cccccc"},
	}}
	f := withdrawSetup(t, fk).funding
	if _, ok := f.chosenWdAddr(); ok {
		t.Fatal("the selector must start with no destination chosen")
	}
	f.moveWdAddr(false) // ← from unchosen: still unchosen
	if f.wdAddrIdx != -1 {
		t.Fatalf("← from unchosen = %d, want -1", f.wdAddrIdx)
	}
	f.moveWdAddr(true) // first → enters at the first address
	if f.wdAddrIdx != 0 {
		t.Fatalf("first → = %d, want 0", f.wdAddrIdx)
	}
	f.moveWdAddr(true)
	if f.wdAddrIdx != 1 {
		t.Fatalf("second → = %d, want 1", f.wdAddrIdx)
	}
	f.moveWdAddr(false)
	f.moveWdAddr(false) // clamps at the first — never back to unchosen
	if f.wdAddrIdx != 0 {
		t.Fatalf("← at the first = %d, want 0 (no return to unchosen)", f.wdAddrIdx)
	}
}

// The KRW app-push note and the fee/min constraints are visible in the pane
// BEFORE anything is committed — not hidden behind an overlay.
func TestFundingFactsVisibleUpFront(t *testing.T) {
	fk := &fakeFunding{
		currencies: testCatalog(),
		wdAddrs:    []FundingWithdrawAddress{{Currency: "btc", Network: "BTC", Address: "bc1qexample"}},
	}
	m := fundingTestModel(t, fk)
	m = seedFundingBalances(t, m)
	m = openFundingScreen(t, m) // krw / deposit
	if !strings.Contains(plain(m.render()), "confirmation push") {
		t.Fatal("the KRW pane must carry the app-push note up front")
	}
	m.funding.selected = "btc"
	m.funding.resetForm()
	mm, cmd := m.funding.switchTab(fundingWithdraw)
	m.funding = mm
	m = drainFunding(t, m, cmd)
	frame := plain(m.render())
	for _, want := range []string{"fee 0.0009", "min 0.001", "8 decimals"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the withdraw pane must show the network constraints up front (%q):\n%s", want, frame)
		}
	}
}
