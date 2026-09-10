// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package callrec

import (
	"fmt"
	"sync/atomic"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/version"
)

// This file implements the operations ledger over the Recorder: Begin makes the
// persist decision ONCE per operation (the same PolicyFunc the per-call path
// uses — the single source of truth), and when recording opens the journal and
// writes the operations row BEFORE any send (the operation's pre-send hard
// guarantee moves here). The returned OpHandle groups every api_call the
// operation makes under that row and folds in the order intent and the final
// outcome. The Recorder satisfies ops.OpJournal through Begin.

// Begin opens the operations ledger for one logical operation. The persist
// decision is made here, once, by running a synthetic CallInfo through the SAME
// PolicyFunc the per-call path uses — so an operation and its calls always agree
// on whether to record. When the decision is "don't record" (or journaling is
// disabled) Begin returns a no-op handle that opens nothing. When recording, it
// is the operation's pre-send hard gate: it opens the journal (a failure is the
// fatal output.Configf open text — nothing has been sent) and writes the
// operations row (its insert failure is likewise fatal pre-send). A pre-send
// open failure is fatal regardless of surface (see the package doc); the
// per-call FailMode governs only post-send write failures.
func (r *Recorder) Begin(o ops.OpStart) (ops.OpHandle, error) {
	info := apiclient.CallInfo{
		Origin:   apiclient.Origin{Surface: o.Surface},
		Auth:     o.Auth,
		Safety:   o.Safety,
		KeyName:  o.KeyName,
		APIKeyID: o.APIKeyID,
	}
	d := r.decide(info)
	r.logDecision(info, d)
	if r.disabled || !d.Record {
		return &OpHandle{recording: false}, nil
	}
	r.log().Debug("journaling operation", "opId", o.OpID, "surface", o.Surface, "safety", string(o.Safety))
	jl, err := r.ensureOpen()
	if err != nil {
		// ensureOpen already wrapped the failure as the journal-open ConfigError.
		return nil, err
	}
	if jl == nil {
		// Recorder closed AFTER we determined this operation must record. Do NOT
		// downgrade to a non-recording handle — for a money op that would let the
		// placement send without its pre-send operations/intent row (the hard
		// guarantee). Refuse pre-send, exactly as an open/insert failure does, so
		// beginOp aborts the operation before anything is sent.
		return nil, output.Configf("cannot record the operation to the action journal at %s: journal closed — set KORBIT_CLI_NO_JOURNAL=1 to disable journaling", r.path)
	}
	id, err := jl.StartOperation(journal.OperationStart{
		StartedAtMs: r.clock(),
		OpID:        o.OpID,
		Surface:     o.Surface,
		Safety:      string(o.Safety),
		KeyName:     o.KeyName,
		APIKeyID:    o.APIKeyID,
		CLIVersion:  version.Version,
	})
	if err != nil {
		return nil, output.Configf("cannot record the operation to the action journal at %s: %v — set KORBIT_CLI_NO_JOURNAL=1 to disable journaling", r.path, err)
	}
	return &OpHandle{
		parent: r, recording: true, failMode: d.PostFailure, operationID: id,
		keyName: o.KeyName, apiKeyID: o.APIKeyID,
	}, nil
}

// OpHandle is the per-operation journal handle. A recording handle groups the
// operation's api_calls under one operations row (each ForCall taking the next
// 1-based sequence) and folds in the order intent and final outcome through the
// shared open Logger. A non-recording handle no-ops everything and opens
// nothing.
type OpHandle struct {
	parent      *Recorder
	recording   bool
	failMode    FailMode // governs the per-call and finish post-send write failures
	operationID int64
	keyName     string // the signing key's name/id for this operation's order rows
	apiKeyID    string
	seq         int64 // atomically incremented per ForCall (1-based)
}

// OperationID returns the operations row id (0 when not recording).
func (h *OpHandle) OperationID() int64 {
	if h == nil || !h.recording {
		return 0
	}
	return h.operationID
}

// ForCall returns a per-api_call recorder bound to this operation, assigning the
// next 1-based sequence within it. A non-recording handle returns nil — the wire
// client treats a nil Recorder as "do not record" (the same sentinel the
// unjournaled path uses), so the send proceeds un-journaled. The journal is
// already open from Begin, so a recording recorder's Ready only captures the
// start timestamp — the hard open gate already fired in Begin.
func (h *OpHandle) ForCall(orderedParams string) apiclient.Recorder {
	if h == nil || !h.recording {
		return nil
	}
	seq := atomic.AddInt64(&h.seq, 1)
	return &opCallRecorder{
		CallRecorder: CallRecorder{
			parent:        h.parent,
			orderedParams: orderedParams,
			operationID:   h.operationID,
			seq:           int(seq),
		},
		failMode: h.failMode,
	}
}

// StartOrder records the order's mint-time intent under this operation BEFORE
// the placement send (the pre-send hard guarantee; its failure is the fatal
// output.Configf order text). A non-recording handle returns a no-op finish.
func (h *OpHandle) StartOrder(in ops.OrderIntent) (ops.OrderFinishFunc, error) {
	if h == nil || !h.recording {
		return func(string, string, string, int) {}, nil
	}
	jl := h.parent.lockedJL()
	if jl == nil {
		// The journal raced shut (TUI shutdown) before this pre-send row. This is
		// the hard-guarantee path, so refuse rather than send an unrecorded order.
		return nil, output.Configf("cannot record the order to the action journal at %s: journal closed — set KORBIT_CLI_NO_JOURNAL=1 to disable journaling", h.parent.path)
	}
	rowID, err := jl.StartOrder(journal.OrderStart{
		CreatedAtMs:   h.parent.clock(),
		OperationID:   h.operationID,
		ClientOrderID: in.ClientOrderID,
		Symbol:        in.Symbol,
		Side:          in.Side,
		OrderType:     in.OrderType,
		Price:         in.Price,
		Qty:           in.Qty,
		Amt:           in.Amt,
		Tif:           in.Tif,
		ParamsJSON:    in.ParamsJSON,
		KeyName:       h.keyName,
		APIKeyID:      h.apiKeyID,
	})
	if err != nil {
		return nil, output.Configf("cannot record the order to the action journal at %s: %v — set KORBIT_CLI_NO_JOURNAL=1 to disable journaling", h.parent.path, err)
	}
	return func(status, orderID, errCode string, attempts int) {
		fin := journal.OrderFinish{
			FinishedAtMs: h.parent.clock(),
			Status:       status, OrderID: orderID, ErrorCode: errCode,
			RetryCount: maxInt(attempts-1, 0),
		}
		jl := h.parent.lockedJL()
		if jl == nil {
			// Journal closed during shutdown; the order's outcome is already
			// decided, so a dropped finish row is diagnostic, not a safety signal.
			return
		}
		if ferr := jl.FinishOrder(rowID, fin); ferr != nil {
			// The order's outcome is already decided; a finish-write failure is
			// diagnostic, so it always warns (never fatal) rather than failing the
			// placement (which already succeeded/failed by now) — hence Warn here
			// regardless of the operation's FailMode.
			h.parent.onPostFailure(Warn, fmt.Errorf("order outcome: %w", ferr))
		}
	}, nil
}

// Finish folds the operation result into its ledger row. The operation's result
// is already determined, so a finish-write failure is diagnostic, not a
// money-safety signal: it always warns. Under FailMode Fail it is ALSO returned
// so the cli can surface it alongside its other journal errors; under Warn it is
// swallowed (returns nil) since the warn already happened.
func (h *OpHandle) Finish(outcome, errorCode string) error {
	if h == nil || !h.recording {
		return nil
	}
	jl := h.parent.lockedJL()
	if jl == nil {
		// Journal closed during shutdown; the operation's result is already
		// decided, so a dropped ledger finish is diagnostic. No-op.
		return nil
	}
	err := jl.FinishOperation(h.operationID, journal.OperationFinish{
		FinishedAtMs: h.parent.clock(),
		Outcome:      outcome,
		ErrorCode:    errorCode,
		Attempts:     int(atomic.LoadInt64(&h.seq)),
	})
	if err == nil {
		return nil
	}
	// Always warn (the operation's result is already decided); under FailMode Fail
	// also return it so the cli surfaces it alongside its other journal errors
	// (ops.Result.JournalErr), under Warn swallow it (the warn already happened).
	h.parent.onPostFailure(Warn, fmt.Errorf("operations ledger: %w", err))
	if h.failMode == Fail {
		return err
	}
	return nil
}

// opCallRecorder is the per-api_call recorder a recording OpHandle hands out. It
// reuses CallRecorder's row-building entirely; it differs only at Ready, where
// the journal is already open (Begin opened it), so Ready only captures the
// start timestamp on the injectable clock and never re-runs the hard open gate.
type opCallRecorder struct {
	CallRecorder
	failMode FailMode
}

// Ready captures the start timestamp on the injectable clock; the journal is
// already open from Begin, so there is no open gate here (it fired in Begin).
func (c *opCallRecorder) Ready(apiclient.CallInfo) error {
	c.startedMs = c.parent.clock()
	c.started = true
	return nil
}

// Record writes the api_calls row under the operation, delivering a write failure
// to the onPostFailure sink with the handle's FailMode (Warn logs/toasts; Fail
// captures for the cli to surface fatally) — never returned.
func (c *opCallRecorder) Record(info apiclient.CallInfo, out apiclient.Outcome) int64 {
	rec := callRecordFrom(info, out)
	rec.OperationID = c.operationID
	rec.Seq = c.seq
	if c.orderedParams != "" {
		rec.ParamsJSON = c.orderedParams
	}
	started := c.startedMs
	if !c.started {
		started = c.parent.clock()
	}
	finished := c.parent.clock()
	rec.StartedAtMs = started
	rec.FinishedAtMs = finished
	rec.DurationMs = finished - started
	jl := c.parent.lockedJL()
	if jl == nil {
		// Journal closed during shutdown (the candle path under --debug). The
		// per-call row is post-send diagnostic, so drop it cleanly.
		return 0
	}
	id, err := jl.LogCall(rec)
	if err != nil {
		c.parent.postFailure(Decision{Record: true, PostFailure: c.failMode}, err)
	}
	return id
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
