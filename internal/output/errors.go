// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package output

import (
	"encoding/json"
	"fmt"
)

// Process exit codes. This taxonomy is a public contract agents branch on, so
// the values are stable — extend additively, never renumber. These are the only
// codes the CLI mints; a passthrough subprocess (e.g. the sandbox bundle) may
// forward its own arbitrary status verbatim. ExitCodes pairs each with its
// agent-facing description.
const (
	ExitSuccess  = 0 // success
	ExitInternal = 1 // network failure or internal error
	ExitUsage    = 2 // usage error — the caller can fix the invocation
	ExitAPI      = 3 // the Digital X API rejected the request (see error.code)
	ExitConfig   = 4 // key/keystore/config problem — fix with `digitalx key ...`
)

// The error taxonomy maps each failure class to a process exit code and the
// shape of the structured error object emitted on stderr: a UsageError is
// ExitUsage, an ApiError is ExitAPI, a ConfigError is ExitConfig, and anything
// else is ExitInternal. Commands return plain errors; EmitError classifies them
// with errors.As, so wrapping with %w preserves the mapping.

// ExitCoder is an error that carries its own process exit code.
type ExitCoder interface {
	error
	ExitCode() int
}

// UsageError marks an invalid invocation the caller can fix.
type UsageError struct{ Message string }

func (e *UsageError) Error() string { return e.Message }
func (e *UsageError) ExitCode() int { return ExitUsage }

// Usagef builds a UsageError from a format string.
func Usagef(format string, a ...any) *UsageError {
	return &UsageError{Message: fmt.Sprintf(format, a...)}
}

// ConfigError marks a key/keystore/config problem (ExitConfig). An optional Cause
// lets a ConfigError wrap a sentinel for errors.Is matching while keeping its
// own user-facing Message — used e.g. to mark "unknown keystore backend" apart
// from "backend unavailable" (both ConfigError, same exit code). Cause never
// changes Error()/ExitCode(), only what errors.Is/Unwrap see.
type ConfigError struct {
	Message string
	Cause   error
}

func (e *ConfigError) Error() string { return e.Message }
func (e *ConfigError) ExitCode() int { return ExitConfig }
func (e *ConfigError) Unwrap() error { return e.Cause }

// Configf builds a ConfigError from a format string.
func Configf(format string, a ...any) *ConfigError {
	return &ConfigError{Message: fmt.Sprintf(format, a...)}
}

// ApiError marks a request the Digital X API rejected (ExitAPI). Code is the
// symbolic error string (the envelope's error.message); HTTPStatus is the
// envelope's error.code.
type ApiError struct {
	Message       string
	HTTPStatus    int
	Code          string          // symbolic code; "" when the body carried none
	RetryAfterSec *int            // set only when a Retry-After header was present
	Body          json.RawMessage // the raw error body, for diagnostics
	// Guidance is CLI-added recovery advice attached on the error path when the
	// symbolic code alone isn't enough to act safely — notably a duplicate place
	// whose order can't be read back ("the order IS placed; do NOT re-place,
	// fetch it"). It rides the error envelope (error.guidance) and the human
	// error line so an agent reading stdout/JSON only never has to parse a
	// free-floating stderr note. Empty for ordinary API rejections.
	Guidance string
}

func (e *ApiError) Error() string { return e.Message }
func (e *ApiError) ExitCode() int { return ExitAPI }
