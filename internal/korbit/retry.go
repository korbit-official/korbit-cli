// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/korbit-official/korbit-cli/internal/output"
)

// RetryPolicy configures ExecuteWithRetry. The zero value (Idempotent false /
// BudgetMs 0) is a single shot — identical to calling Execute directly — which
// is the deliberate fail-safe default for any request that must not be resent.
type RetryPolicy struct {
	// Idempotent is the master gate. When false, ExecuteWithRetry sends exactly
	// once and surfaces the first error, no matter its kind — so money-moving
	// writes and order placement are never auto-resent on an ambiguous failure.
	// Only GET endpoints and explicitly-idempotent writes (cancel, deposit
	// address generate) set this true.
	Idempotent bool
	// BudgetMs bounds the total time the layer will spend SLEEPING between
	// retries (the sum of backoff/Retry-After waits). Each attempt additionally
	// keeps its own opts.TimeoutMs; the corrective EXCEED_TIME_WINDOW retry does
	// not sleep and so does not draw down the budget. 0 disables retry entirely
	// (single shot) even when Idempotent. Tracking scheduled sleep rather than a
	// wall clock keeps the layer deterministic and testable without a fake clock.
	BudgetMs int
	// Correct re-measures the clock for the one corrective retry after an
	// EXCEED_TIME_WINDOW rejection. nil means no correction is available, in
	// which case EXCEED_TIME_WINDOW is treated as a normal (non-corrective)
	// failure and not retried. Called at most once per ExecuteWithRetry.
	Correct func() (Correction, error)
	// Sleep is the delay between retries; injectable for tests. Defaults to
	// time.Sleep.
	Sleep func(time.Duration)
	// Attempts, when non-nil, is incremented once per underlying Execute call
	// (including the single-shot path and the first attempt), so the caller can
	// record how many sends a request actually took. retries = *Attempts - 1.
	Attempts *int
	// Observe, when non-nil, is called with a short human description each time a
	// failed attempt leads to a retry decision (a retry, or giving up because the
	// budget is exhausted) — for --debug diagnostics. Not called on success or a
	// non-retryable (fatal) failure, which the caller surfaces directly.
	Observe func(string)
}

const (
	retryInitialBackoffMs = 100
	retryMaxBackoffMs     = 2000
)

// Correction is the clock adjustment to install on a corrective
// EXCEED_TIME_WINDOW retry. Now, when non-nil, REPLACES opts.Now with a
// corrected clock (the server-clock estimate, already leaned into the past by
// the measurement uncertainty) — it replaces rather than composes, so a request
// whose clock was already corrected proactively by --time-sync on is not
// double-adjusted. RecvWindowMs, when > 0, widens the signed-request validity
// window so the leaned timestamp still lands inside it on a slow/asymmetric link.
type Correction struct {
	Now          func() int64
	RecvWindowMs int
}

// RetryClass is the four-way classification of an Execute error, exported so
// the L2 ops layer (order-place reconcile, history walking) can reason about
// whether a failed call can have had side effects.
//
// The classes split into two groups by side-effect provability:
//
//   - the PRE-EXECUTION group — {ClassTimeWindow, ClassRateLimited} — the
//     server provably rejected the request at a gate (its clock window, or rate
//     limiter) BEFORE any side effect. Resending is always safe, even for a
//     money mover. This is the group RetryPreExec retries.
//   - the AMBIGUOUS group — {ClassTransient} — a network/transport failure or
//     an HTTP 5xx: the request MAY have executed (the response was lost). Only
//     an idempotent request may be blindly resent; otherwise the caller must
//     reconcile (the L2 reconcile protocol consumes exactly this distinction).
//     The TRY_AGAIN cancel "retry shortly" condition is also routed to
//     ClassTransient — not because it is ambiguous (it is side-effect-free) but
//     because it wants the same idempotent-only backoff retry.
//   - ClassFatal — any other API rejection (4xx other than 429, success:false):
//     resending cannot help, so it is surfaced immediately.
//
// Use IsPreExecution / IsAmbiguous to query the grouping by name.
type RetryClass int

const (
	// ClassFatal is a non-retryable failure: surface it.
	ClassFatal RetryClass = iota
	// ClassTimeWindow is EXCEED_TIME_WINDOW — pre-execution; resync the clock
	// and retry.
	ClassTimeWindow
	// ClassRateLimited is HTTP 429 — pre-execution; honor Retry-After else back
	// off.
	ClassRateLimited
	// ClassTransient is retried with exponential backoff, and only for an
	// idempotent request. Its members: a network/transport error or HTTP 5xx —
	// AMBIGUOUS (may have executed), which is exactly why the idempotency gate
	// matters — and the TRY_AGAIN "order is being processed, retry shortly"
	// cancel condition, which is side-effect-free and cancel-only (so it always
	// satisfies the gate) but shares the same backoff-retry mechanics.
	ClassTransient
)

// IsPreExecution reports whether the class is in the pre-execution group: the
// server provably rejected the request before any side effect, so resending is
// safe even for a non-idempotent (money-moving) request.
func (c RetryClass) IsPreExecution() bool {
	return c == ClassTimeWindow || c == ClassRateLimited
}

// IsAmbiguous reports whether the class is the ambiguous group: the request may
// have executed, so only an idempotent request may be blindly resent.
func (c RetryClass) IsAmbiguous() bool { return c == ClassTransient }

// String renders the class as a short stable token, for an operational log
// attribute or the Observe seam.
func (c RetryClass) String() string {
	switch c {
	case ClassTimeWindow:
		return "timeWindow"
	case ClassRateLimited:
		return "rateLimited"
	case ClassTransient:
		return "transient"
	default:
		return "fatal"
	}
}

// Classify maps an Execute error to its RetryClass. A non-ApiError is a
// transport/network failure (Execute wraps those), which is ambiguous;
// ApiErrors are classified by symbolic code and HTTP status.
func Classify(err error) RetryClass {
	var ae *output.ApiError
	if !errors.As(err, &ae) {
		return ClassTransient
	}
	switch {
	case ae.Code == "EXCEED_TIME_WINDOW":
		return ClassTimeWindow
	case ae.Code == "TRY_AGAIN":
		// TRY_AGAIN is the order-cancel "order is currently being processed, try
		// again in a few moments" condition (the only endpoint that returns it is
		// the idempotent order cancel). It is surfaced only by the idempotent
		// cancel, so gating the retry on idempotency (ClassTransient) is correct.
		return ClassTransient
	case ae.HTTPStatus == 429:
		return ClassRateLimited
	case ae.HTTPStatus >= 500 && ae.HTTPStatus <= 599:
		return ClassTransient
	default:
		return ClassFatal
	}
}

// RetryAfterMs returns the Retry-After value in ms if the error carried one
// (an HTTP 429 with the header), else 0. Exported for the L2 layer's own
// rate-limit handling.
func RetryAfterMs(err error) int64 {
	var ae *output.ApiError
	if errors.As(err, &ae) && ae.RetryAfterSec != nil {
		return int64(*ae.RetryAfterSec) * 1000
	}
	return 0
}

// InitialBackoffMs and MaxBackoffMs are the exponential-backoff ladder bounds
// (100ms doubling to a 2000ms ceiling) for the ambiguous/rate-limited classes,
// exported so the L2 layer can run the same ladder. NextBackoffMs doubles a
// backoff, clamped at MaxBackoffMs.
const (
	InitialBackoffMs = retryInitialBackoffMs
	MaxBackoffMs     = retryMaxBackoffMs
)

// NextBackoffMs returns the next backoff step (doubling, clamped at
// MaxBackoffMs).
func NextBackoffMs(b int64) int64 { return nextBackoff(b) }

// retryFreeIterations is the slack in MaxRetryIterations beyond the
// budget-derived term: it covers a loop's first (un-slept) attempt plus the
// finitely-many no-sleep corrective retries (the single EXCEED_TIME_WINDOW
// resync each loop allows). A handful is ample — the ceiling is a structural
// backstop, not a tuned limit.
const retryFreeIterations = 8

// MaxRetryIterations is the hard iteration ceiling for a budgeted retry loop —
// the structural backstop that guarantees such a loop cannot spin forever,
// independent of its per-class decision logic. Every *retrying* iteration
// sleeps at least retryInitialBackoffMs and the total sleep is bounded by
// budgetMs, so at most budgetMs/retryInitialBackoffMs iterations can sleep;
// retryFreeIterations covers the first attempt and the bounded no-sleep
// corrective retries. The ceiling is therefore UNREACHABLE while the budget and
// once-flag bounds hold — it exists so that if a future edit introduces a retry
// path that neither sleeps nor terminates, the loop fails safe (surfaces its
// last error) instead of looping. Deriving the term from the budget keeps it
// correct for any --retry-timeout: a larger budget legitimately permits more
// sleeping iterations, so the cap scales with it and never trips in normal use.
// Both budgeted retry loops in the tree — retryEngine.run here and the L2
// order-place reconcile loop — bound themselves with this one function (see the
// "Loop-safety invariants" section of this package's doc.go).
func MaxRetryIterations(budgetMs int64) int {
	if budgetMs < 0 {
		budgetMs = 0
	}
	return retryFreeIterations + int(budgetMs/retryInitialBackoffMs)
}

// ExecuteWithRetry runs Execute with the bounded, idempotency-gated retry policy
// described by pol. The retry taxonomy (only for Idempotent requests within
// BudgetMs):
//
//   - EXCEED_TIME_WINDOW — the server rejected the request at its clock gate
//     BEFORE any side effect, so it is always safe to resend. If pol.Correct is
//     set, ExecuteWithRetry measures the offset once, installs a corrected clock,
//     and retries immediately (no backoff). This is the auto-correct path.
//   - HTTP 429 — honors error.retryAfterSec when present, else backs off.
//   - network/transport error and HTTP 5xx — exponential backoff. The cancel
//     TRY_AGAIN "order is mid-processing, retry shortly" condition shares this
//     backoff path (it is a definitive 4xx routed to ClassTransient for its
//     mechanics; see Classify).
//   - any other 4xx / API rejection — surfaced immediately.
//
// A non-idempotent request (the default) is sent exactly once.
func ExecuteWithRetry(req Request, opts Options, pol RetryPolicy) (json.RawMessage, error) {
	if !pol.Idempotent || pol.BudgetMs <= 0 {
		if pol.Attempts != nil {
			*pol.Attempts++
		}
		return Execute(req, opts)
	}

	eng := retryEngine{
		idempotent: true,
		retryPre:   true, // an idempotent request retries the pre-exec classes too
		budgetMs:   int64(pol.BudgetMs),
		correct:    pol.Correct,
		sleep:      pol.Sleep,
		observe:    pol.Observe,
		attempts:   pol.Attempts,
	}
	return eng.run(func(o Options) (json.RawMessage, error) { return Execute(req, o) }, opts)
}

// RetryGovernor is the ONE retry ladder shared by the budgeted retry loops in
// the tree — the L1 retryEngine (here) and the L2 order-place reconcile loop
// (placeOp.runReconcile). It encapsulates the whole ladder policy: the four-class
// taxonomy's mechanics, the exponential backoff schedule, the HTTP 429
// Retry-After honor, the single no-sleep EXCEED_TIME_WINDOW corrective retry,
// the total sleep budget, and the hard iteration ceiling (the structural
// backstop). It performs NO I/O — it never sleeps, resyncs, or sends. The caller
// drives its own loop and, per failed attempt, asks Next what the ladder permits,
// then carries out the returned action with its own sleep/resync mechanism. This
// keeps the policy in one place while each caller keeps its own control-flow
// shell: the engine drives a send func and surfaces errors directly, whereas the
// reconcile loop interleaves DUPLICATE-resolution and the eventual-consistency
// endgame, mapping each give-up class to its own clean-fail vs UNKNOWN semantics.
//
// The two policy axes select which classes are retried:
//
//   - idempotent: when true, the ambiguous class (network/5xx) is retried with
//     backoff (the idempotency gate).
//   - retryPre:   when true, the pre-execution classes (EXCEED_TIME_WINDOW via
//     one corrective resync, HTTP 429 via Retry-After/backoff) are retried even
//     when !idempotent.
//
// When neither axis applies to a class that class is surfaced (RetryGiveUp); when
// neither applies at all the loop is effectively a single shot.
//
// The reconnect loop (stream.connManager.run) deliberately does NOT use a
// governor: it is unbounded by design (a resilient stream reconnects forever),
// has no sleep budget, and classifies handshake rejections rather than Classify
// errors, so its safety is a pure rate bound (always back off) — a structurally
// different shape. See the "Loop-safety invariants" section of doc.go.
type RetryGovernor struct {
	idempotent bool
	retryPre   bool
	canResync  bool // a resync hook is available; else EXCEED_TIME_WINDOW is not corrected
	budgetMs   int64

	sleptMs   int64
	backoffMs int64
	resynced  bool
	attempt   int
	ceiling   int
}

// NewRetryGovernor builds a governor for one logical call. canResync reports
// whether the caller has a clock-resync hook (when false, EXCEED_TIME_WINDOW is
// surfaced rather than corrected). budgetMs bounds total inter-retry sleep; it
// also sets the hard iteration ceiling via MaxRetryIterations.
func NewRetryGovernor(idempotent, retryPre, canResync bool, budgetMs int64) *RetryGovernor {
	return &RetryGovernor{
		idempotent: idempotent,
		retryPre:   retryPre,
		canResync:  canResync,
		budgetMs:   budgetMs,
		backoffMs:  retryInitialBackoffMs,
		ceiling:    MaxRetryIterations(budgetMs),
	}
}

// Attempt reports whether another attempt may run, enforcing the hard iteration
// ceiling. Drive the loop with it as the condition: `for g.Attempt() { ... }`.
// It returns false once the ceiling is reached — unreachable while the budget
// and resync-once bounds hold, so the caller must treat a false here as the
// fail-safe backstop (surface the last error / route to its unknown endgame).
func (g *RetryGovernor) Attempt() bool {
	g.attempt++
	return g.attempt <= g.ceiling
}

// Attempts is the number of attempts begun so far (equals the caller's send
// count; handy for a journal / Result.Attempts).
func (g *RetryGovernor) Attempts() int { return g.attempt }

// RetryAction is the mechanical decision Next returns.
type RetryAction int

const (
	// RetryGiveUp: surface the error — not retryable under this policy, the
	// budget is exhausted, or no further clock correction is available.
	RetryGiveUp RetryAction = iota
	// RetryResync: perform the ONE clock resync (no sleep, does not draw down the
	// budget), then retry. Returned at most once per governor, only for
	// EXCEED_TIME_WINDOW when a resync hook is available.
	RetryResync
	// RetryWait: sleep Wait, then retry.
	RetryWait
)

// String renders the action as a short stable token, for an operational log
// attribute.
func (a RetryAction) String() string {
	switch a {
	case RetryResync:
		return "resync"
	case RetryWait:
		return "wait"
	default:
		return "giveUp"
	}
}

// Decision is what the ladder permits for one failed attempt. Wait is set only
// for RetryWait. Reason is a short human description for --debug Observe, and is
// "" when the caller supplies its own message (RetryResync) or the class is
// surfaced silently (a policy-gated or fatal give-up).
type Decision struct {
	Action RetryAction
	Wait   time.Duration
	Reason string
}

// Next decides what the ladder permits for a failed attempt of the given class.
// retryAfterMs is the request's Retry-After in ms (0 if none), honored only for
// ClassRateLimited. It updates the governor's budget / backoff / resync-once
// state as a side effect of the decision it returns, and performs no I/O.
func (g *RetryGovernor) Next(class RetryClass, retryAfterMs int64) Decision {
	switch class {
	case ClassTimeWindow:
		if !g.idempotent && !g.retryPre {
			return Decision{Action: RetryGiveUp} // not retried at all under this policy
		}
		if g.resynced || !g.canResync {
			return Decision{Action: RetryGiveUp, Reason: "EXCEED_TIME_WINDOW: no further clock correction available; surfacing"}
		}
		g.resynced = true
		return Decision{Action: RetryResync} // the caller observes around its own resync
	case ClassRateLimited:
		if !g.idempotent && !g.retryPre {
			return Decision{Action: RetryGiveUp}
		}
		wait := g.backoffMs
		if retryAfterMs > 0 {
			wait = retryAfterMs
		}
		if g.sleptMs+wait > g.budgetMs {
			return Decision{Action: RetryGiveUp, Reason: fmt.Sprintf("HTTP 429: %dms wait would exceed the retry budget; surfacing", wait)}
		}
		g.sleptMs += wait
		g.backoffMs = nextBackoff(g.backoffMs)
		return Decision{Action: RetryWait, Wait: time.Duration(wait) * time.Millisecond, Reason: fmt.Sprintf("HTTP 429: waiting %dms then retrying", wait)}
	case ClassTransient:
		if !g.idempotent {
			return Decision{Action: RetryGiveUp} // ambiguous: only an idempotent request may be blindly resent
		}
		if g.sleptMs+g.backoffMs > g.budgetMs {
			return Decision{Action: RetryGiveUp, Reason: fmt.Sprintf("retryable failure: %dms backoff would exceed the retry budget; surfacing", g.backoffMs)}
		}
		wait := g.backoffMs
		g.sleptMs += wait
		g.backoffMs = nextBackoff(g.backoffMs)
		return Decision{Action: RetryWait, Wait: time.Duration(wait) * time.Millisecond, Reason: fmt.Sprintf("retryable failure: backing off %dms then retrying", wait)}
	default: // ClassFatal
		return Decision{Action: RetryGiveUp}
	}
}

// retryEngine is the transport-aware loop driver behind both ExecuteWithRetry
// and Client.Do. It owns no policy — it delegates every retry decision to a
// RetryGovernor — and no transport: the caller passes a send func that performs
// one attempt with the (possibly clock-corrected) Options. retryEngine's job is
// the I/O the governor refuses to do: run the send, perform the corrective
// resync via correct, and sleep (context-aware via onSleep).
type retryEngine struct {
	idempotent bool
	retryPre   bool
	budgetMs   int64
	correct    func() (Correction, error)
	sleep      func(time.Duration)
	observe    func(string)
	attempts   *int
	// onSleep, when non-nil, is consulted before each backoff/Retry-After sleep
	// and may abort it (Client.Do uses this to make the sleep context-aware).
	// It returns the error to surface if the wait must be cut short, else nil to
	// proceed with a normal sleep.
	onSleep func(d time.Duration) error
	// maxIters, when > 0, overrides the governor's derived iteration ceiling.
	// It exists ONLY so a white-box test can drive the loop to the ceiling and
	// verify the fail-safe exit (the ceiling is otherwise unreachable, since the
	// budget and resync-once bounds always terminate the loop first). No
	// production caller sets it; 0 means "derive from the budget".
	maxIters int
}

func (e *retryEngine) run(send func(Options) (json.RawMessage, error), opts Options) (json.RawMessage, error) {
	sleepFn := e.sleep
	if sleepFn == nil {
		sleepFn = time.Sleep
	}
	observe := func(msg string) {
		if e.observe != nil && msg != "" {
			e.observe(msg)
		}
	}

	// All retry policy — backoff schedule, budget, the single EXCEED_TIME_WINDOW
	// resync, and the hard iteration ceiling (the structural backstop that makes
	// this loop unable to spin) — lives in the governor. retryEngine only does
	// the I/O: send, the corrective resync via e.correct, and the context-aware
	// sleep. The ceiling is unreachable while the budget and resync-once bounds
	// hold; if Attempt ever returns false we surface the last error rather than
	// loop.
	gov := NewRetryGovernor(e.idempotent, e.retryPre, e.correct != nil, e.budgetMs)
	if e.maxIters > 0 {
		gov.ceiling = e.maxIters // test-only override; see retryEngine.maxIters
	}
	var lastErr error
	for gov.Attempt() {
		if e.attempts != nil {
			*e.attempts++
		}
		data, err := send(opts)
		if err == nil {
			return data, nil
		}
		lastErr = err

		dec := gov.Next(Classify(err), RetryAfterMs(err))
		switch dec.Action {
		case RetryResync:
			c, cerr := e.correct()
			if cerr != nil {
				observe("EXCEED_TIME_WINDOW: clock re-measurement failed; surfacing")
				return nil, err // can't measure — surface the original rejection
			}
			if c.Now != nil {
				opts.Now = c.Now
			}
			if c.RecvWindowMs > 0 {
				opts.RecvWindow = c.RecvWindowMs
			}
			observe("EXCEED_TIME_WINDOW: re-synced clock, retrying immediately")
			// retry immediately, no backoff (does not draw down budget)

		case RetryWait:
			observe(dec.Reason)
			if serr := e.doSleep(sleepFn, dec.Wait); serr != nil {
				return nil, serr
			}

		case RetryGiveUp:
			observe(dec.Reason)
			return nil, err
		}
	}
	// Iteration ceiling reached — unreachable in correct operation. Fail safe by
	// surfacing the last error instead of spinning.
	observe("retry loop reached its hard iteration ceiling; surfacing the last error")
	return nil, lastErr
}

// doSleep performs one inter-retry wait, giving the optional onSleep hook a
// chance to abort it (used by Client.Do to honor context cancellation). When
// onSleep returns an error the wait is cut short and that error is surfaced.
func (e *retryEngine) doSleep(sleepFn func(time.Duration), d time.Duration) error {
	if e.onSleep != nil {
		if err := e.onSleep(d); err != nil {
			return err
		}
		return nil
	}
	sleepFn(d)
	return nil
}

func nextBackoff(b int64) int64 {
	b *= 2
	if b > retryMaxBackoffMs {
		b = retryMaxBackoffMs
	}
	return b
}
