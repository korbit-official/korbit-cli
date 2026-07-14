// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

// order history / fills — the cursorless window walk over the typed layer.

// pageParams are the per-page paging arguments the history walk supplies to the
// typed page closure: limit and startTime are always set; endTime is nil when
// the window's upper edge is "now".
type pageParams struct {
	limit     int
	startTime int
	endTime   *int
}

type historyOp struct {
	meta             OpMeta
	tsField, idField string
	page             func(ctx context.Context, raw *rawapi.Client, args historyArgs, p pageParams, pol korbit.Policy) (json.RawMessage, error)
}

func (op historyOp) Meta() OpMeta { return op.meta }

// historyArgs is the typed, bound input for the history/fills window walk: the
// per-symbol selectors plus the resolved window and the --limit total-row cap.
// --limit is a total-row cap (newest first), not a per-request size: the walk
// always pages the window at the server limit and the deduped result is trimmed
// to maxRows. 0 = no cap (the whole window).
type historyArgs struct {
	symbol     string
	accountSeq int
	startMs    int64 // 0 until defaulted to now-HistoryWindowMs in Run
	endMs      int64 // 0 = up to now
	maxRows    int
}

// bindHistory converts the validated RunInput into the typed history args once,
// so the walk body reads typed fields instead of the Values map. startMs is left
// 0 here and defaulted against the server clock in Run (which holds the API).
func bindHistory(in RunInput) historyArgs {
	args := historyArgs{
		symbol:     in.Values["symbol"],
		accountSeq: reqAccountSeq(in.Values),
	}
	if s, ok := in.Values["startTime"]; ok {
		args.startMs, _ = strconv.ParseInt(s, 10, 64)
	}
	if s, ok := in.Values["endTime"]; ok {
		args.endMs, _ = strconv.ParseInt(s, 10, 64)
	}
	if s, ok := in.Values["limit"]; ok {
		args.maxRows, _ = strconv.Atoi(s)
	}
	return args
}

// intPtrVal returns the pointed-to int, or 0 when nil — a log-attribute helper
// for an optional page boundary ("now" prints as 0).
func intPtrVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (op historyOp) Run(ctx context.Context, a *API, in RunInput) (Result, error) {
	args := bindHistory(in)
	ctx, h, berr := a.beginOp(ctx, op.meta, in)
	if berr != nil {
		return Result{}, berr
	}
	pol := policyFor(op.meta.Safety, a)
	log := a.log()
	startMs := args.startMs
	if startMs == 0 {
		startMs = a.serverNowMs() - HistoryWindowMs
	}
	log.Debug("history walk: start",
		"op", opJSKeyName(op.meta), "symbol", args.symbol,
		"startMs", startMs, "endMs", args.endMs, "limit", args.maxRows)

	var rows []json.RawMessage
	pages := 0
	seen := map[string]bool{}
	complete, err := WalkHistory(func(pp []korbit.KV) (json.RawMessage, error) {
		p := pageParams{}
		for _, kv := range pp {
			switch kv.Key {
			case "limit":
				p.limit, _ = strconv.Atoi(kv.Value)
			case "startTime":
				p.startTime, _ = strconv.Atoi(kv.Value)
			case "endTime":
				e, _ := strconv.Atoi(kv.Value)
				p.endTime = &e
			}
		}
		pages++
		trace(log, "history walk: page", "op", opJSKeyName(op.meta), "page", pages,
			"startTime", p.startTime, "endTime", intPtrVal(p.endTime))
		return op.page(ctx, a.Raw, args, p, pol)
	}, nil, startMs, args.endMs, op.tsField, func(page []json.RawMessage) {
		for _, row := range page {
			id := jsonNumberField(row, op.idField)
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
			}
			rows = append(rows, row)
		}
	})
	if err != nil {
		log.Debug("history walk: failed", "op", opJSKeyName(op.meta), "pages", pages, "err", err.Error())
		return Result{JournalErr: finishErr(a, h, err)}, err
	}
	// Reaching the --limit cap is a satisfied request, not a truncated window:
	// trim to the newest maxRows and suppress the incomplete-window note.
	gotEnough := args.maxRows > 0 && len(rows) >= args.maxRows
	log.Debug("history walk: done",
		"op", opJSKeyName(op.meta), "pages", pages, "rows", len(rows),
		"complete", complete, "limitReached", gotEnough)
	if args.maxRows > 0 && len(rows) > args.maxRows {
		rows = rows[:args.maxRows]
	}
	res := Result{Data: joinRows(rows)}
	if !complete && !gotEnough {
		res.Truncated = true
		res.Note = fmt.Sprintf("korbit.%s: the requested window was too large to fully cover — the result is incomplete; narrow startTime/endTime or set --limit", opJSKeyName(op.meta))
		fmt.Fprintf(a.stderr(), "korbit-cli: %s\n", res.Note)
	}
	res.JournalErr = finishOK(a, h)
	return res, nil
}
