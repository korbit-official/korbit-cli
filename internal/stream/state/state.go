// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package state materializes a stream.Session's event stream into current
// state: latest ticker and orderbook per symbol, recent public trades, the
// account's orders, fills, and balances, plus connection health. It is the
// state layer the TUI renders from; the stream package delivers events, this
// package answers "what is true right now".
//
// The Store is NOT goroutine-safe by design: it is built to be confined to a
// single consumer goroutine (the TUI applies events inside its update loop).
// Apply never blocks and never does I/O.
//
// Reconciliation rules (the reason this package exists):
//
//   - Events are not globally ordered across origins (a REST backfill patching
//     an old gap arrives after newer live frames), so nothing is applied by
//     arrival order. Ticker/orderbook (per symbol) order by newest
//     Data.ServerTime — they are public WS-only, so that is a homogeneous
//     same-clock comparison.
//   - Balances (per account+currency) carry no transport timestamp either. A
//     balance has no intrinsic monotonic progress, so two updates are ordered by
//     the private-connection epoch plus origin (see balanceSupersedes): a newer
//     connection's REST snapshot re-baselines older state, and within one
//     connection a live myAsset frame supersedes the reconnect snapshot and the
//     snapshot never clobbers a value a live frame already set. The snapshot is
//     also authoritative for disappearance: a prior-epoch balance absent from it
//     was zeroed while disconnected and is dropped (reconcileBalanceSnapshot).
//   - Every private record (order, fill, balance) carries the sub-account it
//     belongs to (AccountSeq; 1 = main), so a session subscribed to more than one
//     accountSeq keeps them apart — balances key on account+currency, not currency
//     alone. The seq is the live frame's wrapper accountSeq or the backfill call's
//     accountSeq (Data.AccountSeq), defaulting to 1 when the server/request did
//     not tag it.
//   - Orders do NOT order by the transport timestamp: a frame's server
//     send-time, or a backfill's fetch-start estimate, is not the order's own
//     event time. Two updates to the same orderId are ordered by the order's
//     intrinsic monotonic progress instead — a terminal status is absorbing
//     (any status other than the open set pending/open/partiallyFilled — a
//     closed order can never be reopened), then a higher lifecycle rank
//     (pending<open<partiallyFilled), then a larger
//     cumulative filledQty — so the more-advanced state always wins regardless
//     of arrival order or origin. See orderProgress / supersedes in
//     orderprogress.go, the single source of truth for this rule.
//   - A /v2/openOrders backfill payload is an authoritative snapshot of what
//     is open for its symbol: an open order absent from it that was last known
//     from an EARLIER connection (a lower epoch — see Store.epoch) closed while
//     disconnected; its status becomes StatusClosedUnknown until (usually) the
//     matching /v2/allOrders gap rows deliver the real terminal status. Orders
//     first seen in the current connection are spared (placed after the
//     snapshot, or their close arrives as a lossless live terminal frame or a
//     backfilled /v2/allOrders row).
//   - Trades and fills are deduped by tradeId here as well: the stream layer
//     already dedupes, but it deliberately re-emits verbatim frames when a
//     rebuild fails (duplicates over silent loss), so the materialized view
//     must be idempotent.
//   - WS and REST spell the same concepts differently; rows are normalized to
//     one shape (REST "open" for the WS "unfilled" status; Fill.Fee from WS
//     "fee" / REST "feeQty"; Fill.Time from WS "filledAt" / REST "tradedAt").
//     Decimal values stay strings, untouched, end to end — with one read-side
//     exception: Balance.Available is re-derived (exact decimal math) while
//     local holds are active, see localhold.go.
package state

import (
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/shopspring/decimal"
)

func wallClock() int64 { return time.Now().UnixMilli() }

// Synthetic order status: the order disappeared from an authoritative
// /v2/openOrders snapshot while disconnected and no history row has told us
// its real terminal status (yet). Terminal for display purposes.
const StatusClosedUnknown = "closed"

// Config bounds the Store's memory. Zero fields take defaults.
type Config struct {
	TradeCap  int // recent public trades kept per symbol (default 200)
	FillCap   int // recent fills kept across symbols (default 200)
	NoticeCap int // recent notices kept (default 100)
	// Log is the optional operational logger for the correctness-driving
	// reconcile decisions: the private-connection epoch bump and a gap-closed
	// (synthetic "closed") order. nil is silent. Debug level — these are the
	// mechanics behind the materialized view, not program output.
	Log *slog.Logger
}

// Ticker is the latest ticker state for one symbol (WS ticker frames carry
// complete state). All prices/quantities are decimal strings.
type Ticker struct {
	Symbol             string
	Open               string
	High               string
	Low                string
	Close              string
	PrevClose          string
	PriceChange        string
	PriceChangePercent string
	Volume             string
	QuoteVolume        string
	BestBidPrice       string
	BestAskPrice       string
	LastTradedAt       int64
	ServerTime         int64 // ordering key (frame timestamp)
}

// PriceLevel is one orderbook level.
type PriceLevel struct {
	Price string `json:"price"`
	Qty   string `json:"qty"`
}

// Orderbook is the latest complete book for one symbol (every WS orderbook
// frame carries the full book).
type Orderbook struct {
	Symbol     string
	Timestamp  int64        // the book's own timestamp (data.timestamp)
	Bids       []PriceLevel // best (highest) first, as delivered
	Asks       []PriceLevel // best (lowest) first, as delivered
	ServerTime int64        // ordering key (frame timestamp)
}

// Trade is one public trade. The WS frame rows and the /v2/trades backfill
// rows share this shape.
type Trade struct {
	TradeID      int64  `json:"tradeId"`
	Timestamp    int64  `json:"timestamp"`
	Price        string `json:"price"`
	Qty          string `json:"qty"`
	IsBuyerTaker bool   `json:"isBuyerTaker"`
}

// Direction is a price-tick direction for display. The zero value is Neutral.
type Direction int8

const (
	Neutral Direction = iota
	Up
	Down
)

// tickState carries a symbol's last-trade tick direction: dir is the tip's
// direction vs the previous trade at a different price, and price/id are the
// tip (highest-id trade folded in) — id doubles as the fold high-water mark.
type tickState struct {
	dir   Direction
	price string
	id    int64
}

// Order is the materialized state of one of the account's orders, merged from
// WS myOrder frames and /v2/openOrders + /v2/allOrders backfill rows.
type Order struct {
	OrderID       int64
	AccountSeq    int // sub-account (1 = main); from the frame/backfill, default 1
	ClientOrderID string
	Symbol        string
	Side          string
	OrderType     string
	Price         string
	Qty           string
	Amt           string
	FilledQty     string
	FilledAmt     string
	AvgPrice      string
	Status        string // normalized: WS "unfilled" becomes "open"
	CreatedAt     int64
	LastFilledAt  int64
	// ClosedAt is when the store saw the order transition into a terminal
	// status (the store's clock). A real terminal status is absorbing, so
	// the stamp moves only when a synthetic gap-close is revived by a live
	// frame and the order later truly closes. The API carries no close
	// timestamp, so this local observation stamp is the only "recently
	// closed" ordering available; it is a display key, not a server time,
	// and for a close that happened during a disconnect it is the
	// reconnect's reconcile time, not the actual close time.
	ClosedAt int64

	// epoch is the private-connection generation when this order was last
	// updated — the open-orders snapshot reconcile's ordering key (see the
	// Store.epoch field and reconcileOpenSnapshot). Internal to the store.
	epoch int64
}

// openStatuses is THE single source of truth for order-status classification.
// It is the FIXED, exhaustive set of statuses that mean an order is still live
// on the book — a CLOSED set that never grows. Everything else is derived from
// it: a known (non-empty) status that is not in this set is terminal. This
// asymmetry is deliberate. The API may add terminal statuses (new failed/closed
// states) over time, and they must classify correctly with NO change here — so
// we only ever enumerate the open set and NEVER enumerate terminal statuses.
// Do not add "closed"/"filled"/etc. comparisons anywhere; ask statusIsOpen /
// statusIsTerminal instead.
//
// (WS spells the open status "unfilled"; normalizeStatus maps it to "open"
// before any classification, so both feeds use this same set.)
var openStatuses = map[string]bool{
	"pending":         true,
	"open":            true,
	"partiallyFilled": true,
}

// statusIsOpen reports whether a normalized status means the order is still live
// on the book (should appear in an open-orders view).
func statusIsOpen(status string) bool { return openStatuses[status] }

// statusIsTerminal reports whether a normalized status means the order can never
// change again. Terminal is defined as the complement of open: any known
// (non-empty) status that is not in openStatuses. The empty string is "status
// not yet known" — neither open nor terminal — so a partially-populated order
// row is never mistaken for a closed one. Because terminal is a complement, a
// status the API adds in the future is classified terminal automatically.
func statusIsTerminal(status string) bool {
	return status != "" && !statusIsOpen(status)
}

// Terminal reports whether the order can never change again.
func (o Order) Terminal() bool { return statusIsTerminal(o.Status) }

// Open reports whether the order should appear in an open-orders view.
func (o Order) Open() bool { return statusIsOpen(o.Status) }

// Fill is one of the account's fills, normalized across the WS myTrade row
// (fee, filledAt) and the REST /v2/myTrades row (feeQty, tradedAt).
type Fill struct {
	TradeID     int64
	OrderID     int64
	AccountSeq  int // sub-account (1 = main); from the frame/backfill, default 1
	Symbol      string
	Side        string
	Price       string
	Qty         string
	Fee         string
	FeeCurrency string
	IsTaker     bool
	Time        int64 // unix ms (WS filledAt / REST tradedAt)
}

// Balance is the latest balance state for one currency. The WS myAsset row
// and the REST /v2/balance row share their field names (REST has no
// updatedAt).
type Balance struct {
	AccountSeq      int // sub-account (1 = main); from the frame/backfill, default 1
	Currency        string
	Balance         string
	Available       string
	TradeInUse      string
	WithdrawalInUse string
	AvgPrice        string
	UpdatedAt       int64 // per-row server timestamp (0 from REST rows)

	// epoch is the private-connection generation this balance was last set in,
	// and live records whether that came from a live myAsset frame (vs a REST
	// snapshot). Together they order updates WITHOUT a transport timestamp — see
	// balanceSupersedes. Internal to the store.
	epoch int64
	live  bool
}

// EndpointHealth is the connection state of one WebSocket endpoint.
type EndpointHealth struct {
	Known        bool // a CONNECTED/DISCONNECTED notice has been seen
	Up           bool
	LastChangeAt int64
	LastError    string // last CONNECT_FAILED/DISCONNECTED reason while down

	// Delayed and Unreliable are standing degradation conditions, each set by
	// its warning notice (DATA_DELAYED / CONNECTION_UNRELIABLE) and cleared by
	// its recovery notice (DATA_CURRENT / CONNECTION_STABLE) — so a UI can show
	// "currently degraded" rather than a stale warning that never clears.
	// Delayed also clears on (re)connect: a fresh connection has no evidence of
	// lag until a late frame re-raises it. Unreliable is NOT cleared by a
	// reconnect — the stream owns its lifecycle (recent drops keep it set until
	// they age out) and announces recovery with CONNECTION_STABLE.
	//
	// Both describe a connection that is up but degraded, so they are meaningful
	// only while Up: a consumer that surfaces them should gate on Up (a down or
	// fatal endpoint, signaled by Up=false, may still carry a set Unreliable that
	// never had a chance to clear).
	Delayed    bool
	Unreliable bool
}

// Health is the stream's reliability state, derived from notices.
type Health struct {
	Public      EndpointHealth
	Private     EndpointHealth
	Backfilling bool
	GapCount    int   // DATA_GAP notices seen
	LastDataAt  int64 // local receive time of the newest data event
	DataCount   int64
}

// Store materializes events. Create with New, feed with Apply, read with the
// accessor methods. Not goroutine-safe; confine to one goroutine.
type Store struct {
	cfg Config
	lg  *slog.Logger // resolved once from cfg.Log (never nil); see log()

	tickers map[string]Ticker
	books   map[string]Orderbook
	trades  map[string]*tradeRing
	orders  map[int64]Order
	fills   []Fill
	fillIDs map[fillKey]struct{}
	bals    map[balKey]Balance

	// holds are the active local balance reservations for own orders in
	// flight (see localhold.go); Balances/BalancesFor report Available net of
	// them. Empty — and the layer inert — unless the consumer registers holds
	// via AddLocalHold.
	holds map[holdKey]localHold

	// orderSnapAt is the ServerTime of the last applied /v2/openOrders snapshot
	// per {account, symbol} — a newest-wins guard so a stale snapshot (e.g. an
	// on-demand fetch that landed after a fresher reconnect snapshot) can't
	// resurrect or wrongly close orders. orderReady tracks whether an
	// {account, symbol} has an authoritative snapshot since the last (re)connect,
	// for a "still loading" UI hint; it is cleared on DISCONNECTED. Both key by
	// account because a /v2/openOrders snapshot is per sub-account: one account's
	// snapshot must neither drop another account's (independent ServerTimes) nor
	// be read as covering it.
	orderSnapAt map[orderSnapKey]int64
	orderReady  map[orderSnapKey]bool

	// Freshness latches so the UI can hide data that is no longer trustworthy
	// (stale) behind a "loading…" hint rather than present it as live — the
	// public, per-symbol analogue of orderReady. A latch is set when an
	// authoritative frame for that symbol arrives and cleared when the feed
	// backing it drops: the public latches (book/trade/ticker) on a public
	// DISCONNECTED, balReady on a private one. bookReady/tradeReady are
	// additionally reset by the consumer on a market switch (MarkMarketStale),
	// since the retained book/trades for a newly-activated symbol are from its
	// prior subscription until the resubscribe snapshot lands. The data itself is
	// never dropped (the trade ring's high-water mark still drives gap-patching);
	// only its display is gated. Fills carry no latch — the TUI runs snapshot-only
	// backfill, so there is no fill re-snapshot to wait for; their trust is simply
	// whether the private endpoint is up (Health().Private.Up).
	bookReady   map[string]bool
	tradeReady  map[string]bool
	tickerReady map[string]bool
	// balReady latches per sub-account (keyed by effAccountSeq), set when that
	// account's /v2/balance snapshot lands and cleared on a private DISCONNECTED.
	// Readiness is per account, never aggregated here: a session covering several
	// sub-accounts (only monitor/botapi does) tracks each independently, and any
	// "all subscribed accounts ready" rollup is the consumer's to compute — it is
	// the only layer that knows the subscribed set.
	balReady map[int]bool

	// Last-trade tick direction per symbol. Advanced O(1) as live trades append
	// (advanceTickState); recomputed from the ring (rescanTickState) only when a
	// backfill inserts a trade at or below the tip, which can change its
	// predecessor. Gated by tradeReady and the public-trade gap latches, so stale
	// or incomplete predecessor chains read as Neutral.
	tick             map[string]tickState
	tradeGapPending  map[string]int
	tradeGapLost     map[string]bool
	tradeGapSnapshot map[string]bool

	health    Health
	backfills int // active BACKFILL_START notices minus BACKFILL_DONE notices
	notices   []stream.Notice

	// epoch is the private-connection generation, bumped on each private
	// CONNECTED notice. Every order is stamped with the epoch it was last
	// updated in, and the open-orders snapshot reconcile closes only orders from
	// a STRICTLY EARLIER epoch (known from a prior connection). That is the
	// clock-free "did this close during the disconnect?" test — it needs no
	// transport timestamp (a frame's server send-time, or a backfill's
	// fetch-start estimate, can't order events across the two clock sources).
	epoch int64

	// Per-section revision counters, bumped on every mutation of that section so a
	// consumer can detect "did this data change" with a cheap equality check
	// instead of comparing the data itself (the TUI's per-component render cache
	// keys on them). They are plain monotonic counters — only equality matters, so
	// wraparound is irrelevant. All mutation flows through Apply and
	// MarkMarketStale, so the bumps live in exactly those two places. Orderbook and
	// trade frames only ever arrive for the active symbol (only it is subscribed),
	// so a single scalar per section suffices; a symbol-specific consumer also
	// carries the active symbol in its own key, so a switch can't be a false hit.
	revTicker, revBook, revTrade, revOrder, revFill, revBal, revNotice, revHealth uint64

	now func() int64 // injected for LastDataAt; tests pin it
}

type fillKey struct {
	symbol string
	id     int64
}

// balKey keys a balance by sub-account and currency, so a session subscribed to
// more than one accountSeq does not collide two sub-accounts' rows for the same
// currency.
type balKey struct {
	accountSeq int
	currency   string
}

// orderSnapKey keys the open-order snapshot freshness guard and readiness latch
// by sub-account and symbol: /v2/openOrders is fetched per account, so each
// account+symbol snapshot is independent.
type orderSnapKey struct {
	accountSeq int
	symbol     string
}

// effAccountSeq normalizes a frame/backfill accountSeq to a concrete sub-account:
// nil (untagged) or 0 becomes 1, the API's main-account default.
func effAccountSeq(seq *int) int {
	if seq == nil || *seq <= 0 {
		return 1
	}
	return *seq
}

// genOf returns the private-connection generation an event belongs to. A REST
// backfill carries the generation it was fetched in (Data.PrivateEpoch); a live
// frame leaves it 0 and is always applied while the store is still on its
// connection's epoch, so an unstamped event reads as the current epoch. The store
// uses this — never a transport timestamp — to tell a backfill from a superseded
// connection (gen < s.epoch) from a current one.
func (s *Store) genOf(e stream.Data) int64 {
	if e.PrivateEpoch > 0 {
		return e.PrivateEpoch
	}
	return s.epoch
}

// New builds an empty Store. now stamps Health.LastDataAt on each data event;
// nil defaults to the wall clock.
func New(cfg Config, now func() int64) *Store {
	if cfg.TradeCap <= 0 {
		cfg.TradeCap = 200
	}
	if cfg.FillCap <= 0 {
		cfg.FillCap = 200
	}
	if cfg.NoticeCap <= 0 {
		cfg.NoticeCap = 100
	}
	if now == nil {
		now = wallClock
	}
	return &Store{
		cfg:              cfg,
		lg:               logging.Or(cfg.Log),
		tickers:          map[string]Ticker{},
		books:            map[string]Orderbook{},
		trades:           map[string]*tradeRing{},
		orders:           map[int64]Order{},
		fillIDs:          map[fillKey]struct{}{},
		bals:             map[balKey]Balance{},
		holds:            map[holdKey]localHold{},
		orderSnapAt:      map[orderSnapKey]int64{},
		orderReady:       map[orderSnapKey]bool{},
		bookReady:        map[string]bool{},
		tradeReady:       map[string]bool{},
		tickerReady:      map[string]bool{},
		balReady:         map[int]bool{},
		tick:             map[string]tickState{},
		tradeGapPending:  map[string]int{},
		tradeGapLost:     map[string]bool{},
		tradeGapSnapshot: map[string]bool{},
		now:              now,
	}
}

// Apply folds one event into the state. Unknown channels and malformed
// payloads are ignored (the stream layer already surfaces reliability
// problems as notices; a frame this package cannot parse must not kill the
// consumer).
func (s *Store) Apply(ev stream.Event) {
	// Expire overdue local holds on every event, so the TTL backstop fires no
	// later than the next thing that could observe a balance.
	s.sweepLocalHolds()
	switch e := ev.(type) {
	case stream.Notice:
		s.applyNotice(e)
		// A notice changes connection/freshness state (health, and DISCONNECTED
		// resets the book/trade/ticker/balance readiness latches), so conservatively
		// bump every section — notices are infrequent, so over-invalidating is cheap.
		s.revNotice++
		s.revHealth++
		s.revTicker++
		s.revBook++
		s.revTrade++
		s.revBal++
	case stream.Data:
		s.health.LastDataAt = s.now()
		s.health.DataCount++
		s.revHealth++
		switch e.Channel {
		case stream.ChannelTicker:
			s.applyTicker(e)
			s.revTicker++
		case stream.ChannelOrderbook:
			s.applyOrderbook(e)
			s.revBook++
		case stream.ChannelTrade:
			s.applyTrade(e)
			s.revTrade++
		case stream.ChannelMyOrder:
			s.applyMyOrder(e)
			s.pruneTerminalOrders()
			s.revOrder++
		case stream.ChannelMyTrade:
			s.applyMyTrade(e)
			s.revFill++
		case stream.ChannelMyAsset:
			s.applyMyAsset(e)
			s.revBal++
		}
	}
}

// --- reads ---

// Ticker returns the latest ticker for symbol.
func (s *Store) Ticker(symbol string) (Ticker, bool) {
	t, ok := s.tickers[symbol]
	return t, ok
}

// Orderbook returns the latest book for symbol.
func (s *Store) Orderbook(symbol string) (Orderbook, bool) {
	b, ok := s.books[symbol]
	return b, ok
}

// Trades returns the recent public trades for symbol, newest first.
func (s *Store) Trades(symbol string) []Trade {
	r := s.trades[symbol]
	if r == nil {
		return nil
	}
	return r.newestFirst()
}

// TradesSince returns symbol's public trades with TradeID > afterID, oldest
// first — the tail a candle fold has not yet consumed. It allocates only that
// tail (nil when nothing is newer), so a consumer polling every stream batch
// does no work proportional to the whole retained ring when nothing arrived.
func (s *Store) TradesSince(symbol string, afterID int64) []Trade {
	r := s.trades[symbol]
	if r == nil {
		return nil
	}
	return r.since(afterID)
}

// OpenOrders returns the orders currently open for symbol ("" = all symbols),
// newest created first, across every sub-account. A per-account consumer uses
// OpenOrdersFor instead.
func (s *Store) OpenOrders(symbol string) []Order {
	var out []Order
	for _, o := range s.orders {
		if o.Open() && (symbol == "" || o.Symbol == symbol) {
			out = append(out, o)
		}
	}
	sortOrdersNewestFirst(out)
	return out
}

func sortOrdersNewestFirst(out []Order) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].OrderID > out[j].OrderID
	})
}

// OpenOrdersFor returns the orders currently open for one sub-account and
// symbol ("" = all symbols), newest created first — the per-account view of
// OpenOrders. A consumer rendering one sub-account of a multi-account session
// reads through this (paired with OpenOrdersReady for the same key) so another
// account's rows never leak into its panes.
func (s *Store) OpenOrdersFor(accountSeq int, symbol string) []Order {
	seq := effAccountSeq(&accountSeq)
	var out []Order
	for _, o := range s.orders {
		if o.AccountSeq == seq && o.Open() && (symbol == "" || o.Symbol == symbol) {
			out = append(out, o)
		}
	}
	sortOrdersNewestFirst(out)
	return out
}

// ClosedOrdersFor returns accountSeq's terminal orders for symbol ("" = all
// symbols), most recently OBSERVED closed first (Order.ClosedAt — a local
// stamp, not a server time), capped at limit (<= 0 = uncapped). The set is
// session-scoped: it holds the terminal transitions this store has seen (live
// frames, snapshots, backfill rows), not the account's order history.
func (s *Store) ClosedOrdersFor(accountSeq int, symbol string, limit int) []Order {
	seq := effAccountSeq(&accountSeq)
	var out []Order
	for _, o := range s.orders {
		if o.AccountSeq == seq && o.Terminal() && (symbol == "" || o.Symbol == symbol) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ClosedAt != out[j].ClosedAt {
			return out[i].ClosedAt > out[j].ClosedAt
		}
		return out[i].OrderID > out[j].OrderID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// terminalOrdersCap bounds the terminal (closed) orders the store retains for
// the whole session, so a long run stays memory-bounded. Open orders are never
// pruned; only terminal ones, oldest observed-close first (Order.ClosedAt), so
// the recent closed history stays complete. It is a session-wide bound across
// every account and symbol — a consumer's per-view closed list is filtered and
// separately capped on top of this.
const terminalOrdersCap = 200

// pruneTerminalOrders drops the oldest terminal orders beyond terminalOrdersCap,
// keeping the most recently closed (by Order.ClosedAt, then OrderID — the order
// ClosedOrdersFor presents). Open orders are always kept. The count is the fast
// path: no allocation or sort while the terminal set is within the cap.
func (s *Store) pruneTerminalOrders() {
	n := 0
	for _, o := range s.orders {
		if o.Terminal() {
			n++
		}
	}
	if n <= terminalOrdersCap {
		return
	}
	terminal := make([]Order, 0, n)
	for _, o := range s.orders {
		if o.Terminal() {
			terminal = append(terminal, o)
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].ClosedAt != terminal[j].ClosedAt {
			return terminal[i].ClosedAt > terminal[j].ClosedAt
		}
		return terminal[i].OrderID > terminal[j].OrderID
	})
	for _, o := range terminal[terminalOrdersCap:] {
		delete(s.orders, o.OrderID)
	}
}

// OpenOrdersReady reports whether accountSeq's open orders for symbol have an
// authoritative snapshot since the last (re)connect. False means it is still
// loading (or that account+symbol is not being tracked): the live account-wide
// feed may already show some orders, but the set is not yet known to be
// complete. A UI hint, not a correctness gate. Keyed per {account, symbol}: a
// caller covering several sub-accounts must check each — there is no "any/all
// account" rollup here (the aggregate belongs to the consumer that knows the
// subscribed set).
func (s *Store) OpenOrdersReady(accountSeq int, symbol string) bool {
	return s.orderReady[orderSnapKey{accountSeq: effAccountSeq(&accountSeq), symbol: symbol}]
}

// OrderbookReady reports whether symbol's orderbook is current: a book frame has
// arrived since the last public (re)connect and since the symbol was last
// (re)subscribed. False means the displayed book (if any) is from a prior
// subscription or a dropped feed and should show as loading, not as live depth.
func (s *Store) OrderbookReady(symbol string) bool { return s.bookReady[symbol] }

// TradesReady reports whether symbol's public trades are current — the trade
// analogue of OrderbookReady. False ⇒ the retained trades are stale (prior
// subscription or dropped feed); show loading rather than present them as recent.
func (s *Store) TradesReady(symbol string) bool { return s.tradeReady[symbol] }

// LastTick reports the price-tick direction of symbol's most recent trade
// relative to the previous trade at a different price. It is Neutral when the
// trades aren't current (cold start, market switch, or a feed drop), a detected
// trade gap is still being patched or could not be fully recovered, or no price
// change is visible in the retained window. The value is maintained on each
// trade frame (see applyTrade), so this read is O(1).
func (s *Store) LastTick(symbol string) Direction {
	if !s.tradeReady[symbol] || s.tradeGapPending[symbol] > 0 || s.tradeGapLost[symbol] {
		return Neutral
	}
	return s.tick[symbol].dir
}

// TickerReady reports whether symbol's ticker is current (a frame has arrived
// since the last public connect). False ⇒ the last price is stale; show loading.
func (s *Store) TickerReady(symbol string) bool { return s.tickerReady[symbol] }

// BalancesReady reports whether accountSeq's balances have been (re)snapshotted
// since the last private (re)connect. False ⇒ the displayed balances may be
// stale; show loading. Keyed per sub-account: a caller covering several accounts
// must check each (there is no "all accounts" rollup here — see balReady).
func (s *Store) BalancesReady(accountSeq int) bool { return s.balReady[effAccountSeq(&accountSeq)] }

// MarkMarketStale resets the orderbook/trade freshness latches for symbol so the
// UI hides those panes until the next subscription snapshot lands. The consumer
// calls it when it re-points the active-symbol orderbook/trade subscriptions (a
// market switch): the retained book/trades are from the prior subscription and
// must not be shown as current until the resubscribe snapshot arrives. The data
// itself is kept — the trade ring's high-water mark still drives gap-patching.
func (s *Store) MarkMarketStale(symbol string) {
	delete(s.bookReady, symbol)
	delete(s.tradeReady, symbol)
	s.revBook++
	s.revTrade++
}

// MarkBookStale resets only the orderbook freshness latch for symbol. The
// consumer calls it when it re-points the orderbook subscription alone (an
// orderbook grouping-level change): the retained book is at the old
// granularity and must hide until the new subscription's snapshot lands,
// while the untouched trade feed stays current — clearing the trade latch too
// would strand the trades pane on "loading" with no snapshot coming.
func (s *Store) MarkBookStale(symbol string) {
	delete(s.bookReady, symbol)
	s.revBook++
}

// Section revisions: monotonic counters that change whenever that section's data
// (or its freshness/readiness, which gates display) changes. A consumer keys a
// render cache on the revision(s) it depends on instead of diffing the data. Only
// equality is meaningful. Orderbook/trade/ticker frames arrive only for the
// active symbol, so these are process-wide scalars; a symbol-specific consumer
// also carries the active symbol in its key.
func (s *Store) TickerRev() uint64  { return s.revTicker }
func (s *Store) BookRev() uint64    { return s.revBook }
func (s *Store) TradeRev() uint64   { return s.revTrade }
func (s *Store) OrderRev() uint64   { return s.revOrder }
func (s *Store) FillRev() uint64    { return s.revFill }
func (s *Store) BalanceRev() uint64 { return s.revBal }
func (s *Store) NoticeRev() uint64  { return s.revNotice }
func (s *Store) HealthRev() uint64  { return s.revHealth }

// Order returns one order by id.
func (s *Store) Order(orderID int64) (Order, bool) {
	o, ok := s.orders[orderID]
	return o, ok
}

// Fills returns the recent fills across symbols and sub-accounts, newest
// first. A per-account consumer uses FillsFor instead.
func (s *Store) Fills() []Fill {
	out := make([]Fill, len(s.fills))
	copy(out, s.fills)
	return out
}

// FillsFor returns the recent fills of one sub-account, newest first — the
// per-account view of Fills. Note the retained window (Config.FillCap) is
// shared across accounts: a busy account can age another's fills out.
func (s *Store) FillsFor(accountSeq int) []Fill {
	seq := effAccountSeq(&accountSeq)
	out := make([]Fill, 0, len(s.fills))
	for _, f := range s.fills {
		if f.AccountSeq == seq {
			out = append(out, f)
		}
	}
	return out
}

// Balances returns the latest balances across every sub-account, sorted by
// account then currency. A per-account consumer uses BalancesFor instead.
// Available is net of the active local holds (see localhold.go); the read
// also expires overdue holds, so it may mutate hold state (fine on the
// store's single consumer goroutine).
func (s *Store) Balances() []Balance {
	s.sweepLocalHolds()
	out := make([]Balance, 0, len(s.bals))
	for _, b := range s.bals {
		out = append(out, s.applyLocalHolds(b))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AccountSeq != out[j].AccountSeq {
			return out[i].AccountSeq < out[j].AccountSeq
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// BalancesFor returns one sub-account's latest balances, sorted by currency —
// the per-account view of Balances (paired with BalancesReady for the same
// account). Available is net of the active local holds, as in Balances.
func (s *Store) BalancesFor(accountSeq int) []Balance {
	s.sweepLocalHolds()
	seq := effAccountSeq(&accountSeq)
	out := make([]Balance, 0, len(s.bals))
	for k, b := range s.bals {
		if k.accountSeq == seq {
			out = append(out, s.applyLocalHolds(b))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// Health returns the stream's reliability state.
func (s *Store) Health() Health { return s.health }

// Notices returns up to n recent notices, newest first (n <= 0 = all kept).
func (s *Store) Notices(n int) []stream.Notice {
	if n <= 0 || n > len(s.notices) {
		n = len(s.notices)
	}
	out := make([]stream.Notice, 0, n)
	for i := len(s.notices) - 1; i >= len(s.notices)-n; i-- {
		out = append(out, s.notices[i])
	}
	return out
}

// --- notice handling ---

func (s *Store) applyNotice(n stream.Notice) {
	s.notices = append(s.notices, n)
	if len(s.notices) > s.cfg.NoticeCap {
		s.notices = s.notices[len(s.notices)-s.cfg.NoticeCap:]
	}

	endpoint, _ := n.Details["endpoint"].(string)
	eh := s.endpointHealth(endpoint)
	switch n.Code {
	case stream.Connected:
		if endpoint == "private" {
			// A new private connection opens a new epoch: the open-orders
			// snapshot this connect backfills is authoritative over everything
			// known from the previous connection.
			s.epoch++
			// Snapshot authority restarts with the epoch, so drop the
			// per-{account, symbol} newest-wins watermarks. They compare
			// ServerTimes (backfill fetch-start estimates), which a mid-session
			// clock resync can move BACKWARD — without the reset, a superseded
			// connection's watermark could outrank the new connection's snapshot
			// and leave the symbol unreconciled and never ready. Cross-epoch
			// staleness needs no watermark: an old-generation snapshot is
			// rejected by the gen < epoch guard before the watermark is read.
			s.orderSnapAt = map[orderSnapKey]int64{}
			// Local holds belong to the superseded connection: their myOrder
			// release signal may have been lost with it, and the reconnect
			// snapshot re-baselines balances anyway (see localhold.go).
			s.clearLocalHolds()
			s.log().Debug("state epoch bump", "epoch", s.epoch)
		}
		if eh != nil {
			// Preserve Unreliable across the reconnect: the stream clears it with
			// CONNECTION_STABLE once recent drops age out, so a single reconnect
			// must not hide an ongoing flap. Delayed is dropped — a fresh
			// connection has no evidence of lag until a late frame re-raises it.
			*eh = EndpointHealth{Known: true, Up: true, LastChangeAt: n.Time, Unreliable: eh.Unreliable}
		}
	case stream.Disconnected, stream.Fatal:
		if eh != nil {
			reason, _ := n.Details["reason"].(string)
			if reason == "" {
				reason, _ = n.Details["error"].(string)
			}
			// Preserve Unreliable (see Connected): the DISCONNECTED notice arrives
			// right after the stream may have set it on this same drop.
			*eh = EndpointHealth{Known: true, Up: false, LastChangeAt: n.Time, LastError: reason, Unreliable: eh.Unreliable}
		}
		if endpoint == "private" {
			// The private feed dropped: open-order snapshots and balances are no
			// longer authoritative until the reconnect backfill re-snapshots them,
			// so a UI should show them as loading again.
			s.orderReady = map[orderSnapKey]bool{}
			s.balReady = map[int]bool{}
			// And the myOrder events that release local holds can no longer
			// arrive — release them all rather than hold a reservation nothing
			// can clear (see localhold.go).
			s.clearLocalHolds()
		}
		if endpoint == "public" {
			// The public feed dropped: ticker/orderbook/trade frames are no longer
			// current until the reconnect resubscribe re-snapshots, so a UI should
			// show those panes as loading rather than present stale prices/depth.
			s.bookReady = map[string]bool{}
			s.tradeReady = map[string]bool{}
			s.tickerReady = map[string]bool{}
		}
	case stream.ConnectFailed:
		if eh != nil && !eh.Up {
			eh.Known = true
			errText, _ := n.Details["error"].(string)
			eh.LastError = errText
		}
	case stream.BackfillStart:
		s.backfills++
		s.health.Backfilling = true
		if publicTradeGapNotice(n) {
			symbol, _ := n.Details["symbol"].(string)
			if symbol != "" {
				s.tradeGapPending[symbol]++
			}
		}
	case stream.BackfillDone:
		if s.backfills > 0 {
			s.backfills--
		}
		s.health.Backfilling = s.backfills > 0
		if publicTradeGapNotice(n) {
			symbol, _ := n.Details["symbol"].(string)
			if symbol != "" {
				if s.tradeGapPending[symbol] > 1 {
					s.tradeGapPending[symbol]--
				} else {
					delete(s.tradeGapPending, symbol)
				}
				if complete, _ := n.Details["complete"].(bool); !complete {
					s.tradeGapLost[symbol] = true
				}
			}
		}
	case stream.BackfillDisabled, stream.BackfillFailed:
		if publicTradeGapNotice(n) {
			s.markTradeGapLost(n)
		}
	case stream.DataGap:
		s.health.GapCount++
		if channel, _ := n.Details["channel"].(string); channel == stream.ChannelTrade {
			symbol, _ := n.Details["symbol"].(string)
			if symbol != "" {
				s.tradeGapLost[symbol] = true
			}
		}
	case stream.DataDelayed:
		if eh != nil {
			eh.Delayed = true
		}
	case stream.DataCurrent:
		if eh != nil {
			eh.Delayed = false
		}
	case stream.ConnectionUnreliable:
		if eh != nil {
			eh.Unreliable = true
		}
	case stream.ConnectionStable:
		if eh != nil {
			eh.Unreliable = false
		}
	}
}

func (s *Store) markTradeGapLost(n stream.Notice) {
	symbol, _ := n.Details["symbol"].(string)
	if symbol == "" {
		return
	}
	s.tradeGapLost[symbol] = true
	// The snapshot latch is only for a notice that PRECEDES the gap snapshot: the
	// disabled/unavailable paths emit before it with no BACKFILL_START, so no gap
	// is pending here. An async patch failure instead arrives AFTER its snapshot
	// while its own BACKFILL_START is still pending — there the pending count
	// already shielded that snapshot, so latching would wrongly consume a later
	// clean snapshot and leave the tick Neutral one reconnect too long.
	if s.tradeGapPending[symbol] == 0 {
		s.tradeGapSnapshot[symbol] = true
	}
}

func publicTradeGapNotice(n stream.Notice) bool {
	endpoint, _ := n.Details["endpoint"].(string)
	channel, _ := n.Details["channel"].(string)
	reason, _ := n.Details["reason"].(string)
	return endpoint == "public" && channel == stream.ChannelTrade && reason == "gap"
}

// log is the store's operational logger, resolved once at New (never nil).
// Debug-only: the reconcile decisions that drive correctness.
func (s *Store) log() *slog.Logger { return s.lg }

func (s *Store) endpointHealth(endpoint string) *EndpointHealth {
	switch endpoint {
	case "public":
		return &s.health.Public
	case "private":
		return &s.health.Private
	}
	return nil
}

// privateFeedDown reports a KNOWN-down private endpoint (a DISCONNECTED has
// been applied with no CONNECTED after it). It gates the order/balance READY
// latches: a snapshot goroutine outliving its connection (the REST fetch is
// transport-independent and can complete up to its retry budget after the
// drop) lands here AFTER the disconnect cleared the latches but BEFORE the
// next CONNECTED bumps the epoch, so it still passes the gen >= epoch guard —
// its rows and reconcile are valid point-in-time data and are applied, but it
// must not re-mark the panes ready while the feed is down ("loading again"
// is the disconnect contract, see the Disconnected handler). Unknown health
// (no notice seen yet) does not block: data events always follow their
// endpoint's CONNECTED in a real session's FIFO.
func (s *Store) privateFeedDown() bool {
	return s.health.Private.Known && !s.health.Private.Up
}

// --- ticker / orderbook ---

func (s *Store) applyTicker(e stream.Data) {
	var frame struct {
		Data Ticker `json:"data"`
	}
	if json.Unmarshal(e.Payload, &frame) != nil || frame.Data.Close == "" {
		return
	}
	cur, ok := s.tickers[e.Symbol]
	if ok && e.ServerTime < cur.ServerTime {
		return
	}
	t := frame.Data
	t.Symbol = e.Symbol
	t.ServerTime = e.ServerTime
	s.tickers[e.Symbol] = t
	s.tickerReady[e.Symbol] = true
}

func (s *Store) applyOrderbook(e stream.Data) {
	var frame struct {
		Data struct {
			Timestamp int64        `json:"timestamp"`
			Bids      []PriceLevel `json:"bids"`
			Asks      []PriceLevel `json:"asks"`
		} `json:"data"`
	}
	if json.Unmarshal(e.Payload, &frame) != nil {
		return
	}
	cur, ok := s.books[e.Symbol]
	if ok && e.ServerTime < cur.ServerTime {
		return
	}
	s.books[e.Symbol] = Orderbook{
		Symbol:     e.Symbol,
		Timestamp:  frame.Data.Timestamp,
		Bids:       frame.Data.Bids,
		Asks:       frame.Data.Asks,
		ServerTime: e.ServerTime,
	}
	s.bookReady[e.Symbol] = true
}

// --- public trades ---

func (s *Store) applyTrade(e stream.Data) {
	// A WS frame (snapshot or live) for the symbol proves the feed is current —
	// mark ready before the empty-frame return so a dead pair's empty snapshot
	// shows "no trades yet", not a perpetual "loading…". Backfill payloads (the
	// gap patch / history seed) deliver history and prove nothing about the live
	// feed — they land asynchronously and can even arrive AFTER a disconnect
	// already cleared the latch, where setting it would present a dropped feed
	// as current — so they never latch. Nothing is lost: every backfill is
	// preceded by the WS snapshot that latched readiness while connected.
	if e.Origin != stream.OriginBackfill {
		s.tradeReady[e.Symbol] = true
	}

	snapshot := e.Origin == stream.OriginSnapshot
	// The one-shot marker set by a BACKFILL_DISABLED/FAILED gap notice: the
	// snapshot it precedes carries the gap tip, not a clean baseline, so it must
	// never clear the lost latch. Consume it here whether or not the snapshot has
	// rows (an empty gap snapshot still isn't a recovery).
	gapSnapshot := snapshot && s.tradeGapSnapshot[e.Symbol]
	if gapSnapshot {
		delete(s.tradeGapSnapshot, e.Symbol)
	}

	rows := tradeRows(e)
	if len(rows) == 0 {
		// An empty (all-duplicate) resubscribe snapshot adds no new trade, so it
		// cannot re-establish a trustworthy predecessor chain — leave every latch
		// as-is. Clearing tradeGapLost here would expose the stale direction the
		// unrecovered gap poisoned.
		return
	}
	r := s.trades[e.Symbol]
	if r == nil {
		r = newTradeRing(s.cfg.TradeCap)
		s.trades[e.Symbol] = r
	}
	ts := s.tick[e.Symbol]
	belowTip := false
	snap := make([]Trade, 0, len(rows)) // this frame's own rows, for a clean rebase
	for _, row := range rows {
		var t Trade
		if json.Unmarshal(row, &t) != nil || t.TradeID == 0 {
			continue
		}
		if t.TradeID <= ts.id {
			belowTip = true // a backfill/gap row that may change the tip's predecessor
		}
		r.insert(t)
		snap = append(snap, t)
	}

	// A clean resubscribe snapshot (real rows, not the gap tip, no patch pending)
	// re-establishes a trustworthy recent window after an unrecovered gap. Rebase
	// the tick on this snapshot's OWN contiguous window rather than folding onto
	// the pre-gap tip or scanning the retained ring: either would let a stale
	// pre-gap row remain the tip's predecessor (a same-price fold carries the old
	// direction; a rescan compares across the hole). Then drop the lost latch.
	if snapshot && !gapSnapshot && s.tradeGapPending[e.Symbol] == 0 && s.tradeGapLost[e.Symbol] && len(snap) > 0 {
		delete(s.tradeGapLost, e.Symbol)
		s.tick[e.Symbol] = tickFromRows(snap)
		return
	}

	if belowTip {
		s.tick[e.Symbol] = rescanTickState(r) // the running fold can't be trusted
	} else {
		s.tick[e.Symbol] = advanceTickState(ts, r) // live/forward: O(1) per new trade
	}
}

// advanceTickState advances ts over the ring's trades newer than its tip. Live
// frames are strictly forward (the feed is deduped against a high-water mark),
// so each new trade's predecessor is the running tip: an equal price carries the
// prior direction, a differing one sets it.
func advanceTickState(ts tickState, r *tradeRing) tickState {
	for _, t := range r.rows { // ascending by TradeID
		if t.TradeID <= ts.id {
			continue
		}
		if ts.id != 0 {
			switch cmpPrice(t.Price, ts.price) {
			case 1:
				ts.dir = Up
			case -1:
				ts.dir = Down
			}
		}
		ts.price, ts.id = t.Price, t.TradeID
	}
	return ts
}

// rescanTickState recomputes the tip from scratch over the whole retained ring.
// Used when a backfill inserts at or below the tip, where the running fold no
// longer reflects the true predecessor.
func rescanTickState(r *tradeRing) tickState { return tickFromRows(r.rows) }

// tickFromRows computes a tick from an ascending-by-TradeID window: the newest
// trade's price vs the nearest earlier trade at a different price (a single
// trade, or a whole window at one price, is Neutral). rows need not be sorted —
// it is defensively sorted so a snapshot frame's payload order cannot mislead
// the scan. The caller chooses the window: the full ring (rescan) or a single
// snapshot frame (a clean rebase after a lost gap, where retained pre-gap rows
// must NOT serve as the predecessor).
func tickFromRows(rows []Trade) tickState {
	n := len(rows)
	if n == 0 {
		return tickState{}
	}
	if !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].TradeID < rows[j].TradeID }) {
		rows = append([]Trade(nil), rows...)
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].TradeID < rows[j].TradeID })
	}
	tip := rows[n-1]
	ts := tickState{id: tip.TradeID, price: tip.Price}
	for i := n - 2; i >= 0; i-- {
		switch cmpPrice(tip.Price, rows[i].Price) {
		case 1:
			ts.dir = Up
			return ts
		case -1:
			ts.dir = Down
			return ts
		}
	}
	return ts
}

// cmpPrice compares two decimal price strings numerically (-1, 0, +1). A value
// that does not parse compares equal, so a bad row never fabricates a tick.
func cmpPrice(a, b string) int {
	da, err1 := decimal.NewFromString(a)
	db, err2 := decimal.NewFromString(b)
	if err1 != nil || err2 != nil {
		return 0
	}
	return da.Cmp(db)
}

// tradeRows extracts the trade rows from either payload shape: the WS frame
// ({"data": [...]}) or a /v2/trades backfill payload (a bare array).
func tradeRows(e stream.Data) []json.RawMessage {
	if e.Origin == stream.OriginBackfill {
		var rows []json.RawMessage
		if json.Unmarshal(e.Payload, &rows) != nil {
			return nil
		}
		return rows
	}
	var frame struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(e.Payload, &frame) != nil {
		return nil
	}
	return frame.Data
}

// tradeRing keeps the newest cap trades by tradeId, deduped.
type tradeRing struct {
	cap  int
	rows []Trade // sorted ascending by TradeID
	ids  map[int64]struct{}
}

func newTradeRing(cap int) *tradeRing {
	return &tradeRing{cap: cap, ids: map[int64]struct{}{}}
}

func (r *tradeRing) insert(t Trade) {
	if _, dup := r.ids[t.TradeID]; dup {
		return
	}
	i := sort.Search(len(r.rows), func(i int) bool { return r.rows[i].TradeID >= t.TradeID })
	r.rows = append(r.rows, Trade{})
	copy(r.rows[i+1:], r.rows[i:])
	r.rows[i] = t
	r.ids[t.TradeID] = struct{}{}
	if len(r.rows) > r.cap {
		delete(r.ids, r.rows[0].TradeID)
		r.rows = r.rows[1:]
	}
}

func (r *tradeRing) newestFirst() []Trade {
	out := make([]Trade, len(r.rows))
	for i, t := range r.rows {
		out[len(r.rows)-1-i] = t
	}
	return out
}

// since returns the rows with TradeID > afterID, oldest first (rows are already
// sorted ascending). A binary search finds the boundary, so the common
// "nothing new" case is O(log n) and allocates nothing.
func (r *tradeRing) since(afterID int64) []Trade {
	i := sort.Search(len(r.rows), func(i int) bool { return r.rows[i].TradeID > afterID })
	if i >= len(r.rows) {
		return nil
	}
	out := make([]Trade, len(r.rows)-i)
	copy(out, r.rows[i:])
	return out
}

// --- orders ---

// orderRow covers the union of the WS myOrder row, the /v2/openOrders row,
// and the /v2/allOrders row. Pointer fields distinguish "absent" from "empty"
// so a sparser source never blanks a field a richer one already set.
type orderRow struct {
	OrderID       int64   `json:"orderId"`
	ClientOrderID *string `json:"clientOrderId"`
	Symbol        string  `json:"symbol"` // allOrders rows only
	Side          *string `json:"side"`
	OrderType     *string `json:"orderType"`
	Price         *string `json:"price"`
	Qty           *string `json:"qty"`
	Amt           *string `json:"amt"`
	FilledQty     *string `json:"filledQty"`
	FilledAmt     *string `json:"filledAmt"`
	AvgPrice      *string `json:"avgPrice"`
	Status        *string `json:"status"`
	CreatedAt     int64   `json:"createdAt"`
	LastFilledAt  int64   `json:"lastFilledAt"`
}

func (s *Store) applyMyOrder(e stream.Data) {
	gen := s.genOf(e)
	if e.Origin == stream.OriginBackfill {
		var rows []orderRow
		if json.Unmarshal(e.Payload, &rows) != nil {
			return
		}
		// The backfill call's accountSeq (the bare-array rows don't echo it).
		seq := effAccountSeq(e.AccountSeq)
		if e.Source == "/v2/openOrders" {
			// A snapshot from a SUPERSEDED connection (gen < s.epoch — a slow REST
			// that landed after a newer reconnect) is obsolete: its open-set view
			// could wrongly revive a placeholder-closed order or false-close a
			// current one. Skip it whole — the current connection's snapshot is the
			// authority. (Its terminal truth, if any, still arrives via /v2/allOrders.)
			if gen < s.epoch {
				return
			}
			// Current generation. Newest-wins among same-generation snapshots (an
			// on-demand fetch that landed after a fresher one) so a stale on-demand
			// snapshot can't resurrect or wrongly close orders. Per {account, symbol}
			// because /v2/openOrders is fetched per sub-account with independent
			// ServerTimes. Then mark this {account, symbol} ready.
			k := orderSnapKey{accountSeq: seq, symbol: e.Symbol}
			if e.ServerTime < s.orderSnapAt[k] {
				return
			}
			s.orderSnapAt[k] = e.ServerTime
			if !s.privateFeedDown() {
				s.orderReady[k] = true
			}
		}
		for _, row := range rows {
			s.upsertOrder(row, e.Symbol, seq, gen)
		}
		if e.Source == "/v2/openOrders" {
			s.reconcileOpenSnapshot(rows, e.Symbol, seq)
		}
		return
	}
	var frame struct {
		Order struct {
			AccountSeq *int       `json:"accountSeq"`
			Orders     []orderRow `json:"orders"`
		} `json:"order"`
	}
	if json.Unmarshal(e.Payload, &frame) != nil {
		return
	}
	seq := effAccountSeq(frame.Order.AccountSeq)
	for _, row := range frame.Order.Orders {
		s.upsertOrder(row, e.Symbol, seq, gen)
	}
}

// upsertOrder merges one source row into the order keyed by its id. Identity
// fields (side, price, qty, amt, clientOrderId, createdAt, symbol) never change
// over an order's life and are filled from any source that carries them.
// Lifecycle fields (status, filledQty, filledAmt, avgPrice, lastFilledAt)
// advance only when the incoming state supersedes the stored one by the order's
// intrinsic monotonic progress — NOT by any transport timestamp (see
// orderProgress / supersedes, the single source of truth). The order is stamped
// with the connection generation the event came from (gen — never DOWNGRADED, so
// a stale backfill row can't lower an order already confirmed in a newer
// connection), which the open-orders snapshot reconcile uses to tell a gap-closed
// order from one seen in the current connection, and with the sub-account it
// belongs to (accountSeq; 1 = main).
func (s *Store) upsertOrder(row orderRow, symbol string, accountSeq int, gen int64) {
	if row.OrderID == 0 {
		return
	}
	status := ""
	if row.Status != nil {
		status = normalizeStatus(*row.Status)
	}

	// Observing the order releases its local hold, whatever the row's status
	// or source: from this point the balance stream is the authority on the
	// order's reservation (see localhold.go).
	if row.ClientOrderID != nil && *row.ClientOrderID != "" {
		s.releaseHold(holdKey{accountSeq: accountSeq, clientOrderID: *row.ClientOrderID})
	}

	o, exists := s.orders[row.OrderID]
	if !exists {
		o = Order{OrderID: row.OrderID}
	}
	wasTerminal := o.Terminal()

	// Identity/immutable fields: fill from any source, regardless of ordering —
	// they are constant for an order, so a sparse source never blanks them and a
	// stale one never corrupts them.
	if symbol != "" {
		o.Symbol = symbol
	} else if row.Symbol != "" {
		o.Symbol = row.Symbol
	}
	o.AccountSeq = accountSeq
	setIf(&o.ClientOrderID, row.ClientOrderID)
	setIf(&o.Side, row.Side)
	setIf(&o.OrderType, row.OrderType)
	setIf(&o.Price, row.Price)
	setIf(&o.Qty, row.Qty)
	setIf(&o.Amt, row.Amt)
	if row.CreatedAt > 0 {
		o.CreatedAt = row.CreatedAt
	}

	// Lifecycle fields: apply only when the incoming state is at least as
	// advanced as what we hold. A new order applies unconditionally.
	incomingFilled := o.FilledQty
	if row.FilledQty != nil {
		incomingFilled = *row.FilledQty
	}
	incoming := orderProgress{status: status, filledQty: incomingFilled}
	if !exists || incoming.supersedes(orderProgress{status: o.Status, filledQty: o.FilledQty}) {
		if status != "" {
			o.Status = status
		}
		setIf(&o.FilledQty, row.FilledQty)
		setIf(&o.FilledAmt, row.FilledAmt)
		setIf(&o.AvgPrice, row.AvgPrice)
		if row.LastFilledAt > 0 {
			o.LastFilledAt = row.LastFilledAt
		}
	}

	if gen > o.epoch {
		o.epoch = gen
	}
	switch {
	case o.Terminal() && !wasTerminal:
		// The transition INTO terminal is the observation ClosedAt records; a
		// repeated terminal row never moves it (see the field comment).
		o.ClosedAt = s.now()
	case !o.Terminal():
		// Open (or unknown) — including a synthetic gap-close revived by a
		// live frame. Clear any stale stamp; it re-arms on the real close.
		o.ClosedAt = 0
	}
	s.orders[row.OrderID] = o
}

// reconcileOpenSnapshot applies the disappearance inference from a /v2/openOrders
// payload: an open order for this {account, symbol} that is absent from the
// snapshot AND was last known from a STRICTLY EARLIER private connection (a lower
// epoch) must have closed while disconnected. Its real terminal status is unknown
// until a history row delivers it, so it becomes StatusClosedUnknown (which any
// real status replaces). The snapshot is authoritative only for its OWN
// sub-account, so the sweep is scoped to that accountSeq — one account's snapshot
// must never close another account's orders for the same symbol.
//
// The epoch test — not a timestamp — is what makes this correct. The snapshot is
// NOT used as a same-connection "refresh close" mechanism: an order first seen
// in THIS connection (equal epoch) is left alone even when absent. While the
// private feed is up, a real close should arrive as a live terminal myOrder frame;
// for a close that straddled the reconnect but whose order a stale live frame
// already lifted to this epoch, the /v2/allOrders gap row supersedes. So this
// snapshot only infers cross-disconnect closes, with an exact integer comparison
// that no clock skew can perturb. (The snapshot's own rows were upserted just
// above, lifting present orders to the current epoch, so they are never mistaken
// for gap-closed.)
func (s *Store) reconcileOpenSnapshot(rows []orderRow, symbol string, accountSeq int) {
	present := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		present[row.OrderID] = struct{}{}
	}
	for id, o := range s.orders {
		if o.AccountSeq != accountSeq || o.Symbol != symbol || !o.Open() || o.epoch >= s.epoch {
			continue
		}
		if _, ok := present[id]; ok {
			continue
		}
		// A cross-disconnect close: an order open in a prior epoch is absent from
		// this connection's authoritative snapshot. Mark it the synthetic "closed"
		// until a history row delivers the real terminal status. Debug — this drives
		// the materialized view's correctness.
		s.log().Debug("state order gap-closed",
			"orderId", id, "symbol", symbol, "accountSeq", accountSeq,
			"priorEpoch", o.epoch, "epoch", s.epoch)
		o.Status = StatusClosedUnknown
		o.epoch = s.epoch
		o.ClosedAt = s.now() // first terminal observation (see Order.ClosedAt)
		s.orders[id] = o
	}
}

// normalizeStatus maps the WS myOrder spelling onto the REST one: the WS
// "unfilled" status is documented as the same state as REST "open".
func normalizeStatus(status string) string {
	if status == "unfilled" {
		return "open"
	}
	return status
}

func setIf(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}

// --- fills ---

// fillRow covers the WS myTrade row (fee, filledAt) and the REST /v2/myTrades
// row (feeQty, tradedAt, symbol).
type fillRow struct {
	TradeID     int64  `json:"tradeId"`
	OrderID     int64  `json:"orderId"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	Price       string `json:"price"`
	Qty         string `json:"qty"`
	Fee         string `json:"fee"`
	FeeQty      string `json:"feeQty"`
	FeeCurrency string `json:"feeCurrency"`
	IsTaker     bool   `json:"isTaker"`
	FilledAt    int64  `json:"filledAt"`
	TradedAt    int64  `json:"tradedAt"`
}

func (s *Store) applyMyTrade(e stream.Data) {
	var rows []fillRow
	// Backfill: the call's accountSeq. Live: the frame wrapper's accountSeq.
	seq := effAccountSeq(e.AccountSeq)
	if e.Origin == stream.OriginBackfill {
		if json.Unmarshal(e.Payload, &rows) != nil {
			return
		}
	} else {
		var frame struct {
			Trade struct {
				AccountSeq *int      `json:"accountSeq"`
				Trades     []fillRow `json:"trades"`
			} `json:"trade"`
		}
		if json.Unmarshal(e.Payload, &frame) != nil {
			return
		}
		rows = frame.Trade.Trades
		seq = effAccountSeq(frame.Trade.AccountSeq)
	}
	for _, row := range rows {
		if row.TradeID == 0 {
			continue
		}
		f := Fill{
			TradeID:     row.TradeID,
			OrderID:     row.OrderID,
			AccountSeq:  seq,
			Symbol:      row.Symbol,
			Side:        row.Side,
			Price:       row.Price,
			Qty:         row.Qty,
			Fee:         row.Fee,
			FeeCurrency: row.FeeCurrency,
			IsTaker:     row.IsTaker,
			Time:        row.FilledAt,
		}
		if f.Symbol == "" {
			f.Symbol = e.Symbol
		}
		if f.Fee == "" {
			f.Fee = row.FeeQty
		}
		if f.Time == 0 {
			f.Time = row.TradedAt
		}
		s.insertFill(f)
	}
}

func (s *Store) insertFill(f Fill) {
	k := fillKey{symbol: f.Symbol, id: f.TradeID}
	if _, dup := s.fillIDs[k]; dup {
		return
	}
	s.fillIDs[k] = struct{}{}
	// Insert keeping newest-first order (by time, then tradeId). Fills arrive
	// nearly ordered, so the scan is short.
	i := 0
	for i < len(s.fills) {
		if f.Time > s.fills[i].Time ||
			(f.Time == s.fills[i].Time && f.TradeID > s.fills[i].TradeID) {
			break
		}
		i++
	}
	s.fills = append(s.fills, Fill{})
	copy(s.fills[i+1:], s.fills[i:])
	s.fills[i] = f
	if len(s.fills) > s.cfg.FillCap {
		drop := s.fills[len(s.fills)-1]
		delete(s.fillIDs, fillKey{symbol: drop.Symbol, id: drop.TradeID})
		s.fills = s.fills[:len(s.fills)-1]
	}
}

// --- balances ---

// balanceRow covers the WS myAsset row and the REST /v2/balance row (same
// names; REST has no updatedAt).
type balanceRow struct {
	Currency        string `json:"currency"`
	Balance         string `json:"balance"`
	Available       string `json:"available"`
	TradeInUse      string `json:"tradeInUse"`
	WithdrawalInUse string `json:"withdrawalInUse"`
	AvgPrice        string `json:"avgPrice"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// balanceSupersedes decides — WITHOUT any transport timestamp — whether an
// incoming balance row should overwrite the stored one. A balance carries no
// intrinsic monotonic progress (unlike an order's lifecycle), so ordering keys
// on the private-connection epoch plus origin:
//
//   - A newer connection (higher epoch) always wins: its reconnect /v2/balance
//     snapshot re-baselines everything known from an older connection.
//   - Within one connection (equal epoch) a live myAsset frame supersedes the
//     reconnect REST snapshot and the snapshot never clobbers a value a live
//     frame already set — so a snapshot fetched at connect-start can't overwrite
//     a fresher live delta that landed first.
//   - Equal epoch and same origin class: last applied wins (live frames arrive in
//     server order on a lossless connection; only one REST snapshot is taken per
//     connect).
//
// The incoming epoch is the event's own generation (genOf): a live frame reads as
// the current epoch, a backfill carries the generation it was fetched in. So a
// snapshot from a superseded connection (incEpoch < stored.epoch) correctly fails
// to supersede a value the current connection already set — no REST-fetch-time
// guard (the orderSnapAt analogue) is needed.
func balanceSupersedes(incEpoch int64, incLive bool, stored Balance) bool {
	if incEpoch != stored.epoch {
		return incEpoch > stored.epoch
	}
	if incLive != stored.live {
		return incLive // live beats snapshot; snapshot never beats live
	}
	return true
}

func (s *Store) applyMyAsset(e stream.Data) {
	var rows []balanceRow
	// Backfill: the call's accountSeq. Live: the frame wrapper's accountSeq.
	seq := effAccountSeq(e.AccountSeq)
	if e.Origin == stream.OriginBackfill {
		if json.Unmarshal(e.Payload, &rows) != nil {
			return
		}
	} else {
		var frame struct {
			Asset struct {
				AccountSeq *int         `json:"accountSeq"`
				Assets     []balanceRow `json:"assets"`
			} `json:"asset"`
		}
		if json.Unmarshal(e.Payload, &frame) != nil {
			return
		}
		rows = frame.Asset.Assets
		seq = effAccountSeq(frame.Asset.AccountSeq)
	}
	live := e.Origin != stream.OriginBackfill
	gen := s.genOf(e)
	for _, row := range rows {
		if row.Currency == "" {
			continue
		}
		k := balKey{accountSeq: seq, currency: row.Currency}
		if cur, ok := s.bals[k]; ok && !balanceSupersedes(gen, live, cur) {
			continue
		}
		s.bals[k] = Balance{
			AccountSeq:      seq,
			Currency:        row.Currency,
			Balance:         row.Balance,
			Available:       row.Available,
			TradeInUse:      row.TradeInUse,
			WithdrawalInUse: row.WithdrawalInUse,
			AvgPrice:        row.AvgPrice,
			UpdatedAt:       row.UpdatedAt,
			epoch:           gen,
			live:            live,
		}
	}
	// A current-generation /v2/balance snapshot is authoritative for its whole
	// sub-account (the session always fetches it unfiltered), so it also carries
	// disappearance information: sweep it, and only THEN are balances provably
	// complete — an empty snapshot legitimately means "no balances", so the
	// ready latch ignores row count. A live myAsset delta must NOT latch: it
	// proves the feed is live and its own currencies current, but nothing about
	// the rest of the set — after a reconnect it can arrive before the snapshot,
	// when prior-epoch balances zeroed during the disconnect are still un-swept,
	// and if the snapshot then fails the stale set would sit behind a true
	// BalancesReady until the next reconnect. A stale snapshot from a superseded
	// connection (gen < s.epoch) must neither sweep nor mark ready.
	if gen >= s.epoch && !live {
		s.reconcileBalanceSnapshot(rows, seq, gen)
		if !s.privateFeedDown() {
			s.balReady[seq] = true
		}
	}
}

// reconcileBalanceSnapshot applies the disappearance half of an authoritative,
// unfiltered /v2/balance snapshot: a balance for this sub-account absent from
// it that was last set in a STRICTLY EARLIER private connection (a lower epoch)
// is no longer held — its zeroing myAsset frame was lost with the old
// connection — so it is dropped. This is the balance analogue of
// reconcileOpenSnapshot, and deliberately conservative the same way: a balance
// already set in the CURRENT connection (equal epoch) is spared even when
// absent, because within a connection the lossless live feed is the authority
// and the snapshot may simply have been fetched before the currency's first
// live delta. The server's row universe is not a guaranteed contract (a
// zeroed currency may or may not appear as a zero row), and this sweep is
// correct either way: a returned zero row re-baselines via the normal upsert,
// an omitted one is removed here.
func (s *Store) reconcileBalanceSnapshot(rows []balanceRow, accountSeq int, gen int64) {
	present := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		present[row.Currency] = struct{}{}
	}
	for k, b := range s.bals {
		if k.accountSeq != accountSeq || b.epoch >= gen {
			continue
		}
		if _, ok := present[k.currency]; ok {
			continue
		}
		// A cross-disconnect disappearance: drop the stale balance rather than
		// keep presenting a holding the account no longer has.
		s.log().Debug("state balance gap-dropped",
			"currency", k.currency, "accountSeq", accountSeq,
			"priorEpoch", b.epoch, "epoch", gen)
		delete(s.bals, k)
	}
}
