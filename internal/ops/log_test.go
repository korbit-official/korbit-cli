// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// apiWithLog builds a test API whose operation-level decisions are captured into
// sink at the given threshold.
func apiWithLog(f *fakeClient, extra apiExtra, sink *strings.Builder, level slog.Level) *API {
	a := apiOver(f, extra)
	a.Log = logging.New(sink, level)
	return a
}

// TestPlaceLogsDecisionTrail asserts the place protocol logs the operation-level
// trail the wire layer cannot see: the start (with the clientOrderId that ties
// the lines together) and the accepted outcome with the resolved orderId.
func TestPlaceLogsDecisionTrail(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders", step{data: json.RawMessage(`{"orderId":987}`)})
	f.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	var sink strings.Builder
	a := apiWithLog(f, apiExtra{}, &sink, slog.LevelDebug)

	_, err := findOp(t, "order", "place").Run(context.Background(), a,
		runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	out := sink.String()
	for _, want := range []string{"order place: start", "clientOrderId=cid-1", "order place: accepted", "orderId=987"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the place log, got:\n%s", want, out)
		}
	}
	// price/qty ride the journal's params record, not the operational log.
	if strings.Contains(out, "price=") {
		t.Fatalf("order price must not reach the operational log, got:\n%s", out)
	}
}

// TestPlaceUnknownVerdictIsDebugNotWarn asserts the money-unsafe verdict (an
// ambiguous send that can't be read back) logs at Debug — NOT Warn — because the
// verdict already reaches the user as program output (the returned error). At the
// default (Error) the operational log stays silent so the terminal shows the error
// once, not a duplicate; at Debug the verdict joins the trail with its
// clientOrderId.
func TestPlaceUnknownVerdictIsDebugNotWarn(t *testing.T) {
	run := func(level slog.Level) (string, error) {
		f := newFakeClient()
		f.on("POST", "/v2/orders", step{err: errStr("timeout")})             // ambiguous transient
		f.on("GET", "/v2/orders", step{err: apiErr(404, "ORDER_NOT_FOUND")}) // not visible
		var sink strings.Builder
		a := apiWithLog(f, apiExtra{}, &sink, level)
		_, err := findOp(t, "order", "place").Run(context.Background(), a,
			runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
		return sink.String(), err
	}

	// At the default (Error): silent — the verdict rides program output, not the
	// log, so the ops layer emits nothing at the default level (no terminal dup).
	if out, err := run(slog.LevelError); out != "" {
		t.Fatalf("default-level ops log must be silent (no duplicate of the error), got:\n%s", out)
	} else if err == nil {
		t.Fatal("expected an UNKNOWN-placement error")
	}

	// At Warn: still silent — the verdict is Debug, not Warn (no terminal dup).
	if out, _ := run(slog.LevelWarn); strings.Contains(out, "UNKNOWN") {
		t.Fatalf("the UNKNOWN verdict must NOT log at Warn (it would duplicate the error), got:\n%s", out)
	}

	// At Debug: the verdict joins the trail with the clientOrderId to chase.
	out, _ := run(slog.LevelDebug)
	if !strings.Contains(out, "UNKNOWN") || !strings.Contains(out, "cid-1") {
		t.Fatalf("expected the UNKNOWN verdict in the Debug trail with its clientOrderId, got:\n%s", out)
	}
}

// TestPlaceTraceAddsPerSend asserts the per-iteration firehose (each send) lands
// only at Trace, keeping a normal Debug log free of per-attempt noise.
func TestPlaceTraceAddsPerSend(t *testing.T) {
	run := func(level slog.Level) string {
		f := newFakeClient()
		f.on("POST", "/v2/orders", step{data: json.RawMessage(`{"orderId":987}`)})
		f.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
		var sink strings.Builder
		a := apiWithLog(f, apiExtra{}, &sink, level)
		findOp(t, "order", "place").Run(context.Background(), a,
			runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "clientOrderId", "cid-1", "accountSeq", "1")))
		return sink.String()
	}
	if got := run(slog.LevelDebug); strings.Contains(got, "send accepted") {
		t.Fatalf("the per-send Trace line must NOT appear at Debug, got:\n%s", got)
	}
	if got := run(logging.LevelTrace); !strings.Contains(got, "send accepted") {
		t.Fatalf("the per-send line must appear at Trace, got:\n%s", got)
	}
}

// TestNilOpsLoggerSilent confirms the operations layer tolerates an unwired
// logger (the silent default) without panicking.
func TestNilOpsLoggerSilent(t *testing.T) {
	f := newFakeClient()
	f.on("POST", "/v2/orders", step{data: json.RawMessage(`{"orderId":987}`)})
	f.on("GET", "/v2/orders", step{data: json.RawMessage(placedOrderDoc)})
	a := apiOver(f, apiExtra{}) // Log left nil
	if _, err := findOp(t, "order", "place").Run(context.Background(), a,
		runInput(m("symbol", "btc_krw", "side", "buy", "orderType", "limit", "accountSeq", "1"))); err != nil {
		t.Fatalf("place with nil ops logger: %v", err)
	}
}
