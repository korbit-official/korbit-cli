// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
)

// This file defines the journal seam ops sees: a set of interfaces a frontend
// satisfies (with internal/callrec) so ops can group an operation's calls under
// one ledger row without importing internal/journal or internal/callrec — ops
// owns NO journal policy and pulls in NO SQLite. An operation Begins once (the
// persist decision is made there, before any send), threads the returned handle
// through ctx, records each call via the handle, and Finishes it.

// OpStart describes a logical operation as it begins, for the operations ledger.
// The frontend's OpJournal stamps the row times from its own (system) clock, so
// ops supplies no timestamp.
type OpStart struct {
	OpID     string // dotted operation id, e.g. "order.place"
	Surface  string // the frontend that issued it: cli|monitor|mcp|...
	Safety   cmdmeta.Safety
	Auth     bool
	KeyName  string
	APIKeyID string
}

// OrderIntent is an order's mint-time intent, recorded BEFORE the placement send
// (the hard pre-send guarantee). It is keyed to the operation by the OpHandle.
// The frontend stamps the row's create time from its own (system) clock.
type OrderIntent struct {
	ClientOrderID string
	Symbol        string
	Side          string
	OrderType     string
	Price         string
	Qty           string
	Amt           string
	Tif           string
	ParamsJSON    string
}

// OrderFinishFunc folds the placement outcome into the order's journal row.
// Best-effort: a failure here is reported out-of-band, never turned into a
// placement failure the caller could mistake for "not placed".
type OrderFinishFunc func(status, orderID, errorCode string, attempts int)

// OpHandle is the per-operation journal handle a frontend's OpJournal returns
// from Begin. It groups the operation's api_calls under one operations row and
// holds the (already-made) persist decision: a non-recording handle no-ops
// everything and opens nothing.
type OpHandle interface {
	// OperationID is the operations row id (0 when not recording).
	OperationID() int64
	// ForCall returns a per-api_call recorder bound to this operation, assigning
	// the next 1-based sequence within it. orderedParams overrides the recorded
	// params_json with the spec/insertion order ("" keeps the wire client's). It
	// returns nil when the operation is not recorded; the wire client treats a nil
	// Recorder as "do not record", so the send still proceeds.
	ForCall(orderedParams string) apiclient.Recorder
	// StartOrder records the order's mint-time intent BEFORE the placement send
	// (the hard pre-send guarantee) and returns the finish hook. An error aborts
	// the placement with nothing on the wire.
	StartOrder(OrderIntent) (OrderFinishFunc, error)
	// Finish folds the operation result into its ledger row. The api_calls count
	// is the handle's own (it counted every ForCall) and the finish time comes from
	// the frontend's own clock, so callers pass only the outcome. A finish-write
	// failure is diagnostic, not a money-safety signal; it is returned for the
	// caller to surface or warn as its surface dictates.
	Finish(outcome, errorCode string) error
}

// OpJournal is the frontend-supplied journal seam: Begin makes the persist
// decision once and opens the operations ledger row (pre-send) when recording.
type OpJournal interface {
	Begin(OpStart) (OpHandle, error)
}

// handleCtxKey is the context key carrying the active operation's OpHandle from
// an Operation.Run down to the recording doer behind a.Raw.
type handleCtxKey struct{}

// withHandle returns ctx carrying h, so the doer can bind a per-call recorder to
// the operation (HandleFromContext).
func withHandle(ctx context.Context, h OpHandle) context.Context {
	return context.WithValue(ctx, handleCtxKey{}, h)
}

// HandleFromContext returns the operation's OpHandle from ctx, or nil if absent.
// The recording doer reads it to obtain a per-call recorder (ForCall) carrying
// the operation id + sequence; an absent handle leaves the call unrecorded.
func HandleFromContext(ctx context.Context) OpHandle {
	h, _ := ctx.Value(handleCtxKey{}).(OpHandle)
	return h
}

// noopHandle is the process-local handle used when no OpJournal is wired (a.Journal
// nil): it records nothing, opens nothing, and leaves Client.Rec nil so sends
// proceed un-journaled.
type noopHandle struct{}

func (noopHandle) OperationID() int64                { return 0 }
func (noopHandle) ForCall(string) apiclient.Recorder { return nil }
func (noopHandle) StartOrder(OrderIntent) (OrderFinishFunc, error) {
	return func(string, string, string, int) {}, nil
}
func (noopHandle) Finish(string, string) error { return nil }

// beginOp opens the operations ledger for one Operation.Run: it builds the
// OpStart from the operation's metadata and the invocation surface, lets the
// frontend's OpJournal make the persist decision (and open the ledger row
// pre-send when recording), and threads the handle into ctx so the recording
// doer can bind each call to it. With no OpJournal wired it returns a no-op
// handle. A non-nil error is a pre-send journal failure and aborts the operation.
func (a *API) beginOp(ctx context.Context, m OpMeta, in RunInput) (context.Context, OpHandle, error) {
	if a.Journal == nil {
		h := noopHandle{}
		return withHandle(ctx, h), h, nil
	}
	h, err := a.Journal.Begin(OpStart{
		OpID:     opJSKeyName(m),
		Surface:  in.Controls.Surface,
		Safety:   m.Safety,
		Auth:     m.Auth != nil,
		KeyName:  in.KeyName,
		APIKeyID: in.APIKeyID,
	})
	if err != nil {
		return ctx, nil, err
	}
	return withHandle(ctx, h), h, nil
}

// Operation-ledger outcomes folded in by OpHandle.Finish. They mirror the
// operations.outcome values the journal stores; the initial 'running' state is
// written by the journal itself when the row is opened.
const (
	outcomeOK      = "ok"
	outcomeFailed  = "failed"
	outcomeUnknown = "unknown"
)

// finishErr folds a failed operation outcome into the ledger, taking the wire
// error code from err (empty for a non-API error). finishOK folds a success.
// Both return any out-of-band ledger-write error (the finish time is stamped by
// the frontend's journal clock).
func finishErr(_ *API, h OpHandle, err error) error {
	return h.Finish(outcomeFailed, codeOf(apiErrOf(err)))
}

func finishOK(_ *API, h OpHandle) error {
	return h.Finish(outcomeOK, "")
}
