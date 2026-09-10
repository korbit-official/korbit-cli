// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
)

// Customer-protection thresholds for the pre-place dry-run analysis. They are
// advisory: a crossed threshold produces a warning, never a block.
var (
	// slippageWarnPct: warn when the volume-weighted fill price of a marketable
	// order is worse than the best price on its side by more than this percent.
	slippageWarnPct = decimal.NewFromFloat(1.0)
	// fatFingerWarnPct: warn when a limit price sits more than this percent away
	// from the mid in the UNFAVORABLE direction (a buy far above / a sell far
	// below) — the classic extra-digit mistake.
	fatFingerWarnPct = decimal.NewFromFloat(20.0)
	oneHundred       = decimal.NewFromInt(100)
	two              = decimal.NewFromInt(2)
)

// PlaceWarningCode is the stable symbolic identifier of a pre-place warning —
// the value of PlaceWarning.Code. Its string values are part of the `--json`
// contract an agent branches on, so they never change; the typed constants
// below are the single source for the set (referenced by the analysis that
// emits them and by any consumer that switches on them).
type PlaceWarningCode string

const (
	WarnPriceFarAboveMarket   PlaceWarningCode = "PRICE_FAR_ABOVE_MARKET"
	WarnPriceFarBelowMarket   PlaceWarningCode = "PRICE_FAR_BELOW_MARKET"
	WarnPostOnlyWouldReject   PlaceWarningCode = "POST_ONLY_WOULD_REJECT"
	WarnFOKWouldKill          PlaceWarningCode = "FOK_WOULD_KILL"
	WarnIOCWouldExpire        PlaceWarningCode = "IOC_WOULD_EXPIRE"
	WarnNotionalBelowMin      PlaceWarningCode = "NOTIONAL_BELOW_MIN"
	WarnNotionalAboveMax      PlaceWarningCode = "NOTIONAL_ABOVE_MAX"
	WarnPriceOffTick          PlaceWarningCode = "PRICE_OFF_TICK"
	WarnInsufficientLiquidity PlaceWarningCode = "INSUFFICIENT_LIQUIDITY"
	WarnHighSlippage          PlaceWarningCode = "HIGH_SLIPPAGE"
	WarnBookDepthLimited      PlaceWarningCode = "BOOK_DEPTH_LIMITED"
	WarnPriceProtectionCapped PlaceWarningCode = "PRICE_PROTECTION_CAPPED"
	WarnBestPegUnavailable    PlaceWarningCode = "BEST_PEG_UNAVAILABLE"
	// WarnNoOpposingLiquidity: the side this order would take from holds no
	// resting orders, AND this order cannot rest — so nothing would execute. It
	// carries exactly that one meaning, because a consumer styles a warning by its
	// code alone (the TUI renders some codes as errors): a code that meant "fatal"
	// for one order and "normal first maker" for another could not be styled at
	// all. An order that CAN rest is therefore not warned here — resting on an
	// empty side is the ordinary outcome, already stated by the simulation's
	// disposition, unfilled quantity and empty reference prices. Deliberately
	// distinct from WarnInsufficientLiquidity, which means the book ran out
	// part-way through a fill; here there was nothing to fill against at all.
	//
	// The same rule exists a second time as a UI policy: the TUI's
	// fillSideRefusal (internal/tui/orderentry.go) REFUSES to arm the orders warned
	// here, instead of advising. Its refusal set is a strict SUPERSET, because it
	// also refuses a post-only best order whose queue side is empty — the case this
	// layer reports as WarnBestPegUnavailable, the more accurate reason. The
	// duplication is deliberate — a warning is advice an agent may override, a gate
	// is a decision a terminal makes for its user — so keep the two in step: change
	// one, change the other.
	WarnNoOpposingLiquidity PlaceWarningCode = "NO_OPPOSING_LIQUIDITY"
	// WarnMidPriceUnavailable: no mid could be computed (it needs a best price on
	// both sides), so the checks measured against it — a limit's price sanity and
	// --pp price protection — did not run. Emitted only when one of them would
	// otherwise have applied, and never as evidence that the price is sound. The
	// message names the check(s) that actually applied to THIS order, since on most
	// orders only one of the two ever could.
	WarnMidPriceUnavailable PlaceWarningCode = "MID_PRICE_UNAVAILABLE"
)

// PlaceWarning is one advisory customer-protection finding produced by the
// pre-place dry-run analysis. It never blocks a placement; it tells the caller
// the order is risky (would sweep the book to a bad price, would be rejected by
// the server, looks like a fat-finger) so an agent can decide before sending.
type PlaceWarning struct {
	// Code is a stable symbolic identifier an agent can branch on.
	Code PlaceWarningCode `json:"code"`
	// Message is the human-readable warning in English, rendered from Format and
	// Args. It is the value shipped in the `--json` contract.
	Message string `json:"message"`
	// Format and Args are the un-rendered message: Format is the English template
	// (printf %s verbs only) and Args its interpolation values, with Message equal
	// to fmt.Sprintf(Format, Args...). They let a consumer re-render the message
	// its own way — fmt.Sprintf for English, or a localizing printer keyed on the
	// Format string for another language — so this layer needs no localization
	// dependency of its own. Not part of the JSON contract (localization is a
	// display concern of the consumer, not the wire shape agents read).
	Format string `json:"-"`
	Args   []any  `json:"-"`
}

// PlaceOutcome is the structural verdict of the simulation — what becomes of the
// order against the current book — as a stable token a consumer branches on. Its
// string values are part of the `--json` contract, so they never change.
//
// It exists because no other field answers that question. Marketable and the Est*
// fields describe the hypothetical crossing (a killed fill-or-kill and a filled
// market order are both marketable, and only one of them executes), and
// Disposition, which does state the outcome, is prose written for a human to read
// — never to be pattern-matched. Anything that must DECIDE on the outcome — a fee
// estimate, a UI label, an agent's branch — reads this.
type PlaceOutcome string

const (
	// OutcomeFills: some or all of the order executes immediately. A partial fill
	// whose remainder rests or cancels is still this — something trades now.
	OutcomeFills PlaceOutcome = "fills"
	// OutcomeRests: nothing executes now, and the order sits on the book as a maker
	// waiting to be filled — a non-crossing gtc/po order, including the first maker
	// on a side with no resting orders.
	OutcomeRests PlaceOutcome = "rests"
	// OutcomeNothing: nothing executes AND nothing rests. The order is rejected (a
	// crossing post-only), killed (a fill-or-kill that cannot fill in full), expired
	// (a non-crossing IOC), unpriceable (a best order whose peg level is not
	// visible), or canceled in whole — a market/IOC order with no liquidity to take,
	// or an order price protection cancels in full.
	OutcomeNothing PlaceOutcome = "nothing"
)

// SimulationDisclaimer is attached to every PlaceSimulation. It is load-bearing:
// the numbers are a simulation against the current public orderbook, NOT the
// outcome of a real placement — the book moves between this estimate and a real
// send, and hidden/iceberg liquidity and fees are not modeled.
const SimulationDisclaimer = "ESTIMATE ONLY — simulated against the current public orderbook; nothing was placed. The real fill will differ as the book moves, and hidden liquidity and fees are not modeled."

// PlaceSimulation is the estimated outcome of the order against the current
// public orderbook — what the order WOULD do if sent now. Every money/quantity
// field is a decimal string. It is a simulation, not a guarantee (see
// SimulationDisclaimer).
type PlaceSimulation struct {
	// Disclaimer is always set and states plainly that this is an estimate.
	Disclaimer string `json:"disclaimer"`

	// BestBid, BestAsk and Mid are the reference prices the analysis measured the
	// order against. Each is EMPTY when the book cannot supply it: a side with no
	// resting orders has no best price, and the mid needs one on both sides. They
	// are never zero — a fabricated "0" reads as a real (and extremely favorable)
	// price and would corrupt any arithmetic or comparison a caller does on it.
	// The three keys are always present in the JSON; only the value can be empty.
	BestBid string `json:"bestBid"`
	BestAsk string `json:"bestAsk"`
	Mid     string `json:"mid"`

	// QuoteCurrency is the currency Notional and EstFilledQuote are denominated
	// in: the one the pair publishes, or the symbol's quote segment ("btc_krw" →
	// "krw") when the pair's entry was not supplied. Empty when neither is known.
	QuoteCurrency string `json:"quoteCurrency,omitempty"`
	// Notional is the order's estimated value in QuoteCurrency ("" when the size
	// is not determinable from the request).
	Notional string `json:"notional,omitempty"`

	// EstPegPrice is the price a best (BBO) order derives from the current book —
	// its --best-nth level on the side its tif selects. Empty for non-best orders
	// and when that level is not visible in the book.
	EstPegPrice string `json:"estPegPrice,omitempty"`

	// Marketable is true when the order (or a limit's crossing portion) would
	// take liquidity immediately. The Est* fields describe that simulated taker
	// fill; they are empty for a non-crossing limit (which rests in full).
	//
	// It answers "would this take liquidity", NOT "does this execute": a crossing
	// post-only is rejected and an unfillable fill-or-kill is killed, both
	// marketable and both executing nothing. Read Outcome for what becomes of the
	// order.
	Marketable        bool   `json:"marketable"`
	EstFilledQty      string `json:"estFilledQty,omitempty"`
	EstFilledQuote    string `json:"estFilledQuote,omitempty"`
	EstAvgFillPrice   string `json:"estAvgFillPrice,omitempty"`
	EstWorstFillPrice string `json:"estWorstFillPrice,omitempty"`
	EstSlippagePct    string `json:"estSlippagePct,omitempty"`

	// FullyFilled reports whether the whole order size is covered by the visible
	// marketable book.
	FullyFilled bool `json:"fullyFilled"`
	// EstRemainingQty is the base quantity that would NOT fill immediately;
	// Disposition says what becomes of it (rests as a maker limit, or is canceled
	// for a market/IOC order). It is omitted only for a market BUY (quote-sized, so
	// a base remainder is undefined — Disposition explains); a best BUY is
	// base-sized (amt is converted to qty at the peg) and does report it.
	EstRemainingQty string `json:"estRemainingQty,omitempty"`
	Disposition     string `json:"remainingDisposition,omitempty"`
	// Outcome is Disposition's structural counterpart: the same verdict as one of
	// three stable tokens (see PlaceOutcome), for a consumer that must BRANCH on it
	// instead of displaying it. Set wherever a Disposition is; empty only when the
	// request carries no determinable size yet (a UI draft mid-entry), where there
	// is no outcome to state.
	Outcome PlaceOutcome `json:"outcome,omitempty"`
}

// BookLevel is one orderbook level as its wire strings (price and base
// quantity, both decimal strings). It is the transport-free form AnalyzePlace
// consumes, so a caller can feed levels from any source — the REST fetch
// PrePlaceCheck does, or a live WebSocket book a UI already holds.
type BookLevel struct {
	Price string
	Qty   string
}

// PrePlaceCheck runs the customer-protection analysis behind `order place
// --dry-run`. It fetches PUBLIC market data only (orderbook, tick-size policy,
// and the pair listing the order value bounds come from) through the supplied
// creds-less client, so it signs nothing and works before any key is set up.
// The returned warnings are advisory. A non-nil error means the orderbook could
// not be FETCHED (offline, or an invalid/untradable symbol) and no analysis
// happened at all — the caller still emits its plan, just without the safety
// checks. A book that arrives with an empty side is not that case: it is
// analyzed, degrading per AnalyzePlace.
//
// The orderbook is sufficient for every order type: its top levels ARE the best
// bid/ask, and walking it simulates exactly what a marketable order (a market /
// best order, or the crossing portion of a limit order) would fill at.
func PrePlaceCheck(ctx context.Context, raw *rawapi.Client, values map[string]string) (PlaceSimulation, []PlaceWarning, error) {
	req := placeRequest(values)

	book, _, _, err := raw.Orderbook(ctx, rawapi.OrderbookRequest{Symbol: req.Symbol}, apiclient.Policy{Idempotent: true})
	if err != nil {
		return PlaceSimulation{}, nil, err
	}
	// The tick-size policy backs a secondary, limit-only check; fetching it is
	// best-effort (nil bands just skip the alignment check).
	var bands []TickBand
	if req.OrderType == "limit" {
		bands = fetchTickBands(ctx, raw, req.Symbol)
	}
	sim, ws := AnalyzePlace(values, toBookLevels(book.Bids), toBookLevels(book.Asks), bands, fetchOrderValueBounds(ctx, raw, req.Symbol))
	return sim, ws, nil
}

// toBookLevels converts fetched orderbook levels to the transport-free form.
func toBookLevels(in []rawapi.OrderbookLevel) []BookLevel {
	out := make([]BookLevel, len(in))
	for i, l := range in {
		out[i] = BookLevel{Price: l.Price, Qty: l.Qty}
	}
	return out
}

// AnalyzePlace is the pure core of the pre-place analysis: the same simulation
// and warnings as PrePlaceCheck, over a caller-supplied orderbook snapshot,
// (optional) tick-size policy, and (optional) order value bounds — no I/O.
// values are the order's validated wire params (the same map the place
// operation runs on); bids/asks are the book's levels in any order (they are
// sorted defensively); nil/empty bands skip the tick-alignment check; a zero
// bounds, or one whose Min/Max the pair does not publish, skips the
// corresponding bound check. It returns NO error, deliberately: every input is
// pre-validated or optional and every book shape has an answer (see below), so
// the analysis has no failure mode of its own — and a permanently-nil error
// return would only invite dead error handling at each call site.
//
// A book missing a side does not abort the analysis, because most of it does not
// need the missing price. Two independent axes, both read off the ORDER's side
// rather than the book alone:
//
//   - the FILL side — the side the order takes from (asks for a buy, bids for a
//     sell). Without it nothing fills immediately: an order that can rest becomes
//     a maker, one that cannot does not execute. A one-sided book is therefore
//     NOT uniformly degraded — a sell into a bids-only book sweeps normally.
//   - the MID — needs a best price on both sides. Without it a limit's price
//     sanity check and --pp price protection cannot be evaluated at all.
//
// Every check is gated on the price it actually needs, and a price the book
// cannot supply is reported as EMPTY rather than zero (see PlaceSimulation).
// Substituting zero does not merely lose a check, it inverts one: zero is a real
// and extremely favorable price, so a buy "crosses" it and a protection band
// collapses onto it, turning an absent check into a confident wrong answer.
// A check that could not run says so (MID_PRICE_UNAVAILABLE) instead of leaving
// the warnings empty, which reads as "safe to place", and an order that cannot
// execute at all says that (NO_OPPOSING_LIQUIDITY). An order that merely rests
// as a maker is NOT warned: that is the ordinary outcome and the simulation
// already states it.
func AnalyzePlace(values map[string]string, bidLevels, askLevels []BookLevel, bands []TickBand, bounds OrderValueBounds) (PlaceSimulation, []PlaceWarning) {
	req := placeRequest(analysisValues(values))

	asks := sortedLevels(askLevels, true)  // ascending: best ask first
	bids := sortedLevels(bidLevels, false) // descending: best bid first
	// The reference prices are OPTIONAL: a side with no resting orders has no best
	// price, and the mid needs both. Read the has* flags, never the zero decimal.
	hasAsk, hasBid := len(asks) > 0, len(bids) > 0
	var bestAsk, bestBid, mid decimal.Decimal
	if hasAsk {
		bestAsk = asks[0].price
	}
	if hasBid {
		bestBid = bids[0].price
	}
	hasMid := hasAsk && hasBid
	if hasMid {
		mid = bestBid.Add(bestAsk).Div(two)
	}

	side := string(req.Side)
	typ := string(req.OrderType)
	tif := strings.ToLower(tifStr(req.TimeInForce))

	// Whether the request carries a determinable SIZE, read from whichever field
	// sizes this shape: a market or best BUY spends a quote amt, everything else is
	// sized by a base qty. It gates the VERDICT at the end of the analysis — see the
	// clear before the return.
	sizeField := req.Qty
	if (typ == "market" || typ == "best") && side == "buy" {
		sizeField = req.Amt
	}
	_, hasSize := decOf(sizeField)

	// Every quote-denominated field below is in this currency: the one the pair
	// publishes, falling back to the symbol's second segment when the bounds were
	// not supplied. The payload states it so a caller never has to infer it.
	quote := bounds.QuoteCurrency
	if quote == "" {
		quote = QuoteOf(strings.ToLower(string(req.Symbol)))
	}

	sim := PlaceSimulation{
		Disclaimer:    SimulationDisclaimer,
		QuoteCurrency: quote,
	}
	// A price the book cannot supply stays the empty string; dec(zero) would
	// publish "0" as if it were a real quote.
	if hasBid {
		sim.BestBid = dec(bestBid)
	}
	if hasAsk {
		sim.BestAsk = dec(bestAsk)
	}
	if hasMid {
		sim.Mid = dec(mid)
	}
	var ws []PlaceWarning
	add := func(code PlaceWarningCode, format string, a ...any) {
		// Keep the template and args alongside the rendered English Message so a
		// display layer can re-render (e.g. localize) without this layer depending
		// on any localization machinery. Format strings carry only %s verbs.
		ws = append(ws, PlaceWarning{Code: code, Message: fmt.Sprintf(format, a...), Format: format, Args: a})
	}

	// The side of the book a marketable fill consumes, that side's best price, and
	// whether it exists at all. hasBest=false is the fill side of an empty or
	// one-sided book: nothing fills immediately, whatever the other side holds.
	levels, best, hasBest := asks, bestAsk, hasAsk
	if side == "sell" {
		levels, best, hasBest = bids, bestBid, hasBid
	}
	// --best-nth, 1-based.
	nth := 0
	if req.BestNth != nil {
		nth = *req.BestNth
	}
	// A best (BBO) order's peg — the --best-nth level on the side its tif pegs to.
	// Resolved once here because three separate answers hang off it: whether a
	// post-only best order can rest at all (canRest below), the simulated fill in
	// the best branch, and the reference price the order's value is measured at (a
	// best order rests at, or crosses to, its peg).
	var peg decimal.Decimal
	hasPeg := false
	if typ == "best" {
		peg, hasPeg = bestPeg(tif, side, bids, asks, nth)
	}
	// What an empty fill side means for this order — three outcomes, not
	// interchangeable:
	//
	//   canRest: it sits on the book as a maker instead of taking, so an empty fill
	//     side is unremarkable. A gtc/po limit; and a post-only BEST order, which
	//     pegs to its OWN (queue) side rather than the opposing one — on a bids-only
	//     book a po best BUY pegs to the best bid and rests, while a po best SELL,
	//     whose queue side is the empty one, cannot.
	//   unpriceable: a post-only best order its own side cannot price. It does not
	//     rest either, but the peg — not the missing opposing side — is the reason,
	//     and BEST_PEG_UNAVAILABLE below states it.
	//   neither: a market order, an ioc/fok limit, a taker-tif best order. Nothing
	//     executes, which is what NO_OPPOSING_LIQUIDITY reports.
	canRest := typ == "limit" && tif != "ioc" && tif != "fok"
	unpriceable := false
	if typ == "best" && tif == "po" {
		canRest = hasPeg
		unpriceable = !hasPeg
	}
	// ppBound is protectionBound gated on the mid EXISTING. Protection is defined
	// as a percentage band around the mid, so with no mid there is no band and
	// protection is simply not in effect. Passing a zero mid instead would build a
	// band around zero, which excludes every level in the book: the sweep would
	// break on its first level and report the whole order as protection-canceled.
	ppBound := func() (decimal.Decimal, bool) {
		if !hasMid {
			return decimal.Decimal{}, false
		}
		return protectionBound(req, side, mid)
	}

	// State the outcome of an empty fill side up front, so the sweep-derived
	// wording below cannot describe a book that was never populated as exhausted.
	// The branches that can be more specific (a rejection, a kill, an expiry, a
	// best order's peg) overwrite it; fillSimulation deliberately does not.
	if !hasBest {
		if canRest {
			// Not warned: this is the ordinary first-maker outcome, and the simulation
			// already carries it in full (nothing marketable, the whole quantity
			// unfilled, and no reference price on the missing side).
			sim.Disposition = fmt.Sprintf("rests on the book as a maker limit order — the %s side of the book is empty, so nothing fills immediately", oppSideName(side))
			sim.Outcome = OutcomeRests
		} else {
			sim.Disposition = fmt.Sprintf("no fill — the %s side of the book is empty, so there is no liquidity to take; nothing executes", oppSideName(side))
			sim.Outcome = OutcomeNothing
			if !unpriceable {
				// Name the tif, not just the order type: what cannot rest here is an
				// ioc/fok limit, while an otherwise identical gtc limit rests happily.
				// Naming the type alone would read as a claim about every limit order,
				// contradicting the resting case above.
				kind := typ
				if typ == "limit" {
					kind = tif + " limit"
				}
				add(WarnNoOpposingLiquidity, "the %s side of the book is empty, so this %s %s has nothing to fill against and would not execute — a limit order with --tif gtc would rest on the book instead.",
					oppSideName(side), kind, side)
			}
		}
	}
	// Report the checks the missing mid suppressed — but only where one of them
	// would otherwise have run: a limit's price sanity check needs a price to
	// sanity-check (a draft still being typed has none, and the mid is not what
	// stopped that check), and the --pp estimate applies only to an order that asked
	// for protection. For anything else the mid is unused, so the warning would be
	// pure noise.
	//
	// Which of the two applied decides the wording. Naming both on an order only one
	// could ever have run — a plain limit never asked for protection, a protected
	// market order has no limit price to sanity-check — is true but reads as a lost
	// check, and on a money path an invented loss is as misleading as a hidden one.
	// Every variant keeps the load-bearing clause: silence here is not a pass.
	_, hasLimitPrice := decOf(req.Price)
	limitCheck := typ == "limit" && hasLimitPrice
	ppCheck := req.PP != nil && *req.PP
	if !hasMid {
		switch {
		case limitCheck && ppCheck:
			add(WarnMidPriceUnavailable, "no mid price can be computed — the book has no resting orders on one or both sides, so neither the limit price sanity check (a price far from the market) nor the --pp price protection estimate ran. Their absence is not evidence the price is sound; check it against another source.")
		case limitCheck:
			add(WarnMidPriceUnavailable, "no mid price can be computed — the book has no resting orders on one or both sides, so the limit price sanity check (a price far from the market) did not run. Its absence is not evidence the price is sound; check it against another source.")
		case ppCheck:
			add(WarnMidPriceUnavailable, "no mid price can be computed — the book has no resting orders on one or both sides, so the --pp price protection estimate did not run. Its absence is not evidence the fill will be held where you expect; check the price against another source.")
		}
	}

	switch typ {
	case "market":
		// Marketable in full. Buy is sized by amt (quote), sell by qty (base). Any
		// remainder a market/IOC order can't fill is canceled. With price protection
		// (--pp) the taker fill is held to within ppPercent% of the mid; anything
		// past that band is canceled rather than filled at a worse price.
		quoteTarget := side == "buy"
		target, ok := decOf(req.Amt)
		if !quoteTarget {
			target, ok = decOf(req.Qty)
		}
		if ok {
			ppCap, ppOn := ppBound()
			sw := sweep(levels, side, ppCap, ppOn, target, quoteTarget)
			fillSimulation(&sim, side, sw, best, !quoteTarget, target, false)
			// The pp cap (not a lack of liquidity) stopped the sweep when it broke
			// before the book ran out and before the target was met.
			trimmed := ppOn && !sw.fullyFilled && !sw.reachedEnd
			if trimmed {
				addProtectionCapped(add, &sim, typ, side, ppPercentStr(req), mid, ppCap)
			}
			analyzeSweep(add, side, typ, sw, best, false, trimmed)
		}

	case "best":
		// A best (BBO) order is priced off an existing book level chosen by
		// --best-nth and the tif — the opponent side's Nth level for a taker tif
		// (gtc/ioc/fok), the own (queue) side's Nth level for post-only (po) — then
		// behaves like a limit order at that derived price. Simulate the derived
		// price and its crossing.
		if !hasPeg {
			// Fewer than --best-nth levels are visible on the pegging side, so the
			// price can't be derived from the current book. Flag that the order would
			// likely take no liquidity (rather than leave warnings empty, which reads
			// as "safe to place"), and report only the reference prices — with no peg
			// there is no price to value a base-sized SELL at either, so its notional
			// stays empty (a BUY is valued by its own --amt regardless).
			sim.Disposition = "no fill — fewer than --best-nth price levels are visible on the side this order pegs to, so no peg price can be set from the current book"
			sim.Outcome = OutcomeNothing
			add(WarnBestPegUnavailable, "this best (BBO) %s can't be priced — the current book has fewer levels on the side it pegs to than --best-nth requires, so it would likely take no liquidity.",
				side)
			break
		}
		sim.EstPegPrice = dec(peg)
		if tif == "po" {
			// Post-only pegs to the own side, so on a normal book it sits inside the
			// spread and rests as a maker — it never takes. The crossing check below
			// therefore only fires on a transiently locked/crossed book snapshot; it
			// is kept as a safety net, mirroring the crossing post-only limit rule.
			// It needs the OPPOSING best to compare against (the fill side's), so an
			// empty opposing side skips it: nothing can be crossed there, and a zero
			// would make every buy peg look like a crossing one.
			if hasBest && ((side == "buy" && peg.GreaterThanOrEqual(bestAsk)) || (side == "sell" && peg.LessThanOrEqual(bestBid))) {
				sim.Marketable = true
				sim.Disposition = "REJECTED — a post-only order priced to cross the book is rejected by the server; nothing executes"
				sim.Outcome = OutcomeNothing
				add(WarnPostOnlyWouldReject, "this post-only (--tif po) best %s pegs to %s, which crosses the book (best %s %s) — a post-only order that would take liquidity is rejected.",
					side, dec(peg), oppSideName(side), dec(best))
			} else {
				sim.Disposition = fmt.Sprintf("rests on the book as a maker limit order at %s", dec(peg))
				sim.Outcome = OutcomeRests
			}
			break
		}
		// Taker tif (gtc/ioc/fok): the order matches like a limit at the peg. A best
		// order is base-sized here: a SELL uses --qty directly, and a BUY sized by
		// --amt is converted to a base quantity at the peg (qty = amt/peg) up front
		// and matched as that fixed quantity — NOT a market-style quote-budget sweep.
		// Price protection can hold the taker fill tighter than the peg.
		qty, qok := bestQty(req, side, peg)
		if !qok {
			break
		}
		limitPrice, ppBinds := peg, false
		if ppCap, ppOn := ppBound(); ppOn {
			limitPrice = tighter(side, peg, ppCap)
			ppBinds = limitPrice.Equal(ppCap) && !ppCap.Equal(peg)
		}
		sw := sweep(levels, side, limitPrice, true, qty, false)
		switch {
		case tif == "fok" && !sw.fullyFilled:
			// Fill-or-kill that can't fill in full — within the pp band when it binds,
			// else at the peg — executes nothing.
			sim.Marketable = sw.filledBase.IsPositive()
			sim.Disposition = "KILLED — a fill-or-kill order that cannot fill in full executes nothing"
			sim.Outcome = OutcomeNothing
			if ppBinds {
				add(WarnFOKWouldKill, "this is a fill-or-kill (--tif fok) best %s pegged to %s, but price protection (--pp) limits the immediate fill to %s within %s%% of the mid %s — a FOK that cannot fill in full is KILLED (nothing executes).",
					side, dec(peg), dec(sw.filledBase), ppPercentStr(req), dec(mid))
			} else {
				add(WarnFOKWouldKill, "this is a fill-or-kill (--tif fok) best %s pegged to %s, but it cannot fill in full there — a FOK that cannot fill in full is KILLED (nothing executes).",
					side, dec(peg))
			}
		default:
			// gtc rests the remainder at the peg; ioc cancels it.
			fillSimulation(&sim, side, sw, best, true, qty, tif == "gtc")
			trimmed := ppBinds && !sw.fullyFilled && !sw.reachedEnd
			if trimmed {
				addProtectionCapped(add, &sim, typ, side, ppPercentStr(req), mid, limitPrice)
			}
			analyzeSweep(add, side, typ, sw, best, true, trimmed)
		}

	case "limit":
		price, okp := decOf(req.Price)
		qty, okq := decOf(req.Qty)
		if !okp || !okq {
			break
		}
		// Fat-finger: limit price far from mid in the unfavorable direction. It is
		// entirely a statement about the mid, so with no mid there is nothing to
		// compare against and the check is skipped (reported by
		// MID_PRICE_UNAVAILABLE above). A one-sided pseudo-mid — half the lone ask,
		// say — is worse than none: it would put every sane price 100% "away from
		// the market" and warn on all of them.
		if hasMid {
			if side == "buy" && price.GreaterThan(scale(mid, fatFingerWarnPct, true)) {
				add(WarnPriceFarAboveMarket, "limit BUY price %s is %s above the mid %s — double-check for an extra digit (you would overpay relative to the market)",
					dec(price), pctStr(price.Sub(mid), mid), dec(mid))
			}
			if side == "sell" && price.LessThan(scale(mid, fatFingerWarnPct, false)) {
				add(WarnPriceFarBelowMarket, "limit SELL price %s is %s below the mid %s — double-check for a missing digit (you would sell far under the market)",
					dec(price), pctStr(mid.Sub(price), mid), dec(mid))
			}
		}
		// Price protection (--pp) can hold a crossing limit's taker fill tighter
		// than the submitted price: the effective taker bound is the tighter of the
		// two. It applies only to a taker match, so po (maker-only) keeps the bare
		// limit price; ppBinds is true only when protection — not the limit — is the
		// binding constraint.
		effPrice, ppBinds := price, false
		if tif != "po" {
			if ppCap, ppOn := ppBound(); ppOn {
				effPrice = tighter(side, price, ppCap)
				ppBinds = effPrice.Equal(ppCap) && !ppCap.Equal(price)
			}
		}
		// Crossing analysis over the marketable portion (asks<=effPrice / bids>=effPrice).
		sw := sweep(levels, side, effPrice, true, qty, false)
		marketable := sw.filledBase.IsPositive()
		fullyFillable := !sw.filledBase.LessThan(qty) // crossing book covers the full qty within the band
		// crosses is whether the SUBMITTED price reaches the book at all — computed
		// against the bare price, not effPrice, so it stays true even when price
		// protection excludes every in-band level. It is what separates a pp-blocked
		// crossing order (all qty canceled by pp) from a genuinely non-crossing limit
		// (pp irrelevant — it rests or expires on its own). With no best on the fill
		// side there is nothing to cross, so it is false: compared against a zero,
		// every buy price would "cross" a book that holds no asks at all.
		crosses := hasBest && ((side == "buy" && price.GreaterThanOrEqual(best)) || (side == "sell" && price.LessThanOrEqual(best)))
		switch {
		case tif == "po" && marketable:
			// A crossing post-only is rejected outright; it neither fills nor rests.
			sim.Marketable = true
			sim.Disposition = "REJECTED — a post-only order that would cross the book is rejected by the server; nothing executes"
			sim.Outcome = OutcomeNothing
			add(WarnPostOnlyWouldReject, "this is a post-only (--tif po) limit %s at %s, but it crosses the book (best %s %s) — a post-only order that would take liquidity will be rejected. Use a non-crossing price, or drop --tif po.",
				side, dec(price), oppSideName(side), dec(best))
		case tif == "fok" && !fullyFillable:
			// Fill-or-kill that can't fill in full — within the pp band when it binds,
			// else within the book at the limit price — executes nothing.
			sim.Marketable = marketable
			sim.Disposition = "KILLED — a fill-or-kill order that cannot fill in full executes nothing"
			sim.Outcome = OutcomeNothing
			if ppBinds {
				add(WarnFOKWouldKill, "this is a fill-or-kill (--tif fok) limit %s of %s, but price protection (--pp) limits the immediate fill to %s within %s%% of the mid %s — a FOK that cannot fill in full is KILLED (nothing executes).",
					side, dec(qty), dec(sw.filledBase), ppPercentStr(req), dec(mid))
			} else {
				add(WarnFOKWouldKill, "this is a fill-or-kill (--tif fok) limit %s of %s, but only %s can fill immediately at %s or better — a FOK that cannot fill in full is KILLED (nothing executes).",
					side, dec(qty), dec(sw.filledBase), dec(price))
			}
		case ppBinds && crosses && !marketable:
			// Price protection excludes even the best opposing level, so a limit that
			// would otherwise cross takes nothing: the whole quantity is canceled by
			// pp — it neither fills nor rests, and it is NOT the non-crossing case
			// below. gtc and ioc are handled alike here (fok is caught above by its
			// !fullyFillable kill). addProtectionCapped states both the disposition and
			// the nothing-executes outcome, since nothing was marketable.
			sim.Marketable = false
			sim.EstRemainingQty = dec(qty) // base-sized limit: the entire quantity is unfilled (canceled by pp)
			addProtectionCapped(add, &sim, typ, side, ppPercentStr(req), mid, effPrice)
		case tif == "ioc" && !marketable:
			// A non-crossing IOC takes no liquidity and rests for zero time: the
			// server accepts it and it immediately reaches the `expired` status with
			// no fill. (A marketable IOC that fills some and cancels the rest is
			// ordinary behavior handled by the default branch, with no warning.)
			sim.Marketable = false
			sim.EstRemainingQty = dec(qty)
			sim.Disposition = "EXPIRED — an immediate-or-cancel order that does not cross the book takes no liquidity and expires immediately; nothing executes"
			sim.Outcome = OutcomeNothing
			if !hasBest {
				// No opposing side to name a best price from — and no price would cross
				// an empty one, so the advice is to rest rather than to reprice.
				add(WarnIOCWouldExpire, "this is an immediate-or-cancel (--tif ioc) limit %s at %s, but the %s side of the book is empty — no price crosses it, so the order takes no liquidity and expires immediately with no fill. Drop --tif ioc to rest the order instead.",
					side, dec(price), oppSideName(side))
			} else {
				add(WarnIOCWouldExpire, "this is an immediate-or-cancel (--tif ioc) limit %s at %s, but it does not cross the book (best %s %s) — it takes no liquidity and expires immediately with no fill. Use a crossing price, or drop --tif ioc to rest the order.",
					side, dec(price), oppSideName(side), dec(best))
			}
		default:
			// Normal limit: the crossing portion fills now; the remainder rests
			// (gtc/po/fok-full) or is canceled (ioc).
			fillSimulation(&sim, side, sw, best, true, qty, tif != "ioc")
			if marketable {
				// A crossing limit taking liquidity is ordinary behavior, fully
				// reported by the simulation (marketable + the est-fill/remainder
				// fields); it needs no warning of its own. But when price protection
				// (not the book) trimmed the taker fill, attribute the canceled
				// remainder to pp and skip the liquidity warning; otherwise the sweep
				// analysis still runs to catch a bad crossing price (slippage, depth).
				trimmed := ppBinds && !sw.fullyFilled && !sw.reachedEnd
				if trimmed {
					addProtectionCapped(add, &sim, typ, side, ppPercentStr(req), mid, effPrice)
				}
				analyzeSweep(add, side, typ, sw, best, true, trimmed)
			}
		}
	}

	// Notional, in the pair's quote currency. Each bound WARNING is raised only
	// against the bound this pair publishes; a bound it does not publish is
	// skipped, leaving the server as the authority. The notional itself is
	// reported for every pair (the fee estimate downstream is gated on it) —
	// except when the order can only be valued off a book price the book does not
	// have, where it stays empty and BOTH bound checks are skipped: an unpriceable
	// order valued at zero would be reported as below every minimum.
	//
	// The reference price a base-sized order with no price of its own is valued at:
	// a best order's peg, because that is where it rests or crosses to, and the best
	// bid for a market sell, because that is what it sells into. Only a SELL reads
	// it (see orderNotional).
	ref, hasRef := bestBid, hasBid
	if typ == "best" {
		ref, hasRef = peg, hasPeg
	}
	if notional, ok := orderNotional(req, ref, hasRef); ok {
		sim.Notional = dec(notional)
		// Worded and thresholded once in NotionalBoundWarnings, so the same order
		// raises the same warning whatever the book's shape — the bounds need no
		// depth. The unit is the pair entry's own quote
		// currency wherever the entry carries one, falling back to the symbol's
		// second segment — the same currency by API contract.
		ws = append(ws, NotionalBoundWarnings(notional.String(), bounds, quote)...)
	}

	// Tick-size alignment (limit only): a price off the policy grid is rejected.
	// One definition of "on the grid" — the same OnTick the TUI checks against.
	if typ == "limit" {
		if price, ok := decOf(req.Price); ok {
			if onGrid, ok := OnTick(bands, price.String()); ok && !onGrid {
				tick, _ := TickSizeAt(bands, price.String())
				add(WarnPriceOffTick, "limit price %s is not a multiple of the tick size %s for %s — it will be rejected; round the price to the tick grid.",
					dec(price), tick, req.Symbol)
			}
		}
	}

	// No size, no verdict. Every sizing branch above bails out when its size field
	// is absent or unparseable, so what can reach here without one is a statement
	// made from the BOOK's shape alone (before the type switch, or a best order's
	// missing peg) — and a request with no determinable size is not yet an order
	// whose outcome can be stated. That is what Outcome promises, and Notional is
	// empty for the same reason. Clearing the pair together keeps the prose and the
	// token from disagreeing: a sizeless draft must not claim it rests on the book
	// either. The warnings stay — they are advice about the book, the type and the
	// tif, true while the size is still being typed.
	if !hasSize {
		sim.Disposition, sim.Outcome = "", ""
	}

	return sim, ws
}

// analysisValues returns values with accountSeq defaulted: the analysis never
// uses the account, but placeRequest requires the frontier to have resolved it
// and AnalyzePlace callers (a UI previewing a draft) may not carry one.
func analysisValues(values map[string]string) map[string]string {
	if v, ok := values["accountSeq"]; ok && v != "" {
		return values
	}
	next := make(map[string]string, len(values)+1)
	for k, v := range values {
		next[k] = v
	}
	next["accountSeq"] = "1"
	return next
}

// fillSimulation records the estimated fill outcome of a sweep into sim. best is
// the best price on the consumed side. hasOrderQty/orderQty give the order's
// base size when it is base-sized (a sell or a limit), so the remaining quantity
// can be reported; a market BUY is quote-sized, so a base remainder is undefined
// and only the disposition is set. restsRemainder says whether an unfilled
// remainder rests on the book (a gtc/po limit) or is canceled (a market or IOC
// order).
//
// A disposition the caller has ALREADY stated is left alone: the wording here is
// derived from the sweep, so it describes a book that ran out, and the caller's
// is the more specific statement (an empty book side never ran out — it was
// never populated). The Outcome token follows the same rule, for the same reason.
func fillSimulation(sim *PlaceSimulation, side string, sw sweepResult, best decimal.Decimal, hasOrderQty bool, orderQty decimal.Decimal, restsRemainder bool) {
	sim.Marketable = sw.filledBase.IsPositive()
	sim.FullyFilled = sw.fullyFilled
	// The structural outcome, read off the same sweep the wording below is derived
	// from so the token and the prose cannot disagree: anything that fills executes,
	// an untaken order that may sit on the book rests, and one that can do neither
	// is canceled/expired unfilled.
	if sim.Outcome == "" {
		switch {
		case sw.filledBase.IsPositive():
			sim.Outcome = OutcomeFills
		case restsRemainder:
			sim.Outcome = OutcomeRests
		default:
			sim.Outcome = OutcomeNothing
		}
	}
	if sw.filledBase.IsPositive() {
		sim.EstFilledQty = dec(sw.filledBase)
		sim.EstFilledQuote = dec(sw.filledQuote)
	}
	if sw.hasAvg {
		sim.EstAvgFillPrice = dec(sw.avgPrice)
		sim.EstWorstFillPrice = dec(sw.worstPrice)
		var slip decimal.Decimal
		if side == "buy" {
			slip = sw.avgPrice.Sub(best)
		} else {
			slip = best.Sub(sw.avgPrice)
		}
		if slip.IsPositive() {
			sim.EstSlippagePct = pctStr(slip, best)
		}
	}

	if hasOrderQty {
		if rem := orderQty.Sub(sw.filledBase); rem.IsPositive() {
			sim.EstRemainingQty = dec(rem)
			switch {
			case sim.Disposition != "": // already stated, and more specific
			case restsRemainder:
				sim.Disposition = "rests on the book as a maker limit order"
			default:
				sim.Disposition = "canceled — a market/IOC order takes only the available liquidity"
			}
		}
	} else if !sw.fullyFilled && sim.Disposition == "" {
		// Market BUY (quote-sized): the book is exhausted before the full amount is
		// spent, so the unspent quote is canceled. (A price-protection trim overrides
		// this disposition at the call site; a best BUY is base-sized, not here.)
		sim.Disposition = "the order's full amount cannot be spent (book exhausted) — a market/IOC order cancels the unspent remainder"
	}
}

// analyzeSweep emits the liquidity/slippage warnings shared by market/best
// orders and the crossing portion of a marketable limit. limitOrder=true marks
// the limit case: its crossing portion taking only what fits at the limit price,
// with the remainder resting (gtc/po) or canceling (ioc), is the ordinary
// behavior of a marketable — especially IOC — limit and is already reported by
// the simulation, so it is NOT flagged as a completeness warning. A market/best
// order's unfilled remainder, by contrast, is canceled for lack of liquidity and
// is warned via INSUFFICIENT_LIQUIDITY.
func analyzeSweep(add func(PlaceWarningCode, string, ...any), side, typ string, sw sweepResult, best decimal.Decimal, limitOrder, protectionTrimmed bool) {
	if !sw.filledBase.IsPositive() {
		return
	}
	// A protection-trimmed fill is incomplete by design — the pp band, not the
	// book, stopped it — and is already reported by PRICE_PROTECTION_CAPPED, so
	// don't also flag it as a liquidity/depth shortfall.
	if !sw.fullyFilled && !protectionTrimmed {
		if !limitOrder {
			add(WarnInsufficientLiquidity, "the visible book has only enough liquidity to fill ~%s of this %s %s order — a market/IOC order cancels the unfilled remainder.", dec(sw.filledBase), typ, side)
		}
		// The sweep ran off the end of the returned book. The public book caps how
		// many levels it returns per side, so deeper liquidity may exist but be
		// invisible here — the fill and slippage are a lower bound on the real cost.
		// Fires only when book depth (not a limit price) stopped the sweep: it points
		// the caller at a coarser grouping to see more depth, a distinct signal from
		// INSUFFICIENT_LIQUIDITY (not enough depth to fill) that it accompanies for a
		// market/best order and stands alone for a marketable limit.
		if sw.reachedEnd {
			add(WarnBookDepthLimited, "this order sweeps all %s visible %s levels without fully filling — the book returns only so many levels per side, so the fill/slippage are a lower bound; a coarser grouping (a larger `level`) shows more depth.", strconv.Itoa(sw.depth), oppSideName(side))
		}
	}
	if !sw.hasAvg {
		return
	}
	// Slippage of the average fill vs the best price on the consumed side.
	var slip decimal.Decimal
	if side == "buy" {
		slip = sw.avgPrice.Sub(best) // paid above best ask
	} else {
		slip = best.Sub(sw.avgPrice) // received below best bid
	}
	if slip.IsPositive() && pctOf(slip, best).GreaterThanOrEqual(slippageWarnPct) {
		add(WarnHighSlippage, "this %s %s order sweeps the book to an average fill price of %s — %s worse than the best %s %s (worst level touched: %s). Consider a limit order or a smaller size.",
			typ, side, dec(sw.avgPrice), pctStr(slip, best), oppSideName(side), dec(best), dec(sw.worstPrice))
	}
}

// ---- orderbook sweep simulation ----

type bookLevel struct {
	price decimal.Decimal
	qty   decimal.Decimal
}

type sweepResult struct {
	filledBase  decimal.Decimal // base quantity filled
	filledQuote decimal.Decimal // quote spent (buy) / received (sell)
	avgPrice    decimal.Decimal // filledQuote/filledBase, valid when hasAvg
	worstPrice  decimal.Decimal // last (worst) price level touched
	hasAvg      bool
	fullyFilled bool // the whole target was consumed
	// reachedEnd is true when the sweep consumed the deepest level the book
	// returned (it ran off the end rather than stopping on the target or a limit
	// price). Combined with !fullyFilled it flags a book-depth-limited estimate:
	// the public book returns a capped number of levels per side, so any deeper
	// liquidity is invisible and the fill/slippage estimate is only a lower bound.
	reachedEnd bool
	depth      int // number of levels on the consumed side (for the message)
}

// sweep walks levels (best-first) consuming the order's target until it is met
// or the (marketable) book is exhausted. hasLimit caps the marketable range to
// prices at/through limit (a limit order); when false there is no cap (a market
// order). quoteTarget=true sizes by quote spend (a market buy's --amt); otherwise
// by base qty.
func sweep(levels []bookLevel, side string, limit decimal.Decimal, hasLimit bool, target decimal.Decimal, quoteTarget bool) sweepResult {
	res := sweepResult{depth: len(levels)}
	remaining := target
	for i, lvl := range levels {
		if hasLimit {
			if side == "buy" && lvl.price.GreaterThan(limit) {
				break
			}
			if side == "sell" && lvl.price.LessThan(limit) {
				break
			}
		}
		levelQuote := lvl.price.Mul(lvl.qty)
		if quoteTarget {
			if remaining.LessThanOrEqual(levelQuote) {
				res.filledBase = res.filledBase.Add(remaining.Div(lvl.price))
				res.filledQuote = res.filledQuote.Add(remaining)
				remaining = decimal.Decimal{}
			} else {
				res.filledBase = res.filledBase.Add(lvl.qty)
				res.filledQuote = res.filledQuote.Add(levelQuote)
				remaining = remaining.Sub(levelQuote)
			}
		} else {
			if remaining.LessThanOrEqual(lvl.qty) {
				res.filledBase = res.filledBase.Add(remaining)
				res.filledQuote = res.filledQuote.Add(lvl.price.Mul(remaining))
				remaining = decimal.Decimal{}
			} else {
				res.filledBase = res.filledBase.Add(lvl.qty)
				res.filledQuote = res.filledQuote.Add(levelQuote)
				remaining = remaining.Sub(lvl.qty)
			}
		}
		res.worstPrice = lvl.price
		// Consuming the last returned level means the visible book was exhausted
		// (rather than the target or a limit price stopping the walk earlier).
		if i == len(levels)-1 {
			res.reachedEnd = true
		}
		if !remaining.IsPositive() {
			break
		}
	}
	res.fullyFilled = !remaining.IsPositive()
	if res.filledBase.IsPositive() {
		res.avgPrice = res.filledQuote.Div(res.filledBase)
		res.hasAvg = true
	}
	return res
}

// sortedLevels parses and orders orderbook levels best-first (asks ascending,
// bids descending), defensively so the sweep is correct regardless of the
// source's emission order. Unparseable levels are skipped.
func sortedLevels(in []BookLevel, ascending bool) []bookLevel {
	out := make([]bookLevel, 0, len(in))
	for _, l := range in {
		p, errp := decimal.NewFromString(l.Price)
		q, errq := decimal.NewFromString(l.Qty)
		// Skip unparseable or non-positive levels: a zero price would divide by
		// zero in the sweep, and a non-positive qty carries no liquidity.
		if errp != nil || errq != nil || !p.IsPositive() || !q.IsPositive() {
			continue
		}
		out = append(out, bookLevel{price: p, qty: q})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ascending {
			return out[i].price.LessThan(out[j].price)
		}
		return out[i].price.GreaterThan(out[j].price)
	})
	return out
}

// orderNotional estimates the order's value in the pair's QUOTE currency —
// whatever that currency is; nothing here depends on which. ok=false when the
// size is not determinable from the request, or when the only reference price
// that could value it is missing.
//
// The sizing matrix makes this asymmetric, so only one shape is book-dependent: a
// market/best BUY is sized in quote by amt and a limit order carries its own
// price, both determinable from the request alone; a market/best SELL carries a
// base qty and no price of its own, so it is valued at ref — the price the CALLER
// resolves for the order's type, since a market order sells into the opposing best
// while a best order rests at (or crosses to) its own peg. Without ref
// (hasRef=false) such an order cannot be valued: valuing it at zero would report
// it as below every minimum, a rejection claim the analysis cannot support.
func orderNotional(req rawapi.OrderPlaceRequest, ref decimal.Decimal, hasRef bool) (decimal.Decimal, bool) {
	typ := string(req.OrderType)
	if req.Side == "buy" && (typ == "market" || typ == "best") {
		return decOf(req.Amt) // amt is the quote currency to spend
	}
	qty, ok := decOf(req.Qty)
	if !ok {
		return decimal.Decimal{}, false
	}
	if typ == "limit" {
		price, ok := decOf(req.Price)
		if !ok {
			return decimal.Decimal{}, false
		}
		return price.Mul(qty), true
	}
	// market/best sell: value the base qty at the reference price.
	if !hasRef {
		return decimal.Decimal{}, false
	}
	return qty.Mul(ref), true
}

// protectionBound returns the price a price-protected (--pp) taker order is held
// to, and ok=true when protection is enabled. Per the public spec a pp order
// fills only within ppPercent% of the mid (default 5) when it takes liquidity,
// and any quantity past that band is canceled: a buy fills up to mid*(1+pct/100),
// a sell down to mid*(1-pct/100).
func protectionBound(req rawapi.OrderPlaceRequest, side string, mid decimal.Decimal) (decimal.Decimal, bool) {
	if req.PP == nil || !*req.PP {
		return decimal.Decimal{}, false
	}
	pct := decimal.NewFromInt(5)
	if req.PPPercent != nil {
		pct = decimal.NewFromInt(int64(*req.PPPercent))
	}
	return scale(mid, pct, side == "buy"), true
}

// ppPercentStr renders the effective price-protection threshold — the ppPercent
// value, or the default 5 — for a message.
func ppPercentStr(req rawapi.OrderPlaceRequest) string {
	if req.PPPercent != nil {
		return strconv.Itoa(*req.PPPercent)
	}
	return "5"
}

// addProtectionCapped records a price-protection trim: the taker fill was held
// within the protection band and the unfilled remainder is canceled rather than
// filled at a worse price. It sets the disposition and emits the warning. The
// band is directional — a buy fills UP TO the ceiling mid*(1+pct/100), a sell
// DOWN TO the floor mid*(1-pct/100) — so the two sides get distinct wording.
//
// It owns the Outcome token alongside the disposition it overwrites, so the two
// cannot disagree: a partly filled order still executes, while one whose every
// takeable level the band excluded neither fills nor rests — its whole quantity is
// canceled. Callers set the fill fields before calling.
func addProtectionCapped(add func(PlaceWarningCode, string, ...any), sim *PlaceSimulation, typ, side, pct string, mid, bound decimal.Decimal) {
	if !sim.Marketable {
		sim.Outcome = OutcomeNothing
	}
	if side == "sell" {
		sim.Disposition = fmt.Sprintf("unfilled remainder canceled by price protection — fills kept within %s%% of the mid %s (down to %s)", pct, dec(mid), dec(bound))
		add(WarnPriceProtectionCapped, "price protection (--pp) holds this %s %s to fills within %s%% of the mid %s (down to %s); the unfilled remainder is canceled rather than filled at a worse price.",
			typ, side, pct, dec(mid), dec(bound))
		return
	}
	sim.Disposition = fmt.Sprintf("unfilled remainder canceled by price protection — fills kept within %s%% of the mid %s (up to %s)", pct, dec(mid), dec(bound))
	add(WarnPriceProtectionCapped, "price protection (--pp) holds this %s %s to fills within %s%% of the mid %s (up to %s); the unfilled remainder is canceled rather than filled at a worse price.",
		typ, side, pct, dec(mid), dec(bound))
}

// tighter returns whichever of two taker price bounds constrains the sweep more
// on the given side: the lower cap for a buy, the higher floor for a sell.
func tighter(side string, a, b decimal.Decimal) decimal.Decimal {
	if side == "buy" {
		if a.LessThan(b) {
			return a
		}
		return b
	}
	if a.GreaterThan(b) {
		return a
	}
	return b
}

// bestPeg derives a best (BBO) order's price from the visible book: the opponent
// side's nth level for a taker tif (a buy at the nth ask, a sell at the nth bid),
// or the own (queue) side's nth level for post-only (a buy at the nth bid, a sell
// at the nth ask). nth is 1-based (--best-nth). ok is false when that level is not
// present. bids/asks are best-first.
func bestPeg(tif, side string, bids, asks []bookLevel, nth int) (decimal.Decimal, bool) {
	if nth < 1 {
		return decimal.Decimal{}, false
	}
	taker := tif != "po"
	var levels []bookLevel
	switch {
	case side == "buy" && taker, side == "sell" && !taker:
		levels = asks
	default:
		levels = bids
	}
	if i := nth - 1; i < len(levels) {
		return levels[i].price, true
	}
	return decimal.Decimal{}, false
}

// bestQty returns the base quantity a taker best order matches at its peg: a SELL
// uses --qty directly, and a BUY sized by --amt is converted to base at the peg
// (qty = amt/peg, floored to a base precision) and then matched as that fixed
// quantity — matching the engine, which resolves a best order to a limit at the
// peg and derives qty from amt up front rather than sweeping the amt as a budget.
// ok is false when the sizing field is absent/unparseable or the peg is non-positive.
func bestQty(req rawapi.OrderPlaceRequest, side string, peg decimal.Decimal) (decimal.Decimal, bool) {
	if side == "sell" {
		return decOf(req.Qty)
	}
	amt, ok := decOf(req.Amt)
	if !ok || !peg.IsPositive() {
		return decimal.Decimal{}, false
	}
	return amt.Div(peg).Truncate(8), true
}

// fetchTickBands fetches the symbol's tick-size policy bands; nil if the
// policy can't be fetched — the alignment check is best-effort and secondary.
func fetchTickBands(ctx context.Context, raw *rawapi.Client, symbol rawapi.Symbol) []TickBand {
	pols, _, _, err := raw.TickSize(ctx, rawapi.TickSizeRequest{Symbol: symbol}, apiclient.Policy{Idempotent: true})
	if err != nil || len(pols) == 0 {
		return nil
	}
	bands := make([]TickBand, 0, len(pols[0].TickSizePolicy))
	for _, b := range pols[0].TickSizePolicy {
		bands = append(bands, TickBand{PriceGte: b.PriceGte, TickSize: b.TickSize})
	}
	return bands
}

// fetchOrderValueBounds reads the pair's currencies and order value bounds from
// the public pair listing. Best-effort like the tick bands: a failed read is
// resolved as if the listing were empty, which on a KRW-quoted symbol leaves the
// documented KRW figures in place (ResolveBoundsForSymbol) and elsewhere skips
// the bound checks — never a substituted figure from another pair.
func fetchOrderValueBounds(ctx context.Context, raw *rawapi.Client, symbol rawapi.Symbol) OrderValueBounds {
	pairs, _, _, err := raw.Pairs(ctx, rawapi.PairsRequest{}, apiclient.Policy{Idempotent: true})
	if err != nil {
		pairs = nil
	}
	return ResolveBoundsForSymbol(pairs, string(symbol))
}

// ---- decimal helpers (advisory display/threshold math only — order values stay
// their original wire strings end to end) ----

// decOf parses an optional decimal string; ok=false when absent or unparseable.
func decOf(p *string) (decimal.Decimal, bool) {
	if p == nil {
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(*p)
	if err != nil {
		return decimal.Decimal{}, false
	}
	return d, true
}

// scale returns a*(1±pct/100): up=true adds the percent (the unfavorable bound
// for a buy), up=false subtracts it (the unfavorable bound for a sell).
func scale(a, pct decimal.Decimal, up bool) decimal.Decimal {
	frac := pct.Div(oneHundred)
	if up {
		return a.Mul(decimal.NewFromInt(1).Add(frac))
	}
	return a.Mul(decimal.NewFromInt(1).Sub(frac))
}

// pctOf returns (part/whole)*100 as a decimal for threshold comparison.
func pctOf(part, whole decimal.Decimal) decimal.Decimal {
	if whole.IsZero() {
		return decimal.Decimal{}
	}
	return part.Div(whole).Mul(oneHundred)
}

// pctStr renders (part/whole) as a percentage string for a message.
func pctStr(part, whole decimal.Decimal) string {
	return pctOf(part, whole).Round(2).String() + "%"
}

// dec renders a decimal for display, trimmed to 8 fractional places (never used
// for an order value, which stays its original wire string).
func dec(d decimal.Decimal) string {
	return d.Round(8).String()
}

// oppSideName names the book side a side's marketable order consumes.
func oppSideName(side string) string {
	if side == "buy" {
		return "ask"
	}
	return "bid"
}
