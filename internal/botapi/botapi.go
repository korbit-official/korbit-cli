// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package botapi is the monitor command's JavaScript bot runtime: a goja
// event loop on a dedicated script goroutine, an asynchronous Promise-based
// api.* API generated from the command spec, and a script-local SQLite
// db.* surface — so a small script passed via --init/--where/--on can be a
// complete trading bot.
//
// The execution model is the load-bearing part:
//
//   - ALL JavaScript runs on one event-loop goroutine (goja runtimes are not
//     goroutine-safe). The WebSocket pipeline runs no JS.
//   - Every api.* / db.* method returns a Promise immediately and hands
//     its blocking work (REST, SQLite) to a bounded worker-goroutine pool;
//     the worker resolves the Promise back onto the loop. The loop therefore
//     NEVER blocks on I/O, and Promise.all of several calls is genuine
//     concurrency (bounded by the pool size).
//   - --where stays a cheap synchronous filter with no api./db. access; it
//     shares the runtime with --init/--on, so indicator state built anywhere
//     is visible everywhere.
//   - --on handler invocations are serialized by the caller (RunHandler
//     returns only when the handler's promise settles): no two --on handlers
//     ever overlap. But serialization is at JOB granularity, not at handler
//     granularity — while a handler is parked at an `await`, the loop runs
//     other ready jobs, so a timer callback (setTimeout/setInterval) and the
//     continuation of an un-awaited fire-and-forget call left running by an
//     EARLIER handler can interleave at the await point. Shared-state safety is
//     therefore absolute only for a script that AWAITs everything it starts;
//     concurrency within one handler is still explicit (Promise.all).
//   - There is no per-event watchdog: with I/O off the loop only a pure-CPU
//     runaway can stall it, and the signal context interrupts the VM.
//   - The worker pool bounds IN-FLIGHT work, not PENDING work: submission never
//     blocks the loop, so a handler that issues calls in an unbounded loop
//     accumulates pending goroutines parked on the pool semaphore. This is an
//     accepted limit — the runtime defends against accidents, not against a
//     hostile script.
//
// The retry/idempotency policy lives in the Go bindings, not in scripts:
// reads and idempotent writes auto-retry, order placement uses the
// clientOrderId reconcile protocol, and the no-idempotency-key money movers
// are strictly single-shot. Money values stay decimal strings end to end —
// a JS number where a decimal is expected throws.
package botapi

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/ops"
	"github.com/digitalx-official/digitalx-cli/internal/stream"
	"github.com/digitalx-official/digitalx-cli/internal/stream/state"
	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
)

//go:embed prelude.js
var preludeSrc string

// preludeProg is compiled once per process; programs are immutable and safe
// to run on any number of runtimes.
var preludeProg = goja.MustCompile("prelude.js", preludeSrc, false)

// ErrStopped is returned by RunHandler when the signal context was canceled
// while a handler was in flight: a deliberate stop, not a script failure.
var ErrStopped = errors.New("stopped while a handler was running")

// Event is one stream event as the script sees it. Payload is the verbatim
// JSON document (a WebSocket frame, REST backfill data, or a notice object).
type Event struct {
	Type       string // "data" | "notice"
	Channel    string
	Symbol     string
	Origin     string
	ServerTime int64
	Source     string
	// AccountSeq is the sub-account of a private-channel data event (surfaced as
	// ev.accountSeq), nil when not applicable/untagged (public channels, notices,
	// or a frame the server did not tag with an accountSeq).
	AccountSeq *int
	Payload    []byte
}

// Options configures a Runtime.
type Options struct {
	// Where is the per-data-event predicate expression (optional).
	Where string
	// On is the per-event handler body, run after Where passes and for every
	// notice (optional). May use await; api.*/db.* are available.
	On string
	// Init is a script run once before streaming (optional). May use await;
	// api.*/db.* are available. No time budget — only SignalCtx bounds it.
	Init string

	// API is the L2 operations layer behind every api.* method — it owns the
	// retry/idempotency policy and the place/history/candles/funding protocols.
	// nil makes api.* unavailable. The bindings carry NO call policy of their
	// own: they parse+validate, then call API.{PlaceOrder,History,Candles,
	// FundingHistory,Invoke}. The order-row journal seam, the clock-resync hook,
	// the reconcile Sleep, and the per-call retry budget all live on the API.
	API *ops.API
	// Surface is the frontend identity for the operations ledger (e.g. "monitor",
	// "mcp"), threaded into every operation's RunInput.
	Surface string
	// KeyName / APIKeyID identify the signing key for the operations ledger (the
	// public api-key id; never a secret). "" for a public-only session.
	KeyName  string
	APIKeyID string
	// KeyManager provides per-key metadata lookups (MetaDefaultAccountSeq) so
	// the bot's accountSeq resolution honors a stored key's configured default.
	// It is ignored when KeyName is empty or when Inline is set (an inline
	// environment credential has no stored per-key metadata).
	KeyManager *keys.Manager
	// Inline is true when the signing credential came from inline environment
	// material rather than a stored key (mirrors keys.Selection.Inline), so the
	// per-key metadata lookup is skipped.
	Inline bool
	// CredsErr, when non-empty, is the message authenticated methods throw —
	// set when no signing key could be resolved.
	CredsErr string

	// DBPath is the SQLite file behind db.*, opened lazily on first use.
	// Empty makes db.* unavailable.
	DBPath string
	// NoFsync opens the db.* database with PRAGMA synchronous=OFF (faster writes,
	// weaker crash-durability) — the --no-fsync/DIGITALX_CLI_NO_FSYNC opt-in.
	NoFsync bool

	// Stateful builds a materialized state.Store fed by Ingest and exposes it as
	// the synchronous state.* global. When false the Store is never built or fed
	// (zero cost) and every state.* method throws.
	Stateful bool

	// AccountSeqs is the set of private sub-accounts the session subscribed
	// (effAccountSeq-normalized, ≥1), used to roll the store's PER-ACCOUNT
	// readiness up into the single state.ready().balances / .openOrders booleans:
	// ready is true only when EVERY subscribed account is ready, so a partial
	// snapshot (one account healed, another still failing its backfill) never
	// reads as complete. Empty when no private channels are subscribed, which
	// keeps those flags false (as they were when balances/orders never latched).
	AccountSeqs []int

	// MaxConcurrency caps in-flight api.*/db.* work (default 8).
	MaxConcurrency int

	// ServerNow is the session's server-clock estimate, behind api.now() (a bot
	// reads it to compare against server-stamped timestamps). It is NOT used for any
	// local record — see Now.
	ServerNow func() int64
	// Now is the local system clock (unix ms), used only to stamp the materialized
	// state Store's Health.LastDataAt — a local receive time, compared against the
	// reader's own "now", so it stays in the system frame (never the server-clock
	// estimate). nil = time.Now().UnixMilli().
	Now func() int64
	// Sleep delays db.* retries and is injectable for tests (default time.Sleep).
	// The api.* reconcile sleeps use ops.API.Sleep instead.
	Sleep func(time.Duration)
	// Stderr receives console output and runtime warnings (default: discard).
	Stderr io.Writer
	// Log is the optional operational logger passed to the materialized state
	// Store (state reconcile mechanics, Debug). nil = silent. Only used when
	// Stateful is set.
	Log *slog.Logger
	// SignalCtx, when set, interrupts running JavaScript on cancellation
	// (Ctrl-C/SIGTERM) — the only bound on --init and on a runaway script.
	SignalCtx context.Context
}

// Runtime is one bot-script runtime. Create with New (which runs Init);
// call Match/RunHandler from a single controller goroutine; Close when done.
type Runtime struct {
	opts Options
	loop *eventloop.EventLoop
	vm   *goja.Runtime // owned by the loop goroutine; see onLoop
	pool *pool
	db   *botDB

	// stateful and store back the optional state.* read-model. store is built in
	// setupOnLoop only when stateful, fed by Ingest, and read by state.* — all on
	// the loop goroutine, so it needs no lock.
	stateful bool
	store    *state.Store

	// feeRates caches the quote-fee headroom rate per {account, symbol} for
	// place-hold sizing (see quoteFeeRate in bindings.go). Unlike the store it
	// IS lock-guarded (feeMu): it is read and written from worker goroutines.
	feeMu    sync.Mutex
	feeRates map[feeRateKey]string

	whereFn   goja.Callable
	onFn      goja.Callable
	settleFn  goja.Callable // prelude __settle: attaches a Go callback to a promise
	jsonParse goja.Callable

	inWhere   bool // set on the loop while --where evaluates: api./db. deny gate
	unhandled map[*goja.Promise]bool
	fatalCh   chan error // first unhandled promise rejection
	closed    chan struct{}

	// stopCtx is the streaming-phase shutdown context (signal OR --duration),
	// installed by WatchInterrupt before streaming starts. It interrupts a
	// pure-CPU runaway in --where/--on and lets Match/RunHandler tell a
	// deliberate stop from a script failure. nil until WatchInterrupt runs;
	// before then (and during --init) only SignalCtx applies.
	stopCtx context.Context
}

// stopContext is the context Match/RunHandler classify interruptions against:
// the streaming-phase context once WatchInterrupt has run, else SignalCtx.
func (r *Runtime) stopContext() context.Context {
	if r.stopCtx != nil {
		return r.stopCtx
	}
	return r.opts.SignalCtx
}

// opCtx is the cancelable context for in-flight ops calls dispatched to the
// worker pool: the streaming-phase shutdown context once streaming has begun,
// else the signal context, else Background. On shutdown its cancellation aborts
// ops's inter-attempt waits (a backoff or lookup-retry sleep), not a request
// mid-flight; the reconcile / UNKNOWN endgame then resolves any ambiguity safely
// (a maybe-landed order becomes UNKNOWN, never a resend), so workers drain
// promptly instead of waiting out the retry budget.
func (r *Runtime) opCtx() context.Context {
	if c := r.stopContext(); c != nil {
		return c
	}
	return context.Background()
}

// New builds the runtime: starts the event loop, installs the prelude and the
// api./db. bindings, compiles Where/On, and runs Init to completion.
// Errors are user-facing and name the flag they came from. If SignalCtx is
// canceled during Init, the returned error wraps the context error so the
// caller can tell "user aborted" from "init is broken".
func New(opts Options) (*Runtime, error) {
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 8
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	r := &Runtime{
		opts:      opts,
		loop:      eventloop.NewEventLoop(eventloop.EnableConsole(false)),
		pool:      newPool(opts.MaxConcurrency),
		unhandled: make(map[*goja.Promise]bool),
		fatalCh:   make(chan error, 1),
		closed:    make(chan struct{}),
	}
	if opts.DBPath != "" {
		r.db = &botDB{path: opts.DBPath, noFsync: opts.NoFsync}
	}
	r.loop.Start()

	if err := r.setup(); err != nil {
		r.Close()
		return nil, err
	}

	// Interrupt a runaway --init when the signal context is canceled (init has
	// no time budget — Ctrl-C is its only bound). Scoped to --init: once it
	// returns, the streaming-phase interrupt (WatchInterrupt) takes over.
	initDone := make(chan struct{})
	if opts.SignalCtx != nil {
		go func() {
			select {
			case <-opts.SignalCtx.Done():
				r.vm.Interrupt(opts.SignalCtx.Err())
			case <-initDone:
			case <-r.closed:
			}
		}()
	}

	err := r.runInit()
	close(initDone)
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// WatchInterrupt installs the streaming-phase shutdown context: a goroutine
// interrupts the VM when ctx is canceled (Ctrl-C/SIGTERM or --duration), so a
// pure-CPU runaway in --where/--on — the only thing that can stall the loop
// with I/O off it — still unwinds and exits. Call once, after New and before
// streaming. ctx should be canceled by both the signal and --duration.
func (r *Runtime) WatchInterrupt(ctx context.Context) {
	if ctx == nil {
		return
	}
	r.stopCtx = ctx
	go func() {
		select {
		case <-ctx.Done():
			r.vm.Interrupt(ctx.Err())
		case <-r.closed:
		}
	}()
}

// setup runs on the loop: prelude, bindings, rejection tracker, and the
// Where/On compilation. Reported errors are user-facing.
func (r *Runtime) setup() error {
	var err error
	lerr := r.onLoop(func(vm *goja.Runtime) {
		r.vm = vm
		err = r.setupOnLoop(vm)
	})
	if lerr != nil {
		return lerr
	}
	return err
}

func (r *Runtime) setupOnLoop(vm *goja.Runtime) error {
	if _, err := vm.RunProgram(preludeProg); err != nil {
		return fmt.Errorf("internal: script prelude failed: %w", err)
	}
	jsonObj, ok := vm.Get("JSON").(*goja.Object)
	if !ok {
		return errors.New("internal: JSON object missing from the runtime")
	}
	if r.jsonParse, ok = goja.AssertFunction(jsonObj.Get("parse")); !ok {
		return errors.New("internal: JSON.parse missing from the runtime")
	}
	if r.settleFn, ok = goja.AssertFunction(vm.Get("__settle")); !ok {
		return errors.New("internal: prelude __settle missing")
	}

	// Track unhandled promise rejections: a fire-and-forget api.* call
	// whose rejection nobody handles must stop the bot, not vanish — an
	// action script in an unknown state should not keep firing.
	vm.SetPromiseRejectionTracker(func(p *goja.Promise, op goja.PromiseRejectionOperation) {
		switch op {
		case goja.PromiseRejectionReject:
			r.unhandled[p] = true
			// Check after the current job's microtasks have drained: a
			// .then/.catch attached synchronously has run by then.
			r.loop.RunOnLoop(func(*goja.Runtime) {
				if r.unhandled[p] {
					delete(r.unhandled, p)
					r.reportFatal(fmt.Errorf("unhandled promise rejection: %w", errorFromJS(p.Result())))
				}
			})
		case goja.PromiseRejectionHandle:
			delete(r.unhandled, p)
		}
	})

	if err := r.installConsole(vm); err != nil {
		return err
	}
	if err := r.installAPI(vm); err != nil {
		return err
	}
	if err := r.installDB(vm); err != nil {
		return err
	}
	if err := installTA(vm); err != nil {
		return err
	}
	// The state.* global is ALWAYS installed (so the disabled path is a clean
	// throw, not a missing global); the backing Store is built only when stateful.
	r.stateful = r.opts.Stateful
	if r.stateful {
		r.store = state.New(state.Config{Log: r.opts.Log}, r.opts.Now)
	}
	if err := installState(r, vm); err != nil {
		return err
	}

	if r.opts.Where != "" {
		fn, err := r.compileWhere(vm, r.opts.Where)
		if err != nil {
			return err
		}
		r.whereFn = fn
	}
	if r.opts.On != "" {
		fn, err := r.compileOn(vm, r.opts.On)
		if err != nil {
			return err
		}
		r.onFn = fn
	}
	return nil
}

// accountSeqJS converts an event's optional sub-account into the JS value bound
// at ev.accountSeq: the number, or null when the event carries no accountSeq
// (public data, notices, or an untagged private frame).
func accountSeqJS(vm *goja.Runtime, seq *int) goja.Value {
	if seq == nil {
		return goja.Null()
	}
	return vm.ToValue(*seq)
}

// compileWhere wraps the predicate expression: bind the convenience names,
// return the expression's truthiness. Compiled standalone first so a syntax
// error is reported against the user's own text (the trailing newline keeps a
// `// comment` tail from swallowing the closing paren).
func (r *Runtime) compileWhere(vm *goja.Runtime, where string) (goja.Callable, error) {
	if _, err := goja.Compile("--where", "("+where+"\n)", false); err != nil {
		return nil, fmt.Errorf("--where is not a valid JavaScript expression: %w", err)
	}
	wrapper := "(function (channel, symbol, origin, serverTime, source, __json, __accountSeq) {\n" +
		"var payload = JSON.parse(__json);\n" +
		"var rows = __rows(payload);\n" +
		"var ev = { type: 'data', channel: channel, symbol: symbol, origin: origin, serverTime: serverTime, source: source, accountSeq: __accountSeq, payload: payload, rows: rows };\n" +
		"return (" + where + "\n); })"
	fnVal, err := vm.RunString(wrapper)
	if err != nil {
		return nil, fmt.Errorf("--where is not a valid JavaScript expression: %s", jsErrorMessage(err))
	}
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return nil, errors.New("internal: predicate wrapper did not produce a function")
	}
	return fn, nil
}

// compileOn wraps the handler body in an async function, so `await` works and
// the returned promise is what RunHandler awaits before the next event.
func (r *Runtime) compileOn(vm *goja.Runtime, on string) (goja.Callable, error) {
	wrapper := "(async function (__type, channel, symbol, origin, serverTime, source, __json, __accountSeq) {\n" +
		"var payload = JSON.parse(__json);\n" +
		"var rows = __rows(payload);\n" +
		"var ev = { type: __type, channel: channel, symbol: symbol, origin: origin, serverTime: serverTime, source: source, accountSeq: __accountSeq, payload: payload, rows: rows };\n" +
		on + "\n})"
	if _, err := goja.Compile("--on", wrapper, false); err != nil {
		return nil, fmt.Errorf("--on is not valid JavaScript: %w", err)
	}
	fnVal, err := vm.RunString(wrapper)
	if err != nil {
		return nil, fmt.Errorf("--on is not valid JavaScript: %s", jsErrorMessage(err))
	}
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return nil, errors.New("internal: handler wrapper did not produce a function")
	}
	return fn, nil
}

// runInit executes Init and waits for it to finish. A synchronous script runs
// as a plain program (so `var x` declares a global); a script using top-level
// await is wrapped in an async IIFE and awaited — in that mode `var` is
// function-local, so cross-hook state should be assigned bare (`x = 1`) or
// via globalThis.
func (r *Runtime) runInit() error {
	if r.opts.Init == "" {
		return nil
	}
	prog, progErr := goja.Compile("--init", r.opts.Init, false)
	async := false
	if progErr != nil {
		// Not valid as a plain script — maybe it uses top-level await.
		wrapped, wrapErr := goja.Compile("--init", "(async function () {\n"+r.opts.Init+"\n})()", false)
		if wrapErr != nil {
			return fmt.Errorf("--init is not valid JavaScript: %w", progErr)
		}
		prog, async = wrapped, true
	}

	settled := make(chan error, 1)
	ok := r.loop.RunOnLoop(func(vm *goja.Runtime) {
		v, err := vm.RunProgram(prog)
		if err != nil {
			settled <- err
			return
		}
		if !async {
			settled <- nil
			return
		}
		// Await the IIFE's promise via the prelude helper.
		_, err = r.settleFn(goja.Undefined(), v, vm.ToValue(func(call goja.FunctionCall) goja.Value {
			if call.Argument(0).ToBoolean() {
				settled <- nil
			} else {
				settled <- errorFromJS(call.Argument(1))
			}
			return goja.Undefined()
		}))
		if err != nil {
			settled <- err
		}
	})
	if !ok {
		return errors.New("internal: event loop is not running")
	}

	ctx := r.opts.SignalCtx
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	select {
	case err := <-settled:
		if err != nil {
			var ie *goja.InterruptedError
			if errors.As(err, &ie) && ctx != nil && ctx.Err() != nil {
				return fmt.Errorf("--init interrupted: %w", ctx.Err())
			}
			return fmt.Errorf("--init failed: %s", jsErrorMessage(err))
		}
		return nil
	case err := <-r.fatalCh:
		return fmt.Errorf("--init failed: %v", err)
	case <-done:
		// The user aborted while init was awaiting REST/DB work. Interrupt
		// any running JS and leave; the caller treats this as a clean stop.
		r.vm.Interrupt(ctx.Err())
		return fmt.Errorf("--init interrupted: %w", ctx.Err())
	}
}

// Match evaluates --where against one data event. With no --where it matches
// everything. A returned ErrStopped means a deliberate shutdown interrupted
// the evaluation (a pure-CPU runaway predicate broken by Ctrl-C/--duration);
// any other error is a per-event JS exception the caller may skip and
// continue past.
func (r *Runtime) Match(ev Event) (bool, error) {
	if r.whereFn == nil {
		return true, nil
	}
	var matched bool
	var jserr error
	lerr := r.onLoop(func(vm *goja.Runtime) {
		r.inWhere = true
		v, err := r.whereFn(goja.Undefined(),
			vm.ToValue(ev.Channel), vm.ToValue(ev.Symbol), vm.ToValue(ev.Origin),
			vm.ToValue(ev.ServerTime), vm.ToValue(ev.Source), vm.ToValue(string(ev.Payload)),
			accountSeqJS(vm, ev.AccountSeq))
		r.inWhere = false
		if err != nil {
			var ie *goja.InterruptedError
			if ctx := r.stopContext(); errors.As(err, &ie) && ctx != nil && ctx.Err() != nil {
				jserr = ErrStopped
				return
			}
			jserr = fmt.Errorf("--where threw: %s", jsErrorMessage(err))
			return
		}
		matched = v.ToBoolean()
	})
	if lerr != nil {
		return false, lerr
	}
	return matched, jserr
}

// RunHandler invokes --on for one event and returns when the handler's
// promise settles (serializing handlers is the caller's contract — it must
// not pull the next event before RunHandler returns). A returned error is
// fatal to the bot, except ErrStopped (deliberate stop via the signal or
// --duration context). With no --on it is a no-op.
func (r *Runtime) RunHandler(ev Event) error {
	if r.onFn == nil {
		return nil
	}
	settled := make(chan error, 1)
	ok := r.loop.RunOnLoop(func(vm *goja.Runtime) {
		v, err := r.onFn(goja.Undefined(),
			vm.ToValue(ev.Type), vm.ToValue(ev.Channel), vm.ToValue(ev.Symbol), vm.ToValue(ev.Origin),
			vm.ToValue(ev.ServerTime), vm.ToValue(ev.Source), vm.ToValue(string(ev.Payload)),
			accountSeqJS(vm, ev.AccountSeq))
		if err != nil {
			settled <- err // a synchronous throw before the first await
			return
		}
		_, err = r.settleFn(goja.Undefined(), v, vm.ToValue(func(call goja.FunctionCall) goja.Value {
			if call.Argument(0).ToBoolean() {
				settled <- nil
			} else {
				settled <- errorFromJS(call.Argument(1))
			}
			return goja.Undefined()
		}))
		if err != nil {
			settled <- err
		}
	})
	if !ok {
		return errors.New("internal: event loop is not running")
	}

	ctx := r.stopContext()
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	select {
	case err := <-settled:
		if err != nil {
			var ie *goja.InterruptedError
			if errors.As(err, &ie) && ctx != nil && ctx.Err() != nil {
				return ErrStopped
			}
			// A synchronous throw arrives as a goja exception; normalize the
			// thrown value the same way a rejection reason is, so an API
			// error keeps its classification (exit-code 3) either way.
			var ex *goja.Exception
			if errors.As(err, &ex) {
				err = errorFromJS(ex.Value())
			}
			return fmt.Errorf("--on failed: %w", err)
		}
		return nil
	case err := <-r.fatalCh:
		return err
	case <-done:
		// Deliberate stop while the handler was awaiting I/O: interrupt any
		// running JS and leave without waiting for in-flight work.
		r.vm.Interrupt(ctx.Err())
		return ErrStopped
	}
}

// Ingest folds one stream event into the materialized state.Store, on the loop
// goroutine, BLOCKING until applied. The caller (the monitor controller) calls
// it for every event — data and notices — BEFORE Match/RunHandler, so a later
// state.* read observes up-to-date state (apply-before-evaluate), and a
// --where-filtered event still updates state. A no-op when --stateful is off
// (the Store is nil). It must be called from the same controller goroutine as
// Match/RunHandler (never nested inside them).
func (r *Runtime) Ingest(ev stream.Event) {
	if r.store == nil {
		return
	}
	done := make(chan struct{})
	ok := r.loop.RunOnLoop(func(*goja.Runtime) {
		r.store.Apply(ev)
		close(done)
	})
	if !ok {
		return // the loop is gone (shutting down); nothing left to update
	}
	<-done
}

// Fatal exposes the first unhandled promise rejection (a fire-and-forget
// api.* call that failed with nobody listening). The controller should
// select on it alongside its event queue and treat a received error as fatal.
func (r *Runtime) Fatal() <-chan error { return r.fatalCh }

func (r *Runtime) reportFatal(err error) {
	select {
	case r.fatalCh <- err:
	default: // a fatal error is already pending; first one wins
	}
}

// Close drains in-flight worker calls, stops the event loop, and closes the
// script database. Draining the pool first is load-bearing: a worker mid-call
// (e.g. an order placement still writing to the journal) must finish before
// the caller's deferred teardown — closing the journal, etc. — runs, or it
// would hit closed resources. A fire-and-forget api.* call left running by
// a handler that already returned is included; each worker is bounded by the
// REST timeout, so the wait is finite. Close must only be called after
// Match/RunHandler callers are done.
func (r *Runtime) Close() {
	select {
	case <-r.closed:
		return
	default:
	}
	close(r.closed)
	r.pool.wait()
	r.loop.Terminate()
	if r.db != nil {
		r.db.close()
	}
}

// onLoop runs fn on the event-loop goroutine and waits for it to finish.
func (r *Runtime) onLoop(fn func(vm *goja.Runtime)) error {
	done := make(chan struct{})
	ok := r.loop.RunOnLoop(func(vm *goja.Runtime) {
		defer close(done)
		fn(vm)
	})
	if !ok {
		return errors.New("internal: event loop is not running")
	}
	<-done
	return nil
}

// installConsole maps console.log/.error/.warn/.info to stderr — stdout is
// the monitor's NDJSON contract and scripts must not be able to corrupt it.
func (r *Runtime) installConsole(vm *goja.Runtime) error {
	stringify, _ := goja.AssertFunction(vm.Get("JSON").(*goja.Object).Get("stringify"))
	print := func(call goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(call.Arguments))
		for _, arg := range call.Arguments {
			parts = append(parts, consoleFormat(vm, stringify, arg))
		}
		fmt.Fprintln(r.opts.Stderr, strings.Join(parts, " "))
		return goja.Undefined()
	}
	console := vm.NewObject()
	for _, name := range []string{"log", "error", "warn", "info", "debug"} {
		if err := console.Set(name, print); err != nil {
			return err
		}
	}
	return vm.Set("console", console)
}

// consoleFormat renders one console argument: strings verbatim, objects as
// JSON, everything else via String().
func consoleFormat(vm *goja.Runtime, stringify goja.Callable, v goja.Value) string {
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return v.String()
	}
	switch v.Export().(type) {
	case map[string]interface{}, []interface{}:
		if stringify != nil {
			if s, err := stringify(goja.Undefined(), v); err == nil && !goja.IsUndefined(s) {
				return s.String()
			}
		}
	}
	return v.String()
}

// jsErrorMessage renders a goja error for the user: the thrown value for a JS
// exception (concise — no engine stack trace), the error text otherwise.
func jsErrorMessage(err error) string {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return ex.Value().String()
	}
	return err.Error()
}
