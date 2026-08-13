// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package jqfilter compiles a jq program (github.com/itchyny/gojq) and runs it
// over a stream of JSON documents to filter and/or transform them. It is the
// engine behind the monitor command's --jq flag.
//
// Number fidelity is the load-bearing property. Korbit money/quantity values
// are decimal strings on the wire, and gojq passes strings through untouched.
// Numeric JSON values are decoded as json.Number (UseNumber) and gojq preserves
// json.Number verbatim for any operation that does not do arithmetic on that
// value (select/identity/field access/comparison) — so a pure filter never
// rounds a number through float64. Precision is lost only where the program
// itself does number math (tonumber, +, *, …), which is the caller's explicit
// choice. TestGojqPreservesJSONNumber pins this library behavior.
package jqfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/itchyny/gojq"
)

// Program is a compiled jq program. A compiled gojq.Code is stateless across
// runs, so a Program is safe to Run repeatedly.
type Program struct {
	code *gojq.Code
	src  string
}

// Source returns the original program text (for the --dry-run plan).
func (p *Program) Source() string { return p.src }

// Compile parses and compiles src. The returned error is user-facing — it names
// the jq syntax/compile problem — so a caller can surface it as a usage error.
func Compile(src string) (*Program, error) {
	q, err := gojq.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("--jq is not a valid jq program: %w", err)
	}
	code, err := gojq.Compile(q)
	if err != nil {
		return nil, fmt.Errorf("--jq is not a valid jq program: %w", err)
	}
	return &Program{code: code, src: src}, nil
}

// Run executes the program against one input JSON document and returns the
// lines to emit, in order:
//
//   - Each value the program produces becomes one output line. A program that
//     produces no values (e.g. select(false)) returns an empty slice — the
//     document is filtered out.
//   - When an output value is deep-equal to the input (a select-style filter
//     that passed, or identity), the ORIGINAL input bytes are returned verbatim
//     — preserving exact decimal strings and the input's object key order.
//     Only a transformed value is re-encoded by gojq (whose encoder is faithful
//     to json.Number but sorts object keys).
//
// A jq runtime error (e.g. tonumber on a non-numeric string) is returned with
// no output lines; the caller decides whether to skip the document (the monitor
// does, rate-limited, like a --where exception). A jq `halt`/`halt_error` ends
// output for this document without an error — the lines produced before it are
// returned. The context bounds a runaway program so Ctrl-C / --duration can
// interrupt it.
//
// The verbatim line returned for a passing filter ALIASES input — it is not
// copied. Callers must emit it before reusing the input buffer.
func (p *Program) Run(ctx context.Context, input []byte) ([][]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("jqfilter: input is not valid JSON: %w", err)
	}

	var out [][]byte
	iter := p.code.RunWithContext(ctx, v)
	for {
		r, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := r.(error); ok {
			// halt / halt_error deliberately ends the program; in a per-event
			// stream that just ends output for this document — keep the lines
			// already produced and stop, without flagging an error.
			var he *gojq.HaltError
			if errors.As(err, &he) {
				break
			}
			return nil, fmt.Errorf("--jq evaluation error: %s", err)
		}
		if reflect.DeepEqual(r, v) {
			// A filter that passed (or identity): emit the original document
			// byte-for-byte — exact money strings, original key order.
			out = append(out, input)
			continue
		}
		b, err := gojq.Marshal(r)
		if err != nil {
			return out, fmt.Errorf("jqfilter: cannot encode jq output: %w", err)
		}
		out = append(out, b)
	}
	return out, nil
}
