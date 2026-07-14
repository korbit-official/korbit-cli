// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package textout holds the command-agnostic human-output toolkit: the table,
// key/value and number-formatting primitives the cli's human-readable mode is
// built from, plus the TextFormatter seam a command's result type implements to
// render itself.
//
// It lives apart from internal/cli so a command subpackage can build its own
// human output without importing cli (which it cannot), and apart from
// internal/output, which owns the generic JSON/text/error wire contract and
// stays free of presentation logic. Money/quantity values arrive as decimal
// STRINGS and are for DISPLAY ONLY — Num groups thousands on a display copy; the
// wire path never sees this package.
package textout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// TextFormatter is implemented by a local/command-package command's result type
// to render itself as human-readable output. The command returns its typed
// result; the emitter marshals it verbatim in --json mode and, in human mode,
// calls FormatText so the command writes its own text from typed fields — no
// command-key lookup and no JSON round-trip. The catalog-driven endpoint
// commands do NOT use this: they emit verbatim API bytes whose shape varies per
// endpoint and render through the cli's keyed per-command formatter table.
type TextFormatter interface {
	// FormatText writes the result as human-readable text to w, without a
	// trailing newline (the output sink adds one). Build the text with the
	// helpers in this package.
	FormatText(w io.Writer)
}

// ---- column / number helpers ----

// PadRune left-justifies s into a field width measured in runes (not bytes), so
// non-ASCII values still align.
func PadRune(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// PadLeftRune right-justifies s into a rune-measured field width (for numeric
// columns).
func PadLeftRune(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

// Table renders rows under headers with each column sized to its widest cell.
// Numeric columns are right-aligned; the rest left-aligned. align[i]==true means
// right-align column i.
func Table(headers []string, rows [][]string, align []bool) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) {
				if n := utf8.RuneCountInString(c); n > widths[i] {
					widths[i] = n
				}
			}
		}
	}
	pad := func(i int, s string) string {
		if i < len(align) && align[i] {
			return PadLeftRune(s, widths[i])
		}
		return PadRune(s, widths[i])
	}
	var b strings.Builder
	for i, h := range headers {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(pad(i, h))
	}
	for _, r := range rows {
		b.WriteByte('\n')
		for i := range headers {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(pad(i, cell))
		}
	}
	return b.String()
}

// Num formats a decimal-string value with thousand separators for readability.
// The value arrives as a decimal STRING and is for DISPLAY ONLY — the wire path
// never sees this copy. If the string isn't a clean integer-ish/decimal number,
// it is returned verbatim (never guess).
func Num(s string) string {
	if s == "" {
		return s
	}
	neg := false
	body := s
	if strings.HasPrefix(body, "-") {
		neg = true
		body = body[1:]
	}
	intPart, fracPart := body, ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
	}
	if intPart == "" || !allDigits(intPart) || (fracPart != "" && !allDigits(fracPart)) {
		return s // not a plain decimal — print verbatim
	}
	grouped := groupThousands(intPart)
	out := grouped
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg {
		out = "-" + out
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func groupThousands(intPart string) string {
	n := len(intPart)
	if n <= 3 {
		return intPart
	}
	var b strings.Builder
	lead := n % 3
	if lead > 0 {
		b.WriteString(intPart[:lead])
	}
	for i := lead; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(intPart[i : i+3])
	}
	return b.String()
}

// Jstr returns the string at key from m, or "" if absent/non-string. Numbers are
// rendered via json.Number so a numeric field (e.g. a timestamp) still prints.
func Jstr(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return "true"
		}
		return "false"
	}
	return strings.TrimSpace(string(raw))
}

// AsObject decodes raw into an order-agnostic field map (formatters read fields
// by name). ok=false if raw isn't a JSON object.
func AsObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

// AsArray decodes raw into a slice of element messages. ok=false if not an array.
func AsArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, false
	}
	return a, true
}

// KVBlock renders aligned "label: value" lines, skipping rows whose value is "".
func KVBlock(rows [][2]string) string {
	width := 0
	for _, r := range rows {
		if r[1] == "" {
			continue
		}
		if n := utf8.RuneCountInString(r[0]); n > width {
			width = n
		}
	}
	var b strings.Builder
	for _, r := range rows {
		if r[1] == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s  %s", PadRune(r[0]+":", width+1), r[1])
	}
	return b.String()
}

// RawJSON renders value to compact JSON bytes for a keyed formatter to read: a
// json.RawMessage passes through verbatim; anything else is marshalled with HTML
// escaping off (matching the output package). Returns nil on a marshal error.
func RawJSON(value any) json.RawMessage {
	if rm, ok := value.(json.RawMessage); ok {
		return rm
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// ListView unwraps a possibly-truncated list payload for a formatter: the bare
// array for a complete result, or the {"data":[...],"truncated":true,"note":...}
// envelope (see wrapTruncated in cli/endpoint.go) for an incomplete one. It
// returns the row array plus the optional truncation note so the formatter can
// append it to the human table. ok=false only when an object payload isn't that
// envelope.
func ListView(raw json.RawMessage) (rows json.RawMessage, note string, ok bool) {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '{' {
		var env struct {
			Data      json.RawMessage `json:"data"`
			Truncated bool            `json:"truncated"`
			Note      string          `json:"note"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Data != nil {
			return env.Data, env.Note, true
		}
		return nil, "", false
	}
	return raw, "", true
}

// WithTruncationNote appends the truncation note (when present) to a rendered
// human table, so a stdout-only reader sees the result is incomplete.
func WithTruncationNote(text, note string) string {
	if note == "" {
		return text
	}
	return text + "\n\n⚠ " + note
}

// ---- small shared helpers ----

// OrNone returns s, or "(none)" when s is empty — for fields where absence is
// meaningful to show rather than hide.
func OrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// YesNo renders a bool as "yes"/"no" — for FormatText methods rendering a
// boolean field in a key/value block.
func YesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// IndentLines prefixes every line of s with prefix.
func IndentLines(s, prefix string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// OneLine collapses whitespace runs in s to single spaces and truncates to max
// runes (with an ellipsis) — for previewing a user-supplied script/predicate in
// a plan without letting a multi-line value wreck the layout.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

// OrderedKV reads a JSON object preserving its field order, with each value as a
// display string (a JSON string unquoted; anything else verbatim). ok=false if
// raw isn't a JSON object. Used where insertion order is meaningful (the dry-run
// request params are sent in this order).
func OrderedKV(raw json.RawMessage) ([][2]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	var out [][2]string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, false
		}
		k, ok := kt.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			s = strings.TrimSpace(string(v))
		}
		out = append(out, [2]string{k, s})
	}
	return out, true
}
