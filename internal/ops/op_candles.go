// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
)

// candles — auto-pages past the server's 200-candle per-request cap.

// CandlesMaxLimit is the auto-paging ceiling for the candles operation: 25
// requests of the server's 200-candle pages. Far beyond indicator warm-up needs
// while keeping a typo'd limit from turning into an unbounded crawl.
const CandlesMaxLimit = 5000

// candlesServerPage is the server's per-request candle cap.
const candlesServerPage = 200

// candlesOp is the candles operation: a limit at or below the server's
// 200-per-request cap is one GET; a larger limit pages backwards and merges,
// deduplicated by candle timestamp, returned ascending, up to CandlesMaxLimit.
type candlesOp struct{ meta OpMeta }

func (op candlesOp) Meta() OpMeta { return op.meta }

// candlesArgs is the typed, bound input for the candles operation: the wire
// request plus the effective row limit (the spec default applied).
type candlesArgs struct {
	req   rawapi.CandlesRequest
	limit int
}

// bindCandles converts the validated RunInput into the typed candles args once,
// so the operation body reads typed fields instead of the Values map.
func bindCandles(in RunInput) candlesArgs {
	req := rawapi.CandlesRequest{
		Symbol:    rawapi.Symbol(reqStr(in.Values, "symbol")),
		Interval:  reqStr(in.Values, "interval"),
		Limit:     reqInt(in.Values, "limit"),
		StartTime: optInt(in.Values, "startTime"),
		EndTime:   optInt(in.Values, "endTime"),
	}
	return candlesArgs{req: req, limit: req.Limit}
}

func (op candlesOp) Run(ctx context.Context, a *API, in RunInput) (Result, error) {
	args := bindCandles(in)
	ctx, h, berr := a.beginOp(ctx, op.meta, in)
	if berr != nil {
		return Result{}, berr
	}
	// finishFailed folds a failed operation outcome into its ledger row on the way
	// out of any return path; the handle counts the underlying GETs itself.
	finishFailed := func(err error) Result {
		return Result{JournalErr: finishErr(a, h, err)}
	}
	pol := policyFor(op.meta.Safety, a)
	if args.limit <= candlesServerPage {
		_, b, _, err := a.Raw.Candles(ctx, args.req, pol)
		if err != nil {
			return finishFailed(err), err
		}
		return Result{Data: b, JournalErr: finishOK(a, h)}, nil
	}

	log := a.log()
	endMs := int64(0)
	if args.req.EndTime != nil {
		endMs = int64(*args.req.EndTime)
	}
	startMs := int64(0)
	if args.req.StartTime != nil {
		startMs = int64(*args.req.StartTime)
	}
	log.Debug("candles: paging past the server cap",
		"symbol", string(args.req.Symbol), "interval", args.req.Interval,
		"limit", args.limit, "serverPage", candlesServerPage)

	type candle struct {
		ts  int64
		raw json.RawMessage
	}
	byTs := map[int64]json.RawMessage{}
	curEnd := endMs
	pages := 0
	for len(byTs) < args.limit {
		pages++
		want := args.limit - len(byTs)
		if want > candlesServerPage {
			want = candlesServerPage
		}
		var endPtr *int
		if curEnd > 0 {
			e := int(curEnd)
			endPtr = &e
		}
		_, data, _, err := a.Raw.Candles(ctx, candlesPage(args.req, want, endPtr), pol)
		if err != nil {
			return finishFailed(err), err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil {
			return finishFailed(err), fmt.Errorf("candles response was not an array: %v", err)
		}
		minTs := int64(0)
		fresh := 0
		for _, row := range page {
			ts := jsonInt64Field(row, "timestamp")
			if ts <= 0 {
				continue
			}
			if minTs == 0 || ts < minTs {
				minTs = ts
			}
			if _, dup := byTs[ts]; !dup {
				byTs[ts] = row
				fresh++
			}
		}
		trace(log, "candles: page", "page", pages, "got", len(page), "fresh", fresh, "total", len(byTs))
		if len(page) < want || minTs == 0 || fresh == 0 {
			break
		}
		if startMs > 0 && minTs <= startMs {
			break
		}
		curEnd = minTs - 1
	}
	log.Debug("candles: paging done", "pages", pages, "candles", len(byTs))
	candles := make([]candle, 0, len(byTs))
	for ts, raw := range byTs {
		candles = append(candles, candle{ts, raw})
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].ts < candles[j].ts })
	if len(candles) > args.limit {
		candles = candles[len(candles)-args.limit:]
	}
	rows := make([]json.RawMessage, len(candles))
	for i, cd := range candles {
		rows[i] = cd.raw
	}
	return Result{Data: joinRows(rows), JournalErr: finishOK(a, h)}, nil
}

// candlesPage returns base with the per-page limit and endTime overridden
// (endTime nil = omit), leaving symbol/interval/startTime intact.
func candlesPage(base rawapi.CandlesRequest, limit int, endTime *int) rawapi.CandlesRequest {
	base.Limit = limit
	base.EndTime = endTime
	return base
}
