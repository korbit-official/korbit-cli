// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

// tag is the stderr line prefix a tagged text record carries: the invoked
// program name, read the same way the handler reads it, so these assertions
// follow a renamed binary instead of pinning one spelling.
func tag() string { return progname.Name() + ": " }

func TestLevelForGating(t *testing.T) {
	// The default (no --debug, no explicit --log-level) is Error: warn-and-below
	// operational logs are opt-in (failures surface through program output), and
	// only the lean Error tier — a must-know operational failure with no
	// program-output equivalent — shows.
	var buf bytes.Buffer
	log := New(&buf, LevelFor(false))
	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	got := buf.String()
	if strings.Contains(got, "debug:") || strings.Contains(got, "info:") || strings.Contains(got, "warn:") {
		t.Fatalf("debug/info/warn must be suppressed at the default level, got: %q", got)
	}
	if !strings.Contains(got, tag()+"error: e\n") {
		t.Fatalf("the Error tier must show at the default level, got: %q", got)
	}
	if LevelFor(false) != slog.LevelError {
		t.Fatalf("LevelFor(false) = %v; want Error", LevelFor(false))
	}
	if LevelFor(true) != slog.LevelDebug {
		t.Fatalf("LevelFor(true) = %v; want Debug (--debug opts into the verbose log)", LevelFor(true))
	}
}

func TestTraceLevel(t *testing.T) {
	// ParseLevel knows the word and it sits below Debug.
	lvl, ok := ParseLevel("TRACE")
	if !ok || lvl != LevelTrace {
		t.Fatalf("ParseLevel(trace) = %v, %v; want %v, true", lvl, ok, LevelTrace)
	}
	if LevelTrace >= slog.LevelDebug {
		t.Fatalf("LevelTrace (%d) must be below Debug (%d)", LevelTrace, slog.LevelDebug)
	}

	// At Trace, a Trace record shows and renders with the "trace" tag.
	var buf bytes.Buffer
	Trace(New(&buf, LevelTrace), "fine", "k", "v")
	if got := buf.String(); got != tag()+"trace: fine k=v\n" {
		t.Fatalf("trace render: %q", got)
	}

	// At Debug, a Trace record is gated out but Debug still shows.
	buf.Reset()
	log := New(&buf, slog.LevelDebug)
	Trace(log, "fine")
	log.Debug("coarse")
	got := buf.String()
	if strings.Contains(got, "trace:") {
		t.Fatalf("trace must be gated out at --log-level debug, got: %q", got)
	}
	if !strings.Contains(got, tag()+"debug: coarse\n") {
		t.Fatalf("debug should still show, got: %q", got)
	}
}

func TestDebugShowsEverything(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, LevelFor(true)) // debug: Debug and above
	log.Debug("hello")
	log.Info("there")
	if !strings.Contains(buf.String(), tag()+"debug: hello\n") {
		t.Fatalf("debug line missing under debug level: %q", buf.String())
	}
	if !strings.Contains(buf.String(), tag()+"info: there\n") {
		t.Fatalf("info line missing under debug level: %q", buf.String())
	}
}

func TestAttrsRendering(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	log.Info("signing", "key", "my key", "n", 3)
	got := strings.TrimSpace(buf.String())
	want := `digitalx: info: signing key="my key" n=3`
	if got != want {
		t.Fatalf("attr rendering\n got: %q\nwant: %q", got, want)
	}
}

// redactable redacts itself via slog.LogValuer — the secret-safety seam.
type redactable struct{ secret string }

func (redactable) LogValue() slog.Value { return slog.StringValue("REDACTED") }

func TestLogValuerRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	log.Info("auth", "pem", redactable{secret: "PRIVATE"})
	if strings.Contains(buf.String(), "PRIVATE") {
		t.Fatalf("LogValuer secret leaked: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "pem=REDACTED") {
		t.Fatalf("expected redacted value, got: %q", buf.String())
	}
}

func TestOrAndNopAreSilentAndSafe(t *testing.T) {
	// Or(nil) must return a usable, silent logger (no panic, no output).
	var buf bytes.Buffer
	Or(nil).Info("dropped") // must not panic
	Nop().Error("dropped")  // must not panic
	if buf.Len() != 0 {
		t.Fatalf("Nop/Or(nil) should write nothing, got: %q", buf.String())
	}
	// Or(l) must return l unchanged so a wired logger keeps working.
	l := New(&buf, slog.LevelInfo)
	if Or(l) != l {
		t.Fatal("Or(l) should return l unchanged when l is non-nil")
	}
	Or(l).Info("kept")
	if !strings.Contains(buf.String(), tag()+"info: kept\n") {
		t.Fatalf("Or(l) dropped a record, got: %q", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in    string
		want  slog.Level
		valid bool
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", 0, false}, // the long spelling is not accepted
		{"error", slog.LevelError, true},
		{"off", LevelOff, true},
		{"  Error ", slog.LevelError, true}, // case- and space-insensitive
		{"INFO", slog.LevelInfo, true},
		{"verbose", 0, false}, // unrecognized
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseLevel(c.in)
		if ok != c.valid {
			t.Fatalf("ParseLevel(%q) ok=%v, want %v", c.in, ok, c.valid)
		}
		if ok && got != c.want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestLevelOffSuppressesEverything(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, LevelOff)
	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	if buf.Len() != 0 {
		t.Fatalf("LevelOff should suppress all records, got: %q", buf.String())
	}
}

func TestTimestampStyleDropsTagAndStampsTime(t *testing.T) {
	// The --log-file text style stamps a local RFC3339 timestamp, then the level
	// and message, and drops the "<prog>: " tag a shared terminal needs.
	var buf bytes.Buffer
	log := NewStyled(&buf, slog.LevelInfo, Style{Timestamp: true})
	log.Warn("disk slow", "ms", 42)
	got := strings.TrimSpace(buf.String())
	if strings.Contains(got, tag()) {
		t.Fatalf("timestamp style must drop the tag, got: %q", got)
	}
	want := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2} warn disk slow ms=42$`)
	if !want.MatchString(got) {
		t.Fatalf("timestamp render: %q", got)
	}
}

func TestJSONFormat(t *testing.T) {
	// FormatJSON emits one object per record, with the level in this package's
	// vocabulary (not slog's "WARN"/"DEBUG-4"), a time field, and the attributes.
	var buf bytes.Buffer
	log := NewStyled(&buf, LevelTrace, Style{Format: FormatJSON})
	Trace(log, "fine", "k", "v")
	log.Warn("coarse", "n", 3)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON records, got %d: %q", len(lines), buf.String())
	}
	var trace, warn map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &trace); err != nil {
		t.Fatalf("record 0 not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &warn); err != nil {
		t.Fatalf("record 1 not JSON: %v", err)
	}
	if trace["level"] != "trace" || trace["msg"] != "fine" || trace["k"] != "v" {
		t.Fatalf("trace record: %#v", trace)
	}
	if warn["level"] != "warn" || warn["msg"] != "coarse" || warn["n"] != float64(3) {
		t.Fatalf("warn record: %#v", warn)
	}
	if _, ok := warn["time"]; !ok {
		t.Fatalf("JSON record should carry a time field: %#v", warn)
	}
}

// countingWriter records how many Write calls it receives.
type countingWriter struct{ writes int }

func (c *countingWriter) Write(p []byte) (int, error) { c.writes++; return len(p), nil }

// TestEachRecordIsOneWrite pins the property cross-logger atomicity rests on:
// EVERY format assembles a record fully and emits it in exactly ONE Write to the
// sink. The cli shares one locking writer across independently constructed
// loggers (op/stream/notice), so one-write-per-record is what keeps their lines
// from interleaving — and it must hold for text (tagged and timestamped) AND
// json, not just the default.
func TestEachRecordIsOneWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		style Style
	}{
		{"text-tagged", Style{}},
		{"text-timestamp", Style{Timestamp: true}},
		{"json", Style{Format: FormatJSON}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cw := &countingWriter{}
			log := NewStyled(cw, LevelTrace, tc.style)
			Trace(log, "zero", "k", "v")
			log.Info("one", "k", "v")
			log.Warn("two", "n", 2)
			log.Error("three")
			if cw.writes != 4 {
				t.Fatalf("%s: want exactly 1 write per record (4 total), got %d", tc.name, cw.writes)
			}
		})
	}
}

func TestConcurrentWritesAreLineAtomic(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Warn("concurrent line")
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("expected 50 lines, got %d", len(lines))
	}
	for _, l := range lines {
		if l != tag()+"warn: concurrent line" {
			t.Fatalf("interleaved/garbled line: %q", l)
		}
	}
}

// TestTextTagIsTheInvokedProgramName pins that the stderr tag is the name the
// binary was invoked as, not a compiled-in product name: an install that keeps
// the legacy command alongside the current one must not label its own output
// with the other name.
func TestTextTagIsTheInvokedProgramName(t *testing.T) {
	orig := progname.Name()
	t.Cleanup(func() { progname.Set(orig) })

	for _, prog := range []string{"digitalx", "korbit"} {
		progname.Set(prog)
		var buf bytes.Buffer
		NewStyled(&buf, slog.LevelInfo, Style{}).Warn("tagged")
		if got, want := buf.String(), prog+": warn: tagged\n"; got != want {
			t.Fatalf("as %q: got %q, want %q", prog, got, want)
		}
	}
}
