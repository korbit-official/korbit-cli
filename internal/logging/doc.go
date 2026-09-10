// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package logging is the CLI's leveled diagnostic logger, built on log/slog.
//
// It is deliberately distinct from program output. Command results and the
// structured error envelope are the product, and they go through
// internal/output (the stdout/stderr contract): results on stdout, one
// {"error": ...} object plus actionable guidance on stderr, never gated. This
// package carries the other thing — operational logs about the CLI's own
// behavior (retry/backoff decisions, request target and timing, clock-sync
// attempts, journal-write health, the order-place reconcile trail) — to stderr,
// gated by level. The default threshold is Error: a command's outcome and
// failures already reach the user through program output, so warn-and-below
// operational logs are an opt-in diagnostic (they would otherwise only echo, on
// the same stderr sink, what the error envelope already says). Opt in with
// --log-level (or --debug, which lowers the threshold to Debug).
//
// Levels, coarse to fine: error, warn, info, debug, and trace ([LevelTrace],
// one step below Debug).
//
//   - Error is the only tier shown by default, so it is kept LEAN: it is reserved
//     for a genuinely operational failure with NO program-output equivalent —
//     something the operator must see that the error envelope won't tell them.
//     The sole case is a monitor --where/--on/--jq per-event eval error that
//     SILENTLY SKIPS events while the run still exits 0. A condition that is
//     already program output must NEVER be logged at Error (it would duplicate the
//     error on the shared stderr sink).
//   - Debug carries the DECISIONS and OUTCOMES a "run with --log-level debug and
//     send me the log" round-trip needs (each request, the place protocol's
//     resend/duplicate/lookup verdict, a paged walk's pages/rows/complete summary).
//   - Trace adds the per-ITERATION firehose (each send attempt, each lookup retry,
//     each history/candles page) for dissecting a single operation.
//
// A money-unsafe verdict (a placement UNKNOWN or placed-but-unreadable) is NOT
// given a default-visible level: it already reaches the user as program output
// (the returned error + a guidance note), so the operational log records it at
// Debug as part of the trail rather than re-emitting it. --debug resolves to
// Debug, never Trace; Trace is explicit-opt-in (--log-level trace) only.
// slog.Logger has no Trace method, so [Trace] is this package's shim over it.
//
// What is NOT a log (stays program output, never level-gated): the result, the
// error envelope, the "signing as key" safety disclosure, doctor's report, and
// the key/keystore/ip command guidance. A log is telemetry about how the tool
// is operating; product output is the result or a safety-relevant fact about
// the action the user requested.
//
// Output is one record per line, in one of three shapes selected by [Style]
// (the cli resolves it from --log-format and whether --log-file is in use):
//
//	<prog>: <level>: <message>[ key=value …]    // text, stderr (the default)
//	<rfc3339-local> <level> <message>[ key=value …] // text, --log-file (Style.Timestamp)
//	{"time":"…","level":"…","msg":"…",…}            // --log-format json (FormatJSON)
//
// where <level> is one of trace/debug/info/warn/error (the JSON level uses
// the same vocabulary). The "<prog>: " tag — the invoked program name — scopes a
// line to this tool on a shared terminal; a file or JSON trail has no such
// session to scope, so it carries a wall-clock timestamp instead.
//
// Each record is emitted to the sink in exactly ONE write (the line is fully
// assembled first, in every format). Within a single logger and its With/
// WithGroup derivatives that write is also serialized by a shared lock, so one
// logger is safe for concurrent use. But several INDEPENDENTLY constructed
// loggers (the monitor's operational + stream + notice loggers) hold separate
// locks, so keeping THEIR lines from interleaving on one shared sink is the
// caller's job: the cli wraps the sink in a locking writer (*syncWriter), and
// the one-write-per-record guarantee above is what makes that serialization
// line-atomic.
//
// Never log secrets. A value that carries key material must redact itself: the
// keystore-backed types already do under fmt verbs, and any value passed as a
// log attribute should implement slog.LogValuer if it could expose a secret.
// The keystore/keys layer in particular logs only non-secret facts (backend,
// key name, public api-key id, OSStatus-bearing error strings, paths, counts) —
// never PEM/ciphertext/AES-key/HMAC-secret bytes.
//
// Wiring. The lower layers (wire/clock/keystore/keys/config/journal/callrec/
// sandbox/selfupdate/stream and the rawapi/ops call path) take an OPTIONAL operational
// logger and log their own fragile-path diagnostics directly: a struct exposes a
// `Log *slog.Logger` field (nil = silent) and resolves it once with [Or] so call
// sites never need a nil check, or a free function takes a trailing optional
// `*slog.Logger`. The cli (frontend) is the only layer that BUILDS a logger — it
// wires rt.logger() (or, for the alt-screen tui, the file-only logger from
// runtime.tuiLogging) into those fields/params. rawapi.Client logs only its one
// diagnostic (a typed-decode mismatch); ops.API logs the operation-LEVEL
// decisions the wire layer can't see (the place reconcile trail, paging
// summaries) and leaves a single-call passthrough to the wire layer's own log.
// Three seams stand outside this convention: the wire retry-decision seam
// (apiclient.Client.Observe func(string)), the callrec journal-write warn callback,
// and ops.API.Stderr (program output, not a log).
package logging
