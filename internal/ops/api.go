// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

// API is the L2 operations layer over the typed L1 endpoints. It owns ALL retry
// and idempotency policy: every operation decides the apiclient.Policy the wire
// client will execute and the reconcile protocol around it. A frontend builds
// one API and asks an Operation to Run; it never decides call policy itself.
//
// Build one with NewAPI, which seeds the clock/time/scheduling seams (Resync,
// ServerNow, Sleep, Log) from the *apiclient.Client the Operations call through, so
// they cannot drift from the client that actually signs and sends. A frontend
// then sets only the ops-level policy (RetryBudgetMs, Journal, Stderr). The zero
// RetryBudgetMs is treated as "no budgeted (sleeping) retries", same as a single
// shot for the budgeted classes.
type API struct {
	// Raw is the typed endpoint layer the registered Operations call, over the
	// journaled wire client. NewAPI sets it (rawapi.New over the client). Required.
	Raw *rawapi.Client
	// Resync re-measures the server clock once when a single-shot send is
	// rejected with EXCEED_TIME_WINDOW (provably pre-execution) — the place
	// protocol's clock-resync hook (the ONLY user of this field, since that
	// protocol drives single-shot sends through its own reconcile loop rather than
	// the L1 client's retry, which corrects EXCEED_TIME_WINDOW itself). NewAPI
	// seeds it from apiclient.Client.Resync — the SAME resync primitive — so the two
	// never diverge. nil disables that one corrective resync.
	Resync func() error
	// ServerNow is the server-clock estimate in unix ms, used to default the
	// history walk's lookback window (a server-relative time filter sent on the
	// wire). NewAPI seeds it from apiclient.Client.ServerNowMs — the same clock the
	// client signs against. nil falls back to the local wall clock. (The journal's
	// row times do NOT use this — they are stamped by the frontend's own system
	// clock in internal/callrec.)
	ServerNow func() int64
	// Sleep is the inter-retry delay primitive for the place protocol's budgeted
	// waits; NewAPI seeds it from apiclient.Client.Sleep. nil = time.Sleep.
	Sleep func(time.Duration)
	// RetryBudgetMs bounds the place protocol's total reconcile sleep and the
	// default per-call retry budget handed to idempotent passthrough calls.
	RetryBudgetMs int
	// Stderr receives the truncation notes the honesty rule emits. The frontend
	// decides whether to mirror them as structured output; ops only writes the
	// human note here. nil = io.Discard.
	Stderr io.Writer
	// Journal is the operations-ledger seam: every Operation.Run begins an
	// operation here (which makes the persist decision once, pre-send) and threads
	// the returned handle through ctx so the recording doer behind Raw groups the
	// operation's api_calls under it, and the place protocol records its order
	// intent through it. nil disables journaling (a no-op handle is used).
	Journal OpJournal
	// Log is the optional operational logger for the operation-LEVEL decisions the
	// wire layer below cannot see. NewAPI seeds it from the client's own logger
	// (one wiring per surface); set it afterwards only to give ops a distinct
	// logger. It covers: the place reconcile protocol's send/resend/
	// duplicate/lookup trail and final verdict, and the history/candles paging
	// summary. nil = silent (resolved once via logging.Or). Decisions and outcomes
	// (including the final verdict) log at Debug and per-iteration detail at Trace;
	// nothing logs at Warn here, because every money-unsafe verdict
	// (UNKNOWN/placed-but-unreadable/duplicate-placed) ALREADY reaches the user as
	// program output (the returned error + the guidance note), so re-emitting it on
	// the operational log would only duplicate that on the shared stderr sink — the
	// Debug line is the structured copy for the combined --log-file trail. It NEVER
	// logs a secret: only non-sensitive request facts (clientOrderId, symbol, side)
	// and outcomes (price/qty ride the journal, not the log). A single-call
	// passthrough is left to the wire layer's own log, so a routine read adds no
	// ops line.
	Log *slog.Logger
}

// NewAPI builds the L2 operations layer over a wire client. It is the single
// production construction site: the typed endpoint layer (Raw) and the
// clock/time/scheduling seams (Resync, ServerNow, Sleep) and the operational
// logger (Log) are all derived from the one *apiclient.Client the Operations call
// through, so they cannot drift from the client that actually signs and sends —
// the same resync primitive, the same clock estimate, the same sleep and logger.
// The caller then sets only the ops-level policy that the client does not own:
// RetryBudgetMs (the reconcile/idempotent-retry budget), Journal (the
// operations-ledger seam), and Stderr (the honesty-note sink).
//
// Tests build *API directly to inject scripted seams over a rawapi.Doer fake;
// NewAPI is the production path that ties the seams to a real client.
func NewAPI(client *apiclient.Client) *API {
	return &API{
		Raw:       rawapi.New(client, client.Log),
		Resync:    client.Resync,
		ServerNow: client.ServerNowMs,
		Sleep:     client.Sleep,
		Log:       client.Log,
	}
}

// log returns the operations logger, or a no-op when unwired, so call sites log
// unconditionally (the handler gates by level).
func (a *API) log() *slog.Logger { return logging.Or(a.Log) }

// trace logs at LevelTrace (the per-iteration firehose tier, below Debug) — the
// in-package shim over logging.Trace, since slog.Logger has no Trace method.
func trace(l *slog.Logger, msg string, args ...any) { logging.Trace(l, msg, args...) }

// Result is the outcome of an Invoke or a list operation. Data is the response
// document (for the place protocol, the full fetched order). Truncated/Note are
// the honesty-rule signals carried OUT-OF-BAND so a frontend can render them in
// its own idiom (array properties + stderr for the bot, a different shape for a
// CLI). Attempts echoes how many sends the operation took, for callers that
// surface it.
type Result struct {
	Data      json.RawMessage
	Truncated bool
	Note      string
	Attempts  int
	// JournalErr is an OUT-OF-BAND operations-ledger write failure (the Finish
	// folding the operation outcome into its ledger row failed). It is never the
	// operation's own success/failure — a frontend surfaces it the way it surfaces
	// the other journal errors (the cli joins it and fails after the result; the
	// warn-policy surfaces have already warned and leave it nil).
	JournalErr error
}

// sleep waits d, returning early with ctx.Err() if ctx is canceled first — so a
// reconcile backoff or lookup-retry wait aborts promptly on shutdown rather than
// blocking the worker drain. The injected Sleep (tests) still performs the delay
// on its own goroutine; on cancellation we stop waiting on it (it returns by
// itself, bounded by the backoff ladder). A nil Sleep uses time.Sleep.
func (a *API) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sleepFn := a.Sleep
	if sleepFn == nil {
		sleepFn = time.Sleep
	}
	done := make(chan struct{})
	go func() {
		sleepFn(d)
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *API) stderr() io.Writer {
	if a.Stderr != nil {
		return a.Stderr
	}
	return io.Discard
}

func (a *API) serverNowMs() int64 {
	if a.ServerNow != nil {
		return a.ServerNow()
	}
	return time.Now().UnixMilli()
}
