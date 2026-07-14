// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dop251/goja"
	"github.com/korbit-official/korbit-cli/internal/output"
)

// jsError converts a Go error into the JS value a korbit.*/db.* promise
// rejects with: a real Error instance (so `instanceof Error` holds and stack
// traces exist), carrying the structured fields the docs promise for an API
// rejection — .code (symbolic), .httpStatus, .description, .body — so
// `catch (e) { if (e.code === "INSUFFICIENT_BALANCE") ... }` works.
// Must run on the loop goroutine.
func jsError(vm *goja.Runtime, err error) goja.Value {
	obj := newJSError(vm, err.Error())
	var ae *output.ApiError
	if errors.As(err, &ae) {
		_ = obj.Set("code", ae.Code)
		_ = obj.Set("httpStatus", ae.HTTPStatus)
		_ = obj.Set("description", ae.Message)
		if ae.RetryAfterSec != nil {
			_ = obj.Set("retryAfterSec", *ae.RetryAfterSec)
		}
		if len(ae.Body) > 0 {
			if body, perr := parseJSON(vm, ae.Body); perr == nil {
				_ = obj.Set("body", body)
			} else {
				_ = obj.Set("body", string(ae.Body))
			}
		}
	}
	return obj
}

// newJSError constructs `new Error(message)`.
func newJSError(vm *goja.Runtime, message string) *goja.Object {
	ctor, ok := vm.Get("Error").(*goja.Object)
	if ok {
		if obj, err := vm.New(ctor, vm.ToValue(message)); err == nil {
			return obj
		}
	}
	// Unreachable in practice; an object with a message is still useful.
	obj := vm.NewObject()
	_ = obj.Set("message", message)
	return obj
}

// errorFromJS converts a JS rejection reason (or thrown value) back into a Go
// error, reconstructing an output.ApiError when the value carries the API
// error shape — that keeps the exit-code contract (API rejection -> 3) across
// the JS boundary.
func errorFromJS(v goja.Value) error {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return errors.New("unknown script error")
	}
	if obj, ok := v.(*goja.Object); ok {
		status := 0
		if hs := obj.Get("httpStatus"); hs != nil && !goja.IsUndefined(hs) {
			status = int(hs.ToInteger())
		}
		if status > 0 {
			ae := &output.ApiError{Message: messageOf(obj, v), HTTPStatus: status}
			if c := obj.Get("code"); c != nil && !goja.IsUndefined(c) {
				ae.Code = c.String()
			}
			return ae
		}
		return errors.New(messageOf(obj, v))
	}
	return errors.New(v.String())
}

// messageOf prefers an Error-like value's .message over its String() (which
// includes the "Error: " prefix), falling back to the rendered value.
func messageOf(obj *goja.Object, v goja.Value) string {
	if m := obj.Get("message"); m != nil && !goja.IsUndefined(m) {
		if s := m.String(); s != "" {
			return s
		}
	}
	return v.String()
}

// parseJSON converts a raw JSON document to a JS value through the engine's
// own JSON.parse, so scripts see plain objects and decimal strings stay
// strings. Must run on the loop goroutine.
func parseJSON(vm *goja.Runtime, raw json.RawMessage) (goja.Value, error) {
	jsonObj, ok := vm.Get("JSON").(*goja.Object)
	if !ok {
		return nil, errors.New("internal: JSON object missing")
	}
	parse, ok := goja.AssertFunction(jsonObj.Get("parse"))
	if !ok {
		return nil, errors.New("internal: JSON.parse missing")
	}
	v, err := parse(goja.Undefined(), vm.ToValue(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("response is not valid JSON: %w", err)
	}
	return v, nil
}
