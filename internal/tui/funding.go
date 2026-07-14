// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/shopspring/decimal"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/components/curlist"
	"github.com/korbit-official/korbit-cli/internal/tui/components/transfers"
)

// The funding screen (modeFunding): a full-screen master–detail view for
// deposits and withdrawals, crypto and KRW. The left panel lists every
// supported currency (KRW pinned, held assets, then the rest) with live
// balances; the right panel shows the selected currency's deposit or withdraw
// tab — the transfer history as the content, the address / withdrawable-amount
// facts above it, and the actions on the key strip. All REST data flows
// through the Funding seam; results come back as fundingMsg messages.
//
// Safety model: the three request actions (crypto withdrawal, KRW deposit
// push, KRW withdrawal push) are non-idempotent money movers. They sit behind
// an explicit confirm step and share
// the single money-action-in-flight rule with order placement (at most one
// money mover on the wire, TUI-wide). A definite server rejection reopens the
// form with the error inline (the request provably did not execute); an
// ambiguous failure (timeout, connection error) is NEVER retried — the view
// lands on the freshly refetched history with a standing "outcome unknown —
// verify below" banner, because re-sending an unacknowledged money mover can
// double-spend. The request actions need a key provisioned with transfer
// permissions (`setup --with-transfers`); deposit-address generation
// (idempotent) and withdrawal cancel need no such key — stopping money
// movement must never be gated.

// --- the Funding seam (implemented by the cli layer) ---

// FundingNetwork is one blockchain network a currency moves on, with the
// per-network withdrawal constraints the form validates against. Fee, min,
// and precision are "" / -1 when the catalog does not carry them.
type FundingNetwork struct {
	Name                string
	DepositLaunched     bool
	WithdrawalLaunched  bool
	WithdrawalFee       string // fee charged in addition to the sent amount
	WithdrawalMin       string // minimum amount per withdrawal
	WithdrawalPrecision int    // max decimal places of the amount; -1 unknown
	HasSecondaryAddress bool   // the network uses a destination tag / memo
}

// FundingCurrency is one supported currency from the public catalog.
// Networks is empty for fiat (KRW).
type FundingCurrency struct {
	Currency       string // wire code, lower-case ("btc")
	FullName       string
	DefaultNetwork string
	Networks       []FundingNetwork
}

// FundingDepositAddress is one crypto deposit address.
type FundingDepositAddress struct {
	Currency         string
	Network          string
	Address          string
	SecondaryAddress string
}

// FundingWithdrawAddress is one address registered for API withdrawals.
// Currency is "" when the registration is valid for any asset on the network.
type FundingWithdrawAddress struct {
	Currency         string
	Network          string
	Address          string
	SecondaryAddress string
}

// FundingWithdrawable is one asset's withdrawable amount: what can be sent
// now, and what is locked in pending withdrawals.
type FundingWithdrawable struct {
	Currency string
	Amount   string
	InUse    string
}

// FundingTransfer is one deposit or withdrawal record, crypto or KRW (KRW rows
// carry no network/address/hash; deposits carry no fee).
type FundingTransfer struct {
	ID        int64
	Currency  string
	Amount    string
	Fee       string
	Status    string
	Network   string
	Address   string
	TxHash    string
	CreatedAt int64
}

// FundingWithdrawOrder is one crypto-withdrawal request. Amount is the user's
// decimal string, passed through untouched; Address/Network/SecondaryAddress
// come verbatim from a registered withdrawal address.
type FundingWithdrawOrder struct {
	Currency         string
	Network          string
	Amount           string
	Address          string
	SecondaryAddress string
}

// FundingReceipt is the accepted crypto-withdrawal's identity.
type FundingReceipt struct {
	ID     int64
	Status string
}

// Funding is the deposit/withdrawal data and action seam. The cli
// implementation runs every call through the same validated, journaled ops
// path as the endpoint commands. The three request calls and the cancel are
// serialized by the UI (one money action in flight); the reads may be issued
// concurrently, so implementations must be safe for concurrent use.
// History reads route "krw" to the KRW endpoints, so the UI never selects an
// endpoint itself.
// Funding is the funding seam. Every account-scoped call takes the accountSeq to
// operate on: the funding screen always passes the CURRENTLY ACTIVE account (no
// defaulting to main). Deposits/withdrawals are main-account-only server-side, so
// a non-main seq is rejected with ACCOUNT_SEQ_NOT_ALLOWED — the screen warns but
// never silently retargets. (Currencies is the account-agnostic public catalog.)
type Funding interface {
	Currencies() ([]FundingCurrency, error)
	DepositAddresses(accountSeq int) ([]FundingDepositAddress, error)
	GenerateDepositAddress(accountSeq int, currency, network string) (FundingDepositAddress, error)
	WithdrawAddresses(accountSeq int) ([]FundingWithdrawAddress, error)
	Withdrawable(accountSeq int, currency string) (FundingWithdrawable, error)
	DepositHistory(accountSeq int, currency string, limit int) ([]FundingTransfer, error)
	WithdrawHistory(accountSeq int, currency string, limit int) ([]FundingTransfer, error)
	RequestWithdrawal(accountSeq int, o FundingWithdrawOrder) (FundingReceipt, error)
	CancelWithdrawal(accountSeq int, id int64) error
	RequestKRWDeposit(accountSeq int, amount string) error
	RequestKRWWithdraw(accountSeq int, amount string) error
}

// --- sub-model state ---

// fundingTab selects the detail direction: deposits or withdrawals.
type fundingTab int

const (
	fundingDeposit fundingTab = iota
	fundingWithdraw
)

// fundingView is the funding screen's own input mode. There are no overlays:
// confirm and busy replace the request pane's content in place.
type fundingView int

const (
	fundingBrowse      fundingView = iota
	fundingConfirmView             // review step before a money mover (in-pane)
	fundingBusy                    // a request is on the wire; input parked (in-pane)
)

// fundingFocus is which browse pane navigation keys drive. tab cycles them in
// this order (list → request pane → history).
type fundingFocus int

const (
	fundingFocusList    fundingFocus = iota // the currency list
	fundingFocusForm                        // the request pane (the form's fields)
	fundingFocusHistory                     // the transfer-history table
)

// formField identifies one interactive row of the request pane, in the order
// ↑/↓ visit them. One key rule across all of them: ↑/↓ move the field cursor,
// ←/→ change the value of the selector under it, typing edits the input under
// it, enter activates the button (and copies on the deposit address/memo).
type formField int

const (
	ffNone      formField = iota - 1 // a fact/banner line — not interactive
	ffDirection                      // the deposit ⁄ withdraw chips (always first)
	ffNetwork                        // the ‹ network › selector (crypto, more than one network)
	ffAddress                        // withdraw: the ‹ destination › selector; deposit: the assigned address (enter copies)
	ffMemo                           // deposit only: the address's memo/tag (enter copies)
	ffAmount                         // the amount text input
	ffButton                         // the primary action button
)

// fundingActionKind identifies the money action being confirmed / in flight.
type fundingActionKind int

const (
	factNone fundingActionKind = iota
	factWithdraw
	factKRWDeposit
	factKRWWithdraw
	factCancel
)

// fundingAction is the confirmed action payload, captured at confirm time so
// the dispatch cannot act on state that moved under the dialog.
type fundingAction struct {
	kind      fundingActionKind
	order     FundingWithdrawOrder // factWithdraw
	amount    string               // KRW pushes
	cancelID  int64                // factCancel
	cancelCur string               // factCancel: for the result banner
}

// fundingBanner is a standing message on one currency+tab detail (action
// results, the ambiguous-outcome warning, gating notes). It persists until a
// refresh or the next action replaces it — deliberately, for the
// outcome-unknown case, which must stay visible until the user has verified
// the list.
type fundingBanner struct {
	text string
	kind bannerKind
	cur  string
	tab  fundingTab
}

type bannerKind int

const (
	bannerNone bannerKind = iota
	bannerOK
	bannerWarn
	bannerErr
)

// histKey addresses one fetched history: a currency and a direction.
type histKey struct {
	cur string
	dir fundingTab
}

// fundingHist is one (currency, direction) history fetch. seq stamps the
// in-flight request; a reply whose seq no longer matches is dropped, so a
// stale response for a currency the user scrolled past never lands.
type fundingHist struct {
	rows    []FundingTransfer
	loaded  bool
	loading bool
	err     string
	seq     int
}

// fundingAmt is one currency's withdrawable-amount fetch (crypto only).
type fundingAmt struct {
	v       FundingWithdrawable
	loaded  bool
	loading bool
	err     string
	seq     int
}

const (
	// fundingHistoryLimit is how many transfer rows one history fetch asks for
	// (the endpoint caps at 100).
	fundingHistoryLimit = 50
	// fundingSettleMs debounces the detail fetch while the user scrubs the
	// currency list, like the market resubscribe debounce.
	fundingSettleMs = 120
	// fundingKRW is the fiat currency code, the pinned first row.
	fundingKRW = "krw"
)

// fundingModel is the funding screen: a self-contained sub-model the parent
// routes keys, mouse, and fundingMsg messages into. It owns all funding state;
// the parent owns only the mode switch and the shared money-action gate.
// Value-copied like the parent model; the maps and component pointers are
// shared across copies by design (single-goroutine mutation, shared caches).
type fundingModel struct {
	api        Funding
	store      *state.Store // read-only: balances + tickers for the list
	accountSeq int          // the sub-account whose balances the screen shows (parent-stamped at construction)
	now        func() int64

	view   fundingView
	tab    fundingTab
	focus  fundingFocus
	w      int // terminal width (parent-fed on resize)
	bodyH  int // body height (parent-fed on resize)
	closed bool

	// Resizable layout, session-only (reset with '='), dragged like the main
	// screen's seams: sideDiv is the currency-list | detail seam as a fraction
	// of the width. The request pane's height is not a seam — it is sized to
	// its content, and the history pane takes the rest.
	sideDiv float64

	// Currency universe (public catalog), fetched once per session.
	currencies []FundingCurrency
	curReady   bool
	curLoading bool
	curErr     string

	// Deposit addresses (all assets, one call) and registered withdrawal
	// addresses. Fetched on entry, refetched by r on the owning tab.
	depAddrs      []FundingDepositAddress
	depAddrsReady bool
	depLoading    bool
	depErr        string
	wdAddrs       []FundingWithdrawAddress
	wdAddrsReady  bool
	wdLoading     bool
	wdErr         string

	hist   map[histKey]*fundingHist
	wdAmts map[string]*fundingAmt
	// seq is a MONOTONIC fetch-generation counter (never reset — see
	// setAccountSeq): every REST fetch is stamped with nextSeq() and its reply
	// carries that value back, so apply() drops any superseded reply, including
	// one issued for this same account before an account switch away and back.
	seq int
	// depSeq/wdSeq are the latest issued generation for the deposit/withdraw
	// address lists (which, unlike hist/wdAmts, have no per-key slot to hold it).
	// 0 means "no fetch outstanding" (nextSeq never returns 0).
	depSeq, wdSeq int

	selected   string // wire currency code the detail shows
	netIdx     int    // index into the selected currency's Networks (←/→ on the network row)
	listScroll int
	searching  bool
	query      string
	// searchSel is the highlighted currency while the filter is active. The
	// real selection only moves on enter — esc cancels with no effect — the
	// same contract as the main screen's quick search.
	searchSel string

	histCursor int
	histScroll int

	// The request pane's form state. formCursor indexes formFields(); the
	// destination selector starts unchosen (wdAddrIdx -1) and is reset whenever
	// the currency, network, or the registered-address list itself changes — a
	// money destination is never picked implicitly. amount is the pane's one
	// text input, focused exactly while the cursor sits on it.
	formCursor int
	wdAddrIdx  int
	amount     textinput.Model
	formErr    string

	detailGen int // debounce generation for the selection-settled fetch

	// actionInFlight is the funding side of the TUI-wide single money-action
	// rule: it blocks every funding action AND (via the parent's gate) order
	// placement/cancel, and stays set until the action's done message lands —
	// even if the user escapes the busy view or the funding screen entirely.
	actionInFlight bool
	action         fundingAction
	generating     bool // deposit-address generation in flight (inline, browse stays live)

	banner fundingBanner

	spin spinner.Model
	rev  uint64 // bumped on any data change the list/table components render from

	cList *curlist.Model
	cHist *transfers.Model
}

func newFundingModel(api Funding, store *state.Store, accountSeq int, now func() int64) fundingModel {
	f := fundingModel{
		api:        api,
		store:      store,
		accountSeq: accountSeq,
		now:        now,
		selected:   fundingKRW,
		hist:       map[histKey]*fundingHist{},
		wdAmts:     map[string]*fundingAmt{},
		amount:     newFormInput("amount", 22),
		spin:       spinner.New(spinner.WithSpinner(spinner.Dot)),
		cList:      curlist.New(),
		cHist:      transfers.New(),
	}
	f.resetForm()
	f.resetLayout()
	return f
}

// resetLayout restores the default funding proportions: a ~30% currency list.
func (f *fundingModel) resetLayout() {
	f.sideDiv = 0.30
}

// setAccountSeq re-points the funding screen at a different sub-account and drops
// every per-account cache (deposit/withdraw addresses, transfer history,
// withdrawable amounts) plus the form, so a reopen refetches under the new
// account rather than showing the previous one's data. The account-independent
// currency catalog is kept. Called by the parent on an account switch — the
// screen is not open at the time, and open() refetches the cleared data.
func (f *fundingModel) setAccountSeq(seq int) {
	f.accountSeq = seq
	f.depAddrs, f.depAddrsReady, f.depLoading, f.depErr = nil, false, false, ""
	f.wdAddrs, f.wdAddrsReady, f.wdLoading, f.wdErr = nil, false, false, ""
	f.hist = map[histKey]*fundingHist{}
	f.wdAmts = map[string]*fundingAmt{}
	// seq stays MONOTONIC across the switch so a same-account fetch issued before
	// this switch can never collide with one issued after a switch back. Only the
	// per-list "latest generation" trackers reset — the cleared hist/wdAmts slots
	// default to seq 0, and depSeq/wdSeq reset to 0, so any in-flight reply
	// (seq >= 1) is dropped until the reopened screen issues a fresh fetch.
	f.depSeq, f.wdSeq = 0, 0
	f.generating = false // abandon any in-flight generate for the old account
	f.resetForm()
	f.bump()
}

// onMainAccount reports whether the screen's active account is the main account.
// Deposits/withdrawals are main-account-only server-side, so on any other
// sub-account the screen shows a warning banner (see browseLines) — but it still
// operates on the active account and lets the server reject; it never retargets.
func (f fundingModel) onMainAccount() bool { return f.accountSeq == accountseq.Main }

// bump invalidates the component render caches (the list/table Keys carry rev).
func (f *fundingModel) bump() { f.rev++ }

// busy reports whether a funding money action is in flight — the parent folds
// this into its single money-action gate, and it holds across view/mode
// changes until the done message lands.
func (f fundingModel) busy() bool { return f.actionInFlight }

// spinning reports whether the busy/loading spinner animation should advance.
func (f fundingModel) spinning() bool {
	if f.actionInFlight || f.generating {
		return true
	}
	h := f.hist[histKey{f.selected, f.tab}]
	return h != nil && h.loading
}

// setSize records the terminal geometry the browse view lays out with; the
// parent feeds it on every resize and on entry.
func (f *fundingModel) setSize(w, bodyH int) {
	f.w, f.bodyH = w, bodyH
	f.clampScrolls()
}

// open (re)enters the funding screen: reset the transient input state, kick
// off whatever base data is missing, and fetch the selection's detail.
// preselect (a wire currency code, or "") picks the initially selected
// currency — the balances-pane jump passes the currency under its window.
func (f fundingModel) open(preselect string) (fundingModel, tea.Cmd) {
	f.view = fundingBrowse
	f.focus = fundingFocusList
	f.searching, f.query, f.searchSel = false, "", ""
	f.closed = false
	f.formCursor = 0
	f.amount.Blur()
	if preselect != "" && strings.ToLower(preselect) != f.selected {
		f.selected = strings.ToLower(preselect)
		f.netIdx = 0
	}
	// Every entry starts with a fresh form: a destination pick or an amount
	// from a previous visit must never greet the user pre-armed.
	f.resetForm()
	// The screen always operates on the CURRENTLY ACTIVE account — it never
	// defaults to main. Funding is main-account-only server-side, so on a non-main
	// account these fetches return ACCOUNT_SEQ_NOT_ALLOWED and the panes show that;
	// render() adds a warning banner (see browseLines). They are issued regardless
	// so the funding screen behaves exactly like the CLI on the same account.
	var cmds []tea.Cmd
	if !f.curReady && !f.curLoading {
		f.curLoading = true
		f.curErr = ""
		cmds = append(cmds, fetchFundingCurrencies(f.api))
	}
	if !f.depAddrsReady && !f.depLoading {
		f.depLoading = true
		f.depErr = ""
		f.depSeq = f.nextSeq()
		cmds = append(cmds, fetchFundingDepAddrs(f.api, f.accountSeq, f.depSeq))
	}
	if !f.wdAddrsReady && !f.wdLoading {
		f.wdLoading = true
		f.wdErr = ""
		f.wdSeq = f.nextSeq()
		cmds = append(cmds, fetchFundingWdAddrs(f.api, f.accountSeq, f.wdSeq))
	}
	cmds = append(cmds, f.ensureDetail())
	if f.spinning() {
		cmds = append(cmds, f.spin.Tick)
	}
	return f, tea.Batch(cmds...)
}

// --- messages ---

// fundingMsg marks every funding-owned message, so the parent Update routes
// them here with one case.
type fundingMsg interface{ isFundingMsg() }

type fundingCurrenciesMsg struct {
	list []FundingCurrency
	err  error
}
type fundingDepAddrsMsg struct {
	accountSeq int // the account this fetch was issued for; discarded if it changed
	seq        int // fetch generation; discarded if a newer same-account fetch superseded it
	list       []FundingDepositAddress
	err        error
}
type fundingWdAddrsMsg struct {
	accountSeq int
	seq        int
	list       []FundingWithdrawAddress
	err        error
}
type fundingHistMsg struct {
	accountSeq int // the account this fetch was issued for; discarded if it changed
	cur        string
	dir        fundingTab
	seq        int
	rows       []FundingTransfer
	err        error
}
type fundingAmtMsg struct {
	accountSeq int
	cur        string
	seq        int
	v          FundingWithdrawable
	err        error
}
type fundingGenMsg struct {
	accountSeq int
	cur        string
	addr       FundingDepositAddress
	err        error
}
type fundingActionMsg struct {
	act     fundingAction
	receipt FundingReceipt
	err     error
}

// fundingSettleMsg fires when a debounced currency-selection change settles.
type fundingSettleMsg struct{ gen int }

// fundingCopiedMsg is NOT a fundingMsg: it reports a completed clipboard copy
// (deposit address / memo, sent via OSC 52) to the PARENT model, which shows
// it as a footer toast — the funding banner is deliberately sticky and would
// be too heavy an ack for a copy.
type fundingCopiedMsg struct{ what string }

func (fundingCurrenciesMsg) isFundingMsg() {}
func (fundingDepAddrsMsg) isFundingMsg()   {}
func (fundingWdAddrsMsg) isFundingMsg()    {}
func (fundingHistMsg) isFundingMsg()       {}
func (fundingAmtMsg) isFundingMsg()        {}
func (fundingGenMsg) isFundingMsg()        {}
func (fundingActionMsg) isFundingMsg()     {}
func (fundingSettleMsg) isFundingMsg()     {}

// --- commands (each closes over the seam only, never the model) ---

func fetchFundingCurrencies(api Funding) tea.Cmd {
	return func() tea.Msg { l, err := api.Currencies(); return fundingCurrenciesMsg{list: l, err: err} }
}

// Each command captures the active accountSeq at creation (on the update
// goroutine) and passes it to the seam — no shared mutable account state, and no
// defaulting. The hist/withdrawable results also carry that accountSeq back so
// apply can discard a reply that lands after an account switch (its cache slot
// belongs to a different account now); the money movers report their own outcome
// and are not cached per-account, so they need no such guard.

func fetchFundingDepAddrs(api Funding, accountSeq, seq int) tea.Cmd {
	return func() tea.Msg {
		l, err := api.DepositAddresses(accountSeq)
		return fundingDepAddrsMsg{accountSeq: accountSeq, seq: seq, list: l, err: err}
	}
}

func fetchFundingWdAddrs(api Funding, accountSeq, seq int) tea.Cmd {
	return func() tea.Msg {
		l, err := api.WithdrawAddresses(accountSeq)
		return fundingWdAddrsMsg{accountSeq: accountSeq, seq: seq, list: l, err: err}
	}
}

func fetchFundingHist(api Funding, accountSeq int, cur string, dir fundingTab, seq int) tea.Cmd {
	return func() tea.Msg {
		var rows []FundingTransfer
		var err error
		if dir == fundingDeposit {
			rows, err = api.DepositHistory(accountSeq, cur, fundingHistoryLimit)
		} else {
			rows, err = api.WithdrawHistory(accountSeq, cur, fundingHistoryLimit)
		}
		return fundingHistMsg{accountSeq: accountSeq, cur: cur, dir: dir, seq: seq, rows: rows, err: err}
	}
}

func fetchFundingAmt(api Funding, accountSeq int, cur string, seq int) tea.Cmd {
	return func() tea.Msg {
		v, err := api.Withdrawable(accountSeq, cur)
		return fundingAmtMsg{accountSeq: accountSeq, cur: cur, seq: seq, v: v, err: err}
	}
}

func generateFundingAddr(api Funding, accountSeq int, cur, network string) tea.Cmd {
	return func() tea.Msg {
		a, err := api.GenerateDepositAddress(accountSeq, cur, network)
		return fundingGenMsg{accountSeq: accountSeq, cur: cur, addr: a, err: err}
	}
}

func runFundingAction(api Funding, accountSeq int, act fundingAction) tea.Cmd {
	return func() tea.Msg {
		var receipt FundingReceipt
		var err error
		switch act.kind {
		case factWithdraw:
			receipt, err = api.RequestWithdrawal(accountSeq, act.order)
		case factKRWDeposit:
			err = api.RequestKRWDeposit(accountSeq, act.amount)
		case factKRWWithdraw:
			err = api.RequestKRWWithdraw(accountSeq, act.amount)
		case factCancel:
			err = api.CancelWithdrawal(accountSeq, act.cancelID)
		}
		return fundingActionMsg{act: act, receipt: receipt, err: err}
	}
}

func fundingSettleTick(gen int) tea.Cmd {
	return tea.Tick(fundingSettleMs*time.Millisecond, func(time.Time) tea.Msg { return fundingSettleMsg{gen: gen} })
}

// --- data plumbing ---

// nextSeq stamps a new hist/withdrawable fetch.
func (f *fundingModel) nextSeq() int { f.seq++; return f.seq }

// histFor returns (creating if needed) the fetch slot for one currency+direction.
func (f *fundingModel) histFor(k histKey) *fundingHist {
	h, ok := f.hist[k]
	if !ok {
		h = &fundingHist{}
		f.hist[k] = h
	}
	return h
}

// amtFor returns (creating if needed) the withdrawable slot for one currency.
func (f *fundingModel) amtFor(cur string) *fundingAmt {
	a, ok := f.wdAmts[cur]
	if !ok {
		a = &fundingAmt{}
		f.wdAmts[cur] = a
	}
	return a
}

// ensureDetail starts whatever fetches the selected currency+tab still needs:
// its history, and (crypto withdraw tab) its withdrawable amount. An errored
// fetch is not auto-retried — r refetches deliberately.
func (f *fundingModel) ensureDetail() tea.Cmd {
	var cmds []tea.Cmd
	k := histKey{f.selected, f.tab}
	h := f.histFor(k)
	if !h.loaded && !h.loading && h.err == "" {
		h.loading = true
		h.seq = f.nextSeq()
		f.bump()
		cmds = append(cmds, fetchFundingHist(f.api, f.accountSeq, k.cur, k.dir, h.seq))
	}
	if f.tab == fundingWithdraw && f.selected != fundingKRW {
		a := f.amtFor(f.selected)
		if !a.loaded && !a.loading && a.err == "" {
			a.loading = true
			a.seq = f.nextSeq()
			cmds = append(cmds, fetchFundingAmt(f.api, f.accountSeq, f.selected, a.seq))
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	cmds = append(cmds, f.spin.Tick)
	return tea.Batch(cmds...)
}

// refresh refetches the selected currency+tab's REST data and clears the
// standing banner: the history, the owning tab's address set, the withdrawable
// amount, and (only after a failure) the currency catalog. Balances and
// tickers are live off the stream and need no refresh.
func (f *fundingModel) refresh() tea.Cmd {
	f.banner = fundingBanner{}
	var cmds []tea.Cmd
	k := histKey{f.selected, f.tab}
	h := f.histFor(k)
	if !h.loading {
		h.loaded, h.err = false, ""
	}
	if f.tab == fundingDeposit {
		if !f.depLoading {
			f.depLoading, f.depErr = true, ""
			f.depSeq = f.nextSeq()
			cmds = append(cmds, fetchFundingDepAddrs(f.api, f.accountSeq, f.depSeq))
		}
	} else {
		if !f.wdLoading {
			f.wdLoading, f.wdErr = true, ""
			f.wdSeq = f.nextSeq()
			cmds = append(cmds, fetchFundingWdAddrs(f.api, f.accountSeq, f.wdSeq))
		}
		if f.selected != fundingKRW {
			a := f.amtFor(f.selected)
			if !a.loading {
				a.loaded, a.err = false, ""
			}
		}
	}
	if f.curErr != "" && !f.curLoading {
		f.curLoading, f.curErr = true, ""
		cmds = append(cmds, fetchFundingCurrencies(f.api))
	}
	f.bump()
	cmds = append(cmds, f.ensureDetail())
	return tea.Batch(cmds...)
}

// setNote raises a standing banner on the selected currency+tab.
func (f *fundingModel) setNote(text string, kind bannerKind) {
	f.banner = fundingBanner{text: text, kind: kind, cur: f.selected, tab: f.tab}
	f.bump()
}

// apply folds one funding message into the model and returns any follow-up
// command (post-action refetches).
func (f fundingModel) apply(msg fundingMsg) (fundingModel, tea.Cmd) {
	switch msg := msg.(type) {
	case fundingCurrenciesMsg:
		f.curLoading = false
		if msg.err != nil {
			f.curErr = errorText(msg.err)
		} else {
			f.currencies = msg.list
			f.curReady = true
		}
		f.bump()
		return f, nil

	case fundingDepAddrsMsg:
		if msg.accountSeq != f.accountSeq {
			return f, nil // issued for a different account; the current fetch owns depLoading
		}
		if msg.seq != f.depSeq {
			return f, nil // superseded by a newer same-account fetch
		}
		f.depLoading = false
		if msg.err != nil {
			f.depErr = errorText(msg.err)
		} else {
			f.depAddrs = msg.list
			f.depAddrsReady = true
		}
		f.bump()
		return f, nil

	case fundingWdAddrsMsg:
		if msg.accountSeq != f.accountSeq {
			return f, nil // issued for a different account; the current fetch owns wdLoading
		}
		if msg.seq != f.wdSeq {
			return f, nil // superseded by a newer same-account fetch
		}
		f.wdLoading = false
		if msg.err != nil {
			f.wdErr = errorText(msg.err)
		} else {
			f.wdAddrs = msg.list
			f.wdAddrsReady = true
			// The destination list was replaced: a kept index could silently
			// point at a DIFFERENT address, so any explicit pick is dropped.
			f.wdAddrIdx = -1
		}
		f.bump()
		return f, nil

	case fundingHistMsg:
		if msg.accountSeq != f.accountSeq {
			return f, nil // issued for a different account (a switch cleared the slot)
		}
		h := f.histFor(histKey{msg.cur, msg.dir})
		if msg.seq != h.seq {
			return f, nil // superseded fetch; a newer one owns the slot
		}
		h.loading = false
		if msg.err != nil {
			h.err = errorText(msg.err)
		} else {
			h.rows, h.loaded, h.err = msg.rows, true, ""
		}
		f.clampScrolls()
		f.bump()
		return f, nil

	case fundingAmtMsg:
		if msg.accountSeq != f.accountSeq {
			return f, nil // issued for a different account (a switch cleared the slot)
		}
		a := f.amtFor(msg.cur)
		if msg.seq != a.seq {
			return f, nil
		}
		a.loading = false
		if msg.err != nil {
			a.err = errorText(msg.err)
		} else {
			a.v, a.loaded, a.err = msg.v, true, ""
		}
		f.bump()
		return f, nil

	case fundingGenMsg:
		if msg.accountSeq != f.accountSeq {
			return f, nil // generated for a different account; setAccountSeq already reset `generating`
		}
		f.generating = false
		if msg.err != nil {
			f.setNote(i18n.T("generate address failed: %s", fundingErrorText(msg.err)), bannerErr)
			return f, nil
		}
		// Replace the currency+network's address (the endpoint returns the
		// existing one when it was already assigned) or add it.
		replaced := false
		for i, a := range f.depAddrs {
			if a.Currency == msg.addr.Currency && a.Network == msg.addr.Network {
				f.depAddrs[i], replaced = msg.addr, true
				break
			}
		}
		if !replaced {
			f.depAddrs = append(f.depAddrs, msg.addr)
		}
		f.bump()
		return f, nil

	case fundingSettleMsg:
		if msg.gen != f.detailGen {
			return f, nil
		}
		return f, f.ensureDetail()

	case fundingActionMsg:
		return f.applyActionResult(msg)
	}
	return f, nil
}

// applyActionResult lands a money action's outcome. Three cases:
//   - accepted: back to browse on the action's history tab, freshly refetched,
//     with the receipt on the banner;
//   - definite rejection (a Korbit error envelope or a validation error — the
//     request provably did not execute): the form reopens with the error
//     inline and the inputs preserved;
//   - ambiguous (timeout / transport): NO retry, ever — browse on the history
//     tab with a standing outcome-unknown banner over a fresh refetch, so the
//     user verifies before re-sending a money mover.
func (f fundingModel) applyActionResult(msg fundingActionMsg) (fundingModel, tea.Cmd) {
	f.actionInFlight = false
	act := msg.act

	// The result lands on the currency+tab the action targeted, wherever the
	// user has navigated meanwhile.
	cur, tab := f.selected, f.tab
	switch act.kind {
	case factWithdraw:
		cur, tab = act.order.Currency, fundingWithdraw
	case factKRWDeposit:
		cur, tab = fundingKRW, fundingDeposit
	case factKRWWithdraw:
		cur, tab = fundingKRW, fundingWithdraw
	case factCancel:
		cur, tab = act.cancelCur, fundingWithdraw
	}

	if msg.err != nil && isDefiniteRejection(msg.err) && f.view == fundingBusy {
		// Provably not executed: back to the form, inputs intact, error inline.
		f.view = fundingBrowse
		if act.kind == factCancel { // cancel has no form
			f.setNoteAt(cur, tab, i18n.T("cancel failed: %s", fundingErrorText(msg.err)), bannerErr)
			return f, nil
		}
		f.focus = fundingFocusForm
		f.formErr = fundingErrorText(msg.err)
		f.bump()
		return f, nil
	}

	// Accepted or ambiguous: land on the target history, refetched.
	f.view = fundingBrowse
	f.focus = fundingFocusHistory
	f.amount.Blur()
	f.selected, f.tab = cur, tab
	switch {
	case msg.err == nil:
		switch act.kind {
		case factWithdraw:
			// An accepted request can still carry a terminal failure status
			// (e.g. rejected for balance after acceptance) — style by the status.
			kind := bannerOK
			if msg.receipt.Status == "failed" || msg.receipt.Status == "canceled" {
				kind = bannerErr
			}
			f.setNoteAt(cur, tab, i18n.T("withdrawal %d requested — status %s", msg.receipt.ID, msg.receipt.Status), kind)
		case factKRWDeposit:
			f.setNoteAt(cur, tab, i18n.T("deposit push sent — complete the verification in the Korbit app"), bannerOK)
		case factKRWWithdraw:
			f.setNoteAt(cur, tab, i18n.T("withdrawal push sent — complete the verification in the Korbit app"), bannerOK)
		case factCancel:
			f.setNoteAt(cur, tab, i18n.T("cancel of withdrawal %d accepted", act.cancelID), bannerOK)
		}
		if act.kind != factCancel {
			// The request is done: a fresh form, so a stale amount or destination
			// pick can't ride into the next request unnoticed.
			f.resetForm()
		}
	case isDefiniteRejection(msg.err):
		// A definite rejection with the form already gone (the user escaped
		// the busy view): surface it on the banner instead.
		f.setNoteAt(cur, tab, i18n.T("request rejected: %s", fundingErrorText(msg.err)), bannerErr)
	default:
		f.setNoteAt(cur, tab,
			i18n.T("request outcome UNKNOWN (%s) — verify the list below before retrying; the request may still have gone through", errorText(msg.err)),
			bannerWarn)
	}

	// Refetch the landed-on history (+ withdrawable for a crypto withdrawal),
	// bypassing the loaded latch.
	var cmds []tea.Cmd
	k := histKey{cur, tab}
	h := f.histFor(k)
	if !h.loading {
		h.loaded, h.err = false, ""
	}
	if act.kind == factWithdraw || act.kind == factCancel {
		a := f.amtFor(cur)
		if !a.loading {
			a.loaded, a.err = false, ""
		}
	}
	f.bump()
	cmds = append(cmds, f.ensureDetail())
	return f, tea.Batch(cmds...)
}

// setNoteAt raises a standing banner on an explicit currency+tab.
func (f *fundingModel) setNoteAt(cur string, tab fundingTab, text string, kind bannerKind) {
	f.banner = fundingBanner{text: text, kind: kind, cur: cur, tab: tab}
	f.bump()
}

// isDefiniteRejection reports whether an action error proves the request did
// not execute: a Korbit error envelope (the server received and rejected it)
// or a client-side validation error (nothing was sent). Anything else —
// timeout, connection drop — is ambiguous: the request may have executed.
func isDefiniteRejection(err error) bool {
	var ae *output.ApiError
	var ue *output.UsageError
	return errors.As(err, &ae) || errors.As(err, &ue)
}

// fundingErrorText renders an action error like errorText, appending the
// key-permission recovery step when the server refused authorization — the
// standing fix is a key provisioned with transfer permissions.
func fundingErrorText(err error) string {
	text := errorText(err)
	var ae *output.ApiError
	if errors.As(err, &ae) && (ae.HTTPStatus == 401 || ae.HTTPStatus == 403) {
		text += " — transfer writes need a key provisioned with `setup --with-transfers`"
	}
	return text
}

// --- key handling ---

// handleKey routes one key press by the funding view. blocked mirrors the
// parent's order-action-in-flight state: with it set, no funding money action
// may dispatch (one money action TUI-wide). closed=true tells the parent to
// leave modeFunding.
func (f fundingModel) handleKey(msg tea.KeyPressMsg, blocked bool) (fundingModel, tea.Cmd, bool) {
	switch f.view {
	case fundingBusy:
		// The request is on the wire; nothing to interact with. esc returns to
		// the browse pane (the action continues and its result still lands).
		if msg.String() == "esc" {
			f.view = fundingBrowse
		}
		return f, nil, false

	case fundingConfirmView:
		switch msg.String() {
		case "enter":
			return f.dispatchAction(blocked)
		case "esc":
			// Back out to browse: the form (pane state) is intact for a request,
			// and a cancel came from the history — the prior focus still holds.
			f.view = fundingBrowse
		}
		return f, nil, false
	}

	return f.handleBrowseKey(msg, blocked)
}

// dispatchAction sends the confirmed money action, entering the busy view.
// The single-flight gate is checked here — the last point before the wire.
func (f fundingModel) dispatchAction(blocked bool) (fundingModel, tea.Cmd, bool) {
	if f.actionInFlight || blocked {
		return f, nil, false // the confirm stays up; the strip explains the gate
	}
	f.actionInFlight = true
	f.view = fundingBusy
	return f, tea.Batch(runFundingAction(f.api, f.accountSeq, f.action), f.spin.Tick), false
}

// handleBrowseKey handles the master–detail browse view's keys. One key rule
// (the same as the main screen's panes): ↑/↓ move within the focused pane —
// including the request pane's field cursor — ←/→ change the value of the
// selector under the cursor, typing edits the amount input under the cursor,
// enter activates. While the amount input holds the cursor, every key not
// claimed by navigation is text.
func (f fundingModel) handleBrowseKey(msg tea.KeyPressMsg, blocked bool) (fundingModel, tea.Cmd, bool) {
	if f.searching {
		return f.handleSearchKey(msg)
	}
	s := msg.String()

	// Keys that hold everywhere, even while the amount input is under the
	// cursor: esc leaves the screen, tab moves the pane focus.
	switch s {
	case "esc":
		return f, nil, true
	case "tab", "shift+tab":
		f.focus = f.cycleFocus(s == "tab")
		f.clampFormCursor()
		return f, f.syncAmountFocus(), false
	}

	if f.amountFocused() {
		switch s {
		case "up", "down", "pgup", "pgdown":
			fm, cmd := f.handleNav(msg)
			return fm, cmd, false
		case "enter":
			// The amount is the last input: enter moves on to the button.
			f.formCursor = maxInt(0, len(f.formFields())-1)
			return f, f.syncAmountFocus(), false
		default:
			// Everything else — characters, ←/→, home/end, backspace — is text
			// editing. A command letter typed here is amount text, not a command.
			if msg.Text != "" || s == "backspace" || s == "delete" {
				f.formErr = ""
			}
			var cmd tea.Cmd
			f.amount, cmd = f.amount.Update(msg)
			return f, cmd, false
		}
	}

	switch s {
	case "up", "down", "pgup", "pgdown", "home", "end":
		fm, cmd := f.handleNav(msg)
		return fm, cmd, false
	case "left", "right":
		// ←/→ change the value of the selector under the form cursor.
		if f.focus == fundingFocusForm {
			fm, cmd := f.adjustField(s == "right")
			return fm, cmd, false
		}
		return f, nil, false
	case "/":
		f.searching, f.query = true, ""
		f.searchSel = f.selected // the highlight starts where the selection is
		f.focus = fundingFocusList
		f.amount.Blur()
		return f, nil, false
	case "r":
		fm := f
		cmd := fm.refresh()
		return fm, cmd, false
	case "=":
		f.resetLayout()
		f.clampScrolls()
		return f, nil, false
	case "enter":
		switch f.focus {
		case fundingFocusForm:
			fm, cmd := f.activateField(blocked)
			return fm, cmd, false
		case fundingFocusList:
			// Drill toward the request pane, where every action lives.
			f.focus = fundingFocusForm
			f.clampFormCursor()
			return f, f.syncAmountFocus(), false
		}
		return f, nil, false
	case "x":
		return f.startCancel(), nil, false
	}
	return f, nil, false
}

// amountFocused reports whether the amount input owns the keyboard: the
// request pane holds focus and the field cursor sits on the amount row.
func (f fundingModel) amountFocused() bool {
	return f.view == fundingBrowse && f.focus == fundingFocusForm && f.curField() == ffAmount
}

// clampFormCursor keeps the field cursor within the current field set (which
// changes with the tab and selection).
func (f *fundingModel) clampFormCursor() {
	f.formCursor = clamp(f.formCursor, 0, maxInt(0, len(f.formFields())-1))
}

// syncAmountFocus focuses the amount text input exactly while it is the field
// under the cursor (so it shows a cursor and takes typing), blurring it
// otherwise. Call after any focus or field-cursor change.
func (f *fundingModel) syncAmountFocus() tea.Cmd {
	if f.amountFocused() {
		if !f.amount.Focused() {
			return f.amount.Focus()
		}
		return nil
	}
	f.amount.Blur()
	return nil
}

// handleSearchKey owns the keyboard while the currency filter is active: the
// list shows matches live, up/down move the HIGHLIGHT within them, enter
// commits the highlighted currency as the selection, esc cancels with no
// effect — the same contract as the main screen's quick search. Backspace on
// an empty query cancels, the shared back-out gesture.
func (f fundingModel) handleSearchKey(msg tea.KeyPressMsg) (fundingModel, tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		f.endSearch()
		return f, nil, false
	case "enter":
		cur := f.searchSel
		f.endSearch()
		rows := f.currencyRows() // the full list — the query is cleared
		if idx := fundingSelIdx(rows, cur); idx >= 0 && cur != f.selected {
			fm, cmd := f.selectCurrency(cur, idx, len(rows))
			return fm, cmd, false
		}
		return f, nil, false
	case "up", "down", "pgup", "pgdown":
		f.moveSearchSel(msg)
		return f, nil, false
	case "backspace":
		if f.query == "" {
			f.endSearch()
			return f, nil, false
		}
		f.query = f.query[:len(f.query)-1]
		f.anchorSearchSel()
		return f, nil, false
	default:
		if t := msg.Text; t != "" && len(f.query) < searchMax {
			f.query += t
			f.anchorSearchSel()
		}
		return f, nil, false
	}
}

// endSearch leaves the filter, dropping back to the full list with the real
// selection untouched and scrolled back into view.
func (f *fundingModel) endSearch() {
	f.searching, f.query, f.searchSel = false, "", ""
	rows := f.currencyRows()
	if idx := fundingSelIdx(rows, f.selected); idx >= 0 {
		f.ensureListVisible(idx, len(rows))
	}
	f.clampScrolls()
}

// anchorSearchSel re-anchors the highlight on the first match after the query
// changed (mirroring the main search's cursor re-anchor).
func (f *fundingModel) anchorSearchSel() {
	rows := f.currencyRows()
	f.listScroll = 0
	if idx := fundingStep(rows, -1, 0); idx >= 0 { // -1 seed → first selectable row
		f.searchSel = rows[idx].Cur
		f.ensureListVisible(idx, len(rows))
	} else {
		f.searchSel = ""
	}
}

// moveSearchSel moves the highlight within the filtered rows.
func (f *fundingModel) moveSearchSel(msg tea.KeyPressMsg) {
	rows := f.currencyRows()
	step := 0
	switch msg.String() {
	case "down":
		step = 1
	case "up":
		step = -1
	case "pgdown":
		step = f.listVisible()
	case "pgup":
		step = -f.listVisible()
	}
	idx := fundingSelIdx(rows, f.searchSel)
	if nidx := fundingStep(rows, idx, step); nidx >= 0 {
		f.searchSel = rows[nidx].Cur
		f.ensureListVisible(nidx, len(rows))
	}
}

// cycleFocus advances the browse focus through list → info → history (forward)
// or the reverse; the three are contiguous enum values.
func (f fundingModel) cycleFocus(forward bool) fundingFocus {
	const n = fundingFocus(3)
	if forward {
		return (f.focus + 1) % n
	}
	return (f.focus + n - 1) % n
}

// switchTab moves the detail to the given direction and fetches what it needs.
// The amount is cleared (its meaning changes with the direction); the
// destination pick survives a round trip — it was explicit.
func (f fundingModel) switchTab(tab fundingTab) (fundingModel, tea.Cmd) {
	if f.tab == tab {
		return f, nil
	}
	f.tab = tab
	f.histCursor, f.histScroll = 0, 0
	f.formCursor = 0 // stay on the direction chips (the field set changes)
	f.amount.SetValue("")
	f.formErr = ""
	f.bump()
	cmd := tea.Batch(f.ensureDetail(), f.syncAmountFocus())
	return f, cmd
}

// handleNav routes a navigation key to the focused browse pane. A currency
// move returns a debounced settle command, so scrubbing the list doesn't fire
// a detail fetch per row.
func (f fundingModel) handleNav(msg tea.KeyPressMsg) (fundingModel, tea.Cmd) {
	if f.focus == fundingFocusForm && !f.searching {
		f.moveFormCursor(msg)
		return f, f.syncAmountFocus()
	}
	if f.focus == fundingFocusHistory && !f.searching {
		f.moveHistCursor(msg)
		return f, nil
	}
	rows := f.currencyRows()
	idx := fundingSelIdx(rows, f.selected)
	step := 0
	switch msg.String() {
	case "down":
		step = 1
	case "up":
		step = -1
	case "pgdown":
		step = f.listVisible()
	case "pgup":
		step = -f.listVisible()
	case "home":
		step = -len(rows)
	case "end":
		step = len(rows)
	}
	nidx := fundingStep(rows, idx, step)
	if nidx < 0 || (idx >= 0 && rows[nidx].Cur == rows[idx].Cur) {
		return f, nil
	}
	return f.selectCurrency(rows[nidx].Cur, nidx, len(rows))
}

// selectCurrency moves the selection to a currency (keyboard or mouse),
// keeping it visible and debouncing the detail fetch. The form starts fresh:
// a destination pick or an amount must never carry across currencies.
func (f fundingModel) selectCurrency(cur string, idx, total int) (fundingModel, tea.Cmd) {
	f.selected = cur
	f.netIdx = 0
	f.histCursor, f.histScroll = 0, 0
	f.formCursor = 0
	f.resetForm()
	f.ensureListVisible(idx, total)
	f.detailGen++
	return f, tea.Batch(fundingSettleTick(f.detailGen), f.syncAmountFocus())
}

// moveFormCursor moves the field cursor within the request pane (clamped to
// the field set, which changes with the tab and selection).
func (f *fundingModel) moveFormCursor(msg tea.KeyPressMsg) {
	n := len(f.formFields())
	if n == 0 {
		f.formCursor = 0
		return
	}
	switch msg.String() {
	case "down":
		f.formCursor = clamp(f.formCursor+1, 0, n-1)
	case "up":
		f.formCursor = clamp(f.formCursor-1, 0, n-1)
	case "home":
		f.formCursor = 0
	case "end":
		f.formCursor = n - 1
	}
}

// moveHistCursor moves the history selection, keeping it visible.
func (f *fundingModel) moveHistCursor(msg tea.KeyPressMsg) {
	n := len(f.currentHistRows())
	if n == 0 {
		f.histCursor, f.histScroll = 0, 0
		return
	}
	vis := f.histVisible()
	switch msg.String() {
	case "down":
		f.histCursor = clamp(f.histCursor+1, 0, n-1)
	case "up":
		f.histCursor = clamp(f.histCursor-1, 0, n-1)
	case "pgdown":
		f.histCursor = clamp(f.histCursor+vis, 0, n-1)
	case "pgup":
		f.histCursor = clamp(f.histCursor-vis, 0, n-1)
	case "home":
		f.histCursor = 0
	case "end":
		f.histCursor = n - 1
	}
	f.ensureHistVisible()
}

// startGenerate kicks off deposit-address generation for the selected crypto
// currency+network (idempotent server-side, so no confirm step), unless one is
// already assigned or an action is in flight.
func (f fundingModel) startGenerate(blocked bool) (fundingModel, tea.Cmd) {
	if f.tab != fundingDeposit || f.selected == fundingKRW || f.generating {
		return f, nil
	}
	if f.actionInFlight || blocked {
		return f, nil
	}
	if _, ok := f.depositAddrFor(f.selected, f.selectedNetworkName()); ok {
		return f, nil // already assigned; the strip doesn't offer g then
	}
	f.generating = true
	f.bump()
	return f, tea.Batch(generateFundingAddr(f.api, f.accountSeq, f.selected, f.selectedNetworkName()), f.spin.Tick)
}

// startCancel opens the confirm step for canceling the selected withdrawal
// row, when it is cancelable (crypto only; actionRequired/reviewing).
func (f fundingModel) startCancel() fundingModel {
	tr, ok := f.selectedTransfer()
	if !ok || !fundingCancelable(f.tab, f.selected, tr) {
		return f
	}
	f.action = fundingAction{kind: factCancel, cancelID: tr.ID, cancelCur: f.selected}
	f.view = fundingConfirmView
	return f
}

// fundingCancelable reports whether a history row is a cancelable crypto
// withdrawal: the server only cancels actionRequired/reviewing withdrawals,
// and KRW withdrawals cannot be canceled through the API at all.
func fundingCancelable(tab fundingTab, cur string, tr FundingTransfer) bool {
	if tab != fundingWithdraw || cur == fundingKRW {
		return false
	}
	return tr.Status == "actionRequired" || tr.Status == "reviewing"
}

// selectedTransfer is the history row under the cursor.
func (f fundingModel) selectedTransfer() (FundingTransfer, bool) {
	rows := f.currentHistRows()
	if f.histCursor < 0 || f.histCursor >= len(rows) {
		return FundingTransfer{}, false
	}
	return rows[f.histCursor], true
}

// currentHistRows is the fetched history for the selected currency+tab (nil
// while loading/errored).
func (f fundingModel) currentHistRows() []FundingTransfer {
	if h, ok := f.hist[histKey{f.selected, f.tab}]; ok && h.loaded {
		return h.rows
	}
	return nil
}

// --- lookups over the fetched data ---

// currencyFor finds a currency in the catalog; ok=false when the catalog
// hasn't loaded it (the KRW row exists even then).
func (f fundingModel) currencyFor(code string) (FundingCurrency, bool) {
	for _, c := range f.currencies {
		if c.Currency == code {
			return c, true
		}
	}
	return FundingCurrency{Currency: code}, false
}

// balanceFor finds the streamed balance for a currency (under the funding
// screen's sub-account).
func (f fundingModel) balanceFor(code string) (state.Balance, bool) {
	for _, b := range f.store.BalancesFor(f.accountSeq) {
		if b.Currency == code {
			return b, true
		}
	}
	return state.Balance{}, false
}

// selectedNetworks is the selected currency's network list (empty for KRW or
// before the catalog loads).
func (f fundingModel) selectedNetworks() []FundingNetwork {
	c, _ := f.currencyFor(f.selected)
	return c.Networks
}

// selectedNetwork is the network the [ ] selector points at.
func (f fundingModel) selectedNetwork() (FundingNetwork, bool) {
	nets := f.selectedNetworks()
	if len(nets) == 0 {
		return FundingNetwork{}, false
	}
	return nets[clamp(f.netIdx, 0, len(nets)-1)], true
}

// selectedNetworkName is the [ ] selector's network name ("" when unknown, so
// the request falls back to the server-side default network).
func (f fundingModel) selectedNetworkName() string {
	n, ok := f.selectedNetwork()
	if !ok {
		return ""
	}
	return n.Name
}

// --- the request pane's fields ---

// formFields is the ordered set of interactive rows the request pane exposes,
// for the current tab and selection. ↑/↓ move the cursor among them; ←/→
// change the selector under it; enter activates. The withdraw flow reads top
// to bottom: network → address (filtered to it) → amount → button.
func (f fundingModel) formFields() []formField {
	fs := []formField{ffDirection}
	if f.selected == fundingKRW {
		return append(fs, ffAmount, ffButton)
	}
	if len(f.selectedNetworks()) > 1 {
		fs = append(fs, ffNetwork)
	}
	if f.tab == fundingWithdraw {
		return append(fs, ffAddress, ffAmount, ffButton)
	}
	// Crypto deposit: the assigned address and its memo are copy targets; while
	// no address is assigned the generate button stands in their place.
	if a, ok := f.depositAddrFor(f.selected, f.selectedNetworkName()); ok {
		fs = append(fs, ffAddress)
		if a.SecondaryAddress != "" {
			fs = append(fs, ffMemo)
		}
		return fs
	}
	if _, ok := f.formActionLabel(); ok {
		fs = append(fs, ffButton)
	}
	return fs
}

// curField is the field under the form cursor.
func (f fundingModel) curField() formField {
	fs := f.formFields()
	if len(fs) == 0 {
		return ffNone
	}
	return fs[clamp(f.formCursor, 0, len(fs)-1)]
}

// fieldIndex is a field's position in formFields (-1 when absent).
func (f fundingModel) fieldIndex(fld formField) int {
	for i, x := range f.formFields() {
		if x == fld {
			return i
		}
	}
	return -1
}

// formActionLabel is the primary action button's label for the current tab and
// selection, and whether there is one. The crypto deposit tab offers a button
// only while no address is assigned (once one is, there is nothing to do).
func (f fundingModel) formActionLabel() (string, bool) {
	switch {
	case f.selected == fundingKRW && f.tab == fundingDeposit:
		return i18n.T("request KRW deposit…"), true
	case f.selected == fundingKRW && f.tab == fundingWithdraw:
		return i18n.T("request KRW withdrawal…"), true
	case f.tab == fundingWithdraw:
		return i18n.T("review withdrawal…"), true
	default: // crypto deposit tab
		if _, ok := f.depositAddrFor(f.selected, f.selectedNetworkName()); !ok &&
			f.depAddrsReady && !f.generating {
			return i18n.T("generate deposit address"), true
		}
		return "", false
	}
}

// adjustField applies ←/→ to the field under the cursor: the direction chips
// pick their side (← deposit, → withdraw), the network selector cycles, the
// destination selector steps. A no-op on the amount (handled as text) and the
// button.
func (f fundingModel) adjustField(forward bool) (fundingModel, tea.Cmd) {
	switch f.curField() {
	case ffDirection:
		tab := fundingDeposit
		if forward {
			tab = fundingWithdraw
		}
		return f.switchTab(tab)
	case ffNetwork:
		return f.cycleNetwork(forward), nil
	case ffAddress:
		if f.tab == fundingWithdraw {
			f.moveWdAddr(forward)
			f.bump()
		}
	}
	return f, nil
}

// activateField runs enter on the field under the cursor: the button acts, the
// deposit address/memo copy to the clipboard, everything else advances the
// cursor — so enter alone walks the form top to bottom.
func (f fundingModel) activateField(blocked bool) (fundingModel, tea.Cmd) {
	switch f.curField() {
	case ffButton:
		return f.doAction(blocked)
	case ffAddress:
		if f.tab == fundingDeposit {
			return f, f.copyDepositField(false)
		}
	case ffMemo:
		return f, f.copyDepositField(true)
	}
	f.moveFormCursor(tea.KeyPressMsg{Code: tea.KeyDown})
	return f, f.syncAmountFocus()
}

// doAction runs the primary action for the current tab and selection: crypto
// withdraw and the KRW pushes validate and enter the confirm step; the crypto
// deposit button generates an address.
func (f fundingModel) doAction(blocked bool) (fundingModel, tea.Cmd) {
	if f.tab == fundingDeposit && f.selected != fundingKRW {
		return f.startGenerate(blocked)
	}
	if f.selected == fundingKRW {
		if err := f.validateKRW(); err != "" {
			f.formErr = err
			f.bump()
			return f, nil
		}
		kind := factKRWDeposit
		if f.tab == fundingWithdraw {
			kind = factKRWWithdraw
		}
		f.formErr = ""
		f.action = fundingAction{kind: kind, amount: strings.TrimSpace(f.amount.Value())}
		f.view = fundingConfirmView
		return f, nil
	}
	if err := f.validateWithdraw(); err != "" {
		f.formErr = err
		f.bump()
		return f, nil
	}
	f.formErr = ""
	f.action = fundingAction{kind: factWithdraw, order: f.buildWithdrawOrder()}
	f.view = fundingConfirmView
	return f, nil
}

// copyDepositField copies the assigned deposit address (or its memo/tag) to
// the system clipboard via OSC 52 — the TUI captures the mouse, so native
// text selection is unavailable and this is the copy path. The ack rides a
// parent toast; OSC 52 support depends on the terminal.
func (f fundingModel) copyDepositField(memo bool) tea.Cmd {
	a, ok := f.depositAddrFor(f.selected, f.selectedNetworkName())
	if !ok {
		return nil
	}
	text, what := a.Address, i18n.T("deposit address")
	if memo {
		if a.SecondaryAddress == "" {
			return nil
		}
		text, what = a.SecondaryAddress, i18n.T("memo/tag")
	}
	return tea.Batch(tea.SetClipboard(text),
		func() tea.Msg { return fundingCopiedMsg{what: what} })
}

// cycleNetwork moves the network selector (a no-op with fewer than two). The
// address list is network-filtered, so any destination pick is dropped with
// the network that scoped it.
func (f fundingModel) cycleNetwork(forward bool) fundingModel {
	nets := f.selectedNetworks()
	if len(nets) < 2 {
		return f
	}
	d := 1
	if !forward {
		d = len(nets) - 1
	}
	f.netIdx = (clamp(f.netIdx, 0, len(nets)-1) + d) % len(nets)
	f.wdAddrIdx = -1
	f.formErr = ""
	f.bump()
	return f
}

// depositAddrFor finds the assigned deposit address for a currency+network
// (network "" matches the first address for the currency).
func (f fundingModel) depositAddrFor(cur, network string) (FundingDepositAddress, bool) {
	for _, a := range f.depAddrs {
		if a.Currency != cur {
			continue
		}
		if network == "" || a.Network == network {
			return a, true
		}
	}
	return FundingDepositAddress{}, false
}

// withdrawAddrsFor is the registered withdrawal addresses usable for a
// currency: registered for it directly, or network-wide registrations on a
// network the currency moves on.
func (f fundingModel) withdrawAddrsFor(cur string) []FundingWithdrawAddress {
	nets := map[string]bool{}
	if c, ok := f.currencyFor(cur); ok {
		for _, n := range c.Networks {
			nets[n.Name] = true
		}
	}
	var out []FundingWithdrawAddress
	for _, a := range f.wdAddrs {
		if a.Currency == cur || (a.Currency == "" && nets[a.Network]) {
			out = append(out, a)
		}
	}
	return out
}

// --- the currency list projection ---

// fundingRow is one currency-list row (or the held/unheld divider). Value is
// the estimated KRW value of the holding (balance × the pair's last price),
// present only when that pair's ticker is subscribed and ready — a session
// narrowed with --symbols simply shows no estimate for the rest.
type fundingRow struct {
	Cur       string // wire code; "" for the divider row
	Avail     string
	Value     string
	Held      bool
	Suspended bool
	Divider   bool
}

// currencyRows builds the visible list: KRW pinned first, held assets by
// estimated value (unpriced ones after, alphabetical), a divider, then the
// unheld rest alphabetically — filtered live by the search query. It is
// derived on demand (never cached): balances and tickers move under it, and
// the list is small.
func (f fundingModel) currencyRows() []fundingRow {
	bals := map[string]state.Balance{}
	for _, b := range f.store.BalancesFor(f.accountSeq) {
		bals[b.Currency] = b
	}

	match := func(code, full string) bool {
		if f.query == "" {
			return true
		}
		q := strings.ToLower(strings.TrimSpace(f.query))
		return strings.Contains(strings.ToLower(code), q) || strings.Contains(strings.ToLower(full), q)
	}

	var krw *fundingEntry
	var held, unheld []fundingEntry

	add := func(c FundingCurrency) {
		if !match(c.Currency, c.FullName) {
			return
		}
		b, hasBal := bals[c.Currency]
		heldNow := hasBal && !isZeroDecimal(b.Balance)
		e := fundingEntry{row: fundingRow{Cur: c.Currency, Held: heldNow, Suspended: fundingSuspended(c)}}
		if hasBal {
			e.row.Avail = b.Available
		}
		if c.Currency == fundingKRW {
			if hasBal {
				e.row.Value = b.Balance
			}
			krw = &e
			return
		}
		if heldNow {
			if v, ok := f.estValue(c.Currency, b.Balance); ok {
				e.row.Value = v.Round(0).String()
				e.val, e.ok = v, true
			}
			held = append(held, e)
			return
		}
		unheld = append(unheld, e)
	}

	seen := map[string]bool{}
	for _, c := range f.currencies {
		add(c)
		seen[c.Currency] = true
	}
	// Balances can hold currencies the catalog is missing (or the catalog may
	// not be loaded yet) — a held asset must never disappear from the list.
	for code := range bals {
		if !seen[code] {
			add(FundingCurrency{Currency: code})
		}
	}
	if krw == nil && match(fundingKRW, "korean won") {
		krw = &fundingEntry{row: fundingRow{Cur: fundingKRW}}
	}

	// Held: estimated value descending; unpriced after, alphabetical.
	sort.SliceStable(held, func(i, j int) bool { return fundingEntryLess(held[i], held[j]) })
	sort.SliceStable(unheld, func(i, j int) bool { return fundingEntryLess(unheld[i], unheld[j]) })

	rows := make([]fundingRow, 0, 2+len(held)+len(unheld))
	if krw != nil {
		rows = append(rows, krw.row)
	}
	for _, e := range held {
		rows = append(rows, e.row)
	}
	if len(unheld) > 0 && len(rows) > 0 {
		rows = append(rows, fundingRow{Divider: true})
	}
	for _, e := range unheld {
		rows = append(rows, e.row)
	}
	return rows
}

// fundingEntry is a list row plus its sort key (the parsed estimated value).
type fundingEntry struct {
	row fundingRow
	val decimal.Decimal
	ok  bool // val parsed
}

// fundingEntryLess orders held/unheld entries: priced by value descending,
// then unpriced, alphabetical within each.
func fundingEntryLess(a, b fundingEntry) bool {
	switch {
	case a.ok && b.ok:
		if !a.val.Equal(b.val) {
			return a.val.GreaterThan(b.val)
		}
	case a.ok != b.ok:
		return a.ok
	}
	return a.row.Cur < b.row.Cur
}

// estValue is the holding's estimated KRW value: balance × the pair's last
// price, only when the <cur>_krw ticker is subscribed and ready. Display-only
// arithmetic — wire values are never derived from it.
func (f fundingModel) estValue(cur, balance string) (decimal.Decimal, bool) {
	sym := cur + "_krw"
	if !f.store.TickerReady(sym) {
		return decimal.Decimal{}, false
	}
	t, ok := f.store.Ticker(sym)
	if !ok {
		return decimal.Decimal{}, false
	}
	b, err1 := decimal.NewFromString(balance)
	p, err2 := decimal.NewFromString(t.Close)
	if err1 != nil || err2 != nil {
		return decimal.Decimal{}, false
	}
	return b.Mul(p), true
}

// lastPrice is the selected pair's last traded price for the detail header,
// "" when its ticker isn't subscribed/ready.
func (f fundingModel) lastPrice(cur string) string {
	sym := cur + "_krw"
	if !f.store.TickerReady(sym) {
		return ""
	}
	t, ok := f.store.Ticker(sym)
	if !ok {
		return ""
	}
	return t.Close
}

// fundingSuspended reports whether every network of a currency has both
// deposits and withdrawals stopped (the list-level ⊘ marker; per-direction
// detail shows in the info panel). Fiat (no networks) is never suspended.
func fundingSuspended(c FundingCurrency) bool {
	if len(c.Networks) == 0 {
		return false
	}
	for _, n := range c.Networks {
		if n.DepositLaunched || n.WithdrawalLaunched {
			return false
		}
	}
	return true
}

// isZeroDecimal reports whether a decimal string is zero (or unparseable —
// treated as no balance).
func isZeroDecimal(s string) bool {
	d, err := decimal.NewFromString(s)
	return err != nil || d.IsZero()
}

// fundingSelIdx is the index of the selected currency in the rows (-1 when
// filtered out / not present).
func fundingSelIdx(rows []fundingRow, selected string) int {
	for i, r := range rows {
		if !r.Divider && r.Cur == selected {
			return i
		}
	}
	return -1
}

// fundingStep moves an index by step rows, skipping the divider and clamping
// to the list; from -1 (selection filtered out) any move lands on the first
// selectable row. Returns -1 only for an empty list.
func fundingStep(rows []fundingRow, idx, step int) int {
	first, last := -1, -1
	for i, r := range rows {
		if !r.Divider {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return -1
	}
	if idx < 0 {
		return first
	}
	i := idx
	dir := 1
	if step < 0 {
		dir, step = -1, -step
	}
	for ; step > 0; step-- {
		n := i + dir
		for n >= first && n <= last && rows[n].Divider {
			n += dir
		}
		if n < first || n > last {
			break
		}
		i = n
	}
	return clamp(i, first, last)
}
