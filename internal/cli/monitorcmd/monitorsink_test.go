// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package monitorcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/jqfilter"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/stream"
)

// These exercise the sinks in isolation — no WebSocket session, no goja — which
// is the point of the monitorSink split. botSink needs a real botapi runtime
// and stays covered end-to-end by the TestMonitorOn* tests in monitorcmd_test.go.

func newTestSinkParts(maxEvents int) (monitorPrinter, *emitter, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := monitorPrinter{io: output.IO{Out: &out, Err: &errb}, log: discard, jsonMode: true}
	ctx, cancel := context.WithCancel(context.Background())
	em := &emitter{
		maxEvents: maxEvents,
		ctx:       ctx,
		cancel:    cancel,
		warn:      newRateLimiter(predicateWarnIntervalMs),
		log:       slog.New(slog.NewTextHandler(&errb, &slog.HandlerOptions{Level: slog.LevelWarn})),
		now:       func() int64 { return 0 },
	}
	return p, em, &out, &errb
}

func dataEvent(close string) stream.Data {
	return stream.Data{
		Channel: "ticker", Symbol: "btc_krw", Origin: stream.Origin("realtime"), ServerTime: 1,
		Payload: json.RawMessage(`{"data":{"close":"` + close + `"}}`),
	}
}

func noticeEvent() stream.Notice {
	return stream.Notice{Code: stream.NoticeCode("CONNECTED"), Level: stream.Level("info"), Message: "connected", Time: 1}
}

func outLines(buf *bytes.Buffer) []string {
	s := strings.TrimSpace(buf.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestPlainSinkCountsDataNotNotices(t *testing.T) {
	p, em, out, _ := newTestSinkParts(2)
	s := &plainSink{p: p, em: em}

	s.onNotice(noticeEvent()) // always printed, never counted
	s.onData(dataEvent("100"))
	s.onData(dataEvent("200")) // hits the cap -> stop()
	s.onData(dataEvent("300")) // stopped -> dropped
	s.onNotice(noticeEvent())  // notices still narrate after the cap

	lines := outLines(out)
	var data, notice int
	for _, l := range lines {
		switch {
		case strings.Contains(l, `"type":"data"`):
			data++
		case strings.Contains(l, `"type":"notice"`):
			notice++
		}
	}
	if data != 2 {
		t.Errorf("data lines = %d, want 2 (capped at --max-events)", data)
	}
	if notice != 2 {
		t.Errorf("notice lines = %d, want 2 (notices bypass the cap)", notice)
	}
	if em.ctx.Err() == nil {
		t.Errorf("reaching the cap should have canceled the context")
	}
}

func TestJqSinkFilterPassesVerbatimAndDropsNotices(t *testing.T) {
	prog, err := jqfilter.Compile(`select(.type=="data" and .channel=="ticker")`)
	if err != nil {
		t.Fatal(err)
	}
	p, em, out, _ := newTestSinkParts(0)
	s := &jqSink{p: p, em: em, prog: prog}

	s.onNotice(noticeEvent())  // dropped by the data-only filter
	s.onData(dataEvent("100")) // passes -> emitted verbatim

	lines := outLines(out)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (notice dropped, data kept): %q", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"channel":"ticker"`) || !strings.Contains(lines[0], `"close":"100"`) {
		t.Errorf("data line not emitted verbatim: %s", lines[0])
	}
	if strings.Contains(out.String(), `"type":"notice"`) {
		t.Errorf("a notice leaked through a data-only --jq filter")
	}
}

func TestJqSinkTransformPreservesMoneyString(t *testing.T) {
	prog, err := jqfilter.Compile(`select(.type=="data") | {px: .payload.data.close}`)
	if err != nil {
		t.Fatal(err)
	}
	p, em, out, _ := newTestSinkParts(0)
	s := &jqSink{p: p, em: em, prog: prog}

	s.onData(dataEvent("139000000.12345678"))

	lines := outLines(out)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), out.String())
	}
	var got map[string]any
	dec := json.NewDecoder(strings.NewReader(lines[0]))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if got["px"] != "139000000.12345678" {
		t.Errorf("px = %v (%T), want exact money string", got["px"], got["px"])
	}
}

func TestJqSinkRuntimeErrorWarnsAndSkips(t *testing.T) {
	// No .type guard, so tonumber runs on the notice (.payload is null) -> a
	// per-event evaluation error: warn (rate-limited) + skip, never a crash.
	prog, err := jqfilter.Compile(`select((.payload.data.close|tonumber) > 0)`)
	if err != nil {
		t.Fatal(err)
	}
	p, em, out, errb := newTestSinkParts(0)
	s := &jqSink{p: p, em: em, prog: prog}

	s.onNotice(noticeEvent())  // errors on tonumber(null) -> skipped + warned
	s.onData(dataEvent("100")) // passes

	if lines := outLines(out); len(lines) != 1 || !strings.Contains(lines[0], `"close":"100"`) {
		t.Fatalf("want only the data line, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "event skipped") {
		t.Errorf("expected a rate-limited skip warning, got %q", errb.String())
	}
}

func TestJqSinkCapStops(t *testing.T) {
	prog, err := jqfilter.Compile(`.`) // identity: every line passes
	if err != nil {
		t.Fatal(err)
	}
	p, em, out, _ := newTestSinkParts(1)
	s := &jqSink{p: p, em: em, prog: prog}

	s.onData(dataEvent("100"))
	s.onData(dataEvent("200")) // stopped -> dropped

	if lines := outLines(out); len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (capped): %q", len(lines), out.String())
	}
	if em.ctx.Err() == nil {
		t.Errorf("reaching the cap should have canceled the context")
	}
}
