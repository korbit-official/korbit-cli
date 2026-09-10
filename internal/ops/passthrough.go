// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/digitalx-official/digitalx-cli/internal/accountseq"
	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
)

// passthroughOp is a catalog entry whose Run is a single typed rawapi call under
// the policy derived from its Safety. call builds the typed request from the
// validated input and issues the matching a.Raw.<Endpoint> call, returning the
// verbatim response bytes (used as Result.Data) plus the call meta.
type passthroughOp struct {
	meta OpMeta
	call func(ctx context.Context, raw *rawapi.Client, in RunInput, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error)
}

func (op passthroughOp) Meta() OpMeta { return op.meta }

func (op passthroughOp) Run(ctx context.Context, a *API, in RunInput) (Result, error) {
	ctx, h, err := a.beginOp(ctx, op.meta, in)
	if err != nil {
		return Result{}, err
	}
	pol := policyFor(op.meta.Safety, a)
	raw, meta, err := op.call(ctx, a.Raw, in, pol)
	// A passthrough makes one call: ok on success, failed on error.
	if err != nil {
		return Result{Attempts: meta.Attempts, JournalErr: finishErr(a, h, err)}, err
	}
	return Result{Data: raw, Attempts: meta.Attempts, JournalErr: finishOK(a, h)}, nil
}

// ---- typed-build helpers (terse request construction from validated values) --

// reqStr returns the value for key (required; "" when absent).
func reqStr(values map[string]string, key string) string { return values[key] }

// optStr returns a pointer to the value for key, or nil when absent.
func optStr(values map[string]string, key string) *string {
	if v, ok := values[key]; ok {
		return &v
	}
	return nil
}

// optInt returns a pointer to the integer value for key, or nil when absent or
// unparseable. The values are already validated as integers, so a parse error
// cannot occur for a present, validated int param.
func optInt(values map[string]string, key string) *int {
	if v, ok := values[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return &n
		}
	}
	return nil
}

// reqInt returns the integer value for key (0 when absent/unparseable).
func reqInt(values map[string]string, key string) int {
	if v, ok := values[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// reqAccountSeq returns the accountSeq that the frontend must have resolved
// into values before calling ops. Panics on absence — this is a programming
// error in the frontend layer (every surface calls accountseq.Ensure).
//
// Layering contract:
//   - User surface (CLI flag, MCP tool arg, bot JS param): accountSeq is OPTIONAL.
//     Users may omit it; the frontend fills it via accountseq.Ensure (per-key
//     default, falling back to 1).
//   - Ops layer (this package): accountSeq is REQUIRED in RunInput.Values.
//     The ops layer trusts the frontend resolved it; absence is a bug.
//   - rawapi layer: accountSeq is *int (optional on the wire, spec-faithful).
//     Ops converts int → &int at the rawapi call boundary.
func reqAccountSeq(values map[string]string) int {
	v, ok := values[accountseq.APIName]
	if !ok || v == "" {
		panic("BUG: accountSeq must be resolved by frontend before calling ops")
	}
	n, _ := strconv.Atoi(v) // already validated by Ensure
	return n
}

// optBool returns a pointer to true when the (flag) key is present, else nil —
// matching the wire layer's omit-when-absent encoding for optional booleans.
func optBool(values map[string]string, key string) *bool {
	if _, ok := values[key]; ok {
		t := true
		return &t
	}
	return nil
}
