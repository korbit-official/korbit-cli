// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import "strings"

// compactUnits maps a power-of-ten exponent to its unit suffix.
var compactUnits = map[int]string{3: "k", 6: "M", 9: "B", 12: "T"}

// Compact renders a decimal-string value into w display cells: the grouped
// figure (fraction-capped on large values, via GroupThousands) when it fits,
// otherwise a unit form with three significant digits ("9.62B", "48.7k"). For a
// DERIVED display figure this beats
// clipping — a tail clip fakes precision and a head clip destroys magnitude,
// the worst failure mode for money. Never use it on a typed input or a wire
// value (those stay exact decimal strings end to end). A value whose integer
// part is under four digits gains nothing from a unit and falls back to
// ClipTail, as does a non-numeric value.
func Compact(v string, w int) string {
	grouped := GroupThousands(v)
	if len([]rune(grouped)) <= w {
		return grouped
	}
	sign, s := "", v
	if s != "" && (s[0] == '+' || s[0] == '-') {
		sign, s = s[:1], s[1:]
	}
	intPart := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return ClipTail(grouped, w)
		}
	}
	n := len(intPart)
	if n <= 3 {
		return ClipTail(grouped, w)
	}
	ds, carry := roundDigits(intPart, 3)
	if carry {
		ds, n = "100", n+1
	}
	e := (n - 1) / 3 * 3
	if e > 12 {
		e = 12 // beyond T the integer head grows instead
	}
	head := n - e
	var out string
	if head >= len(ds) {
		out = sign + ds + strings.Repeat("0", head-len(ds)) + compactUnits[e]
	} else {
		intStr, frac := ds[:head], ds[head:]
		out = sign + intStr + "." + frac + compactUnits[e]
		// Still too wide (a tiny cell): shed fraction digits before giving up.
		for len([]rune(out)) > w && frac != "" {
			frac = frac[:len(frac)-1]
			out = sign + intStr + strings.TrimSuffix("."+frac, ".") + compactUnits[e]
		}
	}
	if len([]rune(out)) > w {
		return ClipTail(grouped, w)
	}
	return out
}

// roundDigits rounds the digit string d to sig significant digits, returning
// the digits and whether the round carried out of the leading digit (e.g.
// "999…" → "100" with carry: the value gained an order of magnitude).
func roundDigits(d string, sig int) (string, bool) {
	if len(d) <= sig {
		return d, false
	}
	digits := []byte(d[:sig])
	if d[sig] >= '5' {
		i := sig - 1
		for ; i >= 0; i-- {
			if digits[i] < '9' {
				digits[i]++
				break
			}
			digits[i] = '0'
		}
		if i < 0 {
			return "1" + strings.Repeat("0", sig-1), true
		}
	}
	return string(digits), false
}
