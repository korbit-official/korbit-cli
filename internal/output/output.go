// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package output owns the CLI's stdout/stderr contract. On success it emits the
// command result on stdout — by default human-readable (via EmitText, formatted
// by the cli package), or, when the caller opts in with --json/--compact, exactly
// one JSON document (via EmitJSON, a json.RawMessage passthrough preserving the
// API's own field order). On failure it writes the error to stderr — a
// human-readable line by default, or the structured {"error": ...} object under
// --json — and owns the error -> exit-code map (the classification, and thus the
// exit code, is identical in both modes). This package stays generic: it knows
// JSON and raw text, never command-specific formatting.
package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ExitCodes documents every exit code, surfaced in help and the `commands`
// catalog so agents can map a non-zero status to a cause. Keyed by the decimal
// status (from the Exit* taxonomy, never a bare literal). The {prog} token is
// substituted with the program's invoked name by the render/catalog layer.
var ExitCodes = map[string]string{
	strconv.Itoa(ExitSuccess):  "success",
	strconv.Itoa(ExitInternal): "network failure or internal error",
	strconv.Itoa(ExitUsage):    "usage error — the command line is invalid (fix the invocation)",
	strconv.Itoa(ExitAPI):      "the Digital X API rejected the request (see error.code)",
	strconv.Itoa(ExitConfig):   "key or configuration problem — fix with `{prog} key ...` or config.json",
}

// IO bundles the streams a command writes to, so the whole CLI can run in-process
// under test against buffers.
type IO struct {
	Out io.Writer
	Err io.Writer
}

// Note writes a human diagnostic line to stderr. It is never part of the stdout
// JSON contract.
func (io IO) Note(msg string) {
	fmt.Fprintln(io.Err, msg)
}

// Notef is Note with formatting.
func (io IO) Notef(format string, a ...any) {
	fmt.Fprintf(io.Err, format+"\n", a...)
}

// EmitText writes a block of human-readable text to stdout, followed by a single
// trailing newline. It is the stdout sink for the human-readable output mode; the
// text itself is formatted by the cli package, which knows the command. The wire
// (JSON) contract is unaffected — see EmitJSON.
func (io IO) EmitText(s string) error {
	_, err := io.Out.Write(append([]byte(s), '\n'))
	return err
}

// EmitJSON writes value as the single stdout document. A json.RawMessage is
// reproduced verbatim (preserving the API's own field order); any other value
// is marshalled without HTML escaping. compact selects single-line output.
func (io IO) EmitJSON(value any, compact bool) error {
	raw, err := toRaw(value)
	if err != nil {
		return err
	}
	formatted, err := format(raw, compact)
	if err != nil {
		return err
	}
	_, err = io.Out.Write(append(formatted, '\n'))
	return err
}

// EmitError classifies err and writes it to stderr — a human-readable line by
// default, or the structured {"error": ...} envelope when jsonMode is set
// (compact controls single-line vs pretty for the envelope) — then returns the
// process exit code. The classification, and thus the exit code, is identical in
// both modes; only the rendering differs.
func (io IO) EmitError(err error, jsonMode, compact bool) int {
	var (
		usage  *UsageError
		config *ConfigError
		api    *ApiError
	)
	switch {
	case errors.As(err, &usage):
		io.emitErr(jsonMode, compact, usagePayload{Type: "usage", Message: usage.Message}, usage.Message)
		return ExitUsage
	case errors.As(err, &config):
		io.emitErr(jsonMode, compact, usagePayload{Type: "config", Message: config.Message}, config.Message)
		return ExitConfig
	case errors.As(err, &api):
		p := apiPayload{
			Type:          "api",
			Message:       api.Message,
			HTTPStatus:    api.HTTPStatus,
			RetryAfterSec: api.RetryAfterSec,
		}
		if api.Code != "" {
			p.Code = &api.Code
		}
		if len(api.Body) > 0 {
			p.Body = api.Body
		}
		if api.Guidance != "" {
			p.Guidance = api.Guidance
		}
		io.emitErr(jsonMode, compact, p, humanAPIError(api))
		return ExitAPI
	default:
		io.emitErr(jsonMode, compact, usagePayload{Type: "internal", Message: err.Error()}, err.Error())
		return ExitInternal
	}
}

// emitErr renders one classified error to stderr: the JSON envelope when jsonMode
// is set, otherwise a single human-readable line. The human line carries no
// program/log tag — unlike the operational logger and stderr notices (which mark
// out-of-band diagnostics with "<prog>: …"), this IS the direct outcome of
// the command the user ran, so it reads as a plain "error: <message>".
func (io IO) emitErr(jsonMode, compact bool, payload any, humanMsg string) {
	if jsonMode {
		io.writeError(payload, compact)
		return
	}
	fmt.Fprintf(io.Err, "error: %s\n", humanMsg)
}

// humanAPIError renders an API rejection for human mode: the message plus the
// symbolic code / HTTP status / retry hint the JSON envelope carries, so a human
// loses nothing by not passing --json.
func humanAPIError(api *ApiError) string {
	msg := api.Message
	if msg == "" {
		// The wire layer always sets a message; guard the degenerate case so the
		// human line is never a bare "error: ".
		msg = "API request failed"
	}
	var extra []string
	if api.Code != "" {
		extra = append(extra, api.Code)
	}
	if api.HTTPStatus != 0 {
		extra = append(extra, fmt.Sprintf("HTTP %d", api.HTTPStatus))
	}
	if len(extra) > 0 {
		msg += " (" + strings.Join(extra, ", ") + ")"
	}
	if api.RetryAfterSec != nil {
		msg += fmt.Sprintf(" — retry after %ds", *api.RetryAfterSec)
	}
	if api.Guidance != "" {
		msg += " — " + api.Guidance
	}
	return msg
}

// usagePayload is the error shape for usage/config/internal errors (no API
// fields). Field order is the documented one.
type usagePayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// apiPayload is the error shape for API rejections. code is null when the body
// carried no symbolic code; retryAfterSec, body, and guidance appear only when
// present. guidance is CLI-added recovery advice (see ApiError.Guidance).
type apiPayload struct {
	Type          string          `json:"type"`
	Message       string          `json:"message"`
	HTTPStatus    int             `json:"httpStatus"`
	Code          *string         `json:"code"`
	RetryAfterSec *int            `json:"retryAfterSec,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
	Guidance      string          `json:"guidance,omitempty"`
}

func (io IO) writeError(payload any, compact bool) {
	raw, err := toRaw(struct {
		Error any `json:"error"`
	}{payload})
	if err != nil {
		// A structured error must still surface; fall back to a minimal object.
		fmt.Fprintf(io.Err, `{"error":{"type":"internal","message":%q}}`+"\n", err.Error())
		return
	}
	formatted, _ := format(raw, compact)
	// Best-effort: this is the diagnostic stderr path; if even stderr is gone
	// there is nowhere left to report the failure.
	_, _ = io.Err.Write(append(formatted, '\n'))
}

// toRaw renders value to compact JSON bytes: RawMessage verbatim, everything
// else marshalled with HTML escaping off (so &, <, > survive as written, like
// JSON.stringify).
func toRaw(value any) (json.RawMessage, error) {
	if rm, ok := value.(json.RawMessage); ok {
		return rm, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// format compacts or 2-space indents raw JSON without re-escaping it.
func format(raw json.RawMessage, compact bool) ([]byte, error) {
	var buf bytes.Buffer
	if compact {
		if err := json.Compact(&buf, raw); err != nil {
			return nil, err
		}
	} else if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
