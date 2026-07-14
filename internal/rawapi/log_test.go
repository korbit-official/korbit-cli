// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// fakeWire is a minimal rawapi.Doer that returns canned bytes, so a test can
// drive the typed layer's decode path without a real wire client.
type fakeWire struct {
	raw json.RawMessage
	err error
}

func (f fakeWire) Do(context.Context, korbit.Call, korbit.Policy) (json.RawMessage, korbit.Meta, error) {
	return f.raw, korbit.Meta{Attempts: 1}, f.err
}

// TestTypedDecodeMismatchLogged asserts the typed layer's one diagnostic: when
// the verbatim bytes do not fit the endpoint's typed shape, a Debug line names
// the method+path so a --log-level debug run can explain a silently empty typed
// view. The verbatim bytes are still returned (authoritative) with no error.
func TestTypedDecodeMismatchLogged(t *testing.T) {
	var sink strings.Builder
	log := logging.New(&sink, slog.LevelDebug)

	// /v2/currencyPairs decodes into []Pair; an object cannot fit a slice, so the
	// decode fails and the verbatim bytes flow through unchanged.
	body := json.RawMessage(`{"shape":"unexpected-object"}`)
	c := New(fakeWire{raw: body}, log)

	pairs, raw, _, err := c.Pairs(context.Background(), PairsRequest{}, korbit.Policy{})
	if err != nil {
		t.Fatalf("a decode mismatch must NOT be a call failure: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("the typed view should be the zero value on a mismatch, got %v", pairs)
	}
	if string(raw) != string(body) {
		t.Fatalf("the verbatim bytes must pass through untouched, got %s", raw)
	}

	out := sink.String()
	if !strings.Contains(out, "typed decode mismatch") {
		t.Fatalf("expected a decode-mismatch log line, got: %q", out)
	}
	if !strings.Contains(out, "/v2/currencyPairs") {
		t.Fatalf("the log line must name the path, got: %q", out)
	}
	// The response body must NOT be in the log — only method/path and the
	// decoder's own (type-naming) message.
	if strings.Contains(out, "unexpected-object") {
		t.Fatalf("the response body must never reach the log, got: %q", out)
	}
}

// TestTypedDecodeMatchSilent asserts a well-formed response logs nothing — the
// happy path adds no per-call rawapi line (the wire layer below already logs the
// request target/status/timing).
func TestTypedDecodeMatchSilent(t *testing.T) {
	var sink strings.Builder
	log := logging.New(&sink, logging.LevelTrace) // capture everything

	c := New(fakeWire{raw: json.RawMessage(`[]`)}, log) // empty array fits []Pair
	if _, _, _, err := c.Pairs(context.Background(), PairsRequest{}, korbit.Policy{}); err != nil {
		t.Fatalf("Pairs: %v", err)
	}
	if out := sink.String(); out != "" {
		t.Fatalf("a clean decode must log nothing from the typed layer, got: %q", out)
	}
}

// TestNilLoggerSilent confirms New tolerates a nil logger (the silent default).
func TestNilLoggerSilent(t *testing.T) {
	c := New(fakeWire{raw: json.RawMessage(`{"x":1}`)}, nil)
	if _, _, _, err := c.Pairs(context.Background(), PairsRequest{}, korbit.Policy{}); err != nil {
		t.Fatalf("Pairs with nil logger: %v", err)
	}
}
