// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"github.com/shopspring/decimal"
)

// Local holds — optimistic in-flight balance reservations.
//
// Between sending a place-order request and observing that order on the
// myOrder channel, the streamed balances do not yet reflect the reservation
// the exchange makes for it. A consumer that sizes follow-up orders against
// Available during that window oversizes and is rejected with NO_BALANCE. A
// local hold bridges exactly that window: the consumer registers the order's
// estimated reservation BEFORE sending (AddLocalHold, keyed by the
// clientOrderId every placement carries), Balances/BalancesFor report
// Available net of the active holds, and the store releases the hold the
// moment the order is observed on the myOrder channel — whatever its status,
// because from then on the server's own reservation (or the rejected order's
// absence of one) is what the balance stream reports. The relative order of a
// myOrder event and the myAsset frame carrying its reservation is
// unspecified, so a brief double-count (both the local hold and the server's
// reservation applied) or gap can occur around the release; both are
// transient and self-correct as balance frames arrive.
//
// The correctness invariant: a hold must never outlive certainty that its
// order is in flight. Whoever learns the order is NOT (or may not be) on its
// way releases:
//
//   - the store, on myOrder observation of the clientOrderId (any status —
//     including a terminal one, e.g. an order rejected asynchronously after
//     the accept ack);
//   - the consumer, on a definitive rejection or an unresolved failure of the
//     place call (ReleaseLocalHold);
//   - the store, on a private-connection transition (connect or disconnect):
//     the reconnect snapshot re-baselines balances, and holds from the old
//     connection can no longer be matched reliably;
//   - the store, unconditionally, once a hold outlives localHoldTTLMs — the
//     backstop that guarantees no hold is permanent even if every other
//     release path is missed (e.g. the myOrder channel is not subscribed).
//
// Holds only ever reduce the LOCAL view of Available — never what is sent to
// the server — so the worst effect of a stale hold is a transiently
// conservative balance, and the worst effect of an early release is one
// NO_BALANCE rejection, which is exactly the no-holds status quo.
//
// The layer is opt-in by use: a consumer that never registers a hold gets
// the server's Available verbatim.

// localHoldTTLMs is the unconditional hold lifetime backstop: generous
// multiples of a place round-trip (including the place protocol's read-back
// retries), so a hold expiring here means its release signals were lost, not
// that the order is slow.
const localHoldTTLMs = 3_000

// holdKey identifies one hold: the placement's sub-account and its
// clientOrderId (every placement carries one — the CLI mints it when the
// caller doesn't).
type holdKey struct {
	accountSeq    int
	clientOrderID string
}

// localHold is one active reservation against {accountSeq, currency}.
type localHold struct {
	currency string
	amount   decimal.Decimal
	expireAt int64 // s.now() ms deadline (the TTL backstop)
}

// AddLocalHold registers a local reservation of amount (a positive decimal
// string) of currency against accountSeq's Available, keyed by the
// clientOrderId of the order about to be sent. Call it before sending the
// order; the hold is released as documented above. A no-op when any argument
// is unusable (an unregistrable hold is dropped, not an error — the layer is
// advisory). Re-registering a clientOrderId replaces its hold.
func (s *Store) AddLocalHold(accountSeq int, clientOrderID, currency, amount string) {
	if clientOrderID == "" || currency == "" {
		return
	}
	amt, err := decimal.NewFromString(amount)
	if err != nil || !amt.IsPositive() {
		return
	}
	s.holds[holdKey{accountSeq: effAccountSeq(&accountSeq), clientOrderID: clientOrderID}] = localHold{
		currency: currency,
		amount:   amt,
		expireAt: s.now() + localHoldTTLMs,
	}
	s.revBal++
}

// ReleaseLocalHold drops the hold registered for {accountSeq, clientOrderID},
// if any. The consumer calls it when the place call fails — a definitive
// rejection, or an unresolved outcome (the invariant: unsure whether the
// order is in flight ⇒ release).
func (s *Store) ReleaseLocalHold(accountSeq int, clientOrderID string) {
	s.releaseHold(holdKey{accountSeq: effAccountSeq(&accountSeq), clientOrderID: clientOrderID})
}

// releaseHold removes one hold and invalidates the balance section when it
// was present.
func (s *Store) releaseHold(k holdKey) {
	if _, ok := s.holds[k]; !ok {
		return
	}
	delete(s.holds, k)
	s.revBal++
}

// clearLocalHolds drops every hold — the private-connection-transition
// release (see the package comment above).
func (s *Store) clearLocalHolds() {
	if len(s.holds) == 0 {
		return
	}
	s.holds = map[holdKey]localHold{}
	s.revBal++
}

// sweepLocalHolds expires holds past their TTL backstop. Called on every
// Apply and on every balance read, so an expired hold never outlives the next
// time anyone could observe it.
func (s *Store) sweepLocalHolds() {
	if len(s.holds) == 0 {
		return
	}
	now := s.now()
	for k, h := range s.holds {
		if now >= h.expireAt {
			delete(s.holds, k)
			s.revBal++
		}
	}
}

// applyLocalHolds returns b with Available reduced by the active holds on its
// {account, currency}, clamped at zero. Balance/TradeInUse stay the server's
// values — only Available is a local estimate while holds are active. An
// unparseable Available is returned verbatim (never invent a number).
func (s *Store) applyLocalHolds(b Balance) Balance {
	if len(s.holds) == 0 {
		return b
	}
	var sum decimal.Decimal
	for k, h := range s.holds {
		if k.accountSeq == b.AccountSeq && h.currency == b.Currency {
			sum = sum.Add(h.amount)
		}
	}
	if !sum.IsPositive() {
		return b
	}
	avail, err := decimal.NewFromString(b.Available)
	if err != nil {
		return b
	}
	adj := avail.Sub(sum)
	if adj.IsNegative() {
		adj = decimal.Zero
	}
	b.Available = adj.String()
	return b
}

// HoldIntent describes an order about to be placed, in the wire decimal
// strings it will carry, for estimating the reservation the exchange will
// make for it. Base/Quote are the symbol's two currencies (e.g. btc/krw for
// btc_krw). The order type is deliberately absent: the reservation follows
// from which size fields the order carries, so every type — including ones
// the API adds later — estimates correctly from its shape alone.
type HoldIntent struct {
	Side  string // buy | sell
	Price string // limit price ("" when the type carries none)
	Qty   string // base quantity ("" for an amount-sized order)
	Amt   string // quote amount ("" for a quantity-sized order)
	Base  string
	Quote string
	// QuoteFeeRate is the fee rate to reserve on top of a quote-funded buy
	// ("" = none). The exchange reserves notional*(1+maxFeeRate) when buys pay
	// their fee in the quote currency (see the fees operation's guidance), so
	// the caller passes maxFeeRate exactly when buyFeeCurrency is the quote.
	QuoteFeeRate string
}

// EstimateHold returns the currency and amount to hold for the intent,
// derived from which size fields the order carries (the place sizing matrix:
// a sell is qty-sized whatever its type; a buy carries amt for the
// amount-sized types — market and best — or price+qty for limit). A sell
// reserves the base quantity; a buy reserves the quote notional — amt when
// present (an amt-sized order spends at most amt, so it bounds the notional
// even when the execution price is the server's to pick), else price×qty —
// plus the QuoteFeeRate headroom. ok=false means the reservation is not
// computable (unusable values, or a shape carrying no bounded notional) —
// the caller simply skips the hold; missing one order's hold degrades to the
// no-holds status quo for that order. The estimate may be slightly
// conservative (headroom on shapes the exchange reserves less for); it must
// not be optimistic.
func (h HoldIntent) EstimateHold() (currency, amount string, ok bool) {
	switch h.Side {
	case "sell":
		if h.Base == "" {
			return "", "", false
		}
		qty, err := decimal.NewFromString(h.Qty)
		if err != nil || !qty.IsPositive() {
			return "", "", false
		}
		// Pass the original string through — no math, no reformatting.
		return h.Base, h.Qty, true
	case "buy":
		if h.Quote == "" {
			return "", "", false
		}
		var notional decimal.Decimal
		switch {
		case h.Amt != "":
			amt, err := decimal.NewFromString(h.Amt)
			if err != nil || !amt.IsPositive() {
				return "", "", false
			}
			notional = amt
		case h.Price != "" && h.Qty != "":
			price, err1 := decimal.NewFromString(h.Price)
			qty, err2 := decimal.NewFromString(h.Qty)
			if err1 != nil || err2 != nil || !price.IsPositive() || !qty.IsPositive() {
				return "", "", false
			}
			notional = price.Mul(qty)
		default:
			return "", "", false
		}
		if rate, err := decimal.NewFromString(h.QuoteFeeRate); err == nil && rate.IsPositive() {
			notional = notional.Mul(decimal.NewFromInt(1).Add(rate))
		}
		return h.Quote, notional.String(), true
	}
	return "", "", false
}
