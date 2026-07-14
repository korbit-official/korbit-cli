// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package state

import "github.com/shopspring/decimal"

// orderProgress is an order's position in its lifecycle: the intrinsic key used
// to order two updates to the SAME order WITHOUT relying on a transport
// timestamp. A WebSocket frame's timestamp is the server's send-time, and a
// REST backfill row carries only a conservative fetch-start estimate — neither
// is the order's own event time, so neither can correctly order two views of
// one order.
//
// It rests on one invariant of the Korbit order lifecycle: while an order rests
// on the book it only ever moves FORWARD — its status advances
// (pending → open → partiallyFilled) or its cumulative filledQty grows — and
// once it leaves the book it is terminal and frozen. So the tuple
// (terminal, lifecycleRank, filledQty) is monotonically non-decreasing over the
// life of an order, which makes "the more-advanced state wins" a correct,
// clock-free merge rule.
type orderProgress struct {
	status    string // normalized (WS "unfilled" already mapped to "open")
	filledQty string // cumulative filled quantity, decimal string ("" == 0)
}

// lifecycleRank ranks the open, in-book statuses (exactly openStatuses — the
// fixed set is the single source of truth; see state.go) by their forward
// order. Terminal statuses are not ranked here — they are handled by the
// absorbing rule in supersedes. ok is false only for the empty "status not yet
// known" string (the sole non-terminal value outside the open set); any other
// unranked status is terminal and never reaches rule 4/5. See supersedes rule 5.
func lifecycleRank(status string) (rank int, ok bool) {
	switch status {
	case "pending":
		return 0, true
	case "open": // WS "unfilled" is normalized to "open" upstream of here
		return 1, true
	case "partiallyFilled":
		return 2, true
	}
	return 0, false
}

func (p orderProgress) terminal() bool { return statusIsTerminal(p.status) }

// supersedes reports whether the incoming order state should overwrite the
// stored one's LIFECYCLE fields (status, filledQty, filledAmt, avgPrice,
// lastFilledAt). Identity/immutable fields (side, price, qty, clientOrderId,
// createdAt) are merged by the caller regardless — they never change over an
// order's life, so filling a gap from any source is always safe.
//
// The decision is purely the order's intrinsic monotonic progress (see
// orderProgress), never a transport timestamp, in this precedence:
//
//  1. The synthetic StatusClosedUnknown placeholder (an order that vanished
//     from an authoritative open-orders snapshot, real status not yet known)
//     yields to ANY real status — terminal or not — so a later frame proving
//     the order's true state always replaces the guess.
//  2. A terminal status is absorbing: once an order is terminal nothing
//     overwrites it. A stale non-terminal update can never reopen it, and a
//     re-delivered terminal backfill row is a no-op.
//  3. An incoming terminal status supersedes any non-terminal stored state.
//  4. Between two non-terminal states the more-advanced wins: a higher
//     lifecycle rank, then — at equal rank (both partiallyFilled) — a larger
//     cumulative filledQty (fills only accumulate).
//  5. One side carries the empty "status not yet known" string (the only
//     non-terminal value outside the open set — a non-empty unknown status is
//     terminal and already handled by rules 2/3): never regress; accept the
//     update only when its cumulative filledQty grew. This can never lose a real
//     fill and never reorders a lifecycle it cannot rank.
func (incoming orderProgress) supersedes(stored orderProgress) bool {
	// 1. The placeholder yields to any real status.
	if stored.status == StatusClosedUnknown {
		return incoming.status != "" && incoming.status != StatusClosedUnknown
	}
	// 2. A terminal stored status is absorbing.
	if stored.terminal() {
		return false
	}
	// 3. An incoming terminal status beats any non-terminal stored state.
	if incoming.terminal() {
		return true
	}
	// 4. Both non-terminal and recognized: the more-advanced lifecycle wins.
	ir, iok := lifecycleRank(incoming.status)
	sr, sok := lifecycleRank(stored.status)
	if iok && sok {
		if ir != sr {
			return ir > sr
		}
		return cmpDecimal(incoming.filledQty, stored.filledQty) > 0
	}
	// 5. Unrecognized status: never regress — accept only a filledQty increase.
	return cmpDecimal(incoming.filledQty, stored.filledQty) > 0
}

// cmpDecimal compares two decimal-string quantities by exact value, returning
// -1, 0, or +1. It parses through shopspring/decimal for the comparison only;
// callers keep the original strings untouched, so the money/quantity-as-
// decimal-string rule holds end to end. An empty or unparseable string compares
// as zero.
func cmpDecimal(a, b string) int {
	return decimalOrZero(a).Cmp(decimalOrZero(b))
}

func decimalOrZero(s string) decimal.Decimal {
	if s == "" {
		return decimal.Decimal{}
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}
	}
	return d
}
