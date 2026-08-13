// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"math/big"

	"github.com/korbit-official/korbit-cli/internal/output"
)

// The cross-field validation rules: each spans several params of one operation
// and is attached to its OpMeta.CrossValidate. The generic per-value engine
// (NormalizeValue/CoerceValue, which validates a single value against its kind)
// lives in internal/cmdmeta; these rules run after it. Errors say what to do
// instead. Money/quantity values stay strings end to end — never parsed to floats.

// after reports whether unix-ms string b is strictly greater than a.
func after(b, a string) bool {
	bi, _ := new(big.Int).SetString(b, 10)
	ai, _ := new(big.Int).SetString(a, 10)
	if bi == nil || ai == nil {
		return true
	}
	return bi.Cmp(ai) > 0
}

// crossValidateTimeWindow enforces --end after --start when both are given. It
// is the CrossValidate closure for candles, order history, and fills.
func crossValidateTimeWindow(values map[string]string) error {
	start, hasStart := values["startTime"]
	end, hasEnd := values["endTime"]
	if hasStart && hasEnd && !after(end, start) {
		return output.Usagef("--end must be after --start")
	}
	return nil
}

// crossValidateIDXor enforces exactly one of order-id / clientOrderId. It is the
// CrossValidate closure for order get and order cancel.
func crossValidateIDXor(values map[string]string) error {
	_, hasOrderID := values["orderId"]
	_, hasClientID := values["clientOrderId"]
	if hasOrderID == hasClientID {
		return output.Usagef("pass exactly one of --order-id or --client-order-id")
	}
	return nil
}

// crossValidatePlace enforces the order-sizing matrix — the most common
// integration mistake — with a message for every branch that says what to do
// instead. It is the CrossValidate closure for order place.
func crossValidatePlace(p map[string]string) error {
	typ := p["orderType"]
	side := p["side"]
	kind := typ + " " + side
	has := func(k string) bool { _, ok := p[k]; return ok }

	switch typ {
	case "limit":
		if !has("price") || !has("qty") {
			return output.Usagef("a limit order requires both --price and --qty")
		}
		if has("amt") {
			return output.Usagef("--amt is only for market/best BUY orders — a limit order is sized by --price and --qty")
		}
		if has("bestNth") {
			return output.Usagef("--best-nth applies only to --type best orders")
		}
	case "market", "best":
		if has("price") {
			return output.Usagef("--price is only for limit orders — a %s order executes at the market", typ)
		}
		if side == "buy" {
			if has("qty") {
				return output.Usagef("--qty is not allowed for a %s order — size it with --amt (the quote-currency amount to spend) only", kind)
			}
			if !has("amt") {
				return output.Usagef("a %s order is sized by --amt (the quote-currency amount to spend), not --qty", kind)
			}
		} else {
			if has("amt") {
				return output.Usagef("--amt is not allowed for a %s order — size it with --qty (the base-asset quantity to sell) only", kind)
			}
			if !has("qty") {
				return output.Usagef("a %s order is sized by --qty (the base-asset quantity to sell)", kind)
			}
		}
		if typ == "market" {
			if has("bestNth") {
				return output.Usagef("--best-nth applies only to --type best orders")
			}
			if tif, ok := p["timeInForce"]; ok && tif != "ioc" {
				return output.Usagef("market orders accept only --tif ioc (or omit --tif)")
			}
		} else {
			if !has("timeInForce") {
				return output.Usagef("best (BBO) orders require --tif (gtc, ioc, fok, or po)")
			}
			if !has("bestNth") {
				return output.Usagef("best (BBO) orders require --best-nth (price level 1-5)")
			}
		}
	}

	if has("ppPercent") && !has("pp") {
		return output.Usagef("--pp-percent requires --pp (price protection)")
	}
	return nil
}
