// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package monitorcmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/korbit-official/korbit-cli/internal/botapi"
	"github.com/korbit-official/korbit-cli/internal/jqfilter"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

// monitorSink handles stream events for one monitor mode. runMonitor owns the
// session, the bounded queue, and post-run error classification; the sink owns
// what each event becomes — printed (plain), filtered/transformed (jq), or
// acted on (the JS bot). The fatal trio is bot-only; plain/jq embed noFatal,
// whose fatal() returns a nil channel (which never fires in a select).
type monitorSink interface {
	onData(stream.Data)
	onNotice(stream.Notice)
	fatal() <-chan error
	recordFatal(error)
	fatalErr() error
}

// noFatal supplies the fatal trio for sinks that cannot fail mid-stream
// (plain, jq). A nil fatal() channel is never ready, so runMonitor's select
// simply never takes that case for these sinks.
type noFatal struct{}

func (noFatal) fatal() <-chan error { return nil }
func (noFatal) recordFatal(error)   {}
func (noFatal) fatalErr() error     { return nil }

// emitter is the shared output accounting every sink uses: it counts emitted
// lines against --max-events, cancels the run at the cap, and rate-limits the
// per-event warnings that can otherwise recur at frame rate. It does NOT print
// (sinks hold the printer) — it only tracks the stop conditions.
type emitter struct {
	maxEvents int
	ctx       context.Context
	cancel    context.CancelFunc
	warn      *rateLimiter
	log       *slog.Logger
	now       func() int64

	emitted int
}

// capped reports whether --max-events has been reached.
func (e *emitter) capped() bool { return e.maxEvents > 0 && e.emitted >= e.maxEvents }

// stopped reports whether output should cease — the run was canceled
// (Ctrl-C/--duration/a fatal) or the event cap was reached.
func (e *emitter) stopped() bool { return e.ctx.Err() != nil || e.capped() }

// emit counts one emitted output line.
func (e *emitter) emit() { e.emitted++ }

// stop cancels the streaming context (cap reached, deliberate stop, or fatal).
func (e *emitter) stop() { e.cancel() }

// skipErrorf logs a per-event eval failure that SKIPPED an event (a --where/--on
// exception or a --jq evaluation error), rate-limited so one buggy frame shape
// can't flood stderr at frame rate. It is logged at Error — and so shows at the
// default level — because a filter/predicate that silently drops events is a bug
// the operator must see, and the run otherwise exits 0 with NO program-output
// equivalent (unlike a fatal session error, which surfaces as the command's
// error). This is the one operational condition the default level reveals.
func (e *emitter) skipErrorf(format string, args ...any) {
	if e.warn.allow(e.now()) {
		e.log.Error(fmt.Sprintf(format, args...))
	}
}

// plainSink streams every event unfiltered: data lines count toward
// --max-events; notices are always printed (they narrate the stream's health,
// including during a drain after the cap).
type plainSink struct {
	noFatal
	p  monitorPrinter
	em *emitter
}

func (s *plainSink) onData(d stream.Data) {
	if s.em.stopped() {
		return
	}
	s.p.data(d)
	s.em.emit()
	if s.em.capped() {
		s.em.stop()
	}
}

func (s *plainSink) onNotice(n stream.Notice) { s.p.notice(n) }

// jqSink runs the --jq program over every line — data AND notices, matching
// `monitor --json | jq` — and emits whatever it produces. No output drops the
// event; each output is one line. --max-events counts emitted output lines. A
// per-event evaluation error skips the event with a rate-limited warning (the
// same policy as a --where exception) so one odd frame can't kill an overnight
// monitor.
type jqSink struct {
	noFatal
	p    monitorPrinter
	em   *emitter
	prog *jqfilter.Program
}

func (s *jqSink) onData(d stream.Data)     { s.run(dataLineBytes(d)) }
func (s *jqSink) onNotice(n stream.Notice) { s.run(noticeLineBytes(n)) }

func (s *jqSink) run(lineBytes []byte) {
	if s.em.stopped() {
		return
	}
	lines, err := s.prog.Run(s.em.ctx, lineBytes)
	if err != nil {
		if s.em.ctx.Err() != nil {
			return // interrupted by a deliberate stop
		}
		s.em.skipErrorf("%v (event skipped)", err)
		return
	}
	for _, ln := range lines {
		if s.em.capped() {
			break
		}
		s.p.raw(ln)
		s.em.emit()
	}
	if s.em.capped() {
		s.em.stop()
	}
}

// botSink is the experimental JavaScript runtime: --where filters data events
// synchronously, --on acts on each passing data event AND every notice, and an
// unhandled handler failure (or an unhandled promise rejection arriving on
// Fatal()) stops the monitor. It owns the fatal trio.
type botSink struct {
	p     monitorPrinter
	em    *emitter
	bot   *botapi.Runtime
	where string
	onSrc string

	fatalV error
}

func (s *botSink) fatal() <-chan error { return s.bot.Fatal() }
func (s *botSink) fatalErr() error     { return s.fatalV }

// recordFatal records the first fatal failure and stops the stream; later ones
// are dropped (the first one wins, as with the result classification).
func (s *botSink) recordFatal(err error) {
	if s.fatalV == nil {
		s.fatalV = err
	}
	s.em.stop()
}

// runHandler invokes --on for one event, serialized: it returns only when the
// handler's promise settles. A deliberate stop (Ctrl-C/--duration mid-handler)
// is not a failure; any other error is fatal.
func (s *botSink) runHandler(bev botapi.Event) {
	if s.onSrc == "" || s.fatalV != nil || s.em.ctx.Err() != nil {
		return
	}
	if err := s.bot.RunHandler(bev); err != nil {
		if errors.Is(err, botapi.ErrStopped) {
			s.em.stop() // deliberate stop while the handler was in flight
			return
		}
		s.recordFatal(err)
	}
}

func (s *botSink) onNotice(n stream.Notice) {
	// Feed the materialized state first (a no-op without --stateful): notices
	// drive the epoch reconcile and Health, so state.* must see them before --on.
	s.bot.Ingest(n)
	s.p.notice(n)
	s.runHandler(noticeBotEvent(n))
}

func (s *botSink) onData(d stream.Data) {
	// Feed the materialized state for EVERY data event, before --where (a no-op
	// without --stateful): an event --where filters out must still update state
	// (a trade that doesn't match still moves balances/fills). This runs even
	// while draining after a stop — it is a Store apply, not a re-entry of the
	// VM script, so the "do not re-enter --where/--on" rule below does not apply.
	s.bot.Ingest(d)
	if s.em.stopped() {
		// Draining after a stop (fatal, Ctrl-C/--duration, or the event cap):
		// no more output, and — load-bearing — do NOT re-enter --where/--on.
		// The VM interrupt that unwinds a runaway script fires once; running it
		// again here would loop with nobody left to interrupt it.
		return
	}
	bev := dataBotEvent(d)
	if s.where != "" {
		ok, err := s.bot.Match(bev)
		if err != nil {
			if errors.Is(err, botapi.ErrStopped) {
				s.em.stop() // a runaway predicate broken by Ctrl-C/--duration
				return
			}
			s.em.skipErrorf("%v (event skipped)", err)
			return
		}
		if !ok {
			return
		}
	}
	s.p.data(d)
	s.em.emit()
	// Run the handler for this event BEFORE honoring the cap, so
	// `--max-events 1 --on '...'` acts on the one event it waited for (e.g.
	// place an order on the first match, then exit).
	s.runHandler(bev)
	if s.em.capped() {
		s.em.stop()
	}
}
