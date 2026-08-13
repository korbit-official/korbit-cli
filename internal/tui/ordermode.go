// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// Order mode ('b'/'s', modeOrder): a docked order-entry panel that
// replaces the right column while the orderbook stays live beside it as a
// price picker. It is a sub-model in the funding screen's mold — the parent
// owns only the mode switch, message/key routing, geometry, and the shared
// money-action single-flight — with a near one-key rule: ↑/↓ move the field
// cursor; ←/→ change the value under it on the selector rows (side/type/tif/pp
// cycle) but move the text caret on the numeric rows (price/qty/amt), whose
// value the dedicated keys step instead — the price on [ ]{ } (±1/±10 ticks)
// and the balance presets on % (both work from any row). Typing digits edits
// the input under the cursor, enter advances (review, then place). Deliberate additions
// for order entry speed: j/k walk the live orderbook's price levels (writing
// the level's price into the draft); b/s declare the side, re-seeding the
// price to the new side's own best level — pressing the current side's key
// again re-anchors there (join); a jumps the price to the opposite touch
// (cross); m/l anchor to mid/last. Those letters stay commands even while a
// numeric field holds the cursor — the price/qty/amt inputs accept digits and
// a dot only, so a letter is never text here (unlike the funding pane's
// free-form amount, where a stray command letter must stay text).
//
// The confirm step swaps the panel's content in place (no modal): the fully
// resolved order — snapped price, notional, fee estimate, the preplace
// warnings, the book's freshness — with the price still nudgeable on the tick
// grid ([ ] ±1, { } ±10) while armed. Placing swaps to a busy view; the result returns to the
// form (accepted: sticky for the next order, with the new order flashed in
// the open-orders panel below; rejected: the error inline, inputs preserved).

// orderView is which content the order panel is showing.
type orderView int

const (
	orderForm    orderView = iota // the draft form
	orderConfirm                  // the armed review (enter places)
	orderBusy                     // a placement is on the wire
)

// orderField is one row of the order form the field cursor can sit on.
type orderField int

const (
	ofSide orderField = iota
	ofType
	ofPrice
	ofQty
	ofAmt
	ofTIF
	ofPP
	ofButton
	ofNone orderField = -1 // a non-interactive line
)

// tifOptions is the time-in-force cycle for a limit order. A market order is
// ioc-only, so it does not cycle these; its effective tif is fixed by
// orderDraft.tif.
var tifOptions = []string{"gtc", "ioc", "fok", "po"}

// keyPreset is the key that cycles the %-of-balance size presets — shown as a
// cap in the panel's size hint and dispatched when that cap is clicked.
const keyPreset = "%"

// orderAction is what a handled key asks the parent to do.
type orderAction int

const (
	orderActNone  orderAction = iota
	orderActClose             // leave order mode
	orderActPlace             // dispatch draft.form() to the Trader
)

// orderModel is the order-entry sub-model. It reads the store directly (the
// parent mutates it; single goroutine) and caches the per-symbol market
// metadata its math needs — tick-size bands and fee rates — fetched async
// through the Config seams. Missing bands only degrade features (typed
// prices still work); a missing fee policy additionally gates ARMING (never
// panel entry) until its fetch lands, because the review's fee estimate and
// the order's local balance hold size from it — see placeGateReason.
type orderModel struct {
	store      *state.Store
	accountSeq int                                     // the active sub-account (balance readiness + fee tier); re-stamped on a switch
	fetchBands func(symbol string) (TickPolicy, error) // nil = no tick grid, no grouping
	// fetchBounds reads a pair's order value bounds and quote currency; nil (or a
	// failed fetch) = no below-min / above-max warnings for that pair.
	fetchBounds func(symbol string) (ops.OrderValueBounds, error)
	// fetchFees reads a symbol's fee policy for a sub-account (the fee tier can
	// differ per account), so it takes the account explicitly; nil = no estimates.
	fetchFees func(symbol string, accountSeq int) (FeeRates, error)

	draft   orderDraft
	view    orderView
	cursor  int // index into fields()
	formErr string

	price textinput.Model
	qty   textinput.Model
	amt   textinput.Model

	// presetIdx is the active %-of-balance preset on the size row (-1 =
	// custom/typed); ←/→ cycle it, typing resets it.
	presetIdx int
	// sizeLevels are the %-of-balance presets (from config, e.g. 10/25/50/max),
	// the values presetIdx indexes into.
	sizeLevels []int

	// cursorPrice is the orderbook price level the j/k ladder cursor sits on
	// ("" = none yet). It is a price, not a row index, so a book update never
	// silently moves the cursor to a different level.
	cursorPrice string

	// drafts remembers the last draft per symbol for the session, so
	// reopening order mode on a pair restores its size/type/tif.
	drafts map[string]orderDraft

	bands    map[string][]ops.TickBand
	levels   map[string][]string // valid orderbook grouping levels (nil = not fetched yet)
	bandsReq map[string]bool     // a fetch is in flight (or done) for the symbol

	// bounds caches each pair's published order value bounds for the session —
	// pair configuration changes with listings, not with orders. A missing entry
	// (not fetched, or a failed fetch) is the zero value, which raises no bound
	// warning at all rather than one from another pair's figures.
	bounds    map[string]ops.OrderValueBounds
	boundsReq map[string]bool

	// fees caches the fetched fee policies for the whole session, keyed by
	// {account, symbol} — the fee tier is per sub-account, so an account
	// switch reads (or fetches) its own entry and never invalidates
	// another's. feesReq guards one fetch per key ("in flight or done";
	// cleared on a failed fetch so the next trigger retries).
	fees    map[feesKey]FeeRates
	feesReq map[feesKey]bool
}

// feesKey keys the fee-policy cache by sub-account and symbol.
type feesKey struct {
	accountSeq int
	symbol     string
}

// Messages carrying the per-symbol metadata fetches.
type orderBandsMsg struct {
	symbol string
	policy TickPolicy
	err    error
}
type orderBoundsMsg struct {
	symbol string
	bounds ops.OrderValueBounds
	err    error
}
type orderFeesMsg struct {
	symbol     string
	accountSeq int // the account the fees were fetched for (the cache key's account half)
	fees       FeeRates
	err        error
}

func newOrderModel(store *state.Store, accountSeq int, fetchBands func(string) (TickPolicy, error), fetchBounds func(string) (ops.OrderValueBounds, error), fetchFees func(string, int) (FeeRates, error), sizeLevels []int) orderModel {
	return orderModel{
		store:       store,
		accountSeq:  accountSeq,
		fetchBands:  fetchBands,
		fetchBounds: fetchBounds,
		fetchFees:   fetchFees,
		// The price/amount placeholders name the pair's quote currency; until a
		// symbol is open there is no currency to name, so they start generic and
		// `open` re-labels them.
		price:      newFormInput(i18n.T("limit price"), 20),
		qty:        newFormInput(i18n.T("quantity"), 20),
		amt:        newFormInput(i18n.T("amount to spend"), 20),
		presetIdx:  -1,
		sizeLevels: sizeLevels,
		drafts:     map[string]orderDraft{},
		bands:      map[string][]ops.TickBand{},
		levels:     map[string][]string{},
		bandsReq:   map[string]bool{},
		bounds:     map[string]ops.OrderValueBounds{},
		boundsReq:  map[string]bool{},
		fees:       map[feesKey]FeeRates{},
		feesReq:    map[feesKey]bool{},
	}
}

// newFormInput is the shared single-value text-input constructor for the
// order panel's price/qty/amt fields and the funding form's amount field.
func newFormInput(placeholder string, width int) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.CharLimit = 24
	ti.SetWidth(width)
	return ti
}

// open (re)enters order mode for symbol with the given side, restoring the
// symbol's last session draft (its price is re-seeded from the live book so a
// stale price never carries over silently) and kicking off the metadata
// fetches its math wants.
func (o orderModel) open(symbol, side string) (orderModel, tea.Cmd) {
	d, ok := o.drafts[symbol]
	if !ok {
		d = newOrderDraft(symbol, side)
	}
	d.side = side
	o.draft = d
	o.view = orderForm
	o.formErr = ""
	o.presetIdx = -1
	o.cursorPrice = ""

	// Label the quote-denominated fields with the pair's own quote currency.
	if quote := ops.QuoteOf(symbol); quote != "" {
		o.price.Placeholder = i18n.T("limit price (%s)", uikit.FmtCurrency(quote))
		o.amt.Placeholder = i18n.T("%s to spend", uikit.FmtCurrency(quote))
	}

	// Seed the price from the side's own best level (a resting default): the
	// best bid for a buy, the best ask for a sell.
	anchor := "bid"
	if side == "sell" {
		anchor = "ask"
	}
	if p, ok := o.anchor(anchor); ok {
		o.setPrice(p)
	} else {
		o.setPrice(o.draft.price) // whatever the restored draft had (possibly "")
	}
	o.qty.SetValue(o.draft.qty)
	o.amt.SetValue(o.draft.amt)
	o.cursor = o.defaultCursor()
	o.syncInputFocus()
	return o, o.fetchMeta(symbol)
}

// close saves the draft for the session and drops transient state.
func (o orderModel) close() orderModel {
	o.drafts[o.draft.symbol] = o.draft
	o.view = orderForm
	o.formErr = ""
	return o
}

// defaultCursor puts the field cursor on the size row — the field most often
// still missing (the price is pre-seeded from the book).
func (o orderModel) defaultCursor() int {
	for i, f := range o.fields() {
		if f == ofQty || f == ofAmt {
			return i
		}
	}
	return 0
}

// fetchMeta starts the tick-band, order-value-bound, and fee fetches for
// symbol, once per symbol per session (a failed fetch clears its guard so the
// next open retries).
func (o *orderModel) fetchMeta(symbol string) tea.Cmd {
	var cmds []tea.Cmd
	if o.fetchBands != nil && !o.bandsReq[symbol] {
		o.bandsReq[symbol] = true
		fetch := o.fetchBands
		cmds = append(cmds, func() tea.Msg {
			p, err := fetch(symbol)
			return orderBandsMsg{symbol: symbol, policy: p, err: err}
		})
	}
	if o.fetchBounds != nil && !o.boundsReq[symbol] {
		o.boundsReq[symbol] = true
		fetch := o.fetchBounds
		cmds = append(cmds, func() tea.Msg {
			b, err := fetch(symbol)
			return orderBoundsMsg{symbol: symbol, bounds: b, err: err}
		})
	}
	if k := (feesKey{accountSeq: o.accountSeq, symbol: symbol}); o.fetchFees != nil && !o.feesReq[k] {
		o.feesReq[k] = true
		fetch := o.fetchFees
		cmds = append(cmds, func() tea.Msg {
			f, err := fetch(k.symbol, k.accountSeq)
			return orderFeesMsg{symbol: k.symbol, accountSeq: k.accountSeq, fees: f, err: err}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// applyBands/applyFees land the fetched metadata; an error clears the fetch
// guard so a later open retries (the panel meanwhile just shows the feature
// as unknown).
func (o orderModel) applyBands(msg orderBandsMsg) orderModel {
	if msg.err != nil || len(msg.policy.Bands) == 0 {
		delete(o.bandsReq, msg.symbol)
		return o
	}
	o.bands[msg.symbol] = msg.policy.Bands
	// An empty (but fetched) level list is real data: this pair offers no
	// grouping. non-nil marks it known.
	if msg.policy.Levels == nil {
		o.levels[msg.symbol] = []string{}
	} else {
		o.levels[msg.symbol] = msg.policy.Levels
	}
	return o
}

func (o orderModel) applyBounds(msg orderBoundsMsg) orderModel {
	if msg.err != nil {
		// Clear the guard so the next trigger retries. A failed fetch still
		// carries the seam's best-effort bounds — on the KRW market, the
		// documented figures — so keep them unless a real fetch has already
		// landed. A warning `order place` raises must not be missing here just
		// because the listing read failed.
		delete(o.boundsReq, msg.symbol)
		if _, seen := o.bounds[msg.symbol]; !seen && msg.bounds != (ops.OrderValueBounds{}) {
			o.bounds[msg.symbol] = msg.bounds
		}
		return o
	}
	// A pair with no published bound lands as the zero-bound entry: fetched, and
	// carrying nothing to check against.
	o.bounds[msg.symbol] = msg.bounds
	return o
}

func (o orderModel) applyFees(msg orderFeesMsg) orderModel {
	// Land the reply under the key it was fetched for — even when the user has
	// since switched accounts, the entry is that account's to keep (the cache
	// is per {account, symbol} and never cleared for the session).
	k := feesKey{accountSeq: msg.accountSeq, symbol: msg.symbol}
	if msg.err != nil {
		delete(o.feesReq, k)
		return o
	}
	o.fees[k] = msg.fees
	return o
}

// symBands/symBounds/symFees are the active symbol's cached metadata (the zero
// value when unknown).
func (o orderModel) symBands() []ops.TickBand { return o.bands[o.draft.symbol] }
func (o orderModel) symFees() *FeeRates       { return o.symFeesFor(o.draft.symbol) }
func (o orderModel) symBounds() ops.OrderValueBounds {
	return o.boundsFor(o.draft.symbol)
}

// boundsFor is a symbol's cached bounds: zero before any fetch has been recorded
// for it, and after a FAILED fetch whatever that fetch's best-effort fallback
// carried (see applyBounds) — so a bound can be present while a real listing read
// is still outstanding.
func (o orderModel) boundsFor(symbol string) ops.OrderValueBounds { return o.bounds[symbol] }

// fields is the field-cursor ring for the current draft (the sizing matrix
// decides which size rows exist; pp is a market-order rail).
func (o orderModel) fields() []orderField {
	f := []orderField{ofSide, ofType}
	if o.draft.usesPrice() {
		f = append(f, ofPrice)
	}
	if o.draft.inputAmt() {
		f = append(f, ofAmt)
	} else {
		f = append(f, ofQty)
	}
	f = append(f, ofTIF)
	if o.draft.typ == "market" {
		f = append(f, ofPP)
	}
	return append(f, ofButton)
}

func (o orderModel) curField() orderField {
	fields := o.fields()
	return fields[clamp(o.cursor, 0, len(fields)-1)]
}

// moveCursorTo puts the field cursor on field f (a click), if present.
func (o orderModel) moveCursorTo(f orderField) orderModel {
	for i, ff := range o.fields() {
		if ff == f {
			o.cursor = i
			break
		}
	}
	o.syncInputFocus()
	return o
}

// clampCursor re-validates the cursor after the field set changed (a side or
// type toggle adds and removes rows).
func (o *orderModel) clampCursor() {
	o.cursor = clamp(o.cursor, 0, len(o.fields())-1)
	o.syncInputFocus()
}

// syncInputFocus keeps exactly the input under the field cursor focused.
func (o *orderModel) syncInputFocus() {
	o.price.Blur()
	o.qty.Blur()
	o.amt.Blur()
	if o.view != orderForm {
		return
	}
	switch o.curField() {
	case ofPrice:
		o.price.Focus()
	case ofQty:
		o.qty.Focus()
	case ofAmt:
		o.amt.Focus()
	}
}

// setPrice writes a programmatic price (anchor, ladder pick, tick step) into
// the draft and its input, and aligns the ladder cursor with it.
func (o *orderModel) setPrice(p string) {
	o.draft.price = p
	o.price.SetValue(p)
	if p != "" {
		o.cursorPrice = p
	}
}

// pullInputs copies the text inputs back into the draft (typing edits the
// input; the draft is the source the math reads). Size fields are floored to
// the request precision so the preview and the placed order match; the raw
// input text is left as typed.
func (o *orderModel) pullInputs() {
	o.draft.price = strings.TrimSpace(o.price.Value())
	o.draft.qty = floorStr(strings.TrimSpace(o.qty.Value()), roundQty)
	o.draft.amt = floorStr(strings.TrimSpace(o.amt.Value()), roundAmt)
}

// preview is the live analysis of the current draft.
func (o orderModel) preview() orderPreview {
	book, _ := o.store.Orderbook(o.draft.symbol)
	return buildPreview(o.draft, book, o.store.OrderbookStatus(o.draft.symbol), o.store.BalancesFor(o.accountSeq), o.symBands(), o.symBounds(), o.symFees())
}

// anchor resolves a price anchor against the live market.
func (o orderModel) anchor(name string) (string, bool) {
	book, hasBook := o.store.Orderbook(o.draft.symbol)
	t, hasTicker := o.store.Ticker(o.draft.symbol)
	return anchorPrice(name, o.draft.side, book, hasBook, t, hasTicker, o.symBands())
}

// anchorUnavailableReason explains why an a/m/l anchor could not set a price, so
// the key reports it instead of doing nothing. "last" needs a traded price (the
// ticker); "mid"/aggress need a live book to read a two-sided touch from. It
// distinguishes "still loading" from "live but no such quote" so the hint says
// whether to wait or to type a price.
func (o orderModel) anchorUnavailableReason(name string) string {
	sym := o.draft.symbol
	if name == "last" {
		if o.store.TickerStatus(sym) == state.StatusNotReady {
			return i18n.T("no last price yet — the ticker is still loading")
		}
		return i18n.T("no last price — this pair has not traded yet")
	}
	if o.store.OrderbookStatus(sym) == state.StatusNotReady {
		return i18n.T("no price to anchor to — the orderbook is still loading")
	}
	return i18n.T("no quote to anchor to — type a limit price (or pick a row with j/k)")
}

// handleKey routes one key press. gate is the parent's money-action gate
// reason ("" = clear); ladder is the orderbook pane's visible price rows (for
// j/k), top to bottom. The returned action asks the parent to close the mode
// or dispatch the armed order.
func (o orderModel) handleKey(msg tea.KeyPressMsg, gate string, ladder []string) (orderModel, tea.Cmd, orderAction) {
	switch o.view {
	case orderBusy:
		// The placement is on the wire; its result still lands wherever we are.
		if msg.String() == "esc" {
			o.view = orderForm
		}
		return o, nil, orderActNone

	case orderConfirm:
		return o.handleConfirmKey(msg, gate)
	}
	return o.handleFormKey(msg, gate, ladder)
}

// confirmPlace is the single commit step shared by Enter and the [ enter place ]
// click: it re-checks the gate and the sizing-matrix guard before dispatching (a
// price nudge while armed can re-derive an amount-sized qty to an invalid one),
// so the two paths can never diverge.
func (o orderModel) confirmPlace(gate string) (orderModel, orderAction) {
	if gate != "" {
		o.formErr = gate // shown on the confirm view; stays armed
		return o, orderActNone
	}
	if err := o.draft.validate(); err != nil {
		o.formErr = err.Error()
		return o, orderActNone
	}
	o.view = orderBusy
	return o, orderActPlace
}

func (o orderModel) handleConfirmKey(msg tea.KeyPressMsg, gate string) (orderModel, tea.Cmd, orderAction) {
	switch s := msg.String(); s {
	case "enter":
		o, act := o.confirmPlace(gate)
		return o, nil, act
	case "esc":
		o.view = orderForm
		o.formErr = ""
		o.syncInputFocus()
		return o, nil, orderActNone
	case "[", "]":
		// The armed price is still nudgeable on the tick grid; the review
		// re-resolves. [ ] { } — the same vocabulary as the ladder's confirm —
		// rather than ←/→: the bracket keys never depend on a field focus.
		if o.draft.usesPrice() {
			o = o.stepPrice(dir(s == "]"))
		}
		return o, nil, orderActNone
	case "{", "}":
		if o.draft.usesPrice() {
			o = o.stepPrice(10 * dir(s == "}"))
		}
		return o, nil, orderActNone
	}
	return o, nil, orderActNone
}

func dir(positive bool) int {
	if positive {
		return 1
	}
	return -1
}

func (o orderModel) handleFormKey(msg tea.KeyPressMsg, gate string, ladder []string) (orderModel, tea.Cmd, orderAction) {
	s := msg.String()
	switch s {
	case "esc":
		return o.close(), nil, orderActClose
	case "up", "down":
		delta := 1
		if s == "up" {
			delta = -1
		}
		n := len(o.fields())
		o.cursor = (o.cursor + delta + n) % n
		o.syncInputFocus()
		return o, nil, orderActNone
	case "left", "right":
		// On a numeric input row the arrows move the text caret (a text field's
		// expected behavior); the price value steps on [ ]{ } and the size presets
		// cycle on %. On the selector rows there is no text, so the arrows keep
		// adjusting the value under the cursor.
		switch o.curField() {
		case ofPrice:
			o.editInput(&o.price, msg)
		case ofQty:
			o.editInput(&o.qty, msg)
		case ofAmt:
			o.editInput(&o.amt, msg)
		default:
			return o.adjustField(s == "right"), nil, orderActNone
		}
		return o, nil, orderActNone
	case keyPreset:
		// Cycle the %-of-balance size presets forward, wrapping. (The chips
		// themselves are click-to-arm.) Works from any row, like the price
		// nudges below.
		if n := len(o.sizeLevels); n > 0 {
			o = o.applyPresetIdx((o.presetIdx + 1) % n)
		}
		return o, nil, orderActNone
	case "[", "]":
		// Price nudges independent of the field cursor ([ ] { } work from any
		// row; on the price row ←/→ move the caret instead of stepping).
		if o.draft.usesPrice() {
			o = o.stepPrice(dir(s == "]"))
		}
		return o, nil, orderActNone
	case "{", "}":
		if o.draft.usesPrice() {
			o = o.stepPrice(10 * dir(s == "}"))
		}
		return o, nil, orderActNone
	case "enter":
		return o.arm(gate), nil, orderActNone
	case "j", "k":
		return o.moveLadder(dir(s == "j"), ladder), nil, orderActNone
	case "b", "s":
		// Direction keys: declare the side. The price re-seeds to the new
		// side's own best level (join); pressing the current side's key again
		// re-anchors there.
		return o.applySide(sideFor(s == "b")), nil, orderActNone
	case "u":
		// Toggle a limit order between quantity and amount entry (a market
		// order's size unit is fixed by its side).
		return o.toggleSizeUnit(), nil, orderActNone
	case "a", "m", "l":
		if o.draft.usesPrice() {
			name := s
			switch s {
			case "a":
				// Aggress: the opposite touch — the would-cross price.
				name = "ask"
				if o.draft.side == "sell" {
					name = "bid"
				}
			case "m":
				name = "mid"
			case "l":
				name = "last"
			}
			if p, ok := o.anchor(name); ok {
				o.setPrice(p)
				o.formErr = ""
			} else {
				// The anchor's source is not available (no last price, or no live
				// book side to read a touch/mid from). Say so instead of silently
				// ignoring the key.
				o.formErr = o.anchorUnavailableReason(name)
			}
		}
		return o, nil, orderActNone
	}

	// Everything else edits the numeric input under the cursor. The inputs
	// take digits and one dot only, so command letters above can never be
	// swallowed as text.
	switch o.curField() {
	case ofPrice:
		if o.editInput(&o.price, msg) {
			o.pullInputs()
			o.formErr = ""
			o.cursorPrice = "" // a typed price detaches the ladder cursor
		}
	case ofQty:
		if o.editInput(&o.qty, msg) {
			o.pullInputs()
			o.presetIdx = -1
			o.formErr = ""
		}
	case ofAmt:
		if o.editInput(&o.amt, msg) {
			o.pullInputs()
			o.presetIdx = -1
			o.formErr = ""
		}
	}
	return o, nil, orderActNone
}

// editInput forwards an editing key to a numeric input: digits and a dot as
// text, plus backspace/delete and the cursor keys (←/→/home/end). Reports
// whether the value may have changed — a pure cursor move does not, so it
// returns false for those.
func (o *orderModel) editInput(ti *textinput.Model, msg tea.KeyPressMsg) bool {
	s := msg.String()
	if msg.Text != "" && !numericText(msg.Text) {
		return false
	}
	if msg.Text == "" && s != "backspace" && s != "delete" && s != "home" && s != "end" && s != "left" && s != "right" {
		return false
	}
	var cmd tea.Cmd
	*ti, cmd = ti.Update(msg)
	_ = cmd // textinput blink commands are cosmetic; the TUI redraws on ticks anyway
	return s != "home" && s != "end" && s != "left" && s != "right"
}

// numericText reports whether typed text is digits/dot only.
func numericText(t string) bool {
	for _, r := range t {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// applySide sets the draft's side and re-seeds the price to that side's own
// best level: a price picked for one side is usually marketable on the other,
// so it never carries across a flip (the same rule open applies). With no
// live book the price simply stays. The side change can add/remove rows
// (amt↔qty on a market order), so the cursor re-clamps.
func (o orderModel) applySide(side string) orderModel {
	o.draft.side = side
	o.clampCursor()
	o.formErr = ""
	if o.draft.usesPrice() {
		name := "bid"
		if side == "sell" {
			name = "ask"
		}
		if p, ok := o.anchor(name); ok {
			o.setPrice(p)
		}
	}
	return o
}

// toggleSizeUnit flips a limit order between quantity and amount entry, carrying
// the current size across the switch (amount ↔ quantity at the price). A market
// order's size unit is fixed by its side, so it is a no-op there.
func (o orderModel) toggleSizeUnit() orderModel {
	if o.draft.typ != "limit" {
		return o
	}
	if o.draft.sizeInAmt {
		if q := o.draft.wireQty(); q != "" { // a missing price can't derive a qty — keep the existing one
			o.draft.qty = q
		}
		o.draft.sizeInAmt = false
		o.qty.SetValue(o.draft.qty)
	} else {
		o.draft.amt = "" // express the quantity as an amount at the price
		if q, ok := parseDec(o.draft.qty); ok {
			if p, ok := parseDec(o.draft.price); ok && p.IsPositive() {
				o.draft.amt = roundAmt(q.Mul(p), roundFloor).String()
			}
		}
		o.draft.sizeInAmt = true
		o.amt.SetValue(o.draft.amt)
	}
	o.presetIdx = -1
	o.formErr = ""
	o.clampCursor() // the size row swapped ofQty↔ofAmt; re-focus its input
	return o
}

// adjustField cycles the value of the selector row under the cursor — driven by
// ←/→ on that row and by the row's ‹/› click-caps. The numeric rows carry no
// ‹/› caps: ←/→ move their caret, and their values step on [ ]{ } (price) or %
// (size), so adjustField is never reached for them.
func (o orderModel) adjustField(forward bool) orderModel {
	switch o.curField() {
	case ofSide:
		// The same side flip as the b/s keys — the price re-seed included.
		o = o.applySide(sideFor(o.draft.side != "buy"))
	case ofType:
		if o.draft.typ == "limit" {
			o.draft.typ = "market"
		} else {
			o.draft.typ = "limit"
		}
		o.clampCursor()
		o.formErr = ""
	case ofTIF:
		// A market order is ioc-only — the row is fixed, so ←/→ only cycles for a
		// limit order. The limit-mode selection (tifIdx) is left untouched by a
		// market order, so it is restored when the type flips back to limit.
		if o.draft.tifCyclable() {
			n := len(tifOptions)
			delta := 1
			if !forward {
				delta = n - 1
			}
			o.draft.tifIdx = (o.draft.tifIdx + delta) % n
		}
		o.formErr = ""
	case ofPP:
		o.draft.pp = !o.draft.pp
		o.formErr = ""
	}
	return o
}

// stepPrice moves the draft price n ticks along the symbol's tick grid. With
// no policy loaded there is no grid to step — the price stays and the panel's
// tick line already says the grid is unknown.
func (o orderModel) stepPrice(n int) orderModel {
	base := o.draft.price
	if base == "" {
		if p, ok := o.anchor("mid"); ok {
			base = p
		} else {
			return o
		}
	}
	next, ok := ops.StepTicks(o.symBands(), base, n)
	if !ok {
		return o
	}
	o.setPrice(next)
	o.formErr = ""
	return o
}

// applyPresetIdx applies sizeLevels[i] to the draft's size field.
func (o orderModel) applyPresetIdx(i int) orderModel {
	d, reason := applyPreset(o.draft, o.store.BalancesFor(o.accountSeq), o.store.BalancesReady(o.accountSeq), o.symFees(), o.sizeLevels[i])
	if reason != "" {
		o.formErr = i18n.T("cannot size from balance — %s", reason)
		return o
	}
	o.presetIdx = i
	o.draft = d
	o.qty.SetValue(d.qty)
	o.amt.SetValue(d.amt)
	o.formErr = ""
	return o
}

// moveLadder walks the j/k cursor across the orderbook pane's visible price
// rows (delta +1 = down the screen, toward lower prices), writing the level's
// price into the draft. With no cursor yet it starts from the row nearest the
// draft price (or the side's best level).
func (o orderModel) moveLadder(delta int, ladder []string) orderModel {
	prices := make([]string, 0, len(ladder))
	for _, p := range ladder {
		if p != "" {
			prices = append(prices, p)
		}
	}
	if len(prices) == 0 || !o.draft.usesPrice() {
		return o
	}
	at := o.cursorPrice
	if at == "" {
		at = o.draft.price
	}
	idx := nearestPriceIdx(prices, at)
	if o.cursorPrice != "" || at != "" {
		idx = clamp(idx+delta, 0, len(prices)-1)
	}
	o.setPrice(prices[idx])
	o.formErr = ""
	return o
}

// nearestPriceIdx finds the row of price in a descending price list, or the
// nearest lower row when the exact level is gone (0 when price is "" or
// unparseable — the top row).
func nearestPriceIdx(prices []string, price string) int {
	target, ok := parseDec(price)
	if !ok {
		return 0
	}
	for i, p := range prices {
		if d, ok := parseDec(p); ok && !d.GreaterThan(target) {
			return i
		}
	}
	return len(prices) - 1
}

// arm validates the draft and swaps to the confirm view: the size must parse
// positive, a limit needs a price (snapped onto the tick grid when the policy
// is known — the snap is disclosed on the review), and the parent's
// money-action/freshness gate must be clear.
func (o orderModel) arm(gate string) orderModel {
	o.pullInputs()
	if gate != "" {
		o.formErr = gate
		return o
	}
	if o.draft.usesPrice() {
		p, ok := parseDec(o.draft.price)
		if !ok || !p.IsPositive() {
			o.formErr = i18n.T("enter a limit price (j/k picks one from the book; b/s/a/m/l anchor it)")
			return o
		}
		// An explicitly typed price is checked, never modified: reject an
		// off-grid price with its tick so the user fixes it (a programmatic
		// price — anchor, ladder pick, nudge — is already on the grid).
		if onGrid, ok := ops.OnTick(o.symBands(), o.draft.price); ok && !onGrid {
			tick, _ := ops.TickSizeAt(o.symBands(), o.draft.price)
			o.formErr = i18n.T("price is off the tick grid (tick %s) — nudge with [ ] or pick from the book", tick)
			return o
		}
	}
	size := o.draft.sizeValue()
	d, ok := parseDec(size)
	if !ok || !d.IsPositive() {
		if o.draft.inputAmt() {
			o.formErr = i18n.T("enter the amount (%s cycles balance presets)", keyPreset)
		} else {
			o.formErr = i18n.T("enter the quantity (%s cycles balance presets)", keyPreset)
		}
		return o
	}
	if o.draft.typ == "limit" && o.draft.sizeInAmt && o.draft.wireQty() == "" {
		o.formErr = i18n.T("amount too small for a tradable quantity at this price")
		return o
	}
	if err := o.draft.validate(); err != nil {
		o.formErr = err.Error()
		return o
	}
	o.formErr = ""
	o.view = orderConfirm
	o.syncInputFocus() // blurs the inputs while armed
	return o
}

// placeDone lands the Trader's result: back to the form either way — sticky
// on success (the next order often looks like the last), the error inline on
// a rejection. The parent owns the toast/flash side effects.
func (o orderModel) placeDone(err error) orderModel {
	o.view = orderForm
	if err != nil {
		o.formErr = errorText(err)
	} else {
		o.formErr = ""
	}
	o.syncInputFocus()
	return o
}

// orderGate is the draft-independent half of the placement gate, shared by
// every order surface: the TUI-wide money single-flight, the orderbook
// liveness, and the active {account, symbol} fee policy (fetched async;
// retried by the clock tick while an order surface is open, so the gate clears
// on its own). A nil Fees seam disables fee estimates entirely — then there is
// nothing to wait for and the gate must not block on it.
func (m model) orderGate() string {
	sym := m.symbol()
	feesKnown := m.order.fetchFees == nil || m.order.symFeesFor(sym) != nil
	return placeGateReason(m.moneyActionInFlight(), m.orderbookStatus(sym) != state.StatusNotReady, feesKnown)
}

// draftGate is the full placement gate for one concrete draft: orderGate plus
// the empty-book rule, which depends on what the draft IS (a market order
// can't fill, an ioc/fok limit can't rest — emptyBookRefusal). Every surface
// must pass ITS OWN draft — the panel its form draft (panelGate), the command
// bar its resolved/armed order, the ladder its arming/armed order — never
// another surface's: judging, say, a command-bar market order by the panel's
// draft would pass refused orders and refuse valid ones.
func (m model) draftGate(d orderDraft) string {
	if reason := m.orderGate(); reason != "" {
		return reason
	}
	if m.orderbookStatus(m.symbol()) == state.StatusEmpty {
		return emptyBookRefusal(d)
	}
	return ""
}

// panelGate is draftGate for the order panel's own draft — the gate every
// panel render and key route passes down.
func (m model) panelGate() string { return m.draftGate(m.order.draft) }

// fmtTickHint renders the size of one tick for the hint line ("" unknown).
func fmtTickHint(bands []ops.TickBand, price string) string {
	if price == "" {
		return ""
	}
	t, ok := ops.TickSizeAt(bands, price)
	if !ok {
		return ""
	}
	return t
}

// parsePresetLabel renders a preset chip label.
func presetLabel(pct int) string {
	if pct == 100 {
		return "max"
	}
	return strconv.Itoa(pct) + "%"
}
