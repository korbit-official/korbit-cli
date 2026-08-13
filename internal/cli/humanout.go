// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/output"
)

// This file owns the human-readable output mode. The output package owns the
// wire (JSON) contract and stays command-agnostic; the presentation toolkit
// (tables, key/value blocks, number grouping) lives in the cli/textout
// subpackage so a command subpackage can reuse it; command-aware formatting
// lives here, in cli, because only cli knows which command produced a result.
//
// emitMode is how every success-path stdout write is routed. In JSON mode it
// emits exactly one JSON document (EmitJSON, compact respected). In human mode a
// local/command-package command renders itself (its result implements
// textout.TextFormatter); a catalog-driven endpoint command renders its verbatim
// API bytes through the command's keyed per-command formatter. Every command is
// covered by one of the two, so human mode never prints JSON. The pretty-JSON
// branch at the end is a defensive last resort for a payload whose shape a
// formatter doesn't recognize (e.g. an unexpected API response); it is not an
// expected mode of operation.

// emit routes a success result for commandKey to stdout in the active mode.
func (rt *runtime) Emit(commandKey string, value any) error {
	return emitMode(rt.io, rt.jsonMode(), rt.compact, commandKey, value)
}

// emitMode is the shared mode-aware emitter (used by runtime and the key
// commands). jsonMode selects the machine contract; compact only matters in JSON
// mode.
func emitMode(io output.IO, jsonMode, compact bool, commandKey string, value any) error {
	if jsonMode {
		return io.EmitJSON(value, compact)
	}
	// Human mode. A local/command-package command renders itself: its result type
	// implements textout.TextFormatter and writes its own text from typed fields
	// (no command-key lookup, no JSON round-trip). It names the running binary via
	// progname directly, so there is no {prog} placeholder to substitute here.
	if tf, ok := value.(textout.TextFormatter); ok {
		var b strings.Builder
		tf.FormatText(&b)
		return io.EmitText(b.String())
	}
	// A catalog-driven endpoint command emits verbatim API bytes whose shape
	// varies per endpoint, so it renders through the command's keyed formatter.
	// Render the value to raw JSON so that formatter can read its fields; its
	// {prog} placeholder is substituted with the resolved program name so a
	// suggested follow-up command names the running binary.
	if f := endpointFormatters[commandKey]; f != nil {
		if raw := textout.RawJSON(value); raw != nil {
			if text, ok := f(raw); ok {
				return io.EmitText(withProg(text))
			}
		}
	}
	// Defensive last resort: a payload the formatter didn't recognize (or a
	// command without one). Pretty-printed JSON — the same bytes as `--json`.
	return io.EmitJSON(value, false)
}

// endpointFormatter renders a catalog-driven endpoint command's verbatim API
// payload (a json.RawMessage) as human-readable text — the keyed counterpart to
// the textout.TextFormatter that local/command-package commands implement on
// their typed results. It returns ok=false to defer to the pretty-JSON fallback
// (e.g. when the payload shape isn't what the formatter expects), so a surprising
// response never produces a misleading table.
type endpointFormatter func(raw json.RawMessage) (string, bool)

var endpointFormatters = map[string]endpointFormatter{
	"ticker":        fmtTicker,
	"orderbook":     fmtOrderbook,
	"trades":        fmtTrades,
	"candles":       fmtCandles,
	"pairs":         fmtPairs,
	"ticksize":      fmtTicksize,
	"currencies":    fmtCurrencies,
	"time":          fmtTime,
	"order place":   fmtOrder,
	"order get":     fmtOrder,
	"order cancel":  fmtOrderCancel,
	"order open":    fmtOrderList,
	"order history": fmtOrderList,
	"fills":         fmtFills,
	"balance":       fmtBalance,
	"fees":          fmtFees,
	"whoami":        fmtWhoami,

	// funding (deposits / withdrawals)
	"deposit addresses":    arrayTableFmt("(no deposit addresses)", colsDepositAddress),
	"deposit address":      objectKVFmt(colsDepositAddress),
	"deposit generate":     objectKVFmt(colsDepositAddress),
	"deposit history":      arrayTableFmt("(no deposits)", colsCoinDeposit),
	"deposit status":       objectKVFmt(colsCoinDepositDetail),
	"withdraw addresses":   arrayTableFmt("(no registered withdrawal addresses)", colsWithdrawAddress),
	"withdraw amount":      arrayTableFmt("(no withdrawable assets)", colsWithdrawAmount),
	"withdraw request":     objectKVFmt(colsWithdrawRequest),
	"withdraw cancel":      ackFmt("withdrawal cancellation accepted (confirm with `{prog} withdraw status <currency> --id <id>`)"),
	"withdraw history":     arrayTableFmt("(no withdrawals)", colsCoinWithdrawal),
	"withdraw status":      objectKVFmt(colsCoinWithdrawalDetail),
	"krw deposit request":  ackFmt("KRW deposit push sent — confirm it in the Korbit app (track with `{prog} krw deposit history`)"),
	"krw withdraw request": ackFmt("KRW withdrawal push sent — confirm it in the Korbit app (track with `{prog} krw withdraw history`)"),
	"krw deposit history":  arrayTableFmt("(no KRW deposits)", colsKrwDeposit),
	"krw withdraw history": arrayTableFmt("(no KRW withdrawals)", colsKrwWithdrawal),
}

// ---- per-command formatters ----

func fmtTicker(raw json.RawMessage) (string, bool) {
	arr, ok := textout.AsArray(raw)
	if !ok {
		return "", false
	}
	headers := []string{"symbol", "last", "change%", "high", "low", "volume"}
	align := []bool{false, true, true, true, true, true}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "symbol"),
			textout.Num(textout.Jstr(m, "close")),
			textout.Jstr(m, "priceChangePercent"),
			textout.Num(textout.Jstr(m, "high")),
			textout.Num(textout.Jstr(m, "low")),
			textout.Num(textout.Jstr(m, "volume")),
		})
	}
	return textout.Table(headers, rows, align), true
}

func fmtOrderbook(raw json.RawMessage) (string, bool) {
	m, ok := textout.AsObject(raw)
	if !ok {
		return "", false
	}
	bids, ok1 := textout.AsArray(m["bids"])
	asks, ok2 := textout.AsArray(m["asks"])
	if !ok1 || !ok2 {
		return "", false
	}
	level := func(e json.RawMessage) (string, string) {
		o, ok := textout.AsObject(e)
		if !ok {
			return "", ""
		}
		return textout.Num(textout.Jstr(o, "price")), textout.Num(textout.Jstr(o, "qty"))
	}
	var b strings.Builder
	if ts := textout.Jstr(m, "timestamp"); ts != "" {
		fmt.Fprintf(&b, "timestamp: %s\n\n", ts)
	}
	// Asks shown high->low above bids, like a depth ladder.
	b.WriteString("Asks (price / qty)\n")
	ar := make([][]string, 0, len(asks))
	for _, e := range asks {
		p, q := level(e)
		ar = append(ar, []string{p, q})
	}
	for i := len(ar) - 1; i >= 0; i-- {
		ar2 := ar[i]
		fmt.Fprintf(&b, "  %s  %s\n", textout.PadLeftRune(ar2[0], 16), ar2[1])
	}
	b.WriteString("Bids (price / qty)\n")
	for _, e := range bids {
		p, q := level(e)
		fmt.Fprintf(&b, "  %s  %s\n", textout.PadLeftRune(p, 16), q)
	}
	return strings.TrimRight(b.String(), "\n"), true
}

func fmtTrades(raw json.RawMessage) (string, bool) {
	arr, ok := textout.AsArray(raw)
	if !ok {
		return "", false
	}
	headers := []string{"tradeId", "time", "price", "qty", "buyerTaker"}
	align := []bool{true, true, true, true, false}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "tradeId"),
			textout.Jstr(m, "timestamp"),
			textout.Num(textout.Jstr(m, "price")),
			textout.Num(textout.Jstr(m, "qty")),
			textout.Jstr(m, "isBuyerTaker"),
		})
	}
	return textout.Table(headers, rows, align), true
}

func fmtCandles(raw json.RawMessage) (string, bool) {
	data, note, ok := textout.ListView(raw)
	if !ok {
		return "", false
	}
	arr, ok := textout.AsArray(data)
	if !ok {
		return "", false
	}
	headers := []string{"time", "open", "high", "low", "close", "volume"}
	align := []bool{true, true, true, true, true, true}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "timestamp"),
			textout.Num(textout.Jstr(m, "open")),
			textout.Num(textout.Jstr(m, "high")),
			textout.Num(textout.Jstr(m, "low")),
			textout.Num(textout.Jstr(m, "close")),
			textout.Num(textout.Jstr(m, "volume")),
		})
	}
	return textout.WithTruncationNote(textout.Table(headers, rows, align), note), true
}

func fmtPairs(raw json.RawMessage) (string, bool) {
	arr, ok := textout.AsArray(raw)
	if !ok {
		return "", false
	}
	// The bound columns are the reason to read this endpoint before sizing an
	// order, so the text table carries them too — the agent-facing notes point
	// at `pairs` for them, and a two-column table would send a reader to --json
	// with no hint that they were missing. A field the pair does not publish
	// renders as an em dash, never as a blank: in a right-aligned numeric column
	// a blank cell reads as zero, and zero is a bound this market would enforce.
	headers := []string{"symbol", "status", "base", "quote", "minOrderValue", "maxOrderValue"}
	var rows [][]string
	anyCurrency := false
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		quote := textout.Jstr(m, "quoteCurrency")
		if quote != "" {
			anyCurrency = true
		}
		rows = append(rows, []string{
			textout.Jstr(m, "symbol"),
			textout.Jstr(m, "status"),
			orDash(textout.Jstr(m, "baseCurrency")),
			orDash(quote),
			orDash(textout.Jstr(m, "minOrderValue")),
			orDash(textout.Jstr(m, "maxOrderValue")),
		})
	}
	table := textout.Table(headers, rows, []bool{false, false, false, false, true, true})
	// No pair publishing a currency means the server itself predates these
	// fields, so all four columns are empty for every row. Say so, because the
	// dashes alone invite the one wrong reading that costs money: that these
	// markets have no order value bounds. They may well have them — this server
	// just does not publish the figures, and it still enforces whatever it enforces.
	if len(rows) > 0 && !anyCurrency {
		return table + "\n\nnote: this server publishes no currency or order value fields, so those columns are empty for every pair. That is not a statement that these markets are unbounded — size against the server's own rejection, not against these blanks.", true
	}
	return table, true
}

// orDash renders an absent table cell as an em dash, distinguishing "the pair
// does not publish this" from a zero or an empty string.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fmtTicksize(raw json.RawMessage) (string, bool) {
	arr, ok := textout.AsArray(raw)
	if !ok {
		return "", false
	}
	var b strings.Builder
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s\n", textout.Jstr(m, "symbol"))
		policy, ok := textout.AsArray(m["tickSizePolicy"])
		if ok {
			b.WriteString("  priceGte / tickSize\n")
			for _, p := range policy {
				po, ok := textout.AsObject(p)
				if !ok {
					continue
				}
				fmt.Fprintf(&b, "    %s  %s\n", textout.PadLeftRune(textout.Num(textout.Jstr(po, "priceGte")), 14), textout.Num(textout.Jstr(po, "tickSize")))
			}
		}
		if levels, ok := textout.AsArray(m["orderbookLevels"]); ok {
			parts := make([]string, 0, len(levels))
			for _, lv := range levels {
				var s string
				_ = json.Unmarshal(lv, &s)
				parts = append(parts, s)
			}
			fmt.Fprintf(&b, "  orderbookLevels: %s\n", strings.Join(parts, ", "))
		}
	}
	return strings.TrimRight(b.String(), "\n"), true
}

func fmtCurrencies(raw json.RawMessage) (string, bool) {
	arr, ok := textout.AsArray(raw)
	if !ok {
		return "", false
	}
	headers := []string{"name", "fullName", "deposit", "withdrawal"}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "name"),
			textout.Jstr(m, "fullName"),
			textout.Jstr(m, "depositStatus"),
			textout.Jstr(m, "withdrawalStatus"),
		})
	}
	return textout.Table(headers, rows, []bool{false, false, false, false}), true
}

func fmtTime(raw json.RawMessage) (string, bool) {
	m, ok := textout.AsObject(raw)
	if !ok {
		return "", false
	}
	t := textout.Jstr(m, "time")
	if t == "" {
		return "", false
	}
	line := "server time (unix ms): " + t
	// The skew fields are present only when a clean single-RTT sample was taken
	// (ops.withTimeSkew appends them); render them as a second human line when so.
	var sk struct {
		OffsetMs      *int64 `json:"offsetMs"`
		RTTMs         *int64 `json:"rttMs"`
		UncertaintyMs *int64 `json:"uncertaintyMs"`
	}
	if json.Unmarshal(raw, &sk) == nil && sk.OffsetMs != nil && sk.RTTMs != nil {
		unc := int64(0)
		if sk.UncertaintyMs != nil {
			unc = *sk.UncertaintyMs
		}
		line += "\n" + clockSkewLine(*sk.OffsetMs, *sk.RTTMs, unc)
	}
	return line, true
}

// clockSkewLine renders the local-clock skew for `time` in doctor's
// direction/sign convention (offsetMs = serverClock - localClock, so a positive
// offset means the local clock runs BEHIND the server). A magnitude within the
// ±uncertainty measurement noise reads as "in sync" rather than a spurious drift.
func clockSkewLine(offsetMs, rttMs, uncertaintyMs int64) string {
	mag := offsetMs
	if mag < 0 {
		mag = -mag
	}
	if mag <= uncertaintyMs {
		return fmt.Sprintf("local clock: in sync with the server (~%dms, within the ±%dms measurement noise; rtt %dms)", mag, uncertaintyMs, rttMs)
	}
	dir := "BEHIND"
	if offsetMs < 0 {
		dir = "AHEAD of"
	}
	return fmt.Sprintf("local clock: ~%dms %s the server (rtt %dms, ±%dms)", mag, dir, rttMs, uncertaintyMs)
}

func fmtOrder(raw json.RawMessage) (string, bool) {
	m, ok := textout.AsObject(raw)
	if !ok {
		return "", false
	}
	rows := [][2]string{
		{"orderId", textout.Jstr(m, "orderId")},
		{"clientOrderId", textout.Jstr(m, "clientOrderId")},
		{"symbol", textout.Jstr(m, "symbol")},
		{"side", textout.Jstr(m, "side")},
		{"type", textout.Jstr(m, "orderType")},
		{"timeInForce", textout.Jstr(m, "timeInForce")},
		{"status", textout.Jstr(m, "status")},
		{"price", textout.Num(textout.Jstr(m, "price"))},
		{"qty", textout.Num(textout.Jstr(m, "qty"))},
		{"filledQty", textout.Num(textout.Jstr(m, "filledQty"))},
		{"filledAmt", textout.Num(textout.Jstr(m, "filledAmt"))},
		{"avgPrice", textout.Num(textout.Jstr(m, "avgPrice"))},
		{"createdAt", textout.Jstr(m, "createdAt")},
		{"lastFilledAt", textout.Jstr(m, "lastFilledAt")},
	}
	return textout.KVBlock(rows), true
}

func fmtOrderCancel(raw json.RawMessage) (string, bool) {
	m, ok := textout.AsObject(raw)
	if !ok {
		return "", false
	}
	// A bare ack normalizes to {"success": true}; an order-id echo may appear.
	if id := textout.Jstr(m, "orderId"); id != "" {
		return "cancellation accepted for orderId " + id + " (confirm with `{prog} order get`)", true
	}
	if _, ok := m["success"]; ok && len(m) == 1 {
		return "cancellation accepted (confirm with `{prog} order get`)", true
	}
	return "", false
}

func fmtOrderList(raw json.RawMessage) (string, bool) {
	data, note, ok := textout.ListView(raw)
	if !ok {
		return "", false
	}
	arr, ok := textout.AsArray(data)
	if !ok {
		return "", false
	}
	if len(arr) == 0 {
		return textout.WithTruncationNote("(no orders)", note), true
	}
	headers := []string{"orderId", "side", "type", "status", "price", "qty", "filledQty"}
	align := []bool{true, false, false, false, true, true, true}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "orderId"),
			textout.Jstr(m, "side"),
			textout.Jstr(m, "orderType"),
			textout.Jstr(m, "status"),
			textout.Num(textout.Jstr(m, "price")),
			textout.Num(textout.Jstr(m, "qty")),
			textout.Num(textout.Jstr(m, "filledQty")),
		})
	}
	return textout.WithTruncationNote(textout.Table(headers, rows, align), note), true
}

func fmtFills(raw json.RawMessage) (string, bool) {
	data, note, ok := textout.ListView(raw)
	if !ok {
		return "", false
	}
	arr, ok := textout.AsArray(data)
	if !ok {
		return "", false
	}
	if len(arr) == 0 {
		return textout.WithTruncationNote("(no fills)", note), true
	}
	headers := []string{"tradeId", "orderId", "side", "price", "qty", "amt", "fee", "feeCcy", "taker"}
	align := []bool{true, true, false, true, true, true, true, false, false}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "tradeId"),
			textout.Jstr(m, "orderId"),
			textout.Jstr(m, "side"),
			textout.Num(textout.Jstr(m, "price")),
			textout.Num(textout.Jstr(m, "qty")),
			textout.Num(textout.Jstr(m, "amt")),
			textout.Num(textout.Jstr(m, "feeQty")),
			textout.Jstr(m, "feeCurrency"),
			textout.Jstr(m, "isTaker"),
		})
	}
	return textout.WithTruncationNote(textout.Table(headers, rows, align), note), true
}

func fmtBalance(raw json.RawMessage) (string, bool) {
	arr, ok := textout.AsArray(raw)
	if !ok {
		return "", false
	}
	if len(arr) == 0 {
		return "(no balances)", true
	}
	headers := []string{"currency", "balance", "available", "tradeInUse", "withdrawalInUse"}
	align := []bool{false, true, true, true, true}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "currency"),
			textout.Num(textout.Jstr(m, "balance")),
			textout.Num(textout.Jstr(m, "available")),
			textout.Num(textout.Jstr(m, "tradeInUse")),
			textout.Num(textout.Jstr(m, "withdrawalInUse")),
		})
	}
	return textout.Table(headers, rows, align), true
}

func fmtFees(raw json.RawMessage) (string, bool) {
	arr, ok := textout.AsArray(raw)
	if !ok {
		return "", false
	}
	headers := []string{"symbol", "buyFeeCcy", "sellFeeCcy", "maxRate", "takerRate", "makerRate"}
	align := []bool{false, false, false, true, true, true}
	var rows [][]string
	for _, e := range arr {
		m, ok := textout.AsObject(e)
		if !ok {
			return "", false
		}
		rows = append(rows, []string{
			textout.Jstr(m, "symbol"),
			textout.Jstr(m, "buyFeeCurrency"),
			textout.Jstr(m, "sellFeeCurrency"),
			textout.Jstr(m, "maxFeeRate"),
			textout.Jstr(m, "takerFeeRate"),
			textout.Jstr(m, "makerFeeRate"),
		})
	}
	return textout.Table(headers, rows, align), true
}

func fmtWhoami(raw json.RawMessage) (string, bool) {
	m, ok := textout.AsObject(raw)
	if !ok {
		return "", false
	}
	perms := ""
	if a, ok := textout.AsArray(m["permissions"]); ok {
		var ps []string
		for _, p := range a {
			var s string
			_ = json.Unmarshal(p, &s)
			ps = append(ps, s)
		}
		sort.Strings(ps)
		perms = strings.Join(ps, ", ")
	}
	seqs := ""
	if a, ok := textout.AsArray(m["allowedAccountSeqs"]); ok {
		var ss []string
		for _, s := range a {
			ss = append(ss, strings.TrimSpace(string(s)))
		}
		seqs = strings.Join(ss, ", ")
	}
	rows := [][2]string{
		{"apiKey", textout.Jstr(m, "apiKey")},
		{"userUuid", textout.Jstr(m, "userUuid")},
		{"type", textout.Jstr(m, "type")},
		{"publicKey", textout.Jstr(m, "publicKey")},
		{"label", textout.Jstr(m, "label")},
		{"status", textout.Jstr(m, "status")},
		{"permissions", perms},
		{"ipAllowlist", textout.Jstr(m, "whitelist")},
		{"allowedAccountSeqs", seqs},
		{"expiration", textout.Jstr(m, "expiration")},
		{"createdAt", textout.Jstr(m, "createdAt")},
	}
	return textout.KVBlock(rows), true
}
