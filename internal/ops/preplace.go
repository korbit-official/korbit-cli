// Copyright (c) 2026 Korbit Inc.
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

	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
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
	// The server's notional bounds for KRW-quoted pairs (also stated in the
	// order place Notes). Checked only for *_krw symbols.
	minNotionalKRW = decimal.NewFromInt(5_000)
	maxNotionalKRW = decimal.NewFromInt(1_000_000_000)

	oneHundred = decimal.NewFromInt(100)
	two        = decimal.NewFromInt(2)
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

	BestBid     string `json:"bestBid"`
	BestAsk     string `json:"bestAsk"`
	Mid         string `json:"mid"`
	NotionalKRW string `json:"notionalKrw,omitempty"`

	// EstPegPrice is the price a best (BBO) order derives from the current book —
	// its --best-nth level on the side its tif selects. Empty for non-best orders
	// and when that level is not visible in the book.
	EstPegPrice string `json:"estPegPrice,omitempty"`

	// Marketable is true when the order (or a limit's crossing portion) would
	// take liquidity immediately. The Est* fields describe that simulated taker
	// fill; they are empty for a non-crossing limit (which rests in full).
	Marketable        bool   `json:"marketable"`
	EstFilledQty      string `json:"estFilledQty,omitempty"`
	EstFilledQuote    string `json:"estFilledQuoteKrw,omitempty"`
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
// --dry-run`. It fetches PUBLIC market data only (orderbook + tick-size policy)
// through the supplied creds-less client, so it signs nothing and works before
// any key is set up. The returned warnings are advisory. A non-nil error means
// the orderbook could not be fetched (offline, or an invalid/untradable symbol)
// and the analysis was skipped — the caller still emits its plan, just without
// the safety checks.
//
// The orderbook is sufficient for every order type: its top levels ARE the best
// bid/ask, and walking it simulates exactly what a marketable order (a market /
// best order, or the crossing portion of a limit order) would fill at.
func PrePlaceCheck(ctx context.Context, raw *rawapi.Client, values map[string]string) (PlaceSimulation, []PlaceWarning, error) {
	req := placeRequest(values)

	book, _, _, err := raw.Orderbook(ctx, rawapi.OrderbookRequest{Symbol: req.Symbol}, korbit.Policy{Idempotent: true})
	if err != nil {
		return PlaceSimulation{}, nil, err
	}
	// The tick-size policy backs a secondary, limit-only check; fetching it is
	// best-effort (nil bands just skip the alignment check).
	var bands []TickBand
	if req.OrderType == "limit" {
		bands = fetchTickBands(ctx, raw, req.Symbol)
	}
	return AnalyzePlace(values, toBookLevels(book.Bids), toBookLevels(book.Asks), bands)
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
// and warnings as PrePlaceCheck, over a caller-supplied orderbook snapshot and
// (optional) tick-size policy — no I/O. values are the order's validated wire
// params (the same map the place operation runs on); bids/asks are the book's
// levels in any order (they are sorted defensively); nil/empty bands skip the
// tick-alignment check. The error reports an unusable (empty) book — the
// analysis needs both sides for a mid.
func AnalyzePlace(values map[string]string, bidLevels, askLevels []BookLevel, bands []TickBand) (PlaceSimulation, []PlaceWarning, error) {
	req := placeRequest(analysisValues(values))

	asks := sortedLevels(askLevels, true)  // ascending: best ask first
	bids := sortedLevels(bidLevels, false) // descending: best bid first
	if len(asks) == 0 || len(bids) == 0 {
		return PlaceSimulation{}, nil, fmt.Errorf("the orderbook for %s is empty", req.Symbol)
	}
	bestAsk := asks[0].price
	bestBid := bids[0].price
	mid := bestBid.Add(bestAsk).Div(two)

	side := string(req.Side)
	typ := string(req.OrderType)
	tif := strings.ToLower(tifStr(req.TimeInForce))

	sim := PlaceSimulation{
		Disclaimer: SimulationDisclaimer,
		BestBid:    dec(bestBid), BestAsk: dec(bestAsk), Mid: dec(mid),
	}
	var ws []PlaceWarning
	add := func(code PlaceWarningCode, format string, a ...any) {
		// Keep the template and args alongside the rendered English Message so a
		// display layer can re-render (e.g. localize) without this layer depending
		// on any localization machinery. Format strings carry only %s verbs.
		ws = append(ws, PlaceWarning{Code: code, Message: fmt.Sprintf(format, a...), Format: format, Args: a})
	}

	// The side of the book a marketable fill consumes, and that side's best price.
	levels, best := asks, bestAsk
	if side == "sell" {
		levels, best = bids, bestBid
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
			ppCap, ppOn := protectionBound(req, side, mid)
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
		nth := 0
		if req.BestNth != nil {
			nth = *req.BestNth
		}
		peg, pegOK := bestPeg(tif, side, bids, asks, nth)
		if !pegOK {
			// Fewer than --best-nth levels are visible on the pegging side, so the
			// price can't be derived from the current book. Flag that the order would
			// likely take no liquidity (rather than leave warnings empty, which reads
			// as "safe to place"), and report only the reference prices and notional.
			sim.Disposition = "no fill — fewer than --best-nth price levels are visible on the side this order pegs to, so no peg price can be set from the current book"
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
			if (side == "buy" && peg.GreaterThanOrEqual(bestAsk)) || (side == "sell" && peg.LessThanOrEqual(bestBid)) {
				sim.Marketable = true
				sim.Disposition = "REJECTED — a post-only order priced to cross the book is rejected by the server; nothing executes"
				add(WarnPostOnlyWouldReject, "this post-only (--tif po) best %s pegs to %s, which crosses the book (best %s %s) — a post-only order that would take liquidity is rejected.",
					side, dec(peg), oppSideName(side), dec(best))
			} else {
				sim.Disposition = fmt.Sprintf("rests on the book as a maker limit order at %s", dec(peg))
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
		if ppCap, ppOn := protectionBound(req, side, mid); ppOn {
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
		// Fat-finger: limit price far from mid in the unfavorable direction.
		if side == "buy" && price.GreaterThan(scale(mid, fatFingerWarnPct, true)) {
			add(WarnPriceFarAboveMarket, "limit BUY price %s is %s above the mid %s — double-check for an extra digit (you would overpay relative to the market)",
				dec(price), pctStr(price.Sub(mid), mid), dec(mid))
		}
		if side == "sell" && price.LessThan(scale(mid, fatFingerWarnPct, false)) {
			add(WarnPriceFarBelowMarket, "limit SELL price %s is %s below the mid %s — double-check for a missing digit (you would sell far under the market)",
				dec(price), pctStr(mid.Sub(price), mid), dec(mid))
		}
		// Price protection (--pp) can hold a crossing limit's taker fill tighter
		// than the submitted price: the effective taker bound is the tighter of the
		// two. It applies only to a taker match, so po (maker-only) keeps the bare
		// limit price; ppBinds is true only when protection — not the limit — is the
		// binding constraint.
		effPrice, ppBinds := price, false
		if tif != "po" {
			if ppCap, ppOn := protectionBound(req, side, mid); ppOn {
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
		// (pp irrelevant — it rests or expires on its own).
		crosses := (side == "buy" && price.GreaterThanOrEqual(best)) || (side == "sell" && price.LessThanOrEqual(best))
		switch {
		case tif == "po" && marketable:
			// A crossing post-only is rejected outright; it neither fills nor rests.
			sim.Marketable = true
			sim.Disposition = "REJECTED — a post-only order that would cross the book is rejected by the server; nothing executes"
			add(WarnPostOnlyWouldReject, "this is a post-only (--tif po) limit %s at %s, but it crosses the book (best %s %s) — a post-only order that would take liquidity will be rejected. Use a non-crossing price, or drop --tif po.",
				side, dec(price), oppSideName(side), dec(best))
		case tif == "fok" && !fullyFillable:
			// Fill-or-kill that can't fill in full — within the pp band when it binds,
			// else within the book at the limit price — executes nothing.
			sim.Marketable = marketable
			sim.Disposition = "KILLED — a fill-or-kill order that cannot fill in full executes nothing"
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
			// !fullyFillable kill).
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
			add(WarnIOCWouldExpire, "this is an immediate-or-cancel (--tif ioc) limit %s at %s, but it does not cross the book (best %s %s) — it takes no liquidity and expires immediately with no fill. Use a crossing price, or drop --tif ioc to rest the order.",
				side, dec(price), oppSideName(side), dec(best))
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

	// Notional bounds (KRW-quoted pairs only): catch a too-small / too-large
	// order before the server rejects it.
	if notional, ok := orderNotionalKRW(req, bestBid); ok {
		sim.NotionalKRW = dec(notional)
		if notional.LessThan(minNotionalKRW) {
			add(WarnNotionalBelowMin, "order notional ~%s KRW is below the %s KRW minimum — it will be rejected.", dec(notional), minNotionalKRW)
		}
		if notional.GreaterThan(maxNotionalKRW) {
			add(WarnNotionalAboveMax, "order notional ~%s KRW exceeds the %s KRW maximum — it will be rejected.", dec(notional), maxNotionalKRW)
		}
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

	return sim, ws, nil
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
func fillSimulation(sim *PlaceSimulation, side string, sw sweepResult, best decimal.Decimal, hasOrderQty bool, orderQty decimal.Decimal, restsRemainder bool) {
	sim.Marketable = sw.filledBase.IsPositive()
	sim.FullyFilled = sw.fullyFilled
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
			if restsRemainder {
				sim.Disposition = "rests on the book as a maker limit order"
			} else {
				sim.Disposition = "canceled — a market/IOC order takes only the available liquidity"
			}
		}
	} else if !sw.fullyFilled {
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

// orderNotionalKRW estimates the order's KRW notional for a *_krw pair (the bound
// is KRW-specific); ok=false for a non-KRW pair or when the size is unknown. A
// market/best sell is valued at the best bid.
func orderNotionalKRW(req rawapi.OrderPlaceRequest, bestBid decimal.Decimal) (decimal.Decimal, bool) {
	if !strings.HasSuffix(strings.ToLower(string(req.Symbol)), "_krw") {
		return decimal.Decimal{}, false
	}
	typ := string(req.OrderType)
	if req.Side == "buy" && (typ == "market" || typ == "best") {
		return decOf(req.Amt) // amt is the KRW to spend
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
	// market/best sell: value the base qty at the best bid.
	return qty.Mul(bestBid), true
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
func addProtectionCapped(add func(PlaceWarningCode, string, ...any), sim *PlaceSimulation, typ, side, pct string, mid, bound decimal.Decimal) {
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
	pols, _, _, err := raw.TickSize(ctx, rawapi.TickSizeRequest{Symbol: symbol}, korbit.Policy{Idempotent: true})
	if err != nil || len(pols) == 0 {
		return nil
	}
	bands := make([]TickBand, 0, len(pols[0].TickSizePolicy))
	for _, b := range pols[0].TickSizePolicy {
		bands = append(bands, TickBand{PriceGte: b.PriceGte, TickSize: b.TickSize})
	}
	return bands
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
