// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"strings"
	"time"

	"github.com/clipperhouse/displaywidth"
)

// GroupThousands formats a decimal-string value with thousand separators for
// display, capping the fraction on large-magnitude values (see [capFraction]).
// This is the default display formatter: one currency-agnostic rule for every
// value. The wire value is never modified — this renders a copy; a value that is
// not a plain decimal comes back unchanged. Use [GroupExact] where the rendered
// figure must equal the wire value to the last digit (e.g. an order review).
func GroupThousands(v string) string { return GroupExact(capFraction(v)) }

// maxDisplayIntDigits is the integer-digit count past which a displayed value
// keeps only one fractional digit: beyond it the sub-unit digits are noise that
// crowds the significant figures off a narrow panel (a nine-figure price gains
// nothing from them). It is a display rule only and identical for every
// currency — KRW is simply the currency that most often trips it.
const maxDisplayIntDigits = 8

// capFraction trims a decimal string's fraction to a single digit when its
// integer part has more than [maxDisplayIntDigits] digits, appending an ellipsis
// only when digits are actually dropped ("123456789.12345" → "123456789.1…").
// A value with no fraction ("123456789"), a single fractional digit
// ("123456789.0"), a ≤8-digit integer part, or a non-decimal is returned
// unchanged. Display-only, like [GroupThousands]: the wire value is untouched.
func capFraction(v string) string {
	dot := strings.IndexByte(v, '.')
	if dot < 0 {
		return v // no fraction to trim
	}
	digits := v[:dot]
	if digits != "" && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	if digits == "" || !allDigits(digits) || len(digits) <= maxDisplayIntDigits {
		return v
	}
	frac := v[dot+1:]
	if len(frac) <= 1 || !allDigits(frac) {
		return v // already one digit, or not a plain decimal
	}
	return v[:dot+2] + "…" // keep "<int>.<1 digit>", mark the dropped tail
}

// allDigits reports whether s is a non-empty run of ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// GroupExact formats a decimal-string value with thousand separators, preserving
// every fractional digit. The wire value is never modified — this renders a
// copy; a value that is not a plain decimal comes back unchanged.
func GroupExact(v string) string {
	if v == "" {
		return ""
	}
	sign, s := "", v
	if s[0] == '+' || s[0] == '-' {
		sign, s = s[:1], s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	if intPart == "" {
		return v
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return v
		}
	}
	var b strings.Builder
	b.WriteString(sign)
	lead := len(intPart) % 3
	if lead > 0 {
		b.WriteString(intPart[:lead])
	}
	for i := lead; i < len(intPart); i += 3 {
		if b.Len() > len(sign) {
			b.WriteByte(',')
		}
		b.WriteString(intPart[i : i+3])
	}
	b.WriteString(frac)
	return b.String()
}

// IsNegative reports the display sign of a decimal string.
func IsNegative(v string) bool { return strings.HasPrefix(v, "-") }

// FmtSymbol renders a wire currency-pair id for display: "btc_krw" → "BTC/KRW".
// It only transforms a display copy; the wire value is never modified and is
// still used verbatim as the identity everywhere else (map keys, subscriptions,
// the order path). The transform makes no assumption about the id's length or
// shape — every "_" becomes "/" and the whole string is upper-cased — so an id
// with a different segment layout still renders sanely.
func FmtSymbol(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "_", "/"))
}

// FmtCurrency renders a wire currency code for display: "btc" → "BTC".
func FmtCurrency(c string) string { return strings.ToUpper(c) }

// SymbolSearchKey canonicalizes a symbol/currency or a user's search query for
// case- and separator-insensitive substring matching: it lower-cases and folds
// "/" to "_", so both the wire form ("btc_krw") and the displayed form
// ("BTC/KRW") of a query match against the underlying wire id. Matching stays on
// the wire id; only this comparison key is derived.
func SymbolSearchKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "/", "_")
}

// FmtClock renders a unix-ms timestamp as a local wall-clock time.
func FmtClock(unixMs int64) string {
	if unixMs <= 0 {
		return "--:--:--"
	}
	return time.UnixMilli(unixMs).Format("15:04:05")
}

// The pad/clip helpers measure in terminal display cells, so localized labels
// and headers (Korean glyphs render two cells wide) align the same as the
// numeric content. The dominant input — prices, quantities, symbols — is plain
// ASCII (one cell per byte), so that case takes a byte-length fast path and
// pays nothing for the width awareness.

// cellWidths segments s into grapheme clusters with per-cluster display
// widths — the same displaywidth engine lipgloss.Width measures with, so
// padding computed here always agrees with the frame math. ASCII inputs never
// reach it (the callers' fast path handles them).
func cellWidths(s string) (total int, segs []string, cells []int) {
	g := displaywidth.StringGraphemes(s)
	for g.Next() {
		w := g.Width()
		segs = append(segs, g.Value())
		cells = append(cells, w)
		total += w
	}
	return total, segs, cells
}

// isASCII reports whether every byte of s is single-cell ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// PadLeft right-aligns s into width w display cells, truncating from the left
// when too long (keeps the least-significant end — the right behavior for
// prices). Truncation marks the cut with a leading ellipsis; dropping a
// two-cell glyph can leave one spare cell, filled with a space so the result
// is always exactly w cells.
func PadLeft(s string, w int) string {
	if isASCII(s) {
		if len(s) > w {
			if w <= 1 {
				return s[len(s)-w:]
			}
			return "…" + s[len(s)-w+1:]
		}
		return strings.Repeat(" ", w-len(s)) + s
	}
	total, segs, cells := cellWidths(s)
	if total <= w {
		return strings.Repeat(" ", w-total) + s
	}
	budget := w - 1 // the ellipsis takes one cell
	if w <= 1 {
		budget = w
	}
	keep := 0 // cells kept from the right end
	i := len(segs)
	for i > 0 && keep+cells[i-1] <= budget {
		keep += cells[i-1]
		i--
	}
	tail := strings.Join(segs[i:], "")
	if w <= 1 {
		return strings.Repeat(" ", w-keep) + tail
	}
	return "…" + strings.Repeat(" ", budget-keep) + tail
}

// ClipTail right-truncates s to width w display cells with an ellipsis,
// keeping the leading (most significant) characters — the right behavior for
// quantities. Like PadLeft, a dropped two-cell glyph pads with a space so the
// result is exactly min(w, width(s)) cells.
func ClipTail(s string, w int) string {
	if isASCII(s) {
		if len(s) <= w {
			return s
		}
		if w <= 1 {
			return s[:w]
		}
		return s[:w-1] + "…"
	}
	total, segs, cells := cellWidths(s)
	if total <= w {
		return s
	}
	keep, budget := 0, w
	if w > 1 {
		budget = w - 1
	}
	i := 0
	for i < len(segs) && keep+cells[i] <= budget {
		keep += cells[i]
		i++
	}
	head := strings.Join(segs[:i], "")
	if w <= 1 {
		if head == "" && w == 1 {
			return "…" // a leading two-cell glyph cannot be halved; the ellipsis is the one-cell rendering
		}
		return head
	}
	return head + strings.Repeat(" ", budget-keep) + "…"
}

// PadCenter centers s within w display cells, padding both sides (a leftover
// odd cell goes to the right). When s is too wide it is clipped from the tail
// with an ellipsis (ClipTail), so a centered label keeps its leading glyphs.
func PadCenter(s string, w int) string {
	width := len(s)
	if !isASCII(s) {
		width, _, _ = cellWidths(s)
	}
	if width >= w {
		return ClipTail(s, w)
	}
	left := (w - width) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", w-width-left)
}

// PadRight left-aligns s into width w display cells, truncating when too long.
func PadRight(s string, w int) string {
	if isASCII(s) {
		if len(s) > w {
			if w <= 1 {
				return s[:w]
			}
			return s[:w-1] + "…"
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	if total, _, _ := cellWidths(s); total <= w {
		return s + strings.Repeat(" ", w-total)
	}
	return ClipTail(s, w)
}
