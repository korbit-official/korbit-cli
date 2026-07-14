// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

// stream.LogNotice logs each notice at its own level, so the logger's threshold
// (--stream-log-level, off by default) decides what lands: at warn an info
// notice is dropped while warn and error notices print with a greppable code=
// attribute.
func TestMirrorNoticeThreshold(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelWarn)
	stream.LogNotice(log, stream.Notice{Code: stream.Connected, Level: stream.LevelInfo, Message: "up"})
	stream.LogNotice(log, stream.Notice{Code: stream.Disconnected, Level: stream.LevelWarn, Message: "dropped"})
	stream.LogNotice(log, stream.Notice{Code: stream.DataGap, Level: stream.LevelError, Message: "gap"})

	out := buf.String()
	if strings.Contains(out, "code=CONNECTED") {
		t.Errorf("info notice should be below the warn threshold; got %q", out)
	}
	if !strings.Contains(out, "code=DISCONNECTED") || !strings.Contains(out, "code=DATA_GAP") {
		t.Errorf("warn+error notices should appear with their code; got %q", out)
	}
}

// Independent loggers (separate logging.New, hence separate handler mutexes)
// sharing one syncWriter must still produce whole, un-interleaved lines — the
// guarantee the monitor relies on, where the operational, stream, and notice
// loggers all write concurrently to rt.logW. Cross-logger atomicity comes from
// the syncWriter (one locked Write per record), not the per-handler mutex.
func TestSyncWriterSerializesIndependentLoggers(t *testing.T) {
	var buf bytes.Buffer
	sink := &syncWriter{w: &buf}
	streamL := logging.New(sink, slog.LevelInfo).With("component", "stream")
	opL := logging.New(sink, slog.LevelInfo)

	const n = 300
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); streamL.Info("stream line", "k", "v") }()
		go func() { defer wg.Done(); opL.Warn("op line") }()
	}
	wg.Wait()

	wantStream := "korbit-cli: info: stream line component=stream k=v"
	wantOp := "korbit-cli: warn: op line"
	var ns, no int
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		switch l {
		case wantStream:
			ns++
		case wantOp:
			no++
		default:
			t.Fatalf("interleaved/garbled line: %q", l)
		}
	}
	if ns != n || no != n {
		t.Fatalf("want %d of each line, got stream=%d op=%d", n, ns, no)
	}
}

// The same cross-logger guarantee must hold under --log-format json: each
// independently constructed JSON logger has its OWN slog mutex, so only the
// shared syncWriter (one locked Write per record) keeps their objects whole.
// Every emitted line must parse as a complete JSON object — no interleaving.
func TestSyncWriterSerializesIndependentLoggersJSON(t *testing.T) {
	var buf bytes.Buffer
	sink := &syncWriter{w: &buf}
	st := logging.Style{Format: logging.FormatJSON}
	streamL := logging.NewStyled(sink, slog.LevelInfo, st).With("component", "stream")
	opL := logging.NewStyled(sink, slog.LevelInfo, st)

	const n = 300
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); streamL.Info("stream line", "k", "v") }()
		go func() { defer wg.Done(); opL.Warn("op line") }()
	}
	wg.Wait()

	var ns, no int
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("interleaved/garbled JSON line: %v (line=%q)", err, l)
		}
		switch rec["msg"] {
		case "stream line":
			if rec["component"] != "stream" || rec["level"] != "info" {
				t.Fatalf("bad stream record: %#v", rec)
			}
			ns++
		case "op line":
			if rec["level"] != "warn" {
				t.Fatalf("bad op record: %#v", rec)
			}
			no++
		default:
			t.Fatalf("unexpected record: %#v", rec)
		}
	}
	if ns != n || no != n {
		t.Fatalf("want %d of each line, got stream=%d op=%d", n, ns, no)
	}
}

// TestStreamLoggerHonorsLogFormat pins that the resolved log style reaches the
// stream-layer loggers, not just the main operational logger: with
// --log-format json the stream/notice loggers emit JSON too (one shape per run).
func TestStreamLoggerHonorsLogFormat(t *testing.T) {
	var buf bytes.Buffer
	rt := &runtime{
		deps:      resolveDeps(Deps{Getenv: func(string) string { return "" }}),
		io:        output.IO{Err: &buf},
		logFormat: "json",
	}
	rt.buildLogger(nil)

	rt.env().StreamLogger(slog.LevelDebug, stream.LogComponentStream).Debug("ws dial", "url", "wss://x/v2/public")
	stream.LogNotice(rt.env().NoticeLogger(slog.LevelInfo), stream.Notice{Code: stream.DataGap, Level: stream.LevelWarn, Message: "gap"})

	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("stream-layer line is not JSON under --log-format json: %v (line=%q)", err, l)
		}
		if rec["component"] != "stream" {
			t.Fatalf("stream record missing component=stream: %#v", rec)
		}
	}
}

// The stream-layer loggers tag every record so a reader can filter:
// component=stream (mechanics), component=stream/state (the state store), and
// component=stream kind=stream_notice code=<CODE> (mirrored notices). `component=stream`
// (a substring of stream/state) catches the whole layer; `kind=stream_notice` catches
// just notices.
func TestStreamLogTagsAndFiltering(t *testing.T) {
	var buf bytes.Buffer
	rt := &runtime{
		deps: resolveDeps(Deps{Getenv: func(string) string { return "" }}),
		io:   output.IO{Err: &buf},
	}
	rt.buildLogger(nil)

	rt.env().StreamLogger(slog.LevelDebug, stream.LogComponentStream).Debug("ws dial", "url", "wss://x/v2/public")
	rt.env().StreamLogger(slog.LevelDebug, stream.LogComponentState).Debug("state epoch bump", "epoch", 2)
	stream.LogNotice(rt.env().NoticeLogger(slog.LevelInfo), stream.Notice{Code: stream.DataGap, Level: stream.LevelWarn, Message: "gap"})

	out := buf.String()
	for _, want := range []string{
		"ws dial component=stream url=",
		"state epoch bump component=stream/state epoch=2",
		"gap component=stream kind=stream_notice code=DATA_GAP",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	var streamN, noticeN int
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(l, "component=stream") {
			streamN++
		}
		if strings.Contains(l, "kind=stream_notice") {
			noticeN++
		}
	}
	if streamN != 3 {
		t.Errorf("`component=stream` should match all 3 stream-layer lines, got %d:\n%s", streamN, out)
	}
	if noticeN != 1 {
		t.Errorf("`kind=stream_notice` should match only the notice line, got %d:\n%s", noticeN, out)
	}
}
