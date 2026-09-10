// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// Doer is the minimal wire surface the typed layer drives: one logical call
// executed under a policy. *apiclient.Client satisfies it; an interface here keeps
// the typed layer testable with a scripted fake and documents that it needs
// nothing more than Do.
type Doer interface {
	Do(ctx context.Context, call apiclient.Call, pol apiclient.Policy) (json.RawMessage, apiclient.Meta, error)
}

// Client is the typed endpoint layer over a wire client. It is configured once
// (with a Doer, an *apiclient.Client in production) and reused; safety for
// concurrent use matches the wrapped client.
type Client struct {
	wire Doer
	log  *slog.Logger
}

// New wraps a wire client in the typed endpoint layer. log is the optional
// operational logger (nil = silent): the typed layer's one diagnostic is a
// typed-decode mismatch — the verbatim bytes did not fit the endpoint's typed
// view — logged at Debug so a --log-level debug run explains why a typed field
// (e.g. an order's id) came back empty even though the call succeeded. The wire
// layer below already logs the request target/status/timing; the typed layer
// adds nothing per call on the happy path. It never sees a secret (the signature
// is appended below it) and never logs the response body.
func New(wire Doer, log *slog.Logger) *Client {
	return &Client{wire: wire, log: logging.Or(log)}
}

// params accumulates ordered wire parameters in append order. Order is
// load-bearing for signing: the wrapped client signs the exact encoded string
// it sends, so positionals are appended before the remaining parameters and an
// absent optional parameter is skipped.
type params struct {
	kv []apiclient.KV
}

// str appends a required string-valued parameter.
func (p *params) str(key, value string) {
	p.kv = append(p.kv, apiclient.KV{Key: key, Value: value})
}

// strPtr appends an optional string-valued parameter only when set.
func (p *params) strPtr(key string, value *string) {
	if value != nil {
		p.kv = append(p.kv, apiclient.KV{Key: key, Value: *value})
	}
}

// intVal appends a required int-valued parameter, formatted as a string.
func (p *params) intVal(key string, value int) {
	p.kv = append(p.kv, apiclient.KV{Key: key, Value: strconv.Itoa(value)})
}

// intPtr appends an optional int-valued parameter only when set.
func (p *params) intPtr(key string, value *int) {
	if value != nil {
		p.kv = append(p.kv, apiclient.KV{Key: key, Value: strconv.Itoa(*value)})
	}
}

// boolFlag appends an optional boolean parameter only when set, encoding it as
// "true"/"false".
func (p *params) boolFlag(key string, value *bool) {
	if value != nil {
		p.kv = append(p.kv, apiclient.KV{Key: key, Value: strconv.FormatBool(*value)})
	}
}

// call is the shared chokepoint: it issues exactly one wire call under the
// supplied policy and, on success, decodes the verbatim response bytes into the
// typed value T (lenient — unknown fields are ignored). It returns the typed
// value, the verbatim bytes, the call metadata, and the error.
//
// The verbatim bytes are AUTHORITATIVE; the typed value is a best-effort
// convenience. A wire error leaves T at its zero value and returns the error. A
// decode mismatch (the response did not fit T's shape) is NOT treated as a call
// failure: T is left zero and the verbatim bytes are returned with a nil error,
// since the bytes are the result a caller passes through and the API owns the
// response shape. A caller that needs the typed view checks it against the
// bytes; one that only forwards the bytes is unaffected by a shape it never
// reads.
func call[T any](c *Client, ctx context.Context, method, path string, auth bool, p params, pol apiclient.Policy) (T, json.RawMessage, apiclient.Meta, error) {
	var typed T
	wireCall := apiclient.Call{Method: method, Path: path, Params: p.kv, Auth: auth}
	raw, meta, err := c.wire.Do(ctx, wireCall, pol)
	if err != nil {
		return typed, raw, meta, err
	}
	if len(raw) > 0 {
		// A decode mismatch leaves typed at its zero value; the verbatim bytes are
		// still returned (they are the authoritative result). It is not a call
		// failure, but it IS the typed layer's one diagnostic: the response did not
		// fit the shape this endpoint expects, so any typed-field read above (an
		// orderId, a status) will be empty. Surface it at Debug — method/path and
		// the decoder's own message (which names the offending Go field/type, never
		// the response body) — so a --log-level debug run can explain a silently
		// empty typed view.
		if derr := json.Unmarshal(raw, &typed); derr != nil {
			c.log.Debug("typed decode mismatch",
				"method", method, "path", path, "err", derr.Error())
		}
	}
	return typed, raw, meta, nil
}
