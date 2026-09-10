// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

// funding histories — the honest 100-row cap over the typed layer.

type fundingHistoryOp struct {
	meta OpMeta
	call func(ctx context.Context, raw *rawapi.Client, args fundingArgs, pol apiclient.Policy) (json.RawMessage, error)
}

func (op fundingHistoryOp) Meta() OpMeta { return op.meta }

// fundingArgs is the typed, bound input for the funding-history operations: the
// optional asset selector and the effective row limit (the 100-row default
// applied). The crypto histories read currency; the account-wide krw histories
// ignore it. limit is always sent explicitly (these endpoints have no spec
// default) and doubles as the saturation threshold for the honest truncation note.
type fundingArgs struct {
	currency   string // "" when absent (the krw histories)
	limit      int
	accountSeq int // resolved by the frontend (default 1); funding is main-only
}

func bindFunding(in RunInput) fundingArgs {
	limit := 100
	if n := optInt(in.Values, "limit"); n != nil {
		limit = *n
	}
	return fundingArgs{currency: in.Values["currency"], limit: limit, accountSeq: reqAccountSeq(in.Values)}
}

func (op fundingHistoryOp) Run(ctx context.Context, a *API, in RunInput) (Result, error) {
	args := bindFunding(in)
	ctx, h, berr := a.beginOp(ctx, op.meta, in)
	if berr != nil {
		return Result{}, berr
	}
	pol := policyFor(op.meta.Safety, a)
	data, err := op.call(ctx, a.Raw, args, pol)
	if err != nil {
		return Result{JournalErr: finishErr(a, h, err)}, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return Result{JournalErr: finishErr(a, h, err)}, fmt.Errorf("history response was not an array: %v", err)
	}
	res := Result{Data: joinRows(rows)}
	if len(rows) >= args.limit {
		res.Truncated = true
		res.Note = fmt.Sprintf("api.%s: the endpoint returned its full %d-row limit — older rows exist but cannot be reached (the endpoint has no time range or cursor)", opJSKeyName(op.meta), args.limit)
		fmt.Fprintf(a.stderr(), "korbit-cli: %s\n", res.Note)
	}
	res.JournalErr = finishOK(a, h)
	return res, nil
}
