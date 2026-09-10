// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/digitalx-official/digitalx-cli/internal/i18n"
	"github.com/digitalx-official/digitalx-cli/internal/ops"
	"github.com/digitalx-official/digitalx-cli/internal/stream/state"
)

// The order-entry engine: the draft being composed, the live preview computed
// against the store's book and balances, and the sizing math behind presets
// and anchors. Everything in this file is pure — no I/O, no model access — so
// the entry surfaces (the docked panel, and any future entry path) stay thin
// and the math is testable in isolation. Order values are decimal strings end
// to end; decimal.Decimal appears only for the advisory display/threshold math,
// mirroring ops/preplace.go.

// orderDraft is the order being composed. Which size fields apply follows the
// order-sizing matrix (limit → price+qty; market buy → amt; market sell → qty);
// values typed into a field the matrix later hides are retained on the draft
// but never emitted.
type orderDraft struct {
	symbol    string
	side      string // buy | sell
	typ       string // limit | market
	price     string // limit only
	qty       string // limit, and market sell
	amt       string // market buy (quote to spend); or a limit order in amount mode
	sizeInAmt bool   // limit order entered by amount (placed as a derived quantity)
	tifIdx    int    // index into tifOptions — the limit-mode time-in-force (a market order is ioc-only)
	pp        bool   // price protection, applied to market orders only
}

// newOrderDraft opens a draft for symbol: a limit order with price protection
// pre-enabled should the user switch to a market order — a terminal for humans
// defaults the taker-protection rail on.
func newOrderDraft(symbol, side string) orderDraft {
	return orderDraft{symbol: symbol, side: side, typ: "limit", pp: true}
}

// The sizing matrix, as predicates. usesQty/usesAmt describe the field the draft
// emits on the WIRE; inputAmt describes which size the user enters.
func (d orderDraft) usesPrice() bool { return d.typ == "limit" }
func (d orderDraft) usesQty() bool   { return d.typ == "limit" || d.side == "sell" }
func (d orderDraft) usesAmt() bool   { return d.typ == "market" && d.side == "buy" }

// tifCyclable reports whether the time-in-force can be chosen: a limit order
// selects among tifOptions, a market order is ioc-only (fixed by tif).
func (d orderDraft) tifCyclable() bool { return d.typ != "market" }

// canRest reports whether the draft would REST on the book as a maker instead of
// taking from it: a limit order whose time-in-force lets it sit. A market order
// always takes, and an ioc/fok limit is canceled unfilled rather than resting.
// This is the one definition of that rule, read by the placement gate
// (fillSideRefusal) to decide which orders an empty fill side refuses.
//
// It is a property of the DRAFT, so it says only what the tif allows, never what
// the book does. The preview's fee estimate needs the latter and reads the
// simulation's own outcome instead (see feeEstimate).
func (d orderDraft) canRest() bool {
	tif := d.tif()
	return d.typ == "limit" && tif != "ioc" && tif != "fok"
}

// notionalIsEstimate reports whether the order's quote-currency notional is valued against
// the live book rather than fixed by the frozen wire fields — true only for a
// market sell, whose notional is qty × best bid and so moves with the market. A
// limit order's notional is price × qty and a market buy's is its amt, both
// frozen once armed, so their notional is the figure placement commits to, not
// an estimate.
func (d orderDraft) notionalIsEstimate() bool { return d.typ == "market" && d.side == "sell" }

// inputAmt reports whether the size is entered as a quote amount: always for a
// market buy, and for a limit order toggled into amount mode.
func (d orderDraft) inputAmt() bool {
	if d.typ == "market" {
		return d.side == "buy"
	}
	return d.sizeInAmt
}

// wireQty is the quantity placed on the wire. A limit order entered by amount
// derives it at the price (the API has no limit-amount field); otherwise it is
// the qty field. "" when a limit-amount qty is not yet derivable.
func (d orderDraft) wireQty() string {
	if d.typ == "limit" && d.sizeInAmt {
		if q, ok := qtyFromAmt(d.amt, d.price); ok {
			return q
		}
		return ""
	}
	return d.qty
}

// tif is the draft's time-in-force, always set. A market order is ioc-only;
// a limit order takes the selected tifOptions entry. The market override does
// not touch tifIdx, so the limit-mode selection survives a type flip.
func (d orderDraft) tif() string {
	if d.typ == "market" {
		return "ioc"
	}
	return tifOptions[d.tifIdx]
}

// validate is the internal sizing-matrix guard: it asserts the wire params the
// draft would emit match exactly one supported (type, side) shape, so an entry
// surface that composes an unsupported combination (or leaves a stray field) is
// caught here rather than sent. Defense-in-depth over the server-side check,
// exercised by the unit tests; it assumes the size has already been checked
// non-empty. Both arm paths call it before a review is shown.
func (d orderDraft) validate() error {
	if d.side != "buy" && d.side != "sell" {
		return fmt.Errorf("invalid side %q", d.side)
	}
	if d.typ != "limit" && d.typ != "market" {
		return fmt.Errorf("invalid order type %q", d.typ)
	}
	v := d.values()
	_, hasPrice := v["price"]
	_, hasQty := v["qty"]
	_, hasAmt := v["amt"]
	if hasQty == hasAmt {
		return fmt.Errorf("an order must carry exactly one size field (qty=%t amt=%t)", hasQty, hasAmt)
	}
	switch {
	case d.typ == "limit":
		if !hasPrice {
			return fmt.Errorf("a limit order requires a price")
		}
		if hasAmt {
			return fmt.Errorf("a limit order is placed as a quantity, never an amount")
		}
	case d.side == "buy": // market buy
		if hasPrice {
			return fmt.Errorf("a market order must not carry a price")
		}
		if !hasAmt {
			return fmt.Errorf("a market buy is sized by amount")
		}
	default: // market sell
		if hasPrice {
			return fmt.Errorf("a market order must not carry a price")
		}
		if !hasQty {
			return fmt.Errorf("a market sell is sized by quantity")
		}
	}
	if _, hasPP := v["pp"]; hasPP && d.typ != "market" {
		return fmt.Errorf("price protection applies to market orders only")
	}
	return nil
}

// values renders the draft as validated-wire-shaped params — the same map the
// place operation and ops.AnalyzePlace consume. Only matrix-active fields are
// emitted.
func (d orderDraft) values() map[string]string {
	v := map[string]string{
		"symbol": d.symbol, "side": d.side, "orderType": d.typ,
	}
	if d.usesPrice() && d.price != "" {
		v["price"] = d.price
	}
	if q := d.wireQty(); d.usesQty() && q != "" {
		v["qty"] = q
	}
	if d.usesAmt() && d.amt != "" {
		v["amt"] = d.amt
	}
	v["timeInForce"] = d.tif() // mandatory on every order
	if d.typ == "market" && d.pp {
		v["pp"] = "true"
	}
	return v
}

// form converts the draft to the Trader's OrderForm.
func (d orderDraft) form() OrderForm {
	f := OrderForm{Symbol: d.symbol, Side: d.side, Type: d.typ, TIF: d.tif()}
	if d.usesPrice() {
		f.Price = strings.TrimSpace(d.price)
	}
	if d.usesQty() {
		f.Qty = d.wireQty()
	}
	if d.usesAmt() {
		f.Amt = strings.TrimSpace(d.amt)
	}
	if d.typ == "market" {
		f.PP = d.pp
	}
	return f
}

// sizeValue is the draft's entered size — the amount when in amount mode, else
// the quantity.
func (d orderDraft) sizeValue() string {
	if d.inputAmt() {
		return d.amt
	}
	return d.qty
}

// FeeRates is one symbol's trading-fee policy, as fetched from the fees
// endpoint (decimal-string rate fractions). It crosses the Config seam so the
// preview can estimate the fee and the buy presets can reserve headroom for a
// quote-currency fee.
type FeeRates struct {
	MakerRate       string
	TakerRate       string
	MaxRate         string
	BuyFeeCurrency  string
	SellFeeCurrency string
}

// TickPolicy is one symbol's tick metadata, as fetched from the tick-size
// policy endpoint. It crosses the Config seam: the bands feed the price
// grid (stepping/snapping/off-grid checks), the levels are the valid
// orderbook grouping levels the +/- keys cycle (decimal strings, in no
// promised order — the consumer sorts; the raw, ungrouped book is not among
// them).
type TickPolicy struct {
	Bands  []ops.TickBand
	Levels []string
}

// orderPreview is the live pre-submit analysis of a draft: the preplace
// simulation and warnings evaluated against the CURRENT book, plus the
// display-ready derived figures. Rebuilt whenever the draft or the book
// changes; strings stay wire strings (rendering formats them).
type orderPreview struct {
	OK  bool   // the draft was previewable (see buildPreview for the two cases)
	Err string // why not, when !OK

	// Sim is the book simulation — the zero value only when !OK (nothing was
	// analyzed). Otherwise it is ops.AnalyzePlace's own answer for this book,
	// whatever shape it had: a side with no resting orders yields an EMPTY
	// reference price (BestBid/BestAsk/Mid), never a fabricated "0", so read
	// those for presence before deriving anything from them.
	Sim ops.PlaceSimulation
	// Warnings are the analysis warnings — book-independent ones (the order-value
	// bounds, tick alignment) on any book, the depth-derived ones where there is
	// depth to derive them from.
	Warnings []ops.PlaceWarning

	Notional   string // estimated notional in QuoteCcy ("" when the size is not set yet)
	PctFromMid string // a limit price's signed distance from mid, e.g. "-0.02%"
	TickSize   string // tick size at the draft price ("" when unknown / not limit)
	FeeEst     string // estimated fee in quote terms ("" when the rates are unknown, or the order would pay no fee)
	FeeRate    string // the rate fraction used for FeeEst
	FeeKind    string // "taker" | "maker"; "" alongside an empty FeeEst

	BaseCcy    string
	QuoteCcy   string
	AvailBase  string // available base balance ("" when unknown)
	AvailQuote string // available quote balance ("" when unknown)
}

// buildPreview computes the preview for a draft against the live inputs. Two
// outcomes, keyed on the book's status: a NOT-READY book (still loading, or
// mid-switch) yields OK=false with the reason, because its figures are not
// something the store stands behind; every LIVE book — two-sided, one-sided, or
// wholly orderless — runs the full ops.AnalyzePlace. That analysis handles each
// shape itself, gating each check on the price it needs and reporting an absent
// one as EMPTY rather than zero, so there is exactly ONE implementation of the
// money math behind the panel, the ladder and the command bar — the same one
// `order place --dry-run` prints. Missing bands/bounds/fees/balances just leave
// their derived fields empty or their checks unrun (each is optional and
// best-effort).
func buildPreview(d orderDraft, book state.Orderbook, bookStatus state.DataStatus, bals []state.Balance, bands []ops.TickBand, bounds ops.OrderValueBounds, fees *FeeRates) orderPreview {
	base, quote := splitSymbol(d.symbol)
	p := orderPreview{
		BaseCcy: base, QuoteCcy: quote,
		AvailBase:  availableOf(bals, base),
		AvailQuote: availableOf(bals, quote),
	}
	if bookStatus == state.StatusNotReady {
		// The book is still loading (or mid-switch) — the numbers can't be trusted.
		p.Err = i18n.T("waiting for a live orderbook")
		return p
	}
	sim, ws := ops.AnalyzePlace(d.values(), opsLevels(book.Bids), opsLevels(book.Asks), bands, bounds)
	p.OK = true
	p.Sim = sim
	p.Warnings = ws
	p.Notional = sim.Notional

	if d.usesPrice() && d.price != "" {
		if tick, ok := ops.TickSizeAt(bands, d.price); ok {
			p.TickSize = tick
		}
		p.PctFromMid = pctFromMid(d.price, sim.Mid)
	}
	p.FeeEst, p.FeeRate, p.FeeKind = feeEstimate(sim, p.Notional, fees)
	return p
}

// feeEstimate resolves the fee the draft's SIMULATED OUTCOME would pay, and the
// value that fee is charged on — which is NOT the same amount in both cases:
//
//   - fills: the taker rate on the quote value that actually executes
//     (sim.EstFilledQuote). Only the executing portion is charged a taker fee, so
//     billing the whole request overstates the estimate on any partial fill — most
//     visibly where the rest is definitively gone (a price-protected order that
//     cancels its remainder, a market order that exhausts the book). The fallback
//     to the full notional is unreachable in practice: this outcome is derived from
//     a positive fill, which sets the executed value alongside it.
//   - rests: the maker rate on the full notional — the whole order rests, and the
//     maker fee on all of it is the meaningful forecast.
//   - anything else: no estimate at all.
//
// So the figure means exactly "the fee on what this order executes now". A
// partially crossing limit's resting remainder is deliberately NOT folded in: it
// pays a maker fee only if it later fills, a contingent event this simulation does
// not predict, and a single number blending a committed cost with a speculative one
// is trustworthy as neither. The preview shows the estimated fill beside it, so
// what the figure covers stays visible.
//
// All three returns are empty when there is nothing to charge, no rates loaded, or
// no notional to charge against.
//
// The outcome token is the only sound signal here, and the two obvious substitutes
// are both wrong on a money path. sim.Marketable says the order WOULD take
// liquidity, which is equally true of a crossing post-only (rejected) and an
// unfillable fill-or-kill (killed) — each executes nothing and is charged nothing,
// so a taker fee on either invents a cost. And the draft's own canRest cannot see
// the book: a limit that price protection cancels in full never reaches the book,
// yet its tif says it would rest there. Anything that pays nothing gets no
// estimate — including a market order, which must never be labelled a maker.
func feeEstimate(sim ops.PlaceSimulation, notional string, fees *FeeRates) (est, rate, kind string) {
	if fees == nil || notional == "" {
		return "", "", ""
	}
	charged := notional
	switch sim.Outcome {
	case ops.OutcomeFills:
		rate, kind = fees.TakerRate, "taker"
		if sim.EstFilledQuote != "" {
			charged = sim.EstFilledQuote
		}
	case ops.OutcomeRests:
		rate, kind = fees.MakerRate, "maker"
	default:
		return "", "", ""
	}
	v, ok := mulDec(charged, rate)
	if !ok {
		return "", "", ""
	}
	return roundSigFigs(v, feeSigFigs).String(), rate, kind
}

// fillSideRefusal is the single rule for what the TUI refuses to place against a
// LIVE book. It turns on the ONE side the draft depends on, not on the book as a
// whole: an order that cannot rest needs resting orders on the side it would take
// from (asks for a buy, bids for a sell), and a best (BBO) order needs a level on
// the side it pegs to. So a one-sided book is not uniformly refused — a market
// SELL sweeps a bids-only book normally while the market BUY beside it has
// nothing to fill against — and a wholly orderless book is just the case where
// both sides are missing. "" = placeable (a gtc/po limit rests as the first
// maker). book must be the live book of the draft's own market.
//
// This is the refusing twin of ops.WarnNoOpposingLiquidity
// (internal/ops/preplace.go), which reports the same finding as advice — with one
// case more here: the post-only best order below, which that layer reports as
// BEST_PEG_UNAVAILABLE, so this refusal set is a strict superset of that warning's.
// The duplication is deliberate — a warning is advice an agent may act on or
// override, a gate is a decision this terminal makes for its user — so keep the
// two in step: change one, change the other.
//
// Only the placement gate (model.draftGate) applies it; the preview does not.
// ops.AnalyzePlace already states the same outcome there (an error-styled
// NO_OPPOSING_LIQUIDITY warning, plus a disposition saying nothing would
// execute), and routing the whole preview through one implementation is the
// point.
func fillSideRefusal(d orderDraft, book state.Orderbook) string {
	switch d.typ {
	case "limit":
		// A limit carries its own price, so gtc/po simply rests — on a side with no
		// resting orders it becomes the first maker there, which is unremarkable.
		// ioc/fok cannot rest: the server accepts it and cancels it unfilled.
		if d.canRest() {
			return ""
		}
		if bookSideFilled(fillSideName(d.side), book) {
			return ""
		}
		return i18n.T("the %s side of the book is empty — an ioc/fok limit can't fill and would cancel; use gtc or po to rest as the first maker",
			fillSideName(d.side))

	case "best":
		// A best (BBO) order takes its PRICE from a book level, so an empty peg side
		// leaves it with no price at all — and the peg side is not the tif's to
		// decide alone: a taker tif (gtc/ioc/fok) pegs to the opposing side, while
		// post-only pegs to its OWN queue side. The two po sides of a one-sided book
		// therefore land oppositely: on a bids-only book a po best BUY pegs to the
		// best bid and rests, while the po best SELL beside it dies, its queue side
		// being the empty one.
		peg := fillSideName(d.side)
		if d.tif() == "po" {
			peg = ownSideName(d.side)
		}
		if bookSideFilled(peg, book) {
			return ""
		}
		return i18n.T("the %s side of the book is empty — a best order has no level there to peg to; place a limit order to rest as the first maker", peg)

	default:
		// A market order — and, defensively, any type this file does not know: assume
		// it takes rather than rests, so an unrecognized shape is refused on an empty
		// fill side rather than waved through. (validate() rejects such a shape
		// before any arm, so the wording below only ever describes a market order.)
		if bookSideFilled(fillSideName(d.side), book) {
			return ""
		}
		return i18n.T("the %s side of the book is empty — a market order can't fill; place a limit order to rest as the first maker",
			fillSideName(d.side))
	}
}

// fillSideName and ownSideName name the two book sides from the ORDER's side, in
// the bid/ask vocabulary ops words its own warnings in (trading vocabulary stays
// English — see the localization rules in README.md): a marketable order consumes
// the opposing side (a buy takes asks), while a resting one joins its own side (a
// buy queues among the bids).
func fillSideName(side string) string {
	if side == "sell" {
		return "bid"
	}
	return "ask"
}

func ownSideName(side string) string {
	if side == "sell" {
		return "ask"
	}
	return "bid"
}

// bookSideFilled reports whether the named side ("bid"/"ask") of the book holds
// any resting orders.
func bookSideFilled(name string, book state.Orderbook) bool {
	if name == "bid" {
		return len(book.Bids) > 0
	}
	return len(book.Asks) > 0
}

// pctFromMid renders a limit price's signed distance from the mid ("+0.02%" is
// above the mid, "-0.02%" below); "" when either input is unparseable — which
// includes the EMPTY mid a one-sided book yields, so a book with no two-sided
// touch drops the line instead of showing a fabricated "0%".
func pctFromMid(price, mid string) string {
	pd, err1 := decimal.NewFromString(price)
	md, err2 := decimal.NewFromString(mid)
	if err1 != nil || err2 != nil || !md.IsPositive() {
		return ""
	}
	pct := pd.Sub(md).Div(md).Mul(decimal.NewFromInt(100)).Round(2)
	if pct.IsNegative() {
		return pct.String() + "%"
	}
	return "+" + pct.String() + "%"
}

// defaultOrderLevels are the built-in %-of-balance size presets: a percentage
// of the available balance that funds the order's side. They seed a fresh
// config on the first tui launch and are pinned there (see the tui command), so
// the active levels the order UIs use come from config — this is only the
// initial default a new user gets.
var defaultOrderLevels = []int{10, 25, 50, 100}

// DefaultOrderLevels returns a copy of the built-in size-preset defaults, for
// the cli layer to pin into config.json on first launch.
func DefaultOrderLevels() []int { return append([]int(nil), defaultOrderLevels...) }

// maxSizeLevels caps how many size presets the order UIs bind — one per number
// key 1..9 (the ladder's digit-key cases). It must stay ≥ config.MaxOrderLevels
// (the count config accepts and pins); a mismatch is pinned by a test, so
// raising one without the other fails the build rather than silently dropping
// configured levels here.
const maxSizeLevels = 9

// resolveOrderLevels returns the size presets to use: the configured levels
// (copied so the model owns the slice) when present, else the built-in
// defaults. The cli layer validates and pins config, so a non-empty slice here
// is already sound; the length guard is a defensive fallback.
func resolveOrderLevels(levels []int) []int {
	if len(levels) == 0 || len(levels) > maxSizeLevels {
		return DefaultOrderLevels()
	}
	return append([]int(nil), levels...)
}

// sizeKeysLabel renders the size-preset key range for hints, e.g. "1-4" (the
// presets bind to number keys 1..N), or "1" for a lone level.
func sizeKeysLabel(levels []int) string {
	if len(levels) <= 1 {
		return "1"
	}
	return "1-" + strconv.Itoa(len(levels))
}

// sizeValuesLabel renders the size-preset values for hints, e.g. "10/25/50/max"
// (bare percentages, 100 → "max"), so a hint never disagrees with the
// configured levels shown on the chips.
func sizeValuesLabel(levels []int) string {
	parts := make([]string, len(levels))
	for i, p := range levels {
		if p == 100 {
			parts[i] = "max"
		} else {
			parts[i] = strconv.Itoa(p)
		}
	}
	return strings.Join(parts, "/")
}

// applyPreset sets the draft's active size field to pct% of the funding
// balance: a buy spends pct% of the available quote (converted to base qty at
// the draft price for a limit), a sell offers pct% of the available base.
// When the fee policy says buys pay their fee in the quote currency, the buy
// sizing reserves that headroom (avail / (1+maxFeeRate)) so a 100% preset is
// not rejected for insufficient balance.
//
// The return is a short reason the preset could not be applied ("" on success),
// so the caller can tell the failure modes apart: balances not loaded yet (a
// transient wait), no balance in the sized currency (a real zero the user
// should be told about — a zero balance is not stored as a row, so an absent
// currency once balances are ready is a real zero, not a wait), a limit buy
// without a price, or a balance too small for pct% to round above zero.
func applyPreset(d orderDraft, bals []state.Balance, balancesReady bool, fees *FeeRates, pct int) (orderDraft, string) {
	base, quote := splitSymbol(d.symbol)
	pctFrac := decimal.NewFromInt(int64(pct)).Div(decimal.NewFromInt(100))

	if d.side == "sell" {
		avail, reason := sizingAvail(bals, balancesReady, base)
		if reason != "" {
			return d, reason
		}
		qty := avail.Mul(pctFrac)
		if d.inputAmt() { // limit sell in amount mode: amount = qty × price
			price, ok := parseDec(d.price)
			if !ok || !price.IsPositive() {
				return d, i18n.T("waiting for a limit price")
			}
			d.amt = roundAmt(qty.Mul(price), roundFloor).String()
			return d, zeroPresetReason(d.amt, pct)
		}
		d.qty = roundQty(qty, roundFloor).String()
		return d, zeroPresetReason(d.qty, pct)
	}

	// buy: spend a fraction of the quote balance, reserving quote-fee headroom.
	avail, reason := sizingAvail(bals, balancesReady, quote)
	if reason != "" {
		return d, reason
	}
	spend := avail.Mul(pctFrac) // exact
	// When buys pay the fee in the quote currency the exchange reserves
	// notional*(1+maxFeeRate), so fold that into the divisor. Dividing by it
	// once (via divFloor*, which floors at the wire precision) sizes to a value
	// whose reservation still fits — a 100% preset floors rather than being
	// rejected for insufficient balance.
	feeDiv := decimal.NewFromInt(1)
	if fees != nil && strings.EqualFold(fees.BuyFeeCurrency, quote) {
		if maxRate, ok := parseDec(fees.MaxRate); ok && maxRate.IsPositive() {
			feeDiv = feeDiv.Add(maxRate)
		}
	}
	if d.inputAmt() { // market buy, or limit buy in amount mode
		d.amt = divFloorAmt(spend, feeDiv).String()
		return d, zeroPresetReason(d.amt, pct)
	}
	price, ok := parseDec(d.price)
	if !ok || !price.IsPositive() {
		return d, i18n.T("waiting for a limit price")
	}
	// One floored division by (1+maxFeeRate)*price — the divisor is exact
	// (Mul), so there is no intermediate rounding to compound.
	d.qty = divFloorQty(spend, feeDiv.Mul(price)).String()
	return d, zeroPresetReason(d.qty, pct)
}

// sizingAvail resolves the balance a preset sizes from in the given currency,
// telling "balances not loaded yet" apart from "loaded, but nothing here".
// reason=="" means the returned amount is usable (positive).
func sizingAvail(bals []state.Balance, balancesReady bool, currency string) (decimal.Decimal, string) {
	d, ok := parseDec(availableOf(bals, currency))
	switch {
	case ok && d.IsPositive():
		return d, ""
	case balancesReady:
		return decimal.Decimal{}, i18n.T("no %s available", strings.ToUpper(currency))
	default:
		return decimal.Decimal{}, i18n.T("waiting for balances")
	}
}

// zeroPresetReason reports why a resolved size is unusable — pct% of a real but
// tiny balance can truncate to zero. reason=="" when size is positive.
func zeroPresetReason(size string, pct int) string {
	if d, ok := parseDec(size); !ok || !d.IsPositive() {
		return i18n.T("%s of your balance rounds to zero", presetLabel(pct))
	}
	return ""
}

// qtyFromAmt derives a base quantity from a quote amount at price (qty =
// amt / price, via roundQty). The API sizes only a market buy in quote terms,
// so an amount on any limit order (either side) is placed by deriving the
// quantity here at the resolved price. ok=false when the price is not yet
// positive; qty is "" when the amount is too small to round above zero.
func qtyFromAmt(amt, price string) (qty string, ok bool) {
	a, okA := parseDec(amt)
	p, okP := parseDec(price)
	if !okA || !okP || !p.IsPositive() {
		return "", false
	}
	q := divFloorQty(a, p)
	if !q.IsPositive() {
		return "", true
	}
	return q.String(), true
}

// snapPriceForSide snaps an off-grid price onto the tick grid in the side's
// conservative direction: down for a buy (never bid above the reference), up
// for a sell (never ask below it).
func snapPriceForSide(bands []ops.TickBand, price, side string) (string, bool) {
	if side == "sell" {
		return ops.SnapUpToTick(bands, price)
	}
	return ops.SnapToTick(bands, price)
}

// anchorPrice resolves a price anchor against the live market: "bid"/"ask"
// are the book's best levels, "mid" is their midpoint snapped onto the tick
// grid in the side's direction (when the policy is known — a raw mid can sit
// between ticks), "last" is the ticker's last price. ok=false when the backing
// data is absent.
func anchorPrice(anchor, side string, book state.Orderbook, hasBook bool, t state.Ticker, hasTicker bool, bands []ops.TickBand) (string, bool) {
	switch anchor {
	case "bid":
		if hasBook && len(book.Bids) > 0 {
			return book.Bids[0].Price, true
		}
	case "ask":
		if hasBook && len(book.Asks) > 0 {
			return book.Asks[0].Price, true
		}
	case "mid":
		if !hasBook || len(book.Bids) == 0 || len(book.Asks) == 0 {
			return "", false
		}
		bid, ok1 := parseDec(book.Bids[0].Price)
		ask, ok2 := parseDec(book.Asks[0].Price)
		if !ok1 || !ok2 {
			return "", false
		}
		mid := bid.Add(ask).Div(decimal.NewFromInt(2))
		if snapped, ok := snapPriceForSide(bands, mid.String(), side); ok {
			return snapped, true
		}
		return mid.String(), true
	case "last":
		if hasTicker && t.Close != "" {
			return t.Close, true
		}
	}
	return "", false
}

// placeGateReason is the submit gate: "" when an order may be armed/placed,
// otherwise why not. Unlike the display panes (which just show "loading…"),
// placement must refuse to run against a stale or still-loading book — the
// preview's numbers and the user's intent were formed against data the store
// no longer stands behind. bookLive is true once the orderbook subscription is
// settled, whether or not it has resting orders: a book missing a side is a valid
// place target for a resting limit (the first maker there), so only a NOT-ready
// book gates here — which orders a missing side refuses depends on the draft and
// is draftGate's half (fillSideRefusal). The fee policy is part of the gate for the same
// reason: the review's fee estimate and the order's local balance hold both
// size from it, so arming waits for the (async, retried) fetch rather than
// reviewing against unknown fees.
func placeGateReason(inFlight, bookLive, feesKnown bool) string {
	switch {
	case inFlight:
		return i18n.T("an order action is already in flight — wait for it to finish")
	case !bookLive:
		return i18n.T("the orderbook is not live yet — wait for it to load")
	case !feesKnown:
		return i18n.T("the fee policy is not loaded yet — wait for it to load")
	}
	return ""
}

// --- small helpers ---

// splitSymbol splits a trading pair into base and quote ("btc_krw" → btc, krw).
func splitSymbol(symbol string) (base, quote string) { return ops.SplitSymbol(symbol) }

// availableOf finds the available balance for a currency ("" when unknown).
func availableOf(bals []state.Balance, currency string) string {
	for _, b := range bals {
		if strings.EqualFold(b.Currency, currency) {
			return b.Available
		}
	}
	return ""
}

// opsLevels converts the store's book levels to the analysis form.
func opsLevels(in []state.PriceLevel) []ops.BookLevel {
	out := make([]ops.BookLevel, len(in))
	for i, l := range in {
		out[i] = ops.BookLevel{Price: l.Price, Qty: l.Qty}
	}
	return out
}

// parseDec parses a decimal string; ok=false when empty or unparseable.
func parseDec(s string) (decimal.Decimal, bool) {
	if strings.TrimSpace(s) == "" {
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Decimal{}, false
	}
	return d, true
}

// mulDec multiplies two decimal strings; ok=false when either is unparseable.
// feeSigFigs is how many significant figures an estimated fee is DISPLAYED to.
// A fee is denominated in the pair's quote currency, so its magnitude spans
// whole thousands on one market and a small fraction on another; a fixed number
// of decimal places is either noise on the first or a rounded-to-zero fee on the
// second. Significant figures read correctly on both. Display only — the fee the
// exchange charges is exact.
const feeSigFigs = 3

// roundSigFigs rounds d to n significant figures (half-up).
func roundSigFigs(d decimal.Decimal, n int32) decimal.Decimal {
	if d.IsZero() {
		return d
	}
	// The coefficient's digit count plus the exponent places the leading digit:
	// d is in [10^mag, 10^(mag+1)), so keeping n figures means rounding at
	// n-1-mag decimal places (negative places round to tens/hundreds/…).
	mag := int32(d.NumDigits()) + d.Exponent() - 1
	return d.Round(n - 1 - mag)
}

func mulDec(a, b string) (decimal.Decimal, bool) {
	ad, ok1 := parseDec(a)
	bd, ok2 := parseDec(b)
	if !ok1 || !ok2 {
		return decimal.Decimal{}, false
	}
	return ad.Mul(bd), true
}

// reqDecimalPlaces caps qty/amt/price precision on an order request; a value is
// floored to it before placement, the same for every pair and currency.
const reqDecimalPlaces = 8

// roundMode selects how roundQty/roundAmt round to reqDecimalPlaces.
type roundMode int

const (
	roundFloor roundMode = iota
	roundCeil
)

func applyRound(v decimal.Decimal, m roundMode) decimal.Decimal {
	if m == roundCeil {
		return v.RoundCeil(reqDecimalPlaces)
	}
	return v.RoundFloor(reqDecimalPlaces)
}

// roundQty applies the shared quantity precision policy.
func roundQty(v decimal.Decimal, m roundMode) decimal.Decimal { return applyRound(v, m) }

// roundAmt applies the shared amount precision policy.
func roundAmt(v decimal.Decimal, m roundMode) decimal.Decimal { return applyRound(v, m) }

// divFloorQty divides a by b and floors the quotient to the request precision.
// It uses QuoRem so the truncation is exact and set by reqDecimalPlaces rather
// than the package-global decimal.DivisionPrecision — a plain a.Div(b) computes
// only DivisionPrecision (16) places and rounds half-away-from-zero there, so
// flooring its result is only floor-safe while reqDecimalPlaces stays below 16.
// Order sizes are always positive, so truncating toward zero is a floor: the
// quotient never exceeds a/b, and a balance-sized order never rounds UP into an
// insufficient-balance rejection.
func divFloorQty(a, b decimal.Decimal) decimal.Decimal {
	q, _ := a.QuoRem(b, reqDecimalPlaces)
	return q
}

// divFloorAmt is divFloorQty for a quote amount; qty and amt share the request
// precision and floor policy.
func divFloorAmt(a, b decimal.Decimal) decimal.Decimal { return divFloorQty(a, b) }

// floorStr floors a decimal string to the request precision, leaving an
// unparseable (partial) edit untouched.
func floorStr(s string, round func(decimal.Decimal, roundMode) decimal.Decimal) string {
	d, ok := parseDec(s)
	if !ok {
		return s
	}
	return round(d, roundFloor).String()
}
