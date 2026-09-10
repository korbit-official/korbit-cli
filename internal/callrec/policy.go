// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package callrec

import (
	"net/http"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
)

// FailMode is how a POST-call Record failure is handled — the second axis of a
// Decision. (A pre-send open failure at Ready time is always fatal regardless,
// since nothing has been sent: see the package doc.)
type FailMode int

const (
	// Fail hands the journal error to the onPostFailure sink, which the cli's
	// closure captures and surfaces after emitting the API result (runEndpoint
	// behavior: a post-success journal failure is hard-surfaced with a non-zero
	// exit).
	Fail FailMode = iota
	// Warn hands the journal error to the onPostFailure sink, which logs/toasts it
	// and lets the command still succeed (monitor/tui/mcp behavior).
	Warn
)

// Decision is the journaling policy's verdict for one call: whether to record it
// at all, and — if the post-call write fails — whether that is fatal or a
// warning.
type Decision struct {
	// Record is whether this call is journaled at all. When false the journal is
	// never opened (no DB file is created) and PostFailure is irrelevant.
	Record bool
	// PostFailure governs a post-call Record write failure only.
	PostFailure FailMode
	// Reason is the short fixed label explaining this verdict, logged verbatim in
	// the journaling-decision Debug line.
	Reason string
}

// PolicyFunc decides, per call, whether and how to journal it. It is a
// constructor parameter of the Recorder so a future caller can swap the whole
// policy wholesale without touching the recorder. The CallInfo it receives is
// the pre-signing call description (origin/surface, read/write class, params) —
// the only inputs a policy gets to reason over.
type PolicyFunc func(info apiclient.CallInfo) Decision

// DefaultPolicy returns the journaling policy, parameterized by whether debug
// mode is on. It is THE policy table — the single place to change journaling
// behavior:
//
//		surface           class   record?               post-failure
//		-------           -----   -------               ------------
//		cli               write   always                Fail
//		cli               read    only in debug         Fail
//		doctor            any     NEVER                  (n/a)
//		stream-backfill   any     NEVER                  (n/a)
//		<other>           write   always                Warn
//		<other>           read    only in debug         Warn
//
//	  - Writes (order place/cancel, transfers) are always journaled; reads only in
//	    --debug. A write operation's read sub-calls (e.g. place's reconcile fetch)
//	    are grouped under the write operation, so they are journaled with it — the
//	    class comes from the operation (CallInfo.Safety), not each sub-call.
//	  - "cli" surfaces a post-success journal failure fatally (Fail) after the
//	    result; "doctor" and "stream-backfill" never record (the stream layer's
//	    REST recovery reads are recovery machinery, not user actions). The decision
//	    lives in this table, not at the call sites.
//	  - any other surface (notably "monitor") records the same calls cli does but
//	    Warns on a post-failure, so a long-running session isn't killed by it.
func DefaultPolicy(debug bool) PolicyFunc {
	return func(info apiclient.CallInfo) Decision {
		switch info.Origin.Surface {
		case apiclient.SurfaceDoctor:
			// Signed diagnostic calls are never journaled. Explicit so flipping
			// doctor on to record is a one-line change here.
			return Decision{Record: false, Reason: "surface never journaled"}
		case apiclient.SurfaceStreamBackfill:
			// Stream REST recovery reads (snapshots re-fetched on every reconnect
			// and focus change) are recovery machinery, not user actions — never
			// journaled, so reconnect storms don't bury the real actions.
			return Decision{Record: false, Reason: "surface never journaled"}
		case apiclient.SurfaceCLI:
			rec, reason := recordDecision(info, debug)
			return Decision{Record: rec, PostFailure: Fail, Reason: reason}
		default:
			rec, reason := recordDecision(info, debug)
			return Decision{Record: rec, PostFailure: Warn, Reason: reason}
		}
	}
}

// recordDecision applies the write/read rule shared by cli and the other
// surfaces — writes always record, reads only under debug — with the matching
// diagnostic reason.
func recordDecision(info apiclient.CallInfo, debug bool) (record bool, reason string) {
	switch {
	case isWrite(info):
		return true, "write"
	case debug:
		return true, "read, in debug"
	default:
		return false, "read, not in debug"
	}
}

// isWrite reports whether a call mutates state. An operation supplies its class
// via CallInfo.Safety (anything but readOnly writes); an ad-hoc call has no
// Safety, so its HTTP method classifies it (anything but GET writes).
func isWrite(info apiclient.CallInfo) bool {
	if info.Safety != "" {
		return info.Safety != cmdmeta.SafetyReadOnly
	}
	return info.Method != "" && info.Method != http.MethodGet
}
