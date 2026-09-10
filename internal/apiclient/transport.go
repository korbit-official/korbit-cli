// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/output"
)

// errNoCreds is returned by Do for a signed call on a public-only client.
var errNoCreds = errors.New("apiclient: a signed call requires credentials, but this client is public-only")

// Clock is the read view of the shared server-clock estimate a Client signs
// against (and that the stream reads for delivery-delay detection). It is an
// interface so package apiclient does not import internal/clock — clock.State
// satisfies it structurally, preserving the no-import-cycle invariant in both
// packages' doc.go. Resync (the measurement that updates the estimate) is NOT on
// this read-only view; it is Client.Resync, so a client can sign against a clock
// without being able to auto-correct it (the doctor surface signs with a raw,
// never-resynced clock to surface skew).
type Clock interface {
	// SignNow is the corrected, lean-adjusted unix-ms timestamp to sign with.
	SignNow() int64
	// RecvWindowMs is the widened signed-request validity window in ms, 0 = omit
	// (server default 5s). One value for every surface (omit-below-5s).
	RecvWindowMs() int
	// Offset is the raw server−local offset in ms (delivery-delay detection).
	Offset() int64
	// Measured reports whether a server-clock estimate has been installed yet
	// (false = the bare local clock, offset 0). The stream layer's delivery-delay
	// check reads it to decide whether a suspected delay against an unmeasured
	// clock warrants a one-off reactive resync — otherwise a skewed local clock
	// masquerades as delivery lag.
	Measured() bool
	// ServerNowMs is the clock's best estimate of the server's current unix-ms
	// time (the local clock plus the raw Offset, UN-leaned — unlike SignNow,
	// which leans into the past for the signing bound). It is the source for
	// defaulting server-relative lookback windows sent on the wire (the history
	// walk's start). It reads the same local-clock seam the estimate was measured
	// against, so it honors a test clock.
	ServerNowMs() int64
}

// Client is the single front door for calling Korbit: the L1 transport that
// signs REST calls and the WebSocket upgrade (SignHandshake), runs exactly the
// retry Policy it is HANDED (the zero Policy is a single shot), owns its clock
// (read via Clock, resync via Resync), and records each call through a per-call
// Recorder it mints itself. It holds NO call policy of its own — callers (L2
// ops, frontends) decide the policy per call. See doc.go for the package-level
// contracts.
//
// A Client is configured once and reused; Do is safe for concurrent use as long
// as the injected Clock/Resync/Sleep/Observe/NewRecorder are. NewRecorder mints
// a fresh per-call Recorder for every Do, so a shared Client has no shared
// mutable recording state.
type Client struct {
	// BaseURL is the API host (e.g. https://api.korbit.co.kr). Required.
	BaseURL string
	// Doer performs the HTTP request; nil uses http.DefaultClient.
	Doer Doer
	// Creds sign private calls; nil = public-only (a Call with Auth true then
	// fails fast).
	Creds *Credentials
	// UserAgent overrides the User-Agent header; "" keeps the default
	// ("korbit-cli/<version>"). Composing it is the caller's job (see Options.UserAgent).
	UserAgent string
	// Clock is the shared server-clock estimate this client signs against; nil =
	// sign with the local wall clock. Wire it to one clock.State (via
	// clock.Syncer.State) so a single estimate covers every surface.
	Clock Clock
	// Resync re-measures the server clock; it is the ONE resync primitive (wire
	// it to clock.Syncer.Sync). On EXCEED_TIME_WINDOW (when the policy retries the
	// pre-execution class) Do calls Resync once, then re-signs reading the now
	// corrected Clock. nil = no auto-correct: EXCEED_TIME_WINDOW is then surfaced,
	// not retried (the public and doctor surfaces leave it nil).
	Resync func() error
	// TimeoutMs is the per-attempt HTTP timeout; 0 = the package default (15s).
	// A Call may override it per call.
	TimeoutMs int
	// Sleep is the inter-retry delay primitive; nil = time.Sleep. (Do also makes
	// the wait context-aware regardless — see Do.)
	Sleep func(time.Duration)
	// Observe receives short retry-decision descriptions for --debug; nil = off.
	Observe func(string)
	// NewRecorder mints a FRESH per-call journal Recorder for one Do (reading the
	// active operation from ctx and the call's params), so concurrent calls never
	// share recorder state. nil — or a nil Recorder returned — means this call is
	// not recorded.
	NewRecorder func(ctx context.Context, call Call) Recorder
	// Origin attributes calls to a frontend; flows into CallInfo.
	Origin Origin
	// KeyName / APIKeyID are recorded into CallInfo (the public key id; never a
	// secret). APIKeyID is informational for the recorder — Creds.APIKeyID is
	// what actually signs.
	KeyName  string
	APIKeyID string
	// Log is the optional operational logger for this client's wire activity
	// (request target/timing/attempt at Debug, a network error about to be
	// retried at Warn). nil = silent. It NEVER receives a secret, signature, or
	// raw response body — only the method, path, host, status, and symbolic code.
	Log *slog.Logger
}

// log returns the client's logger, or a no-op when unwired, so Do can log
// unconditionally (the handler gates by level).
func (c *Client) log() *slog.Logger { return logging.Or(c.Log) }

// signNow is the timestamp to sign with: the shared Clock's estimate, or the
// local wall clock when no Clock is wired.
func (c *Client) signNow() int64 {
	if c.Clock != nil {
		return c.Clock.SignNow()
	}
	return time.Now().UnixMilli()
}

// ServerNowMs is the clock's best estimate of the server's current unix-ms time
// (un-leaned), for defaulting server-relative lookback windows sent on the wire.
// It reads the shared Clock (which honors a test clock seam), falling back to the
// local wall clock when no Clock is wired. L2 ops reads it for the history walk's
// default start.
func (c *Client) ServerNowMs() int64 {
	if c.Clock != nil {
		return c.Clock.ServerNowMs()
	}
	return time.Now().UnixMilli()
}

// recvWindowMs is the validity window to sign with: the shared Clock's
// auto-widened window (omit-below-5s), else 0 (omit — the server applies its 5s
// default). It is read fresh on each sign (initial and the post-resync re-sign),
// so a resync that widens the window is picked up without any separate plumbing.
func (c *Client) recvWindowMs() int {
	if c.Clock != nil {
		return c.Clock.RecvWindowMs()
	}
	return 0
}

// Call is one logical API call, pre-signing. TimeoutMs overrides the client's
// per-attempt timeout for this call (0 = client default).
type Call struct {
	Method    string
	Path      string
	Params    []KV
	Auth      bool
	TimeoutMs int
}

// Policy is the per-call retry policy — INPUT decided by the caller (L2 ops or
// a frontend), never by the Client. The zero value is a single shot (the
// fail-safe default, identical to the RetryPolicy zero value).
type Policy struct {
	// Idempotent gates the FULL retry ladder exactly as RetryPolicy.Idempotent:
	// when true the ambiguous class (network/5xx) is retried with backoff, and
	// the pre-execution classes too.
	Idempotent bool
	// RetryPreExec, when true, retries ONLY the provably-pre-execution classes —
	// EXCEED_TIME_WINDOW (one corrective resync) and HTTP 429 (Retry-After /
	// backoff within budget) — even when !Idempotent. The ambiguous classes
	// (network/5xx) are NOT retried unless Idempotent. It exists for the L2
	// reconcile path.
	RetryPreExec bool
	// BudgetMs bounds total inter-retry SLEEP (the corrective EXCEED_TIME_WINDOW
	// resync does not draw it down). 0 disables the budgeted (sleeping) classes;
	// the no-sleep corrective retry still runs when its gate allows.
	BudgetMs int
}

// Meta reports how a Do executed. A post-call journal-write failure is NOT here:
// the Recorder delivers it through its own sink (callrec's onPostFailure), never
// back through Do — so a journal fault can never be conflated with the API error.
type Meta struct {
	Attempts     int
	StartedAtMs  int64
	FinishedAtMs int64
	RecordID     int64
}

// isAPIError reports whether err is a server error envelope (vs a pre-response
// transport failure), so Do can log the two at different levels.
func isAPIError(err error) bool {
	var ae *output.ApiError
	return errors.As(err, &ae)
}

// nowMs is the wall clock used to bracket a Do (the Outcome timing). It is the
// real wall clock, independent of SignNow (which is the server-clock estimate
// used only for signing).
func nowMs() int64 { return time.Now().UnixMilli() }

// Do executes one logical call under the given policy: it signs via the shared
// clock, runs exactly the retry ladder the policy selects, and (when a per-call
// Recorder is minted) gates the first send on Recorder.Ready and writes one
// Recorder.Record after the call completes.
//
// On EXCEED_TIME_WINDOW, when Resync is wired and the policy retries the
// pre-execution class, Do resyncs the shared clock once and re-signs reading the
// now-corrected Clock (timestamp and widened recvWindow); with no Resync the
// rejection is surfaced, not retried.
//
// Context: each attempt's HTTP deadline is the TIGHTER of Call.TimeoutMs (else
// the client default, 15s) and ctx. Inter-retry sleeps select on ctx and abort
// promptly on cancellation. A cancellation mid-retry surfaces ctx.Err()
// (context.Canceled or context.DeadlineExceeded); a cancellation mid-attempt
// surfaces the transport error from the aborted request (Execute's
// "...failed before a response was received" wrap), classified as transient.
//
// One Do == one Recorder.Record. A Ready error aborts with nothing sent and is
// returned as Do's error (the call never started). A post-call journal failure
// is delivered through the Recorder's own sink, never via Do's error or Meta.
func (c *Client) Do(ctx context.Context, call Call, pol Policy) (json.RawMessage, Meta, error) {
	startedAt := nowMs()
	info := c.callInfo(call)
	log := c.log()
	host := hostOf(c.BaseURL)

	// Fail fast on a signed call with no credentials, before Ready or any send.
	if call.Auth && c.Creds == nil {
		return nil, Meta{StartedAtMs: startedAt, FinishedAtMs: nowMs()}, errNoCreds
	}

	// Disclose the signing scheme + public key id (never the signature/secret) so
	// a --debug run can confirm which key/algorithm a rejected call signed with.
	if call.Auth && c.Creds != nil {
		log.Debug("signing call",
			"scheme", SchemeOf(c.Creds.Signer),
			"keyName", c.KeyName,
			"apiKeyId", c.APIKeyID,
			"recvWindow", c.recvWindowMs())
	}

	// One fresh per-call Recorder for this Do (nil = not recorded), so concurrent
	// calls on a shared client never share recording state.
	var rec Recorder
	if c.NewRecorder != nil {
		rec = c.NewRecorder(ctx, call)
	}

	// Pre-send gate: Ready must succeed before anything goes on the wire.
	if rec != nil {
		if err := rec.Ready(info); err != nil {
			return nil, Meta{StartedAtMs: startedAt, FinishedAtMs: nowMs()}, err
		}
	}

	opts := c.options(call)
	req := Request{Method: call.Method, Path: call.Path, Params: call.Params, Auth: call.Auth}

	attempts := 0
	eng := retryEngine{
		idempotent: pol.Idempotent,
		retryPre:   pol.Idempotent || pol.RetryPreExec, // Idempotent implies the pre-exec classes too
		budgetMs:   int64(pol.BudgetMs),
		correct:    c.correction(),
		sleep:      c.Sleep,
		observe:    c.Observe,
		attempts:   &attempts,
		onSleep: func(d time.Duration) error {
			// Context-aware wait: abort promptly on cancellation, else sleep the
			// requested duration (via the injected Sleep when present).
			return ctxSleep(ctx, d, c.Sleep)
		},
	}

	data, err := eng.run(func(o Options) (json.RawMessage, error) {
		// If the context is already done (e.g. canceled during a prior sleep),
		// surface that rather than firing another request.
		if ce := ctx.Err(); ce != nil {
			return nil, ce
		}
		sendStart := nowMs()
		log.Debug("http send",
			"method", call.Method,
			"path", call.Path,
			"host", host,
			"attempt", attempts,
			"recvWindow", o.RecvWindow)
		data, err := executeCtxLog(ctx, req, o, log)
		elapsed := nowMs() - sendStart
		switch {
		case err == nil:
			log.Debug("http ok",
				"method", call.Method, "path", call.Path,
				"status", 200, "elapsedMs", elapsed)
		case isAPIError(err):
			// The server answered with an error envelope: log the symbolic code +
			// HTTP status (NOT the body — apiError already capped it for the caller).
			var ae *output.ApiError
			errors.As(err, &ae)
			log.Debug("http error response",
				"method", call.Method, "path", call.Path,
				"status", ae.HTTPStatus, "code", ae.Code, "elapsedMs", elapsed)
		default:
			// A network/transport failure before any response — the #1 thing to
			// surface with context. It is retried (idempotent) or surfaced; either
			// way the operator wants to see it.
			log.Warn("http transport error",
				"method", call.Method, "path", call.Path,
				"host", host, "attempt", attempts,
				"elapsedMs", elapsed, "err", err.Error())
		}
		return data, err
	}, opts)

	finishedAt := nowMs()
	meta := Meta{Attempts: attempts, StartedAtMs: startedAt, FinishedAtMs: finishedAt}

	if rec != nil {
		out := buildOutcome(err, attempts, startedAt, finishedAt)
		meta.RecordID = rec.Record(info, out)
	}

	return data, meta, err
}

// correction builds the EXCEED_TIME_WINDOW resync hook for the retry engine from
// Resync + Clock, or nil when Resync is unwired (no auto-correct, so the engine
// leaves EXCEED_TIME_WINDOW un-retried). It resyncs the shared clock once, then
// hands back the now-corrected timestamp + widened recvWindow to re-sign with.
// Reading Clock AFTER the resync is what makes the corrective retry use the fresh
// estimate; below the 5s threshold recvWindowMs() returns 0 and the engine keeps
// the prior window (the server's 5s default still covers it).
func (c *Client) correction() func() (Correction, error) {
	if c.Resync == nil {
		return nil
	}
	return func() (Correction, error) {
		if err := c.Resync(); err != nil {
			return Correction{}, err
		}
		return Correction{Now: c.signNow, RecvWindowMs: c.recvWindowMs()}, nil
	}
}

// SignHandshake builds the signed query string for the private WebSocket upgrade,
// the way the server verifies it: "timestamp[&recvWindow]&signature", with the
// signature computed over the exact encoded parameter string and appended last
// (the SAME ordered-encode + signature-last core REST signing uses, via
// signAuthParams). It signs with the shared Clock (and its widened recvWindow,
// omit-below-5s), so a correction discovered on any surface covers the next
// handshake too. The Signer's scheme is opaque here. It returns only the signed
// query — the caller owns dialing and the X-KAPI-KEY header (Client.APIKeyID).
// Errors only when the client is public-only (no Creds to sign with).
func (c *Client) SignHandshake() (string, error) {
	if c.Creds == nil {
		return "", errNoCreds
	}
	recvWindow := c.recvWindowMs()
	p := &orderedParams{}
	signAuthParams(p, c.Creds.Signer, c.signNow(), recvWindow)
	// Log the signing PARAMS only — never the signature itself (a secret-adjacent
	// value) nor the X-KAPI-KEY. recvWindowMs 0 means the param was omitted.
	c.log().Debug("ws upgrade signed", "offsetMs", c.clockOffset(), "recvWindowMs", recvWindow)
	return p.encode(), nil
}

// clockOffset is the shared clock's raw offset for diagnostics (0 when no Clock
// is wired).
func (c *Client) clockOffset() int64 {
	if c.Clock != nil {
		return c.Clock.Offset()
	}
	return 0
}

// callInfo builds the pre-signing CallInfo for a call. ParamsJSON is the
// pre-signing params only.
func (c *Client) callInfo(call Call) CallInfo {
	info := CallInfo{
		Origin:  c.Origin,
		Method:  call.Method,
		Path:    call.Path,
		BaseURL: c.BaseURL,
		Auth:    call.Auth,
	}
	if call.Auth {
		info.KeyName = c.KeyName
		info.APIKeyID = c.APIKeyID
	}
	if len(call.Params) > 0 {
		info.ParamsJSON = paramsJSON(call.Params)
	}
	return info
}

// options assembles the per-call apiclient.Options for the transport.
func (c *Client) options(call Call) Options {
	opts := Options{
		BaseURL:    c.BaseURL,
		RecvWindow: c.recvWindowMs(),
		TimeoutMs:  c.TimeoutMs,
		Doer:       c.Doer,
		Now:        c.signNow,
		UserAgent:  c.UserAgent,
	}
	if call.TimeoutMs > 0 {
		opts.TimeoutMs = call.TimeoutMs
	}
	if call.Auth {
		opts.Creds = c.Creds
	}
	return opts
}

// buildOutcome distills the retry result into an Outcome for the Recorder.
func buildOutcome(err error, attempts int, startedAt, finishedAt int64) Outcome {
	out := Outcome{
		Err:          err,
		Attempts:     attempts,
		StartedAtMs:  startedAt,
		FinishedAtMs: finishedAt,
		DurationMs:   finishedAt - startedAt,
	}
	if err == nil {
		out.HTTPStatus = 200
		return out
	}
	var ae *output.ApiError
	if errors.As(err, &ae) {
		out.HTTPStatus = ae.HTTPStatus
		out.Code = ae.Code
		out.Message = ae.Message
	} else {
		// A pre-response transport failure: no status, the wrapped message.
		out.Message = err.Error()
	}
	return out
}

// paramsJSON encodes the pre-signing params as a JSON object keyed by param
// name. Insertion order does not matter here — this is a journal record, not
// the signed payload (signing uses the ordered encoder over call.Params plus
// the appended timestamp/recvWindow/signature, none of which reach this map).
func paramsJSON(params []KV) []byte {
	m := make(map[string]string, len(params))
	for _, kv := range params {
		m[kv.Key] = kv.Value
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// ctxSleep waits d, returning early with ctx.Err() if ctx is canceled first.
// When sleep is non-nil it is used for the actual delay on a goroutine so the
// injected (deterministic) Sleep is still honored while staying cancelable;
// otherwise a timer is used.
func ctxSleep(ctx context.Context, d time.Duration, sleep func(time.Duration)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if sleep != nil {
		done := make(chan struct{})
		// The injected Sleep must return on its own (time.Sleep, or a fake): on
		// a ctx cancellation we stop waiting on it, but the goroutine still runs
		// until sleep(d) returns — bounded by the backoff ladder (<=2s) in
		// production. close(done) never blocks (buffered by the select below).
		go func() {
			sleep(d)
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
