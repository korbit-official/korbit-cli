// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/korbit-official/korbit-cli/internal/candles"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/candlechart"
	"github.com/korbit-official/korbit-cli/internal/tui/components/balances"
	"github.com/korbit-official/korbit-cli/internal/tui/components/fills"
	"github.com/korbit-official/korbit-cli/internal/tui/components/footer"
	"github.com/korbit-official/korbit-cli/internal/tui/components/header"
	"github.com/korbit-official/korbit-cli/internal/tui/components/ladder"
	"github.com/korbit-official/korbit-cli/internal/tui/components/notices"
	"github.com/korbit-official/korbit-cli/internal/tui/components/orderbook"
	"github.com/korbit-official/korbit-cli/internal/tui/components/orders"
	"github.com/korbit-official/korbit-cli/internal/tui/components/sidebar"
	"github.com/korbit-official/korbit-cli/internal/tui/components/trades"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// mode is the input mode: which surface owns the next key press.
type mode int

const (
	modeNormal mode = iota
	modeOrder       // order entry: the docked order panel replaces the right column
	modeLadder      // trade ladder: the DOM-style ladder replaces the orderbook/trades center
	modeConfirm
	modeCancelAll
	modeHelp
	modeNotices
	modeChart
	modeFunding
	modeQuitConfirm   // q pressed while a money action is in flight: confirm before quitting
	modeAccountSwitch // '@' pressed: the sub-account switcher popup
)

// dragKind identifies which resizable seam a mouse drag is currently moving.
type dragKind int

const (
	dragNone     dragKind = iota
	dragColS              // sidebar | orderbook
	dragColA              // orderbook | trades
	dragColB              // trades | right column
	dragRowA              // orders | fills (private right column)
	dragRowB              // fills | balances
	dragRowChart          // inline chart strip | orderbook/trades (center region)
	dragFundSide          // funding: currency list | detail column
)

// focusPane is the interactive pane that receives navigation keys (up/down,
// paging). tab/shift+tab cycle focus; mouse click focuses a pane directly.
type focusPane int

const (
	focusMarket   focusPane = iota // the symbol selector
	focusOrders                    // the open-orders table (private mode only)
	focusBalances                  // the balances list (private mode only) — scrolls, no selection
)

// Messages.
type streamEventMsg struct{ ev stream.Event }    // a single stream event (tests)
type streamBatchMsg struct{ evs []stream.Event } // coalesced events from the pump
type eventsClosedMsg struct{}

// isOrderEvent reports whether a stream event changes the open-order set, so the
// orders table is rebuilt only when it actually needs to be (not on every
// ticker/trade/balance tick).
func isOrderEvent(ev stream.Event) bool {
	d, ok := ev.(stream.Data)
	return ok && d.Channel == stream.ChannelMyOrder
}

// logNotice mirrors a reliability notice into the TUI's notice logger when
// --log-file is set (tagged component=stream kind=stream_notice). On the alt-screen the
// notice is shown live in the footer status line and the 'n' popup; this is its
// persistent record. A no-op for data events or when no log file is configured.
func (m model) logNotice(ev stream.Event) {
	if m.cfg.NoticeLog == nil {
		return
	}
	if n, ok := ev.(stream.Notice); ok {
		stream.LogNotice(m.cfg.NoticeLog, n)
	}
}

// placeOrigin is which entry surface armed a placed order, so the result
// routes back to it (inline on the surface, vs a toast for the others).
type placeOrigin int

const (
	placeFromPanel placeOrigin = iota
	placeFromBar
	placeFromLadder
)

type placeDoneMsg struct {
	res    PlaceResult
	err    error
	origin placeOrigin
	// accountSeq/clientOrderID identify the local balance hold registered at
	// dispatch, so a failed place releases it (see dispatchPlaceForm).
	accountSeq    int
	clientOrderID string
}

type cancelDoneMsg struct {
	orderID int64
	warning string
	err     error
}

// cancelAllStepMsg reports the result of one order's cancel within a cancel-all
// batch (the batch issues them one at a time — there is no bulk cancel endpoint
// and the single-action in-flight gate must hold).
type cancelAllStepMsg struct {
	orderID int64
	err     error
}
type clockTickMsg time.Time

// resubscribeMsg fires (after a short debounce) to re-point the active symbol's
// orderbook/trade subscriptions. The selection (m.active) changes instantly on
// each arrow; the actual Subscribe/Unsubscribe is debounced so scrubbing through
// the list doesn't churn the stream. gen is the debounce generation — only the
// latest scheduled tick acts.
type resubscribeMsg struct{ gen int }

func clockTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return clockTickMsg(t) })
}

const (
	// wheelStep is how many rows one mouse-wheel notch scrolls.
	wheelStep = 3
	// resubscribeDelayMs debounces the active-symbol re-subscribe so holding an
	// arrow key through the list doesn't fire a Subscribe/Unsubscribe per row.
	resubscribeDelayMs = 120
)

// cancelTarget is one order in a cancel-all snapshot: the symbol is captured
// alongside the id so the cancel can be issued even after the order leaves the
// store (filled/closed).
type cancelTarget struct {
	symbol     string
	orderID    int64
	accountSeq int // the order's own sub-account, captured at snapshot time
}

// cancelAllState is the "cancel all" dialog. The order set it will cancel is
// SNAPSHOTTED (frozen) the moment the dialog opens or the scope changes and the
// scope's orders are loaded — so the count the user confirms can't drift as live
// orders come and go. It is reset to the zero value whenever the dialog closes,
// so a reopen always starts clean.
type cancelAllState struct {
	active   bool           // the dialog is open
	allPairs bool           // scope: true = every watched pair, false = the active pair
	snapped  bool           // a snapshot has been taken for the current scope (orders were loaded)
	snapshot []cancelTarget // the frozen set to cancel
	// Batch execution (sequential, one in flight at a time).
	running bool
	aborted bool // esc during the run: stop after the in-flight cancel returns
	idx     int  // next snapshot index to dispatch
	ok      int  // cancels accepted so far
	failed  int  // cancels that errored so far
}

// toast is a transient status line (order results, cancel results).
type toast struct {
	text    string
	isError bool
	until   int64 // unix ms expiry
}

const toastMs = 1_500

// flashMs is how long a just-accepted order's row stays highlighted in the
// open-orders panel.
const flashMs = 3_000

type model struct {
	cfg   Config
	store *state.Store

	// Pane view-components. Each is a pure renderer caching its last frame on a
	// comparable key (store revisions + geometry + style + this pane's UI scalars),
	// so a redraw that doesn't touch a pane reuses its cached string. They are
	// pointers so the value-receiver View can write through the shared cache. The
	// model owns the interactive state; it builds each component's Key/Data.
	cHeader    *header.Model
	cFooter    *footer.Model
	cSidebar   *sidebar.Model
	cOrderbook *orderbook.Model
	cTrades    *trades.Model
	cFills     *fills.Model
	cBalances  *balances.Model
	cOrders    *orders.Model
	cNotices   *notices.Model
	cLadder    *ladder.Model
	ordersRev  uint64 // bumped whenever the order table projection (orderRows) is rebuilt

	w, h  int
	ready bool // first WindowSizeMsg seen

	active    int       // index into cfg.Symbols: the selected symbol the detail panels show
	scroll    int       // sidebar window top row, moved by the mouse wheel only
	balScroll int       // balances list window top row (no selection — it just scrolls)
	focus     focusPane // which pane navigation keys drive
	mode      mode

	// Sub-account switching (private, multi-account sessions). activeSeq is the
	// sub-account the detail panels, tracking, and order dispatch currently use —
	// the single value model.accountSeq() returns. accountSeqs is the switchable
	// set (cfg.AccountSeqs; the '@' popup lists it), acctCursor is the popup's
	// keyboard-highlighted row, and acctScroll is the top row of the popup's fixed
	// window into accountSeqs (the list scrolls when it exceeds
	// accountSwitchMaxRows). A session with <=1 account leaves the switcher inert.
	activeSeq   int
	accountSeqs []int
	acctCursor  int
	acctScroll  int

	// Coloring: the color scheme is the up/down convention (green-red or red-blue), profile
	// is the terminal's detected color support (arrives via tea.ColorProfileMsg),
	// and pal is the up/down styles resolved from the two. 'C' toggles the color scheme.
	colorScheme uikit.ColorScheme
	profile     colorprofile.Profile
	pal         uikit.Palette

	// subscribed is the symbol whose orderbook/trade channels are currently
	// subscribed; subscribedGrp is the orderbook grouping level that
	// subscription carries ("" = the raw book); subGen is the debounce
	// generation for re-pointing them when the selection settles (see
	// resubscribeMsg / setActive / cycleBookLevel).
	subscribed    string
	subscribedGrp string
	subGen        int

	// bookGrp is each symbol's chosen orderbook grouping level ("" / absent =
	// the raw book), sticky per session; +/- cycle it through the symbol's
	// valid levels (from the tick-size policy metadata).
	bookGrp map[string]string

	// ordersAllPairs selects the orders scope (both tabs): false (default) shows
	// and keeps an authoritative snapshot for the ACTIVE pair only; true covers
	// every watched pair. Toggled with 'a' on the orders pane; drives the
	// session's tracked snapshot set via cfg.SetTrackedOrders.
	ordersAllPairs bool

	// ordersClosed selects the orders pane's closed tab: the terminal orders
	// this session has observed, most recently closed first, read-only (no
	// cancel target). Switched by 'o' (normal view from any pane, and ladder
	// browse) or a click on the title's tab switcher; the selection is STICKY
	// — it survives focus moves and mode changes until switched back. The
	// pane keeps ONE row projection — refreshOrders fills it from the open or
	// the closed set per the active tab — so the cursor/scroll/wheel plumbing
	// is tab-agnostic; the cursor resets on toggle.
	ordersClosed bool

	// The open-orders table is rendered by this package (not the bubbles table)
	// so a mouse click maps exactly to a row and the scroll model matches the
	// sidebar/balances. orderRows holds the pre-formatted cells (parallel to
	// orderIDs); orderCursor is the selected row; orderScroll is the window top.
	orderRows      [][]string
	orderIDs       []int64 // row index -> orderID (parallel to orderRows)
	orderCanceling []bool  // row index -> cancel-in-flight (parallel to orderRows)
	orderCursor    int
	orderScroll    int

	// Resizable layout, session-only (reset with '='). The vertical column
	// seams are fractions of the width; the right-column horizontal seams are
	// fractions of the body height. colWidths/rightHeights derive pixels from
	// these (with per-panel minimums); a mouse drag on a seam moves them.
	sideDiv float64 // sidebar | orderbook seam
	colDivA float64 // orderbook | trades seam
	colDivB float64 // trades | right-column seam
	rowDivA float64 // orders | fills seam (private right column)
	rowDivB float64 // fills | balances seam
	// chartDiv is the inline-chart strip | orderbook/trades seam, a fraction of the
	// body height of the center region. Only used while the inline chart is shown.
	chartDiv float64
	drag     dragKind

	// The order-entry panel ('b'/'s', modeOrder) — a self-contained
	// sub-model like the funding screen; the parent owns the mode switch,
	// key/mouse routing, geometry, and the money-action single-flight.
	order orderModel
	// The trade ladder ('t', modeLadder) — the same sub-model mold; its
	// tick/fee metadata rides the order panel's per-symbol caches.
	ladder ladderModel
	// The command bar (':'): like quick search it lives inside the normal
	// view and owns the keyboard while active; two lines above the footer.
	cmdbar         cmdBarModel
	confirmOrderID int64
	confirmSymbol  string // captured at confirm time, when the order is known to exist
	confirmSeq     int    // the order's own sub-account, captured with the symbol
	// orderInFlight is the single gate that at most one order action (a place
	// or a cancel) is dispatched at a time — the contract the Trader relies
	// on. It also blocks arming a new order or starting a cancel while an
	// action is pending (e.g. after backing out of an in-flight panel).
	orderInFlight   bool
	cancelsInFlight map[int64]bool // per-order cancel-in-flight → grayed-out order row
	cancelAll       cancelAllState
	toast           toast
	// flashOrderID highlights a just-accepted order's row in the open-orders
	// panel until flashUntil (unix ms) — the feedback lands where the eye is.
	flashOrderID int64
	flashUntil   int64
	// placedWatch holds orders placed by this session until a real terminal
	// status arrives. A time-in-force reject is not a place error — the REST
	// accept is only an ack, and the death lands later as a terminal myOrder
	// status — so noticePlacedTerminal watches for orders that end with no
	// fill and toasts their fate.
	placedWatch map[int64]bool

	// Quick search ("/"): while active, key presses build searchQuery, shown on
	// the bottom-left of the searched pane's border. searchPane is the pane the
	// search targets (focusMarket or focusBalances, captured when "/" was pressed).
	// Search FILTERS the list live — the pane shows only rows whose symbol /
	// currency contains the query. searchCursor highlights the candidate row in
	// the filtered markets list (enter selects it); searchScroll is the filtered
	// window top (both panes). The underlying active symbol and balances scroll
	// are NOT touched until enter, so esc just closes with nothing to restore.
	searching    bool
	searchPane   focusPane
	searchQuery  string
	searchCursor int // highlighted row within the filtered markets list
	searchScroll int // window top within the filtered list (markets or balances)

	// Candle chart ('g' overlay, modeChart). chart is the rendered component;
	// feed builds its live series for chartSym at chartIv from a REST seed plus
	// folded trades (internal/candles; converted to chart floats at the edge —
	// see chartCandles). chartGen invalidates stale fetch/re-sync messages when
	// the symbol or interval changes or the chart closes.
	chart    candlechart.Model
	feed     candles.Series
	chartSym string // symbol the feed/chart currently holds
	chartIv  string // candles interval, e.g. "60"
	chartGen int
	chartVol bool // volume pane (v) — on by default for the full chart
	chartInd int  // index into chartIndicatorOptions: the active overlay indicator (i); 0 = off
	// chartInline shows a compact, display-only candle pane over the orderbook/
	// trades center region (toggled with G). It renders the same feed as the 'g'
	// overlay — inheriting its interval, zoom, and volume toggle, and color
	// scheme — but always pinned to the live edge (no scroll, no selection) and
	// following the active market. Off by default; hidden on a short terminal.
	chartInline bool
	// Scroll-back history backfill: chartLoadingOlder gates a single in-flight
	// older-page fetch; chartAtOldest latches once a page comes back empty (no
	// more history). Both reset on a symbol/interval change. chartSpin animates
	// the "loading history…" hint while a backfill is in flight.
	chartLoadingOlder bool
	chartAtOldest     bool
	chartSpin         spinner.Model
	// chartErr holds the reason the chart is empty when a seed fetch failed, so
	// the overlay shows the error instead of an endless "loading…". Set only when
	// the failure left the feed empty; cleared on a successful seed or a reload.
	chartErr string
	// chartSeeded latches once a seed fetch has returned for the current
	// symbol/interval (success or empty), so an empty result reads as "no candles
	// yet" (a never-traded pair) rather than an endless "loading…". Reset on a
	// reload (loadChart) so a symbol/interval switch shows loading until its own
	// seed lands.
	chartSeeded bool
	// chartSeedRefetching guards the one re-fetch a settled-but-empty chart issues
	// when its first trades arrive (the seed defines the KST bucket grid a local
	// fold cannot). It collapses a burst of first trades into a single fetch;
	// cleared when any seed result returns (applyCandles) or on a reload.
	chartSeedRefetching bool

	// The funding screen ('f', modeFunding) — a self-contained sub-model; the
	// parent owns only the mode switch, message/key routing, and the shared
	// money-action single-flight (see moneyActionInFlight).
	funding fundingModel
}

// chartIntervals is the interval ladder the chart cycles with [ and ], matching
// the candles endpoint's enum. defaultChartInterval is where the chart opens.
var chartIntervals = []string{"1", "5", "15", "30", "60", "240", "1D", "1W"}

const (
	defaultChartInterval = "30"
	chartHistoryLimit    = 300 // candles fetched per seed/re-sync
	chartBackfillLimit   = 200 // candles fetched per scroll-back history page (one server page)

	// maxFeedCandles is the hard bound on an open chart's retained series: a window
	// of the newest N buckets, so memory stays bounded no matter how long a session
	// runs or how far back the user scrolls. It doubles as the scroll-back limit —
	// maybeBackfill stops paging older history once the window is full, and every
	// feed change trims to it (setChartFromFeed). ~35 days of 1-minute candles, deep
	// enough that the limit is invisible in normal use; at the boundary the oldest
	// bucket slides off as a newer one rolls in.
	maxFeedCandles = 50_000
)

// searchMax bounds the quick-search query length (a symbol/currency is short).
const searchMax = 32

func newModel(cfg Config) model {
	m := model{
		cfg:             cfg,
		store:           state.New(state.Config{Log: cfg.StoreLog}, cfg.Now),
		cancelsInFlight: map[int64]bool{},
		placedWatch:     map[int64]bool{},
		colorScheme:     uikit.ParseColorScheme(cfg.ColorScheme),
		profile:         colorprofile.NoTTY,                   // refined by the first tea.ColorProfileMsg
		chart:           candlechart.New(minWidth, minHeight), // sized to the terminal in the WindowSizeMsg handler
		chartVol:        true,                                 // full chart shows volume + last-price by default
		chartInline:     true,                                 // inline candle pane on by default (hidden on a short terminal)
		chartInd:        1,                                    // EMA 20 overlay on by default (index into chartIndicatorOptions)
		chartSpin:       spinner.New(spinner.WithSpinner(spinner.Dot)),
		cHeader:         header.New(),
		cFooter:         footer.New(),
		cSidebar:        sidebar.New(),
		cOrderbook:      orderbook.New(),
		cTrades:         trades.New(),
		cFills:          fills.New(),
		cBalances:       balances.New(),
		cOrders:         orders.New(),
		cNotices:        notices.New(),
		cLadder:         ladder.New(),
	}
	m.chart.SetIndicators(indicatorsFor(m.chartInd)) // wire the default indicator onto the chart
	// The active sub-account and the switchable set. activeSeq is the one value
	// accountSeq() normalizes and returns; it starts on cfg.AccountSeq and moves
	// only through switchAccount. accountSeqs is what the '@' popup offers.
	m.activeSeq = cfg.AccountSeq
	m.accountSeqs = cfg.AccountSeqs
	// Sub-models capture the account at construction — hand them the NORMALIZED
	// value (accountSeq(), never the raw cfg field) so there is exactly one
	// defaulting point. switchAccount re-stamps these fields when the active
	// account changes.
	m.funding = newFundingModel(cfg.Funding, m.store, m.accountSeq(), cfg.Now)
	sizeLevels := resolveOrderLevels(cfg.OrderLevels)
	m.order = newOrderModel(m.store, m.accountSeq(), cfg.TickSizePolicy, cfg.OrderValueBounds, cfg.Fees, sizeLevels)
	m.ladder = newLadderModel(m.store, m.accountSeq(), sizeLevels)
	m.cmdbar = newCmdBar()
	m.bookGrp = map[string]string{}
	m.pal = uikit.PaletteFor(m.colorScheme, m.profile)
	if len(cfg.Symbols) > 0 {
		m.subscribed = cfg.Symbols[0] // matches the initial orderbook/trade subscription
	}
	m.resetLayout()
	return m
}

// styleID is the comparable palette identity the pane components key their render
// caches on: a color-scheme toggle or a terminal-profile change flips it, so a
// component re-renders, while the component resolves the actual palette from it.
func (m model) styleID() uikit.StyleID {
	return uikit.StyleID{Scheme: uint8(m.colorScheme), Profile: m.profile}
}

// resetLayout restores the default panel proportions: a narrow market sidebar
// (16%), then the orderbook and trades columns (26% each), leaving 32% for the
// account column; that column splits 40/30/30 into orders/fills/balances. The
// vertical seams are stored as cumulative fractions of the width.
func (m *model) resetLayout() {
	m.sideDiv = 0.16
	m.colDivA, m.colDivB = 0.42, 0.68
	m.rowDivA, m.rowDivB = 0.40, 0.70
	m.chartDiv = 0.5 // inline chart takes the top half of the center region
}

func (m model) Init() tea.Cmd { return clockTick() }

func (m model) symbol() string { return m.cfg.Symbols[m.active] }

// accountSeq is the sub-account the TUI currently displays and places orders
// under: the model's seq-scoped store reads, tracking scopes, and order
// dispatches read it here, and the order/ladder/funding sub-models are handed
// its value at construction and re-stamped on a switch (switchAccount). It is
// the active account (activeSeq), which starts on Config.AccountSeq and moves
// through the '@' switcher; normalized to 1/main when unset per the Config
// contract — the one defaulting point below the cli boundary.
func (m model) accountSeq() int {
	if m.activeSeq < 1 {
		return 1
	}
	return m.activeSeq
}

// switchAccount makes seq the active sub-account: it re-stamps the order/ladder/
// funding sub-models (which captured the account at construction), re-points the
// session's open-order tracking to the new account's scope, and rebuilds the
// open-orders projection. The balances/fills/orders panes read through the
// per-account store accessors, so they follow on the next render — showing
// "loading…" via the per-account readiness latches until this account's backfill
// lands. A no-op if seq is already active or not one of the session's accounts.
func (m *model) switchAccount(seq int) {
	if seq == m.accountSeq() || !m.hasAccount(seq) {
		return
	}
	m.activeSeq = seq
	// Re-stamp the sub-models' captured account. The order panel's fee cache
	// is keyed by {account, symbol}, so the switch just reads the new
	// account's entry (fetched on the next order-surface trigger when
	// missing); funding clears its per-account caches (transfer history,
	// withdrawable amounts) so a reopen never shows the previous account's
	// data.
	m.order.accountSeq = seq
	m.ladder.accountSeq = seq
	m.funding.setAccountSeq(seq)
	// The open-orders scope is {account, symbol}: re-track under the new account
	// (the account-wide myOrder feed is already live for it; the snapshot is
	// fetched on demand) and rebuild the table for the new account.
	m.applyOrderTracking()
	m.refreshOrders()
	m.setToast(i18n.T("sub-account: %d", seq), false)
}

// hasAccount reports whether seq is one of the session's subscribed sub-accounts
// (so the switcher never activates an account whose channels aren't subscribed).
func (m model) hasAccount(seq int) bool {
	for _, s := range m.accountSeqs {
		if s == seq {
			return true
		}
	}
	return false
}

// multiAccount reports whether the session holds more than one sub-account, so
// the '@' switcher (and its footer hint) is meaningful.
func (m model) multiAccount() bool { return len(m.accountSeqs) > 1 }

// trackScopes builds the session tracking scopes for the given symbols under
// the active account (the shape SetTrackedOrders takes).
func (m model) trackScopes(symbols ...string) []stream.OrderScope {
	scopes := make([]stream.OrderScope, 0, len(symbols))
	for _, sym := range symbols {
		scopes = append(scopes, stream.OrderScope{AccountSeq: m.accountSeq(), Symbol: sym})
	}
	return scopes
}

// marketSettled reports whether the active symbol's orderbook/trade subscriptions
// have settled on it (not mid-switch): the selection moves immediately but the
// re-subscribe is debounced, so between the two the panes must show loading
// rather than the previous symbol's retained book/trades.
func (m model) marketSettled() bool { return m.subscribed == m.symbol() }

// tickerStatus / orderbookStatus / tradeStatus are the single classification the
// panes and the place gate read, so loading-vs-empty-vs-present is computed in one
// place. Ticker is subscribed for every symbol up front, so it never rides the
// market-switch gate; orderbook/trades are re-pointed per active symbol, so while
// a switch is mid-flight (selection moved, re-subscribe debounced) they read
// NotReady rather than surface the store's status for a not-yet-resubscribed pair.
func (m model) tickerStatus(sym string) state.DataStatus { return m.store.TickerStatus(sym) }

func (m model) orderbookStatus(sym string) state.DataStatus {
	if !m.marketSettled() {
		return state.StatusNotReady
	}
	return m.store.OrderbookStatus(sym)
}

func (m model) tradeStatus(sym string) state.DataStatus {
	if !m.marketSettled() {
		return state.StatusNotReady
	}
	return m.store.TradeStatus(sym)
}

// moneyActionInFlight reports whether ANY money action — an order place/cancel
// or a funding request — is currently on the wire. It is the TUI-wide
// single-flight gate: order actions and funding actions block each other, so
// at most one money mover is ever unresolved at a time.
func (m model) moneyActionInFlight() bool { return m.orderInFlight || m.funding.busy() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ready = true
		m.layout()
		m.funding.setSize(m.w, m.bodyHeight())
		// Size the chart to its overlay area now, so scroll/zoom math (handled on
		// this stored model) uses the same capacity the View renders at.
		cw, ch := m.chartDims()
		m.chart.SetSize(cw, ch)
		// The inline pane is on by default: seed its feed once this resize makes it
		// visible (and re-point it if the market changed while it was hidden).
		return m.ensureChartLoaded()

	case tea.ColorProfileMsg:
		// The terminal's color support, reported once at startup. Re-resolve the
		// palette so depth bars and up/down text match what this terminal can show.
		m.profile = msg.Profile
		m.pal = uikit.PaletteFor(m.colorScheme, m.profile)
		return m, nil

	case clockTickMsg:
		// Besides the re-render (ages, toast expiry; state is in the store),
		// the tick retries the active {account, symbol} metadata fetch while
		// an order surface is open — fetchMeta no-ops when everything is
		// fetched or in flight, so this only fires after a failed fetch (its
		// guard clears) and keeps the fee-policy place gate self-clearing.
		if m.mode == modeOrder || m.mode == modeLadder || m.cmdbar.active {
			if fetch := m.order.fetchMeta(m.symbol()); fetch != nil {
				return m, tea.Batch(clockTick(), fetch)
			}
		}
		return m, clockTick()

	case candlesLoadedMsg:
		return m.applyCandles(msg)

	case spinner.TickMsg:
		// Advance a spinner only while its surface is actually loading, so the
		// animation loop dies otherwise (each spinner re-arms on its own accepted
		// tick and drops the other's by ID): the chart's during a history
		// backfill or the initial/interval seed, the funding screen's while a
		// request/fetch is in flight there.
		var cmds []tea.Cmd
		if m.chartLoadingOlder || (m.mode == modeChart && m.feed.Empty() && m.chartErr == "") {
			var cmd tea.Cmd
			m.chartSpin, cmd = m.chartSpin.Update(msg)
			cmds = append(cmds, cmd)
		}
		if m.mode == modeFunding && m.funding.spinning() {
			var cmd tea.Cmd
			m.funding.spin, cmd = m.funding.spin.Update(msg)
			cmds = append(cmds, cmd)
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)

	case chartResyncMsg:
		// Re-fetch on the cadence while the chart is live (overlay open OR inline
		// pane shown); let the loop die once both close or the symbol/interval
		// changes (a newer gen supersedes it).
		if msg.gen != m.chartGen || !m.chartActive() {
			return m, nil
		}
		return m, tea.Batch(
			m.fetchCandlesCmd(m.chartGen, m.chartSym, m.chartIv),
			chartResyncCmd(m.chartGen, m.chartIv),
		)

	case streamEventMsg:
		m.store.Apply(msg.ev)
		m.logNotice(msg.ev)
		if isOrderEvent(msg.ev) {
			m.refreshOrders()
			m.noticePlacedTerminal()
			m.maybeSnapshotCancelAll()
		}
		m = m.reconcileLadderCursor()
		cmd := m.foldChartTrades()
		return m, cmd

	case streamBatchMsg:
		// The pump coalesces stream events and delivers them in batches (see
		// tui.Run), so the program runs Update+View at most a few dozen times a
		// second regardless of how fast the stream ticks — without this, a
		// session subscribed to hundreds of pairs floods the event loop with
		// per-event redraws and starves keyboard/mouse input. Rebuild the orders
		// table at most once per batch, and only if an order actually changed.
		orders := false
		for _, ev := range msg.evs {
			m.store.Apply(ev)
			m.logNotice(ev)
			if isOrderEvent(ev) {
				orders = true
			}
		}
		if orders {
			m.refreshOrders()
			m.noticePlacedTerminal()
			m.maybeSnapshotCancelAll() // a cancel-all dialog waiting on all-pairs load may now be ready
		}
		m = m.reconcileLadderCursor()
		cmd := m.foldChartTrades()
		return m, cmd

	case eventsClosedMsg:
		// The session ended underneath us (fatal, or StopSession during quit).
		// Leave; the cli layer reports the session error after the screen is
		// restored.
		return m, tea.Quit

	case placeDoneMsg:
		m.orderInFlight = false
		m.closeQuitConfirmIfIdle()
		switch msg.origin {
		case placeFromPanel:
			m.order = m.order.placeDone(msg.err)
		case placeFromLadder:
			m.ladder = m.ladder.placeDone(msg.err)
		}
		if msg.err != nil {
			// The place failed — definitively rejected, or its outcome is
			// unresolved. Either way the order is not known to be in flight,
			// so its local hold must not stand (see state.AddLocalHold).
			m.store.ReleaseLocalHold(msg.accountSeq, msg.clientOrderID)
			// The panel and the ladder show their own rejection inline (fix
			// and re-arm); the toast covers a bar-placed order and a user who
			// already left the arming surface.
			inline := (msg.origin == placeFromPanel && m.mode == modeOrder) ||
				(msg.origin == placeFromLadder && m.mode == modeLadder)
			if !inline {
				m.setToast(i18n.T("order rejected: %s", errorText(msg.err)), true)
			}
			return m, nil
		}
		// Accepted: order mode stays open (sticky — the next order often looks
		// like the last), the new order's row flashes in the open-orders panel.
		if id, err := strconv.ParseInt(msg.res.OrderID, 10, 64); err == nil && id != 0 {
			m.flashOrderID, m.flashUntil = id, m.cfg.Now()+flashMs
			m.placedWatch[id] = true
		}
		if msg.res.Warning != "" {
			m.setToast(i18n.T("order accepted — orderId %s · clientOrderId %s — WARNING: %s",
				msg.res.OrderID, msg.res.ClientOrderID, msg.res.Warning), true)
		} else {
			m.setToast(i18n.T("order accepted — orderId %s · clientOrderId %s (watch myOrder for status)",
				msg.res.OrderID, msg.res.ClientOrderID), false)
		}
		// The order may already be terminal in the store: an immediately-rejected
		// post-only/ioc/fok order's myOrder frame can beat this REST ack, and no
		// further order event would arrive to trigger the no-fill notice. Run it
		// now — it supersedes the accepted toast above when the order is a
		// zero-fill death, and is a no-op while the order is still live.
		m.noticePlacedTerminal()
		return m, nil

	case orderBandsMsg:
		m.order = m.order.applyBands(msg)
		return m, nil

	case orderBoundsMsg:
		m.order = m.order.applyBounds(msg)
		return m, nil

	case orderFeesMsg:
		m.order = m.order.applyFees(msg)
		return m, nil

	case cancelDoneMsg:
		m.orderInFlight = false
		m.closeQuitConfirmIfIdle()
		delete(m.cancelsInFlight, msg.orderID)
		m.ladder = m.ladder.cancelDone() // a ladder-armed cancel returns to browsing
		m.refreshOrders()                // clear the canceling row style now
		switch {
		case msg.err != nil:
			m.setToast(i18n.T("cancel of order %d failed: %s", msg.orderID, errorText(msg.err)), true)
		case msg.warning != "":
			m.setToast(i18n.T("cancel of order %d accepted — WARNING: %s", msg.orderID, msg.warning), true)
		default:
			m.setToast(i18n.T("cancel of order %d accepted — confirm via the open-orders panel", msg.orderID), false)
		}
		return m, nil

	case cancelAllStepMsg:
		if !m.cancelAll.running {
			return m, nil // stale/duplicate: only one step is ever in flight (defensive)
		}
		delete(m.cancelsInFlight, msg.orderID)
		if msg.err != nil {
			m.cancelAll.failed++
		} else {
			m.cancelAll.ok++
		}
		m.cancelAll.idx++
		m.refreshOrders()
		// Keep going unless the snapshot is exhausted or the user asked to stop.
		if m.cancelAll.idx < len(m.cancelAll.snapshot) && !m.cancelAll.aborted {
			return m, m.dispatchNextCancelAll()
		}
		// Batch finished: report and close (the dialog state resets).
		ok, failed, aborted := m.cancelAll.ok, m.cancelAll.failed, m.cancelAll.aborted
		m.orderInFlight = false
		m.closeCancelAll()
		m.refreshOrders()
		switch {
		case aborted:
			m.setToast(i18n.T("cancel all stopped — %d canceled, %d failed", ok, failed), failed > 0)
		case failed > 0:
			m.setToast(i18n.T("cancel all: %d canceled, %d failed", ok, failed), true)
		default:
			m.setToast(i18n.T("cancel all: %d order(s) canceled", ok), false)
		}
		return m, nil

	case fundingCopiedMsg:
		// A funding copy-to-clipboard (OSC 52) completed: ack it as a transient
		// footer toast — a copy must not raise a sticky banner.
		m.setToast(i18n.T("%s copied to the clipboard (needs terminal OSC 52 support)", msg.what), false)
		return m, nil

	case fundingMsg:
		// Every funding-owned message (fetch results, action outcomes, the
		// selection-settle debounce) routes to the sub-model in one case.
		var cmd tea.Cmd
		m.funding, cmd = m.funding.apply(msg)
		m.closeQuitConfirmIfIdle() // a funding action landing may release the soft-quit dialog
		return m, cmd

	case resubscribeMsg:
		// Debounce settled: re-point the active symbol's market data, unless a
		// newer selection superseded this tick. One debounce serves both moves:
		// a symbol switch re-points orderbook+trades (carrying the new pair's
		// grouping level), a grouping change re-points the orderbook alone.
		if msg.gen != m.subGen {
			return m, nil
		}
		next := m.symbol()
		switch {
		case m.cfg.SetActiveMarket != nil && next != m.subscribed:
			m.cfg.SetActiveMarket(m.subscribed, next, m.bookGrp[next])
			m.subscribed = next
			m.subscribedGrp = m.bookGrp[next]
			// The retained book/trades for the new pair are from its prior
			// subscription; hide them until this resubscribe's snapshot lands.
			m.store.MarkMarketStale(next)
			if !m.ordersAllPairs {
				// Per-pair view: re-point the open-order snapshot to the new
				// active pair (all-pairs already tracks everything).
				m.applyOrderTracking()
			}
		case m.cfg.SetOrderbookLevel != nil && next == m.subscribed && m.bookGrp[next] != m.subscribedGrp:
			// next == m.subscribed guards the seams being wired independently:
			// without SetActiveMarket a selection can move while only the
			// initial symbol is subscribed — a level re-point must never
			// target (and stale) a symbol that was never subscribed.
			m.cfg.SetOrderbookLevel(next, m.bookGrp[next])
			m.subscribedGrp = m.bookGrp[next]
			// Only the book changes granularity; the trade feed is untouched
			// and must not flip to "loading".
			m.store.MarkBookStale(next)
		}
		// The inline chart follows the active market on the same settle, so
		// scrubbing the list doesn't refetch candles per row. Only while it's
		// actually shown — a pane hidden on a short terminal stays quiet.
		if m.inlineChartVisible() && m.chartSym != m.symbol() {
			return m.loadChart(m.symbol(), m.chartIv)
		}
		return m, nil

	case tea.MouseClickMsg:
		// A left click on a key-hint cap behaves exactly like pressing that key:
		// stripKeyAt finds the cap under the pointer (footer, chart, or overlay
		// hint) and we dispatch its synthetic press through the normal key path.
		// It matches only on a strip row, so candle selection / sidebar / order-row
		// clicks below fall through untouched.
		if msg.Button == tea.MouseLeft {
			if send, ok := m.stripKeyAt(msg.X, msg.Y); ok {
				return m.handleKey(send)
			}
		}
		// A left click on the header acct chip opens the switcher, or toasts why when
		// it can't (openAccountSwitch decides). Handled before the mode blocks so it
		// works from any docked layout; the centered overlays keep their own
		// outside-click-to-dismiss handling.
		if msg.Button == tea.MouseLeft && m.acctChipAt(msg.X, msg.Y) &&
			!m.searching && !m.cmdbar.active &&
			(m.mode == modeNormal || m.mode == modeOrder || m.mode == modeLadder || m.mode == modeFunding) {
			m.openAccountSwitch()
			return m, nil
		}
		// In the chart overlay a left click inside the box selects the candle under
		// the pointer; a click outside the box dismisses the overlay (so a mis-click
		// that opened it is reversible with the mouse alone). Wheel scrolling is
		// handled in the MouseWheelMsg case.
		if m.mode == modeChart {
			if msg.Button == tea.MouseLeft {
				if m.inChartOverlay(msg.X, msg.Y) {
					m.selectChartAt(msg.X, msg.Y)
				} else {
					m.mode = modeNormal // the re-sync loop sees the mode change and stops
				}
			}
			return m, nil
		}
		// The help and notices overlays close on a left click outside their box (a
		// click inside or on the frame keeps them), the same reversible mouse gesture
		// as the chart.
		if m.mode == modeHelp || m.mode == modeNotices {
			if msg.Button == tea.MouseLeft && !m.inActiveOverlay(msg.X, msg.Y) {
				m.mode = modeNormal
			}
			return m, nil
		}
		// The funding screen has no overlays: a left-press on its seam starts a
		// resize drag, like the main layout's dividers; otherwise the click maps
		// to a currency row, a request-pane field (chips, ‹ › arrows, the copy
		// targets, the button — including the in-pane confirm's enter/esc), or a
		// history row. The strip caps were already handled by stripKeyAt above.
		if m.mode == modeFunding {
			if msg.Button == tea.MouseLeft {
				if d := m.funding.hitDivider(msg.X, msg.Y, m.bodyTop()); d != dragNone {
					m.drag = d
					return m, nil
				}
				var cmd tea.Cmd
				m.funding, cmd = m.funding.click(msg.X, msg.Y, m.bodyTop(), m.orderInFlight)
				return m, cmd
			}
			return m, nil
		}
		// Order mode is a docked layout, not an overlay: a left click on an
		// orderbook level writes that price into the draft (the HTS gesture), a
		// click inside the panel moves the field cursor / flips the toggle /
		// applies the preset / presses the button under it, and clicks anywhere
		// else are inert (esc leaves the mode deliberately, never by mis-click).
		if m.mode == modeOrder {
			if msg.Button == tea.MouseLeft {
				// The open-orders slice sits beneath the docked panel; its title tabs
				// stay clickable there — the mouse analogue of 'o', switching open↔closed
				// without leaving order entry. Form view only: an armed review /
				// in-flight placement owns every gesture, exactly as the 'o' key gate.
				if to, ok := m.ordersTitleTab(msg.X, msg.Y); ok && m.order.view == orderForm {
					m.setFocus(focusOrders)
					m.showClosedOrders(to)
					return m, nil
				}
				// A click on a hinted key-cap (`[`, `%`, `u`, `j`, `b`, …) presses
				// that key, exactly like the footer caps — handled before the panel
				// span/book gestures below.
				if send, ok := m.orderPanelKeyAt(msg.X, msg.Y); ok {
					return m.handleKey(send)
				}
				if p, ok := m.bookPriceAt(msg.X, msg.Y); ok {
					if m.order.view == orderForm && m.order.draft.usesPrice() {
						m.order.setPrice(p)
						m.order.formErr = ""
					}
					return m, nil
				}
				if row, col, ok := m.orderPanelPos(msg.X, msg.Y); ok {
					var act orderAction
					_, _, rightW := m.orderColumnGeom()
					m.order, act = m.order.panelClick(row, col, rightW-2, m.panelGate())
					if act == orderActPlace {
						return m.dispatchPlace()
					}
				}
			}
			return m, nil
		}
		// The trade ladder is a docked layout too: a left click on a price row
		// moves the ladder cursor (while browsing — the armed strip is
		// keyboard/footer-confirmed), and clicks anywhere else are inert.
		if m.mode == modeLadder {
			if msg.Button == tea.MouseLeft && m.ladder.view == ladderBrowse {
				if to, ok := m.ordersTitleTab(msg.X, msg.Y); ok {
					// The right column keeps its normal layout in ladder mode, so
					// the orders title tabs stay clickable — the mouse analogue of
					// 'o', switching open↔closed without leaving the ladder.
					m.setFocus(focusOrders)
					m.showClosedOrders(to)
				} else if kind, dir, ok := m.ladderTitleChipAt(msg.X, msg.Y); ok {
					// A click focuses the chip; on an arrow (dir != 0) it also
					// steps it — the same focus+adjust a footer arrow-cap does.
					m.ladder.titleFocus = kind
					if dir != 0 {
						m.ladder = m.ladder.stepChip(dir)
					}
					m.ladder.stripErr = ""
				} else if p, ok := m.ladderRowAt(msg.X, msg.Y); ok {
					m.ladder.cursorPrice = p
					m.ladder.stripErr = ""
				}
			}
			return m, nil
		}
		// Modal dialogs (confirm / cancel-all / account switcher). Their hint caps are
		// already handled by stripKeyAt above; here a left click on an interactive body
		// region runs its action (flip a toggle, pick a cancel scope, switch sub-account),
		// and a click outside the box dismisses it — the same reversible gesture as the
		// other overlays. A *running* cancel-all batch is the one exception: an outside
		// click must not silently abort an in-flight batch (only its esc/stop hint does).
		if m.mode == modeConfirm || m.mode == modeCancelAll || m.mode == modeQuitConfirm || m.mode == modeAccountSwitch {
			if msg.Button == tea.MouseLeft {
				if do, ok := m.overlayBodyAt(msg.X, msg.Y); ok {
					cmd := do(&m) // mutate m, then return the (possibly nil) follow-up cmd
					return m, cmd
				}
				if !m.inModalOverlay(msg.X, msg.Y) {
					return m.dismissModal()
				}
			}
			return m, nil
		}
		// Left-press on a panel seam starts a resize drag; a click on a sidebar
		// row selects that symbol; a click on the inline chart strip opens the full
		// overlay; any other press focuses the pane under it. Only in the normal
		// view — an overlay owns the body, and quick search owns the keyboard+screen
		// until it's dismissed (enter/esc).
		if m.mode == modeNormal && !m.searching && !m.cmdbar.active && msg.Button == tea.MouseLeft {
			if d := m.hitDivider(msg.X, msg.Y); d != dragNone {
				m.drag = d
			} else if idx, ok := m.sidebarRowAt(msg.X, msg.Y); ok {
				m.setFocus(focusMarket)
				return m, m.setActive(idx) // click a sidebar row to switch the active symbol
			} else if to, ok := m.ordersTitleTab(msg.X, msg.Y); ok {
				m.setFocus(focusOrders)
				m.showClosedOrders(to) // click a title tab to select it
			} else if r, ok := m.ordersRowAt(msg.X, msg.Y); ok {
				m.setFocus(focusOrders)
				m.orderCursor = r // click an order row to select it (then x cancels it)
				m.ensureOrderVisible()
			} else if m.inlineChartAt(msg.X, msg.Y) {
				return m.openChart() // click the inline strip to open the full chart
			} else if f, ok := m.paneAt(msg.X, msg.Y); ok {
				m.setFocus(f) // a click off any focusable pane leaves focus untouched
			}
		}
		return m, nil

	case tea.MouseMotionMsg:
		if m.drag != dragNone && (m.mode == modeNormal || m.mode == modeFunding) {
			m.applyDrag(msg.X, msg.Y)
		}
		return m, nil

	case tea.MouseWheelMsg:
		// The wheel scrolls the pane under the pointer; it never changes the
		// selected market (sidebar wheel moves only the window). Suppressed while
		// quick search owns the screen.
		if m.mode == modeChart {
			m.chart, _ = m.chart.Update(msg) // wheel scrolls the chart back/forward in time
			return m, m.maybeBackfill()      // scrolling back may need older history
		} else if m.mode == modeNormal && !m.searching && !m.cmdbar.active {
			m.handleWheel(msg)
		} else if m.mode == modeFunding && m.funding.view == fundingBrowse {
			step := wheelStep
			if msg.Button == tea.MouseWheelUp {
				step = -wheelStep
			}
			m.funding.wheel(msg.X, msg.Y, step, m.bodyTop())
		} else if m.mode == modeOrder && m.order.view == orderForm {
			// A wheel notch over the orderbook walks the ladder cursor one level
			// (precision beats speed while picking a price).
			if _, ok := m.bookPriceAt(msg.X, msg.Y); ok {
				delta := 1
				if msg.Button == tea.MouseWheelUp {
					delta = -1
				}
				m.order = m.order.moveLadder(delta, m.orderLadderRows())
			}
		} else if m.mode == modeAccountSwitch {
			// A wheel notch over the popup walks the highlight one row (the window
			// follows via ensureAcctVisible), so a long account list scrolls.
			n := len(m.accountSeqs)
			delta := 1
			if msg.Button == tea.MouseWheelUp {
				delta = -1
			}
			m.acctCursor = clamp(m.acctCursor+delta, 0, n-1)
			m.ensureAcctVisible()
		} else if m.mode == modeLadder && m.ladder.view == ladderBrowse {
			// A wheel notch over the trade ladder walks its cursor one row.
			left, w := m.ladderGeom()
			top, bodyH := m.bodyTop(), m.bodyHeight()
			if msg.X >= left && msg.X < left+w && msg.Y >= top && msg.Y < top+bodyH {
				delta := 1
				if msg.Button == tea.MouseWheelUp {
					delta = -1
				}
				m.ladder = m.ladder.moveCursor(delta, m.ladderPrices())
			}
		}
		return m, nil

	case tea.MouseReleaseMsg:
		m.drag = dragNone
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	// Note: a SIGINT delivered as an OS signal does NOT arrive here — bubbletea
	// returns tea.ErrInterrupted from Run before dispatching it to Update. That
	// clean-stop case is handled in Run. On a real TTY, Ctrl-C arrives as a
	// "ctrl+c" KeyPressMsg instead and is handled in handleKey.
	return m, nil
}

// dismissModal backs out of the open modal dialog on a click outside its box.
// It reuses each dialog's esc handler (single source for close/cancel semantics),
// except a running cancel-all batch: esc there means "abort", which an accidental
// outside click must not trigger, so it is left to the explicit esc/stop hint.
func (m model) dismissModal() (tea.Model, tea.Cmd) {
	if m.mode == modeCancelAll && m.cancelAll.running {
		return m, nil
	}
	return m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
}

func (m model) quit() (tea.Model, tea.Cmd) {
	if m.cfg.StopSession != nil {
		m.cfg.StopSession()
	}
	return m, tea.Quit
}

// closeQuitConfirmIfIdle dismisses the soft-quit dialog once the in-flight
// money action it was asking about has finished — the dialog's "its result
// will not be shown" would be stale, and the result toast underneath is the
// better answer to "quit now?". Called by the action-done message handlers.
func (m *model) closeQuitConfirmIfIdle() {
	if m.mode == modeQuitConfirm && !m.moneyActionInFlight() {
		m.mode = modeNormal
	}
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m.quit()
	}
	// Quick search and the command bar run inside the normal view (they are
	// not full overlays): while active they own the keyboard.
	if m.searching {
		return m.handleSearchKey(msg)
	}
	if m.cmdBarVisible() {
		return m.handleCmdBarKey(msg)
	}
	switch m.mode {
	case modeOrder:
		return m.handleOrderKey(msg)
	case modeLadder:
		return m.handleLadderKey(msg)
	case modeConfirm:
		return m.handleConfirmKey(msg)
	case modeCancelAll:
		return m.handleCancelAllKey(msg)
	case modeHelp:
		// esc/enter dismiss; ? is a same-key toggle (the key that opened it
		// closes it). n must NOT close help — it means "no" elsewhere.
		switch msg.String() {
		case "esc", "enter", "?":
			m.mode = modeNormal
		}
		return m, nil
	case modeNotices:
		switch msg.String() {
		case "esc", "enter", "n":
			m.mode = modeNormal
		}
		return m, nil
	case modeQuitConfirm:
		switch msg.String() {
		case "enter":
			return m.quit()
		case "esc":
			m.mode = modeNormal
		}
		return m, nil
	case modeAccountSwitch:
		return m.handleAccountSwitchKey(msg)
	case modeChart:
		return m.handleChartKey(msg)
	case modeFunding:
		// The funding screen owns the keyboard.
		nf, cmd, closed := m.funding.handleKey(msg, m.orderInFlight)
		m.funding = nf
		if closed {
			m.mode = modeNormal
		}
		return m, cmd
	}
	return m.handleNormalKey(msg)
}

// openFunding enters the funding screen, optionally preselecting a currency
// (the balances-pane jump). It is the single entry point for modeFunding.
func (m model) openFunding(currency string) (tea.Model, tea.Cmd) {
	if m.cfg.Funding == nil {
		m.setToast(i18n.T("deposits/withdrawals need a private session (run without --public)"), true)
		return m, nil
	}
	m.mode = modeFunding
	m.funding.setSize(m.w, m.bodyHeight())
	var cmd tea.Cmd
	m.funding, cmd = m.funding.open(currency)
	return m, cmd
}

func (m model) handleNormalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch s := msg.String(); s {
	case "q":
		// q is the soft quit: instant when idle, but with a money action still
		// in flight it asks first (the result has nowhere to land after exit).
		// ctrl+c stays the unconditional quit.
		if m.moneyActionInFlight() {
			m.mode = modeQuitConfirm
			return m, nil
		}
		return m.quit()
	case "tab":
		m.cycleFocus(1)
		return m, nil
	case "shift+tab":
		m.cycleFocus(-1)
		return m, nil
	case "up", "down", "pgup", "pgdown", "home", "end":
		return m.handleNavKey(msg)
	case "b", "s":
		if m.cfg.Trader == nil {
			m.setToast(i18n.T("order entry is disabled (public mode — run without --public to trade)"), true)
			return m, nil
		}
		if m.moneyActionInFlight() {
			m.setToast(i18n.T("another money action is already in flight — wait for it to finish"), true)
			return m, nil
		}
		side := "buy"
		if s == "s" {
			side = "sell"
		}
		m.mode = modeOrder
		var cmd tea.Cmd
		m.order, cmd = m.order.open(m.symbol(), side)
		return m, cmd
	case "t":
		// The trade ladder is an order-entry surface, so it shares the order
		// panel's gates (and its per-symbol tick/fee caches, warmed here).
		if m.cfg.Trader == nil {
			m.setToast(i18n.T("order entry is disabled (public mode — run without --public to trade)"), true)
			return m, nil
		}
		if m.moneyActionInFlight() {
			m.setToast(i18n.T("another money action is already in flight — wait for it to finish"), true)
			return m, nil
		}
		m.mode = modeLadder
		m.ladder = m.ladder.open(m.symbol())
		return m, m.order.fetchMeta(m.symbol())
	case "x":
		if m.cfg.Trader == nil {
			return m, nil
		}
		// Cancel acts on the open-orders selection, so it is only live when that
		// pane has focus — otherwise the footer doesn't even advertise it.
		if m.focus != focusOrders {
			m.setToast(i18n.T("focus the open orders pane (tab) to cancel an order"), true)
			return m, nil
		}
		if m.ordersClosed {
			m.setToast(i18n.T("closed orders are read-only — press o for the open tab to cancel"), true)
			return m, nil
		}
		if m.moneyActionInFlight() {
			m.setToast(i18n.T("another money action is already in flight — wait for it to finish"), true)
			return m, nil
		}
		if id, ok := m.selectedOrder(); ok {
			// Capture the symbol now, while the order is known to exist: by
			// confirm time a fill/close event may have removed it from the
			// store, and the cancel needs its symbol.
			o, known := m.store.Order(id)
			if !known {
				return m, nil
			}
			m.confirmOrderID = id
			m.confirmSymbol = o.Symbol
			m.confirmSeq = o.AccountSeq
			m.mode = modeConfirm
		}
		return m, nil
	case "X":
		// Cancel-all opens a dialog (scope: active pair / all pairs). Unlike x,
		// it acts on a snapshot of the open-orders set rather than the table
		// selection, so it needs no pane focus and is available from any pane.
		if m.cfg.Trader == nil {
			return m, nil
		}
		if m.moneyActionInFlight() {
			m.setToast(i18n.T("another money action is already in flight — wait for it to finish"), true)
			return m, nil
		}
		m.openCancelAll()
		return m, nil
	case "a":
		// Toggle the orders scope (active pair ↔ all pairs), for both tabs. Only
		// meaningful with the orders pane focused — the footer advertises it
		// only there.
		if m.focus != focusOrders {
			return m, nil
		}
		m.ordersAllPairs = !m.ordersAllPairs
		m.applyOrderTracking()
		m.refreshOrders()
		if m.ordersAllPairs {
			m.setToast(i18n.T("orders: all pairs"), false)
		} else {
			m.setToast(i18n.T("orders: active pair only"), false)
		}
		return m, nil
	case "o":
		// The orders pane exists only in a private session, so there is nothing
		// to toggle in public mode. Gated on Private, not Trader — a read-only
		// private session (no order entry) still has the pane.
		if !m.cfg.Private {
			return m, nil
		}
		// Toggle the orders pane between its open and closed tabs, from any
		// pane — checking a recent close must not require a focus round-trip.
		// Closed shows the terminal orders observed this session, most
		// recently closed first, read-only.
		m.showClosedOrders(!m.ordersClosed)
		return m, nil
	case "/":
		// Quick search is only meaningful on a list pane: it filters the markets
		// or balances list to matches as you type. The selection/scroll only move
		// on enter, so starting a search needs no saved state.
		if m.focus == focusMarket || (m.cfg.Private && m.focus == focusBalances) {
			m.searching = true
			m.searchPane = m.focus
			m.searchQuery = ""
			m.searchCursor = 0
			m.searchScroll = 0
		}
		return m, nil
	case ":":
		if m.cfg.Trader == nil {
			m.setToast(i18n.T("order entry is disabled (public mode — run without --public to trade)"), true)
			return m, nil
		}
		m.cmdbar = m.cmdbar.open()
		return m, m.order.fetchMeta(m.symbol()) // the bar resolves on the same tick/fee caches
	case "g":
		if m.cfg.Candles == nil {
			m.setToast(i18n.T("candle chart unavailable"), true)
			return m, nil
		}
		return m.openChart()
	case "f":
		return m.openFunding("")
	case "enter":
		// On the focused balances pane, enter jumps into the funding screen at
		// the currency the window is showing (the list has no selection, so the
		// window top is the jump target).
		if m.cfg.Private && m.focus == focusBalances && m.cfg.Funding != nil {
			bals := m.store.BalancesFor(m.accountSeq())
			if len(bals) > 0 {
				i := clamp(m.balScroll, 0, len(bals)-1)
				return m.openFunding(bals[i].Currency)
			}
			return m.openFunding("")
		}
		return m, nil
	case "G":
		return m.toggleInlineChart()
	case "n":
		m.mode = modeNotices
		return m, nil
	case "=":
		m.resetLayout()
		m.layout()
		m.setToast(i18n.T("panel layout reset"), false)
		return m, nil
	case "+", "-":
		return m, m.cycleBookLevel(msg.String() == "+")
	case "C":
		m.toggleColorScheme()
		return m, nil
	case "?":
		m.mode = modeHelp
		return m, nil
	case "@":
		m.openAccountSwitch()
		return m, nil
	default:
		// The digit keys are deliberately unbound (the sub-account switcher is the
		// '@' popup, navigated with the arrows).
		return m, nil
	}
}

// openAccountSwitch opens the sub-account switcher, or toasts why when it can't,
// so the '@' key and the header chip click share one gate. It opens only when the
// switch is meaningful and safe: a private, multi-account session, in the normal
// view, with no money action in flight. The money-in-flight guard comes first —
// switching the DISPLAYED account out from under a place/cancel/withdrawal still
// on the wire breaks "what you see is what is happening" — and via funding.busy()
// it still holds after an esc out of a funding request.
func (m *model) openAccountSwitch() {
	if !m.cfg.Private {
		return // no account channels, and no chip drawn to click
	}
	if !m.multiAccount() {
		m.setToast(i18n.T("this session holds one sub-account — pass --account-seq (a list, or omit it) for more"), false)
		return
	}
	if m.moneyActionInFlight() {
		m.setToast(i18n.T("a money action is in flight — wait for it to finish before switching sub-account"), true)
		return
	}
	if m.mode != modeNormal {
		m.setToast(i18n.T("leave this screen (esc) to switch sub-account"), false)
		return
	}
	m.mode = modeAccountSwitch
	m.acctCursor = m.activeAccountIndex()
	m.ensureAcctVisible()
}

// ensureAcctVisible clamps acctScroll so the fixed-height window keeps acctCursor
// on screen: it scrolls just far enough when the cursor steps above the top or
// below the bottom of the accountSwitchMaxRows-tall window, and never past the
// end of the list. The single source of the popup's scroll math, shared by the
// key handler and the row-click handler so the render and hit-test can't drift.
func (m *model) ensureAcctVisible() {
	n := len(m.accountSeqs)
	vis := accountSwitchMaxRows
	if vis > n {
		vis = n
	}
	maxScroll := n - vis
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.acctScroll > m.acctCursor {
		m.acctScroll = m.acctCursor // cursor stepped above the window
	}
	if m.acctCursor >= m.acctScroll+vis {
		m.acctScroll = m.acctCursor - vis + 1 // cursor stepped below the window
	}
	m.acctScroll = clamp(m.acctScroll, 0, maxScroll)
}

// activeAccountIndex is the index of the active account within accountSeqs (0 if
// not found, defensively) — where the '@' popup opens its cursor.
func (m model) activeAccountIndex() int {
	for i, s := range m.accountSeqs {
		if s == m.accountSeq() {
			return i
		}
	}
	return 0
}

// handleAccountSwitchKey owns the keyboard while the '@' sub-account popup is
// open: the arrows move the highlight, enter switches to the highlighted account
// and closes, esc closes without switching. A mouse click on a row moves the
// highlight to it (a preview, not a commit — see selectAccountRow); the switch
// happens only on enter (or its clickable hint cap).
func (m model) handleAccountSwitchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	n := len(m.accountSeqs)
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
	case "enter":
		m.mode = modeNormal
		m.switchAccount(m.accountSeqs[clamp(m.acctCursor, 0, n-1)])
	case "up":
		m.acctCursor = clamp(m.acctCursor-1, 0, n-1)
	case "down":
		m.acctCursor = clamp(m.acctCursor+1, 0, n-1)
	case "pgup":
		m.acctCursor = clamp(m.acctCursor-accountSwitchMaxRows, 0, n-1)
	case "pgdown":
		m.acctCursor = clamp(m.acctCursor+accountSwitchMaxRows, 0, n-1)
	case "home":
		m.acctCursor = 0
	case "end":
		m.acctCursor = n - 1
	}
	m.ensureAcctVisible()
	return m, nil
}

// clickAccountRow handles a left click on an account row. A click on a row that
// is not the current selection moves the highlight to it (a preview, not a
// commit) — the active account (the "›" chevron) and the panels are untouched, so
// a mis-click costs nothing and esc still cancels cleanly. A click on the
// already-selected row confirms it, exactly as enter does: it switches and closes.
// (The popup opens with the active account selected, so a single click on it
// confirms staying put.)
func (m *model) clickAccountRow(seq int) {
	idx := -1
	for i, s := range m.accountSeqs {
		if s == seq {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	if idx == m.acctCursor {
		m.mode = modeNormal
		m.switchAccount(seq)
		return
	}
	m.acctCursor = idx
	m.ensureAcctVisible()
}

// focusable is the focus ring for the current mode: the market selector always,
// plus the open-orders table and the balances list when trading (private).
// Public mode has neither, so its ring is a single pane and tab is a no-op.
func (m model) focusable() []focusPane {
	if m.cfg.Private {
		return []focusPane{focusMarket, focusOrders, focusBalances}
	}
	return []focusPane{focusMarket}
}

// cycleFocus moves focus by delta around the ring.
func (m *model) cycleFocus(delta int) {
	ring := m.focusable()
	idx := 0
	for i, f := range ring {
		if f == m.focus {
			idx = i
			break
		}
	}
	m.setFocus(ring[(idx+delta+len(ring))%len(ring)])
}

// setFocus moves pane focus through one seam, so a focus-move side effect has
// a single home. The orders pane's tab selection deliberately survives focus
// moves: a trader flips to closed, works elsewhere, and finds it as left.
func (m *model) setFocus(f focusPane) {
	m.focus = f
}

// showClosedOrders switches the orders pane's tab, resetting the cursor and
// rebuilding the row projection on an actual change.
func (m *model) showClosedOrders(on bool) {
	if m.ordersClosed == on {
		return
	}
	m.ordersClosed = on
	m.orderCursor, m.orderScroll = 0, 0
	m.refreshOrders()
}

// handleNavKey routes a navigation key to the focused pane: the open-orders
// table or the balances window when one of those is focused, otherwise the
// market selector (up/down move the active symbol; home/end jump to the ends).
func (m model) handleNavKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.focus == focusOrders {
		m.moveOrderCursor(msg)
		return m, nil
	}
	if m.focus == focusBalances {
		// Balances have no selected row — the arrows (and paging) just scroll the
		// window, like the mouse wheel.
		m.scrollBalancesKey(msg)
		return m, nil
	}
	// focusMarket: the arrows move the SELECTION immediately (the detail panels
	// switch at once); the dynamic re-subscribe is debounced inside setActive so
	// scrubbing the list doesn't churn the stream.
	n := len(m.cfg.Symbols)
	switch msg.String() {
	case "down":
		return m, m.setActive(clamp(m.active+1, 0, n-1))
	case "up":
		return m, m.setActive(clamp(m.active-1, 0, n-1))
	case "pgdown":
		return m, m.setActive(clamp(m.active+10, 0, n-1))
	case "pgup":
		return m, m.setActive(clamp(m.active-10, 0, n-1))
	case "home":
		return m, m.setActive(0)
	case "end":
		return m, m.setActive(n - 1)
	}
	return m, nil
}

// handleSearchKey owns the keyboard while quick search is active. The list is
// filtered to matches as the query changes; the arrows move within the filtered
// results (the markets highlight, or the balances window); enter commits the
// pick and drops back to the full list; esc closes without committing. The
// underlying selection/scroll only move on enter, so esc has nothing to restore.
func (m model) handleSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch s := msg.String(); s {
	case "esc":
		m.endSearch()
		return m, nil
	case "enter":
		cmd := m.commitSearch()
		m.endSearch()
		return m, cmd
	case "up":
		m.moveSearch(-1)
		return m, nil
	case "down":
		m.moveSearch(1)
		return m, nil
	case "pgup":
		m.moveSearch(-m.searchPageStep())
		return m, nil
	case "pgdown":
		m.moveSearch(m.searchPageStep())
		return m, nil
	case "backspace":
		// Backspace on an already-empty query cancels the search, the same as esc —
		// the natural "delete past the start to back out" gesture.
		if m.searchQuery == "" {
			m.endSearch()
			return m, nil
		}
		m.searchQuery = m.searchQuery[:len(m.searchQuery)-1]
		m.searchCursor, m.searchScroll = 0, 0 // re-anchor on the new result set
		return m, nil
	default:
		// Append the key's literal text (printable characters only — special keys
		// like the arrows carry empty Text and are ignored).
		if t := msg.Text; t != "" && len(m.searchQuery) < searchMax {
			m.searchQuery += t
			m.searchCursor, m.searchScroll = 0, 0
		}
		return m, nil
	}
}

// endSearch leaves search mode and drops back to the full, unfiltered list.
func (m *model) endSearch() {
	m.searching = false
	m.searchQuery = ""
	m.searchCursor, m.searchScroll = 0, 0
}

// commitSearch applies the current filtered pick: select the highlighted symbol
// (markets) or scroll the full balances list to the first match (balances). A
// no-match query is a no-op.
func (m *model) commitSearch() tea.Cmd {
	if m.searchPane == focusBalances {
		if idxs := m.filteredBalances(); len(idxs) > 0 {
			// Land the full list on the top of what the filtered view was showing.
			m.scrollBalancesTo(idxs[clamp(m.searchScroll, 0, len(idxs)-1)])
		}
		return nil
	}
	idxs := m.filteredSymbols()
	if len(idxs) == 0 {
		return nil
	}
	cmd := m.setActive(idxs[clamp(m.searchCursor, 0, len(idxs)-1)])
	m.ensureActiveVisible()
	return cmd
}

// filteredSymbols returns the indices into cfg.Symbols whose symbol contains the
// search query (all of them when the query is empty), preserving list order.
func (m model) filteredSymbols() []int {
	q := strings.ToLower(strings.TrimSpace(m.searchQuery))
	out := make([]int, 0, len(m.cfg.Symbols))
	for i, s := range m.cfg.Symbols {
		if q == "" || strings.Contains(strings.ToLower(s), q) {
			out = append(out, i)
		}
	}
	return out
}

// filteredBalances returns the indices into the sorted balances whose currency
// contains the search query (all when empty), preserving order.
func (m model) filteredBalances() []int {
	q := strings.ToLower(strings.TrimSpace(m.searchQuery))
	bals := m.store.BalancesFor(m.accountSeq())
	out := make([]int, 0, len(bals))
	for i, b := range bals {
		if q == "" || strings.Contains(strings.ToLower(b.Currency), q) {
			out = append(out, i)
		}
	}
	return out
}

// searchPageStep is one page in the filtered list (the focused pane's visible
// rows), at least one.
func (m model) searchPageStep() int {
	if m.searchPane == focusBalances {
		return m.balancesVisible()
	}
	if v := m.sidebarVisible(); v > 1 {
		return v
	}
	return 1
}

// moveSearch moves within the filtered results: the highlighted markets row
// (keeping it visible) or the balances window.
func (m *model) moveSearch(delta int) {
	if m.searchPane == focusBalances {
		n := len(m.filteredBalances())
		max := n - m.balancesVisible()
		if max < 0 {
			max = 0
		}
		m.searchScroll = clamp(m.searchScroll+delta, 0, max)
		return
	}
	n := len(m.filteredSymbols())
	if n == 0 {
		m.searchCursor, m.searchScroll = 0, 0
		return
	}
	m.searchCursor = clamp(m.searchCursor+delta, 0, n-1)
	vis := m.sidebarVisible()
	if vis < 1 {
		return
	}
	if m.searchCursor < m.searchScroll {
		m.searchScroll = m.searchCursor
	} else if m.searchCursor >= m.searchScroll+vis {
		m.searchScroll = m.searchCursor - vis + 1
	}
}

// setActive selects a symbol: it moves the selection (what the detail panels
// show) immediately and scrolls the sidebar to keep it visible, then returns a
// debounced command to re-point the orderbook/trade subscriptions once the
// selection settles. It is the single place the active symbol moves.
func (m *model) setActive(i int) tea.Cmd {
	if i < 0 || i >= len(m.cfg.Symbols) || i == m.active {
		return nil
	}
	m.active = i
	m.ensureActiveVisible()
	if !m.ordersAllPairs {
		// Per-pair view: refilter the open-orders list to the new active symbol
		// immediately (the snapshot for it is re-pointed on the debounced tick).
		m.refreshOrders()
	}
	if m.cfg.SetActiveMarket == nil {
		return nil
	}
	return m.scheduleResubscribe()
}

// scheduleResubscribe arms the market-data debounce: only the newest
// generation that settles for resubscribeDelayMs fires a resubscribeMsg, so
// scrubbing the sidebar (or tapping +/- through grouping levels) coalesces
// into one re-point.
func (m *model) scheduleResubscribe() tea.Cmd {
	m.subGen++
	gen := m.subGen
	return tea.Tick(time.Duration(resubscribeDelayMs)*time.Millisecond,
		func(time.Time) tea.Msg { return resubscribeMsg{gen: gen} })
}

// cycleBookLevel steps the active symbol's orderbook grouping: finer (+)
// toward the raw book, coarser (-) toward the largest level. The valid
// levels ride the same per-symbol metadata fetch as the tick bands; until it
// lands the keys kick the fetch and say so. A change re-points the orderbook
// subscription on the shared resubscribe debounce.
func (m *model) cycleBookLevel(finer bool) tea.Cmd {
	if m.cfg.SetOrderbookLevel == nil {
		m.setToast(i18n.T("orderbook grouping is unavailable in this session"), true)
		return nil
	}
	sym := m.symbol()
	levels, known := m.order.levels[sym]
	if !known {
		m.setToast(i18n.T("fetching grouping levels for %s — press again in a moment", uikit.FmtSymbol(sym)), false)
		return m.order.fetchMeta(sym)
	}
	if len(levels) == 0 {
		m.setToast(i18n.T("%s offers no orderbook grouping", uikit.FmtSymbol(sym)), true)
		return nil
	}
	seq := groupingSeq(levels)
	cur := 0
	for i, l := range seq {
		if l == m.bookGrp[sym] {
			cur = i
			break
		}
	}
	next := clamp(cur+dir(!finer), 0, len(seq)-1)
	if next == cur {
		if finer {
			m.setToast(i18n.T("orderbook grouping: already the raw book"), false)
		} else {
			m.setToast(i18n.T("orderbook grouping: already the coarsest level"), false)
		}
		return nil
	}
	m.bookGrp[sym] = seq[next]
	// The order panel's book cursor is keyed to a price of the old
	// granularity; detach it so j/k re-seeds from the draft price.
	m.order.cursorPrice = ""
	label := "off — raw book"
	if l := seq[next]; l != "" {
		label = uikit.GroupThousands(l)
	}
	m.setToast(i18n.T("orderbook grouping: %s", label), false)
	return m.scheduleResubscribe()
}

// groupingSeq is the +/- cycling order for a symbol's grouping levels: the
// raw book ("") first, then the valid levels ascending.
func groupingSeq(levels []string) []string {
	sorted := append([]string(nil), levels...)
	sort.Slice(sorted, func(i, j int) bool {
		a, okA := parseDec(sorted[i])
		b, okB := parseDec(sorted[j])
		if !okA || !okB {
			return sorted[i] < sorted[j]
		}
		return a.LessThan(b)
	})
	return append([]string{""}, sorted...)
}

// sidebarVisible is the number of symbol rows the sidebar panel can show.
func (m model) sidebarVisible() int { return m.bodyHeight() - 3 }

// ensureActiveVisible scrolls the sidebar window just enough to keep the
// selected row on screen (after a keyboard move).
func (m *model) ensureActiveVisible() {
	vis := m.sidebarVisible()
	if vis < 1 {
		return
	}
	if m.active < m.scroll {
		m.scroll = m.active
	} else if m.active >= m.scroll+vis {
		m.scroll = m.active - vis + 1
	}
}

// scrollSidebar moves the sidebar window by delta rows (mouse wheel). It never
// changes the selection — only what is visible.
func (m *model) scrollSidebar(delta int) {
	vis := m.sidebarVisible()
	maxStart := len(m.cfg.Symbols) - vis
	if maxStart < 0 {
		maxStart = 0
	}
	m.scroll = clamp(m.scroll+delta, 0, maxStart)
}

// balancesVisible is the number of balance entry rows the balances panel can
// show (its content rows minus the pinned column header).
func (m model) balancesVisible() int {
	_, _, bh := m.rightHeights(m.bodyHeight())
	v := bh - 4 // border (2) + panel title (1) + column header (1)
	if v < 1 {
		v = 1
	}
	return v
}

// balScrollMax is the largest valid balances scroll offset for the current
// balance count and panel height.
func (m model) balScrollMax() int {
	max := len(m.store.BalancesFor(m.accountSeq())) - m.balancesVisible()
	if max < 0 {
		max = 0
	}
	return max
}

// scrollBalances moves the balances window by delta rows, clamped. The balances
// list has no selection — scrolling only changes what is visible.
func (m *model) scrollBalances(delta int) {
	m.balScroll = clamp(m.balScroll+delta, 0, m.balScrollMax())
}

// scrollBalancesTo scrolls the balances window so entry i is visible (placed at
// the top when possible, clamped so the window never runs past the end).
func (m *model) scrollBalancesTo(i int) {
	m.balScroll = clamp(i, 0, m.balScrollMax())
}

// scrollBalancesKey maps a navigation key to a balances scroll move.
func (m *model) scrollBalancesKey(msg tea.KeyPressMsg) {
	switch msg.String() {
	case "down":
		m.scrollBalances(1)
	case "up":
		m.scrollBalances(-1)
	case "pgdown":
		m.scrollBalances(m.balancesVisible())
	case "pgup":
		m.scrollBalances(-m.balancesVisible())
	case "home":
		m.balScroll = 0
	case "end":
		m.balScroll = m.balScrollMax()
	}
}

// handleWheel scrolls the pane under the pointer: the sidebar window (selection
// unchanged), the open-orders table cursor, or the balances window.
func (m *model) handleWheel(msg tea.MouseWheelMsg) {
	top, bodyH := m.bodyTop(), m.bodyHeight()
	if msg.Y < top || msg.Y >= top+bodyH {
		return
	}
	step := wheelStep
	if msg.Button == tea.MouseWheelUp {
		step = -wheelStep
	}
	sideW, _, _, _ := m.colWidths()
	if msg.X < sideW {
		m.scrollSidebar(step)
		return
	}
	if !m.cfg.Private {
		return // no scrollable right-column pane in public mode
	}
	switch f, _ := m.paneAt(msg.X, msg.Y); f {
	case focusOrders:
		if n := len(m.orderIDs); n > 0 {
			m.orderCursor = clamp(m.orderCursor+step, 0, n-1)
			m.ensureOrderVisible()
		}
	case focusBalances:
		m.scrollBalances(step)
	}
}

// handleOrderKey routes keys while order mode is open, and dispatches the
// armed order when the sub-model confirms it.
func (m model) handleOrderKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Orderbook grouping is a market-display concern, so the parent owns the
	// +/- keys (form view only — the armed review's numbers must not have the
	// book regrouped under them mid-confirm).
	if s := msg.String(); (s == "+" || s == "-") && m.order.view == orderForm {
		return m, m.cycleBookLevel(s == "+")
	}
	// The orders-pane tab switch also works while the entry panel is docked — the
	// open-orders slice stays visible beneath it, so a trader can check a recent
	// close without leaving order entry. Form view only: an armed review / an
	// in-flight placement owns every key.
	if msg.String() == "o" && m.order.view == orderForm {
		m.showClosedOrders(!m.ordersClosed)
		return m, nil
	}
	var cmd tea.Cmd
	var act orderAction
	m.order, cmd, act = m.order.handleKey(msg, m.panelGate(), m.orderLadderRows())
	switch act {
	case orderActClose:
		m.mode = modeNormal
	case orderActPlace:
		mm, place := m.dispatchPlace()
		return mm, tea.Batch(cmd, place)
	}
	return m, cmd
}

// dispatchPlace sends the panel's armed draft through the Trader — the same
// validated, journaled ops path as `order place` — under the single
// money-action gate.
func (m model) dispatchPlace() (tea.Model, tea.Cmd) {
	return m.dispatchPlaceForm(m.order.draft.form(), placeFromPanel)
}

// dispatchPlaceForm is the one place an order leaves the TUI: it holds the
// in-flight gate, stamps the active sub-account onto the form (the drafts
// carry everything else; the account is dispatch-time context), mints the
// clientOrderId and registers the order's local balance hold under it (so
// Available stays honest while the order is in flight — see
// state.AddLocalHold), and runs the Trader in a command. origin routes the
// result — a panel- or ladder-armed order lands inline on its surface, a
// bar-placed one reports via toast/flash only.
func (m model) dispatchPlaceForm(form OrderForm, origin placeOrigin) (tea.Model, tea.Cmd) {
	m.orderInFlight = true
	form.AccountSeq = m.accountSeq()
	form.ClientOrderID = ops.MintClientOrderID()
	m.registerPlaceHold(form)
	trader := m.cfg.Trader
	return m, func() tea.Msg {
		res, err := trader.Place(form)
		return placeDoneMsg{res: res, err: err, origin: origin,
			accountSeq: form.AccountSeq, clientOrderID: form.ClientOrderID}
	}
}

// registerPlaceHold registers the form's estimated balance reservation as a
// local hold before the order is sent (this runs on the update loop, which
// owns the store). The estimate mirrors the exchange's reservation: quote
// notional plus the maxFeeRate headroom for a quote-fee buy, base quantity
// for a sell. An inestimable form just skips the hold — that order degrades
// to the no-holds status quo.
func (m *model) registerPlaceHold(form OrderForm) {
	base, quote := splitSymbol(form.Symbol)
	intent := state.HoldIntent{
		Side:  form.Side,
		Price: form.Price, Qty: form.Qty, Amt: form.Amt,
		Base: base, Quote: quote,
	}
	if f := m.order.symFeesFor(form.Symbol); f != nil && strings.EqualFold(f.BuyFeeCurrency, quote) {
		intent.QuoteFeeRate = f.MaxRate
	}
	if cur, amt, ok := intent.EstimateHold(); ok {
		m.store.AddLocalHold(form.AccountSeq, form.ClientOrderID, cur, amt)
	}
}

// handleLadderKey routes keys while the trade ladder is open, and dispatches
// the armed order or cancel when the sub-model confirms it.
func (m model) handleLadderKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// The parent owns the +/- grouping keys while browsing (never mid-confirm
	// — the armed review's numbers must not have the book regrouped under
	// them).
	if s := msg.String(); (s == "+" || s == "-") && m.ladder.view == ladderBrowse {
		return m, m.cycleBookLevel(s == "+")
	}
	// The orders-pane tab switch also works while browsing the ladder — a
	// trader watching for a fill or reject must not have to leave it. Never
	// mid-confirm: an armed review owns every key.
	if msg.String() == "o" && m.ladder.view == ladderBrowse {
		m.showClosedOrders(!m.ordersClosed)
		return m, nil
	}
	var act ladderAction
	m.ladder, act = m.ladder.handleKey(msg, m.draftGate, m.ladderBusyGate(),
		m.ladderPrices(), m.ladderBands(), m.ladderFees(), m.bookGrp[m.symbol()])
	switch act {
	case ladderActClose:
		m.mode = modeNormal
	case ladderActPlace:
		return m.dispatchPlaceForm(m.ladder.armed.draft.form(), placeFromLadder)
	case ladderActCancel:
		return m.dispatchLadderCancel()
	}
	return m, nil
}

// dispatchLadderCancel issues the ladder's armed cancel through the Trader,
// under the same single money-action gate and canceling row styling as the
// open-orders x path.
func (m model) dispatchLadderCancel() (tea.Model, tea.Cmd) {
	id, symbol := m.ladder.armed.cancelID, m.ladder.symbol
	// The cancel targets the order's OWN sub-account (o.AccountSeq), which need
	// not be the active account in a multi-account session. The store's order map
	// never drops a row, so the arm-time order is still there; the active account
	// is only a defensive fallback for an order the store somehow lacks.
	seq := m.accountSeq()
	if o, ok := m.store.Order(id); ok {
		seq = o.AccountSeq
	}
	m.orderInFlight = true
	m.cancelsInFlight[id] = true
	m.refreshOrders() // mark the row canceling immediately
	trader := m.cfg.Trader
	return m, func() tea.Msg {
		warning, err := trader.Cancel(symbol, id, seq)
		return cancelDoneMsg{orderID: id, warning: warning, err: err}
	}
}

// orderLadderRows is the orderbook pane's visible price rows for the current
// geometry — what order mode's j/k cursor and click mapping walk. Empty while
// the book is loading/unsettled, and on a settled-but-empty book (no levels to
// pick) — read off the same classifier the pane renders from.
func (m model) orderLadderRows() []string {
	sym := m.symbol()
	book, ok := m.store.Orderbook(sym)
	if !ok || m.orderbookStatus(sym) != state.StatusPresent {
		return nil
	}
	_, h := m.bookPaneGeom()
	return orderbook.RowPrices(h, book)
}

func (m model) handleConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		id, symbol, seq := m.confirmOrderID, m.confirmSymbol, m.confirmSeq
		m.mode = modeNormal
		m.orderInFlight = true
		m.cancelsInFlight[id] = true
		m.refreshOrders() // mark the row canceling immediately
		trader := m.cfg.Trader
		cancel := func() tea.Msg {
			warning, err := trader.Cancel(symbol, id, seq)
			return cancelDoneMsg{orderID: id, warning: warning, err: err}
		}
		return m, cancel
	case "esc":
		m.mode = modeNormal
	}
	return m, nil
}

// --- cancel all ---

// openCancelAll opens the cancel-all dialog. The default scope follows the
// current open-orders view; the snapshot is taken immediately if the scope's
// orders are loaded, otherwise tracking is turned on and the dialog shows a
// loading state until they arrive (maybeSnapshotCancelAll completes it).
func (m *model) openCancelAll() {
	m.mode = modeCancelAll
	m.cancelAll = cancelAllState{active: true, allPairs: m.ordersAllPairs}
	m.prepareCancelAllScope()
	m.tryCancelAllSnapshot()
}

// closeCancelAll dismisses the dialog, resets its state (so a reopen starts
// clean), and restores the session's tracked-order set to the visible scope.
func (m *model) closeCancelAll() {
	m.cancelAll = cancelAllState{}
	m.mode = modeNormal
	m.applyOrderTracking()
}

// prepareCancelAllScope tells the session which scopes (under the active
// account) to keep an authoritative open-order snapshot for, per the dialog's
// scope: every watched pair for the all-pairs scope (turning on track-all so
// they load), or the active pair.
func (m *model) prepareCancelAllScope() {
	if m.cfg.SetTrackedOrders == nil {
		return
	}
	if m.cancelAll.allPairs {
		m.cfg.SetTrackedOrders(m.trackScopes(m.cfg.Symbols...))
	} else {
		m.cfg.SetTrackedOrders(m.trackScopes(m.symbol()))
	}
}

// setCancelAllScope switches the dialog to the given scope and re-snapshots for it
// (frozen again once ready). It is the single scope-change point shared by the
// key handler (←/→/tab toggle) and a click on a scope chip; a no-op while a batch
// is running or when already on that scope.
func (m *model) setCancelAllScope(allPairs bool) {
	if m.cancelAll.running || m.cancelAll.allPairs == allPairs {
		return
	}
	m.cancelAll.allPairs = allPairs
	m.cancelAll.snapped = false
	m.cancelAll.snapshot = nil
	m.prepareCancelAllScope()
	m.tryCancelAllSnapshot()
}

// cancelAllScopeReady reports whether the dialog's scope has an authoritative
// open-order snapshot (so the count is trustworthy and the order set complete).
func (m model) cancelAllScopeReady() bool {
	loaded, total := m.cancelAllLoaded()
	return loaded == total
}

// cancelAllLoaded reports how many of the scope's pairs have an authoritative
// open-order snapshot, out of the total, so the loading view can show progress.
// (An all-pairs scope stalls here if a per-pair snapshot fetch failed and hasn't
// been retried — the count makes that visible rather than a silent spinner.)
func (m model) cancelAllLoaded() (loaded, total int) {
	if !m.cancelAll.allPairs {
		if m.store.OpenOrdersReady(m.accountSeq(), m.symbol()) {
			return 1, 1
		}
		return 0, 1
	}
	total = len(m.cfg.Symbols)
	for _, s := range m.cfg.Symbols {
		if m.store.OpenOrdersReady(m.accountSeq(), s) {
			loaded++
		}
	}
	return loaded, total
}

// tryCancelAllSnapshot freezes the set of orders the dialog will cancel, once
// the scope's orders are loaded. It is a no-op once a snapshot has been taken
// (the count is deliberately frozen) or while a batch is running.
func (m *model) tryCancelAllSnapshot() {
	if m.cancelAll.snapped || m.cancelAll.running || !m.cancelAllScopeReady() {
		return
	}
	filter := "" // all pairs
	if !m.cancelAll.allPairs {
		filter = m.symbol()
	}
	open := m.store.OpenOrdersFor(m.accountSeq(), filter)
	snap := make([]cancelTarget, 0, len(open))
	for _, o := range open {
		snap = append(snap, cancelTarget{symbol: o.Symbol, orderID: o.OrderID, accountSeq: o.AccountSeq})
	}
	m.cancelAll.snapshot = snap
	m.cancelAll.snapped = true
}

// maybeSnapshotCancelAll re-attempts the snapshot when new order data arrives
// while the dialog is open and still waiting for its scope to load.
func (m *model) maybeSnapshotCancelAll() {
	if m.mode == modeCancelAll && m.cancelAll.active {
		m.tryCancelAllSnapshot()
	}
}

// dispatchNextCancelAll issues the cancel for the current snapshot entry
// (m.cancelAll.idx) and marks it in-flight for the canceling row style. The
// batch is sequential — one cancel in flight at a time — so the single-action
// gate (orderInFlight) holds and there is no bulk endpoint to misuse.
func (m *model) dispatchNextCancelAll() tea.Cmd {
	t := m.cancelAll.snapshot[m.cancelAll.idx]
	m.cancelsInFlight[t.orderID] = true
	trader := m.cfg.Trader
	return func() tea.Msg {
		_, err := trader.Cancel(t.symbol, t.orderID, t.accountSeq)
		return cancelAllStepMsg{orderID: t.orderID, err: err}
	}
}

func (m model) handleCancelAllKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.cancelAll.running {
			// Stop after the in-flight cancel returns (it's already on the wire);
			// the step handler finalizes and closes.
			m.cancelAll.aborted = true
			return m, nil
		}
		m.closeCancelAll()
		return m, nil
	case "left", "right", "tab":
		m.setCancelAllScope(!m.cancelAll.allPairs)
		return m, nil
	case "enter":
		if m.cancelAll.running {
			return m, nil
		}
		if !m.cancelAll.snapped {
			m.setToast(i18n.T("still loading orders for all pairs — wait until the count settles"), true)
			return m, nil
		}
		if len(m.cancelAll.snapshot) == 0 {
			m.setToast(i18n.T("no open orders to cancel in this scope"), false)
			m.closeCancelAll()
			return m, nil
		}
		if m.moneyActionInFlight() {
			m.setToast(i18n.T("another money action is already in flight — wait for it to finish"), true)
			return m, nil
		}
		m.cancelAll.running = true
		m.cancelAll.idx = 0
		m.orderInFlight = true
		cmd := m.dispatchNextCancelAll()
		m.refreshOrders() // mark the first row canceling
		return m, cmd
	}
	return m, nil
}

// selectedOrder maps the orders cursor to an order id.
func (m model) selectedOrder() (int64, bool) {
	if m.orderCursor < 0 || m.orderCursor >= len(m.orderIDs) {
		return 0, false
	}
	return m.orderIDs[m.orderCursor], true
}

// ordersVisible is the number of open-order rows the orders panel can show (its
// content rows minus the column header).
func (m model) ordersVisible() int {
	oh, _, _ := m.rightHeights(m.bodyHeight())
	v := oh - 4 // border (2) + panel title (1) + column header (1)
	if v < 1 {
		v = 1
	}
	return v
}

// orderScrollMax is the largest valid orders scroll offset.
func (m model) orderScrollMax() int {
	max := len(m.orderIDs) - m.ordersVisible()
	if max < 0 {
		max = 0
	}
	return max
}

// orderScrollClamped is the effective orders scroll offset (clamped on read).
func (m model) orderScrollClamped() int { return clamp(m.orderScroll, 0, m.orderScrollMax()) }

// ensureOrderVisible scrolls the orders window just enough to keep the selected
// row on screen after a cursor move.
func (m *model) ensureOrderVisible() {
	vis := m.ordersVisible()
	if vis < 1 {
		return
	}
	if m.orderCursor < m.orderScroll {
		m.orderScroll = m.orderCursor
	} else if m.orderCursor >= m.orderScroll+vis {
		m.orderScroll = m.orderCursor - vis + 1
	}
	m.orderScroll = clamp(m.orderScroll, 0, m.orderScrollMax())
}

// moveOrderCursor moves the open-orders selection by a navigation key, keeping
// it visible. The orders list, unlike the balances list, has a selected row
// (the cancel target).
func (m *model) moveOrderCursor(msg tea.KeyPressMsg) {
	n := len(m.orderIDs)
	if n == 0 {
		m.orderCursor, m.orderScroll = 0, 0
		return
	}
	switch msg.String() {
	case "down":
		m.orderCursor = clamp(m.orderCursor+1, 0, n-1)
	case "up":
		m.orderCursor = clamp(m.orderCursor-1, 0, n-1)
	case "pgdown":
		m.orderCursor = clamp(m.orderCursor+m.ordersVisible(), 0, n-1)
	case "pgup":
		m.orderCursor = clamp(m.orderCursor-m.ordersVisible(), 0, n-1)
	case "home":
		m.orderCursor = 0
	case "end":
		m.orderCursor = n - 1
	}
	m.ensureOrderVisible()
}

// refreshOrders rebuilds the open-orders rows from the store (all symbols or the
// active pair, newest first — the symbol column disambiguates). It is called
// when the order set can change — an order (myOrder) stream event, or a cancel
// label toggling — not on every stream tick. It preserves the selection BY ORDER
// ID, so a set that shifts under the cursor doesn't silently move the highlight
// onto a different order; the scroll window is re-clamped to keep it visible.
func (m *model) refreshOrders() {
	prevID, hadSelection := m.selectedOrder()
	filter := "" // all pairs
	if !m.ordersAllPairs {
		filter = m.symbol() // active pair only
	}
	var rows [][]string
	var ids []int64
	var canceling []bool
	if m.ordersClosed {
		for _, o := range m.store.ClosedOrdersFor(m.accountSeq(), filter, closedOrdersCap) {
			rows = append(rows, closedOrderRow(o))
			ids = append(ids, o.OrderID)
		}
	} else {
		open := m.store.OpenOrdersFor(m.accountSeq(), filter)
		rows = make([][]string, 0, len(open))
		ids = make([]int64, 0, len(open))
		canceling = make([]bool, 0, len(open))
		for _, o := range open {
			qty := o.Qty
			if qty == "" {
				qty = o.Amt // market buy: sized by amount
			}
			rows = append(rows, []string{uikit.FmtSymbol(o.Symbol), o.Side, uikit.GroupThousands(o.Price), qty, o.FilledQty})
			ids = append(ids, o.OrderID)
			canceling = append(canceling, m.cancelsInFlight[o.OrderID]) // rendered as a struck row, not a column
		}
	}
	m.orderRows = rows
	m.orderIDs = ids
	m.orderCanceling = canceling
	m.ordersRev++ // the projection changed; invalidate the orders pane's render cache
	cursor := 0   // default: top
	if hadSelection {
		for i, id := range ids {
			if id == prevID {
				cursor = i
				break
			}
		}
	}
	m.orderCursor = clamp(cursor, 0, maxInt(0, len(ids)-1))
	m.orderScroll = clamp(m.orderScroll, 0, m.orderScrollMax())
	m.ensureOrderVisible()
}

// noticePlacedTerminal surfaces the fate of a session-placed order that dies
// with no fill. Such an order never appears in the open-orders panel and has
// no fill row, so without a toast the accepted-then-rejected sequence (e.g. a
// post-only order that would cross, an ioc/fok with nothing to match — all
// terminal `expired`) is invisible. Silent here: an order that ended WITH fills
// (the fills panel shows it) and a canceled order (the cancel path already
// reports it — a cancel is never an invisible fate). Called whenever a myOrder
// event lands, and once on the place ack for an order already terminal by then.
func (m *model) noticePlacedTerminal() {
	for id := range m.placedWatch {
		o, ok := m.store.Order(id)
		if !ok || !o.Terminal() {
			continue
		}
		if o.Status == state.StatusClosedUnknown {
			// Closed while disconnected: the real fate — and any fill during
			// the gap — is unknown, so "no fill" could be a lie. Keep
			// watching; a real terminal status supersedes the synthetic one.
			continue
		}
		delete(m.placedWatch, id)
		if o.Status == "canceled" || !isZeroDecimal(o.FilledQty) {
			continue
		}
		switch o.Status {
		case "expired":
			// The API's terminal status for a time-in-force kill (post-only that
			// would cross, ioc/fok that can't fully match) — "rejected" in plain
			// prose, matching the synchronous place-reject toast. The closed tab's
			// status column keeps the literal "expired".
			m.setToast(i18n.T("order %d rejected — no fill", id), true)
		default:
			m.setToast(i18n.T("order %d closed — no fill (status %s)", id, o.Status), true)
		}
	}
}

// closedOrdersCap bounds the closed tab's rows on screen; the store separately
// bounds how many terminal orders it retains, so a long session needs neither
// unbounded memory nor an unbounded list.
const closedOrdersCap = 200

// closedOrderRow projects one terminal order into the closed tab's cells:
// symbol, side, price, filled/qty, status. filled/qty is in the order's own
// sizing terms (qty, or amt for an amount-sized market order) so a partial
// fill's returned remainder is legible. The synthetic closed-while-
// disconnected status renders as "closed?" — the real fate is unknown.
func closedOrderRow(o state.Order) []string {
	filled, size := o.FilledQty, o.Qty
	if size == "" {
		filled, size = o.FilledAmt, o.Amt // market buy: sized by amount
	}
	status := o.Status
	if status == state.StatusClosedUnknown {
		status = "closed?"
	}
	return []string{uikit.FmtSymbol(o.Symbol), o.Side, uikit.GroupThousands(o.Price), filled + "/" + size, status}
}

// applyOrderTracking tells the session which scopes' open-order snapshots to
// keep authoritative for the current view (under the active account): every
// watched pair in all-pairs view, or just the active pair otherwise. A no-op
// in non-lazy sessions (SetTrackedOrders nil).
func (m *model) applyOrderTracking() {
	if m.cfg.SetTrackedOrders == nil {
		return
	}
	if m.ordersAllPairs {
		m.cfg.SetTrackedOrders(m.trackScopes(m.cfg.Symbols...))
	} else {
		m.cfg.SetTrackedOrders(m.trackScopes(m.symbol()))
	}
}

// ordersLoading reports whether the open orders on display are still awaiting an
// authoritative snapshot (so the panel can say "loading"): the active pair in
// per-pair view, or any watched pair in all-pairs view. The live account-wide
// feed may already show some orders, but the set isn't yet known to be complete.
func (m model) ordersLoading() bool {
	if !m.ordersAllPairs {
		return !m.store.OpenOrdersReady(m.accountSeq(), m.symbol())
	}
	for _, sym := range m.cfg.Symbols {
		if !m.store.OpenOrdersReady(m.accountSeq(), sym) {
			return true
		}
	}
	return false
}

// toggleColorScheme flips the up/down color convention (green-red ↔ red-blue) and
// re-resolves the palette. Every surface that colors by direction — the
// orderbook depth bars, ticker, and the candle chart (which derives its styles
// from the palette at render time) — follows on the next frame. Bound to 'C' in
// both normal and chart modes.
func (m *model) toggleColorScheme() {
	m.colorScheme = m.colorScheme.Next()
	m.pal = uikit.PaletteFor(m.colorScheme, m.profile)
	m.setToast(i18n.T("color scheme: %s", m.colorScheme.String()), false)
}

// persistColorScheme reports the scheme name to save on exit and whether it
// differs from the loaded value. The C-key toggle applies live but is written
// to disk once, when the TUI exits (see tui.Run) — so the on-disk value always
// matches the last scheme shown, with no per-keypress file writes and no
// ordering hazard from concurrent saves. Returns false (skip the write) when
// the user never changed the scheme away from what was loaded.
func (m model) persistColorScheme() (name string, changed bool) {
	if m.colorScheme == uikit.ParseColorScheme(m.cfg.ColorScheme) {
		return "", false
	}
	return m.colorScheme.String(), true
}

// setToast shows a transient status message in the footer.
func (m *model) setToast(text string, isError bool) {
	m.toast = toast{text: text, isError: isError, until: m.cfg.Now() + toastMs}
}

// errorText renders an action error with its symbolic API code when present
// (the code is what the docs and support key off).
func errorText(err error) string {
	var ae *output.ApiError
	if errors.As(err, &ae) && ae.Code != "" {
		return ae.Code + " — " + ae.Message
	}
	return err.Error()
}
