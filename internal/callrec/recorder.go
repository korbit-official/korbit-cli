// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package callrec

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/version"
)

// Recorder is the journal-backed apiclient.Recorder. It enacts a PolicyFunc over a
// lazily-opened journal.Logger: the DB file is created only when a call actually
// records (at Ready time), so pure public use never touches a read-only home.
// One Recorder is opened at most once and its handle is reused for the api_calls
// write and the order-row seam (StartOrder/FinishOrder); Close releases it.
//
// Safe for concurrent use: open is guarded so the lazy open happens once even if
// several Do calls race (the monitor surface drives this concurrently).
type Recorder struct {
	path     string
	disabled bool       // DIGITALX_CLI_NO_JOURNAL — short-circuits with no open
	noFsync  bool       // --no-fsync/DIGITALX_CLI_NO_FSYNC — opens the journal with synchronous=OFF
	policy   PolicyFunc // the single source of journaling policy

	// onPostFailure is the sink for every POST-call journal-write failure — the
	// api_calls row (per the call's FailMode), the order-outcome row, and the
	// operations-ledger row (both always Warn). The Recorder NEVER returns such a
	// failure to the wire client; it hands (mode, error) here and each surface's
	// closure reacts: Warn logs/toasts it, Fail captures it for the cli to surface
	// fatally after the API result. A pre-send open failure is fatal regardless
	// and is returned directly (Ready/Begin), never through this sink.
	onPostFailure func(FailMode, error)

	// Log receives operational diagnostics about journaling decisions and the
	// lazy open (the Debug "journaled / skipped" lines, the lazy-open path passed
	// to journal.Open). nil = silent (logging.Or). The user-facing post-failure
	// report goes through onPostFailure — this is the telemetry around it.
	Log *slog.Logger

	// clock is the caller's (system) wall clock for EVERY journal time column —
	// the operations, orders, and api_calls rows alike. apiclient.Do brackets a call
	// with the real, un-injectable wall clock; the journal instead stamps all its
	// own times from this one injectable clock so every row stays deterministic
	// under a test clock: api_calls started at Ready (pre-send) / finished at
	// Record (post-call), and the operation/order rows at Begin/StartOrder/Finish.
	// nil = time.Now().UnixMilli().
	clock func() int64

	mu     sync.Mutex
	jl     *journal.Logger // nil until the first recording call opens it
	closed bool            // terminal: set by Close; ensureOpen never reopens after it
}

// New builds a Recorder writing to the journal at path. disabled mirrors
// journal.Disabled (DIGITALX_CLI_NO_JOURNAL): when true every Decision is
// short-circuited to "don't record" and the DB is never opened. noFsync mirrors
// the --no-fsync/DIGITALX_CLI_NO_FSYNC opt-in: the journal is opened with
// synchronous=OFF (faster writes, weaker crash-durability). policy is the
// (swappable) journaling policy; clock is the caller's injectable (system) wall
// clock for every journal time column (nil = time.Now().UnixMilli()); onPostFailure
// is the post-call write-failure sink (a no-op sink is used if nil).
func New(path string, disabled, noFsync bool, policy PolicyFunc, clock func() int64, onPostFailure func(FailMode, error)) *Recorder {
	if onPostFailure == nil {
		onPostFailure = func(FailMode, error) {}
	}
	if clock == nil {
		clock = func() int64 { return time.Now().UnixMilli() }
	}
	return &Recorder{path: path, disabled: disabled, noFsync: noFsync, policy: policy, clock: clock, onPostFailure: onPostFailure}
}

// log returns the recorder's operational logger, never nil (logging.Or).
func (r *Recorder) log() *slog.Logger { return logging.Or(r.Log) }

// decide computes the journaling Decision for a call. DIGITALX_CLI_NO_JOURNAL
// short-circuits to "don't record" before the policy is consulted, so opting out
// never opens the DB. It is a pure function of info (no stashing between Ready
// and Record), which keeps the Recorder concurrency-safe.
func (r *Recorder) decide(info apiclient.CallInfo) Decision {
	if r.disabled {
		return Decision{Record: false, Reason: "NO_JOURNAL opt-out"}
	}
	return r.policy(info)
}

// logDecision emits a Debug line explaining whether a call is journaled and why
// — the routine "did/didn't journal, here's the reason" diagnostic a --debug run
// needs to explain a missing journal row. It logs only pre-signing facts
// (surface/auth/method/path), never params or secrets.
func (r *Recorder) logDecision(info apiclient.CallInfo, d Decision) {
	r.log().Debug("journaling decision",
		"record", d.Record, "reason", d.Reason,
		"surface", info.Origin.Surface, "auth", info.Auth,
		"method", info.Method, "path", info.Path)
}

// ForCall returns a per-call apiclient.Recorder bound to this Recorder's journal,
// policy, clock, and warn sink, carrying ONE call's own state — the
// spec/insertion-ordered params JSON for the api_calls row (orderedParams; "" to
// keep the client's CallInfo.ParamsJSON) and the start timestamp it captures at
// Ready. A fresh CallRecorder per apiclient.Do keeps every call's state isolated, so
// concurrent calls — including several of the SAME command through one shared
// parent Recorder (the monitor surface) — never collide. Wire it as the Client's
// Rec for exactly one Do.
func (r *Recorder) ForCall(orderedParams string) *CallRecorder {
	return &CallRecorder{parent: r, orderedParams: orderedParams}
}

// CallRecorder is the apiclient.Recorder for ONE logical call. It holds only that
// call's state (its ordered params, its operation link/sequence, and its
// Ready-captured start), delegating the shared journal/policy/clock to its
// parent — so it is the per-call isolation seam that makes concurrent recording
// correct. An ad-hoc CallRecorder (from Recorder.ForCall) carries operationID 0
// and seq 0; one minted by an OpHandle.ForCall carries the operation's id and a
// 1-based sequence within it.
type CallRecorder struct {
	parent        *Recorder
	orderedParams string // spec/insertion-ordered params_json override ("" = use CallInfo)
	operationID   int64  // 0 => ad-hoc (NULL operation link)
	seq           int    // 1-based within the operation; 0 for ad-hoc
	startedMs     int64  // captured at Ready (on the injectable clock)
	started       bool
}

// Ready is the L1 client's pre-send gate. When the policy says this call records,
// it opens the journal (creating the DB if needed) — so a broken journal fails
// the command here, BEFORE anything is sent (the hard guarantee). When the call
// is not recorded it opens nothing and permits the send. It also captures the
// start timestamp on the injectable clock for the api_calls started_at_ms column.
func (c *CallRecorder) Ready(info apiclient.CallInfo) error {
	d := c.parent.decide(info)
	c.parent.logDecision(info, d)
	if !d.Record {
		return nil
	}
	c.startedMs = c.parent.clock()
	c.started = true
	_, err := c.parent.ensureOpen()
	return err
}

// Record is the post-call write. When the call is not recorded it is a no-op.
// Otherwise it writes the api_calls row with the same fields runEndpoint wrote. A
// write failure is delivered to the onPostFailure sink with the call's FailMode
// (the caller's closure warns or captures-for-fatal) — never returned, so a
// journal fault can never be conflated with the API error.
func (c *CallRecorder) Record(info apiclient.CallInfo, out apiclient.Outcome) int64 {
	d := c.parent.decide(info)
	if !d.Record {
		return 0
	}
	jl, err := c.parent.ensureOpen()
	if err != nil {
		c.parent.postFailure(d, err)
		return 0
	}
	if jl == nil {
		return 0 // recorder closed; drop the post-send row (never reopen)
	}
	rec := callRecordFrom(info, out)
	rec.OperationID = c.operationID
	rec.Seq = c.seq
	if c.orderedParams != "" {
		// Preserve the spec/insertion order for params_json
		// (the client's CallInfo.ParamsJSON is map-sorted).
		rec.ParamsJSON = c.orderedParams
	}
	// Stamp the timing on the injectable clock: started captured at Ready, finished
	// now. The client's Outcome times come from the un-injectable real wall clock,
	// so we don't use them for these columns. The fallback (Record without a prior
	// Ready) can't happen on the apiclient.Do path — Do calls Ready before Record —
	// but keeps a synthetic-but-consistent (duration 0) row if a future caller ever
	// bypasses it.
	started := c.startedMs
	if !c.started {
		started = c.parent.clock()
	}
	finished := c.parent.clock()
	rec.StartedAtMs = started
	rec.FinishedAtMs = finished
	rec.DurationMs = finished - started
	id, err := jl.LogCall(rec)
	if err != nil {
		c.parent.postFailure(d, err)
	}
	return id
}

// postFailure hands a post-call journal error to the onPostFailure sink with the
// Decision's FailMode; the surface's closure decides what to do (Warn logs/toasts
// it, Fail captures it for the cli to surface fatally after the API result). The
// error is the plain journal fact — formatting and program-name/severity framing
// are the sink's presentation concern.
func (r *Recorder) postFailure(d Decision, err error) {
	r.onPostFailure(d.PostFailure, err)
}

// StartOrder records the order's mint-time intent row BEFORE the placement send
// (the pre-send hard guarantee), opening the journal if needed. Its failure is
// fatal — nothing has been sent yet — and is reported with output.Configf so
// the error and its exit-4 classification are preserved.
// Returns the order row id for a later FinishOrder. DIGITALX_CLI_NO_JOURNAL
// (disabled) short-circuits to (0, nil) without opening the DB; the matching
// FinishOrder then no-ops, so opting out skips the order rows exactly as it
// skips api_calls.
func (r *Recorder) StartOrder(o journal.OrderStart) (int64, error) {
	if r.disabled {
		return 0, nil
	}
	jl, err := r.ensureOpen()
	if err != nil {
		// ensureOpen already wrapped an open failure as a ConfigError with the
		// journal-open text; surface it directly.
		return 0, err
	}
	if jl == nil {
		// Recorder closed: this is a pre-send hard-guarantee write, so refuse
		// rather than silently skipping the row and letting the caller send.
		return 0, output.Configf("cannot record the order to the action journal at %s: journal closed — set DIGITALX_CLI_NO_JOURNAL=1 to disable journaling", r.path)
	}
	id, err := jl.StartOrder(o)
	if err != nil {
		return 0, output.Configf("cannot record the order to the action journal at %s: %v — set DIGITALX_CLI_NO_JOURNAL=1 to disable journaling", r.path, err)
	}
	return id, nil
}

// FinishOrder folds the placement result into the order row. It is a pass-through
// to journal.FinishOrder; the journal is already open (StartOrder opened it).
func (r *Recorder) FinishOrder(id int64, f journal.OrderFinish) error {
	r.mu.Lock()
	jl := r.jl
	r.mu.Unlock()
	if jl == nil {
		return nil
	}
	return jl.FinishOrder(id, f)
}

// Path returns the journal database path (for messages and the debug note). It
// is the configured path whether or not the DB has been opened yet.
func (r *Recorder) Path() string { return r.path }

// Opened reports whether the journal has been opened (a call recorded). Used by
// the CLI to emit the "recorded to action journal" debug note only when there
// was actually a write.
func (r *Recorder) Opened() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jl != nil
}

// Close releases the journal handle if it was opened and marks the recorder
// closed for good: a late call arriving after Close (a leaked TUI command
// goroutine) finds ensureOpen a no-op and never reopens the journal.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.jl == nil {
		return nil
	}
	err := r.jl.Close()
	r.jl = nil
	return err
}

// ensureOpen opens the journal exactly once and returns the live handle under
// the lock, so an open-then-write caller gets a non-nil pointer that cannot have
// raced Close()'s jl = nil between the open and the write. After Close it is a
// terminal no-op — returns (nil, nil) so a late caller cleanly skips its write
// instead of reopening the journal (the open-then-write callers nil-check the
// returned handle). An open failure returns the output.Configf "cannot open the
// action journal" message with the DIGITALX_CLI_NO_JOURNAL hint, preserving the
// hard-guarantee error text.
func (r *Recorder) ensureOpen() (*journal.Logger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil
	}
	if r.jl != nil {
		return r.jl, nil
	}
	r.log().Debug("lazily opening journal", "path", r.path)
	jl, err := journal.Open(r.path, r.noFsync, r.Log)
	if err != nil {
		return nil, output.Configf("cannot open the action journal at %s: %v — fix the path/permissions or set DIGITALX_CLI_NO_JOURNAL=1 to disable journaling", r.path, err)
	}
	r.jl = jl
	return jl, nil
}

// lockedJL reads the journal handle under mu, returning nil if Close has already
// run. Post-send writers (the order/operation finish paths and the per-call
// recorders, whose journal was opened earlier by Ready/Begin) read through it so
// the handle read cannot race Close()'s jl = nil. Reading under the lock still
// cannot stop Close from closing the handle mid-write — that degrades to a
// journal error handled as a post-send Warn — but it removes the pointer data
// race and the nil-pointer panic.
func (r *Recorder) lockedJL() *journal.Logger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jl
}

// callRecordFrom builds the api_calls row from the call info and outcome.
// The timing columns (started/finished/duration) are NOT set here — Record
// stamps them from the injectable clock (see Record). http_status is set ONLY
// for an API rejection (an ApiError, where out.Code is non-empty) — it stays
// NULL on success and on a pre-response transport failure (the Outcome's
// synthetic 200 on success is deliberately not stored).
func callRecordFrom(info apiclient.CallInfo, out apiclient.Outcome) journal.CallRecord {
	retryCount := out.Attempts - 1
	if retryCount < 0 {
		retryCount = 0
	}
	rec := journal.CallRecord{
		Method:     info.Method,
		Path:       info.Path,
		BaseURL:    info.BaseURL,
		Auth:       info.Auth,
		KeyName:    info.KeyName,
		APIKeyID:   info.APIKeyID,
		ParamsJSON: string(info.ParamsJSON),
		Success:    out.Err == nil,
		RetryCount: retryCount,
		CLIVersion: version.Version,
	}
	if out.Err != nil {
		if isAPIError(out) {
			status := out.HTTPStatus
			rec.HTTPStatus = &status
			rec.ErrorCode = out.Code
			rec.ErrorMessage = out.Message
		} else {
			// A pre-response transport failure: no status, just the message.
			rec.ErrorMessage = out.Message
		}
	}
	return rec
}

// isAPIError reports whether the outcome describes an API rejection (vs a
// pre-response transport failure). apiclient.buildOutcome fills Code only from an
// *output.ApiError, so a non-empty Code is the discriminator — equivalent to
// errors.As(err, *ApiError), but without re-inspecting the error.
func isAPIError(out apiclient.Outcome) bool {
	if out.Code != "" {
		return true
	}
	// Defensive: an ApiError with an empty symbolic code still carries a status.
	var ae *output.ApiError
	return errors.As(out.Err, &ae)
}
