// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package apiclient

import "github.com/digitalx-official/digitalx-cli/internal/cmdmeta"

// Origin attributes a logical call to the frontend that issued it: which
// Surface a Client acts for, with an optional finer Detail. It flows into
// CallInfo (the recorder seam); the journal `origin` column is reserved and not
// written.
type Origin struct {
	// Surface is the frontend identity, one of the Surface* constants below.
	Surface string
	// Detail is optional finer attribution (e.g. a sub-command), free-form.
	Detail string
}

// The canonical call-surface identities. A surface names the frontend/context a
// Client acts for; it is stamped on Origin.Surface (and reused as the
// User-Agent component) and the journaling policy keys its per-surface rules on
// it (see callrec.DefaultPolicy). Defining them once here — rather than as raw
// string literals at every call site and in the policy switch — keeps the
// producer and the matcher from drifting on a typo.
const (
	SurfaceCLI            = "cli"             // single-shot endpoint commands
	SurfaceTUI            = "tui"             // interactive trading terminal
	SurfaceMonitor        = "monitor"         // streaming bot runtime
	SurfaceMCP            = "mcp"             // MCP server tools
	SurfaceDoctor         = "doctor"          // read-only diagnostics (never journaled)
	SurfaceStreamBackfill = "stream-backfill" // stream REST recovery reads (never journaled)
	SurfaceStreamWS       = "stream-ws"       // the WebSocket upgrade itself (User-Agent only; never journaled)
)

// CallInfo describes one logical call for the journal seam, captured BEFORE
// signing. It deliberately carries only non-secret, pre-signing facts:
// ParamsJSON must be the request's pre-signing params — the timestamp,
// recvWindow, and signature are appended inside Build AFTER CallInfo is built,
// so they never reach a Recorder. The private key is never present.
type CallInfo struct {
	Origin   Origin
	Method   string
	Path     string
	BaseURL  string
	Auth     bool
	KeyName  string // the named key signing this call ("" for public calls)
	APIKeyID string // the public api-key id ("" for public calls)
	// Safety is the read/write class when this call belongs to an operation; the
	// journaling policy reads it to record writes and skip reads. Empty for an
	// ad-hoc call, where Method classifies it instead.
	Safety cmdmeta.Safety
	// ParamsJSON is the pre-signing params as a JSON object (wire param names →
	// values), or nil/empty when there are none. Pre-signing ONLY: never carries
	// timestamp/recvWindow/signature.
	ParamsJSON []byte
}

// Outcome is the post-call result handed to Recorder.Record. Exactly one of the
// success/error shapes is meaningful: on success Err is nil and HTTPStatus is
// the 2xx seen; on failure Err is the surfaced error and Code/HTTPStatus/Message
// describe it (from the ApiError when the failure was an API rejection).
type Outcome struct {
	// Err is the error Do is about to return (nil on success).
	Err error
	// HTTPStatus is the final HTTP status observed (0 if no response was
	// received — a pre-response transport failure).
	HTTPStatus int
	// Code is the symbolic error code on an API rejection ("" otherwise).
	Code string
	// Message is a short human description of the failure ("" on success).
	Message string
	// Attempts is the number of underlying sends this logical call took.
	Attempts int
	// StartedAtMs / FinishedAtMs bracket the whole logical call (all attempts),
	// in unix ms; DurationMs is their difference for convenience.
	StartedAtMs  int64
	FinishedAtMs int64
	DurationMs   int64
}

// Recorder is the journal seam for the L1 client. It is an interface here on
// purpose: package apiclient must NOT import internal/journal (that would invert
// the layering and pull SQLite into the wire layer). A frontend supplies the
// concrete Recorder.
//
// Severity contract: the Recorder — not Client.Do — decides fail-vs-warn, and it
// delivers a POST-call failure entirely through its own sink (callrec's
// onPostFailure), never back through Do. So Record cannot return an error: a
// broken journal at write time is the Recorder's to surface (warn, toast, or
// capture-for-fatal), keeping the API error and the journal error on strictly
// separate channels. The only failure the client honors is a PRE-send Ready
// error, which aborts with nothing on the wire (the hard guarantee).
type Recorder interface {
	// Ready is the pre-send gate, called once BEFORE the first send of a call
	// that will be recorded. An error aborts the call with nothing on the wire,
	// preserving the open-journal-before-send hard guarantee (e.g. a read-only
	// home fails at this point, having sent nothing). Returning nil permits the
	// send.
	Ready(info CallInfo) error
	// Record is the post-call write, called exactly once after the logical call
	// completes (success or failure) with the attempt count folded in — one
	// logical Do == one record, one api_calls row per call. recordID is the
	// stored row id (0 when not applicable). A write failure is NOT returned: the
	// Recorder routes it through its own sink (see the severity contract above).
	Record(info CallInfo, out Outcome) (recordID int64)
}
