// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/components/ladder"
)

// Trade ladder mode ('t', modeLadder): the orderbook+trades center region
// becomes a full-height DOM-style price ladder (the ladder component) and
// order entry collapses to its fastest loop — size decided calmly in advance,
// price picked spatially, one confirm. It is a sub-model in the funding
// screen's mold: this file owns the state and keys, ladderview.go the render,
// and the parent owns the mode switch, routing, geometry, and the money
// single-flight (placement dispatches only through the parent).
//
// The loop: number keys arm a size preset (a % of the funding balance, sticky
// per symbol for the session), t sets the ladder's default tif (session-wide);
// both show as guilleted chips in the title. Setting a chip (a number for size,
// t for tif) focuses it, left/right then steps the focused chip, and a click on
// ‹/› steps directly. j/k walk the ladder cursor across the
// visible price rows, b/s arm a LIMIT order at the cursor price (B/S a market),
// and x arms a cancel of the resting order at the cursor row. Arming stamps the
// default tif onto the order and swaps a confirm strip into the ladder's foot:
// the fully resolved order (the engine's live preview — notional, fee, preplace
// warnings, book freshness) with the price still nudgeable on the tick grid
// ([ ] ±1, { } ±10) and the tif cyclable (t) for THIS order only — a confirm
// tif change never moves the ladder default. The armed price is frozen — the
// cursor pick is explicit, so there is no anchor to drift (unlike the command
// bar).

// ladderView is which content the ladder's foot strip is showing.
type ladderView int

const (
	ladderBrowse  ladderView = iota // walking the ladder; the strip is a size/error line
	ladderConfirm                   // an armed order or cancel awaits enter
	ladderBusy                      // the action is on the wire
)

// ladderArmKind is what the confirm strip is armed with.
type ladderArmKind int

const (
	armNone ladderArmKind = iota
	armPlace
	armCancel
)

// ladderArmed is the frozen action under the confirm strip.
type ladderArmed struct {
	kind  ladderArmKind
	draft orderDraft // armPlace: the fully resolved order (price frozen; nudges edit it)
	// armCancel: the resting order captured at arm time (it may leave the
	// store before the confirm lands).
	cancelID    int64
	cancelSide  string
	cancelQty   string
	cancelPrice string
}

// ladderAction is what a handled key asks the parent to do.
type ladderAction int

const (
	ladderActNone   ladderAction = iota
	ladderActClose               // leave ladder mode
	ladderActPlace               // dispatch armed.draft.form() to the Trader
	ladderActCancel              // dispatch a cancel of armed.cancelID
)

// ladderPageStep is how many rows J/K jump.
const ladderPageStep = 5

// ladderChip identifies a title chip — the size preset or the default tif — for
// the focus cursor that setting a chip moves, left/right adjusts, and a click
// targets.
type ladderChip int

const (
	chipSize ladderChip = iota // the zero value: a fresh ladder focuses size (focus then persists, like defaultTifIdx)
	chipTif
)

// ladderModel is the trade-ladder sub-model. It reads the store directly (the
// parent mutates it; single goroutine); the per-symbol tick bands and fee
// rates come in through the key/render calls — they live on the order panel's
// caches (one fetch feeds every entry surface).
type ladderModel struct {
	store      *state.Store
	accountSeq int // the session's pinned sub-account (balance readiness lookup)
	symbol     string

	view     ladderView
	armed    ladderArmed
	stripErr string

	// cursorPrice is the ladder cursor's row — a price, not a row index, so a
	// book update never silently moves the cursor to a different level.
	cursorPrice string

	// sizePcts is the armed size preset per symbol (a sizeLevels value; 0 =
	// not set), sticky for the session — at execution time only direction and
	// price are chosen.
	sizePcts map[string]int
	// sizeLevels are the %-of-balance presets (from config), bound to number
	// keys 1..N.
	sizeLevels []int

	// defaultTifIdx is the ladder's default time-in-force (an index into
	// tifOptions), set with t while browsing and stamped onto every limit arm.
	// It is session-wide — one preference across symbols, not per-symbol like
	// the size — and not persisted (resets to gtc on restart). A per-order
	// change in the confirm strip edits the armed copy, never this default.
	defaultTifIdx int

	// titleFocus is the title chip left/right adjusts and a click lands on
	// (size or tif). Only the focused chip is highlighted, so the guilleted
	// chips read as real steppers rather than always-focused decoration.
	titleFocus ladderChip
}

func newLadderModel(store *state.Store, accountSeq int, sizeLevels []int) ladderModel {
	return ladderModel{store: store, accountSeq: accountSeq, sizePcts: map[string]int{}, sizeLevels: sizeLevels}
}

// open (re)enters ladder mode for symbol: the cursor seeds at the best bid
// (the spread's near side) and the symbol's session size preset is restored.
func (l ladderModel) open(symbol string) ladderModel {
	l.symbol = symbol
	l.view = ladderBrowse
	l.armed = ladderArmed{}
	l.stripErr = ""
	l.cursorPrice = ""
	if book, ok := l.store.Orderbook(symbol); ok && len(book.Bids) > 0 {
		l.cursorPrice = book.Bids[0].Price
	}
	return l
}

// sizePct is the active symbol's armed size preset (0 = not set).
func (l ladderModel) sizePct() int { return l.sizePcts[l.symbol] }

// handleKey routes one key press. placeGate is the parent's freshness+money
// gate for placement ("" = clear), resolved per draft (model.draftGate) — the
// ladder arms both limit and market orders, and what an empty book refuses
// depends on the draft being armed, so a single precomputed string cannot
// serve both b/s and B/S; busyGate is the money single-flight alone (a cancel
// must work against a stale book); prices are the ladder's visible row prices
// top to bottom (the row-layout source); bands/fees are the symbol's cached
// metadata; level is the book's grouping level ("" = raw), which buckets the
// cancel-at-row matching the same way the MINE cells bucket. The returned
// action asks the parent to close the mode or dispatch the armed order/cancel.
func (l ladderModel) handleKey(msg tea.KeyPressMsg, placeGate func(orderDraft) string, busyGate string, prices []string, bands []ops.TickBand, fees *FeeRates, level string) (ladderModel, ladderAction) {
	switch l.view {
	case ladderBusy:
		// The action is on the wire; its result still lands wherever we are.
		if msg.String() == "esc" {
			l.view = ladderBrowse
			l.armed = ladderArmed{}
		}
		return l, ladderActNone
	case ladderConfirm:
		return l.handleConfirmKey(msg, placeGate, busyGate, bands)
	}
	return l.handleBrowseKey(msg, placeGate, busyGate, prices, fees, level)
}

func (l ladderModel) handleBrowseKey(msg tea.KeyPressMsg, placeGate func(orderDraft) string, busyGate string, prices []string, fees *FeeRates, level string) (ladderModel, ladderAction) {
	switch s := msg.String(); s {
	case "esc":
		return l, ladderActClose
	case "j", "down":
		return l.moveCursor(1, prices), ladderActNone
	case "k", "up":
		return l.moveCursor(-1, prices), ladderActNone
	case "J", "pgdown":
		return l.moveCursor(ladderPageStep, prices), ladderActNone
	case "K", "pgup":
		return l.moveCursor(-ladderPageStep, prices), ladderActNone
	case "c":
		return l.recenter(prices), ladderActNone
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Number keys 1..N arm a size preset (N = len(sizeLevels)); a digit past
		// the configured levels is a no-op. The chip you just set becomes the
		// focused one, so left/right keeps adjusting it.
		if i, _ := strconv.Atoi(s); i <= len(l.sizeLevels) {
			l.sizePcts[l.symbol] = l.sizeLevels[i-1]
			l.titleFocus = chipSize
			l.stripErr = ""
		}
		return l, ladderActNone
	case "t":
		// t cycles the ladder's session-wide default tif — the value every limit
		// arm inherits (a market arm is ioc-only, so the default is a limit-order
		// preference) — and focuses the tif chip so left/right keeps adjusting it.
		l.titleFocus = chipTif
		return l.stepTif(1), ladderActNone
	case "left":
		return l.stepChip(-1), ladderActNone
	case "right":
		return l.stepChip(1), ladderActNone
	case "b", "s":
		return l.armOrder(sideFor(s == "b"), "limit", placeGate, fees), ladderActNone
	case "B", "S":
		return l.armOrder(sideFor(s == "B"), "market", placeGate, fees), ladderActNone
	case "x":
		return l.armCancelAtCursor(busyGate, level), ladderActNone
	}
	return l, ladderActNone
}

// stepChip adjusts the focused title chip by dir (+1 next, -1 prev).
func (l ladderModel) stepChip(dir int) ladderModel {
	if l.titleFocus == chipTif {
		return l.stepTif(dir)
	}
	return l.stepSize(dir)
}

// stepSize walks the size preset among the configured levels. Unset sits below
// the scale, so either arrow steps onto the first preset; thereafter the scale
// clamps at both ends (the cyclic tif chip wraps; this bounded size selector
// does not).
func (l ladderModel) stepSize(dir int) ladderModel {
	n := len(l.sizeLevels)
	if n == 0 {
		return l
	}
	idx := -1
	for i, v := range l.sizeLevels {
		if v == l.sizePct() {
			idx = i
			break
		}
	}
	idx = clamp(idx+dir, 0, n-1)
	l.sizePcts[l.symbol] = l.sizeLevels[idx]
	l.stripErr = ""
	return l
}

// stepTif cycles the default tif among tifOptions (wrapping).
func (l ladderModel) stepTif(dir int) ladderModel {
	n := len(tifOptions)
	l.defaultTifIdx = ((l.defaultTifIdx+dir)%n + n) % n
	l.stripErr = ""
	return l
}

func sideFor(buy bool) string {
	if buy {
		return "buy"
	}
	return "sell"
}

// moveCursor walks the cursor across the visible price rows (delta +1 = down
// the screen, toward lower prices). With no cursor yet it starts at the row
// nearest the previous position (or the top).
func (l ladderModel) moveCursor(delta int, prices []string) ladderModel {
	rows := make([]string, 0, len(prices))
	for _, p := range prices {
		if p != "" {
			rows = append(rows, p)
		}
	}
	if len(rows) == 0 {
		return l
	}
	idx := nearestPriceIdx(rows, l.cursorPrice)
	if l.cursorPrice != "" {
		idx = clamp(idx+delta, 0, len(rows)-1)
	}
	l.cursorPrice = rows[idx]
	l.stripErr = ""
	return l
}

// reconcileCursor re-seats the cursor onto a live row when its pinned price has
// left the book. The cursor pins to a PRICE, not a row index, so a level
// inserting or leaving elsewhere never moves it (open's intent) — but a
// volatile book (a live-mirror pair, or the walk between ticks) churns levels
// in and out between key presses, and a price that vanishes entirely would
// otherwise strand the cursor on a nonexistent row: no marker rendered, and an
// arm or preset estimate silently resolving against a price no longer on the
// book. While the pinned price still has a row it is left exactly where it is;
// only when it is gone does the cursor snap to the nearest surviving price, so
// it stays visible and always names a real level.
func (l ladderModel) reconcileCursor(prices []string) ladderModel {
	if l.cursorPrice == "" {
		return l
	}
	rows := make([]string, 0, len(prices))
	for _, p := range prices {
		if p != "" {
			rows = append(rows, p)
		}
	}
	if len(rows) == 0 {
		return l // book loading/empty — keep the pin, nothing to snap to
	}
	for _, p := range rows {
		if p == l.cursorPrice {
			return l // still a live row
		}
	}
	l.cursorPrice = rows[nearestPriceIdx(rows, l.cursorPrice)]
	return l
}

// recenter puts the cursor on the row nearest the last trade price (or the
// mid when no ticker is live yet).
func (l ladderModel) recenter(prices []string) ladderModel {
	target := ""
	if t, ok := l.store.Ticker(l.symbol); ok && t.Close != "" {
		target = t.Close
	} else if book, ok := l.store.Orderbook(l.symbol); ok && len(book.Bids) > 0 {
		target = book.Bids[0].Price
	}
	if target == "" {
		return l
	}
	rows := make([]string, 0, len(prices))
	for _, p := range prices {
		if p != "" {
			rows = append(rows, p)
		}
	}
	if len(rows) == 0 {
		return l
	}
	l.cursorPrice = rows[nearestPriceIdx(rows, target)]
	l.stripErr = ""
	return l
}

// armOrder builds and freezes an order at the cursor: the armed size preset
// resolves to a quantity/amount through the engine's %-of-balance sizing (a
// limit sizes at the cursor price, so a 100% preset is exact), and the strip
// swaps to the confirm view. Refused — with the reason on the strip — while
// the gate stands against this arm's draft, without a size preset, or (limit)
// without a cursor row.
func (l ladderModel) armOrder(side, typ string, gate func(orderDraft) string, fees *FeeRates) ladderModel {
	d := newOrderDraft(l.symbol, side)
	d.typ = typ
	d.tifIdx = l.defaultTifIdx // inherit the ladder default (limit only; market resolves to ioc)
	if g := gate(d); g != "" {
		l.stripErr = g
		return l
	}
	pct := l.sizePct()
	if pct == 0 {
		l.stripErr = i18n.T("arm a size first — %s set %s %% of balance", sizeKeysLabel(l.sizeLevels), sizeValuesLabel(l.sizeLevels))
		return l
	}
	if d.usesPrice() {
		if l.cursorPrice == "" {
			l.stripErr = i18n.T("no price row under the cursor yet — j/k to pick one")
			return l
		}
		d.price = l.cursorPrice
	}
	sized, reason := applyPreset(d, l.store.BalancesFor(l.accountSeq), l.store.BalancesReady(l.accountSeq), fees, pct)
	if reason != "" {
		l.stripErr = i18n.T("cannot size from balance — %s", reason)
		return l
	}
	l.armed = ladderArmed{kind: armPlace, draft: sized}
	l.view = ladderConfirm
	l.stripErr = ""
	return l
}

// armCancelAtCursor arms a cancel of the account's resting order at the
// cursor row (the newest when several rest at the same price row), capturing
// its identity now — by confirm time a fill may have removed it from the
// store. On a grouped book an order belongs to its bucket row
// (ladder.Bucket, the same mapping that places its MINE cell); a bucket can
// even hold both a buy and a sell (a self-locked own book) and the newest
// still wins — safe either way, because the confirm strip discloses the
// captured order's exact side, price, and id before anything is sent.
func (l ladderModel) armCancelAtCursor(busyGate, level string) ladderModel {
	if busyGate != "" {
		l.stripErr = busyGate
		return l
	}
	if l.cursorPrice == "" {
		l.stripErr = i18n.T("no price row under the cursor yet — j/k to pick one")
		return l
	}
	for _, o := range l.store.OpenOrdersFor(l.accountSeq, l.symbol) { // newest first
		if ladder.Bucket(o.Price, level, o.Side == "sell") == l.cursorPrice {
			l.armed = ladderArmed{kind: armCancel, cancelID: o.OrderID,
				cancelSide: o.Side, cancelQty: o.Qty, cancelPrice: o.Price}
			l.view = ladderConfirm
			l.stripErr = ""
			return l
		}
	}
	l.stripErr = i18n.T("no resting order at this price row")
	return l
}

func (l ladderModel) handleConfirmKey(msg tea.KeyPressMsg, placeGate func(orderDraft) string, busyGate string, bands []ops.TickBand) (ladderModel, ladderAction) {
	switch s := msg.String(); s {
	case "esc":
		l.view = ladderBrowse
		l.armed = ladderArmed{}
		l.stripErr = ""
		return l, ladderActNone
	case "enter":
		if l.armed.kind == armCancel {
			if busyGate != "" {
				l.stripErr = busyGate
				return l, ladderActNone
			}
			l.view = ladderBusy
			return l, ladderActCancel
		}
		// Gate the ARMED draft: its tif may have been cycled since arming (the
		// confirm strip's t key), and on an empty book that changes the verdict.
		if g := placeGate(l.armed.draft); g != "" {
			l.stripErr = g
			return l, ladderActNone
		}
		l.view = ladderBusy
		return l, ladderActPlace
	case "[", "]":
		return l.nudgeArmed(dir(s == "]"), bands), ladderActNone
	case "{", "}":
		return l.nudgeArmed(10*dir(s == "}"), bands), ladderActNone
	case "t":
		// t cycles the tif of THIS armed order only (the draft is a copy made at
		// arm time — this never touches the ladder's default tif). A market order
		// is ioc-only, so it applies to a limit arm only.
		if l.armed.kind == armPlace && l.armed.draft.tifCyclable() {
			l.armed.draft.tifIdx = (l.armed.draft.tifIdx + 1) % len(tifOptions)
			l.stripErr = ""
		}
		return l, ladderActNone
	}
	return l, ladderActNone
}

// nudgeArmed steps the armed limit price n ticks along the grid (a no-op for
// a market order or when the tick policy is unknown — the strip's tick hint
// already says so).
func (l ladderModel) nudgeArmed(n int, bands []ops.TickBand) ladderModel {
	if l.armed.kind != armPlace || !l.armed.draft.usesPrice() {
		return l
	}
	next, ok := ops.StepTicks(bands, l.armed.draft.price, n)
	if !ok {
		return l
	}
	l.armed.draft.price = next
	l.stripErr = ""
	return l
}

// placeDone lands the Trader's result: a rejection returns to the confirm
// strip with the error inline (the armed order kept — nudge and re-place, or
// esc out; if the busy view was already dismissed the error shows on the
// browse foot instead); an accept disarms back to browsing (the parent
// flashes the new order's MINE cell and toasts).
func (l ladderModel) placeDone(err error) ladderModel {
	if err != nil {
		l.stripErr = errorText(err)
		if l.armed.kind == armPlace {
			l.view = ladderConfirm
		}
		return l
	}
	l.view = ladderBrowse
	l.armed = ladderArmed{}
	l.stripErr = ""
	return l
}

// cancelDone lands a cancel result dispatched from the ladder: back to
// browsing either way (the parent's toast reports the outcome, and the
// canceled order leaves the MINE column as the store updates).
func (l ladderModel) cancelDone() ladderModel {
	if l.view == ladderBusy && l.armed.kind == armCancel {
		l.view = ladderBrowse
		l.armed = ladderArmed{}
	}
	return l
}
