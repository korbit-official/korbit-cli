// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func newIO() (*bytes.Buffer, *bytes.Buffer, IO) {
	var out, err bytes.Buffer
	return &out, &err, IO{Out: &out, Err: &err}
}

func TestEmitJSONPreservesRawOrder(t *testing.T) {
	out, _, io := newIO()
	// A RawMessage with non-alphabetical keys must be reproduced verbatim
	// (field order preserved), not re-sorted.
	raw := json.RawMessage(`{"price":"100","amount":"1","symbol":"btc_krw"}`)
	if err := io.EmitJSON(raw, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `{"price":"100","amount":"1","symbol":"btc_krw"}` {
		t.Fatalf("compact raw passthrough changed: %q", got)
	}
}

func TestEmitJSONPrettyAndCompact(t *testing.T) {
	out, _, io := newIO()
	io.EmitJSON(map[string]string{"a": "b"}, false)
	if !strings.Contains(out.String(), "{\n  \"a\": \"b\"\n}") {
		t.Fatalf("pretty output unexpected: %q", out.String())
	}
}

func TestEmitJSONNoHTMLEscape(t *testing.T) {
	out, _, io := newIO()
	io.EmitJSON(map[string]string{"u": "a&b<c>"}, true)
	if !strings.Contains(out.String(), "a&b<c>") {
		t.Fatalf("HTML chars should not be escaped: %q", out.String())
	}
}

func TestEmitErrorExitCodesAndShapes(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantType string
	}{
		{"usage", &UsageError{Message: "bad flag"}, 2, "usage"},
		{"config", &ConfigError{Message: "no key"}, 4, "config"},
		{"api", &ApiError{Message: "dup", HTTPStatus: 422, Code: "DUPLICATE_CLIENT_ORDER_ID"}, 3, "api"},
		{"internal", errStr("boom"), 1, "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errBuf, io := newIO()
			code := io.EmitError(tt.err, true, true)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tt.wantCode)
			}
			var doc struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(errBuf.Bytes(), &doc); err != nil {
				t.Fatalf("stderr not valid json: %v (%s)", err, errBuf.String())
			}
			if doc.Error["type"] != tt.wantType {
				t.Fatalf("type = %v, want %v", doc.Error["type"], tt.wantType)
			}
		})
	}
}

func TestEmitErrorAPIIncludesStatusAndRetry(t *testing.T) {
	_, errBuf, io := newIO()
	ra := 30
	io.EmitError(&ApiError{Message: "rate", HTTPStatus: 429, Code: "TOO_MANY_REQUESTS", RetryAfterSec: &ra}, true, true)
	s := errBuf.String()
	if !strings.Contains(s, `"httpStatus":429`) || !strings.Contains(s, `"retryAfterSec":30`) {
		t.Fatalf("missing api fields: %s", s)
	}
}

// In human mode (no --json) an error is a plain "error: <message>" line on
// stderr — NOT the JSON envelope, and with no operational-log tag (it is the
// direct outcome of the command, not an out-of-band diagnostic). Exit codes are
// unchanged from JSON mode. An API error still surfaces its symbolic code, HTTP
// status, and retry hint inline so a human loses nothing by omitting --json.
func TestEmitErrorHumanMode(t *testing.T) {
	t.Run("usage", func(t *testing.T) {
		_, errBuf, io := newIO()
		code := io.EmitError(&UsageError{Message: "needs a subcommand"}, false, false)
		if code != 2 {
			t.Fatalf("exit=%d, want 2", code)
		}
		got := strings.TrimSpace(errBuf.String())
		if got != "error: needs a subcommand" {
			t.Fatalf("human usage error = %q", got)
		}
		if strings.Contains(got, "{") || strings.Contains(got, "korbit-cli") {
			t.Fatalf("human error must be plain, untagged: %q", got)
		}
	})
	t.Run("api", func(t *testing.T) {
		_, errBuf, io := newIO()
		ra := 30
		code := io.EmitError(&ApiError{Message: "rate limited", HTTPStatus: 429, Code: "TOO_MANY_REQUESTS", RetryAfterSec: &ra}, false, false)
		if code != 3 {
			t.Fatalf("exit=%d, want 3", code)
		}
		got := strings.TrimSpace(errBuf.String())
		want := "error: rate limited (TOO_MANY_REQUESTS, HTTP 429) — retry after 30s"
		if got != want {
			t.Fatalf("human api error =\n %q\nwant\n %q", got, want)
		}
	})
}

// The error envelope is a public contract. These pin the EXACT compact bytes for
// each class — field SET, field ORDER, and the presence/nullability rules — so a
// struct-tag rename, a reordered field, or a flipped omitempty/null rule fails a
// test rather than silently breaking a consumer's error handling.
func TestErrorEnvelopeExactShape(t *testing.T) {
	body := json.RawMessage(`{"success":false}`)
	ra := 30
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			"usage",
			&UsageError{Message: "bad flag"},
			`{"error":{"type":"usage","message":"bad flag"}}`,
		},
		{
			"config",
			&ConfigError{Message: "no key"},
			`{"error":{"type":"config","message":"no key"}}`,
		},
		{
			"internal",
			errStr("boom"),
			`{"error":{"type":"internal","message":"boom"}}`,
		},
		{
			// API error with a symbolic code, no Retry-After, no body: code is the
			// string and retryAfterSec/body are omitted.
			"api with code",
			&ApiError{Message: "dup", HTTPStatus: 422, Code: "DUPLICATE_CLIENT_ORDER_ID"},
			`{"error":{"type":"api","message":"dup","httpStatus":422,"code":"DUPLICATE_CLIENT_ORDER_ID"}}`,
		},
		{
			// No symbolic code: code is JSON null (present, not omitted) so a
			// consumer can always read error.code.
			"api without code",
			&ApiError{Message: "boom", HTTPStatus: 500},
			`{"error":{"type":"api","message":"boom","httpStatus":500,"code":null}}`,
		},
		{
			// Full API error: retryAfterSec and body appear when present, in order.
			"api full",
			&ApiError{Message: "rate", HTTPStatus: 429, Code: "TOO_MANY_REQUESTS", RetryAfterSec: &ra, Body: body},
			`{"error":{"type":"api","message":"rate","httpStatus":429,"code":"TOO_MANY_REQUESTS","retryAfterSec":30,"body":{"success":false}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errBuf, io := newIO()
			io.EmitError(tt.err, true, true) // jsonMode + compact: exact bytes
			if got := strings.TrimSpace(errBuf.String()); got != tt.want {
				t.Fatalf("envelope mismatch:\n got=%s\nwant=%s", got, tt.want)
			}
		})
	}
}

// Under --json the error envelope is JSON on stderr — compact only when
// --compact is also set, pretty otherwise (plain --json). This pins that the
// pretty form is valid JSON with the same field set.
func TestErrorEnvelopePrettyIsJSON(t *testing.T) {
	_, errBuf, io := newIO()
	io.EmitError(&ApiError{Message: "dup", HTTPStatus: 422, Code: "X"}, true, false)
	var doc struct {
		Error struct {
			Type       string  `json:"type"`
			Message    string  `json:"message"`
			HTTPStatus int     `json:"httpStatus"`
			Code       *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errBuf.Bytes(), &doc); err != nil {
		t.Fatalf("pretty error not valid json: %v (%s)", err, errBuf.String())
	}
	if doc.Error.Type != "api" || doc.Error.HTTPStatus != 422 || doc.Error.Code == nil || *doc.Error.Code != "X" {
		t.Fatalf("pretty envelope fields wrong: %+v", doc.Error)
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }
