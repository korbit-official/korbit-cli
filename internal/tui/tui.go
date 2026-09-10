// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package tui is the interactive trading terminal behind the `tui` command: a
// full-screen view of live market data (ticker, orderbook, trades) and the
// account's orders, fills, and balances, materialized from a stream.Session's
// events by internal/stream/state and kept current through reconnects and
// REST backfill by the stream layer.
//
// Division of labor: the stream package delivers events and reliability
// notices, the state package answers "what is true right now", and this
// package only renders and routes input. Order placement and cancellation go
// through the Trader interface, implemented by the cli package over the same
// validated, journaled request path as `order place` / `order cancel` — the
// TUI never signs or sends anything itself.
//
// Concurrency model: one pump goroutine forwards session events into the
// Bubble Tea program (Program.Send is safe from other goroutines and a no-op
// after exit); the state.Store is mutated only inside Update, so it needs no
// locking. Trader calls run inside tea commands (the runtime's goroutines)
// with an in-flight guard so only one order action runs at a time.
package tui

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/korbit-official/korbit-cli/internal/candles"
	"github.com/korbit-official/korbit-cli/internal/envalias"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

// OrderForm is one order entry from the order panel. Values are the user's
// decimal strings, passed through untouched; empty fields are omitted from
// the request. Type "best" is not offered by the panel (it needs the bestNth
// level and a mandatory time-in-force — use `order place`).
type OrderForm struct {
	Symbol string
	Side   string // buy | sell
	Type   string // limit | market
	Price  string // limit only
	Qty    string // limit, and market sell
	Amt    string // market buy
	TIF    string // time-in-force, always set (gtc/ioc/fok/po; a market order is ioc)
	PP     bool   // price protection (market orders): cap taker fills near the mid
	// AccountSeq is the sub-account the order is placed under (>= 1). Stamped
	// by the model's single dispatch point (dispatchPlaceForm) with the active
	// account — never defaulted below the TUI, so the Trader always receives it
	// explicitly.
	AccountSeq int
	// ClientOrderID is the placement's idempotency key, minted by
	// dispatchPlaceForm before the order leaves the TUI so the model can key
	// the order's local balance hold (state.AddLocalHold) on it. The Trader
	// must send it as the order's clientOrderId (minting its own only when
	// empty, for callers other than the model).
	ClientOrderID string
}

// PlaceResult is the accepted order's identifiers. Warning carries a
// non-fatal post-acceptance problem (the order IS live): a full-screen program
// can't surface it as a printed result line plus a non-zero exit, so the TUI
// shows the warning loudly in-UI instead.
type PlaceResult struct {
	OrderID       string
	ClientOrderID string
	Warning       string
}

// Trader places and cancels orders. The cli implementation validates with
// the same cross-field rules as `order place`, journals the action, and
// sends the signed request; a validation failure comes back as a
// *output.UsageError so the form can show it inline. Cancel's warning has
// the same meaning as PlaceResult.Warning (the cancel was accepted).
// The sub-account is explicit on every call (OrderForm.AccountSeq / Cancel's
// accountSeq): a cancel targets the ORDER'S OWN account (from the store row),
// which need not be the account new orders are placed under. Implementations
// must be safe to call from a non-UI goroutine (calls are serialized by the
// UI).
type Trader interface {
	Place(t OrderForm) (PlaceResult, error)
	Cancel(symbol string, orderID int64, accountSeq int) (warning string, err error)
}

// Config describes one TUI session.
type Config struct {
	// Symbols are the trading pairs to watch (normalized, at least one).
	Symbols []string
	// Events is the stream session's output; the TUI consumes it until closed.
	Events <-chan stream.Event
	// StopSession cancels the stream session; called when the TUI exits (and
	// must be idempotent).
	StopSession func()
	// Private says the account channels (myOrder/myTrade/myAsset) are
	// subscribed, which selects the account-state panels.
	Private bool
	// AccountSeq is the sub-account the session starts active on — displayed,
	// tracked, and placed under until the user switches (the '@' account
	// switcher). The store's readiness and per-account reads are keyed by the
	// active account; the model reads it through model.accountSeq() (which also
	// normalizes zero to 1/main), and the order/ladder/funding sub-models capture
	// the active value, re-stamped on a switch. Defaults to 1 (main) when zero.
	AccountSeq int
	// AccountSeqs are all the sub-accounts this session subscribed (the account
	// channels cover every one), and the set the '@' switcher offers. AccountSeq
	// is one of them (the initial active). Empty/one-element means a
	// single-account session — the switcher is inert and its footer hint hidden.
	// The cli layer fills this from --account-seq (an explicit list) or, when
	// omitted, the key's allowedAccountSeqs.
	AccountSeqs []int
	// Trader enables the order panel and order cancellation; nil (public
	// mode) disables order entry.
	Trader Trader
	// SetActiveMarket re-subscribes the active symbol's market-data channels
	// (orderbook + trades) when the user switches symbols in the sidebar. It is
	// called with the previous and next symbol whenever the active symbol
	// actually changes; nextLevel is the next symbol's chosen orderbook
	// grouping level ("" = the raw book), so a switch-back restores the pair's
	// grouping. nil disables dynamic switching (the initial symbol's
	// market data is whatever was subscribed up front). Must be safe to call
	// from the UI goroutine and must not block (the cli wires it to the stream
	// session's non-blocking Subscribe/Unsubscribe).
	SetActiveMarket func(prev, next, nextLevel string)
	// SetOrderbookLevel re-points the active symbol's orderbook subscription
	// at a grouping level ("" = the raw book) without touching the trade
	// channel. The wire order is unsubscribe-then-subscribe: the stream
	// registry (and the server) match an unsubscribe by channel+symbol
	// regardless of level, so subscribing the new level first would be undone
	// by the old level's unsubscribe. nil disables grouping (the +/- keys
	// toast). Must be safe to call from the UI goroutine and must not block.
	SetOrderbookLevel func(symbol, level string)
	// SetTrackedOrders sets which {account, symbol} scopes' open-order
	// snapshots the session keeps authoritative (the stream session's
	// LazyOpenOrders mode): the active pair under the active account in the
	// default per-pair view, or every watched pair in the "all pairs" view.
	// Called on an active-symbol switch and on the view toggle. Must be safe to
	// call from the UI goroutine and must not block. nil disables the lazy
	// behavior (every subscribed myOrder symbol stays snapshotted).
	SetTrackedOrders func(scopes []stream.OrderScope)
	// Funding enables the deposit/withdrawal screen (the 'f' key): list
	// currencies with balances, show deposit addresses and transfer histories,
	// and request transfers. nil (public mode) disables the screen.
	Funding Funding
	// TickSizePolicy fetches a symbol's tick metadata (a public read): the
	// tick-size bands and the valid orderbook grouping levels. The bands back
	// the order panel's price stepper, snapping, and off-grid warnings; the
	// levels back the +/- orderbook grouping keys. nil (or a failed fetch)
	// leaves the panel without a grid — typed prices only, the server still
	// validates — and grouping unavailable. Must be safe to call from a tea
	// command goroutine.
	TickSizePolicy func(symbol string) (TickPolicy, error)
	// OrderValueBounds fetches a pair's order value bounds and quote currency
	// from the public pair listing. They back the order panel's and the ladder's
	// below-min / above-max warnings, which are raised only against the bound the
	// pair itself publishes. nil (or a failed fetch) leaves those warnings out —
	// the server still rejects an out-of-bounds order — and never substitutes
	// another pair's figure. Must be safe to call from a tea command goroutine.
	OrderValueBounds func(symbol string) (ops.OrderValueBounds, error)
	// Fees fetches a sub-account's trading-fee policy for a symbol (a signed
	// read). It takes the account explicitly (the fee tier can differ per
	// sub-account) so it follows the active account across a switch; results
	// are cached per {account, symbol} for the session. It backs the order
	// panel's fee estimate, the buy presets' quote-fee headroom, and the
	// placed order's local balance hold — so when the seam is wired, arming
	// an order is gated until the active pair's policy has loaded (fetched
	// async and retried; the gate says why). nil disables all of it, gate
	// included. Must be safe to call from a tea command goroutine.
	Fees func(symbol string, accountSeq int) (FeeRates, error)
	// Candles fetches up to limit OHLC bars for symbol at interval (the candles
	// endpoint's interval enum: "1","5","15","30","60","240","1D","1W"), oldest
	// first. endMs bounds the newest bar (unix ms, exclusive of later buckets);
	// 0 means "up to now", so the current still-open bucket comes last. A non-zero
	// endMs pages back into history for scroll-back backfill. It backs the candle
	// chart (the 'g' overlay); nil disables the chart. Wired to the candles
	// operation in the cli layer; must be safe to call from a tea command
	// goroutine.
	Candles func(symbol, interval string, limit int, endMs int64) ([]candles.Bar, error)
	// KeyName is the signing key's name, for the header ("" in public mode).
	KeyName string
	// BaseURL is shown in the header so a sandbox session is unmistakable.
	BaseURL string
	// ColorScheme is the initial up/down color convention ("green-red" or
	// "red-blue"); "" or an unrecognized value starts at the default. The cli
	// layer passes the value persisted in config.json.
	ColorScheme string
	// OrderLevels are the %-of-balance size presets the order UIs bind to number
	// keys (e.g. [10,25,50,100], 100 → "max"). The cli layer passes the levels
	// pinned in config.json; empty falls back to the built-in defaults. Pinning
	// keeps a preset key stable across program updates so muscle memory holds.
	OrderLevels []int
	// SaveColorScheme persists the color scheme so it survives the next launch.
	// It is called once, when the TUI exits (Run), with the final scheme's name
	// ("green-red"/"red-blue") and only when it differs from ColorScheme — never
	// on the UI goroutine and never per keypress, so a file write can't stall
	// rendering and rapid toggles can't race. A returned error is best-effort
	// (the caller logs it); it does not change the exit outcome. nil disables
	// persistence (tests, and any caller that doesn't want it).
	SaveColorScheme func(scheme string) error
	// Out/In are the terminal streams the program runs on.
	Out io.Writer
	In  io.Reader
	// Now is the local clock (unix ms); nil = wall clock. Display only.
	Now func() int64
	// StoreLog is the optional logger for the materialized state Store (reconcile
	// mechanics, Debug; tagged component=stream/state). NoticeLog is the optional
	// logger for mirrored reliability notices (tagged component=stream kind=stream_notice).
	// Both nil = silent; the cli sets them only when --log-file diverts logs off
	// the alt-screen.
	StoreLog  *slog.Logger
	NoticeLog *slog.Logger

	// programOpts are appended to the Bubble Tea options (test seam).
	programOpts []tea.ProgramOption
}

// eventCoalesceMs bounds how often coalesced stream events are delivered to the
// program (≈16fps). Market data redraws at this rate at most; input is handled
// immediately between batches. Ticking numbers read fine at this rate, and the
// lower redraw frequency keeps steady-state CPU down; DIGITALX_CLI_TUI_COALESCE_MS
// overrides it.
const eventCoalesceMs = 60

// coalesceInterval is the stream-batch flush interval: eventCoalesceMs by
// default, overridable via DIGITALX_CLI_TUI_COALESCE_MS (milliseconds, clamped to
// 10..1000) to gauge how much of the redraw cost is frame-rate driven.
func coalesceInterval() time.Duration {
	ms := eventCoalesceMs
	if v := envalias.Lookup(os.Getenv, "DIGITALX_CLI_TUI_COALESCE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 && n <= 1000 {
			ms = n
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// Run starts the TUI and blocks until it exits. A deliberate quit returns
// nil; the caller owns reporting any session error after the screen is
// restored.
func Run(cfg Config) error {
	if len(cfg.Symbols) == 0 {
		return errors.New("tui: at least one symbol is required")
	}
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().UnixMilli() }
	}

	m := newModel(cfg)
	opts := append([]tea.ProgramOption{tea.WithOutput(cfg.Out), tea.WithInput(cfg.In)}, cfg.programOpts...)
	p := tea.NewProgram(m, opts...)

	// The pump forwards session events into the program, COALESCED: it
	// accumulates events and delivers them as one batch at a capped rate
	// (eventCoalesceMs ≈ 16fps) rather than one Program.Send per event. This is
	// load-bearing — bubbletea recomputes the whole View on every message
	// (Update then render per message), so a session subscribed to hundreds of
	// pairs would otherwise drive thousands of full-screen redraws per second on
	// the single event-loop goroutine and starve keyboard/mouse input (a hard
	// hang). Batching bounds the data-driven redraw rate independent of the
	// stream rate; a batch's events are applied to the store in order, so
	// ordering/dedup are unaffected. It stops when Events closes (session ended)
	// or when the program exits (progDone), so it can't block forever on an
	// Events channel that never closes.
	pumpDone := make(chan struct{})
	progDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		if cfg.Events == nil {
			return
		}
		ticker := time.NewTicker(coalesceInterval())
		defer ticker.Stop()
		var pending []stream.Event
		flush := func() {
			if len(pending) > 0 {
				p.Send(streamBatchMsg{evs: pending}) // no-op if the program already exited
				pending = nil
			}
		}
		for {
			select {
			case ev, ok := <-cfg.Events:
				if !ok {
					flush()
					p.Send(eventsClosedMsg{})
					return
				}
				pending = append(pending, ev)
			case <-ticker.C:
				flush()
			case <-progDone:
				return
			}
		}
	}()

	fm, err := p.Run()
	close(progDone)
	if cfg.StopSession != nil {
		cfg.StopSession() // ends the session; its Events channel then closes
	}
	<-pumpDone

	// Persist a color-scheme change once, on exit: the C-key toggle is applied
	// live during the session but written to disk only here, so the stored value
	// always matches the last scheme the user saw — with no per-keypress writes
	// and no ordering hazard from concurrent saves. Best-effort and cosmetic: a
	// write failure is swallowed here (the cli's SaveColorScheme closure logs it)
	// and never changes the exit outcome.
	if cfg.SaveColorScheme != nil {
		if fin, ok := fm.(model); ok {
			if name, changed := fin.persistColorScheme(); changed {
				_ = cfg.SaveColorScheme(name)
			}
		}
	}
	// A SIGINT delivered as an OS signal makes Run return ErrInterrupted (the
	// model never sees it); that is a deliberate stop, exit 0 — matching the
	// monitor command's contract. ErrProgramKilled is NOT treated as clean: it
	// can wrap a panic.
	if err != nil && !errors.Is(err, tea.ErrInterrupted) {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
