// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LevelOff is a threshold above every real record level, so a logger created
// with it emits nothing. It backs the "off" log level (--log-level off).
const LevelOff = slog.Level(1 << 30)

// LevelTrace is one step below slog.LevelDebug — the firehose tier for
// per-iteration call sites (each send attempt, each lookup retry, each history/
// candles page) that would bloat a normal Debug log. It backs "--log-level
// trace": Debug shows the decisions and outcomes a support log needs; Trace adds
// the step-by-step detail when a single operation must be dissected. --debug
// resolves to Debug (LevelFor), never Trace — Trace is explicit-opt-in only.
const LevelTrace = slog.Level(-8)

// LevelFor maps the CLI's debug switch onto a handler threshold: Debug when
// debug mode is on (show everything), else Error. A command's failures and
// safety-relevant facts surface through program output (the error envelope +
// guidance notes, never gated), so the warn-and-below operational log is a pure
// opt-in diagnostic that would otherwise only echo what the error already says;
// opt in with --log-level or --debug. The Error tier alone shows by default and
// is reserved for a genuinely operational failure with NO program-output
// equivalent — keep it lean and never duplicate the error envelope (a default
// run with no such failure is quiet). This is the default when no explicit
// --log-level is given.
func LevelFor(debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelError
}

// ParseLevel maps a level word (case- and space-insensitive) onto a handler
// threshold: trace, debug, info, warn, error, or off (off suppresses every
// operational log). ok is false for an unrecognized word, so each caller can
// phrase its own usage error. This is the one place the level vocabulary is
// defined.
func ParseLevel(s string) (level slog.Level, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace, true
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	case "off":
		return LevelOff, true
	}
	return 0, false
}

// Trace logs at LevelTrace (one step below Debug). slog.Logger exposes no Trace
// method, so this is the package's Trace shim: it is the firehose tier for
// per-iteration call sites. l must be non-nil (resolve via Or first); args are
// slog key/value pairs. The handler gates by level, so a disabled Trace costs
// only the Enabled check.
func Trace(l *slog.Logger, msg string, args ...any) {
	l.Log(context.Background(), LevelTrace, msg, args...)
}

// Nop returns a logger that discards every record. It is the right zero value
// for an optional Log field on a lower-layer type, so call sites can log
// unconditionally (the handler drops everything) without a nil check.
func Nop() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Or returns l, or a no-op logger when l is nil. A lower layer that takes an
// optional *slog.Logger calls Or(x.Log) once and then logs unconditionally, so
// an unwired field is silent rather than a nil panic.
func Or(l *slog.Logger) *slog.Logger {
	if l == nil {
		return Nop()
	}
	return l
}

// Format selects the wire shape of each emitted record.
type Format int

const (
	// FormatText is the human-readable one-line form. On stderr it is tagged
	// "korbit-cli: <level>: <message>[ k=v …]"; with [Style.Timestamp] set (the
	// form used when logs are diverted to a file) it is stamped with a local
	// RFC3339 timestamp instead of the tag — "<ts> <level> <message>[ k=v …]" —
	// since a file trail wants a wall-clock anchor and has no terminal session to
	// scope the "korbit-cli:" tag to.
	FormatText Format = iota
	// FormatJSON emits one JSON object per record via slog's JSON handler (the
	// standard time/level/msg keys, then the attributes), with the level rendered
	// in this package's vocabulary (trace/debug/info/warn/error).
	FormatJSON
)

// Style is the resolved presentation for a logger: which [Format], and — for
// FormatText only — whether to stamp lines with a timestamp instead of the
// "korbit-cli: " tag. The zero Style is the stderr default (text, tagged, no
// timestamp).
type Style struct {
	Format Format
	// Timestamp applies to FormatText only: prefix each line with a local
	// RFC3339 timestamp and drop the "korbit-cli: " tag. FormatJSON always
	// carries a timestamp (slog's time key), so this field is ignored there.
	Timestamp bool
}

// New returns a logger that writes human diagnostic lines to w at or above
// level in the default stderr style (text, "korbit-cli: " tagged). Each record
// is assembled into a single line and written under an internal lock, so the
// returned logger and every logger derived from it are safe to use concurrently.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return NewStyled(w, level, Style{})
}

// NewStyled is New with an explicit [Style] (text vs JSON, timestamped vs
// tagged). The cli resolves one Style per invocation from --log-format and
// whether --log-file is in use, and builds every logger — the operational
// logger and the stream-layer loggers — through it so a run's lines share a
// shape. Like New, every derived logger is safe for concurrent use.
func NewStyled(w io.Writer, level slog.Level, st Style) *slog.Logger {
	if st.Format == FormatJSON {
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: jsonReplaceAttr,
		}))
	}
	return slog.New(&textHandler{w: w, mu: &sync.Mutex{}, level: level, timestamp: st.Timestamp})
}

// jsonReplaceAttr renders the level with this package's vocabulary
// (trace/debug/info/warn/error, including the custom LevelTrace) instead of
// slog's default "DEBUG-4"/"WARN" spellings, so the level word is identical
// across the text and JSON formats.
func jsonReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey {
		if lvl, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(levelTag(lvl))
		}
	}
	return a
}

// textHandler renders slog records as text — "korbit-cli: <level>: <message>[ k=v …]"
// by default, or "<rfc3339> <level> <message>[ k=v …]" when timestamp is set
// (the FormatJSON path uses slog's own JSON handler instead). Derived handlers
// (With/WithGroup) share the same writer and mutex pointer, so all writes to one
// stderr are serialized regardless of which logger emitted them.
type textHandler struct {
	w         io.Writer
	mu        *sync.Mutex
	level     slog.Level
	timestamp bool // stamp an RFC3339 (local) time and drop the "korbit-cli: " tag
	attrs     []slog.Attr
	group     string // dotted prefix applied to attribute keys ("" = none)
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	if h.timestamp {
		if !r.Time.IsZero() {
			b.WriteString(r.Time.Format(time.RFC3339))
			b.WriteByte(' ')
		}
		b.WriteString(levelTag(r.Level))
		b.WriteByte(' ')
	} else {
		b.WriteString("korbit-cli: ")
		b.WriteString(levelTag(r.Level))
		b.WriteString(": ")
	}
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		writeAttr(&b, h.group, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, h.group, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *textHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	nh := *h
	nh.attrs = append(append(make([]slog.Attr, 0, len(h.attrs)+len(as)), h.attrs...), as...)
	return &nh
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	if nh.group == "" {
		nh.group = name
	} else {
		nh.group += "." + name
	}
	return &nh
}

// writeAttr appends " key=value" for one attribute, resolving LogValuers (the
// redaction seam) and flattening groups into dotted keys.
func writeAttr(b *strings.Builder, group string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return // an empty attr is dropped, matching slog's own handlers
	}
	key := a.Key
	if group != "" {
		key = group + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			writeAttr(b, key, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	v := a.Value.String()
	if v == "" || strings.ContainsAny(v, " \t\"") {
		v = strconv.Quote(v)
	}
	b.WriteString(v)
}

// levelTag renders the level word in lower case to match slog's level
// vocabulary (slog spells it "WARN"), e.g. "warn" rather than "WARN".
func levelTag(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "trace"
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}
